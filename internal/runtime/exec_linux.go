// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/LiStudioorg/licore/internal/execns"
	"github.com/LiStudioorg/licore/internal/resource"
)

// pty ioctl 常量（syscall 包未导出）。编码：dir<<30 | size<<16 | type<<8 | nr。
//
//	TIOCSPTLCK：向内核**写入** 4 字节（dir=1），参数是 int 指针，指向 0 = 解锁；
//	TIOCGPTN ：从内核**读出** 4 字节（dir=2），参数是 uint32 指针，返回从端号。
const (
	tiocsptlck = 0x40045431
	tiocgptn   = 0x80045430
)

// ExecOptions 定义见 config.go（跨平台）。

// Exec 在目标容器的命名空间里执行命令并等待退出，返回退出码（信号死亡时
// 128+signum）。需要 root（CAP_SYS_ADMIN）以 setns 进入他人命名空间。
//
// setns(mount namespace) 在纯 Go 下会失败（Go issue #9091），因此进入容器
// 命名空间由 internal/execns 交给系统的 nsenter 完成（util-linux / Toybox /
// busybox 均可）。本包不再依赖 cgo，CGO_ENABLED=0 即可获得完整 exec 能力。
//
// 伪终端分配、stdio 透传仍在本函数内完成（nsenter 不管 pty），工作目录与
// uid/gid 也由 Go 侧在 exec 前处理。
func Exec(o *ExecOptions) (int, error) {
	if err := o.validate(); err != nil {
		return -1, err
	}
	if o.TargetPID <= 0 {
		return -1, fmt.Errorf("exec: target PID 非法: %w", ErrBadConfig)
	}
	if os.Geteuid() != 0 {
		return -1, ErrNotRoot
	}
	// 这里刻意**不**预判 execns.Enabled()：直接交给 Enter 报错，才能拿到
	// 完整的安装指引（nsenterMissingError 带 apt/yum 与 Magisk/busybox 提示）。
	// 若在此处用裸哨兵提前返回，用户只会看到 "exec: exec 需要 nsenter"，
	// 丢失「该怎么装」这条最有用的信息。

	// 决定三个 stdio fd。
	stdin := firstNonNil(o.Stdin, os.Stdin)
	stdout := firstNonNil(o.Stdout, os.Stdout)
	stderr := firstNonNil(o.Stderr, os.Stderr)
	inFd := int(stdin.Fd())
	outFd := int(stdout.Fd())
	errFd := int(stderr.Fd())

	// TTY：开伪终端，slave 作为子进程 stdio，master 由本进程转发给调用方。
	var master *os.File
	if o.TTY {
		m, s, err := openpty()
		if err != nil {
			return -1, fmt.Errorf("openpty: %w", err)
		}
		master = m
		defer func() { _ = s.Close(); _ = master.Close() }()
		inFd, outFd, errFd = int(s.Fd()), int(s.Fd()), int(s.Fd())
	}

	// 经容器内的 helper 收口后再执行用户命令：helper 是 licore 自身，
	// 由容器启动时只读 bind 进 HelperPathInContainer（见 InstallExecHelper）。
	// 它会在 execve 用户命令之前做 no_new_privs / cap-drop / seccomp——
	// 否则 exec 出来的进程是宿主 root 满能力，容器 init 的隔离对它无效。
	//
	// 同时把 exec 进程放入容器的 cgroup：否则它落在调用者（CLI）自己的
	// cgroup 里，**完全绕过 --memory / --pids-limit 等资源限额**。
	cgroupDir := containerCgroupDir(o.CgroupID)
	pid, err := execns.Enter(o.TargetPID, o.Workdir, o.User, o.Env, inFd, outFd, errFd,
		o.Cmd, HelperPathInContainer, cgroupDir)
	if err != nil {
		return -1, err
	}
	if master != nil {
		relayLoop(master)
	}
	code := execns.Wait(pid)
	if code < 0 {
		return -1, fmt.Errorf("exec: 等待子进程失败")
	}
	return code, nil
}

