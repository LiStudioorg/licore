// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"syscall"
)

// helperArgv0Marker 是补 argv[0] 时使用的占位符。
//
// ParseExecSetupArgs 的约定是"argv 含 argv[0]"，而 CLI 子命令拿到的是
// cobra 的 args（不含）。用一个固定标记补位，使两条路径共用同一套解析，
// 同时可被测试识别——比"看长度猜"可靠。
const helperArgv0Marker = "exec-setup"

// RunExecSetup 是 `licore exec` 的收口入口：在**容器内**被 nsenter 唤起，
// 按容器配置收紧权限后 execve 用户命令。
//
// 与容器 init 的关系：init 的收口在 executeContainerCmd 里，exec 的收口在这里。
// 两处的**顺序与内容必须一致**（no_new_privs → capability 裁剪 → seccomp），
// 否则 exec 进入的进程权限与容器 1 号进程不同，"隔离"就有个洞。
//
// 参数形式（argv[0] 为 helper 自身路径）：
//
//	licore exec-setup [--] <cmd> <args...>
//
// 收口规格经环境变量下发（LICORE_CAPS_DROP），与容器 init 一致。
func RunExecSetup(argv []string) error {
	// 约定：argv **含 argv[0]**（helper 自身路径），与真实进程的 os.Args 一致。
	// CLI 子命令传入的是 cobra 的 args（不含 argv[0]），因此这里补一个占位，
	// 让两条调用路径共用同一套解析。
	if len(argv) == 0 || argv[0] != helperArgv0Marker {
		argv = append([]string{helperArgv0Marker}, argv...)
	}
	workdir, cmd, err := ParseExecSetupArgs(argv)
	if err != nil {
		return err
	}

	// 1. no_new_privs：阻止 execve 获得新特权（与容器 init 同一顺序第一步）。
	if err := setNoNewPrivs(); err != nil {
		return err
	}
	// 2. capability 裁剪。**只减不加**——ExecSetupEnv 不接受 capsAdd，
	//    因此这里永远只传 drop 列表，语义上不可能放宽。
	drop := ExecSetupCapsDrop()
	kept, err := ApplyCapabilities(drop, nil)
	if err != nil {
		return fmt.Errorf("exec-setup: 裁剪能力失败: %w", err)
	}
	slog.Debug("exec 收口：能力已裁剪", slog.Any("kept", kept), slog.Any("drop", drop))
	// 3. seccomp：与容器 init 用同一份黑名单。
	if err := installSeccompFilter(); err != nil {
		return fmt.Errorf("exec-setup: 安装 seccomp 过滤器失败: %w", err)
	}

	// 4. 工作目录：**在这里 chdir**，而不是交给 nsenter 的 -w。
	//
	// nsenter 的 -w 是"先 chdir 再 setns"：chdir 发生在宿主 mount namespace，
	// 随后切到容器 mount ns，cwd 指向的 inode 在新视图里可能不存在，
	// getcwd 直接失败（真机实测报 getcwd: No such file or directory）。
	// helper 本身就在容器的 mount ns 内运行，它的 chdir 天然正确。
	//
	// 失败必须明确报错：静默忽略会让用户以为 -w 生效了，实际落在别的目录。
	if err := chdirIfSet(workdir); err != nil {
		return err
	}

	// 4.5 降权到目标用户（--user）。
	//
	// **必须排在能力裁剪与 seccomp 之后、execve 之前**：
	//  - 裁剪要求调用者持有 CAP_SETPCAP（PR_CAPBSET_DROP 的硬性前提），
	//    先降权就会报 `prctl(PR_CAPBSET_DROP, …): operation not permitted`
	//    —— 真机实测：exec 一个 --user 1000 的容器时炸在这里；
	//  - 而 execve 之后进程已经是用户命令，再改也来不及。
	//
	// 收口顺序因此是：no_new_privs → cap 裁剪 → seccomp → chdir → 降权 → execve。
	// 与容器 init 的 applyWorkdirAndUser 语义一致（那里同样在裁剪之后降权）。
	//
	// 安全性：降权发生在收口之后，不会削弱隔离——no_new_privs 已设，
	// 且能力已被裁剪，降权后的进程无法通过 execve 拿回任何特权。
	if err := applyExecUser(); err != nil {
		return err
	}

	// 5. execve 用户命令。路径在容器视图内解析（此时已在容器的 mount ns 里）。
	env := envWithoutLiCore()
	if err := syscall.Exec(cmd[0], cmd, env); err != nil {
		return fmt.Errorf("exec-setup: exec %s: %w", cmd[0], err)
	}
	return nil // 不可达
}

