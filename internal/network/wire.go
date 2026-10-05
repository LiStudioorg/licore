// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package network

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LiStudioorg/licore/internal/network/netlink"
)

// vethPair 返回某容器在某桥接网络上的 veth 接口名（网桥侧与容器侧）。
// 命名规则与 Manager.Connect / Disconnect 保持一致，保证两处算出同名。
func vethPair(netName, containerID string) (hostVeth, peerVeth string) {
	return epVethName(netName, containerID), epPeerName(netName, containerID)
}

// AttachVeth 在宿主命名空间装配容器 veth：创建一对 veth，
// 网桥侧挂接到宿主网桥并上线，容器侧移入 childPID 所在容器 netns。
// 由 runtime.StartWith 在 fork 出容器 init 后调用（childPID 为其 PID）。
func AttachVeth(netName, containerID string, childPID int) error {
	hostVeth, peerVeth := vethPair(netName, containerID)
	br := bridgeHostIface(&Network{Name: netName})

	// 清理可能的残留（上次异常退出留下的同名接口），避免 EXCL 冲突。
	_ = netlink.DelLink(hostVeth)
	_ = netlink.DelLink(peerVeth)
	if err := netlink.NewVeth(hostVeth, peerVeth); err != nil {
		return fmt.Errorf("创建 veth 对 %s/%s: %w", hostVeth, peerVeth, err)
	}
	removeOnErr := func(step string, err error) error {
		_ = netlink.DelLink(hostVeth)
		slog.Debug("veth 装配失败，清理", "step", step, "err", err)
		return fmt.Errorf("%s: %w", step, err)
	}
	if err := netlink.SetLinkMaster(hostVeth, br); err != nil {
		return removeOnErr("挂接网桥侧 veth 到 "+br, err)
	}
	if err := netlink.LinkUp(hostVeth); err != nil {
		return removeOnErr("上线网桥侧 veth", err)
	}
	if err := netlink.SetLinkNetnsPid(peerVeth, childPID); err != nil {
		return removeOnErr("移动容器侧 veth 进容器 netns", err)
	}
	return nil
}

// BringUpLoopback 拉起当前 network namespace 的回环接口。
//
// 内核新建 netns 时 lo 处于 **DOWN**：容器内进程发往 127.0.0.1 的流量会一直
// 等到超时（而不是立刻 ECONNREFUSED），极易被误判成"应用没监听"。连我们
// 自己写进 /etc/hosts 的 `127.0.0.1 localhost` 都因此不可用。runc 与 Docker
// 都会在容器启动时拉起 lo，此处对齐。
//
// 必须在容器**自己的** netns 内调用（runtime.RunInit 在 pivot_root 之后、
// execve 之前）。bridge 与 none 模式都需要——两者都会新建 netns；host 模式
// 共享宿主 netns，不得调用。
func BringUpLoopback() error {
	if err := netlink.LinkUp("lo"); err != nil {
		return fmt.Errorf("上线容器回环 lo: %w", err)
	}
	return nil
}

