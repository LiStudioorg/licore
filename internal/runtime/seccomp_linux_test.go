// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// -------- BPF 结构与程序骨架 --------

// TestSockFprogLayoutMatchesABI 断言 sock_fprog / sock_filter 布局与内核一致。
//
// 布局错了会让 prctl 读到垃圾长度或野指针——比直接失败更危险（可能装上一个
// 语义完全不同的过滤器）。用偏移断言而非"跑一次没崩"，与 capability 那边同一手法。
func TestSockFprogLayoutMatchesABI(t *testing.T) {
	// struct sock_filter { __u16 code; __u8 jt; __u8 jf; __u32 k; }
	if got := unsafe.Sizeof(sockFilter{}); got != 8 {
		t.Errorf("sockFilter 大小 = %d，ABI 要求 8", got)
	}
	if got := unsafe.Offsetof(sockFilter{}.Code); got != 0 {
		t.Errorf("sockFilter.Code 偏移 = %d，要求 0", got)
	}
	if got := unsafe.Offsetof(sockFilter{}.Jt); got != 2 {
		t.Errorf("sockFilter.Jt 偏移 = %d，要求 2", got)
	}
	if got := unsafe.Offsetof(sockFilter{}.Jf); got != 3 {
		t.Errorf("sockFilter.Jf 偏移 = %d，要求 3", got)
	}
	if got := unsafe.Offsetof(sockFilter{}.K); got != 4 {
		t.Errorf("sockFilter.K 偏移 = %d，要求 4", got)
	}
	// struct sock_fprog { unsigned short len; struct sock_filter *filter; }
	if got := unsafe.Offsetof(sockFprog{}.Len); got != 0 {
		t.Errorf("sockFprog.Len 偏移 = %d，要求 0", got)
	}
	// 指针自然对齐：64 位在偏移 8、总大小 16；32 位在偏移 4、总大小 8。
	wantPtrOff, wantSize := uintptr(8), uintptr(16)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		wantPtrOff, wantSize = 4, 8
	}
	if got := unsafe.Offsetof(sockFprog{}.Filter); got != wantPtrOff {
		t.Errorf("sockFprog.Filter 偏移 = %d，要求 %d", got, wantPtrOff)
	}
	if got := unsafe.Sizeof(sockFprog{}); got != wantSize {
		t.Errorf("sockFprog 大小 = %d，要求 %d", got, wantSize)
	}
}

// TestBuildSeccompFilterStructure 验证程序骨架：arch 校验头 + 逐条规则 + ALLOW 收尾。
func TestBuildSeccompFilterStructure(t *testing.T) {
	const arch = 0xC000003E
	rules := []seccompRule{
		{Name: "reboot", NR: 169},
		{Name: "ptrace", NR: 101},
	}
	prog := buildSeccompFilter(arch, rules)

	want := 4 + 2*len(rules) + 1 // 头 4 + 每条 2 + 收尾 1
	if len(prog) != want {
		t.Fatalf("指令数 = %d，期望 %d", len(prog), want)
	}
	if prog[0].Code != instrLoadAbs || prog[0].K != seccompOffArch {
		t.Errorf("第 0 条应为 LD arch，得到 %+v", prog[0])
	}
	if prog[1].Code != instrJumpEq || prog[1].K != arch {
		t.Errorf("第 1 条应为 JEQ arch，得到 %+v", prog[1])
	}
	if prog[1].Jt != 1 || prog[1].Jf != 0 {
		t.Errorf("arch 相符应跳过下一条（jt=1 jf=0），得到 jt=%d jf=%d", prog[1].Jt, prog[1].Jf)
	}
	if prog[2].Code != instrReturn || prog[2].K != seccompRetErrnoEperm {
		t.Errorf("第 2 条应为 arch 不符时 ERRNO(EPERM)，得到 %+v", prog[2])
	}
	if prog[3].Code != instrLoadAbs || prog[3].K != seccompOffNR {
		t.Errorf("第 3 条应为 LD nr，得到 %+v", prog[3])
	}
	if last := prog[len(prog)-1]; last.Code != instrReturn || last.K != seccompRetAllow {
		t.Errorf("末条应为 RET ALLOW，得到 %+v", last)
	}
	for i, r := range rules {
		base := 4 + 2*i
		if prog[base].Code != instrJumpEq || prog[base].K != r.NR {
			t.Errorf("规则 %s 的 JEQ 不正确: %+v", r.Name, prog[base])
		}
		if prog[base].Jt != 0 || prog[base].Jf != 1 {
			t.Errorf("规则 %s 应为 jt=0 jf=1，得到 jt=%d jf=%d", r.Name, prog[base].Jt, prog[base].Jf)
		}
		if prog[base+1].K != seccompRetErrnoEperm {
			t.Errorf("规则 %s 后应跟 RET ERRNO(EPERM)，得到 %+v", r.Name, prog[base+1])
		}
	}
}

