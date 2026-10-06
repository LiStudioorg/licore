// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package network

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/LiStudioorg/licore/internal/network/netlink"
)

// withBridgeAddrs 临时替换 bridgeAddrs 注入桩数据，测试结束自动还原。
func withBridgeAddrs(t *testing.T, addrs []netlink.IfAddr, err error) {
	t.Helper()
	orig := bridgeAddrs
	bridgeAddrs = func(string) ([]netlink.IfAddr, error) { return addrs, err }
	t.Cleanup(func() { bridgeAddrs = orig })
}

// 本组测试对应 L-6：网桥已存在时不校验网段，导致容器拿到指向不存在网关的
// 默认路由 —— "起来了但完全没网，且无任何报错"。

func TestCheckBridgeSubnetAcceptsMatchingGateway(t *testing.T) {
	n := New(PresetBridgeName, DriverBridge)
	n.Subnet = "172.22.0.0/16"
	n.Gateway = "172.22.0.1"
	withBridgeAddrs(t, []netlink.IfAddr{{IP: "172.22.0.1", Prefix: 16}}, nil)

	if err := checkBridgeSubnet("licore0", n); err != nil {
		t.Fatalf("网段一致时应放行，却报错: %v", err)
	}
}

// 网桥挂多个地址是合法用法，不能要求"只有它一个"。
func TestCheckBridgeSubnetAcceptsExtraAddrs(t *testing.T) {
	n := New(PresetBridgeName, DriverBridge)
	n.Subnet = "172.22.0.0/16"
	n.Gateway = "172.22.0.1"
	withBridgeAddrs(t, []netlink.IfAddr{
		{IP: "10.0.0.1", Prefix: 8},
		{IP: "172.22.0.1", Prefix: 16},
		{IP: "192.168.5.1", Prefix: 24},
	}, nil)

	if err := checkBridgeSubnet("licore0", n); err != nil {
		t.Fatalf("含匹配地址的多地址网桥应放行，却报错: %v", err)
	}
}

// **核心用例**：真机实测的错配场景 —— 宿主网桥是 172.22.0.1/16，
// 新数据目录给出 172.21.0.0/16。修复前这里静默放行，容器无网。
func TestCheckBridgeSubnetRejectsMismatch(t *testing.T) {
	n := New(PresetBridgeName, DriverBridge)
	n.Subnet = "172.21.0.0/16" // 新 store 分配的
	n.Gateway = "172.21.0.1"
	withBridgeAddrs(t, []netlink.IfAddr{{IP: "172.22.0.1", Prefix: 16}}, nil) // 宿主既有的

	err := checkBridgeSubnet("licore0", n)
	if err == nil {
		t.Fatal("!!! 漏洞复现：网段不匹配却放行了 —— 容器会起来但无网络")
	}
	if !errors.Is(err, ErrBridgeSubnetMismatch) {
		t.Fatalf("错误未包装 ErrBridgeSubnetMismatch: %v", err)
	}
	// 报错必须能让用户直接行动：讲清现状、期望、怎么办。
	for _, want := range []string{"172.22.0.1/16", "172.21.0.1", "172.21.0.0/16", "LICORE_HOME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息缺少关键信息 %q：\n%s", want, err)
		}
	}
}

// 网段对但网关不在网桥上 → 容器默认路由仍指向不存在的下一跳，必须拒绝。
func TestCheckBridgeSubnetRejectsWrongGatewaySameSubnet(t *testing.T) {
	n := New(PresetBridgeName, DriverBridge)
	n.Subnet = "172.22.0.0/16"
	n.Gateway = "172.22.0.1"
	withBridgeAddrs(t, []netlink.IfAddr{{IP: "172.22.0.9", Prefix: 16}}, nil)

	if err := checkBridgeSubnet("licore0", n); err == nil {
		t.Fatal("网关不在网桥上时应拒绝")
	} else if !errors.Is(err, ErrBridgeSubnetMismatch) {
		t.Fatalf("错误类型不对: %v", err)
	}
}

// 前缀长度不同也算不一致：/16 与 /24 决定容器的连本网段路由范围。
func TestCheckBridgeSubnetRejectsDifferentPrefix(t *testing.T) {
	n := New(PresetBridgeName, DriverBridge)
	n.Subnet = "172.22.0.0/16"
	n.Gateway = "172.22.0.1"
	withBridgeAddrs(t, []netlink.IfAddr{{IP: "172.22.0.1", Prefix: 24}}, nil)

	if err := checkBridgeSubnet("licore0", n); err == nil {
		t.Fatal("前缀长度不同应拒绝")
	}
}

