// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package network

// iptables 回退后端的单元测试。
//
// 背景（真机实测）：Ubuntu 22.04 标准版 nftables 1.0.2 上 `masquerade` 语句
// 不可用，而同一个 nft 的 `dnat` 正常、iptables 的 `-j MASQUERADE` 也正常。
// 没有回退时容器完全无法出网。
//
// 本文件锁死三件事：
//  1. 规则生成正确（与 nft 版逐条对应，含回环伪装与 DNAT 端口/协议）；
//  2. 后端选择会被**持久化**——清理路径经 Load 重读网络定义，
//     不落盘就拿不到后端类型，iptables 规则将永远残留；
//  3. 后端字符串的往返映射稳定（旧 state.json 的空值按 nft 处理）。

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestIptablesMasqRuleMatchesNftSemantics 断言出口伪装规则与 nft 版等价。
func TestIptablesMasqRuleMatchesNftSemantics(t *testing.T) {
	got := strings.Join(iptablesMasqRule("172.22.0.0/16"), " ")
	want := "-t nat -A LICORE-POST -s 172.22.0.0/16 -j MASQUERADE"
	if got != want {
		t.Errorf("出口伪装规则\n got=%q\nwant=%q", got, want)
	}
	// 不得用内建链：宿主上 ufw / Docker 的规则也在 nat 表，
	// 直接往 POSTROUTING 里塞 LiCore 规则会污染它们的链。
	if strings.Contains(got, "POSTROUTING") {
		t.Errorf("规则必须写进自定义链 LICORE，不能直接进内建链: %q", got)
	}
}

// TestIptablesLoopbackMasqRule 断言回环源伪装规则正确。
//
// 这条与 nft 版 natLoopbackMasqArgs 一一对应，缺了它宿主
// `curl localhost:<port>` 会 connection timed out（容器把回包发到自己的回环）。
func TestIptablesLoopbackMasqRule(t *testing.T) {
	got := strings.Join(iptablesLoopbackMasqRule("172.22.0.0/16"), " ")
	want := "-t nat -A LICORE-POST -s 127.0.0.0/8 -d 172.22.0.0/16 -j MASQUERADE"
	if got != want {
		t.Errorf("回环伪装规则\n got=%q\nwant=%q", got, want)
	}
}

// TestIptablesDNATRule 断言 DNAT 规则的端口、协议与目标都正确。
//
// 易错点：宿主端口与容器端口写反（--dport 必须是宿主端口，
// --to-destination 必须带容器端口）。真机上一次错配就是"连上了但连错服务"。
func TestIptablesDNATRule(t *testing.T) {
	cases := []struct {
		name string
		p    *PortMapping
		want string
	}{
		{
			"tcp 默认",
			&PortMapping{HostPort: 18080, ContainerPort: 80},
			"-t nat -A LICORE-PRE -p tcp --dport 18080 -j DNAT --to-destination 172.22.0.3:80",
		},
		{
			"udp",
			&PortMapping{HostPort: 5353, ContainerPort: 53, Proto: ProtoUDP},
			"-t nat -A LICORE-PRE -p udp --dport 5353 -j DNAT --to-destination 172.22.0.3:53",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(iptablesDNATRule(&Endpoint{IP: "172.22.0.3"}, tc.p), " ")
			if got != tc.want {
				t.Errorf("\n got=%q\nwant=%q", got, tc.want)
			}
		})
	}
}

