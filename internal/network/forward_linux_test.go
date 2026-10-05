// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package network

// LICORE-FORWARD / LICORE-INPUT 规则的单元测试。
//
// 两个链分工，**放错链的症状是"规则看着对、命中计数恒为 0、功能不生效"**，
// 不会有任何报错。因此必须由测试锁死"什么规则进哪个链"：
//
//	LICORE-FORWARD（挂 FORWARD）  放行：容器↔容器、容器→外网、回包
//	LICORE-INPUT  （挂 INPUT）    隔离：DROP 容器→宿主本机地址
//
// 真机实测的失败形态（第一版把 DROP 放进 FORWARD）：
//   - 出网成功（走 FORWARD ✅）；
//   - `nc <宿主IP> 2222` 仍通（走 INPUT ❌）；
//   - FORWARD 里 5 条 DROP 的命中计数**全是 0**。

import (
	"strings"
	"testing"
)

// ruleIndex 返回指定 Kind 的规则下标；不存在返回 -1。
func ruleIndex(rules []fwdRule, kind string) int {
	for i, r := range rules {
		if r.Kind == kind {
			return i
		}
	}
	return -1
}

// -------- LICORE-FORWARD：只放行 --------

// TestForwardRulesContainNoDrop 断言 FORWARD 链里**没有 DROP**。
//
// 这是第一版失败的直接教训：容器→宿主是本地投递，走 INPUT。
// 往 FORWARD 放 DROP 不但无效，还会让维护者误以为隔离已经做了。
func TestForwardRulesContainNoDrop(t *testing.T) {
	rules := buildForwardRules("licore0", []string{"eth0"})
	for _, r := range rules {
		if strings.Contains(strings.Join(r.Args, " "), "-j DROP") {
			t.Fatalf("FORWARD 链不应含 DROP（容器→宿主走 INPUT，见文件头说明）: %v", r.Args)
		}
	}
}

// TestForwardRulesAcceptEgress 断言容器→外网放行存在且指向出口接口。
//
// 缺了它：宿主 FORWARD 策略为 DROP 时（ufw active 必然如此）容器**没有外网**。
// 真机实测：MASQUERADE 已正确写入但 `nc 8.8.8.8 53` 仍超时，
// 补一条 `-i licore0 -o <ext> -j ACCEPT` 后立刻恢复。
func TestForwardRulesAcceptEgress(t *testing.T) {
	rules := buildForwardRules("licore0", []string{"eth0"})
	i := ruleIndex(rules, fwdKindContainerToExternal)
	if i < 0 {
		t.Fatal("缺少 container-to-external 规则——容器将无法出网")
	}
	if got := strings.Join(rules[i].Args, " "); got != "-i licore0 -o eth0 -j ACCEPT" {
		t.Errorf("出网规则 = %q", got)
	}
	// 多网卡机器：每个出口接口一条，只放行 eth0 会让走另一张网卡的容器没网。
	var n int
	for _, r := range buildForwardRules("licore0", []string{"eth0", "eth1"}) {
		if r.Kind == fwdKindContainerToExternal {
			n++
		}
	}
	if n != 2 {
		t.Errorf("两个出口接口应生成 2 条放行规则，实际 %d", n)
	}
}

// TestForwardRulesContainerToContainer 断言同网桥互访被放行且在首位。
func TestForwardRulesContainerToContainer(t *testing.T) {
	rules := buildForwardRules("licore0", []string{"eth0"})
	i := ruleIndex(rules, fwdKindContainerToContainer)
	if i < 0 {
		t.Fatal("缺少 container-to-container 规则——容器间无法通信")
	}
	if got := strings.Join(rules[i].Args, " "); got != "-i licore0 -o licore0 -j ACCEPT" {
		t.Errorf("同网桥规则 = %q", got)
	}
	if i != 0 {
		t.Errorf("同网桥放行应在首位，实际下标 %d", i)
	}
}

// TestForwardRulesEstablishedReply 断言回包放行规则存在。
//
// 端口映射（-p）的入站连接，回包方向是 `-o licore0`，靠 conntrack 放行。
// 缺了它 `licore run -p` 的端口在外网**连不上**。
func TestForwardRulesEstablishedReply(t *testing.T) {
	rules := buildForwardRules("licore0", []string{"eth0"})
	i := ruleIndex(rules, fwdKindEstablished)
	if i < 0 {
		t.Fatal("缺少 established 回包放行规则——端口映射的回包会被丢掉")
	}
	got := strings.Join(rules[i].Args, " ")
	if !strings.Contains(got, "-o licore0") || !strings.Contains(got, "RELATED,ESTABLISHED") {
		t.Errorf("回包规则 = %q，应含 -o licore0 与 RELATED,ESTABLISHED", got)
	}
}

