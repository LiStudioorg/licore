// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/LiStudioorg/licore/internal/network"
	"github.com/LiStudioorg/licore/internal/runtime"
	"github.com/LiStudioorg/licore/internal/store"
)

// parsePorts 把 run -p 的原始串解析为端口映射列表。支持
// "8080:80"、"8080:80/udp"、"80"（随机宿主端口）、"127.0.0.1:8080:80"。
func parsePorts(raw []string) ([]*network.PortMapping, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]*network.PortMapping, 0, len(raw))
	for _, s := range raw {
		p, err := parsePort(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// parsePort 解析单条端口映射。
func parsePort(raw string) (*network.PortMapping, error) {
	proto := network.ProtoTCP
	if i := strings.IndexByte(raw, '/'); i >= 0 {
		switch raw[i+1:] {
		case "tcp":
			proto = network.ProtoTCP
		case "udp":
			proto = network.ProtoUDP
		default:
			return nil, fmt.Errorf("run: 非法协议 %q", raw[i+1:])
		}
		raw = raw[:i]
	}
	parts := strings.Split(raw, ":")
	p := &network.PortMapping{Proto: proto}
	switch len(parts) {
	case 1:
		p.ContainerPort = atoi(parts[0])
		p.HostPort = 0
	case 2:
		p.HostPort = atoi(parts[0])
		p.ContainerPort = atoi(parts[1])
	case 3:
		p.HostIP = parts[0]
		p.HostPort = atoi(parts[1])
		p.ContainerPort = atoi(parts[2])
	default:
		return nil, fmt.Errorf("run: 非法端口映射 %q", raw)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	return p, nil
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// wireNetworkBeforeStart 在容器真正启动前接入网络：为容器分配 IP、把端口
// 映射登记到端点并实化 NAT（DNAT 绑定到容器 IP）。成功后把 Network/IP
// 回写进 config.json，供 detach 路径的 shim 感知网络。失败时回滚端点并
// 返回错误，调用方负责清理容器目录。
func wireNetworkBeforeStart(st *store.Store, cfg *store.ContainerConfig, netName, wantIP string, ports []*network.PortMapping) error {
	mode := network.ParseNetMode(netName)
	// host/none：不实化 veth，仅记录模式。host 复用宿主栈，none 建空 netns。
	if mode != network.ModeBridge {
		cfg.Network = string(mode)
		cfg.IP = ""
		if err := st.WriteContainerConfig(cfg); err != nil {
			return err
		}
		if len(ports) > 0 {
			return fmt.Errorf("网络 %s 不支持端口映射（-p 仅 bridge 网络可用）", netName)
		}
		return nil
	}
	if netName == "" {
		netName = network.PresetBridgeName
	}
	m, err := network.NewManager(st.Root)
	if err != nil {
		return err
	}
	// 目标网络必须存在；缺省 licore0 未定义时自动补建。
	if netName == network.PresetBridgeName {
		if err := m.EnsurePreset(); err != nil {
			return fmt.Errorf("初始化预置网络 %s 失败: %w", netName, err)
		}
	}
	if _, err := m.Load(netName); err != nil {
		return fmt.Errorf("接入网络 %s 失败: %w", netName, err)
	}
	if err := m.EnsureDriver(netName); err != nil {
		slog.Warn("网络网桥未就绪（可能无 root），veth 装配可能失败", "net", netName, "err", err)
	}
	// 清理历史残留端点（容器已不存在的端点仍会被 ApplyNAT 登记 DNAT，指到死
	// IP 会挡掉真实容器的流量），避免 curl 空。
	_ = m.PruneEndpoints(netName, func(cid string) bool {
		_, err := st.LoadContainer(cid)
		return err == nil
	})
	// 分配 IP 并登记为该网络端点。
	ep, err := m.Connect(netName, cfg.ID, cfg.Name, wantIP)
	if err != nil {
		return fmt.Errorf("接入网络 %s: %w", netName, err)
	}
	rollback := func() { _ = m.Disconnect(netName, cfg.ID) }
	// 登记端口映射（仅 -p 时才有）与实化 NAT。
	//
	// **ApplyNAT 必须无条件调用，不能只在 len(ports) > 0 时调**：
	// applyPortRules 除了 DNAT（端口映射）还负责**出口 MASQUERADE**，
	// 而出口 NAT 是所有 bridge 容器都需要的——不带 -p 的容器同样要能访问外网
	// （与 Docker 语义一致）。原先写在 if len(ports) > 0 里面，导致不带 -p 的
	// 容器根本没有 masquerade 规则、**完全无法出网**（真机实测确认）。
	//
	// **失败必须回滚并冒泡**，不能 slog.Warn 后继续：NAT 写不进去意味着
	// 容器连不上外网（带 -p 时还包括端口映射不生效）。让容器"起来了但没网"
	// 是比启动失败更难排查的意外行为——用户会去查容器内部，而问题在网络层。
	// 宁可起不来，也不要静默降级（AGENTS.md：禁止参数接受但运行时假装生效）。
	if len(ports) > 0 {
		if err := m.AllocatePorts(netName, cfg.ID, ports); err != nil {
			rollback()
			return err
		}
	}
	if err := m.ApplyNAT(netName); err != nil {
		// 权限不足：非 root 时容器本来就起不来（网桥 / veth 同样建不了），
		// 保持降级告警即可——这里再报错只会掩盖更根本的权限问题。
		if errors.Is(err, network.ErrNotRoot) {
			slog.Warn("网络 NAT 未应用（权限不足，容器可能无外网）",
				"net", netName, "err", err)
		} else {
			// 其余失败是真正的配置错误（如本机 nft 不支持 masquerade）：
			// 必须回滚并冒泡。让容器"起来了但连不上网"比启动失败难排查得多。
			rollback()
			return fmt.Errorf("容器网络 NAT 配置失败（已回滚，容器未创建）: %w", err)
		}
	}
	cfg.Network = netName
	cfg.IP = ep.IP
	if err := st.WriteContainerConfig(cfg); err != nil {
		rollback()
		return err
	}
	return nil
}

// disconnectContainer 移除容器网络端点与 NAT（stop/rm 收敛路径）。
func disconnectContainer(st *store.Store, cfg *store.ContainerConfig) {
	if cfg == nil || cfg.Network == "" {
		return
	}
	if network.ParseNetMode(cfg.Network) != network.ModeBridge {
		return
	}
	m, err := network.NewManager(st.Root)
	if err != nil {
		return
	}
	_ = m.ReleasePorts(cfg.Network, cfg.ID)
	_ = m.ClearNAT(cfg.Network)
	_ = m.Disconnect(cfg.Network, cfg.ID)
}

// netEnvFor 为运行时装配备齐网络环境变量（追加到 runtime.Config.Env）。
// 委托 runtime.ResolveNetEnv 按容器已登记的网络档案解析。
func netEnvFor(st *store.Store, cfg *store.ContainerConfig, hostname string) []string {
	if cfg == nil {
		return nil
	}
	return runtime.ResolveNetEnv(st.Root, cfg.Network, cfg.ID, hostname)
}
