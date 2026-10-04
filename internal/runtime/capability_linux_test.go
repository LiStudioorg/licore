// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// capgetSelf 用与 capset 相同的结构布局读回当前进程的能力集。
//
// 测试文件里单独实现 capget（生产代码只需要 capset）：这样"写"与"读"
// 两侧共用同一份结构定义，一旦布局写错，二者都会错，但下面的
// TestCapabilityStructLayoutMatchesProc 会用 /proc/self/status 这个
// **独立来源**做交叉校验，把错误暴露出来。
func capgetSelf() ([2]capData, error) {
	hdr := capHeader{Version: capVersion3, PID: 0}
	var data [2]capData
	_, _, errno := syscall.Syscall(syscall.SYS_CAPGET,
		uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0)
	if errno != 0 {
		return data, errno
	}
	return data, nil
}

// procCapField 读 /proc/self/status 的某个能力字段并解析为 64 位掩码。
func procCapField(t *testing.T, field string) uint64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatalf("读 /proc/self/status: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, field+":"); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			if err != nil {
				t.Fatalf("解析 %s=%q: %v", field, v, err)
			}
			return n
		}
	}
	t.Fatalf("/proc/self/status 缺少 %s 字段", field)
	return 0
}

// effectiveMaskOf 把 capget 读回的结构拼成 64 位掩码。
func effectiveMaskOf(d [2]capData) uint64 {
	return uint64(d[0].Effective) | uint64(d[1].Effective)<<32
}

// TestCapabilityStructOffsetsMatchABI 静态断言结构布局与内核 ABI 一致。
//
// 为什么需要这条"编译期就能算出来"的断言，而不是只靠运行时比对：
// 下面的 capget 交叉校验在**无特权环境**里是无判别力的——此时 Effective 与
// Permitted 都是 0，把两个字段顺序写反也照样 0 == 0 通过。
// （这不是假设：最初的版本就只写了运行时比对，用"交换字段顺序"做变异测试时
// 它没能失败。）字段偏移是布局契约本身，与进程持有什么能力无关。
func TestCapabilityStructOffsetsMatchABI(t *testing.T) {
	// struct __user_cap_header_struct { __u32 version; int pid; }
	if got := unsafe.Offsetof(capHeader{}.Version); got != 0 {
		t.Errorf("capHeader.Version 偏移 = %d，ABI 要求 0", got)
	}
	if got := unsafe.Offsetof(capHeader{}.PID); got != 4 {
		t.Errorf("capHeader.PID 偏移 = %d，ABI 要求 4", got)
	}
	if got := unsafe.Sizeof(capHeader{}); got != 8 {
		t.Errorf("capHeader 大小 = %d，ABI 要求 8", got)
	}
	// struct __user_cap_data_struct { __u32 effective, permitted, inheritable; }
	if got := unsafe.Offsetof(capData{}.Effective); got != 0 {
		t.Errorf("capData.Effective 偏移 = %d，ABI 要求 0", got)
	}
	if got := unsafe.Offsetof(capData{}.Permitted); got != 4 {
		t.Errorf("capData.Permitted 偏移 = %d，ABI 要求 4", got)
	}
	if got := unsafe.Offsetof(capData{}.Inheritable); got != 8 {
		t.Errorf("capData.Inheritable 偏移 = %d，ABI 要求 8", got)
	}
	if got := unsafe.Sizeof(capData{}); got != 12 {
		t.Errorf("capData 大小 = %d，ABI 要求 12", got)
	}
	// ABI v3 要求 2 个 capData，覆盖 64 位能力编号。
	if got := unsafe.Sizeof([2]capData{}); got != 24 {
		t.Errorf("[2]capData 大小 = %d，ABI 要求 24", got)
	}
}

// TestCapabilityStructLayoutMatchesProc 用 capget 读回有效集，与
// /proc/self/status 的 CapEff 比对。
//
// 这是运行时的端到端校验：验证 syscall 号、指针传参与结构布局合起来确实
// 被内核接受并返回正确数据。/proc 是独立于本项目代码的来源，因此有判别力
// ——前提是进程确实持有能力（无特权时退化为 0==0，故另有上面的偏移断言）。
func TestCapabilityStructLayoutMatchesProc(t *testing.T) {
	data, err := capgetSelf()
	if err != nil {
		if err == syscall.ENOSYS {
			t.Skip("内核不支持 capget")
		}
		t.Fatalf("capget: %v", err)
	}
	got := effectiveMaskOf(data)
	want := procCapField(t, "CapEff")
	if got != want {
		t.Fatalf("capget 读回的有效集 %#x 与 /proc/self/status 的 CapEff %#x 不一致——"+
			"capHeader/capData 结构布局与内核不匹配", got, want)
	}

	// 顺带校验 permitted：它与 CapPrm 也必须一致。
	pm := uint64(data[0].Permitted) | uint64(data[1].Permitted)<<32
	if pm != procCapField(t, "CapPrm") {
		t.Errorf("capget 的 permitted %#x != CapPrm %#x", pm, procCapField(t, "CapPrm"))
	}
}