// TestForwardRulesAllScopedToBridge 断言放行规则都以 -i licore0 限定入接口。
//
// 少了 -i 限定，规则会作用于**全宿主**的转发流量，影响 Docker 容器、
// 虚拟机等无关流量——这是往 shared FORWARD 链插规则最容易闯的祸。
func TestForwardRulesAllScopedToBridge(t *testing.T) {
	for _, r := range buildForwardRules("licore0", []string{"eth0"}) {
		joined := strings.Join(r.Args, " ")
		if r.Kind == fwdKindEstablished {
			if !strings.Contains(joined, "-o licore0") {
				t.Errorf("回包规则应以 -o licore0 限定: %q", joined)
			}
			continue
		}
		if !strings.Contains(joined, "-i licore0") {
			t.Errorf("规则 %s 缺少 -i licore0 限定，会误伤其它转发流量: %q", r.Kind, joined)
		}
	}
}

// -------- LICORE-INPUT：隔离 --------

// TestInputRulesDropHostIPs 断言每个宿主地址各有一条 DROP。
//
// 只堵 eth0 地址会留下绕过路径：容器仍可访问宿主回环、其它网桥地址等。
func TestInputRulesDropHostIPs(t *testing.T) {
	orig := isBridgeOwnAddr
	t.Cleanup(func() { isBridgeOwnAddr = orig })
	isBridgeOwnAddr = func(ip, iface string) bool { return false }

	hostIPs := []string{"127.0.0.1", "45.207.198.91", "172.17.0.1"}
	rules := buildInputRules("licore0", hostIPs)

	var drops int
	for _, r := range rules {
		if r.Kind != inKindDropToHost {
			continue
		}
		drops++
		got := strings.Join(r.Args, " ")
		if !strings.HasPrefix(got, "-i licore0 -d ") || !strings.HasSuffix(got, " -j DROP") {
			t.Errorf("DROP 规则形态错误: %q", got)
		}
	}
	if drops != len(hostIPs) {
		t.Fatalf("应有 %d 条 DROP，实际 %d: %+v", len(hostIPs), drops, rules)
	}
}

// TestInputRulesSkipBridgeGateway 断言**网桥自身地址不被 DROP**。
//
// 容器访问网关（172.22.0.1）是本地投递，会经过 INPUT。若把网关也 DROP，
// 容器内一切指向网关的访问都会断——首当其冲是 DNS（resolv.conf 指向网关）。
// 这会表现为"域名不通但 IP 通"，极难排查。
func TestInputRulesSkipBridgeGateway(t *testing.T) {
	orig := isBridgeOwnAddr
	t.Cleanup(func() { isBridgeOwnAddr = orig })
	isBridgeOwnAddr = func(ip, iface string) bool { return ip == "172.22.0.1" }

	rules := buildInputRules("licore0", []string{"172.22.0.1", "45.207.198.91"})

	for _, r := range rules {
		if strings.Contains(strings.Join(r.Args, " "), "172.22.0.1") {
			t.Fatal("网桥网关地址被 DROP——容器访问网关会全部失败（含 DNS）")
		}
	}
	var drops int
	for _, r := range rules {
		if r.Kind == inKindDropToHost {
			drops++
		}
	}
	if drops != 1 {
		t.Errorf("应只剩 1 条 DROP（非网关地址），实际 %d", drops)
	}
}

// TestInputRulesNoHostIPsMeansNoRules 断言宿主地址为空时**不生成规则**。
//
// 宁可不生成，也不生成"堵 0.0.0.0/0"之类的宽泛规则——那会把容器出网
// 一并堵死。调用方（applyForwardRules）据此报错而非静默继续。
func TestInputRulesNoHostIPsMeansNoRules(t *testing.T) {
	rules := buildInputRules("licore0", nil)
	for _, r := range rules {
		if r.Kind == inKindDropToHost {
			t.Error("宿主地址为空时不应生成 DROP 规则")
		}
	}
	// 放行规则仍应在：端口映射的回包不依赖宿主地址枚举。
	if ruleIndex(rules, inKindEstablishedAccept) < 0 {
		t.Error("回包放行规则不应因宿主地址枚举失败而缺失")
	}
}

