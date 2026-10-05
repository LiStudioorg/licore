// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"log/slog"
	"strconv"
	"strings"

	"github.com/LiStudioorg/licore/internal/network"
)

// 网络装配通过内部环境变量从父进程（engine/shim）传给容器 init。
// envWithoutLiCore 统一剥离 LICORE_* 前缀，故这些键不会泄漏进用户命令行。
const (
	envNetMode    = "LICORE_NET_MODE"    // bridge|host|none
	envNetName    = "LICORE_NET_NAME"    // 网络名
	envNetIP      = "LICORE_NET_IP"      // 容器 IP
	envNetGateway = "LICORE_NET_GATEWAY" // 桥接网关
	envNetPrefix  = "LICORE_NET_PREFIX"  // 子网前缀长度
	envNetHost    = "LICORE_NET_HOST"    // 容器名（写入 /etc/hosts）
	envNetCID     = "LICORE_NET_CID"     // 容器 ID（veth 命名用）
)

// NetEnv 把网络装配参数编码为一组环境变量（追加到 runtime.Config.Env）。
// mode 与 hostname 必填；bridge 还需 cid/name/ip/gateway/prefix。host/none
// 仅标注 mode（不开新 netns 或只建空 netns）。engine/shim 在启动前调用。
func NetEnv(mode network.NetMode, cid, name, ip, gateway, hostname string, prefix int) []string {
	base := []string{
		envNetMode + "=" + string(mode),
		envNetCID + "=" + cid,
		envNetHost + "=" + hostname,
	}
	switch mode {
	case network.ModeBridge:
		return append(base,
			envNetName+"="+name,
			envNetIP+"="+ip,
			envNetGateway+"="+gateway,
			envNetPrefix+"="+strconv.Itoa(prefix),
		)
	default:
		return base
	}
}

// ResolveNetEnv 依据容器已登记的网络档案解析运行时装配置，返回待追加的
// 环境变量。engine（前台）与 shim（detach）在启动容器前调用；桥接网络从
// <storeRoot>/networks/<name>.json 读取网关与子网，host/none 仅标注模式。
// 解析失败时返回 nil 并告警（调用方维持现状启动，容器无网络但可运行）。
func ResolveNetEnv(storeRoot, netName, containerID, hostname string) []string {
	env := resolveNetEnvBase(storeRoot, netName, containerID, hostname)
	// 追加宿主侧探测到的 DNS 上游。
	//
	// **必须在宿主侧探测**：写 resolv.conf 的代码在容器 init 里执行
	// （已 pivot_root），那时读到的 /run/systemd/resolve/resolv.conf 与
	// /etc/resolv.conf 都是**容器的**——前者不存在、后者是容器自己的，
	// 因此容器内永远拿不到宿主真实上游（真机实测确认：容器内探测只能
	// 走公共 DNS 兜底）。
	//
	// 本函数由 engine（前台）与 shim（detach）在**宿主侧**调用，是唯一能
	// 读到宿主 DNS 配置的时机。结果经 LICORE_NET_DNS 传进容器。
	if len(env) > 0 {
		if ns := network.ResolveNameservers(); len(ns) > 0 {
			env = append(env, network.EnvNetDNS+"="+strings.Join(ns, ","))
		}
	}
	return env
}

// resolveNetEnvBase 是按网络模式解析装配参数的原实现。
func resolveNetEnvBase(storeRoot, netName, containerID, hostname string) []string {
	mode := network.ParseNetMode(netName)
	switch mode {
	case network.ModeHost:
		return NetEnv(network.ModeHost, containerID, "", "", "", hostname, 0)
	case network.ModeNone:
		return NetEnv(network.ModeNone, containerID, "", "", "", hostname, 0)
	case network.ModeBridge:
		if netName == "" {
			netName = network.PresetBridgeName
		}
		m, err := network.NewManager(storeRoot)
		if err != nil {
			slog.Warn("读取网络管理器失败，容器将无网络配置", "container", containerID, "err", err)
			return nil
		}
		cn, err := m.ClientNetConfig(netName, containerID)
		if err != nil {
			slog.Warn("读取容器网络配置失败，容器将无网络配置", "container", containerID, "err", err)
			return nil
		}
		return NetEnv(network.ModeBridge, containerID, cn.Name, cn.IP, cn.Gateway, hostname, cn.Prefix)
	}
	return nil
}

// parseNetEnv 从环境变量切片解析网络装配参数；找不到 MODE 时返回 nil。
func parseNetEnv(env []string) (*network.NetMode, string, string, string, string, string, int, bool) {
	get := func(k string) (string, bool) {
		prefix := k + "="
		for _, e := range env {
			if strings.HasPrefix(e, prefix) {
				return strings.TrimPrefix(e, prefix), true
			}
		}
		return "", false
	}
	ms, ok := get(envNetMode)
	if !ok {
		return nil, "", "", "", "", "", 0, false
	}
	mode := network.NetMode(ms)
	hostname, _ := get(envNetHost)
	cid, _ := get(envNetCID)
	if mode != network.ModeBridge {
		return &mode, cid, "", "", "", hostname, 0, true
	}
	name, _ := get(envNetName)
	ip, _ := get(envNetIP)
	gw, _ := get(envNetGateway)
	prefix := 0
	if ps, ok := get(envNetPrefix); ok {
		prefix, _ = strconv.Atoi(ps)
	}
	return &mode, cid, name, ip, gw, hostname, prefix, true
}
