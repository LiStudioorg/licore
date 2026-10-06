// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

// 本文件是能力裁剪的**系统调用**部分（prctl 清边界集 + capset 收紧集合）。
// 纯逻辑（编号表、名称解析、集合运算）在 capability.go，那份不带 build tag。

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// prCapbsetDrop 是 prctl(PR_CAPBSET_DROP)：把某个能力从边界集里移除。
const prCapbsetDrop = 24

// capLastCap 读内核支持的最大能力编号。
//
// 用 /proc/sys/kernel/cap_last_cap 而不是硬编码：不同内核这个值不同
// （较新内核为 40），硬编码会在更新的内核上漏掉编号更大的能力。
// 读不到时退化为一个足够大的上界——多试的编号会被内核以 EINVAL 拒绝，无害。
func capLastCap() int {
	data, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return capLastCapFallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 {
		return capLastCapFallback
	}
	return n
}

// dropBoundingSet 把不在 keep 里的能力从边界集（bounding set）中移除。
//
// 必须在 capset **之前**做：capset 会把有效集降到 keep，之后就不再持有
// CAP_SETPCAP，而 PR_CAPBSET_DROP 要求有效集里有 CAP_SETPCAP。顺序反了
// 会导致剩余能力永远留在边界集里（子进程可通过 exec 重新获得）。
//
// 边界集只限制"将来能否再获得"该能力，不影响当前有效集，因此循环内部
// 即使先把 CAP_SETPCAP 从边界集移除，也不影响后续的 drop 调用。
func dropBoundingSet(keep map[int]bool) error {
	last := capLastCap()
	for c := 0; c <= last; c++ {
		if keep[c] {
			continue
		}
		_, _, errno := syscall.Syscall(syscall.SYS_PRCTL, uintptr(prCapbsetDrop), uintptr(c), 0)
		switch errno {
		case 0:
		case syscall.EINVAL:
			// 该编号此内核未定义（读不到 cap_last_cap 时会遇到），跳过。
		default:
			return fmt.Errorf("prctl(PR_CAPBSET_DROP, %d): %w", c, errno)
		}
	}
	return nil
}

// capability ABI 结构（linux/capability.h）。
//
//	_LINUX_CAPABILITY_VERSION_3 = 0x20080522，配套 2 个 capData（覆盖 64 位）。
//
// 字段顺序与宽度必须与内核结构逐字节一致；测试用 capget 交叉校验
// （capget 读回的值与 /proc/self/status 的 CapEff 比对）来保证这一点。
type capHeader struct {
	Version uint32
	PID     int32
}

type capData struct {
	Effective   uint32
	Permitted   uint32
	Inheritable uint32
}

var capVersion3 = uint32(0x20080522)

// capset 设置当前进程的有效/允许/继承能力集。
//
// 只能收紧不能放宽：内核要求新的 permitted ⊆ 旧的 permitted，否则 EPERM。
func capset(caps []int) error {
	hdr := capHeader{Version: capVersion3, PID: 0} // PID 0 = 当前进程
	var data [2]capData
	for _, c := range caps {
		idx := c / 32
		if idx < 0 || idx >= len(data) {
			return fmt.Errorf("capability 编号 %d 超出 ABI v3 可表达范围", c)
		}
		bit := uint32(1) << (uint(c) % 32)
		data[idx].Effective |= bit
		data[idx].Permitted |= bit
		data[idx].Inheritable |= bit
	}
	_, _, errno := syscall.Syscall(syscall.SYS_CAPSET,
		uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0)
	if errno != 0 {
		return fmt.Errorf("capset: %w", errno)
	}
	return nil
}

// capsetFn / dropBoundingSetFn 是**测试注入缝**：ApplyCapabilities 与
// ApplyCapabilitiesSplit 通过它们调用真实实现。
//
// 为什么需要：这两个系统调用在无特权环境必定 EPERM，导致"调用顺序"这类
// 纯逻辑属性无法在 CI 里断言 —— 而这恰恰是最容易写错、且错了就容器启动
// 失败的地方（真机为定下正确顺序踩了五次）。经变量间接调用后，测试可以
// 换成桩来记录顺序，真机行为仍由 scripts/verify-capabilities.sh 覆盖。
var (
	capsetFn          = capset
	dropBoundingSetFn = dropBoundingSet
)

// ApplyCapabilities 按 drop/add 规格裁剪当前进程的能力，返回最终保留的能力名。
//
// 顺序不可交换（安全属性）：
//  1. 先从边界集移除不要的能力——此时还持有 CAP_SETPCAP；
//  2. 再 capset 收紧有效/允许/继承集。
func ApplyCapabilities(drop, add []string) ([]string, error) {
	keepList, err := resolveCapabilities(drop, add)
	if err != nil {
		return nil, err
	}
	keep := make(map[int]bool, len(keepList))
	for _, c := range keepList {
		keep[c] = true
	}
	if err := dropBoundingSetFn(keep); err != nil {
		return nil, err
	}
	if err := capsetFn(keepList); err != nil {
		return nil, err
	}
	return CapabilityNames(keepList), nil
}

// applyCapabilitiesFromEnv 是容器 init 的入口：从自身环境变量读取裁剪规格并应用。
func applyCapabilitiesFromEnv() ([]string, error) {
	drop, add := capsFromEnv(os.Environ())
	return ApplyCapabilities(drop, add)
}

