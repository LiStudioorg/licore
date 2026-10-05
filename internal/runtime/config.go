// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// 子进程通过环境变量传递初始化参数（exec 后仍然存活）。
const (
	envInitMarker       = "LICORE_CHILD"    // 存在即表示本进程是容器 init
	envRootfs           = "LICORE_ROOTFS"   // 新根目录（宿主机路径）
	envHostname         = "LICORE_HOSTNAME" // 容器 UTS hostname
	envChildCmdPrefix   = "LICORE_ARG"      // LICORE_ARG0..N 用户命令 argv
	envChildCmdCountKey = "LICORE_ARGC"     // argv 参数个数
	envCID              = "LICORE_CID"      // 本次容器实例唯一 ID（旧根目录名后缀）
)

// 哨兵错误。
var (
	// ErrNotInit 表示当前进程不是被 fork 出来的容器 init。
	ErrNotInit = errors.New("licore/runtime: 当前进程不是容器 init")
	// ErrBadConfig 表示启动配置非法。
	ErrBadConfig = errors.New("licore/runtime: 配置非法")
	// ErrNotRoot 表示当前进程既非 root 又未显式允许 rootless（启动/exec 需特权）。
	ErrNotRoot = errors.New("licore/runtime: 需要 root")
	// ErrUnsupported 表示本平台尚无运行时后端（非 Linux 文件实现）。
	ErrUnsupported = errors.New("licore/runtime: 本平台运行时未实现（阶段 2 仅支持 linux）")
)

// Config 描述一次容器启动。
type Config struct {
	// Rootfs 是容器新根目录在宿主机上的路径，必须已存在。
	Rootfs string
	// Hostname 是容器 UTS 名，空则沿用默认。
	Hostname string
	// Cmd 是容器 1 号进程 argv，必填。
	Cmd []string
	// Env 是容器环境变量（KEY=VALUE）。
	Env []string
	// Rootless 强制走 user namespace 路线；false 时按 euid 自动决定。
	Rootless bool
}

// StartResult 是一次容器启动（父/shim 侧）的结果。
type StartResult struct {
	// ChildPID 是容器 init 进程在宿主上的 PID（shim 视角）。
	ChildPID int
	// ExitCode 是容器 1 号进程的退出码（信号死亡时为 128+signum）。
	ExitCode int
}

// ExecOptions 描述一次 `licore exec`：在运行中容器（由 TargetPID 所指 init
// 的命名空间）里执行命令。跨平台类型；Linux 后端实现。
type ExecOptions struct {
	// TargetPID 是容器 init 进程在宿主上的 PID（runtime.json 的 initPid）。
	TargetPID int
	// Cmd 是要执行的命令 argv（在容器 mount namespace 内解析，如 /bin/sh）。
	Cmd []string
	// Env 是命令环境变量（覆盖容器内环境）。
	Env []string
	// Workdir 是工作目录（容器内路径）；空则用 /。
	Workdir string
	// User 是 uid[:gid] 运行身份；空则保持当前。
	User string
	// Stdin/Stdout/Stderr 透传给命令；nil 时取 os.*。
	Stdin, Stdout, Stderr *os.File
	// TTY 表示申请伪终端（-t）。
	TTY bool
	// CgroupID 是容器 ID，用于把 exec 进程放入容器的 cgroup。
	//
	// v0.9.2 新增字段，**不改动既有签名**（按 AGENTS.md，新增接口需在
	// 《冻结接口》登记）。为空时跳过 cgroup 归置。
	//
	// 为什么必须有它：exec 走宿主侧 nsenter，新起的进程默认落在**调用者
	// （CLI）自己的 cgroup**——实测为 user.slice/user-0.slice/session-7.scope，
	// 而不是容器的 /licore/<id>。这使 `licore exec` 完全绕过 --memory /
	// --pids-limit 等所有资源限额：实测容器限额 256 MiB，exec 进去的进程
	// 吃到 400 MiB 也不被拦，宿主 OOM 风险直接回归。
	CgroupID string
}

// Validate 检查配置完备性。
func (c *Config) Validate() error {
	if c.Rootfs == "" {
		return fmt.Errorf("rootfs 为空: %w", ErrBadConfig)
	}
	fi, err := os.Stat(c.Rootfs)
	if err != nil {
		return fmt.Errorf("rootfs %s: %s: %w", c.Rootfs, err, ErrBadConfig)
	}
	if !fi.IsDir() {
		return fmt.Errorf("rootfs %s 不是目录: %w", c.Rootfs, ErrBadConfig)
	}
	if len(c.Cmd) == 0 {
		return fmt.Errorf("容器命令为空: %w", ErrBadConfig)
	}
	for _, seg := range c.Cmd {
		if strings.ContainsRune(seg, 0) {
			return fmt.Errorf("命令参数含 NUL: %w", ErrBadConfig)
		}
	}
	return nil
}

// IsInitProcess 报告当前进程是否为 fork 出来的容器 init。main.go 用它分流。
func IsInitProcess() bool { return os.Getenv(envInitMarker) == "1" }

// StartOptions 是 Start/StartWith 的可选参数（跨平台类型；Linux 后端生效）。
type StartOptions struct {
	// Stdin/Stdout/Stderr 透传给容器 init；nil 时取 os.Stdin/os.Stdout。
	Stdin, Stdout, Stderr *os.File
	// StopCh 非 nil 时：等待期间该通道可读，立即向 init 转发 SIGTERM，
	// Grace 后仍未退出则 SIGKILL（停止语义由调用方提供信号源）。
	StopCh <-chan struct{}
	// Grace 是 SIGTERM 到 SIGKILL 的宽限，默认 10s。
	Grace time.Duration
}
