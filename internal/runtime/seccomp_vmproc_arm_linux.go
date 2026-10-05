// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux && arm

package runtime

// 32 位 ARM 上「跨进程内存访问」类系统调用的拦截项。
//
// **这里是本项目唯一一处必须手写编号、且无法与标准库交叉校验的架构**：
// 标准库在 arm(32) 上导出了 SYS_PROCESS_VM_READV=376 / SYS_PROCESS_VM_WRITEV=377，
// 但**没有**导出 SYS_KCMP（已核实 $GOROOT/src/syscall/zsysnum_linux_arm.go）。
//
// 取值依据（内核 arch/arm/tools/syscall.tbl，asm-generic 基线 + ARM 私有号）：
//
//	376  common  process_vm_readv
//	377  common  process_vm_writev
//	378  common  kcmp
//
// 前两项与标准库一致，说明该表与内核同步；kcmp 顺延一位（378），
// 与 arm64 的 (270,271,272) 三者连续的关系完全吻合。

import "syscall"

const nrKcmpArm = 378

// archSpecificVMProcRules 返回 32 位 ARM 的跨进程内存访问拦截项。
func archSpecificVMProcRules() []seccompRule {
	nr := func(name string, n uintptr) seccompRule {
		return seccompRule{Name: name, NR: uint32(n)}
	}
	return []seccompRule{
		nr("process_vm_readv", syscall.SYS_PROCESS_VM_READV),
		nr("process_vm_writev", syscall.SYS_PROCESS_VM_WRITEV),
		nr("kcmp", nrKcmpArm),
	}
}
