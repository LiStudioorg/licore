// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

// Package execns 提供进入目标进程命名空间并执行的辅助能力。
//
// 背景：纯 Go 进程调用 setns(CLONE_NEWNS) 进入挂载命名空间会返回 EINVAL
// （Go runtime 是多线程的，setns 要求调用线程不与其它线程共享 CLONE_FS，
// 见 Go issue #9091），因此进入容器命名空间需要借助一个「单线程、exec 前」
// 的上下文。
//
// 本包的实现在 v0.8.0 由自研 cgo fork 改为调用系统 nsenter（util-linux /
// Toybox / busybox 均提供）。好处：
//   - 二进制保持纯 Go，CGO_ENABLED=0 即可编译，静态链接、无 glibc 依赖；
//   - 不再需要 C 工具链，交叉编译与发布矩阵都简化；
//   - nsenter 是久经考验的实现，且默认会在 setns 之后**再 fork 一次**
//     （这正是 PID namespace 所必需的语义，详见下文）。
//
// 只有「进入命名空间」这一步交给 nsenter；伪终端分配、stdio 透传、工作
// 目录与 uid/gid 的解析仍由 Go 侧（internal/runtime）负责，两边职责不重叠。
package execns

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrNoNsenter 表示系统里找不到可用的 nsenter 实现。
var ErrNoNsenter = errors.New("exec 需要 nsenter")

// ErrUnsupported 表示当前构建/平台不支持该能力（非 Linux）。
var ErrUnsupported = errors.New("exec 在当前平台不受支持")

// 与 stub 保持同名，避免调用方需要按构建标签区分。
//
// ErrNoCgoExec 保留为 ErrNoNsenter 的别名：调用方（internal/runtime、
// CLI）此前用 errors.Is(err, execns.ErrNoCgoExec) 判定「这个构建不支持
// exec」，现在原因变成「缺 nsenter」，语义位置不变，调用方无需改动。
var ErrNoCgoExec = ErrNoNsenter

// Enabled 报告当前构建是否支持进入命名空间执行。
//
// 与旧实现（编译期常量）不同，现在取决于**运行期**是否存在 nsenter：
// 同一个二进制在装了 util-linux 的机器上可用，在没装的机器上不可用。
func Enabled() bool { return findNsenter() != nil }

// nsenter 定位结果。用变量保存以便测试注入（seam）。
type nsenterProg struct {
	// path 是可直接执行的路径（busybox 后备时是 busybox 自身）。
	path string
	// args 是调用 nsenter 时需要前置的固定参数。
	// util-linux / Toybox 直接可执行 nsenter；busybox 需要 `busybox nsenter`。
	args []string
}

// lookPath 与 lookExtensions 做成变量，便于单元测试注入假的探测结果。
var (
	lookPath = exec.LookPath
)

// findNsenter 按优先级探测可用的 nsenter。
//
//  1. 系统自带的 nsenter（util-linux / Toybox 的独立可执行文件）
//  2. busybox nsenter（Magisk、精简 Android 环境常见：没有独立 nsenter，
//     但 busybox 是多调用二进制，可 `busybox nsenter`）
//
// 返回 nil 表示两者都没有。
func findNsenter() *nsenterProg {
	if p, err := lookPath("nsenter"); err == nil && p != "" {
		return &nsenterProg{path: p}
	}
	if p, err := lookPath("busybox"); err == nil && p != "" {
		return &nsenterProg{path: p, args: []string{"nsenter"}}
	}
	return nil
}

// nsenterMissingError 构造「找不到 nsenter」的错误，按平台给出安装指引。
func nsenterMissingError() error {
	return fmt.Errorf("%w，请安装：\n"+
		"  - Linux:   sudo apt install util-linux（或 yum/dnf install util-linux）\n"+
		"  - Android: 安装 Magisk，或安装 busybox（Toybox 新版自带 nsenter）",
		ErrNoNsenter)
}