// TestBuildSeccompFilterArgRuleComesLast 验证带参数的规则被排到最后，
// 且块内跳转偏移自洽。
//
// 顺序是**正确性**要求而非风格：带参数规则会重新加载 args[0]，冲掉累加器里
// 的 nr；若它后面还有 JEQ nr 的比较，那些比较就是拿 args[0] 的低 32 位去比，
// 结果完全错误——这正是"过滤器静默放行"的典型来源。用例故意把带参数规则
// 放在入参**前面**，验证两趟构造能纠正它。
func TestBuildSeccompFilterArgRuleComesLast(t *testing.T) {
	rules := []seccompRule{
		{Name: "clone(CLONE_NEWUSER)", NR: 56, ArgMask: cloneNewuser},
		{Name: "reboot", NR: 169},
	}
	prog := buildSeccompFilter(0xC000003E, rules)

	if prog[4].Code != instrJumpEq || prog[4].K != 169 {
		t.Fatalf("第 4 条应为 reboot 的 JEQ(169)，得到 %+v（无条件规则未排在前）", prog[4])
	}
	base := len(prog) - 1 - 5 // 收尾 ALLOW 之前正好是 5 条的带参数块
	block := prog[base : base+5]
	if block[0].Code != instrJumpEq || block[0].K != 56 || block[0].Jf != 4 {
		t.Errorf("带参数块首条应为 JEQ(56, jf=4)，得到 %+v", block[0])
	}
	if block[1].Code != instrLoadAbs || block[1].K != seccompOffArg0 {
		t.Errorf("应加载 args[0]，得到 %+v", block[1])
	}
	if block[2].Code != instrAluAnd || block[2].K != cloneNewuser {
		t.Errorf("应按 CLONE_NEWUSER 掩码，得到 %+v", block[2])
	}
	if block[3].Code != instrJumpEq || block[3].K != 0 || block[3].Jt != 1 {
		t.Errorf("掩码为 0 时应跳过 RET（jt=1），得到 %+v", block[3])
	}
	if block[4].K != seccompRetErrnoEperm {
		t.Errorf("带参数块的 RET 应为 ERRNO(EPERM)，得到 %+v", block[4])
	}
	if dst := base + 1 + int(block[0].Jf); dst != base+5 {
		t.Errorf("jf=4 落到 %d，期望块后位置 %d（不能越界或落回块内）", dst, base+5)
	}
}

// TestSeccompFilterWithinInstructionLimit 验证指令数在内核上限内。
func TestSeccompFilterWithinInstructionLimit(t *testing.T) {
	arch, err := nativeAuditArch()
	if err != nil {
		t.Skipf("本架构不支持: %v", err)
	}
	prog := buildSeccompFilter(arch, blockedSyscallRules())
	if len(prog) > bpfMaxInsns {
		t.Errorf("指令数 %d 超过内核上限 %d（会 EINVAL）", len(prog), bpfMaxInsns)
	}
	t.Logf("默认过滤器 %d 条指令（上限 %d）", len(prog), bpfMaxInsns)
}

