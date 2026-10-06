// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PresetBridgeName 是内置预置网桥名。
const PresetBridgeName = "licore0"

// PresetBridgeSubnet / PresetBridgeGateway 是 licore0 的预置网段。
const (
	PresetBridgeSubnet  = "172.18.0.0/16"
	PresetBridgeGateway = "172.18.0.1"
)

// PresetBridgeHostInterface 是预置网桥在宿主上的接口名（与网络名一致）。
const PresetBridgeHostInterface = "licore0"

// Manager 提供网络定义的增删改查与端点（容器）接入管理，并把定义持久化在
// <root>/networks/ 下。<root> 复用 LiCore 数据目录（~/.licore 等），网络文件
// 属于新增子路径，不改动既有磁盘布局。
type Manager struct {
	root string // 数据目录根
}

// NewManager 返回网络管理器。Root 为空则沿用 store.Open 的数据目录逻辑，
// 以便自定义 --data-dir 时网络与容器落在同一根下。
func NewManager(dataRoot string) (*Manager, error) {
	root := dataRoot
	if root == "" {
		r, err := defaultStoreRoot()
		if err != nil {
			return nil, err
		}
		root = r
	}
	return &Manager{root: root}, nil
}

// Root 返回管理器的数据根目录。
func (m *Manager) Root() string { return m.root }

// networksRoot 返回网络定义目录 <root>/networks。
func (m *Manager) networksRoot() string { return filepath.Join(m.root, "networks") }