// ConfigurePeer 在容器 netns（内部由 runtime.RunInit 在容器进程里调用）配置
// veth 的容器侧：上线、绑定 IP、默认路由，并写容器内 resolv.conf 与 hosts
// 使容器 DNS 指向桥接网关（内置 DNS）。返回任何错误由 init 上报给父进程。
//
// 回环接口不在此处理：lo 与 veth 无关，且 --network none 同样需要，
// 故由调用方（runtime.RunInit）统一调用 BringUpLoopback。
func ConfigurePeer(netName, containerID, ip, gateway string, prefix int, hostname string) error {
	_, peerVeth := vethPair(netName, containerID)

	// 父进程移动 peer 进 netns 与子进程配置存在竞态：短促重试直到接口出现。
	// （历史 bug：LinkByName 对 NUL 结尾的接口名匹配失败，导致这里永远"
	// 未就绪"；已修复。）
	const (
		attempts      = 20
		retryInterval = 100 * time.Millisecond
	)
	found := false
	for i := 0; i < attempts; i++ {
		if _, err := netlink.LinkByName(peerVeth); err == nil {
			found = true
			break
		}
		time.Sleep(retryInterval)
	}
	if !found {
		return fmt.Errorf("容器侧 veth %s 未就绪（%d 次重试后）: %w", peerVeth, attempts, netlink.ErrLinkNotFound)
	}

	if err := netlink.LinkUp(peerVeth); err != nil {
		return fmt.Errorf("上线容器侧 veth %s: %w", peerVeth, err)
	}
	if err := netlink.AddAddr(peerVeth, ip, prefix); err != nil {
		return fmt.Errorf("为 %s 绑定 IP %s/%d: %w", peerVeth, ip, prefix, err)
	}
	if err := netlink.AddRoute("", 0, gateway, peerVeth); err != nil {
		// 无默认路由会阻隔出口 NAT 之外的流量，属核心配置，失败即报错。
		return fmt.Errorf("添加默认路由 via %s: %w", gateway, err)
	}

	if err := writeDNSFiles(ip, gateway, hostname); err != nil {
		return err
	}
	return nil
}

// writeDNSFiles 在容器根（/）写 /etc/resolv.conf 与 /etc/hosts。
func writeDNSFiles(ip, gateway, hostname string) error {
	return writeDNSFilesTo("/", ip, gateway, hostname)
}

// writeDNSFilesTo 在给定 root 下写 etc/resolv.conf 与 etc/hosts（root=容器新根）。
// root 拆出便于单测在临时目录验证、避免碰宿主 /etc。
//
// **DNS 来源**：优先用 `LICORE_NET_DNS` 环境变量——它由**宿主侧**
// （engine/shim）在启动容器前探测并注入。
//
// 为什么不在容器内自己探测：本函数运行在容器 init 进程里（已 pivot_root），
// `/run/systemd/resolve/resolv.conf` 与 `/etc/resolv.conf` 看到的都是**容器的**
// 文件系统——前者不存在、后者是容器自己的。因此容器内探测**拿不到宿主上游**，
// 只能走公共 DNS 兜底（实测确认：容器内读到的 /etc/resolv.conf 已是本函数
// 自己写的内容，形成循环）。
//
// 环境变量缺失时（老版本 engine、单测直调）回退到容器内探测 → 公共 DNS，
// 保证容器至少有解析器，不会出现"完全无法解析域名"。
func writeDNSFilesTo(root, ip, gateway, hostname string) error {
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		return fmt.Errorf("创建容器 %s: %w", etc, err)
	}
	// resolv.conf：写入宿主侧探测到的上游 DNS。
	//
	// 不再写网桥网关：网关上没有 DNS 服务，写了等于让容器所有域名解析超时
	// （真机实测 nslookup 报 connection timed out）。
	nameservers := ParseNameserverList(os.Getenv(EnvNetDNS))
	if len(nameservers) == 0 {
		// 没有注入时退化为容器内探测（会走公共 DNS 兜底）。
		nameservers = resolveNameservers()
	}
	if err := os.WriteFile(filepath.Join(etc, "resolv.conf"),
		[]byte(formatResolvConf(nameservers)), 0o644); err != nil {
		return fmt.Errorf("写 resolv.conf: %w", err)
	}
	// hosts：追加本机名与网关映射（保留既有 loopback 行，不覆盖）。
	var b strings.Builder
	b.WriteString("127.0.0.1 localhost\n")
	if hostname != "" {
		fmt.Fprintf(&b, "%s %s\n", ip, hostname)
	}
	fmt.Fprintf(&b, "%s licore-gw\n", gateway)
	if err := os.WriteFile(filepath.Join(etc, "hosts"), []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("写 hosts: %w", err)
	}
	return nil
}
