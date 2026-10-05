// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package network

// FORWARD 链规则：容器出网放行 + 容器→宿主隔离（H2）。
//
// ## 为什么需要它
//
// 宿主 `FORWARD` 链的**策略通常是 DROP**（本机实测：ufw active + policy DROP）。
// 容器出网必须经过 FORWARD，没有放行规则时**完全无法访问外网**——NAT 规则
// 写得再对也没用（实测：MASQUERADE 已生效，但 `nc 8.8.8.8 53` 仍超时；
// 临时插一条 `-i licore0 -j ACCEPT` 后立刻恢复，确认根因）。
//
// Docker 靠自建的 `DOCKER-FORWARD` 链末尾 `ACCEPT` 兜底解决；LiCore 原先
// 没有任何 FORWARD 规则，因此完全依赖宿主的默认策略——在 ufw 机器上等于
// 容器没有外网。
//
// ## 同时解决 H2（容器→宿主隔离）
//
// 真机实测发现：容器可经网桥网关 `172.22.0.1` 访问宿主上**监听在通配地址**
// 的服务（实测拿到 forgejo 的 `SSH-2.0-Go` 横幅）。Docker 靠 `DOCKER-USER`
// 链供用户加 DROP；LiCore 直接把隔离做进默认规则。
//
// **一次做对**：用一个链同时放行该放行的、堵住该堵的。分两次改同一条链
// 既容易漏，也会让"中间态"（只放行未隔离）短暂存在。
//
// ## 两处链，别搞混（真机踩过）
//
// **容器 → 宿主本机的流量走 `INPUT`，不走 `FORWARD`。**
//
// 这是 netfilter 的路由语义：**目的地址是宿主自身 IP 的包属于「本地投递」
// （local delivery），进入 INPUT 链**；FORWARD 只处理"穿过宿主"的流量
// （容器 ↔ 容器、容器 ↔ 外网）。
//
// 第一版把 H2 的 DROP 全放在 FORWARD 里，实测结果：
//   - 出网成功（走 FORWARD ✅）；
//   - 但 `nc <宿主IP> 2222` 仍然通（走 INPUT ❌），
//     且 FORWARD 里 5 条 DROP 的**命中计数全是 0**；
//   - 临时 `-I INPUT 1 -d <宿主IP> -j DROP` 后立刻封住。
//
// 因此两个链分工：
//
//	LICORE-FORWARD  放行：容器↔容器、容器→外网、回包
//	LICORE-INPUT    隔离：DROP 容器→宿主本机地址
//
// **Docker 也没有解决这个问题**：它的 `DOCKER-USER` 只挂 FORWARD。
// Docker 默认 `-p` 绑 0.0.0.0，用户得自己加 INPUT 规则或改成
// `-p 127.0.0.1:...` 才能规避。LiCore 把它做进默认规则。
//
// ## 规则顺序（安全属性，不可调换）
//
// LICORE-FORWARD：
//
//	1. -i licore0 -o licore0 -j ACCEPT                     容器 ↔ 容器（同网桥）
//	2. -i licore0 -o <外部接口> -j ACCEPT                   容器 → 外网
//	3. -o licore0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT   回包
//	4. -i licore0 -j ACCEPT                                 兜底放行
//
// LICORE-INPUT：
//
//	1. -i licore0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
//	                                                       回包放行（端口映射必需）
//	2. -i licore0 -d <宿主IP> -j DROP                      **H2：堵容器 → 宿主**
//
// INPUT 的第 1 条与 FORWARD 的第 3 条**不是重复**：前者放行"容器→宿主"
// 方向的回包（端口映射的 SYN-ACK 走这里），后者放行"外部→容器"方向的回包。
// 两者方向相反、走的链也不同，缺任何一个都会有连接卡在 SYN_RECV。
//
// INPUT 链**没有兜底 ACCEPT**：INPUT 的默认策略由宿主决定（本机是 ufw 的
// DROP），LiCore 不该替宿主放行自己的入站流量。

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// fwdChain 是 LiCore 独占的 FORWARD 自定义链名（只放行，不隔离）。
//
// 用独立链名而不是往 FORWARD 里直接堆规则：清理时只需删一条跳转 + 删链，
// 不必逐条匹配删除（逐条删容易漏，留下永久规则）。也不复用 Docker 的
// `DOCKER-USER`——那是 Docker 的链，宿主上可能没装 Docker。
const fwdChain = "LICORE-FORWARD"

// inChain 是 LiCore 独占的 INPUT 自定义链名（H2 隔离）。
//
// **为什么隔离必须放 INPUT**：容器访问宿主自身 IP 的包是「本地投递」，
// 走 INPUT 不走 FORWARD（详见文件头说明）。放错链的表现是"规则看着对、
// 命中计数恒为 0、隔离完全不生效"。
const inChain = "LICORE-INPUT"

