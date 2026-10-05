// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

// 本文件实现 seccomp-bpf 基础过滤（纯 Go，无 cgo）：用黑名单拦掉一批
// 明确危险的系统调用，作为 capability 裁剪之外的纵深防御。
//
// 为什么是黑名单而不是 Docker 那样的白名单：
//   - Docker 的 default profile 是"列出约 300 个允许的 syscall"的白名单。
//     白名单更严，但它会**拒绝未来新增的 syscall**——内核加新调用而本表
//     没跟上，用户程序就会莫名 EPERM，且很难排查。
//   - 黑名单不会拦到未知调用，兼容性好得多；在已经丢光能力的前提下，
//     它挡的是"万一某个能力被放回来"时的二次利用路径。
//
// 拦截动作统一为 SECCOMP_RET_ERRNO|EPERM（而不是 KILL）：进程拿到 EPERM
// 能给出可读的错误，而 KILL 会让容器静默消失、极难排查。Docker 默认
// 也是返回 EPERM。
//
// 前置条件：安装 seccomp 过滤器要求当前进程已设 PR_SET_NO_NEW_PRIVS
// （或持有 CAP_SYS_ADMIN）。前者由 executeContainerCmd 的第一步保证，
// 因此即使能力已被裁剪、CAP_SYS_ADMIN 已不在，本调用依然成立。

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// prctl 选项与 seccomp 模式（linux/prctl.h、linux/seccomp.h）。
const (
	prSetSeccomp      = 22
	seccompModeFilter = 2
)

// BPF 指令编码（linux/filter.h）。标准库未导出，自行定义。
const (
	bpfLD  = 0x00 // BPF_LD
	bpfW   = 0x00 // BPF_W
	bpfABS = 0x20 // BPF_ABS
	bpfJMP = 0x05 // BPF_JMP
	bpfJEQ = 0x10 // BPF_JEQ
	bpfK   = 0x00 // BPF_K
	bpfRET = 0x06 // BPF_RET
	bpfALU = 0x04 // BPF_ALU
	bpfAND = 0x50 // BPF_AND
)

// 组合后的指令码。
const (
	instrLoadAbs = bpfLD | bpfW | bpfABS  // 0x20：A = *(u32 *)(data + k)
	instrJumpEq  = bpfJMP | bpfJEQ | bpfK // 0x15：if A == k jump
	instrReturn  = bpfRET | bpfK          // 0x06：return k
	instrAluAnd  = bpfALU | bpfAND | bpfK // 0x54：A &= k
)

// seccomp 返回值（linux/seccomp.h）。
const (
	seccompRetAllow = 0x7fff0000
	// SECCOMP_RET_ERRNO | EPERM：让被拦的系统调用返回 EPERM。
	seccompRetErrnoEperm = 0x00050000 | uint32(syscall.EPERM)
)

// struct seccomp_data（linux/seccomp.h）的字段偏移。
const (
	seccompOffNR   = 0  // int nr
	seccompOffArch = 4  // __u32 arch
	seccompOffArg0 = 16 // __u64 args[0]
)

// bpfMaxInsns 是内核对单个 seccomp 过滤器的指令数上限（BPF_MAXINSNS）。
const bpfMaxInsns = 4096

// CLONE_NEWUSER：容器内自行创建 user namespace 的逃逸路径，单独按参数拦截。
const cloneNewuser = 0x10000000

// sockFilter 对应 struct sock_filter。
type sockFilter struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

// sockFprog 对应 struct sock_fprog{ unsigned short len; struct sock_filter *filter; }。
//
// 不手写填充：Go 会按目标平台的指针对齐自动补齐（64 位下 6 字节、32 位下
// 2 字节），与内核结构一致。seccomp_linux_test.go 用 unsafe.Sizeof/Offsetof
// 把这一点钉死。
type sockFprog struct {
	Len    uint16
	Filter *sockFilter
}

// seccompRule 描述一条拦截规则。
type seccompRule struct {
	// Name 是 syscall 名（仅用于日志与测试断言）。
	Name string
	// NR 是系统调用号。
	NR uint32
	// ArgMask 非 0 时表示"只在 args[0] & ArgMask != 0 时拦截"；
	// 为 0 表示无条件拦截该系统调用。
	ArgMask uint32
}

// nativeAuditArch 返回当前架构的 AUDIT_ARCH_* 值（linux/audit.h）。
//
// 校验 arch 是必要的：同一套 syscall 号在不同 ABI 下含义不同，
// 不校验就可能被 32 位 ABI（int 0x80）绕过黑名单。
func nativeAuditArch() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return 0xC000003E, nil
	case "386":
		return 0x40000003, nil
	case "arm64":
		return 0xC00000B7, nil
	case "arm":
		return 0x40000028, nil
	case "riscv64":
		return 0xC00000F3, nil
	}
	// 未知架构不能返回 0：那会让 arch 校验恒不等，把容器里的所有系统调用
	// 都拒掉（比不装过滤器严重得多）。交给调用方报错。
	return 0, fmt.Errorf("seccomp: 不支持在 %s 上安装过滤器（未知 AUDIT_ARCH）", runtime.GOARCH)
}

