// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package network

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteDNSFilesToCreatesEtc 回归：FROM scratch 镜像没有 /etc，写
// resolv.conf/hosts 前必须 MkdirAll /etc，否则 init 报
// "写 /etc/resolv.conf: no such file or directory" 并退出。
func TestWriteDNSFilesToCreatesEtc(t *testing.T) {
	root := t.TempDir()
	if err := writeDNSFilesTo(root, "172.18.0.3", "172.18.0.1", "demo"); err != nil {
		t.Fatalf("writeDNSFilesTo: %v", err)
	}
	rc, err := os.ReadFile(filepath.Join(root, "etc", "resolv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	// 本测试的意图是「/etc 被自动创建、文件写成功」，与具体 DNS 地址无关。
	// resolv.conf 的内容现在来自宿主探测（见 dnsresolve_linux.go），
	// 因此这里只断言"写出了可用的 nameserver 行"，不硬编码地址——
	// 硬编码会在换机器（上游不同）时误报失败。
	rs := string(rc)
	if !strings.Contains(rs, "nameserver ") {
		t.Fatalf("resolv.conf 应含 nameserver 行，实际 %q", rs)
	}
	// **且不应再指向网桥网关**：网关上没有 DNS 服务，那正是本缺陷的根因。
	if strings.Contains(rs, "172.18.0.1") {
		t.Fatalf("resolv.conf 不应指向网桥网关（无 DNS 服务），实际 %q", rs)
	}
	hf, err := os.ReadFile(filepath.Join(root, "etc", "hosts"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(hf)
	if !strings.Contains(s, "172.18.0.3 demo") || !strings.Contains(s, "172.18.0.1 licore-gw") {
		t.Fatalf("hosts 缺本机名/网关: %q", s)
	}
}

// TestWriteDNSFilesToOverwrites 验证可重跑（写现有文件不报错）。
func TestWriteDNSFilesToOverwrites(t *testing.T) {
	root := t.TempDir()
	if err := writeDNSFilesTo(root, "172.18.0.3", "172.18.0.1", "demo"); err != nil {
		t.Fatal(err)
	}
	if err := writeDNSFilesTo(root, "172.18.0.3", "172.18.0.1", "demo"); err != nil {
		t.Fatalf("二次调用: %v", err)
	}
}