// InstallExecHelper 把 licore 自身的二进制只读 bind 到容器内的固定路径。
//
// 必须在 pivot_root **之前**调用（与卷挂载同一时机）：rootfs 此时已 bind 为
// 挂载点，挂载会随新根一起进入容器；pivot 之后再挂就得先回到容器视图外，
// 做不到。
//
// 只读的实现方式是 v0.6.0 学到的教训：**必须先 MS_BIND，再
// MS_REMOUNT|MS_BIND|MS_RDONLY**。单次 mount(MS_BIND|MS_RDONLY) 的
// MS_RDONLY 会被内核忽略，bind 结果仍是可写的。
func InstallExecHelper(rootfs string) error {
	self, err := os.Executable()
	if err != nil {
		// 拿不到自身路径就无法提供 exec 收口。宁可启动失败也不要静默跳过——
		// 静默跳过会让用户以为 exec 有隔离，实际是宿主满能力。
		return fmt.Errorf("exec-helper: 无法确定 licore 二进制路径: %w", err)
	}
	dir := rootfs + HelperDirInContainer
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("exec-helper: 创建 %s: %w", HelperDirInContainer, err)
	}
	dst := rootfs + HelperPathInContainer
	// 目标文件必须先存在，bind 才有落点。用 0 字节占位即可，
	// bind 之后内容被源文件覆盖。
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("exec-helper: 创建挂载点 %s: %w", HelperPathInContainer, err)
	}
	_ = f.Close()

	if err := syscall.Mount(self, dst, "", uintptr(msBind), ""); err != nil {
		return fmt.Errorf("exec-helper: bind %s → %s: %w", self, HelperPathInContainer, err)
	}
	// 先 bind 再 remount 才能生效（见函数注释）。
	if err := syscall.Mount(dst, dst, "",
		uintptr(msBind|syscall.MS_REMOUNT|syscall.MS_RDONLY), ""); err != nil {
		return fmt.Errorf("exec-helper: 设为只读失败: %w", err)
	}
	slog.Debug("exec helper 已只读挂入容器", slog.String("path", HelperPathInContainer))
	return nil
}

// execHelperAvailableInRootfs 报告 rootfs 里是否已有可执行的 helper。
//
// 供测试与诊断使用：它读的是 rootfs 内路径（宿主视角），
// 与运行时容器内的 HelperPathInContainer 是同一位置。
func execHelperAvailableInRootfs(rootfs string) bool {
	fi, err := os.Stat(rootfs + HelperPathInContainer)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// chdirIfSet 在 workdir 非空时切换当前工作目录。
//
// 抽成独立函数是为了能在**无特权**环境直接验证：真实 RunExecSetup 里
// chdir 排在能力裁剪之后，而沙箱没有 CAP_SETPCAP，裁剪会先失败，
// chdir 那几行永远跑不到——那样这段逻辑在本机就没有任何覆盖。
func chdirIfSet(workdir string) error {
	if workdir == "" {
		return nil
	}
	if err := os.Chdir(workdir); err != nil {
		return fmt.Errorf("exec-setup: 切换工作目录到 %q 失败: %w", workdir, err)
	}
	return nil
}

// applyExecUser 应用 exec 的目标用户（LICORE_USER）。
//
// 从**环境变量**而不是 argv 读取：argv 要原样透传给用户命令，往里塞
// 引擎自己的参数会污染用户命令。这也与 LICORE_CAPS_DROP 的下发方式一致。
//
// 空值 = 不改身份（保持调用者身份）。失败必须报错——静默忽略会让用户
// 以为"以 uid 1000 跑了"，实际是 root。
func applyExecUser() error {
	spec := userFromEnv(os.Environ())
	if spec == "" {
		return nil
	}
	uid, gid, err := ParseUserSpec(spec)
	if err != nil {
		return fmt.Errorf("exec-setup: 解析 --user %q 失败: %w", spec, err)
	}
	// 先 gid 后 uid：setuid 之后不再持有 CAP_SETGID。
	if err := syscall.Setgroups([]int{gid}); err != nil {
		slog.Debug("exec-setup: 设置附加组失败（忽略）", "gid", gid, "err", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("exec-setup: 设置 gid=%d 失败: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("exec-setup: 设置 uid=%d 失败: %w", uid, err)
	}
	slog.Debug("exec-setup: 已切换用户", "uid", uid, "gid", gid)
	return nil
}