// fwdRule 是一条 FORWARD 规则的抽象描述。
//
// 用结构化描述而非直接拼命令串，是为了让**顺序**可被单元测试直接断言
// ——顺序在这套规则里是安全属性（见文件头第 4/5 条的顺序要求）。
type fwdRule struct {
	// Kind 标识规则用途，便于测试与日志定位。
	Kind string
	// Args 是 iptables 规则体（不含 -A <chain> 前缀）。
	Args []string
}

// 规则 Kind 常量。
const (
	fwdKindContainerToContainer = "container-to-container"
	fwdKindContainerToExternal  = "container-to-external"
	fwdKindEstablished          = "reply-established"
	fwdKindFallback             = "fallback-accept"
	inKindEstablishedAccept     = "input-established-accept"
	inKindDropToHost            = "drop-to-host"
)

// buildForwardRules 构造 LICORE-FORWARD 链规则（**只放行**）。
//
// hostIPs 与 IFACES 参数：
//   - bridge 是容器的网桥接口名（如 licore0）；
//   - externalIfaces 是"外网出口"接口名列表（如 eth0）。
//
// 本链**不含 DROP**：容器→宿主是本地投递，走 INPUT，由 buildInputRules 负责。
// 第一版把 DROP 放在这里，实测命中计数恒为 0、隔离完全不生效。
func buildForwardRules(bridge string, externalIfaces []string) []fwdRule {
	rules := make([]fwdRule, 0, 6)

	// 1. 容器 ↔ 容器：同网桥互访（bridge 内部转发，不经宿主路由）。
	rules = append(rules, fwdRule{
		Kind: fwdKindContainerToContainer,
		Args: []string{"-i", bridge, "-o", bridge, "-j", "ACCEPT"},
	})

	// 2. 容器 → 外网：每个出口接口一条（多网卡机器要全覆盖）。
	for _, iface := range externalIfaces {
		rules = append(rules, fwdRule{
			Kind: fwdKindContainerToExternal,
			Args: []string{"-i", bridge, "-o", iface, "-j", "ACCEPT"},
		})
	}

	// 3. 回包：宿主 / 外部主动连进来的连接（含 -p 端口映射的回程）。
	//    方向是 -o bridge，靠 conntrack 识别为已建立连接的一部分。
	rules = append(rules, fwdRule{
		Kind: fwdKindEstablished,
		Args: []string{"-o", bridge, "-m", "conntrack",
			"--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	})

	// 4. 兜底放行：不属于上面任何一类、且目的不是宿主本机的流量。
	rules = append(rules, fwdRule{
		Kind: fwdKindFallback,
		Args: []string{"-i", bridge, "-j", "ACCEPT"},
	})

	return rules
}

// buildInputRules 构造 LICORE-INPUT 链规则（**H2 隔离**）。
//
// ## 规则顺序（安全属性，不可调换）
//
//  1. -i <bridge> -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
//  2. -i <bridge> -d <宿主IP> -j DROP   （每个宿主地址一条）
//
// **第 1 条必须在 DROP 之前**，它放行的是**端口映射的回包**。
//
// 回包为什么走 INPUT 而不是 FORWARD：端口映射的流量路径是——
//
//	宿主 curl <宿主IP>:18080
//	  → OUTPUT DNAT，目的改为 172.22.0.3:80，源仍是 <宿主IP>
//	  → SYN 到容器；nginx 回 SYN-ACK：src=172.22.0.3 dst=<宿主IP>
//	  → 目的地址是**宿主自身 IP** ⇒ 本地投递 ⇒ **走 INPUT**
//	  → 被 DROP 的话连接永远停在 SYN_RECV
//
// 真机实测确认过这个失败形态：容器内 `cat /proc/net/tcp` 显示连接状态
// `03`（SYN_RECV），且 `-d <宿主IP> -j DROP` 的命中计数持续增长。
//
// 也就是说「容器→宿主」这个方向**同时包含恶意主动连接与合法回包**，
// 二者只能靠 conntrack 区分：回包是已建立连接的一部分（ESTABLISHED）。
// 第一版没有这条放行规则，H2 隔离生效了，但**端口映射也一起失效**。
//
// 跳过网桥自身地址：容器访问网关（如 172.22.0.1）是本地投递，
// 把网关也 DROP 会让指向网关的访问全部失败。同网桥互访由 FORWARD 覆盖。
//
// hostIPs 为空时只保留放行规则：宁可不隔离，也不生成"堵 0.0.0.0/0"
// 之类的宽泛规则——那会把容器出网一并堵死。调用方据此决定告警。
func buildInputRules(bridge string, hostIPs []string) []fwdRule {
	rules := make([]fwdRule, 0, len(hostIPs)+1)

	// 1. 回包放行。**必须在 DROP 之前**，否则端口映射失效。
	rules = append(rules, fwdRule{
		Kind: inKindEstablishedAccept,
		Args: []string{"-i", bridge, "-m", "conntrack",
			"--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	})

	// 2. H2 隔离：容器主动发起、目的为宿主本机地址的新连接一律 DROP。
	for _, ip := range hostIPs {
		if ip == "" || isBridgeOwnAddr(ip, bridge) {
			continue
		}
		rules = append(rules, fwdRule{
			Kind: inKindDropToHost,
			Args: []string{"-i", bridge, "-d", ip, "-j", "DROP"},
		})
	}
	return rules
}

// isBridgeOwnAddr 判断某地址是否属于网桥自身（容器网关）。
//
// 为什么必须排除：容器 DNS 指向网关（resolv.conf 里是 172.22.0.1），
// 若把网关地址也放进 DROP，DNS 解析会全部超时——容器表现为"域名不通但
// IP 通"，极难排查。同网桥互访已由第 1 条覆盖。
//
// 判定依据不是"地址是否形如网关"，而是**该地址是否配在网桥接口上**：
// 由调用方在枚举宿主地址时标注来源接口，这里按已知的网桥地址集合比对。
var bridgeOwnAddrs = func(iface string) []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil || ifi == nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil {
			continue
		}
		// 只收集确实配在该接口上的地址。
		if ownAddrBelongsTo(ifi, ipn.IP) {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

// ownAddrBelongsTo 判断 addr 是否配在 ifi 上。
func ownAddrBelongsTo(ifi *net.Interface, addr net.IP) bool {
	addrs, err := ifi.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ipn.IP.Equal(addr) {
			return true
		}
	}
	return false
}

// isBridgeOwnAddr 判断 ip 是否为网桥自身的地址（用注入的查询函数，便于测试）。
var isBridgeOwnAddr = func(ip, iface string) bool {
	for _, own := range bridgeOwnAddrs(iface) {
		if own == ip {
			return true
		}
	}
	return false
}

// hostIPv4Addrs 枚举宿主所有 IPv4 地址（去重、排序）。
//
// 覆盖 127.0.0.1 与各接口地址：容器访问宿主上任意一个本地地址都应当被
// 第 4 条 DROP 拦住，只堵 eth0 地址会留下回环与其它网桥的绕过路径。
func hostIPv4Addrs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil {
			continue
		}
		s := ipn.IP.String()
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// externalIfaces 返回"容器出网应走的出口接口"列表。
//
// 判定：排除回环、排除 LiCore 自己的网桥、排除其它容器网桥（docker0 /
// br-* / virbr*），其余 UP 且非点对点的接口即视为出口。
//
// 为什么不硬编码 eth0：宿主可能有多张网卡（多宿主 / 云主机常有内网+外网
// 两张），只放行 eth0 会让走另一张网卡的容器没有外网。
func externalIfaces() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isBridgeLikeIface(ifi.Name) {
			continue
		}
		out = append(out, ifi.Name)
	}
	sort.Strings(out)
	return out
}

// isBridgeLikeIface 判断接口是否像容器网桥（应排除在"外网出口"之外）。
func isBridgeLikeIface(name string) bool {
	if name == "licore0" || name == "docker0" {
		return true
	}
	for _, prefix := range []string{"br-", "veth", "virbr", "vpe", "tap", "tun"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// fwdEnsureChain 幂等创建 LICORE-FORWARD 链并把跳转插到 FORWARD 首位。
//
// **插入位置选首位（-I FORWARD 1）**：本机 FORWARD 链的第 3–8 条全是 ufw
// 的链（ufw-before-* / ufw-after-* / ufw-reject-*），其中 ufw-reject-forward
// 会拒绝未被 ufw 规则放行的转发流量。插在首位可保证 LiCore 自己的判定
// **先于** ufw 求值，行为不随宿主 ufw 配置漂移。
//
// 与 Docker 的共存：Docker 的 DOCKER-USER / DOCKER-FORWARD 在前两位，
// 我们插到首位会把它们顺次后移——这不影响 Docker（其链是独立求值的跳转，
// 且 DOCKER-USER 默认为空）。反过来若插在 Docker 之后，一旦 DOCKER-FORWARD
// 末尾的 ACCEPT 先命中，LiCore 的兜底放行就永远轮不到。
func fwdEnsureChain() error {
	if err := runIptables("-N", fwdChain); err != nil {
		return err
	}
	// 先删旧跳转再插首位：重复 run 会让 FORWARD 里堆叠多条同名跳转
	// （真机在 nat 表上见过这个现象）。
	_ = runIptables("-D", "FORWARD", "-j", fwdChain)
	return runIptables("-I", "FORWARD", "1", "-j", fwdChain)
}

// inEnsureChain 幂等创建 LICORE-INPUT 链并把跳转插到 INPUT 首位。
//
// 同样插首位：ufw 的 INPUT 链结构是 ufw-before-input → ufw-user-input →
// ufw-after-input，未被其规则放行的入站流量最终落到宿主默认策略。
// 插在首位保证 LiCore 的 DROP **先于** ufw 求值——否则一旦 ufw 先 ACCEPT，
// 我们的 DROP 永远轮不到。这正是第一版把规则放 FORWARD 时的失败形态
// （规则看着对、命中计数恒为 0、隔离完全不生效）。
func inEnsureChain() error {
	if err := runIptables("-N", inChain); err != nil {
		return err
	}
	_ = runIptables("-D", "INPUT", "-j", inChain)
	return runIptables("-I", "INPUT", "1", "-j", inChain)
}

// fwdFlushChain 清空 LICORE-FORWARD 内的规则（保留链与跳转）。
func fwdFlushChain() {
	_ = runIptables("-F", fwdChain)
}

// inFlushChain 清空 LICORE-INPUT 内的规则（保留链与跳转）。
func inFlushChain() {
	_ = runIptables("-F", inChain)
}

// fwdTeardown 彻底移除 LICORE-FORWARD 与 LICORE-INPUT 链及其跳转。
//
// 顺序不可交换：先删宿主链上的跳转，否则链被引用而无法删除。
// 幂等：链或跳转不存在时静默通过（runIptables 把"不存在"类错误视为幂等）。
func fwdTeardown() error {
	var firstErr error
	// FORWARD 侧
	if err := runIptables("-D", "FORWARD", "-j", fwdChain); err != nil && firstErr == nil {
		firstErr = err
	}
	_ = runIptables("-F", fwdChain)
	if err := runIptables("-X", fwdChain); err != nil && firstErr == nil {
		firstErr = err
	}
	// INPUT 侧（H2 隔离链）
	if err := runIptables("-D", "INPUT", "-j", inChain); err != nil && firstErr == nil {
		firstErr = err
	}
	_ = runIptables("-F", inChain)
	if err := runIptables("-X", inChain); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// applyForwardRules 实化 LICORE-FORWARD（放行）与 LICORE-INPUT（隔离）。
//
// 两件事一起做，因为它们是同一需求的两面：既要容器能出网，又要它碰不到宿主。
// 分开做会让"只放行、未隔离"的中间态短暂存在。
//
// 失败必须冒泡：FORWARD 规则缺失时容器**没有外网**（宿主 policy DROP 时），
// INPUT 规则缺失时 **H2 隔离失效**。两种都不能静默通过。
func (n *Network) applyForwardRules() error {
	bridge := bridgeHostIface(n)
	if bridge == "" {
		return fmt.Errorf("%w: 网桥接口名未知，无法生成 FORWARD/INPUT 规则", ErrNATApply)
	}

	// FORWARD：放行容器出网、容器互访、回包。
	if err := fwdEnsureChain(); err != nil {
		return err
	}
	fwdFlushChain()
	for _, r := range buildForwardRules(bridge, externalIfaces()) {
		if err := runIptables(append([]string{"-A", fwdChain}, r.Args...)...); err != nil {
			return err
		}
	}

	// INPUT：隔离容器 → 宿主本机地址（H2）。
	if err := inEnsureChain(); err != nil {
		return err
	}
	inFlushChain()
	hostIPs := hostIPv4Addrs()
	if len(hostIPs) == 0 {
		// 枚举不到宿主地址说明环境异常。此时**不能**静默继续——那会让
		// H2 隔离悄悄不生效，而用户以为已经隔离了。
		return fmt.Errorf("%w: 未能枚举宿主 IPv4 地址，容器→宿主隔离无法生效",
			ErrNATApply)
	}
	for _, r := range buildInputRules(bridge, hostIPs) {
		if err := runIptables(append([]string{"-A", inChain}, r.Args...)...); err != nil {
			return err
		}
	}
	return nil
}

// removeForwardRules 移除 FORWARD 与 INPUT 链（网络拆除 / 容器下线时调用）。
func (n *Network) removeForwardRules() error {
	return fwdTeardown()
}