// 网桥存在但一个地址都没有（刚建好还没配）→ 同样会让容器无网，拒绝。
func TestCheckBridgeSubnetRejectsNoAddrs(t *testing.T) {
	n := New(PresetBridgeName, DriverBridge)
	n.Subnet = "172.22.0.0/16"
	n.Gateway = "172.22.0.1"
	withBridgeAddrs(t, nil, nil)

	err := checkBridgeSubnet("licore0", n)
	if err == nil {
		t.Fatal("无地址的网桥应拒绝")
	}
	if !strings.Contains(err.Error(), "无 IPv4 地址") {
		t.Errorf("应说明网桥上没有任何地址：%v", err)
	}
}

// 列地址失败必须报错，不能当作"没问题"放行（静默放行=退回修复前行为）。
func TestCheckBridgeSubnetFailsClosedOnListError(t *testing.T) {
	n := New(PresetBridgeName, DriverBridge)
	n.Subnet = "172.22.0.0/16"
	n.Gateway = "172.22.0.1"
	withBridgeAddrs(t, nil, fmt.Errorf("模拟 netlink 失败"))

	if err := checkBridgeSubnet("licore0", n); err == nil {
		t.Fatal("读不到地址时必须失败关闭，不能静默放行")
	}
}

// 非法网段要落到 ErrBadNetwork，而不是被误报成"网段不匹配"。
func TestCheckBridgeSubnetBadSubnet(t *testing.T) {
	n := New(PresetBridgeName, DriverBridge)
	n.Subnet = "不是网段"
	n.Gateway = "172.22.0.1"
	withBridgeAddrs(t, []netlink.IfAddr{{IP: "172.22.0.1", Prefix: 16}}, nil)

	err := checkBridgeSubnet("licore0", n)
	if err == nil {
		t.Fatal("非法网段应报错")
	}
	if !errors.Is(err, ErrBadNetwork) {
		t.Fatalf("应包装 ErrBadNetwork: %v", err)
	}
}

