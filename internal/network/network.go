// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

// Package network 实现 LiCore 自研容器网络：容器间通信与对外 NAT 出口，
// 以及 bridge / host / none 三种网络模式的创建、管理与端口映射。
// 阶段 3 落地于本包；非 Linux 平台由 *_nonlinux.go 提供占位（返回
// ErrUnsupported），保证全仓库可交叉编译。
package network

import (
	"fmt"
	"time"
)

// Driver 是网络驱动类型。
type Driver string

// 内置驱动。
const (
	DriverBridge Driver = "bridge" // 自建 veth + 网桥 + nftables NAT
	DriverHost   Driver = "host"   // 直接复用宿主网络栈
	DriverNone   Driver = "none"   // 完全隔离，无网络
)

// Valid 报告驱动值是否合法。
func (d Driver) Valid() bool {
	switch d {
	case DriverBridge, DriverHost, DriverNone:
		return true
	}
	return false
}

// NetMode 是容器启动时接入网络的方式（运行时装配用）。
type NetMode string

// 网络接入模式。
const (
	ModeBridge NetMode = "bridge"
	ModeHost   NetMode = "host"
	ModeNone   NetMode = "none"
)

// ParseNetMode 把网络名/模式串规整为接入模式：""|licore0|bridge → bridge；
// host → host（复用宿主网络栈）；none → none。其余名字视为自定义桥接网络。
func ParseNetMode(n string) NetMode {
	switch n {
	case "", "licore0", "bridge":
		return ModeBridge
	case "host":
		return ModeHost
	case "none":
		return ModeNone
	default:
		return ModeBridge // 自定义网络名一律走桥接 veth。
	}
}

// Proto 是端口映射协议。
type Proto string

const (
	ProtoTCP Proto = "tcp"
	ProtoUDP Proto = "udp"
)

// PortMapping 是一条 -p 端口映射。
type PortMapping struct {
	// HostIP 是宿主机监听地址（默认 0.0.0.0）。
	HostIP string `json:"hostIP,omitempty"`
	// HostPort 是宿主机端口（0 表示随机）。
	HostPort int `json:"hostPort"`
	// ContainerPort 是容器内端口。
	ContainerPort int `json:"containerPort"`
	// Proto 是协议 tcp/udp。
	Proto Proto `json:"proto,omitempty"`
}

// Endpoint 是容器接入网络的一个端点。
type Endpoint struct {
	// ContainerID 是容器 ID。
	ContainerID string `json:"containerId"`
	// ContainerName 是容器名（用于内置 DNS 解析）。
	ContainerName string `json:"containerName"`
	// IP 是分配给该容器的地址。
	IP string `json:"ip"`
	// Ports 是该容器在本网络上的端口映射（仅 bridge）。
	Ports []*PortMapping `json:"ports,omitempty"`
}

// Network 是一个已定义网络的完整描述（持久化于 <root>/networks/<name>.json）。
type Network struct {
	// Name 是网络名。
	Name string `json:"name"`
	// Driver 是驱动类型。
	Driver Driver `json:"driver"`
	// Subnet 是网段 CIDR（如 172.18.0.0/16），仅 bridge 有意义。
	Subnet string `json:"subnet,omitempty"`
	// Gateway 是网关地址（如 172.18.0.1），仅 bridge。
	Gateway string `json:"gateway,omitempty"`
	// Internal 表示不提供对外 NAT 出口。
	Internal bool `json:"internal,omitempty"`
	// CreatedAt 是创建时间（UTC RFC 3339）。
	CreatedAt string `json:"createdAt"`
	// Endpoints 是当前接入的容器端点。
	Endpoints []*Endpoint `json:"endpoints,omitempty"`

	// NATBackend 记录 NAT 规则写进了哪个后端（"nft" / "iptables"），
	// 供清理时用**同一个**后端移除规则——两个后端的规则互不相通，
	// 用错后端会留下残余规则（真机验证专门检查了这一点）。
	//
	// **必须持久化**：清理路径（`licore stop` / `rm` / Disconnect）都经
	// `Load` 从 state.json 重新读出网络定义，内存里的字段活不到那一刻。
	// 若不落盘，iptables 回退写下的规则永远不会被清掉。
	//
	// 空值（旧版本写的 state.json）按 nft 处理——那是 v0.9.3 之前唯一的
	// 写入位置。omitempty 保证旧版本读新配置不失败。
	NATBackend string `json:"natBackend,omitempty"`
}

// natBackendKind 标识 NAT 规则写在哪个后端。
type natBackendKind int

const (
	// natBackendNone 表示尚未应用过 NAT（旧 state.json 的默认值）。
	natBackendNone natBackendKind = iota
	// natBackendNft 表示规则写在 nft（`ip licore` 表）。
	natBackendNft
	// natBackendIptables 表示规则写在 iptables（`LICORE` 自定义链）。
	natBackendIptables
)

// String 便于日志与测试断言。
func (k natBackendKind) String() string {
	switch k {
	case natBackendNft:
		return "nft"
	case natBackendIptables:
		return "iptables"
	default:
		return "none"
	}
}

// natBackendFromString 由持久化字符串还原后端类型。
func natBackendFromString(s string) natBackendKind {
	switch s {
	case "nft":
		return natBackendNft
	case "iptables":
		return natBackendIptables
	default:
		return natBackendNone
	}
}

// backend 返回本次应使用的 NAT 后端（由持久化字段还原）。
func (n *Network) backend() natBackendKind {
	return natBackendFromString(n.NATBackend)
}

// New 构造一个未持久化的网络定义。
func New(name string, d Driver) *Network {
	return &Network{Name: name, Driver: d, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
}

// IsPreset 报告该网络是否为内置预置网络（licore0）。
func (n *Network) IsPreset() bool { return n.Name == PresetBridgeName }

// String 面向用户的可读摘要。
func (n *Network) String() string {
	return fmt.Sprintf("%s\t%s\t%s", n.Name, n.Driver, n.Subnet)
}

// ErrDataDir 表示无法确定数据目录（Manager 构造失败）。
var ErrDataDir = fmt.Errorf("licore/network: 无法确定数据目录")
