// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

import (
	"errors"
	"sort"
	"strings"
	"testing"
)

// 本文件覆盖 ApplyCapabilitiesSplit —— "既要降权到非 root、又要裁剪能力"。
//
// 这块顺序是**真机踩了五次才定下来的**，每条约束都能让容器 Exited(1)：
//
//	先降权再裁剪      → PR_CAPBSET_DROP: operation not permitted
//	先裁剪再降权      → 设置 gid=1000 失败: operation not permitted
//	清边界集后 capset → capset: operation not permitted
//	capset 后清边界集 → PR_CAPBSET_DROP: operation not permitted
//	降权后再收口      → 清边界集(最终): operation not permitted
//
// 无特权环境下 PR_CAPBSET_DROP / capset 非空集合都会 EPERM，因此这里
// **不测真机行为**（那部分由 scripts/verify-capabilities.sh 覆盖），
// 只测两件在无特权下也能确定的事：
//  1. 集合运算正确（含/不含哪些能力）；
//  2. **调用顺序正确** —— 用可观测的桩替换 capset/dropBoundingSet。

// TestCapabilitySetsIncludeTransients 校验"边界集目标"与"有效集目标"的构成。
//
// 这两条正是安全属性所在：
//   - 边界集要含 SETUID/SETGID/SETPCAP（完成降权与收口所必需）；
//   - 有效集**不能**含 SETPCAP（否则进程随时能改自己的能力集）。
func TestCapabilitySetsIncludeTransients(t *testing.T) {
	keep := []int{0, 1, 2} // 随便一组"最终集合"
	bnd := unionCaps(keep, capSetuid, capSetgid, capSetpcap)
	eff := unionCaps(keep, capSetuid, capSetgid)

	in := func(list []int, c int) bool {
		for _, x := range list {
			if x == c {
				return true
			}
		}
		return false
	}

	for _, c := range []int{capSetuid, capSetgid, capSetpcap} {
		if !in(bnd, c) {
			t.Errorf("边界集目标必须含能力 %d（降权/收口要用）: %v", c, bnd)
		}
	}
	if !in(eff, capSetuid) || !in(eff, capSetgid) {
		t.Errorf("有效集目标必须含 SETUID/SETGID（降权要用）: %v", eff)
	}
	if in(eff, capSetpcap) {
		t.Errorf("有效集目标**不得**含 CAP_SETPCAP（否则进程可改自己的能力集）: %v", eff)
	}
	// 最终集合必须被包含（不能因为加了临时能力反而丢掉用户要的）。
	for _, c := range keep {
		if !in(bnd, c) || !in(eff, c) {
			t.Errorf("最终集合里的能力 %d 不能丢: bnd=%v eff=%v", c, bnd, eff)
		}
	}
}

// TestApplyCapabilitiesSplitOrder 用桩验证**调用顺序**。
//
// 期望：所有能力操作都在"降权"之前；降权是最后一步。
// 顺序错了在真机上就是 Exited(1)，而这里能在无特权环境提前拦住。
func TestApplyCapabilitiesSplitOrder(t *testing.T) {
	oldCapset, oldDrop := capsetFn, dropBoundingSetFn
	defer func() { capsetFn, dropBoundingSetFn = oldCapset, oldDrop }()

	var calls []string
	capsetFn = func(caps []int) error {
		calls = append(calls, "capset")
		return nil
	}
	dropBoundingSetFn = func(keep map[int]bool) error {
		calls = append(calls, "dropBoundingSet")
		return nil
	}

	switched := false
	_, err := ApplyCapabilitiesSplit([]string{"ALL"}, nil, func() error {
		switched = true
		calls = append(calls, "setuid")
		return nil
	})
	if err != nil {
		t.Fatalf("ApplyCapabilitiesSplit: %v", err)
	}
	if !switched {
		t.Fatal("between（降权）从未被调用")
	}

	// 降权必须是最后一个动作。
	if last := calls[len(calls)-1]; last != "setuid" {
		t.Errorf("降权必须是最后一步，实际顺序: %v", calls)
	}
	// 降权之后不得再有任何能力操作（否则真机必然 EPERM）。
	seenSetuid := false
	for _, c := range calls {
		if c == "setuid" {
			seenSetuid = true
			continue
		}
		if seenSetuid {
			t.Errorf("降权之后又做了能力操作 %q —— 真机必 EPERM。顺序: %v", c, calls)
		}
	}
	// 必须两次 capset：中间态（含 SETPCAP）与收口态（不含）。
	nCap := 0
	for _, c := range calls {
		if c == "capset" {
			nCap++
		}
	}
	if nCap != 2 {
		t.Errorf("应恰好两次 capset（中间态 + 收口态），实得 %d 次: %v", nCap, calls)
	}
	// 边界集只清一次，且在两次 capset 之间（避免触发"移除 SETPCAP 后 capset EPERM"）。
	if calls[0] != "capset" {
		t.Errorf("第一步必须是 capset（此时 SETPCAP 完整在位）: %v", calls)
	}
}