// nsFlags 是要进入的命名空间开关。
//
// 一律用**短选项**：util-linux、Toybox 与 busybox 三者都支持 `-t/-m/-u/-i/-n/-p`，
// 而 busybox 的 nsenter **不支持任何长选项**（`--target` 会直接报错），
// 因此长选项虽然在 util-linux 上更可读，却不是可移植选择。
//
// 顺序沿用 runc：mount → uts → ipc → net → pid。
var nsFlags = []string{"-m", "-u", "-i", "-n", "-p"}

// Enter 进入 targetPID 的命名空间并执行 cmd，返回子进程 pid（须随后 Wait）。
//
// 子进程的 stdio 用 in/out/err fd。工作目录与 uid/gid 都由 nsenter 自己设置
// （`-w<dir>` 与 `-S/-G`）：Go 侧先 chdir 是无效的——chdir 发生在宿主视图下，
// 进 mount namespace 后该 cwd 不再存在；先 setuid 则更不行，降权后无法 setns。
//
// helperPath 非空时，实际执行的是「helperPath exec-setup -- <cmd>」而不是 cmd
// 本身：helper 在容器内做权限收口（no_new_privs / cap-drop / seccomp）后再
// execve cmd。传空串保持原行为（直接执行 cmd）。
//
// 为什么必须经 helper：nsenter 进程来自宿主 root，继承**宿主满能力**；
// 容器 init 里的收口对它无效。不做这一步的话，任何能跑 licore exec 的人
// 都能拿到宿主 root 的全部能力。
//
// cgroupDir 非空时把新进程放进该 cgroup（容器资源限额据此生效）。
// 详见 enterWithCgroup 的说明。
func Enter(targetPID int, workdir, user string, env []string, inFd, outFd, errFd int, cmd []string, helperPath, cgroupDir string) (int, error) {
	prog := findNsenter()
	if prog == nil {
		return -1, nsenterMissingError()
	}
	// workdir 不再交给 nsenter 的 -w，而是经 argv 传给容器内的 helper。
	// 原因：nsenter 的 -w 是"先 chdir 再 setns"，chdir 发生在**宿主** mount
	// namespace；随后 setns 切到容器 mount ns，cwd 指向的 inode 在新视图里
	// 可能不存在，getcwd 直接失败（真机实测报 getcwd: No such file or
	// directory）。helper 本身就在容器 mount ns 内运行，它的 chdir 天然正确。
	argv, err := buildNsenterArgv(prog, targetPID, wrapWithHelper(helperPath, workdir, cmd))
	if err != nil {
		return -1, err
	}
	// **不再用 nsenter 的 -S/-G 做降权**（虽然两个实现的短选项写法一致）。
	//
	// 原因：-S/-G 在 setns 之后、**执行 helper 之前**就降权，于是容器内的
	// helper 是以目标 uid 运行的 —— 而 helper 要做 capability 裁剪
	// （PR_CAPBSET_DROP 需要 CAP_SETPCAP），非 root 身份下直接
	// `operation not permitted`（真机实测）。表现为
	// `licore exec -u 1000 ...` 报"裁剪能力失败"。
	//
	// 改为把 uid/gid 经环境变量交给 helper，由 helper 在**完成全部收口之后、
	// execve 用户命令之前**降权，与容器 init 的顺序一致。
	// user 参数保留在签名里（冻结接口），供 helper 侧的 env 构造使用；
	// 这里只做格式校验，不产生 nsenter 参数。
	if _, _, _, err := parseExecUser(user); err != nil {
		return -1, err
	}

	// #nosec G204：argv 由本函数按固定模板构造，目标命令来自用户显式指定的
	// exec 命令（与 docker exec 同性质），不存在 shell 展开。
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdin = os.NewFile(uintptr(inFd), "stdin")
	c.Stdout = os.NewFile(uintptr(outFd), "stdout")
	c.Stderr = os.NewFile(uintptr(errFd), "stderr")
	if len(env) > 0 {
		c.Env = env
	}

	// **首选路径：clone3(CLONE_INTO_CGROUP)**，让 nsenter 出生即在容器 cgroup。
	//
	// 这是唯一**无竞态**的做法：nsenter 会 setns 并 fork 出 helper 与用户命令，
	// 若等它跑起来再写 cgroup.procs，从 fork 到写入之间产生的子进程会逃脱限额
	// （写 cgroup.procs 只迁移该进程自身与其线程，**不迁移已存在的子进程**）。
	// CLONE_INTO_CGROUP 在内核 fork 阶段就完成归置，全部后代天然继承。
	cgroupFile, err := openCgroupForClone(cgroupDir)
	if err != nil {
		return -1, err
	}
	if cgroupFile != nil {
		defer func() { _ = cgroupFile.Close() }()
		c.SysProcAttr = &syscall.SysProcAttr{
			UseCgroupFD: true,
			CgroupFD:    int(cgroupFile.Fd()),
		}
	}

	if err := c.Start(); err != nil {
		// clone3 在本内核/本环境不可用时回退到"启动后迁移"。回退有竞态，
		// 但比"完全不归置"好得多，且会记 Debug 便于诊断。
		if cgroupFile != nil && isClone3Unsupported(err) {
			slog.Debug("clone3(CLONE_INTO_CGROUP) 不可用，回退为启动后迁移 cgroup",
				slog.String("cgroup", cgroupDir), slog.Any("err", err))
			c.SysProcAttr = nil
			if err2 := c.Start(); err2 != nil {
				return -1, fmt.Errorf("execns: 启动 nsenter 失败: %w", err2)
			}
			pid := c.Process.Pid
			if err3 := movePidToCgroup(pid, cgroupDir); err3 != nil {
				_ = c.Process.Kill()
				_, _ = c.Process.Wait()
				return -1, err3
			}
			return pid, nil
		}
		return -1, fmt.Errorf("execns: 启动 nsenter 失败: %w", err)
	}
	// 不再需要 Go 侧持有的 *os.File 包装：fd 已复制给子进程。
	// （NewFile 不接管 fd 的所有权语义在此处是刻意的——这些 fd 属于调用方。）
	return c.Process.Pid, nil
}