// containerCgroupDir 由容器 ID 构造 cgroup 目录路径；ID 为空时返回空串
// （调用方据此跳过 cgroup 归置）。
//
// 路径约定与 internal/resource 保持一致：
//
//	<CgroupV2Mount>/<LiCoreGroup>/<containerID>
//	= /sys/fs/cgroup/licore/<id>
//
// 刻意复用 resource 的常量而不是硬编码：cgroup 挂载点与一级组名是
// **跨模块契约**（internal/resource 是写方，本包是读方/使用者），
// 硬编码会在任一方调整时静默失配——而失配的后果是 exec 悄悄失去限额，
// 恰恰是本次修复要消除的问题。
func containerCgroupDir(containerID string) string {
	if containerID == "" {
		return ""
	}
	return filepath.Join(resource.CgroupV2Mount, resource.LiCoreGroup, containerID)
}

// validate 检查 exec 选项。
func (o *ExecOptions) validate() error {
	if len(o.Cmd) == 0 {
		return fmt.Errorf("exec: 命令为空: %w", ErrBadConfig)
	}
	for _, seg := range o.Cmd {
		if strings.ContainsRune(seg, 0) {
			return fmt.Errorf("exec: 命令参数含 NUL: %w", ErrBadConfig)
		}
	}
	return nil
}

// 关于 -u/--user：**不在 Go 侧 setuid**。
//
// 曾经的 applyExecUser 在这里做 setgroups/setgid/setuid，nsenter 改造后已删除。
// 原因是它在新的执行模型下必然失效：Exec 需要先以 root 启动 nsenter 才能
// setns 进入容器命名空间，而 setuid 一旦发生就永久失去 CAP_SYS_ADMIN，
// 后续 setns 会直接 EPERM。因此 uid/gid 改由 nsenter 的 -S/-G 承担
// （见 internal/execns.insertUserFlags）——那两个开关是在**进入命名空间之后**
// 才生效的，语义才正确。此注释刻意保留，避免后来者"顺手"把 setuid 加回来。

// devPtmxPath / devPtsDir 是 pty 相关路径，做成变量以便测试注入。
//
// 生产环境恒为 /dev/ptmx 与 /dev/pts；测试通过注入临时 devpts 挂载点，
// 让 openpty 的分配逻辑（含 TIOCSPTLCK 指针语义）可被真实执行覆盖。
var (
	devPtmxPath = "/dev/ptmx"
	devPtsDir   = "/dev/pts"
)

// openpty 分配一个伪终端，返回 master/slave。从 /dev/ptmx 创建。
//
// 易错点：TIOCSPTLCK（解锁从端）的第三个参数是**指向 int 的指针**，不是
// 解锁值本身。内核会从该地址读 4 字节，因此传字面量 0 会让内核解引用地址 0
// 并返回 EFAULT（"bad address"）——这正是此前 `exec -it` 恒定失败的根因。
// 必须传 &unlock。两个 ioctl 的指针都要用 unsafe.Pointer 包裹，Go 的
// Syscall 不会阻止 GC 在调用期间移动/回收被指向的变量。
func openpty() (master, slave *os.File, err error) {
	m, err := os.OpenFile(devPtmxPath, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("打开 %s: %w", devPtmxPath, err)
	}
	// TIOCSPTLCK=0x40045431：解锁从端；参数是指针，指向 0 表示"解锁"。
	unlock := int32(0)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), tiocsptlck,
		uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		_ = m.Close()
		return nil, nil, fmt.Errorf("解锁从端: %w", errno)
	}
	// TIOCGPTN=0x80045430：取从端号。
	var n uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), tiocgptn,
		uintptr(unsafe.Pointer(&n))); errno != 0 {
		_ = m.Close()
		return nil, nil, fmt.Errorf("取从端号: %w", errno)
	}
	// 指向局部变量的指针跨 Syscall 使用后必须立即取用，避免被 GC 判定为死变量。
	runtime.KeepAlive(&n)
	s, err := os.OpenFile(filepath.Join(devPtsDir, strconv.Itoa(int(n))), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = m.Close()
		return nil, nil, fmt.Errorf("打开从端: %w", err)
	}
	return m, s, nil
}

// relayLoop 在 exec 等待期间把主端与调用方 stdio 双向转发（TTY 交互）。
func relayLoop(master *os.File) {
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				_, _ = os.Stdout.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				_, _ = master.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
}
