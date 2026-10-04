// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux && arm

package runtime

// 32 位 ARM 的 seccomp 拦截项。
//
// 32 位 ARM 仍保留 uselib/ustat 这两个遗留接口（与 amd64/386 相同），
// 但它没有 x86 的 iopl/ioperm/sysfs。放在单独文件是因为 arm64/riscv64
// 连 uselib/ustat 都没有，标准库在这些平台不导出对应常量。

import "syscall"

// archSpecificRules 返回 32 位 ARM 额外的拦截项。
func archSpecificRules() []seccompRule {
	nr := func(name string, n uintptr) seccompRule {
		return seccompRule{Name: name, NR: uint32(n)}
	}
	return []seccompRule{
		nr("uselib", syscall.SYS_USELIB),
		nr("ustat", syscall.SYS_USTAT),
	}
}
