// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package network

// runNft 失败分类的单元测试。
//
// 背景（真机实测，2026-10-05）：生产服务器上 `licore` 的容器**完全无法出网**。
// 根因是两层叠加：
//  1. runNft 把 "No such file or directory" 当幂等错误**静默吞掉**——而 nft
//     在**缺内核特性**时也用这句话报错（本机 `masquerade` 语句即如此），
//     于是 NAT 规则没写进去却没人知道；
//  2. engine 侧 ApplyNAT 失败只 slog.Warn，容器照常启动。
//
// 结果是"容器起来了但连不上网"——比启动失败难排查得多。
//
// 本文件锁死修复后的分类语义：**只有真正的权限问题允许降级，其余必须冒泡**。

import (
	"errors"
	"strings"
	"testing"
)

// TestNftIdempotentMarkersExcludeNotFound 断言幂等标记里**不含**
// "No such file or directory"。
//
// 这是本修复最关键的一条：把它当幂等错误会让 NAT 静默失效。
// 真正的"表/链已存在"报的是 "File exists"。
func TestNftIdempotentMarkersExcludeNotFound(t *testing.T) {
	for _, m := range nftIdempotentMarkers {
		if strings.Contains(strings.ToLower(m), "no such file") {
			t.Fatalf("幂等标记 %q 会把「缺内核特性」误判为幂等，导致 NAT 静默失效", m)
		}
	}
	// 反向确认：真正的幂等错误仍被识别。
	if !containsAny("Error: File exists", nftIdempotentMarkers...) {
		t.Error("File exists 应被识别为幂等错误（表/链已存在）")
	}
}

// TestIsPermissionDenied 断言权限类错误被正确识别。
//
// 分类错了有两种坏结果：把权限问题当配置错误 → 非 root 下 run 直接失败
// （掩盖更根本的权限问题）；把配置错误当权限问题 → 静默降级，回到本次要修的洞。
func TestIsPermissionDenied(t *testing.T) {
	denied := []string{
		"Error: Could not process rule: Operation not permitted (you must be root)",
		"Operation not permitted",
		"you must be root",
		"Permission denied",
	}
	for _, s := range denied {
		if !isPermissionDenied(s) {
			t.Errorf("%q 应被识别为权限不足", s)
		}
	}
	// **关键反例**：缺特性报的这句话**不是**权限问题，必须冒泡。
	notDenied := []string{
		"Error: Could not process rule: No such file or directory",
		"Error: syntax error, unexpected masquerade",
	}
	for _, s := range notDenied {
		if isPermissionDenied(s) {
			t.Errorf("%q 不应被识别为权限不足（它是配置/特性问题，必须让 run 失败）", s)
		}
	}
}

// TestNftFailureHintMentionsMasquerade 断言已知失败给出可执行提示。
//
// 本机实测：nft 1.0.2（Ubuntu 22.04 标准包）上 `masquerade` 不可用，
// 而同一个 nft 的 `dnat` 正常、iptables 的 MASQUERADE 也正常。
// 提示必须点出这条替代路径，否则用户只能看到一句"应用失败"。
func TestNftFailureHintMentionsMasquerade(t *testing.T) {
	hint := nftFailureHint("Error: Could not process rule: No such file or directory")
	if hint == "" {
		t.Fatal("对 No such file or directory 应给出提示")
	}
	if !strings.Contains(hint, "masquerade") {
		t.Errorf("提示应点名 masquerade，实际: %s", hint)
	}
	if !strings.Contains(hint, "iptables") {
		t.Errorf("提示应给出 iptables 这条替代路径，实际: %s", hint)
	}
	// 无关错误不该附会提示。
	if got := nftFailureHint("Error: syntax error, unexpected foo"); got != "" {
		t.Errorf("无关错误不应给 masquerade 提示，实际: %s", got)
	}
}

// TestNftErrorsAreSentinelWrapped 断言失败错误能被 errors.Is 判定，
// 供 engine 区分「权限不足 → 降级告警」与「配置错误 → 回滚失败」。
func TestNftErrorsAreSentinelWrapped(t *testing.T) {
	// ErrNoFirewall 与 ErrNATApply 必须是不同的哨兵，语义不同：
	// 前者是"没有工具"，后者是"工具有但命令失败"。
	if errors.Is(ErrNATApply, ErrNoFirewall) {
		t.Error("ErrNATApply 与 ErrNoFirewall 语义不同，不应互相匹配")
	}
	if ErrNATApply.Error() == "" || ErrNotRoot.Error() == "" {
		t.Error("哨兵错误必须有面向用户的文案")
	}
}
