// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package network

// 从宿主解析容器应使用的 DNS 服务器。
//
// ## 为什么不能直接写网桥网关
//
// 原实现把容器 `resolv.conf` 写成 `nameserver <网桥网关>`（172.22.0.1），
// 前提是"网关上跑着 LiCore 内置 DNS"。但**网关上并没有 DNS 服务**——
// 于是容器里所有域名解析都超时（真机实测 `nslookup google.com` →
// `connection timed out`），表现为"ping IP 通、域名不通"。
//
// ## 为什么不能直接照抄宿主 /etc/resolv.conf
//
// 现代发行版普遍用 systemd-resolved，宿主 `/etc/resolv.conf` 是
// **stub** 文件，内容是 `nameserver 127.0.0.53`——那是宿主**回环**上的
// stub 监听，容器有独立 netns，回环指向容器自己，**必然解析失败**。
// （真机实测本机正是这个形态：/etc/resolv.conf → stub-resolv.conf。）
//
// systemd-resolved 把真实上游写在 `/run/systemd/resolve/resolv.conf`，
// 探针必须识别并改读它。
//
// ## 探测顺序
//
//  1. `/run/systemd/resolve/resolv.conf`（存在则优先——它是 systemd-resolved
//     形态下**唯一**给出真实上游的地方）；
//  2. `/etc/resolv.conf`（非 systemd 形态的常规位置）；
//  3. 两者的结果里过滤掉本机回环地址（127.0.0.0/8、::1）——容器内不可达；
//  4. 全被过滤掉或文件都读不到 → 回退公共 DNS（1.1.1.1、8.8.8.8）。
//
// 最多取 maxNameservers 个：glibc 的解析器只读前 3 条，多写无益且会
// 让人误以为后面的也在用。

import (
	"bufio"
	"net"
	"os"
	"strings"
)

// EnvNetDNS 是宿主侧把探测到的 DNS 列表传给容器的环境变量名。
//
// 约定与 `LICORE_NET_*` 一致：由 engine/shim 在宿主侧探测后注入，
// 容器 init 读取并写进 resolv.conf，最后由 `envWithoutLiCore` 剥离，
// 不泄漏进用户命令的环境。
const EnvNetDNS = "LICORE_NET_DNS"

// maxNameservers 是写入容器 resolv.conf 的上限。
//
// glibc 的 resolver 只读取前 MAXNS(3) 条 nameserver，写更多不会生效，
// 反而让用户以为后面的也在用。
const maxNameservers = 3

// publicFallbackDNS 是探测不到任何可用上游时的回退。
//
// 选这两个：覆盖面广、长期稳定，且都是无需鉴权的公共递归解析器。
const publicFallbackDNS = "1.1.1.1,8.8.8.8"

// systemdResolvedUpstream 是 systemd-resolved 暴露真实上游的文件。
//
// 路径做成变量以便测试注入。
var systemdResolvedUpstream = "/run/systemd/resolve/resolv.conf"

// hostResolvConf 是常规的宿主 resolv.conf。
var hostResolvConf = "/etc/resolv.conf"

// ResolveNameservers 探测**宿主侧**可用的 DNS 服务器（供 engine/shim 调用）。
//
// **必须由宿主侧调用**：写入容器 resolv.conf 的代码跑在容器 init 进程里
// （已 pivot_root），那时 `/run/systemd/resolve/resolv.conf` 与 `/etc/resolv.conf`
// 看到的都是**容器的**文件系统——前者根本不存在，后者是容器自己的。
// 因此容器内探测**永远拿不到宿主上游**，只能走 fallback。
//
// 正确流程：engine/shim 在宿主侧调本函数 → 结果经 `LICORE_NET_DNS`
// 环境变量传入容器 → 容器侧用 ResolveNameservers 的入参版本写入文件。
//
// 导出版本供 internal/engine 与 internal/shim 使用。
func ResolveNameservers() []string {
	return resolveNameservers()
}

// ResolveNameservers 有两种语义，见上：宿主侧探测。容器侧请用
// FormatResolvConf 配合经环境变量传入的列表。

