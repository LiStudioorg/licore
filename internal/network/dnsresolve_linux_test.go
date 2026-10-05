// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package network

// 宿主 DNS 探测的单元测试。
//
// 背景（真机实测）：容器 resolv.conf 原先硬编码为网桥网关，但网关上**没有
// DNS 服务**，容器里所有域名解析超时（`nslookup google.com` →
// connection timed out）。本机宿主是 systemd-resolved 形态：
// /etc/resolv.conf 是指向 stub-resolv.conf 的符号链接，内容是
// `nameserver 127.0.0.53`——宿主回环，容器内不可达。
//
// 因此探测必须：
//   1. 优先读 systemd-resolved 的真实上游文件；
//   2. 过滤掉回环地址（容器有独立 netns，宿主回环指向容器自己）；
//   3. 全都拿不到时回退公共 DNS——**绝不返回空列表**。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp 在临时目录写一个文件并返回路径。
func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写 %s: %v", p, err)
	}
	return p
}

// TestResolvePrefersSystemdUpstream 断言 systemd 上游文件优先于 /etc/resolv.conf。
//
// 这是本机真实形态：/etc/resolv.conf 给的是 127.0.0.53（无用），
// 真实上游在 /run/systemd/resolve/resolv.conf。
func TestResolvePrefersSystemdUpstream(t *testing.T) {
	systemd := writeTemp(t, "upstream.conf", "nameserver 8.8.8.8\nnameserver 114.114.114.114\n")
	etc := writeTemp(t, "resolv.conf", "nameserver 127.0.0.53\n")

	got := resolveNameserversFrom(systemd, etc)
	want := []string{"8.8.8.8", "114.114.114.114"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v（应优先采用 systemd 的真实上游）", got, want)
	}
}

// TestResolveFallsBackToEtc 断言没有 systemd 文件时用 /etc/resolv.conf。
func TestResolveFallsBackToEtc(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.conf")
	etc := writeTemp(t, "resolv.conf", "nameserver 223.5.5.5\n")

	got := resolveNameserversFrom(missing, etc)
	if len(got) != 1 || got[0] != "223.5.5.5" {
		t.Errorf("got %v, want [223.5.5.5]", got)
	}
}

// TestResolveFiltersLoopback 断言回环地址被过滤掉。
//
// 这是本缺陷的核心：宿主 resolv.conf 里的 127.0.0.53 是宿主回环上的
// stub 监听，容器有独立 netns，回环指向容器自己——**必然解析失败**。
// 原实现「照抄宿主 resolv.conf」的做法在这里就会踩坑。
func TestResolveFiltersLoopback(t *testing.T) {
	etc := writeTemp(t, "resolv.conf", strings.Join([]string{
		"nameserver 127.0.0.53",
		"nameserver 127.0.0.1",
		"nameserver ::1",
		"nameserver 8.8.8.8",
		"nameserver 127.1.2.3",
	}, "\n"))

	got := resolveNameserversFrom(filepath.Join(t.TempDir(), "none"), etc)
	if strings.Join(got, ",") != "8.8.8.8" {
		t.Errorf("got %v，应只剩非回环的 8.8.8.8（127.0.0.0/8 与 ::1 必须被丢弃）", got)
	}
}

// TestResolveAllLoopbackFallsBackToPublic 断言全是回环时回退公共 DNS。
//
// 关键：**不能返回空列表**。空列表会让容器完全无法解析域名，
// 比用公共 DNS 更糟。
func TestResolveAllLoopbackFallsBackToPublic(t *testing.T) {
	etc := writeTemp(t, "resolv.conf", "nameserver 127.0.0.53\n")

	got := resolveNameserversFrom(filepath.Join(t.TempDir(), "none"), etc)
	if len(got) == 0 {
		t.Fatal("全是回环时必须回退公共 DNS，不能返回空列表")
	}
	if strings.Join(got, ",") != publicFallbackDNS {
		t.Errorf("got %v, want %v", got, publicFallbackDNS)
	}
}

// TestResolveMissingFilesFallsBack 断言文件都读不到时回退。
func TestResolveMissingFilesFallsBack(t *testing.T) {
	dir := t.TempDir()
	got := resolveNameserversFrom(filepath.Join(dir, "a"), filepath.Join(dir, "b"))
	if strings.Join(got, ",") != publicFallbackDNS {
		t.Errorf("got %v, want %v", got, publicFallbackDNS)
	}
}