// blockedSyscallRules 返回默认黑名单。
//
// 选材参考 Docker 的 default seccomp profile 中**无条件拦截**的那些项
// （Docker 里还需要按参数过滤的 clone/personality/socket 等不在内）。
//
// 只使用标准库导出的 syscall.SYS_* 常量，好处是系统调用号由 Go 按目标
// 架构生成，不会写错；代价是标准库没导出的调用无法列入，见文件末尾的
// 《已知未覆盖》注释。
func blockedSyscallRules() []seccompRule {
	nr := func(name string, n uintptr) seccompRule {
		return seccompRule{Name: name, NR: uint32(n)}
	}
	rules := []seccompRule{
		// 内核与固件：能直接搞死或篡改宿主内核。
		nr("reboot", syscall.SYS_REBOOT),
		nr("kexec_load", syscall.SYS_KEXEC_LOAD),
		nr("init_module", syscall.SYS_INIT_MODULE),
		nr("delete_module", syscall.SYS_DELETE_MODULE),
		// 交换与记账：影响宿主全局状态。
		nr("swapon", syscall.SYS_SWAPON),
		nr("swapoff", syscall.SYS_SWAPOFF),
		nr("acct", syscall.SYS_ACCT),
		// 宿主时钟。
		nr("settimeofday", syscall.SYS_SETTIMEOFDAY),
		nr("clock_settime", syscall.SYS_CLOCK_SETTIME),
		nr("adjtimex", syscall.SYS_ADJTIMEX),
		// 调试与 I/O 特权端口。
		nr("ptrace", syscall.SYS_PTRACE),
		// 内核信息与陈旧接口。
		nr("lookup_dcookie", syscall.SYS_LOOKUP_DCOOKIE),
		nr("perf_event_open", syscall.SYS_PERF_EVENT_OPEN),
		// 命名空间与挂载：能力已丢，这里做二次封堵。
		nr("unshare", syscall.SYS_UNSHARE),
		nr("mount", syscall.SYS_MOUNT),
		nr("umount2", syscall.SYS_UMOUNT2),
		nr("pivot_root", syscall.SYS_PIVOT_ROOT),
		// 内核密钥环：常被用于容器逃逸的载荷投放。
		nr("add_key", syscall.SYS_ADD_KEY),
		nr("keyctl", syscall.SYS_KEYCTL),
		nr("request_key", syscall.SYS_REQUEST_KEY),
		// NUMA / 磁盘配额。
		nr("mbind", syscall.SYS_MBIND),
		nr("move_pages", syscall.SYS_MOVE_PAGES),
		nr("set_mempolicy", syscall.SYS_SET_MEMPOLICY),
		nr("get_mempolicy", syscall.SYS_GET_MEMPOLICY),
		nr("quotactl", syscall.SYS_QUOTACTL),
	}
	// clone(CLONE_NEWUSER)：不封 clone 本身（线程创建依赖它），
	// 只在带 CLONE_NEWUSER 时拒绝——这正是"容器内自建 userns 拿全部能力"
	// 的逃逸路径。
	rules = append(rules, seccompRule{
		Name: "clone(CLONE_NEWUSER)", NR: uint32(syscall.SYS_CLONE), ArgMask: cloneNewuser,
	})
	// 跨进程内存访问：process_vm_readv/writev 能直接读写**其它进程的内存**，
	// kcmp 能比较两个进程的 fd/内存状态（经典侧信道）。三者都需
	// CAP_SYS_PTRACE，该能力已被默认集丢弃，因此这里是**第二层**——
	// 但它挡的正是"用户 --cap-add SYS_PTRACE 之后"的场景：ptrace 本身有
	// 无条件拦截兜底，这三条原先没有，会出现"加了 SYS_PTRACE 就能绕过"的洞。
	//
	// 编号按架构分文件（seccomp_vmproc_*_linux.go）：x86 上标准库不导出这些
	// 常量，只能手写；arm64/riscv64 直接复用 syscall.SYS_*。
	rules = append(rules, archSpecificVMProcRules()...)

	// 架构专属项：iopl/ioperm/sysfs 只存在于 x86，写在按架构分文件的
	// archSpecificRules 里，否则非 x86 平台无法编译（标准库不导出这些常量）。
	return append(rules, archSpecificRules()...)
}

