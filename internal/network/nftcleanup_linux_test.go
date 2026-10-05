// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package network

// nft 部分失败的清理测试。
//
// 背景（真机实测）：nftCreateTables 会**先**把表和三条链建好，之后才写规则。
// 本机 `masquerade` 不受支持，于是规则那一步失败，而表和链已经留在宿主上
// ——`nft list tables` 里残留一张空的 `table ip licore`（0 条规则）。
//
// 空链虽无功能影响，但会让"nft 表是否存在"这类诊断产生误导，也污染宿主
// 规则集。因此 nft 路径任何失败都必须把可能已建的表清掉。

import (
	"os/exec"
	"strings"
	"testing"
)

// TestNftDropTablesIsIdempotent 断言反复删除不报错。
//
// 清理路径会在多种失败分支里被调用（无回退可用、回退成功、回退也失败），
// 幂等是前提——否则清理本身会盖掉真正的错误原因。
func TestNftDropTablesIsIdempotent(t *testing.T) {
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("本机无 nft")
	}
	// 表不存在时也不应 panic 或阻塞（runNft 把"不存在"类错误视为幂等）。
	nftDropTables()
	nftDropTables()
}

// TestNftIdempotentMarkersCoverDeleteCases 断言删除类错误被识别为幂等。
//
// `nft delete table` 在表不存在时报 "No such file or directory"——
// 这条**必须**算幂等，否则清理会误报失败。注意它与"缺内核特性"报的是
// 同一句话，所以 runNft 的过滤规则改造（见 nftfail_linux_test.go）
// 与这里是配套的：**幂等标记不再包含 "No such file or directory"**，
// 但 delete 的失败由调用方（nftDropTables）显式忽略返回值来兜住。
func TestNftIdempotentMarkersCoverDeleteCases(t *testing.T) {
	for _, m := range nftIdempotentMarkers {
		if strings.Contains(strings.ToLower(m), "no such file") {
			t.Fatal("幂等标记不得含 No such file（会把缺特性误判为幂等，导致 NAT 静默失效）")
		}
	}
}