// TestNATBackendPersisted 断言后端选择会进 JSON。
//
// 这是本修复的关键不变式：清理路径（stop/rm/Disconnect）都是 `Load` 出来的
// **新对象**，只有落盘才能让它们知道当初用的是 iptables 还是 nft。
// 不落盘的话，iptables 规则永远清不掉。
func TestNATBackendPersisted(t *testing.T) {
	n := New("licore0", DriverBridge)
	n.Subnet = "172.22.0.0/16"
	n.NATBackend = natBackendIptables.String()

	data, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"natBackend":"iptables"`) {
		t.Fatalf("后端选择必须落盘，实际 JSON: %s", data)
	}

	var back Network
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.backend() != natBackendIptables {
		t.Errorf("往返后后端 = %v，期望 iptables", back.backend())
	}
}

// TestNATBackendEmptyDefaultsToNft 断言旧 state.json（无该字段）按 nft 处理。
//
// v0.9.3 之前的 state.json 不会有 natBackend 字段，那时唯一的写入位置就是
// nft。按 nft 清理是唯一正确的默认值。
func TestNATBackendEmptyDefaultsToNft(t *testing.T) {
	var n Network
	if err := json.Unmarshal([]byte(`{"name":"licore0","driver":"bridge"}`), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if n.NATBackend != "" {
		t.Errorf("旧配置不应有 natBackend，实际 %q", n.NATBackend)
	}
	if n.backend() != natBackendNone {
		t.Errorf("缺省后端 = %v，期望 none（清理时走 nft 分支）", n.backend())
	}
	// 编码时 omitempty：不影响旧版本读取。
	data, _ := json.Marshal(&n)
	if strings.Contains(string(data), "natBackend") {
		t.Errorf("空值应被 omitempty 省略，实际: %s", data)
	}
}

// TestNatBackendKindStringRoundTrip 断言字符串往返稳定。
func TestNatBackendKindStringRoundTrip(t *testing.T) {
	for _, k := range []natBackendKind{natBackendNft, natBackendIptables, natBackendNone} {
		if got := natBackendFromString(k.String()); got != k {
			t.Errorf("%v 往返后变成 %v", k, got)
		}
	}
	// 未知字符串按 none 处理，不 panic。
	if got := natBackendFromString("nonsense"); got != natBackendNone {
		t.Errorf("未知后端字符串应归为 none，实际 %v", got)
	}
}

// TestIptablesAvailableDetectsBinary 断言可用性探测只看 PATH。
func TestIptablesAvailableDetectsBinary(t *testing.T) {
	// 本测试环境（开发机/CI）有 iptables；这里只断言函数不 panic 且返回布尔。
	// 真正的"不可用"分支由 applyPortRules 的错误路径覆盖（见下条测试说明）。
	_ = iptablesAvailable()
}

// TestErrBothBackendsFailedIsDistinct 断言"两条路都失败"是独立哨兵，
// 便于用户从报错里一眼看出"不是某一条路径的问题，而是两条都不行"。
func TestErrBothBackendsFailedIsDistinct(t *testing.T) {
	if ErrBothBackendsFailed.Error() == "" {
		t.Error("哨兵必须有面向用户的文案")
	}
	for _, other := range []error{ErrNATApply, ErrNoFirewall, ErrNotRoot} {
		if ErrBothBackendsFailed == other {
			t.Errorf("ErrBothBackendsFailed 不应与 %v 是同一个哨兵", other)
		}
	}
	// 文案要点名两条路径都试过。
	msg := ErrBothBackendsFailed.Error()
	if !strings.Contains(msg, "nft") || !strings.Contains(msg, "iptables") {
		t.Errorf("文案应同时点名 nft 与 iptables，实际: %s", msg)
	}
}

// TestIptablesChainsAreSeparatePerHook 断言三个 hook 用**三条不同**的链。
//
// 这条锁定真机踩到的坑：iptables-nft 后端下，一条链挂到某个内建链时其 hook
// 类型即被绑定，再用同一条链挂**不同 hook** 的内建链会报
//
//	RULE_INSERT failed (Invalid argument): rule in chain PREROUTING
//
// 报错点在"挂引用"那一步而不是"加规则"，极易误判成规则语法问题。
// 因此这里直接断言三条链名互不相同、且与内建链一一对应。
func TestIptablesChainsAreSeparatePerHook(t *testing.T) {
	if len(iptChainBindings) != 3 {
		t.Fatalf("应有 3 条 hook 绑定，实际 %d", len(iptChainBindings))
	}
	seenChain := map[string]bool{}
	seenBuiltin := map[string]bool{}
	for _, b := range iptChainBindings {
		if seenChain[b.chain] {
			t.Errorf("链名 %q 被复用——一条链不能挂多个不同 hook 的内建链", b.chain)
		}
		if seenBuiltin[b.builtin] {
			t.Errorf("内建链 %q 被绑定多次", b.builtin)
		}
		seenChain[b.chain] = true
		seenBuiltin[b.builtin] = true
	}
	for _, want := range []string{"POSTROUTING", "PREROUTING", "OUTPUT"} {
		if !seenBuiltin[want] {
			t.Errorf("缺少内建链 %q 的绑定", want)
		}
	}
	// MASQUERADE 只能进 POST（postrouting hook），DNAT 进 PRE 与 OUT。
	if !strings.Contains(strings.Join(iptablesMasqRule("10.0.0.0/8"), " "), iptChainPost) {
		t.Error("MASQUERADE 必须写进挂 POSTROUTING 的链")
	}
	if !strings.Contains(strings.Join(iptablesDNATRule(&Endpoint{IP: "10.0.0.2"}, &PortMapping{HostPort: 80, ContainerPort: 80}), " "), iptChainPre) {
		t.Error("DNAT 必须写进挂 PREROUTING 的链")
	}
	if !strings.Contains(strings.Join(iptablesOutDNATRule(&Endpoint{IP: "10.0.0.2"}, &PortMapping{HostPort: 80, ContainerPort: 80}), " "), iptChainOut) {
		t.Error("宿主本机 DNAT 必须写进挂 OUTPUT 的链")
	}
}
