// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件下发"容器 1 号进程以什么身份、在哪个目录启动"。
//
// **为什么用环境变量而不是 runtime.Config 字段**：`runtime.Config` 是
// 《冻结接口》里登记的稳定契约，加字段需先提 issue。而 LICORE_NET_* /
// LICORE_MOUNT_* / LICORE_CGROUP_ID / LICORE_CAPS_* 已经建立了"装配参数经
// 环境变量从 engine/shim 传给 init"的既定约定，本组变量沿用同一约定。
//
// envWithoutLiCore 统一剥离 LICORE_* 前缀，因此这些变量不会泄漏进容器里
// 用户命令的环境中。

// envWorkdir / envUser 是 engine/shim → init 的私有信道。
const (
	envWorkdir = "LICORE_WORKDIR"
	envUser    = "LICORE_USER"
)

// WorkdirEnv 生成设置容器 1 号进程工作目录的环境变量。
// workdir 为空表示不改（沿用 runtime 的默认行为，即根目录 /）。
func WorkdirEnv(workdir string) []string {
	if strings.TrimSpace(workdir) == "" {
		return nil
	}
	return []string{envWorkdir + "=" + workdir}
}

// UserEnv 生成设置容器 1 号进程 uid/gid 的环境变量。
// user 形如 "1000" 或 "1000:1000"；为空表示不改（沿用 root）。
func UserEnv(user string) []string {
	if strings.TrimSpace(user) == "" {
		return nil
	}
	return []string{envUser + "=" + user}
}

// ParseUserSpec 解析 "uid[:gid]" 形式的用户规格。
//
// gid 省略时**沿用 uid**（与 Docker 的 `--user 1000` 语义一致：只给 uid 时
// 补同一个数字作为 gid，而不是留 0）。uid 与 gid 都必须是十进制非负整数；
// 名字形式的用户（如 "nobody"）需要查 /etc/passwd，当前不支持——显式报错，
// 不静默忽略，否则用户会得到一个"以 root 跑着、却以为换了身份"的容器。
func ParseUserSpec(spec string) (uid, gid int, err error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return 0, 0, fmt.Errorf("用户规格为空")
	}
	uidStr, gidStr, hasGid := strings.Cut(s, ":")
	uid, err = parseID(uidStr)
	if err != nil {
		return 0, 0, fmt.Errorf("非法 uid %q: %w", uidStr, err)
	}
	if !hasGid {
		return uid, uid, nil
	}
	gid, err = parseID(gidStr)
	if err != nil {
		return 0, 0, fmt.Errorf("非法 gid %q: %w", gidStr, err)
	}
	return uid, gid, nil
}

// parseID 解析单个非负十进制 ID。
func parseID(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("为空")
	}
	// 拒绝非纯数字（strconv.Atoi 会接受 "+1"/"-1"，这里明确不接受）。
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("含非数字字符")
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("不可为负")
	}
	return n, nil
}

// workdirFromEnv 从环境变量切片读取工作目录；不存在返回 ""。
func workdirFromEnv(env []string) string {
	prefix := envWorkdir + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	return ""
}

// userFromEnv 从环境变量切片读取用户规格；不存在返回 ""。
func userFromEnv(env []string) string {
	prefix := envUser + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	return ""
}
