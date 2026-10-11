// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package engine

import (
	"os"
	"testing"

	"github.com/LiStudioorg/licore/internal/network"
	"github.com/LiStudioorg/licore/internal/store"
)

func TestParsePorts(t *testing.T) {
	cases := []struct {
		raw  string
		host int
		cont int
		ip   string
		udp  bool
	}{
		{"80", 0, 80, "", false},
		{"8080:80", 8080, 80, "", false},
		{"8080:80/udp", 8080, 80, "", true},
		{"127.0.0.1:8080:80", 8080, 80, "127.0.0.1", false},
	}
	for _, c := range cases {
		got, err := parsePorts([]string{c.raw})
		if err != nil {
			t.Fatalf("parsePorts(%q) 错误: %v", c.raw, err)
		}
		p := got[0]
		if p.HostPort != c.host || p.ContainerPort != c.cont || p.HostIP != c.ip {
			t.Fatalf("parsePorts(%q)=%+v 期望 host=%d cont=%d ip=%s", c.raw, p, c.host, c.cont, c.ip)
		}
		if (p.Proto == network.ProtoUDP) != c.udp {
			t.Fatalf("parsePorts(%q) 协议错误: %s", c.raw, p.Proto)
		}
	}
}

func TestParsePortsBad(t *testing.T) {
	for _, raw := range []string{"a:b:c:d", "8080:80/foo", "0:0"} {
		if _, err := parsePorts([]string{raw}); err == nil {
			t.Fatalf("parsePorts(%q) 应报错", raw)
		}
	}
}

// TestWireNetworkHostNone 验证 host/none 网络只记录模式、不要求网络存在。
func TestWireNetworkHostNone(t *testing.T) {
	st := &store.Store{Root: t.TempDir()}
	for i, mode := range []string{"host", "none"} {
		cid := "abc123def" + string(rune('0'+i)) + string(rune('0'+i))
		cfg := &store.ContainerConfig{
			ConfigVersion: 1, ID: cid, Name: "c" + string(rune('0'+i)), ImageRef: "x:v1",
			Restart: store.RestartNo, Cmd: []string{"/bin/sh"}, Rootfs: "/tmp/x", Network: mode,
		}
		if err := st.CreateContainer(cfg); err != nil {
			t.Fatal(err)
		}
		if err := wireNetworkBeforeStart(st, cfg, mode, "", nil); err != nil {
			t.Fatalf("模式 %s 不应失败: %v", mode, err)
		}
		got, err := st.LoadContainer(cfg.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Network != mode || got.IP != "" {
			t.Fatalf("模式 %s 记录错误: %+v", mode, got)
		}
	}
}

// TestWireNetworkPresetEnsured 验证缺省 licore0 网络会自动补建（无 root 时
// 网桥创建失败仅告警，不阻断端点登记与 IP 分配）。
//
// **需要 root + nft/iptables**：缺省 bridge 网络要真实建网桥并实化 NAT，
// 而 ApplyNAT 失败对 bridge 容器是致命错误（不能只告警——那会产出"起来了
// 但完全没网"的容器）。非 root 环境无法满足，直接跳过而非报错。
func TestWireNetworkPresetEnsured(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("非 root：缺省 bridge 网络需建网桥并实化 NAT，跳过")
	}
	st := &store.Store{Root: t.TempDir()}
	cfg := &store.ContainerConfig{
		ConfigVersion: 1, ID: "abcdef123456", Name: "web", ImageRef: "x:v1",
		Restart: store.RestartNo, Cmd: []string{"/bin/sh"}, Rootfs: "/tmp/x", Network: "",
	}
	if err := st.CreateContainer(cfg); err != nil {
		t.Fatal(err)
	}
	if err := wireNetworkBeforeStart(st, cfg, "", "", nil); err != nil {
		t.Fatalf("缺省网络不应失败: %v", err)
	}
	if cfg.IP == "" {
		t.Fatal("缺少分配 IP")
	}
	got, err := st.LoadContainer(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != network.PresetBridgeName || got.IP == "" {
		t.Fatalf("配置回写错误: %+v", got)
	}
	// 清理：断开端点后应可删除容器目录。
	disconnectContainer(st, got)
}

// TestWireNetworkUnknownNet 验证引用不存在的自定义网络会报错。
func TestWireNetworkUnknownNet(t *testing.T) {
	st := &store.Store{Root: t.TempDir()}
	cfg := &store.ContainerConfig{
		ConfigVersion: 1, ID: "abcdef123456", Name: "web", ImageRef: "x:v1",
		Restart: store.RestartNo, Cmd: []string{"/bin/sh"}, Rootfs: "/tmp/x", Network: "ghostnet",
	}
	if err := st.CreateContainer(cfg); err != nil {
		t.Fatal(err)
	}
	err := wireNetworkBeforeStart(st, cfg, "ghostnet", "", nil)
	if err == nil {
		t.Fatal("不存在的自定义网络应报错")
	}
}

// TestDisconnectContainer 验证对未知网络的断开调用不 panic（容错）。
func TestDisconnectContainer(t *testing.T) {
	st := &store.Store{Root: t.TempDir()}
	disconnectContainer(st, nil)
	disconnectContainer(st, &store.ContainerConfig{Network: "host"})
	disconnectContainer(st, &store.ContainerConfig{Network: "ghost", ID: "x"})
}

// TestNetEnvFor 验证 netEnvFor 对 host/none 直接给模式 env、nil cfg 返回 nil。
func TestNetEnvFor(t *testing.T) {
	st := &store.Store{Root: t.TempDir()}
	if got := netEnvFor(st, nil, "h"); got != nil {
		t.Fatalf("nil cfg 应返回 nil，实得 %v", got)
	}
	host := netEnvFor(st, &store.ContainerConfig{Network: "host", ID: "abc"}, "h")
	if len(host) == 0 {
		t.Fatal("host 网络应返回模式 env")
	}
	none := netEnvFor(st, &store.ContainerConfig{Network: "none", ID: "abc"}, "h")
	if len(none) == 0 {
		t.Fatal("none 网络应返回模式 env")
	}
	// 自定义/未知网络名解析失败应返回 nil（不 panic）。
	_ = netEnvFor(st, &store.ContainerConfig{Network: "ghostnet", ID: "abc"}, "h")
}