// TestBlockedRulesCoverRequiredSyscalls 锁定必拦项清单。
func TestBlockedRulesCoverRequiredSyscalls(t *testing.T) {
	names := map[string]bool{}
	for _, n := range SeccompFilterRuleNames() {
		names[n] = true
	}
	for _, want := range []string{
		"reboot", "kexec_load", "init_module", "delete_module",
		"swapon", "swapoff", "acct", "settimeofday", "clock_settime", "ptrace",
	} {
		if !names[want] {
			t.Errorf("默认黑名单缺少 %s", want)
		}
	}
	if !names["clone(CLONE_NEWUSER)"] {
		t.Error("默认黑名单缺少 clone(CLONE_NEWUSER)（容器内自建 userns 的逃逸路径）")
	}
	t.Logf("默认黑名单 %d 条：%s", len(names), strings.Join(SeccompFilterRuleNames(), ", "))
}

// TestAuditArchMatchesBuildArch 验证 AUDIT_ARCH 常量没写错。
//
// 写错的后果是灾难性的：arch 校验恒不等 → 容器里**所有**系统调用都被拒。
func TestAuditArchMatchesBuildArch(t *testing.T) {
	got, err := nativeAuditArch()
	if err != nil {
		t.Skipf("本架构不支持: %v", err)
	}
	// 期望值来自 linux/audit.h：EM_* | LE(0x40000000) | 64BIT(0x80000000)。
	want, ok := map[string]uint32{
		"amd64":   0xC000003E,
		"386":     0x40000003,
		"arm64":   0xC00000B7,
		"arm":     0x40000028,
		"riscv64": 0xC00000F3,
	}[runtime.GOARCH]
	if !ok {
		t.Skipf("未登记架构 %s", runtime.GOARCH)
	}
	if got != want {
		t.Fatalf("AUDIT_ARCH = %#x，期望 %#x（写错会把容器所有 syscall 都拒掉）", got, want)
	}
}

// -------- 真实安装与生效（子进程） --------

const (
	helperEnvSeccomp = "LICORE_SECCOMP_HELPER"
	helperEnvSMode   = "LICORE_SECCOMP_MODE"
	helperEnvFSProbe = "LICORE_SECCOMP_FS_PROBE"
)

// TestSeccompHelperProcess 是子进程入口：可选地装过滤器，然后探测行为。
//
// 探针选 unshare(0) 是刻意的：
//   - unshare(flags=0) 不创建任何 namespace，**不需要任何 capability**，
//     正常情况下返回 0；
//   - 它又在我们的黑名单里。
//
// 因此"返回 EPERM"只可能来自过滤器。反过来，像 acct/settimeofday 那样
// 本来就因缺能力而 EPERM 的调用**没有判别力**，不能拿来当探针。
func TestSeccompHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvSeccomp) != "1" {
		return
	}
	if os.Getenv(helperEnvSMode) == "install" {
		// 前置条件：no_new_privs。无 CAP_SYS_ADMIN 时装过滤器必须靠它。
		if err := setNoNewPrivs(); err != nil {
			fmt.Printf("NNP_FAILED:%v\n", err)
			os.Exit(1)
		}
		if err := installSeccompFilter(); err != nil {
			fmt.Printf("INSTALL_FAILED:%v\n", err)
			os.Exit(1)
		}
	}

	// Seccomp 模式：0=off 1=strict 2=filter。
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		fmt.Printf("READ_FAILED:%v\n", err)
		os.Exit(1)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "Seccomp:"); ok {
			fmt.Printf("SECCOMP=%s\n", strings.TrimSpace(v))
		}
	}

	// 常规文件操作，验证黑名单没误伤基础调用。
	if os.Getenv(helperEnvFSProbe) == "1" {
		fmt.Printf("FS_PROBE=%s\n", fsProbe())
	}

	err = syscall.Unshare(0)
	fmt.Printf("UNSHARE=%v\n", err)
	fmt.Printf("UNSHARE_IS_EPERM=%v\n", err == syscall.EPERM)
	os.Exit(0)
}