// openCgroupForClone 打开容器 cgroup 目录，供 SysProcAttr.CgroupFD 使用。
//
// 返回 (nil, nil) 表示无需归置（cgroupDir 为空），调用方保持旧行为。
// cgroupDir 非空但目录不存在时返回错误——静默跳过会让 exec 悄悄失去限额，
// 那正是本修复要消除的状态，不能降级。
func openCgroupForClone(cgroupDir string) (*os.File, error) {
	if cgroupDir == "" {
		return nil, nil
	}
	// O_RDONLY 即可：CLONE_INTO_CGROUP 要的是指向 cgroup v2 目录的 fd。
	// 打开而非仅在 Start 时传路径——fd 在 fork 期间必须保持有效。
	f, err := os.Open(cgroupDir)
	if err != nil {
		return nil, fmt.Errorf("execns: 打开容器 cgroup %s: %w", cgroupDir, err)
	}
	return f, nil
}

// isClone3Unsupported 判断 Start 的失败是否属于"clone3 或 CLONE_INTO_CGROUP
// 在本内核/本环境不可用"，从而值得回退。
//
// 依据：内核不支持 clone3 → ENOSYS；不支持 CLONE_INTO_CGROUP → EINVAL；
// 被 seccomp 拦 → EPERM。其余错误（如可执行文件不存在 ENOENT）不该回退，
// 重试只会得到同样的失败。
func isClone3Unsupported(err error) bool {
	for _, target := range []error{syscall.ENOSYS, syscall.EINVAL, syscall.EPERM, syscall.EOPNOTSUPP} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// movePidToCgroup 把**已经在运行的** pid 加入 cgroupDir 的 cgroup.procs。
//
// 这是 clone3 不可用时的**回退路径**（首选路径见 Enter 里的
// SysProcAttr.CgroupFD + CLONE_INTO_CGROUP）。
//
// 回退路径**有竞态**：从进程启动到写入之间，它已在旧 cgroup 里跑了一会儿；
// 且写 cgroup.procs 只迁移该进程自身与其线程，**不迁移已 fork 的子进程**。
// 因此回退仅用于 clone3/CLONE_INTO_CGROUP 不可用的环境，并记 Debug 便于诊断。
func movePidToCgroup(pid int, cgroupDir string) error {
	procs := filepath.Join(cgroupDir, "cgroup.procs")
	f, err := os.OpenFile(procs, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("execns: 打开 %s: %w", procs, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(strconv.Itoa(pid)); err != nil {
		return fmt.Errorf("execns: 把 pid %d 加入 %s: %w", pid, procs, err)
	}
	return nil
}

// buildNsenterArgv 构造完整的 nsenter 命令行。
//
// 形如：
//
//	nsenter -t <pid> -m -u -i -n -p -- <cmd> <args...>
//
// busybox 后备时为 `busybox nsenter -t ... -- ...`。
func buildNsenterArgv(prog *nsenterProg, targetPID int, cmd []string) ([]string, error) {
	if targetPID <= 0 {
		return nil, fmt.Errorf("execns: target PID 非法: %d", targetPID)
	}
	if len(cmd) == 0 {
		return nil, fmt.Errorf("execns: 命令为空")
	}
	if prog == nil || prog.path == "" {
		return nil, nsenterMissingError()
	}

	argv := make([]string, 0, len(prog.args)+len(nsFlags)+6+len(cmd))
	argv = append(argv, prog.path)
	argv = append(argv, prog.args...)
	argv = append(argv, "-t", strconv.Itoa(targetPID))
	argv = append(argv, nsFlags...)
	// 刻意**不加 -r/**。
	//
	// setns(CLONE_NEWNS) 之后进程的根目录**已经是容器 root**，nsenter 再补一次
	// chroot 属于多余操作，而且会把 cwd 搞坏：真机实测加 -r/ 后 `pwd` 报
	// `getcwd: No such file or directory`，不加则正常输出 `/`。
	//
	// 历史教训（本仓库曾经写反过）：早先的注释声称"不加 -r/ 时 cwd 会失效"，
	// 于是加上 -r/ 去"修"它——实测证明恰好相反，是 -r/ 导致了 cwd 失效；
	// 当时还据此加了 -w/ 去补偿，等于用第二个参数掩盖第一个参数造成的问题。
	// 现在两个默认都不下发：进容器后 cwd 天然就是 `/`，无需任何补偿。
	//
	// 刻意**不加 -w**：见 Enter 的说明——nsenter 的 -w 是"先 chdir 再 setns"，
	// 切到容器 mount namespace 后 cwd 失效。工作目录改由容器内的 helper
	// 自己 chdir（wrapWithHelper 把 workdir 放进 helper 的 argv）。
	// `--` 终止选项解析：目标命令自身可能以 '-' 开头，不加会被当成 nsenter 的选项。
	argv = append(argv, "--")
	argv = append(argv, cmd...)
	return argv, nil
}

// wrapWithHelper 把目标命令包成「经 helper 收口后再执行」的形式。
//
// helperPath 为空时原样返回 cmd（保持 Enter 的旧行为，便于测试与降级）。
// 非空时返回：
//
//	<helperPath> exec-setup -- <cmd...>
//
// 为什么必须经过 helper：nsenter 进程来自宿主 root，继承**宿主满能力**；
// 容器 init 里的 no_new_privs / cap-drop / seccomp 对它完全无效。
// 不做这一步，任何能跑 licore exec 的人都能拿到宿主 root 的全部能力。
//
// helperPath 是容器内路径（见 runtime.HelperPathInContainer）：nsenter 的
// -r/ 已把 root 切到容器，exec 的命令按容器视图解析。
func wrapWithHelper(helperPath, workdir string, cmd []string) []string {
	if helperPath == "" {
		return cmd
	}
	out := make([]string, 0, len(cmd)+6)
	out = append(out, helperPath, "exec-setup")
	// workdir 交给 helper 自己 chdir（见 Enter 的说明：nsenter 的 -w 会失效）。
	// 空值或 "/" 不下发，helper 保持当前 cwd（进容器后天然是 /）。
	if workdir != "" && workdir != "/" {
		out = append(out, "--workdir", workdir)
	}
	out = append(out, "--")
	out = append(out, cmd...)
	return out
}

// Wait 等待 Enter 返回的子进程并返回退出码（信号死亡 128+signum）。
//
// 说明：这里无法直接 wait 那个 pid——子进程是 exec.Command 启动的，
// 需要由启动它的 *exec.Cmd 来 Wait。为保持与旧实现的调用形状一致
// （Enter 返回 pid、Wait 收 pid），这里用「按 pid 轮询 + 从进程表 reap」
// 的方式实现，见 waitPid。
func Wait(pid int) int {
	return waitPid(pid)
}

// waitPid 等待指定 pid 退出并返回其退出码。
//
// 实现要点：子进程由 exec.Command 启动后，其 Cmd 对象在 Enter 返回时就
// 被丢弃了，这里只能靠 wait4 直接回收。若该 pid 已被其它 goroutine 回收
// （返回 ECHILD），则退化为通过 /proc 观察存活状态——拿不到退出码时
// 返回 0，并让调用方以「已结束」为准（与旧实现在极端情况下的行为一致）。
func waitPid(pid int) int {
	code, err := wait4(pid)
	if err == nil {
		return code
	}
	// ECHILD 等：尽力而为地等待进程从 /proc 消失。
	for processAlive(pid) {
		if !sleepBriefly() {
			break
		}
	}
	return 0
}

// processAlive 报告 pid 是否仍然存在。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// parseExecUser 解析 "uid[:gid]"。
//
// 返回 ok=false 表示未指定（user 为空），此时不动 nsenter 的默认行为。
func parseExecUser(user string) (uid, gid int, ok bool, err error) {
	if strings.TrimSpace(user) == "" {
		return 0, -1, false, nil
	}
	uidStr, gidStr, _ := strings.Cut(user, ":")
	v, e := strconv.Atoi(uidStr)
	if e != nil || v < 0 {
		return 0, -1, false, fmt.Errorf("execns: 非法用户 %q（应为数字 uid[:gid]）", user)
	}
	uid, gid = v, -1
	if gidStr != "" {
		g, e := strconv.Atoi(gidStr)
		if e != nil || g < 0 {
			return 0, -1, false, fmt.Errorf("execns: 非法 gid %q", gidStr)
		}
		gid = g
	}
	return uid, gid, true, nil
}

// insertUserFlags 把 -S <uid> [-G <gid>] 插到 nsenter 自身的选项区（"--" 之前）。
func insertUserFlags(argv []string, prog *nsenterProg, uid, gid int) []string {
	// argv 结构：[prog] [prog.args...] -t <pid> <nsFlags...> -- <cmd...>
	// 在 "--" 之前插入，保证不被当成目标命令的参数。
	cut := len(argv)
	for i, a := range argv {
		if a == "--" {
			cut = i
			break
		}
	}
	extra := []string{"-S", strconv.Itoa(uid)}
	if gid >= 0 {
		extra = append(extra, "-G", strconv.Itoa(gid))
	}
	out := make([]string, 0, len(argv)+len(extra))
	out = append(out, argv[:cut]...)
	out = append(out, extra...)
	out = append(out, argv[cut:]...)
	return out
}