// TestResolveCapsAtThree 断言最多保留 3 个。
//
// glibc 的 resolver 只读前 MAXNS(3) 条，写更多不会生效，
// 反而让用户以为后面的也在用。
func TestResolveCapsAtThree(t *testing.T) {
	etc := writeTemp(t, "resolv.conf", strings.Join([]string{
		"nameserver 1.1.1.1",
		"nameserver 8.8.8.8",
		"nameserver 9.9.9.9",
		"nameserver 208.67.222.222",
	}, "\n"))

	got := resolveNameserversFrom(filepath.Join(t.TempDir(), "none"), etc)
	if len(got) != maxNameservers {
		t.Errorf("应截断到 %d 个，实际 %d: %v", maxNameservers, len(got), got)
	}
	// 保留的是**前**三个（顺序有意义：resolver 按顺序尝试）。
	if got[0] != "1.1.1.1" || got[2] != "9.9.9.9" {
		t.Errorf("应保留前 %d 个，实际 %v", maxNameservers, got)
	}
}

// TestParseNameserversTolerance 覆盖解析的容错与边界。
func TestParseNameserversTolerance(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"空内容", "", nil},
		{"只有注释", "# comment\n; another\n", nil},
		{"大小写不敏感", "NAMESERVER 8.8.8.8\nNameserver 1.1.1.1\n", []string{"8.8.8.8", "1.1.1.1"}},
		{"前导空白", "   nameserver   8.8.8.8  \n", []string{"8.8.8.8"}},
		{"缺参数", "nameserver\n", nil},
		{"非法地址", "nameserver not-an-ip\n", nil},
		{"去重", "nameserver 8.8.8.8\nnameserver 8.8.8.8\n", []string{"8.8.8.8"}},
		{"忽略其它指令", "search example.com\noptions edns0\nnameserver 8.8.8.8\n", []string{"8.8.8.8"}},
		{"IPv6 上游", "nameserver 2001:4860:4860::8888\n", []string{"2001:4860:4860::8888"}},
		{"行尾注释不影响", "nameserver 8.8.8.8 # primary\n", []string{"8.8.8.8"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNameservers(tc.content)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFormatResolvConf 断言生成的内容只含 nameserver 行。
//
// 刻意不照抄宿主的 search / options：宿主 search 域对容器内应用往往没有
// 意义，`options edns0 trust-ad` 是 stub 专用、直连上游时不该带。
func TestFormatResolvConf(t *testing.T) {
	got := formatResolvConf([]string{"8.8.8.8", "114.114.114.114"})
	want := "nameserver 8.8.8.8\nnameserver 114.114.114.114\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "search") || strings.Contains(got, "options") {
		t.Errorf("不应包含 search/options 行: %q", got)
	}
}

// TestWriteDNSFilesUsesResolvedNameservers 断言写入容器 root 的
// resolv.conf 用的是**探测结果**而非网桥网关。
//
// 这是端到端的行为断言：直接把 root 指到临时目录，检查真实落盘内容。
func TestWriteDNSFilesUsesResolvedNameservers(t *testing.T) {
	root := t.TempDir()
	// 造一个可探测的宿主环境，避免依赖真实机器配置。
	upstream := writeTemp(t, "up.conf", "nameserver 203.0.113.53\n")
	origSystemd, origEtc := systemdResolvedUpstream, hostResolvConf
	t.Cleanup(func() { systemdResolvedUpstream, hostResolvConf = origSystemd, origEtc })
	systemdResolvedUpstream, hostResolvConf = upstream, upstream

	if err := writeDNSFilesTo(root, "172.22.0.3", "172.22.0.1", "box"); err != nil {
		t.Fatalf("writeDNSFilesTo: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "etc", "resolv.conf"))
	if err != nil {
		t.Fatalf("读 resolv.conf: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "nameserver 203.0.113.53") {
		t.Errorf("resolv.conf 应含探测到的上游，实际: %q", got)
	}
	if strings.Contains(got, "172.22.0.1") {
		t.Errorf("resolv.conf **不应**再指向网桥网关（网关上没有 DNS 服务）: %q", got)
	}
}

// TestResolveRealHostHasUsableServers 对真实宿主环境的冒烟测试。
//
// 只断言"能拿到非空且不含回环的列表"——具体地址随机器而变，不做硬断言。
func TestResolveRealHostHasUsableServers(t *testing.T) {
	got := resolveNameservers()
	if len(got) == 0 {
		t.Fatal("真实宿主上探测不应返回空列表")
	}
	for _, ns := range got {
		if strings.HasPrefix(ns, "127.") || ns == "::1" {
			t.Errorf("结果不应含回环地址（容器内不可达）: %v", got)
		}
	}
	if len(got) > maxNameservers {
		t.Errorf("不应超过 %d 个: %v", maxNameservers, got)
	}
	t.Logf("本机探测结果: %v", got)
}

// TestWriteDNSFilesPrefersInjectedEnv 断言 `LICORE_NET_DNS` 优先于容器内探测。
//
// 这是修复的关键：写 resolv.conf 的代码跑在容器 init 里（已 pivot_root），
// 那时读到的 resolv.conf 是**容器自己的**，永远拿不到宿主上游。
// 因此正确流程是宿主侧（engine/shim）探测后经环境变量注入。
//
// 本测试模拟"容器内"场景：把探测用的系统路径指到不存在的位置（等价于
// 容器内看不到宿主文件），只靠环境变量提供 DNS——若不优先用环境变量，
// 就会退化成公共 DNS，达不到"用宿主上游"的设计目标。
func TestWriteDNSFilesPrefersInjectedEnv(t *testing.T) {
	root := t.TempDir()
	// 模拟容器内：宿主 resolv.conf 路径均不可读。
	dir := t.TempDir()
	origSystemd, origEtc := systemdResolvedUpstream, hostResolvConf
	t.Cleanup(func() { systemdResolvedUpstream, hostResolvConf = origSystemd, origEtc })
	systemdResolvedUpstream = filepath.Join(dir, "absent-a")
	hostResolvConf = filepath.Join(dir, "absent-b")

	t.Setenv(EnvNetDNS, "9.9.9.9,149.112.112.112")
	if err := writeDNSFilesTo(root, "172.22.0.3", "172.22.0.1", "box"); err != nil {
		t.Fatalf("writeDNSFilesTo: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "etc", "resolv.conf"))
	if err != nil {
		t.Fatalf("读 resolv.conf: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "nameserver 9.9.9.9") {
		t.Errorf("应优先采用注入的 DNS，实际: %q", got)
	}
	if !strings.Contains(got, "nameserver 149.112.112.112") {
		t.Errorf("应保留注入的全部 DNS，实际: %q", got)
	}
	if strings.Contains(got, "1.1.1.1") {
		t.Errorf("不应退化成公共 DNS 兜底（环境变量已提供），实际: %q", got)
	}
}

// TestWriteDNSFilesFallsBackWithoutEnv 断言没有注入时仍有可用解析器。
//
// 环境变量缺失（老版本 engine、单测直调）时不能写出空 resolv.conf
// ——那会让容器完全无法解析域名，比用公共 DNS 更糟。
func TestWriteDNSFilesFallsBackWithoutEnv(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	origSystemd, origEtc := systemdResolvedUpstream, hostResolvConf
	t.Cleanup(func() { systemdResolvedUpstream, hostResolvConf = origSystemd, origEtc })
	systemdResolvedUpstream = filepath.Join(dir, "absent-a")
	hostResolvConf = filepath.Join(dir, "absent-b")
	t.Setenv(EnvNetDNS, "")

	if err := writeDNSFilesTo(root, "172.22.0.3", "172.22.0.1", "box"); err != nil {
		t.Fatalf("writeDNSFilesTo: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "etc", "resolv.conf"))
	if !strings.Contains(string(data), "nameserver ") {
		t.Errorf("无注入时也必须有可用解析器，实际: %q", data)
	}
}

// TestParseNameserverList 覆盖环境变量值的解析。
func TestParseNameserverList(t *testing.T) {
	cases := []struct{ in, want string }{
		{"8.8.8.8", "8.8.8.8"},
		{"8.8.8.8,1.1.1.1", "8.8.8.8,1.1.1.1"},
		{" 8.8.8.8 , 1.1.1.1 ", "8.8.8.8,1.1.1.1"},
		{"8.8.8.8,,1.1.1.1", "8.8.8.8,1.1.1.1"}, // 空项被丢弃
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		got := strings.Join(ParseNameserverList(tc.in), ",")
		if got != tc.want {
			t.Errorf("ParseNameserverList(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// 超过 3 个要被截断。
	if got := ParseNameserverList("1.1.1.1,2.2.2.2,3.3.3.3,4.4.4.4"); len(got) != maxNameservers {
		t.Errorf("应截断到 %d 个，实际 %d", maxNameservers, len(got))
	}
}
