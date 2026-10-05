// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package network

// iptables 回退后端。
//
// **为什么需要它**：真机实测发现，Ubuntu 22.04 标准版 nftables 1.0.2 上
// `masquerade` 语句不可用（`Could not process rule: No such file or directory`），
// 而**同一个 nft 的 `dnat` 正常**、**iptables 的 `-j MASQUERADE` 也正常**。
// 也就是说：内核的 NAT 能力在（xt_MASQUERADE 路径可用），只是 nft 的原生
// NAT 表达式路径在这台机器上不通。
//
// 后果是容器**完全无法出网**。产出"容器起来了但连不上网"还不如启动失败，
// 因此这里提供回退：nft 优先（新系统体验更好），失败则走 iptables。
//
// iptables 不是容器组件，而是内核 netfilter 的用户态工具，引入它不违反
// 「零外部容器组件」的项目定位。
//
// **链设计（真机踩过的坑，不要简化）**：用**三个独立的自定义链**，各自只挂
// 一个内建链：
//
//	nat POSTROUTING  → LICORE-POST  （出口 MASQUERADE）
//	nat PREROUTING   → LICORE-PRE   （外部进来的 DNAT）
//	nat OUTPUT       → LICORE-OUT   （宿主本机发起的 DNAT）
//
// **不能只用一条链去挂三个 hook**：iptables-nft 后端下，一条链挂到某个内建链
// 时其 hook 类型即被绑定，再用同一条链挂**不同 hook** 的内建链会报
//
//	iptables v1.8.7 (nf_tables): RULE_INSERT failed (Invalid argument)
//	                                 rule in chain PREROUTING
//
// （真机实测。注意报错点是"挂引用"那一步而不是"加规则"，极易误判成规则语法
// 问题——排查时先怀疑链复用，再怀疑规则体。）三个 hook 类型不同
// （prerouting / postrouting / output），因此必须分链。
//
// 用自定义链而不是直接写内建链，是为了清理时能**精确移除自己的规则**：
// 生产机上 ufw / Docker 的规则都在同一张 nat 表里，flush 内建链会闯大祸。

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// iptables 自定义链名。三个 hook 各一条，见文件头说明。
const (
	iptChainPost = "LICORE-POST" // 挂 POSTROUTING：出口伪装
	iptChainPre  = "LICORE-PRE"  // 挂 PREROUTING：外部进来的 DNAT
	iptChainOut  = "LICORE-OUT"  // 挂 OUTPUT：宿主本机发起的 DNAT
)

// iptChainBindings 是「内建链 → 自定义链」的绑定表。
var iptChainBindings = []struct{ builtin, chain string }{
	{"POSTROUTING", iptChainPost},
	{"PREROUTING", iptChainPre},
	{"OUTPUT", iptChainOut},
}

// iptablesAvailable 报告 iptables 是否可用（仅探测，不执行任何变更）。
func iptablesAvailable() bool {
	_, err := exec.LookPath("iptables")
	return err == nil
}