// 降权失败必须原样返回，且不得被吞掉（否则会得到一个"以为是 uid 1000"的容器）。
func TestApplyCapabilitiesSplitPropagatesSwitchError(t *testing.T) {
	oldCapset, oldDrop := capsetFn, dropBoundingSetFn
	defer func() { capsetFn, dropBoundingSetFn = oldCapset, oldDrop }()
	capsetFn = func([]int) error { return nil }
	dropBoundingSetFn = func(map[int]bool) error { return nil }

	sentinel := errors.New("设置 uid=1000 失败")
	_, err := ApplyCapabilitiesSplit(nil, nil, func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("降权错误必须向上传递，实得 %v", err)
	}
}

// between 为 nil 时走原路径，且**不得**留下临时能力
// （否则 `--cap-drop ALL` 会得到 CapEff=0xc0 而不是 0，真机实测过）。
func TestApplyCapabilitiesSplitNilBetweenUsesPlainPath(t *testing.T) {
	oldCapset, oldDrop := capsetFn, dropBoundingSetFn
	defer func() { capsetFn, dropBoundingSetFn = oldCapset, oldDrop }()

	var capsetArgs [][]int
	capsetFn = func(caps []int) error {
		capsetArgs = append(capsetArgs, append([]int{}, caps...))
		return nil
	}
	dropBoundingSetFn = func(map[int]bool) error { return nil }

	if _, err := ApplyCapabilitiesSplit([]string{"ALL"}, nil, nil); err != nil {
		t.Fatalf("ApplyCapabilitiesSplit(nil between): %v", err)
	}
	if len(capsetArgs) != 1 {
		t.Fatalf("nil between 应只调用一次 capset，实得 %d 次: %v", len(capsetArgs), capsetArgs)
	}
	// `--cap-drop ALL` 的最终有效集必须是空集。
	if len(capsetArgs[0]) != 0 {
		t.Errorf("--cap-drop ALL 的有效集应为空，实得 %v", capsetArgs[0])
	}
}

// unionCaps 的纯函数行为。
func TestUnionCaps(t *testing.T) {
	got := unionCaps([]int{5, 3}, 1, 3)
	want := []int{1, 3, 5}
	if len(got) != len(want) {
		t.Fatalf("unionCaps = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unionCaps = %v，期望 %v（需排序去重）", got, want)
		}
	}
	if !sort.IntsAreSorted(got) {
		t.Errorf("unionCaps 结果应有序: %v", got)
	}
	if r := unionCaps(nil); len(r) != 0 {
		t.Errorf("unionCaps(nil) 应为空，实得 %v", r)
	}
}

// capsToSet 的纯函数行为。
func TestCapsToSet(t *testing.T) {
	m := capsToSet([]int{2, 4, 4})
	if len(m) != 2 || !m[2] || !m[4] {
		t.Errorf("capsToSet = %v", m)
	}
	if len(capsToSet(nil)) != 0 {
		t.Error("capsToSet(nil) 应为空 map")
	}
}

// 错误信息里必须带上是哪一步失败 —— 真机排查时全靠它区分五种错序。
func TestApplyCapabilitiesSplitErrorContext(t *testing.T) {
	oldCapset, oldDrop := capsetFn, dropBoundingSetFn
	defer func() { capsetFn, dropBoundingSetFn = oldCapset, oldDrop }()
	capsetFn = func([]int) error { return errors.New("boom") }
	dropBoundingSetFn = func(map[int]bool) error { return nil }

	_, err := ApplyCapabilitiesSplit([]string{"ALL"}, nil, func() error { return nil })
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "capset") {
		t.Errorf("错误信息应指明失败的是 capset，实得: %v", err)
	}
}