// buildSeccompFilter 把规则编译成 BPF 程序。
//
// 程序结构（A 为累加器）：
//
//	0: A = seccomp_data.arch
//	1: if A == native arch → 跳到 3，否则落到 2
//	2: return ERRNO(EPERM)          ← 架构不符：拒绝
//	3: A = seccomp_data.nr
//	4..: 每条无条件规则 2 条指令；每条带参数规则 5 条指令
//	末: return ALLOW
//
// 无条件规则必须全部排在带参数规则**之前**：带参数规则会重新从内存加载
// args[0] 冲掉 A 里的 nr，之后再用 JEQ nr 比较就会错。两趟构造保证了这个
// 顺序，而不是依赖调用方传参顺序。
func buildSeccompFilter(arch uint32, rules []seccompRule) []sockFilter {
	prog := make([]sockFilter, 0, 4+2*len(rules)+bpfMaxInsns/64)

	// 头：校验 arch。
	prog = append(prog,
		sockFilter{Code: instrLoadAbs, K: seccompOffArch},
		// arch 相符则跳过下一条（jt=1），否则执行下一条（jf=0）。
		sockFilter{Code: instrJumpEq, Jt: 1, Jf: 0, K: arch},
		sockFilter{Code: instrReturn, K: seccompRetErrnoEperm},
		// 载入 nr 供后续逐条比较。
		sockFilter{Code: instrLoadAbs, K: seccompOffNR},
	)

	// 第一趟：无条件规则。
	for _, r := range rules {
		if r.ArgMask != 0 {
			continue
		}
		prog = append(prog,
			// 命中则执行下一条 RET（jt=0），否则跳过它（jf=1）。
			sockFilter{Code: instrJumpEq, Jt: 0, Jf: 1, K: r.NR},
			sockFilter{Code: instrReturn, K: seccompRetErrnoEperm},
		)
	}

	// 第二趟：带参数规则。跳转偏移是块内固定值：jf=4 跳过本块的
	// 后 4 条指令（LD/AND/JEQ/RET），落到块外的下一条。
	for _, r := range rules {
		if r.ArgMask == 0 {
			continue
		}
		prog = append(prog,
			sockFilter{Code: instrJumpEq, Jt: 0, Jf: 4, K: r.NR},
			sockFilter{Code: instrLoadAbs, K: seccompOffArg0},
			sockFilter{Code: instrAluAnd, K: r.ArgMask},
			// 掩码位为 0（未请求该 flag）→ 跳过下一条 RET（jt=1）。
			sockFilter{Code: instrJumpEq, Jt: 1, Jf: 0, K: 0},
			sockFilter{Code: instrReturn, K: seccompRetErrnoEperm},
		)
	}

	prog = append(prog, sockFilter{Code: instrReturn, K: seccompRetAllow})
	return prog
}

// installSeccompFilter 安装默认 seccomp 过滤器。
//
// 调用前必须已设置 PR_SET_NO_NEW_PRIVS（见 executeContainerCmd），否则
// 无 CAP_SYS_ADMIN 时 prctl 会返回 EACCES。
func installSeccompFilter() error {
	arch, err := nativeAuditArch()
	if err != nil {
		return err
	}
	prog := buildSeccompFilter(arch, blockedSyscallRules())
	if len(prog) > bpfMaxInsns {
		return fmt.Errorf("seccomp: 过滤器 %d 条指令超过内核上限 %d", len(prog), bpfMaxInsns)
	}
	fprog := sockFprog{Len: uint16(len(prog)), Filter: &prog[0]}

	_, _, errno := syscall.Syscall(syscall.SYS_PRCTL,
		uintptr(prSetSeccomp), uintptr(seccompModeFilter), uintptr(unsafe.Pointer(&fprog)))
	// 保证 prog 在系统调用期间存活（GC 不会移动对象，但显式保持引用更稳）。
	runtime.KeepAlive(prog)
	if errno != 0 {
		return fmt.Errorf("prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER): %w", errno)
	}
	return nil
}

// SeccompFilterRuleNames 返回默认黑名单里的 syscall 名（供日志与测试使用）。
func SeccompFilterRuleNames() []string {
	rules := blockedSyscallRules()
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.Name)
	}
	return out
}

// 已知未覆盖（诚实记录，不假装完整）
//
// 下列 Docker default profile 会拦、但本实现**没有**拦截的调用：
//
//	bpf, userfaultfd, open_by_handle_at, name_to_handle_at,
//	kexec_file_load, finit_module, clock_adjtime
//
// 其中 bpf 需 CAP_BPF/CAP_SYS_ADMIN、finit_module 需 CAP_SYS_MODULE、
// kexec_file_load 需 CAP_SYS_BOOT，而这些能力都已被默认集丢弃，
// 因此实际风险有限；但这是**纵深防御的第二层**，不应假定它完整。
//
// v0.9.0 起新增覆盖（原在本清单内，已修）：
//
//	process_vm_readv, process_vm_writev, kcmp
//
// 仍然只拦 clone(CLONE_NEWUSER)，未拦 CLONE_NEWPID/NEWNS/NEWNET 等标志组合
// ——这些同样需 CAP_SYS_ADMIN，已在 capability 层丢弃；**不建议**无条件拦
// clone 的命名空间标志，会破坏 systemd 容器、嵌套构建等合法负载。