// TestCapsetIsIdempotentForCurrentSet 用当前有效集调 capset，再读回比对。
//
// capset 只能收紧，把自己现有的集合同样设一遍是合法操作，因此这个用例
// 不依赖特权、可在普通用户下运行——它验证的是**写路径**（syscall 号、
// 指针传参、结构布局）确实生效，而不是"调用后被忽略"。
func TestCapsetIsIdempotentForCurrentSet(t *testing.T) {
	before, err := capgetSelf()
	if err != nil {
		t.Fatalf("capget: %v", err)
	}
	var caps []int
	for i := 0; i < 64; i++ {
		if uint64(before[i/32].Effective)&(1<<uint(i%32)) != 0 {
			caps = append(caps, i)
		}
	}
	if err := capset(caps); err != nil {
		t.Fatalf("capset(当前集合) 应成功: %v", err)
	}
	after, err := capgetSelf()
	if err != nil {
		t.Fatalf("capget after: %v", err)
	}
	if effectiveMaskOf(after) != effectiveMaskOf(before) {
		t.Errorf("capset 后的有效集 %#x != 之前 %#x",
			effectiveMaskOf(after), effectiveMaskOf(before))
	}
}

// TestCapLastCapReadable 验证能读到内核支持的最大能力编号。
func TestCapLastCapReadable(t *testing.T) {
	n := capLastCap()
	if n < capCheckpointRstrt {
		t.Errorf("capLastCap() = %d，应至少覆盖已知的最后一个能力 %d", n, capCheckpointRstrt)
	}
}

// 特权子进程 helper：应用能力裁剪，然后报告进程自身的能力位图。
//
// 只有持有 CAP_SETPCAP 时才能跑通（PR_CAPBSET_DROP 需要它），
// 因此在无特权环境里相关用例会显式 skip；真机验证由服务器上的脚本完成。
const (
	helperEnvCaps     = "LICORE_CAPS_HELPER"
	helperEnvCapsMode = "LICORE_CAPS_MODE"
)

func TestCapsHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvCaps) != "1" {
		return
	}
	mode := os.Getenv(helperEnvCapsMode)

	drop, add := capsFromEnv(os.Environ())
	kept, err := ApplyCapabilities(drop, add)
	if err != nil {
		fmt.Printf("APPLY_FAILED:%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("KEPT=%s\n", strings.Join(kept, ","))

	data, err := capgetSelf()
	if err != nil {
		fmt.Printf("CAPGET_FAILED:%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("CAPEFF=%#x\n", effectiveMaskOf(data))
	if mode == "drop-all" {
		if effectiveMaskOf(data) != 0 {
			fmt.Printf("EXPECT_EMPTY_BUT_GOT:%#x\n", effectiveMaskOf(data))
			os.Exit(2)
		}
	}
	os.Exit(0)
}

func runCapsHelper(t *testing.T, drop, add []string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("取可执行文件路径失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe,
		"-test.run=TestCapsHelperProcess", "-test.timeout=20s")
	env := envWith(os.Environ(), helperEnvCaps+"=1")
	env = envWith(env, CapsEnv(drop, add)...)
	cmd.Env = env

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

// hasSetpcap 报告当前进程有效集里是否有 CAP_SETPCAP。
// 没有它就无法从边界集移除能力，也就无法真正裁剪。
func hasSetpcap(t *testing.T) bool {
	data, err := capgetSelf()
	if err != nil {
		return false
	}
	return uint64(data[0].Effective)&(1<<uint(capSetpcap)) != 0
}

// TestApplyCapabilitiesDropsToDefault 在特权环境里验证默认裁剪真的生效。
//
// 无 CAP_SETPCAP 时 skip：沙箱/普通用户下 PR_CAPBSET_DROP 会 EPERM，
// 这是环境限制而非代码缺陷。真机上这一步是本修复最关键的行为断言。
func TestApplyCapabilitiesDropsToDefault(t *testing.T) {
	if !hasSetpcap(t) {
		t.Skip("当前进程无 CAP_SETPCAP（非特权环境），能力裁剪只能在 root 下验证；" +
			"真机验证见 scripts/verify-capabilities.sh")
	}
	out := runCapsHelper(t, nil, nil)
	if !strings.Contains(out, "KEPT=") {
		t.Fatalf("helper 未报告保留集:\n%s", out)
	}
	// CAP_SYS_ADMIN 必须已被丢弃——这是攻击路径的关键能力。
	keptLine := lineWith(t, out, "KEPT=")
	if strings.Contains(keptLine, "SYS_ADMIN") {
		t.Errorf("裁剪后仍持有 SYS_ADMIN：%s", keptLine)
	}
	for _, must := range []string{"CHOWN", "NET_RAW", "SETUID", "SETGID"} {
		if !strings.Contains(keptLine, must) {
			t.Errorf("默认集应保留 %s：%s", must, keptLine)
		}
	}
}

// TestApplyCapabilitiesDropAllYieldsEmptySet 在特权环境里验证 drop ALL 得到空集。
func TestApplyCapabilitiesDropAllYieldsEmptySet(t *testing.T) {
	if !hasSetpcap(t) {
		t.Skip("当前进程无 CAP_SETPCAP（非特权环境），只能在 root 下验证")
	}
	out := runCapsHelper(t, []string{"ALL"}, nil)
	if strings.Contains(out, "EXPECT_EMPTY_BUT_GOT") {
		t.Fatalf("--cap-drop ALL 后有效集应为空:\n%s", out)
	}
	if line := lineWith(t, out, "CAPEFF="); line != "CAPEFF=0x0" {
		t.Errorf("--cap-drop ALL 后 CapEff = %s，期望 0x0", line)
	}
}

// lineWith 返回输出里以 prefix 开头的那一行。
func lineWith(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("输出里没有 %q 开头的行:\n%s", prefix, out)
	return ""
}