// List 返回全部网络（含自动补建的预置 licore0），按名字排序。
func (m *Manager) List() ([]*Network, error) {
	if err := m.ensurePreset(); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(m.networksRoot())
	if errors.Is(err, os.ErrNotExist) {
		return []*Network{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("扫描网络目录失败: %w", err)
	}
	out := []*Network{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		n, err := m.Load(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			slog.Warn("跳过损坏的网络定义", "file", e.Name(), "err", err)
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Load 按名字读取网络定义；不存在返回 ErrNetworkNotFound。
func (m *Manager) Load(name string) (*Network, error) {
	data, err := os.ReadFile(m.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("网络 %q: %w", name, ErrNetworkNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("读取网络定义失败: %w", err)
	}
	var n Network
	if err := json.Unmarshal(data, &n); err != nil {
		return nil, fmt.Errorf("解析网络 %s 失败: %w", name, err)
	}
	if n.Name == "" || !n.Driver.Valid() {
		return nil, fmt.Errorf("网络 %s 定义损坏: %w", name, ErrBadNetwork)
	}
	return &n, nil
}

// Save 原子写网络定义。
func (m *Manager) Save(n *Network) error {
	if n.Name == "" || strings.ContainsAny(n.Name, `/\`) || strings.HasPrefix(n.Name, ".") {
		return fmt.Errorf("网络名 %q 非法: %w", n.Name, ErrBadNetwork)
	}
	if !n.Driver.Valid() {
		return fmt.Errorf("驱动 %q 非法: %w", n.Driver, ErrBadNetwork)
	}
	dir := m.networksRoot()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建网络目录失败: %w", err)
	}
	data, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化网络定义失败: %w", err)
	}
	tmp := filepath.Join(dir, ".net.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写网络定义失败: %w", err)
	}
	if err := os.Rename(tmp, m.path(n.Name)); err != nil {
		return fmt.Errorf("落位网络定义失败: %w", err)
	}
	return nil
}

// Remove 删除一个网络。预置网络与仍被容器使用的网络拒绝删除。
func (m *Manager) Remove(name string) error {
	n, err := m.Load(name)
	if err != nil {
		return err
	}
	if n.IsPreset() {
		return fmt.Errorf("预置网络 %s 不能删除: %w", name, ErrBadNetwork)
	}
	if len(n.Endpoints) > 0 {
		return fmt.Errorf("网络 %s 仍有 %d 个容器接入，请先断开: %w", name, len(n.Endpoints), ErrEndpointExists)
	}
	// 先清理宿主侧接口。
	if err := driverTeardown(n); err != nil {
		slog.Warn("清理网络接口失败（非致命）", "net", name, "err", err)
	}
	if err := os.Remove(m.path(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除网络定义失败: %w", err)
	}
	return nil
}

// path 返回某网络的持久化路径。
func (m *Manager) path(name string) string { return filepath.Join(m.networksRoot(), name+".json") }

// ensurePreset 无条件确保预置 licore0 存在（不存在则创建）。
//
// **关键：如果宿主上已经有同名网桥，必须采用它真实的网段，而不是另分配一个。**
//
// 曾经的写法是无条件 `pickFreeSubnet`。而 pickFreeSubnet 会**跳过
// licore0 自己的路由**（见 subnetRoutedByOther：`iface == PresetBridgeName
// … continue`，本意是"不要被自己绊倒"），于是它看不见既有网桥占的
// 172.22.0.0/16，很自然地选出 172.21.0.0/16：
//
//	宿主网桥实际  : 172.22.0.1/16   （还在，继续服务老容器）
//	新数据目录定义: 172.21.0.1/16   （凭空分配，网桥上根本没有这个地址）
//
// 之后 driverBootstrap 复用同名网桥 → 容器默认路由指向不存在的
// 172.21.0.1 → 容器 Up 但完全没网（L-6）。
//
// 正确的语义是"**既有的网桥是事实来源**"：它上面配了什么，本数据目录就
// 该记什么。这样多数据目录共用同一宿主时彼此一致，也不会与既有容器打架。
// 只有当宿主上还没有网桥时，才需要本数据目录自己挑一个空闲网段。
func (m *Manager) ensurePreset() error {
	if _, err := os.Stat(m.path(PresetBridgeName)); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查预置网络失败: %w", err)
	}
	n := New(PresetBridgeName, DriverBridge)

	if subnet, gateway, ok := existingBridgeSubnet(PresetBridgeName); ok {
		// 复用既有网桥的网段。
		n.Subnet, n.Gateway = subnet, gateway
		slog.Info("复用宿主上既有网桥的网段",
			"net", PresetBridgeName, "subnet", subnet, "gateway", gateway)
	} else {
		subnet, gateway := pickFreeSubnet(PresetBridgeSubnet)
		n.Subnet, n.Gateway = subnet, gateway
	}

	if err := m.Save(n); err != nil {
		return err
	}
	// 尝试创建宿主网桥；失败不致命（容器管理仍可用）。
	// 注意：复用既有网桥时上面的网段已与它一致，这里不会再报不匹配。
	if err := driverBootstrap(n); err != nil {
		slog.Warn("预置网桥创建失败，请以 root 运行", "net", PresetBridgeName, "err", err)
	}
	return nil
}

// pickFreeSubnet 返回一个不与宿主既有路由冲突的 /16 网段与网关（.1）。
// prefer 优先（如 172.18.0.0/16）；若已被其他接口占用（如 Docker 的 br-*
// 也用 172.18.0.0/16），则顺延从 172.20.0.0/16..172.31.0.0/16 选首个空闲。
// 冲突症状是"licore 容器能 Up，但 host→容器路由指向别的网桥、curl 空"。
func pickFreeSubnet(prefer string) (string, string) {
	if prefer != "" && !subnetRoutedByOther(prefer) {
		_, ipnet, _ := net.ParseCIDR(prefer)
		if ipnet != nil {
			gw := netIP4Inc(ipnet.IP.To4())
			return prefer, gw
		}
	}
	for c := 20; c <= 31; c++ {
		sub := fmt.Sprintf("172.%d.0.0/16", c)
		if subnetRoutedByOther(sub) {
			continue
		}
		_, ipnet, _ := net.ParseCIDR(sub)
		if ipnet == nil {
			continue
		}
		return sub, netIP4Inc(ipnet.IP.To4())
	}
	// 都给占用了就用 192.168.99.0/24。
	return "192.168.99.0/24", "192.168.99.1"
}

// subnetRoutedByOther 报告 net 是否被宿主某非 licore 接口的路由覆盖。
func subnetRoutedByOther(subnet string) bool {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return true // 无法解析视为占用，避免误用
	}
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		iface, dstHex, maskHex := f[0], f[1], f[7]
		if iface == PresetBridgeName || iface == "lo" {
			continue
		}
		dst := hexToIP32(dstHex)
		mask := hexToIP32(maskHex)
		if dst == 0 && mask == 0 {
			continue // 默认路由（0.0.0.0/0）不算冲突
		}
		// 判断该路由网段与目标网段是否重叠。
		netU := ipToU32(ipnet.IP.To4())
		netMask := ipToU32(net.IP(ipnet.Mask).To4())
		if (dst&netMask) == (netU&netMask) && mask&netMask != 0 {
			return true
		}
	}
	return false
}

func hexToIP32(hex string) uint32 {
	if len(hex) != 8 {
		return 0
	}
	// /proc/net/route 每字节一对 hex、小端存储：字符串末两字符是 IP 最高位字节。
	// "0001A8C0" → IP 字节 [C0 A8 01 00] → 值 0xC0A80100。
	nb := func(b byte) uint32 {
		if b >= '0' && b <= '9' {
			return uint32(b - '0')
		}
		if b >= 'A' && b <= 'F' {
			return uint32(b-'A') + 10
		}
		if b >= 'a' && b <= 'f' {
			return uint32(b-'a') + 10
		}
		return 0
	}
	var v uint32
	// byte0（最高位字节）= 字符串 char6,char7；byte1=char4,5；byte2=char2,3；byte3=char0,1。
	for b := 0; b < 4; b++ {
		hi := nb(hex[6-2*b])
		lo := nb(hex[7-2*b])
		v = v<<8 | hi<<4 | lo
	}
	return v
}

func ipToU32(ip []byte) uint32 {
	if len(ip) < 4 {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func netIP4Inc(ip []byte) string {
	if len(ip) < 4 {
		return ""
	}
	// 网关 = .1。
	return fmt.Sprintf("%d.%d.%d.%d", ip[0], ip[1], ip[2], 1)
}

// Create 创建新网络并做宿主侧实化（bridge 建网桥、内存回环等）。
func (m *Manager) Create(name string, d Driver, subnet, gateway string) (*Network, error) {
	validName := func(s string) bool {
		return s != "" && !strings.ContainsAny(s, `/\`) && !strings.HasPrefix(s, ".")
	}
	if !validName(name) {
		return nil, fmt.Errorf("网络名 %q 非法: %w", name, ErrBadNetwork)
	}
	if _, err := m.Load(name); err == nil {
		return nil, fmt.Errorf("网络 %q 已存在: %w", name, ErrNetworkExists)
	}
	n := New(name, d)
	if d == DriverBridge {
		if err := validateSubnet(subnet, gateway); err != nil {
			return nil, err
		}
		n.Subnet = subnet
		n.Gateway = gateway
	}
	if err := m.Save(n); err != nil {
		return nil, err
	}
	// 宿主侧实化独立于定义落盘：无 root 时网络定义仍可管理（bridge 可后续
	// 由 licore network create 以 root 重建）。失败仅告警，不阻断数据层。
	if err := driverBootstrap(n); err != nil {
		slog.Warn("实化网络接口失败，请以 root 运行或在下次创建时重试", "net", name, "err", err)
	}
	return n, nil
}

// Inspect 返回网络定义（供 inspect 命令与 JSON 输出）。
func (m *Manager) Inspect(name string) (*Network, error) { return m.Load(name) }

// Connect 把容器接入网络：bridge 下分配 IP 并实化 veth + 挂桥。
func (m *Manager) Connect(netName, containerID, containerName, wantIP string) (*Endpoint, error) {
	n, err := m.Load(netName)
	if err != nil {
		return nil, err
	}
	for _, e := range n.Endpoints {
		if e.ContainerID == containerID {
			return nil, fmt.Errorf("容器 %s 已接入网络 %s: %w", containerID, netName, ErrEndpointExists)
		}
	}
	ep := &Endpoint{ContainerID: containerID, ContainerName: containerName}
	switch n.Driver {
	case DriverBridge:
		ip, err := n.allocateIP(wantIP)
		if err != nil {
			return nil, err
		}
		ep.IP = ip
		if err := bridgeAttachEndpoints(n); err != nil {
			return nil, fmt.Errorf("实化容器 %s 到网络 %s: %w", containerID, netName, err)
		}
	case DriverHost, DriverNone:
		// host/none 无自有 IP 分配。
	}
	n.Endpoints = append(n.Endpoints, ep)
	if err := m.Save(n); err != nil {
		return nil, err
	}
	return ep, nil
}

// Disconnect 把容器从网络断开（bridge 释放 IP、清理 veth）。
func (m *Manager) Disconnect(netName, containerID string) error {
	n, err := m.Load(netName)
	if err != nil {
		return err
	}
	for i, e := range n.Endpoints {
		if e.ContainerID == containerID {
			if n.Driver == DriverBridge {
				_ = bridgeDetachEndpoint(epVethName(n.Name, e.ContainerID))
				_ = bridgeDetachEndpoint(epPeerName(n.Name, e.ContainerID))
			}
			n.Endpoints = append(n.Endpoints[:i], n.Endpoints[i+1:]...)
			return m.Save(n)
		}
	}
	return fmt.Errorf("容器 %s 未接入网络 %s: %w", containerID, netName, ErrEndpointNotFound)
}

// PruneEndpoints 移除网络里容器已不存在（alive 谓词返回 false）的端点，
// 并清掉其 veth/NAT，避免历史残留端点继续被 DNAT、导致指向死 IP 的连接被
// 丢弃（表现为 curl 空）。供 engine 在接入新容器前调用。
func (m *Manager) PruneEndpoints(netName string, alive func(containerID string) bool) error {
	if alive == nil {
		return nil
	}
	n, err := m.Load(netName)
	if err != nil {
		return err
	}
	kept := n.Endpoints[:0]
	changed := false
	for _, e := range n.Endpoints {
		if alive(e.ContainerID) {
			kept = append(kept, e)
			continue
		}
		if n.Driver == DriverBridge {
			_ = bridgeDetachEndpoint(epVethName(n.Name, e.ContainerID))
			_ = bridgeDetachEndpoint(epPeerName(n.Name, e.ContainerID))
		}
		changed = true
	}
	n.Endpoints = kept
	if changed {
		return m.Save(n)
	}
	return nil
}

// AllocatePorts 在 bridge 网络上为容器登记一组端口映射（纯数据；NAT 实化
// 通过 ApplyNAT 单独执行，以便无 root 时仍可管理定义）。
func (m *Manager) AllocatePorts(netName, containerID string, ports []*PortMapping) error {
	n, err := m.Load(netName)
	if err != nil {
		return err
	}
	if n.Driver != DriverBridge {
		return fmt.Errorf("网络 %s 驱动 %q 不支持端口映射（仅 bridge）: %w", netName, n.Driver, ErrBadPortMapping)
	}
	var ep *Endpoint
	for _, e := range n.Endpoints {
		if e.ContainerID == containerID {
			ep = e
			break
		}
	}
	if ep == nil {
		return fmt.Errorf("容器 %s 未接入网络 %s: %w", containerID, netName, ErrEndpointNotFound)
	}
	for _, p := range ports {
		if err := p.Validate(); err != nil {
			return err
		}
	}
	ep.Ports = ports
	return m.Save(n)
}

// ReleasePorts 移除容器所有端口映射规则。
func (m *Manager) ReleasePorts(netName, containerID string) error {
	n, err := m.Load(netName)
	if err != nil {
		return err
	}
	found := false
	for _, e := range n.Endpoints {
		if e.ContainerID == containerID {
			e.Ports = nil
			found = true
		}
	}
	if !found {
		return nil
	}
	return m.Save(n)
}

// ApplyNAT 把网络的全部端口映射与出口 NAT 实化到防火墙。无可用工具或非
// root 时返回相应错误，供 `licore run` 路径显式处理（而非静默失败）。
//
// 后端选择：nft 优先，失败回退 iptables（详见 applyPortRules）。实际用了
// 哪个后端会**落盘到 state.json**——清理路径（stop/rm/Disconnect）都是重新
// `Load` 出来的新对象，不落盘就拿不到后端类型，iptables 规则将永远残留。
func (m *Manager) ApplyNAT(netName string) error {
	n, err := m.Load(netName)
	if err != nil {
		return err
	}
	if err := n.applyPortRules(); err != nil {
		return err
	}
	// 落盘后端选择。Save 失败只告警：NAT 此刻已经生效，为此返回错误会让
	// 用户以为网络没配好。实际影响仅是下次清理可能用错后端，属次要问题。
	if err := m.Save(n); err != nil {
		slog.Warn("记录 NAT 后端失败，清理时可能残留规则",
			slog.String("net", netName), slog.Any("err", err))
	}
	return nil
}

// ClearNAT 清空网络的全部 NAT 规则。
func (m *Manager) ClearNAT(netName string) error {
	n, err := m.Load(netName)
	if err != nil {
		return err
	}
	return n.removePortRules("")
}

// EnsurePreset 无条件确保预置 licore0 网络定义与宿主网桥存在（幂等）。
func (m *Manager) EnsurePreset() error {
	return m.ensurePreset()
}

// EnsureDriver 保证网络对应的宿主侧拓扑存在（bridge 建立网桥接口）。
// 容器启动前调用，让 runtime 装配 veth 时网桥已就绪。
func (m *Manager) EnsureDriver(name string) error {
	n, err := m.Load(name)
	if err != nil {
		return err
	}
	return driverBootstrap(n)
}

// ClientNet 描述某个容器接入一个网络所需的运行时装配置（供 engine/shim
// 在启动前求出，再经环境变量注入 init）。仅 bridge 有意义。
type ClientNet struct {
	// Name 是网络名。
	Name string
	// IP 是分配给该容器的地址。
	IP string
	// Gateway 是网桥网关。
	Gateway string
	// Prefix 是子网掩码长度。
	Prefix int
}

// ClientNetConfig 返回容器 A 接入网络所需的 bridge 装配参数。
// 容器必须已通过 Connect 接入（否则返回 ErrEndpointNotFound）。
func (m *Manager) ClientNetConfig(netName, containerID string) (*ClientNet, error) {
	n, err := m.Load(netName)
	if err != nil {
		return nil, err
	}
	if n.Driver != DriverBridge {
		return nil, fmt.Errorf("网络 %s 驱动 %q 非桥接，无 veth 装配: %w", netName, n.Driver, ErrBadNetwork)
	}
	for _, e := range n.Endpoints {
		if e.ContainerID == containerID {
			if e.IP == "" {
				return nil, fmt.Errorf("容器 %s 在网络 %s 上未分到 IP: %w", containerID, netName, ErrBadNetwork)
			}
			_, ipnet, err := net.ParseCIDR(n.Subnet)
			if err != nil {
				return nil, fmt.Errorf("网络 %s 子网非法: %w", netName, ErrBadNetwork)
			}
			ones, _ := ipnet.Mask.Size()
			return &ClientNet{Name: netName, IP: e.IP, Gateway: n.Gateway, Prefix: ones}, nil
		}
	}
	return nil, fmt.Errorf("容器 %s 未接入网络 %s: %w", containerID, netName, ErrEndpointNotFound)
}

// validateSubnet 校验 bridge 网段与网关。
func validateSubnet(subnet, gateway string) error {
	if subnet == "" {
		return fmt.Errorf("bridge 网络必须提供 --subnet: %w", ErrBadNetwork)
	}
	if gateway == "" {
		return fmt.Errorf("bridge 网络必须提供 --gateway: %w", ErrBadNetwork)
	}
	if _, _, err := ParseCIDR(subnet); err != nil {
		return fmt.Errorf("非法 --subnet %q: %w", subnet, ErrBadNetwork)
	}
	if netIP4Bytes(gateway) == nil {
		return fmt.Errorf("非法 --gateway %q: %w", gateway, ErrBadNetwork)
	}
	return nil
}

var _ = time.Now // 保留 import

func (p *PortMapping) Validate() error {
	if p.ContainerPort <= 0 || p.ContainerPort > 65535 {
		return fmt.Errorf("容器端口 %d 非法: %w", p.ContainerPort, ErrBadPortMapping)
	}
	if p.HostPort < 0 || p.HostPort > 65535 {
		return fmt.Errorf("宿主端口 %d 非法: %w", p.HostPort, ErrBadPortMapping)
	}
	if p.Proto == "" {
		p.Proto = ProtoTCP
	}
	if p.Proto != ProtoTCP && p.Proto != ProtoUDP {
		return fmt.Errorf("协议 %q 非法: %w", p.Proto, ErrBadPortMapping)
	}
	return nil
}