// runIptables 执行 iptables 并贴上下文。
//
// 与 runNft 同一套错误分类：权限不足包 ErrNotRoot，其余包 ErrNATApply。
// 幂等类错误（链已存在 / 规则不存在）忽略，保证重放安全。
func runIptables(args ...string) error {
	cmd := exec.Command("iptables", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if containsAny(msg, iptIdempotentMarkers...) {
		return nil
	}
	if isPermissionDenied(msg) {
		return fmt.Errorf("%w: iptables %s: %v (%s)",
			ErrNotRoot, strings.Join(args, " "), err, msg)
	}
	return fmt.Errorf("%w: iptables %s: %v (%s)",
		ErrNATApply, strings.Join(args, " "), err, msg)
}

// iptIdempotentMarkers 是"重复操作"类错误的识别串（清理与重放都会遇到）。
var iptIdempotentMarkers = []string{
	"Chain already exists",
	"chain with name",
	"No chain/target/match by that name",
	"Bad rule (does a matching rule exist in that chain?)",
	"does not exist",
}

// iptEnsureChains 幂等创建三条自定义链并把它们各挂到一个内建链上。
//
// 跳转用 `-I ... 1`（插到首位）而不是 `-A`（追加）：宿主上已有 ufw / Docker
// 的规则，插到最前面才能保证 LiCore 的 NAT 先于它们求值，不被既有规则截胡。
// 同一条跳转重复插入会堆叠（真机曾见 POSTROUTING 里挂了两条），因此先删再插。
func iptEnsureChains() error {
	for _, b := range iptChainBindings {
		if err := runIptables("-t", "nat", "-N", b.chain); err != nil {
			return err
		}
		// 先删可能存在的同名跳转（重复 run 会堆叠），再插到首位。
		// 删除失败是正常的（首次运行时还没有），因此忽略错误。
		_ = runIptables("-t", "nat", "-D", b.builtin, "-j", b.chain)
		if err := runIptables("-t", "nat", "-I", b.builtin, "1", "-j", b.chain); err != nil {
			return err
		}
	}
	return nil
}

// iptFlushChains 清空三条自定义链内的所有规则（保留链本身与跳转）。
func iptFlushChains() {
	for _, b := range iptChainBindings {
		_ = runIptables("-t", "nat", "-F", b.chain)
	}
}

// iptTeardown 彻底移除三条自定义链及其跳转。
//
// 顺序**不可交换**：必须先删内建链上的跳转，才能 flush 并删自定义链
// ——被引用的链无法删除（内核会拒绝）。
func iptTeardown() error {
	var firstErr error
	for _, b := range iptChainBindings {
		if err := runIptables("-t", "nat", "-D", b.builtin, "-j", b.chain); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, b := range iptChainBindings {
		_ = runIptables("-t", "nat", "-F", b.chain)
		if err := runIptables("-t", "nat", "-X", b.chain); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// applyPortRulesIptables 用 iptables 实化出口 NAT 与端口 DNAT。
//
// 与 nft 版本逐条对应：
//
//	nft post_nat  ip saddr <subnet> masquerade
//	  → LICORE-POST: -s <subnet> -j MASQUERADE
//	nft post_nat  ip saddr 127.0.0.0/8 ip daddr <subnet> masquerade
//	  → LICORE-POST: -s 127.0.0.0/8 -d <subnet> -j MASQUERADE
//	nft pre_nat   <proto> dport <hp> dnat to <cip>:<cp>
//	  → LICORE-PRE:  -p <proto> --dport <hp> -j DNAT --to-destination <cip>:<cp>
//	nft out_nat   ip daddr 127.0.0.0/8 <proto> dport <hp> dnat to <cip>:<cp>
//	  → LICORE-OUT:  -p <proto> --dport <hp> -j DNAT --to-destination <cip>:<cp>
//
// DNAT 规则体在 PRE 与 OUT 里相同；分链只是因为 hook 不同（见文件头说明）。
func (n *Network) applyPortRulesIptables() error {
	if err := iptEnsureChains(); err != nil {
		return err
	}
	iptFlushChains()

	if !n.Internal {
		if err := runIptables(iptablesMasqRule(n.Subnet)...); err != nil {
			return err
		}
		if err := runIptables(iptablesLoopbackMasqRule(n.Subnet)...); err != nil {
			return err
		}
	}
	for _, e := range n.Endpoints {
		for _, p := range e.Ports {
			if err := runIptables(iptablesDNATRule(e, p)...); err != nil {
				return err
			}
			if err := runIptables(iptablesOutDNATRule(e, p)...); err != nil {
				return err
			}
		}
	}
	return nil
}

// iptablesDNATRule 与 nft 版 dnatRuleArgs 对应的纯逻辑（写进 LICORE-PRE）。
func iptablesDNATRule(e *Endpoint, p *PortMapping) []string {
	return append([]string{"-t", "nat", "-A", iptChainPre}, iptDNATBody(e, p)...)
}

// iptablesOutDNATRule 与 nft 版 dnatOutRuleArgs 对应的纯逻辑（写进 LICORE-OUT）。
func iptablesOutDNATRule(e *Endpoint, p *PortMapping) []string {
	return append([]string{"-t", "nat", "-A", iptChainOut}, iptDNATBody(e, p)...)
}

// iptDNATBody 是 DNAT 规则的公共规则体。
//
// 抽成纯函数是为了让"规则长什么样"可被无 root 环境测试——真机验证只能证明
// 一次，而规则一旦被改错（漏掉 -j DNAT、宿主/容器端口写反）需要测试长期锁住。
func iptDNATBody(e *Endpoint, p *PortMapping) []string {
	proto := "tcp"
	if p.Proto == ProtoUDP {
		proto = "udp"
	}
	return []string{
		"-p", proto, "--dport", strconv.Itoa(p.HostPort),
		"-j", "DNAT",
		"--to-destination", e.IP + ":" + strconv.Itoa(p.ContainerPort),
	}
}

// iptablesMasqRule 与 nft 版 natMasqArgs 对应的纯逻辑。
func iptablesMasqRule(subnet string) []string {
	return []string{"-t", "nat", "-A", iptChainPost, "-s", subnet, "-j", "MASQUERADE"}
}

// iptablesLoopbackMasqRule 与 nft 版 natLoopbackMasqArgs 对应的纯逻辑。
func iptablesLoopbackMasqRule(subnet string) []string {
	return []string{"-t", "nat", "-A", iptChainPost,
		"-s", "127.0.0.0/8", "-d", subnet, "-j", "MASQUERADE"}
}

// ErrBothBackendsFailed 表示 nft 与 iptables 两条路径都失败。
var ErrBothBackendsFailed = errors.New("licore/network: nft 与 iptables 均无法应用 NAT 规则")