// resolveNameservers 探测容器应使用的 DNS 服务器列表（宿主侧视角）。
//
// **不返回空列表**：探测失败时回退公共 DNS，保证容器至少有可用解析器。
// 返回空列表会让容器陷入"完全无法解析域名"的状态，比用公共 DNS 更糟。
func resolveNameservers() []string {
	return resolveNameserversFrom(systemdResolvedUpstream, hostResolvConf)
}

// resolveNameserversFrom 是 resolveNameservers 的可注入实现。
//
// 优先读 systemd 的上游文件：在 systemd-resolved 形态下，
// `/etc/resolv.conf` 只给 `127.0.0.53`，读了也没用（会被回环过滤掉），
// 而 `/run/systemd/resolve/resolv.conf` 才有真实上游。
func resolveNameserversFrom(systemdPath, etcPath string) []string {
	// 1. systemd-resolved 的真实上游优先。
	if ns := parseNameservers(readFileOrEmpty(systemdPath)); len(ns) > 0 {
		return capNameservers(ns)
	}
	// 2. 回退常规 /etc/resolv.conf（非 systemd 形态）。
	if ns := parseNameservers(readFileOrEmpty(etcPath)); len(ns) > 0 {
		return capNameservers(ns)
	}
	// 3. 都拿不到 → 公共 DNS。
	return strings.Split(publicFallbackDNS, ",")
}

// readFileOrEmpty 读文件；失败返回空串（调用方按"没有可用上游"处理）。
//
// **刻意不返回错误**：探测是多级回退的一环，"这个文件读不到"是预期内的
// 分支（很多系统没有 systemd-resolved），不该中断整个探测流程。
func readFileOrEmpty(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// parseNameservers 从 resolv.conf 内容里提取可用的 nameserver 地址。
//
// 过滤规则：
//   - 只认 `nameserver <addr>` 行（大小写不敏感，容忍前导空白）；
//   - 丢弃回环地址（127.0.0.0/8、::1）——容器有独立 netns，
//     宿主回环在容器里指向容器自己，解析必然失败；
//   - 丢弃非 IP 的垃圾值；
//   - 去重，保持出现顺序（resolver 按顺序尝试，顺序有意义）。
func parseNameservers(content string) []string {
	var out []string
	seen := map[string]bool{}

	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// 跳过注释与空行。
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "nameserver") {
			continue
		}
		addr := fields[1]
		ip := net.ParseIP(addr)
		if ip == nil {
			continue // 非法地址，跳过
		}
		if ip.IsLoopback() {
			// 宿主回环在容器内不可达（容器有自己的 lo）。
			continue
		}
		if seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, addr)
	}
	return out
}

// capNameservers 截断到 maxNameservers 个。
func capNameservers(ns []string) []string {
	if len(ns) > maxNameservers {
		return ns[:maxNameservers]
	}
	return ns
}

// FormatResolvConf 生成容器 resolv.conf 内容。
//
// 只写 nameserver 行：不照抄宿主的 `search` / `options`——
// 宿主 search 域对容器内的应用往往没有意义（甚至是错误答案），
// 而 `options edns0 trust-ad` 是 stub 专用的，直连上游时不该带。
func FormatResolvConf(nameservers []string) string {
	return formatResolvConf(nameservers)
}

// formatResolvConf 是 FormatResolvConf 的内部实现（保留小写名便于既有测试）。
func formatResolvConf(nameservers []string) string {
	var b strings.Builder
	for _, ns := range nameservers {
		b.WriteString("nameserver ")
		b.WriteString(ns)
		b.WriteByte('\n')
	}
	return b.String()
}

// ParseNameserverList 解析 `LICORE_NET_DNS` 环境变量的值（逗号分隔）。
//
// 导出供 runtime 侧解码。空值返回 nil，调用方据此回退到"容器内探测 +
// 公共 DNS"（虽然宿主侧探测才是正路，但环境变量缺失时不能没有解析器）。
func ParseNameserverList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return capNameservers(out)
}
