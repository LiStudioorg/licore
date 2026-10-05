// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux && (amd64 || 386)

package runtime

// amd64 / 386 上「跨进程内存访问」类系统调用的编号。
//
// **为什么手写而不复用 syscall.SYS_\***：标准库在 x86 上不导出这三个常量。
// 已核实 $GOROOT/src/syscall/zsysnum_linux_amd64.go 与 zsysnum_linux_386.go
// 中均无 SYS_PROCESS_VM_READV / SYS_PROCESS_VM_WRITEV / SYS_KCMP
// （arm64 / riscv64 反而有）。这正是 v0.9.0 把它们留在 seccomp「已知未覆盖」
// 清单里的原因。
//
// 取值依据（内核 arch/x86/entry/syscalls/syscall_64.tbl，x86_64）：
//
//	309  common  kcmp
//	310  common  process_vm_readv
//	311  common  process_vm_writev
//
// 三者都是 common，即 64 位与 32 位 compat 共用同一组号（ia32 表里
// "310 i386 process_vm_readv" 与之一致），因此一份常量覆盖两种 ABI。
//
// **编号写错的后果**：seccomp 按号比较，号错了就是拦了别的调用，
// 会造成莫名其妙的 EPERM 而不报错。因此 seccomp_vmproc_linux_test.go 里有
// 交叉校验：在能编译出对应常量的平台上，用内核实际号比对（见测试）。
const (
	nrProcessVMReadv  = 310
	nrProcessVMWritev = 311
	nrKcmp            = 309
)

// archSpecificVMProcRules 返回 x86 的跨进程内存访问拦截项。
func archSpecificVMProcRules() []seccompRule {
	nr := func(name string, n uintptr) seccompRule {
		return seccompRule{Name: name, NR: uint32(n)}
	}
	return []seccompRule{
		nr("process_vm_readv", nrProcessVMReadv),
		nr("process_vm_writev", nrProcessVMWritev),
		nr("kcmp", nrKcmp),
	}
}
