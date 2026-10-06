// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package network

// 非 Linux 平台：网络实化统一返回 ErrUnsupported，定义管理仍可用（纯数据）。

func bridgeHostIface(n *Network) string { return bridgeHostInterface(n.Name) }

func hostDefaultIface() (string, error) { return "", ErrUnsupported }

func driverBootstrap(n *Network) error {
	if n.Driver != DriverBridge {
		return nil
	}
	return ErrUnsupported
}

func driverTeardown(n *Network) error {
	if n.Driver != DriverBridge {
		return nil
	}
	return ErrUnsupported
}

func bridgeAttachEndpoints(n *Network) error { return ErrUnsupported }

func bridgeDetachEndpoint(veth string) error { return ErrUnsupported }

// existingBridgeSubnet 在非 Linux 上读不到宿主网桥，恒为"不存在"，
// 调用方（Manager.ensurePreset）会退回自行挑选网段。
// 与 Linux 实现保持同签名，保证 manager.go（不带 build tag）可跨平台编译。
func existingBridgeSubnet(ifname string) (subnet, gateway string, ok bool) {
	return "", "", false
}

func (n *Network) applyPortRules() error        { return ErrUnsupported }
func (n *Network) removePortRules(string) error { return ErrUnsupported }
