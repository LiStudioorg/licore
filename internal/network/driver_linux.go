// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package network

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"

	"github.com/LiStudioorg/licore/internal/network/netlink"
)

// bridgeHostIface 返回某网络在宿主上的网桥接口名。
func bridgeHostIface(n *Network) string { return bridgeHostInterface(n.Name) }

// hostDefaultIface 探测宿主默认出口接口（用于 NAT 出口）。
func hostDefaultIface() (string, error) {
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err != nil {
		return "", fmt.Errorf("读取默认路由失败: %w", err)
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("未发现宿主默认出口接口")
}

// driverBootstrap 按驱动创建并上线宿主侧网络拓扑。
func driverBootstrap(n *Network) error {
	if n.Driver != DriverBridge {
		return nil
	}
	br := bridgeHostIface(n)
	if _, err := netlink.LinkByName(br); err == nil {
		return nil // 已存在，幂等
	}
	if err := netlink.NewLink(br, netlink.KindBridge); err != nil {
		return fmt.Errorf("创建网桥 %s: %w", br, err)
	}
	if err := netlink.LinkUp(br); err != nil {
		return fmt.Errorf("上线网桥 %s: %w", br, err)
	}
	_, ipnet, err := net.ParseCIDR(n.Subnet)
	if err != nil {
		return fmt.Errorf("网段 %s 非法: %w", n.Subnet, ErrBadNetwork)
	}
	ones, _ := ipnet.Mask.Size()
	if err := netlink.AddAddr(br, n.Gateway, ones); err != nil {
		return fmt.Errorf("为网桥添加网关 %s/%d: %w", n.Gateway, ones, err)
	}
	return nil
}

// driverTeardown 删除网络在宿主侧的网桥。
func driverTeardown(n *Network) error {
	if n.Driver != DriverBridge {
		return nil
	}
	return netlink.DelLink(bridgeHostIface(n))
}

// bridgeAttachEndpoints 为 bridge 网络实化端点（本分支仅登记端点；veth
// 对与容器命名空间注入由运行时的 infra 侧配合，见交接摘要）。
func bridgeAttachEndpoints(n *Network) error { return nil }

// bridgeDetachEndpoint 清理某容器的 veth（忽略已不存在的接口）。
func bridgeDetachEndpoint(veth string) error {
	if err := netlink.DelLink(veth); err != nil {
		slog.Debug("删除 veth 失败（可能已不存在）", "veth", veth, "err", err)
	}
	return nil
}

// nftCheck 检查 nft 是否可用；不可用返回 ErrNoFirewall。
func nftCheck() error {
	if _, err := exec.LookPath("nft"); err != nil {
		return ErrNoFirewall
	}
	return nil
}

// nftCreateTables 幂等创建 nft 表与三条链（post_nat=出口/回环 NAT，
// pre_nat=外部进来流量的 DNAT，out_nat=宿主本机发起流量的 DNAT）。
func nftCreateTables() error {
	if err := nftCheck(); err != nil {
		return err
	}
	// 表
	_ = runNft("add", "table", "ip", "licore")
	// 链：post_nat / pre_nat / out_nat
	_ = runNft("add", "chain", "ip", "licore", "post_nat",
		"{ type nat hook postrouting priority srcnat; policy accept; }")
	_ = runNft("add", "chain", "ip", "licore", "pre_nat",
		"{ type nat hook prerouting priority dstnat; policy accept; }")
	_ = runNft("add", "chain", "ip", "licore", "out_nat",
		"{ type nat hook output priority -100; policy accept; }")
	return nil
}

// nftDropTables 删除 LiCore 自建的 nft 表（幂等；表不存在时静默通过）。
//
// 用于 nft 路径**部分失败后的清理**：nftCreateTables 会先把表和三条链建好，
// 若随后的规则写入失败（如本机 masquerade 不受支持），那张表就**残留**下来
// ——空的三条链虽无功能影响，但会让"nft 表是否存在"这类诊断产生误导，
// 也污染宿主规则集。
//
// 真机实测确认过这个残留：nft 路径失败后 `nft list tables` 里仍有
// `table ip licore`（0 条规则）。
func nftDropTables() {
	_ = runNft("delete", "table", "ip", "licore")
}

// nftIdempotentMarkers 是"重复操作"类错误的识别串。
//
// **不要往里加 "No such file or directory"**：nft 在**缺少内核特性**时
// 也用这句话报错，最典型的是本机实测到的
//
//	# nft add rule ip test post masquerade
//	Error: Could not process rule: No such file or directory
//	                                            ^^^^^^^^^^
//
// 把它当幂等错误吞掉，会让 NAT 规则**静默不生效**：容器照常启动、
// 用户却连不上网。真正的"表/链已存在"报的是 "File exists"。
var nftIdempotentMarkers = []string{
	"File exists",
	"already exists",
}

// runNft 运行 nft 子进程并贴上下文；只容忍**幂等类**错误（表/链已存在）。
//
// 失败时返回包装了 ErrNATApply 的错误，且带上：完整命令、nft 原始 stderr、
// 以及可能原因提示。错误必须能被调用方冒泡到 `licore run`——静默降级会让
// 用户拿到一个"起来了但连不上网"的容器，比启动失败难排查得多。
func runNft(args ...string) error {
	cmd := exec.Command("nft", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if containsAny(msg, nftIdempotentMarkers...) {
		return nil
	}
	// 权限不足单独归类：非 root 时容器本来就起不来（网桥都建不了），
	// 调用方据此保持"降级告警"行为，而不是把整个 run 判死。
	// 其余失败（如本机 nft 不支持 masquerade）才是真正的配置错误，
	// 必须让 run 失败——容器起来了却连不上网是最难排查的状态。
	if isPermissionDenied(msg) {
		return fmt.Errorf("%w: nft %s: %v (%s)",
			ErrNotRoot, strings.Join(args, " "), err, msg)
	}
	return fmt.Errorf("%w: nft %s: %v (%s)%s",
		ErrNATApply, strings.Join(args, " "), err, msg, nftFailureHint(msg))
}

// isPermissionDenied 判断 nft 的失败是否属于"权限不足"。
//
// nft 在非 root 下报 "Operation not permitted (you must be root)"，
// 在缺 CAP_NET_ADMIN 时可能只给 EPERM 字样；两者都要认。
func isPermissionDenied(stderr string) bool {
	return containsAny(stderr, "Operation not permitted", "you must be root", "Permission denied")
}

// nftFailureHint 针对已知的 nft 失败给出可执行提示。
//
// 目前只覆盖一条真机实测遇到的：Ubuntu 22.04 标准版 nftables 1.0.2 上
// `masquerade` 语句不可用（同一个 nft 的 `dnat` 却正常）。这不是版本太老，
// 而是该语句依赖的内核 NAT 注册路径在这台机器上不通；iptables 的
// `-j MASQUERADE`（xt_MASQUERADE 路径）在同一台机器上可用——因此提示里
// 直接给出可用的替代命令。
func nftFailureHint(stderr string) string {
	if !strings.Contains(stderr, "No such file or directory") {
		return ""
	}
	return "；若失败的是 masquerade，说明本机 nft 不支持该语句" +
		"（可用 `nft add rule ... masquerade` 复现），" +
		"iptables 的 MASQUERADE 通常仍可用"
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// applyPortRules 实化出口 MASQUERADE 与全部端口 DNAT。
//
// **后端选择**：nft 优先，失败则回退 iptables。
//
// 为什么要有回退：真机实测 Ubuntu 22.04 标准版 nftables 1.0.2 上
// `masquerade` 语句不可用，而**同一个 nft 的 `dnat` 正常**、**iptables 的
// `-j MASQUERADE` 也正常**——内核 NAT 能力在，只是 nft 的原生表达式路径
// 在这台机器上不通。没有回退时容器**完全无法出网**。
//
// 保留 nft 优先的理由：新系统（Ubuntu 24.04+）nft 原生支持更好，
// 且 nft 是 netfilter 的当前主线工具。
//
// 回退只在 **ErrNATApply**（工具在、命令失败）时触发；ErrNotRoot 不触发
// ——权限不足时 iptables 同样会失败，换个工具只是把同一个错误重演一遍。
//
// 先 flush 再 add rule：既保证语法正确，也保证重放幂等（避免规则重复堆叠）。
func (n *Network) applyPortRules() error {
	if n.Driver != DriverBridge {
		return nil
	}
	// 先试 nft。
	nftErr := n.applyPortRulesNft()
	if nftErr == nil {
		n.NATBackend = natBackendNft.String()
	} else if errors.Is(nftErr, ErrNotRoot) {
		// 权限不足：换 iptables 也一样失败（同样需要 CAP_NET_ADMIN），
		// 直接返回原错误，避免把同一个权限问题重演一遍后给出误导性报错。
		return nftErr
	} else if !iptablesAvailable() {
		// nft 失败且无回退可用：清掉 nft 可能已建的表再报错。
		nftDropTables()
		return fmt.Errorf("%w；nft 失败原因: %v；iptables 也不可用（未安装），"+
			"请安装 nftables（推荐，需内核支持 masquerade）或 iptables",
			ErrBothBackendsFailed, nftErr)
	} else if iptErr := n.applyPortRulesIptables(); iptErr != nil {
		nftDropTables()
		return fmt.Errorf("%w；nft 失败: %v；iptables 回退也失败: %w",
			ErrBothBackendsFailed, nftErr, iptErr)
	} else {
		// **nft 失败但 iptables 成功**：nft 可能已建了一半（表和链成功、
		// 规则失败），必须清掉，否则残留一张空的 `ip licore` 表。
		// 真机实测确认过这个残留。
		nftDropTables()
		slog.Info("nft 不可用，NAT 已回退到 iptables",
			slog.String("net", n.Name), slog.Any("nft_err", nftErr))
		n.NATBackend = natBackendIptables.String()
	}

	// FORWARD 链：放行容器出网 + 隔离容器→宿主（H2）。
	//
	// **独立于 NAT 后端**：NAT 决定"地址怎么改写"，FORWARD 决定"包能不能过"。
	// 宿主 FORWARD 策略普遍是 DROP（ufw active 时必然如此），缺了这套规则
	// 容器**完全没有外网**——NAT 写得再对也没用。真机实测：MASQUERADE 已生效
	// 但 `nc 8.8.8.8 53` 仍超时；临时插一条 `-i licore0 -j ACCEPT` 后立刻恢复。
	//
	// 固定用 iptables 实现：本机 nft 的 masquerade 不可用，而 FORWARD 与 NAT
	// 是两个独立机制，不要求同后端。若两条路径都不可用，上面的 NAT 分支已返回。
	if err := n.applyForwardRules(); err != nil {
		return err
	}
	return nil
}

// applyPortRulesNft 是原先的 nft 实现（表 + 三条链 + 规则）。
func (n *Network) applyPortRulesNft() error {
	if err := nftCreateTables(); err != nil {
		return err
	}
	n.flushChains()
	// 出口 NAT
	if !n.Internal {
		if err := runNft(natMasqArgs(n.Subnet)...); err != nil {
			return err
		}
		// 回环源伪装：与 out_nat 配套，缺一不可，见 natLoopbackMasqArgs 注释。
		if err := runNft(natLoopbackMasqArgs(n.Subnet)...); err != nil {
			return err
		}
	}
	for _, e := range n.Endpoints {
		for _, p := range e.Ports {
			if err := n.addDnatRule(e, p); err != nil {
				return err
			}
			// 同一映射在 out_nat 再挂一份，覆盖宿主本机发起的流量。
			if err := runNft(dnatOutRuleArgs(e, p)...); err != nil {
				return err
			}
		}
	}
	return nil
}

// natMasqArgs 返回出口的 NAT 的 nft 参数（add rule，语法与 nft -c 校验一致：
// `nft add rule ip licore post_nat ip saddr <subnet> masquerade`）。
func natMasqArgs(subnet string) []string {
	return []string{"add", "rule", "ip", "licore", "post_nat",
		"ip", "saddr", subnet, "masquerade"}
}

// natLoopbackMasqArgs 返回**回环源地址**的 NAT 参数。
//
// 为什么需要那条规则：宿主上 `curl localhost:18080` 的数据包源地址是
// 127.0.0.1。DNAT 把目的改成容器 IP 后，报文从网桥送出，容器看到的源地址
// 仍是 127.0.0.1——而容器自己的 netns 里 127.0.0.0/8 是本地路由，回包因此
// 发到容器**自己**的回环、永远回不到宿主。把源地址伪装成网桥 IP 后回包才
// 正常返回。此规则与 out_nat 配套：只加 out_nat 时表现是"connection
// timed out"（容器 lo DOWN 时假性可通，拉起 lo 后必然超时）。
//
// 目的网段限定在本子网，不影响容器访问外部 127.x 地址。
func natLoopbackMasqArgs(subnet string) []string {
	return []string{"add", "rule", "ip", "licore", "post_nat",
		"ip", "saddr", "127.0.0.0/8", "ip", "daddr", subnet, "masquerade"}
}

// dnatRuleArgs 返回单条 DNAT 的 nft 参数（add rule：
// `nft add rule ip licore pre_nat tcp dport <hp> dnat to <cip>:<cport>`）。
func dnatRuleArgs(e *Endpoint, p *PortMapping) []string {
	proto := "tcp"
	if p.Proto == ProtoUDP {
		proto = "udp"
	}
	return []string{"add", "rule", "ip", "licore", "pre_nat",
		proto, "dport", strconv.Itoa(p.HostPort),
		"dnat", "to", e.IP + ":" + strconv.Itoa(p.ContainerPort)}
}

// dnatOutRuleArgs 返回**宿主本机发起**流量的 DNAT 规则（挂在 out_nat 即以
// output hook 生效）。
//
// 为什么需要：prerouting 只处理**从网卡进来**的报文，本机进程（curl
// localhost:8080、本机健康检查、浏览器点 127.0.0.1）走的根本是 output 路径。
// 少这一条时宿主本机连自己的发布端口是 connection refused。限定只显示目的
// 地址是回环段的流量，避免把"访问外部主机同端口"也误 DNAT 进容器。
func dnatOutRuleArgs(e *Endpoint, p *PortMapping) []string {
	proto := "tcp"
	if p.Proto == ProtoUDP {
		proto = "udp"
	}
	return []string{"add", "rule", "ip", "licore", "out_nat",
		"ip", "daddr", "127.0.0.0/8",
		proto, "dport", strconv.Itoa(p.HostPort),
		"dnat", "to", e.IP + ":" + strconv.Itoa(p.ContainerPort)}
}

// flushChains 清空 NAT 表的三条链（表或链不存在时幂等，为空时清空）。
// out_nat 必须一并清空：applyPortRules 重放时若只 flush 两条，宿主本机
// DNAT 规则会随每次 run 无限堆叠。
func (n *Network) flushChains() {
	_ = runNft("flush", "chain", "ip", "licore", "post_nat")
	_ = runNft("flush", "chain", "ip", "licore", "pre_nat")
	_ = runNft("flush", "chain", "ip", "licore", "out_nat")
}

// deleteAllDnat 清空 NAT 规则（在重放数据前调用，避免重复规则堆叠）。
//
// **按实际使用的后端清理**：nft 与 iptables 的规则互不相通，用错后端会
// 留下残余规则。旧网络定义（无 natBackend 字段）默认按 nft 清理——那是
// v0.9.3 之前唯一可能的写入位置。
func (n *Network) deleteAllDnat() {
	switch n.backend() {
	case natBackendIptables:
		iptFlushChains()
	default:
		n.flushChains()
	}
}

func (n *Network) addDnatRule(e *Endpoint, p *PortMapping) error {
	return runNft(dnatRuleArgs(e, p)...)
}

// removePortRules 移除全部 NAT 与 FORWARD 规则（容器下线 / 网络拆除时调用）。
//
// iptables 后端要**彻底拆除**（删跳转 → flush → 删链），而不只是 flush：
// 留着一条挂在内建链上的空跳转，会让宿主上每次经过该表的报文都多绕一跳，
// 且下次 run 若改用别的后端就成了孤儿规则。
//
// FORWARD 链**无论 NAT 用哪个后端都要拆**：它固定由 iptables 实现，
// 不带 NATBackend 判断——按 NAT 后端决定是否拆 FORWARD 会漏掉
// "NAT 走 nft、FORWARD 走 iptables"的混合场景（本机的实际形态）。
func (n *Network) removePortRules(veth string) error {
	_ = veth
	// 先拆 FORWARD（与 NAT 后端无关）。
	if err := n.removeForwardRules(); err != nil {
		slog.Debug("拆除 FORWARD 链失败（可能本就不存在）", slog.Any("err", err))
	}
	if n.backend() == natBackendIptables {
		// 失败不阻断：可能是链本就不存在（幂等）。
		if err := iptTeardown(); err != nil {
			slog.Debug("拆除 iptables NAT 链失败（可能本就不存在）", slog.Any("err", err))
		}
		return nil
	}
	n.deleteAllDnat()
	return nil
}

var _ = hostDefaultIface
var _ = netlink.KindVeth