// TestInputRulesEstablishedAcceptBeforeDrop 断言回包放行规则在 DROP **之前**。
//
// 这是端口映射能工作的前提：端口映射的回包（SYN-ACK）目的地址是宿主自身
// IP，属本地投递，走 INPUT。若 DROP 排在前面，连接永远停在 SYN_RECV
// ——真机实测确认过这个失败形态（容器内连接状态 03，DROP 计数持续增长）。
func TestInputRulesEstablishedAcceptBeforeDrop(t *testing.T) {
	orig := isBridgeOwnAddr
	t.Cleanup(func() { isBridgeOwnAddr = orig })
	isBridgeOwnAddr = func(ip, iface string) bool { return false }

	rules := buildInputRules("licore0", []string{"1.2.3.4"})
	ai := ruleIndex(rules, inKindEstablishedAccept)
	di := ruleIndex(rules, inKindDropToHost)

	if ai < 0 {
		t.Fatal("缺少回包放行规则——端口映射的回包会被 DROP，连接卡在 SYN_RECV")
	}
	if di < 0 {
		t.Fatal("缺少 drop-to-host 规则——H2 隔离失效")
	}
	if ai > di {
		t.Fatalf("回包放行(下标 %d)必须在 DROP(下标 %d) 之前，否则端口映射失效", ai, di)
	}
	got := strings.Join(rules[ai].Args, " ")
	if !strings.Contains(got, "RELATED,ESTABLISHED") || !strings.Contains(got, "-i licore0") {
		t.Errorf("回包放行规则形态错误: %q", got)
	}
}

// TestInputRulesNoBlanketAccept 断言 INPUT 链**没有无条件的 ACCEPT**。
//
// 唯一的 ACCEPT 必须是 conntrack 限定的回包。无条件的 ACCEPT 会把宿主的
// INPUT 策略架空（本机是 ufw 的 DROP），等于取消宿主防火墙。
func TestInputRulesNoBlanketAccept(t *testing.T) {
	orig := isBridgeOwnAddr
	t.Cleanup(func() { isBridgeOwnAddr = orig })
	isBridgeOwnAddr = func(ip, iface string) bool { return false }

	for _, r := range buildInputRules("licore0", []string{"1.2.3.4"}) {
		got := strings.Join(r.Args, " ")
		if !strings.Contains(got, "-j ACCEPT") {
			continue
		}
		if !strings.Contains(got, "RELATED,ESTABLISHED") {
			t.Errorf("INPUT 链的 ACCEPT 必须是 conntrack 限定的回包: %q", got)
		}
	}
}

// -------- 辅助逻辑 --------

// TestIsBridgeLikeIface 断言网桥类接口被正确识别（不当作外网出口）。
func TestIsBridgeLikeIface(t *testing.T) {
	for _, n := range []string{"licore0", "docker0", "br-abc123", "veth1234", "virbr0", "vpe0b0ac", "tap0", "tun0"} {
		if !isBridgeLikeIface(n) {
			t.Errorf("%s 应被识别为网桥类（排除出外网出口）", n)
		}
	}
	for _, n := range []string{"eth0", "ens18", "enp3s0", "wlan0", "bond0"} {
		if isBridgeLikeIface(n) {
			t.Errorf("%s 是外网出口，不应被排除", n)
		}
	}
}

// TestHostIPv4AddrsNonEmpty 断言能枚举到宿主地址（本机至少有回环）。
func TestHostIPv4AddrsNonEmpty(t *testing.T) {
	addrs := hostIPv4Addrs()
	if len(addrs) == 0 {
		t.Fatal("应至少枚举到回环地址")
	}
	found := false
	for _, a := range addrs {
		if a == "127.0.0.1" {
			found = true
		}
	}
	if !found {
		t.Errorf("应包含 127.0.0.1，实际: %v", addrs)
	}
}

// TestExternalIfacesExcludesBridges 断言出口接口枚举排除了网桥与回环。
func TestExternalIfacesExcludesBridges(t *testing.T) {
	for _, n := range externalIfaces() {
		if n == "lo" || isBridgeLikeIface(n) {
			t.Errorf("出口接口不应包含 %s", n)
		}
	}
}

// TestChainsAreDistinct 断言 FORWARD 与 INPUT 用**不同**的链名。
//
// 混用会让两类规则落到同一个链：要么隔离规则进了 FORWARD（无效），
// 要么放行规则进了 INPUT（架空宿主策略）。
func TestChainsAreDistinct(t *testing.T) {
	if fwdChain == inChain {
		t.Fatal("FORWARD 与 INPUT 必须用不同的自定义链")
	}
	if !strings.HasPrefix(fwdChain, "LICORE") || !strings.HasPrefix(inChain, "LICORE") {
		t.Errorf("链名应带 LICORE 前缀便于识别: %q / %q", fwdChain, inChain)
	}
}
