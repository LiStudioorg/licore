// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package network

// 非 Linux 平台的 DNS 探测 stub。
//
// 为什么需要：`internal/runtime/network.go` 的 `ResolveNetEnv` **不带 build tag**
// （CLI 全平台都要调用），它引用 `ResolveNameservers` 与 `EnvNetDNS`。
// 若这两个符号只存在于 linux 文件里，darwin 交叉编译会直接失败
// （AGENTS.md 要求三平台交叉编译通过）。
//
// 非 Linux 上网络本就是 ErrUnsupported（见 driver_nonlinux.go），
// 这里只需保证**符号存在且可编译**，不需要真实实现。

// EnvNetDNS 是宿主侧把 DNS 列表传给容器的环境变量名。
// 非 Linux 上不产生实际效果，仅为保持符号一致。
const EnvNetDNS = "LICORE_NET_DNS"

// ResolveNameservers 在非 Linux 平台返回公共 DNS 兜底。
//
// 返回非空是本函数的契约（见 linux 版说明）：调用方依赖"至少有可用解析器"，
// 返回 nil 会让上层误判为探测失败并触发额外分支。macOS 后端走轻量虚拟机
// 路线，容器内的 DNS 由 VM 侧处理，这里给的值不会被用到。
func ResolveNameservers() []string {
	return []string{"1.1.1.1", "8.8.8.8"}
}