// fsProbe 做一串最基础的文件系统操作，全成功返回 "ok"。
func fsProbe() string {
	dir, err := os.MkdirTemp("", "licore-seccomp-")
	if err != nil {
		return "mkdirtemp_failed:" + err.Error()
	}
	defer func() { _ = os.RemoveAll(dir) }()
	p := filepath.Join(dir, "probe")
	if err := os.WriteFile(p, []byte("hi"), 0o644); err != nil {
		return "write_failed:" + err.Error()
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "hi" {
		return "read_failed"
	}
	if _, err := os.ReadDir(dir); err != nil {
		return "readdir_failed:" + err.Error()
	}
	if err := os.Remove(p); err != nil {
		return "remove_failed:" + err.Error()
	}
	return "ok"
}

func runSeccompHelper(t *testing.T, mode string, extraEnv ...string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("取可执行文件路径失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe,
		"-test.run=TestSeccompHelperProcess", "-test.timeout=20s")
	env := envWith(os.Environ(), helperEnvSeccomp+"=1", helperEnvSMode+"="+mode)
	cmd.Env = envWith(env, extraEnv...)

	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		t.Fatalf("helper 超时未退出\n输出:\n%s", text)
	}
	if err != nil {
		t.Fatalf("helper 失败: %v\n输出:\n%s", err, text)
	}
	return text
}

// probeUsable 报告 unshare(0) 在本环境是否适合当探针。
func probeUsable(t *testing.T) bool {
	t.Helper()
	out := runSeccompHelper(t, "noinstall")
	if strings.Contains(out, "UNSHARE_IS_EPERM=true") {
		return false
	}
	return strings.Contains(out, "UNSHARE=<nil>")
}

// TestUnshareZeroSucceedsWithoutFilter 是**对照组**：不装过滤器时 unshare(0) 必须成功。
//
// 没有这条，下面"被拦"的用例就无法排除"unshare(0) 在这台机器上本来就 EPERM"
// （例如系统禁用了 userns），那样断言就没有判别力。
func TestUnshareZeroSucceedsWithoutFilter(t *testing.T) {
	out := runSeccompHelper(t, "noinstall")
	if strings.Contains(out, "SECCOMP=2") {
		t.Fatalf("对照组不应处于 filter 模式:\n%s", out)
	}
	if !probeUsable(t) {
		t.Skipf("本环境 unshare(0) 不适合当探针（可能本来就 EPERM），跳过相关断言:\n%s", out)
	}
	if got := lineWith(t, out, "UNSHARE="); got != "UNSHARE=<nil>" {
		t.Errorf("未装过滤器时 unshare(0) 应成功，得到 %s", got)
	}
}

// TestSeccompFilterBlocksUnshare 验证过滤器真的生效：Seccomp 模式为 2，
// 且 unshare(0) 被拒为 EPERM。
func TestSeccompFilterBlocksUnshare(t *testing.T) {
	if !probeUsable(t) {
		t.Skip("本环境 unshare(0) 本来就 EPERM，探针无判别力")
	}

	out := runSeccompHelper(t, "install")
	if strings.Contains(out, "INSTALL_FAILED") {
		t.Fatalf("安装过滤器失败:\n%s", out)
	}
	if got := lineWith(t, out, "SECCOMP="); got != "SECCOMP=2" {
		t.Errorf("Seccomp 模式 = %s，期望 2（filter）", got)
	}
	if got := lineWith(t, out, "UNSHARE_IS_EPERM="); got != "UNSHARE_IS_EPERM=true" {
		t.Errorf("unshare(0) 未被拦截，期望 EPERM：\n%s", out)
	}
}

// TestSeccompFilterAllowsNormalWork 验证过滤器没有误伤常规调用。
//
// 黑名单最常见的失败模式是"拦太宽"，把容器正常要用的调用也拒了，
// 表现为"容器莫名其妙起不来"。这里在装了过滤器的子进程里跑一串基础
// 文件操作，必须全部成功。
func TestSeccompFilterAllowsNormalWork(t *testing.T) {
	out := runSeccompHelper(t, "install", helperEnvFSProbe+"=1")
	if !strings.Contains(out, "SECCOMP=2") {
		t.Fatalf("过滤器未装上，本用例无意义:\n%s", out)
	}
	if got := lineWith(t, out, "FS_PROBE="); got != "FS_PROBE=ok" {
		t.Errorf("装了过滤器后基础文件操作失败：%s（黑名单可能拦太宽）\n完整输出:\n%s", got, out)
	}
}
