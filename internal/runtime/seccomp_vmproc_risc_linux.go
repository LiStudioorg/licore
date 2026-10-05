// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux && (arm64 || riscv64)

package runtime

import "syscall"

// arm64 / riscv64 上「跨进程内存访问」类系统调用的拦截项。
//
// 与 x86 不同，标准库在 asm-generic 平台上**导出了**这三个常量，因此
// 直接复用 syscall.SYS_*，不手写数字——少一处可能写错的地方。
//
// 放在按架构分文件的这里（而不是公共黑名单）是为了与 x86 那份保持同一
// 分层：两边都提供 archSpecificVMProcRules，seccomp_linux.go 统一调用，
// 编译期由 build tag 决定取哪一份。
func archSpecificVMProcRules() []seccompRule {
	nr := func(name string, n uintptr) seccompRule {
		return seccompRule{Name: name, NR: uint32(n)}
	}
	return []seccompRule{
		nr("process_vm_readv", syscall.SYS_PROCESS_VM_READV),
		nr("process_vm_writev", syscall.SYS_PROCESS_VM_WRITEV),
		nr("kcmp", syscall.SYS_KCMP),
	}
}