// ApplyCapabilitiesSplit 是"既要降权到非 root、又要裁剪能力"时的正确入口。
//
// **内核约束（全部真机实测确认）**：
//
//	(a) PR_CAPBSET_DROP 需要 CAP_SETPCAP 在**有效集**里；
//	(b) 从边界集移除某能力时，内核同时把它从 permitted/effective 清掉
//	    —— 所以 SETPCAP 一旦离开边界集，之后**任何** capset 都 EPERM；
//	(c) capset 收紧**非空**集合同样需要 CAP_SETPCAP 在有效集里；
//	(d) setuid/setgid 到非 0 需要 CAP_SETUID / CAP_SETGID 在有效集里；
//	(e) 降权成功会清空 permitted/effective —— **降权必须是最后一步能力操作**。
//
// (a)(b)(c) 构成一个死锁：dropBoundingSet 需要 SETPCAP 在有效集，而让
// SETPCAP 离开边界集又会清掉有效集里的它，使随后的 capset 失败。
// **解法是不把 SETPCAP 移出边界集**——它留在边界集里无害，理由见下。
//
// 顺序（唯一能同时满足 (a)–(e) 的）：
//
//  1. capset 到「最终集合 ∪ {SETUID, SETGID, SETPCAP}」
//  2. 清边界集到同一集合
//     —— 边界集此后不再变动（避免触发 (b) 的死锁）
//  3. capset 到「最终集合 ∪ {SETUID, SETGID}」
//     —— 从**有效集**摘掉 SETPCAP（(c)：此刻 SETPCAP 在边界集里，故有效集里也有）
//  4. between()：降权。之后不再做任何能力操作（(e)）
//
// **为什么边界集里留着 SETPCAP / SETUID / SETGID 是安全的**：
// 边界集只是"execve 时最多能获得哪些能力"的上限，它本身**不授予**任何能力。
// 要兑现边界集里的项目必须经过 execve 提权（file capability 或 setuid-root
// 程序），而容器已设 `no_new_privs=1`，内核在 execve 时不会赋予任何新特权。
// 至于"直接调 capset 改自己的能力集"——那需要 SETPCAP 在**有效集**里，
// 而第 3 步已把它从有效集移除，且此后 permitted/inheritable 均为空，
// 子进程也继承不到它。两条路都堵死，故残留不构成提权面。
//
// 真机实测失败过的朴素顺序（容器一律 Exited(1)）：
//
//	先降权再裁剪        → (a) prctl(PR_CAPBSET_DROP, 2): operation not permitted
//	先裁剪再降权        → (d) 设置 gid=1000 失败: operation not permitted
//	清边界集后 capset    → (b) capset: operation not permitted
//	capset 后清边界集    → (a) 或 (b): operation not permitted
//	降权后再收口        → (e) 清边界集(最终): operation not permitted
//
// between 为 nil 时退化为 ApplyCapabilities 的等价行为。
func ApplyCapabilitiesSplit(drop, add []string, between func() error) ([]string, error) {
	keepList, err := resolveCapabilities(drop, add)
	if err != nil {
		return nil, err
	}
	if between == nil {
		// 无需降权：走原路径，行为完全一致。
		return ApplyCapabilities(drop, add)
	}
	// 边界集目标：最终集合 ∪ {SETUID, SETGID, SETPCAP}（此后不再变动）。
	bnd := unionCaps(keepList, capSetuid, capSetgid, capSetpcap)
	// 有效集目标：最终集合 ∪ {SETUID, SETGID}（降权后即等于最终集合）。
	eff := unionCaps(keepList, capSetuid, capSetgid)

	// 1. capset 到边界集目标（(c)：SETPCAP 此刻完整在位）。
	if err := capsetFn(bnd); err != nil {
		return nil, fmt.Errorf("capset(%v): %w", bnd, err)
	}
	// 2. 清边界集到同一集合（(a)：SETPCAP 尚在有效集；(b) 因保留 SETPCAP 而不触发）。
	if err := dropBoundingSetFn(capsToSet(bnd)); err != nil {
		return nil, fmt.Errorf("清边界集(%v): %w", bnd, err)
	}
	// 3. capset 到有效集目标——**从有效集摘掉 SETPCAP**。
	//    这是关键安全步骤：此后进程无法再改任何能力集。
	if err := capsetFn(eff); err != nil {
		return nil, fmt.Errorf("capset(%v): %w", eff, err)
	}
	// 4. 降权——**最后一步**（(e)：之后不能再做能力操作）。
	if err := between(); err != nil {
		return nil, err
	}
	return CapabilityNames(keepList), nil
}

// unionCaps 把 extra 并入 base 后返回排序去重的切片。
func unionCaps(base []int, extra ...int) []int {
	set := make(map[int]bool, len(base)+len(extra))
	for _, c := range base {
		set[c] = true
	}
	for _, c := range extra {
		set[c] = true
	}
	out := make([]int, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Ints(out)
	return out
}

// capsToSet 把能力列表转成查表用的 map。
func capsToSet(list []int) map[int]bool {
	m := make(map[int]bool, len(list))
	for _, c := range list {
		m[c] = true
	}
	return m
}
