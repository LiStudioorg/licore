// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux && (amd64 || 386)

package runtime

// x86 专属的 seccomp 拦截项。
//
// 为什么必须按架构分文件而不是全写在一处：标准库**不导出** iopl/ioperm/
// sysfs/uselib/ustat 这些常量在 arm64/riscv64 上的定义（内核在这些平台
// 上根本没有对应调用），写在一起会让非 x86 交叉编译直接失败。
//
// 分层依据（实测各架构编译结果）：
//   - iopl/ioperm/sysfs：仅 amd64/386 有（x86 的 I/O 端口特权调用）；
//   - uselib/ustat：32 位遗留接口，amd64/386/arm 有，arm64/riscv64 没有。

import "syscall"

// archSpecificRules 返回 x86 平台额外的拦截项。
func archSpecificRules() []seccompRule {
	nr := func(name string, n uintptr) seccompRule {
		return seccompRule{Name: name, NR: uint32(n)}
	}
	return []seccompRule{
		nr("iopl", syscall.SYS_IOPL),     // 直接操作 I/O 端口权限位
		nr("ioperm", syscall.SYS_IOPERM), // 同上，按端口范围授权
		nr("sysfs", syscall.SYS_SYSFS),   // 已废弃的文件系统信息接口
		nr("uselib", syscall.SYS_USELIB), // 加载任意共享库的遗留接口
		nr("ustat", syscall.SYS_USTAT),   // 已废弃的文件系统统计
	}
}
