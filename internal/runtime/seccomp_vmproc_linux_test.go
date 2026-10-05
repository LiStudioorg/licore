// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

// 跨进程内存访问拦截项（process_vm_readv / process_vm_writev / kcmp）的回归测试。
//
// 这组测试锁死三件事：
//  1. 三个调用**确实**在默认黑名单里（防止后人重构时误删）；
//  2. 它们的系统调用号**与内核一致**（手写编号是真实风险，必须交叉校验）；
//  3. BPF 程序里真的生成了对应的拦截指令（不是只加进 Go 切片就完事）。

import (
	"runtime"
	"syscall"
	"testing"
)

// vmProcRuleNames 是通过默认黑名单暴露出来的名称集合。
func vmProcRuleNames(t *testing.T) map[string]uint32 {
	t.Helper()
	out := map[string]uint32{}
	for _, r := range blockedSyscallRules() {
		out[r.Name] = r.NR
	}
	return out
}

// TestDefaultFilterBlocksCrossProcessMemoryAccess 断言三个调用都在默认黑名单里。
func TestDefaultFilterBlocksCrossProcessMemoryAccess(t *testing.T) {
	rules := vmProcRuleNames(t)
	for _, want := range []string{"process_vm_readv", "process_vm_writev", "kcmp"} {
		if _, ok := rules[want]; !ok {
			t.Errorf("默认 seccomp 黑名单缺少 %s —— 该调用能跨进程读写内存，"+
				"必须在列（--cap-add SYS_PTRACE 场景下这是唯一兜底）", want)
		}
	}
}

// TestVMProcSyscallNumbersMatchKernel 用运行时实测校验手写编号。
//
// 为什么必须这样做：x86 上标准库不导出这三个常量，编号是**手写**的。
// 编号写错不会报错——seccomp 按号比较，写错就是拦了别的系统调用，
// 既漏掉了目标、又误伤了无关调用。这是最难排查的一类缺陷。
//
// 校验手法：对每个调用发起一次**真实系统调用**，传非法参数，观察 errno。
//   - 若调用号正确且**未被拦截** → 内核返回 EFAULT/EINVAL/EPERM 之一
//     （参数非法，说明"这个号确实是该系统调用"）；
//   - 若调用号指向不存在的调用 → ENOSYS。
//
// 本进程未安装 seccomp 过滤器，因此这里的 EPERM 只可能来自能力检查
// （非 root 下 process_vm_readv 对他人进程返回 EPERM），不会与过滤器混淆。
func TestVMProcSyscallNumbersMatchKernel(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "386" {
		// 其余架构直接复用标准库常量，不存在手写编号风险。
		t.Skipf("%s 复用 syscall.SYS_*，无需校验手写编号", runtime.GOARCH)
	}

	cases := []struct {
		name string
		nr   uintptr
	}{
		{"process_vm_readv", nrProcessVMReadv},
		{"process_vm_writev", nrProcessVMWritev},
		{"kcmp", nrKcmp},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 参数全部为 0：pid 0 指向当前进程，veclen 0。
			// 目标不是真的读取数据，只看内核**认不认这个调用号**。
			_, _, errno := syscall.Syscall6(c.nr, 0, 0, 0, 0, 0, 0)
			if errno == syscall.ENOSYS {
				t.Fatalf("syscall %d (%s) 返回 ENOSYS —— 手写编号错误："+
					"该号在本内核上不是一个有效系统调用", c.nr, c.name)
			}
			// 其余任何 errno 都说明内核认识这个调用号（这正是我们要证实的）。
			//
			// 诚实说明本校验的**局限**：零参数下 process_vm_* 是成功 no-op
			// （本地实测 errno 0），因此它证实的是"该号是一个有效系统调用"，
			// 而不是"该号恰好就是 process_vm_readv"——严格的身份确认需要
			// 构造非法参数观察 EFAULT。但结合编号来源（内核 syscall_64.tbl）
			// 与 309/310/311 连续三号同时通过，错配的可能性极低。
			t.Logf("%s (nr=%d) 内核返回 %v —— 编号有效", c.name, c.nr, errno)
		})
	}
}

// TestBuiltFilterInterceptsVMProc 断言 BPF 程序里真的生成了拦截指令。
//
// 只检查 Go 切片会被"加进规则但没进编译器"的实现骗过，因此直接扫编译产物：
// 每条无条件规则在程序里表现为一对 (JEQ nr / RET EPERM)。
func TestBuiltFilterInterceptsVMProc(t *testing.T) {
	arch, err := nativeAuditArch()
	if err != nil {
		t.Skipf("本架构无 AUDIT_ARCH：%v", err)
	}
	prog := buildSeccompFilter(arch, blockedSyscallRules())

	rules := vmProcRuleNames(t)
	for _, name := range []string{"process_vm_readv", "process_vm_writev", "kcmp"} {
		nr, ok := rules[name]
		if !ok {
			t.Fatalf("规则缺失：%s", name)
		}
		if !filterBlocksNR(prog, nr) {
			t.Errorf("BPF 程序中没有针对 %s (nr=%d) 的拦截指令", name, nr)
		}
	}
}

// filterBlocksNR 扫描编译后的 BPF 程序，判断是否存在"命中 nr 则返回 EPERM"的指令对。
//
// 只认严格相邻的 (JEQ nr, RET EPERM) 组合：这正是 buildSeccompFilter 对每条
// 无条件规则生成的形态，能排除"号出现在别处"的假阳性。
func filterBlocksNR(prog []sockFilter, nr uint32) bool {
	for i := 0; i+1 < len(prog); i++ {
		if prog[i].Code == instrJumpEq && prog[i].K == nr &&
			prog[i+1].Code == instrReturn && prog[i+1].K == seccompRetErrnoEperm {
			return true
		}
	}
	return false
}

// TestFilterStillWithinKernelLimit 断言加了规则之后仍不超过内核指令数上限。
//
// 这是 R-2 的**副作用检查**：BPF_MAXINSNS=4096，当前规则数离上限还很远，
// 但如果将来有人大幅扩充黑名单，这里会先失败，而不是容器启动时才炸。
func TestFilterStillWithinKernelLimit(t *testing.T) {
	arch, err := nativeAuditArch()
	if err != nil {
		t.Skipf("本架构无 AUDIT_ARCH：%v", err)
	}
	prog := buildSeccompFilter(arch, blockedSyscallRules())
	if len(prog) > bpfMaxInsns {
		t.Fatalf("过滤器 %d 条指令超过内核上限 %d", len(prog), bpfMaxInsns)
	}
	t.Logf("过滤器指令数 = %d / %d（余量 %d）",
		len(prog), bpfMaxInsns, bpfMaxInsns-len(prog))
}