// 反向约束：非 bridge 驱动不应受本检查影响（driverBootstrap 提前返回）。
func TestDriverBootstrapSkipsNonBridge(t *testing.T) {
	n := New("hostnet", DriverHost)
	// 若实现改成"先查网桥再判驱动"，这里会因 ListAddrs 失败而报错。
	if err := driverBootstrap(n); err != nil {
		t.Fatalf("非 bridge 驱动不应走网桥检查: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ensurePreset：既有网桥是事实来源
// ---------------------------------------------------------------------------

// **根因用例**：宿主已有 licore0（172.22.0.1/16），新建的数据目录必须
// **采用它的网段**，而不是 pickFreeSubnet 另分配一个（那会得到 172.21，
// 网桥上根本没这个地址，容器起来后无网 —— L-6）。
func TestEnsurePresetAdoptsExistingBridgeSubnet(t *testing.T) {
	withBridgeAddrs(t, []netlink.IfAddr{{IP: "172.22.0.1", Prefix: 16}}, nil)

	m := newTestManager(t)
	if err := m.EnsurePreset(); err != nil {
		t.Fatalf("EnsurePreset: %v", err)
	}
	n, err := m.Load(PresetBridgeName)
	if err != nil {
		t.Fatal(err)
	}
	if n.Subnet != "172.22.0.0/16" {
		t.Errorf("应采用既有网桥网段 172.22.0.0/16，实际 %s（容器会因网关不存在而无网）", n.Subnet)
	}
	if n.Gateway != "172.22.0.1" {
		t.Errorf("应采用既有网桥网关 172.22.0.1，实际 %s", n.Gateway)
	}
}

// 反向约束：宿主上**没有**网桥时，仍应自行挑一个空闲网段（保持原行为）。
func TestEnsurePresetPicksSubnetWhenNoBridge(t *testing.T) {
	withBridgeAddrs(t, nil, nil) // 网桥不存在

	m := newTestManager(t)
	if err := m.EnsurePreset(); err != nil {
		t.Fatalf("EnsurePreset: %v", err)
	}
	n, err := m.Load(PresetBridgeName)
	if err != nil {
		t.Fatal(err)
	}
	if n.Subnet == "" || n.Gateway == "" {
		t.Fatal("无既有网桥时应自行分配网段与网关")
	}
	if _, ipnet, _ := net.ParseCIDR(n.Subnet); ipnet == nil {
		t.Errorf("分配的网段不可解析: %q", n.Subnet)
	}
}

// 采用既有网段后，driverBootstrap 的网段校验必须放行（两者不再打架）。
func TestEnsurePresetThenBootstrapPasses(t *testing.T) {
	withBridgeAddrs(t, []netlink.IfAddr{{IP: "172.22.0.1", Prefix: 16}}, nil)

	m := newTestManager(t)
	if err := m.EnsurePreset(); err != nil {
		t.Fatalf("EnsurePreset: %v", err)
	}
	// EnsureDriver 会 Load 后再 driverBootstrap；网段已一致，不应报不匹配。
	if err := m.EnsureDriver(PresetBridgeName); err != nil &&
		errors.Is(err, ErrBridgeSubnetMismatch) {
		t.Fatalf("采用既有网段后仍报网段不匹配，说明两处逻辑不一致: %v", err)
	}
}

// existingBridgeSubnet 的纯函数行为。
func TestExistingBridgeSubnet(t *testing.T) {
	cases := []struct {
		name  string
		addrs []netlink.IfAddr
		err   error
		want  string // 期望的 subnet，"" 表示 ok=false
	}{
		{"正常 /16", []netlink.IfAddr{{IP: "172.22.0.1", Prefix: 16}}, nil, "172.22.0.0/16"},
		{"正常 /24", []netlink.IfAddr{{IP: "192.168.9.1", Prefix: 24}}, nil, "192.168.9.0/24"},
		{"多地址取第一个", []netlink.IfAddr{{IP: "10.1.0.1", Prefix: 16}, {IP: "10.2.0.1", Prefix: 16}}, nil, "10.1.0.0/16"},
		{"无地址", nil, nil, ""},
		{"读取失败", nil, fmt.Errorf("boom"), ""},
		{"前缀非法", []netlink.IfAddr{{IP: "172.22.0.1", Prefix: 0}}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withBridgeAddrs(t, c.addrs, c.err)
			subnet, gateway, ok := existingBridgeSubnet("licore0")
			if c.want == "" {
				if ok {
					t.Fatalf("期望 ok=false，却得到 %s / %s", subnet, gateway)
				}
				return
			}
			if !ok {
				t.Fatal("期望 ok=true，却得到 false")
			}
			if subnet != c.want {
				t.Errorf("subnet: 期望 %s，实际 %s", c.want, subnet)
			}
			if gateway == "" {
				t.Error("gateway 不应为空")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 端到端：必须经由 driverBootstrap 真正走到校验，而不是只测 helper
// ---------------------------------------------------------------------------

// **这条才是 L-6 的真正回归护栏**。
//
// 反向验证时发现：只测 checkBridgeSubnet 的用例在"回退 driverBootstrap
// 里的复用检查"后**依然全绿** —— 因为它们是直接调用 helper，绕过了
// 调用点。helper 正确不等于接线正确。因此这里从 driverBootstrap 进入，
// 并注入 LinkByName 的桩，确保走的是"网桥已存在"那条分支。
func TestDriverBootstrapRejectsMismatchedExistingBridge(t *testing.T) {
	// 桩：网桥"已存在"（LinkByName 成功），且它的地址与网络定义不一致。
	origLink := linkByNameFn
	linkByNameFn = func(string) (*netlink.Link, error) { return &netlink.Link{Name: "licore0"}, nil }
	t.Cleanup(func() { linkByNameFn = origLink })
	withBridgeAddrs(t, []netlink.IfAddr{{IP: "172.22.0.1", Prefix: 16}}, nil)

	n := New(PresetBridgeName, DriverBridge)
	n.Subnet = "172.21.0.0/16"
	n.Gateway = "172.21.0.1"

	err := driverBootstrap(n)
	if err == nil {
		t.Fatal("!!! 漏洞复现：driverBootstrap 复用了网段不匹配的既有网桥")
	}
	if !errors.Is(err, ErrBridgeSubnetMismatch) {
		t.Fatalf("错误未包装 ErrBridgeSubnetMismatch: %v", err)
	}
}

// 反向约束：网段一致时 driverBootstrap 必须放行（幂等，不做任何改动）。
func TestDriverBootstrapAcceptsMatchingExistingBridge(t *testing.T) {
	origLink := linkByNameFn
	linkByNameFn = func(string) (*netlink.Link, error) { return &netlink.Link{Name: "licore0"}, nil }
	t.Cleanup(func() { linkByNameFn = origLink })
	withBridgeAddrs(t, []netlink.IfAddr{{IP: "172.22.0.1", Prefix: 16}}, nil)

	n := New(PresetBridgeName, DriverBridge)
	n.Subnet = "172.22.0.0/16"
	n.Gateway = "172.22.0.1"

	if err := driverBootstrap(n); err != nil {
		t.Fatalf("网段一致时应幂等放行，却报错: %v", err)
	}
}
