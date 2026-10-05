// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package network

import (
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
// 先 flush 两条链再 add rule：既保证语法正确（add rule），也保证重放幂等
// （避免 replace 语法错误以及规则重复堆叠）。
func (n *Network) applyPortRules() error {
	if n.Driver != DriverBridge {
		return nil
	}
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

// deleteAllDnat 清空 pre_nat 链（在重放数据前调用，避免重复规则堆叠）。
func (n *Network) deleteAllDnat() {
	n.flushChains()
}

func (n *Network) addDnatRule(e *Endpoint, p *PortMapping) error {
	return runNft(dnatRuleArgs(e, p)...)
}

// removePortRules 移除全部 DNAT 规则（简化实现：flush 整链）。
func (n *Network) removePortRules(veth string) error {
	_ = veth
	n.deleteAllDnat()
	return nil
}

var _ = hostDefaultIface
var _ = netlink.KindVeth
