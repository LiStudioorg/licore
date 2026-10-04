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
func RunExecSetup(args []string) error {
	// 去掉可选的分隔符 "--"。execns 侧始终会加，但保留容错。
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return fmt.Errorf("exec-setup: 缺少要执行的命令: %w", ErrBadConfig)
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

	// 4. execve 用户命令。路径在容器视图内解析（此时已在容器的 mount ns 里）。
	env := envWithoutLiCore()
	if err := syscall.Exec(args[0], args, env); err != nil {
		return fmt.Errorf("exec-setup: exec %s: %w", args[0], err)
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
