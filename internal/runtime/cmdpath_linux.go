// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// defaultContainerPath 是镜像未提供 PATH 时使用的默认值。
//
// 与 Docker 的默认值一致：Docker 在镜像 config 没有 PATH 时会注入这一串，
// 因此容器里 `sleep`、`nginx` 这类裸命令名才有意义。
const defaultContainerPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// resolveContainerCmd 把容器命令的第一个元素解析成可执行文件的路径。
//
// 为什么需要这一步：`execve(2)` 是**内核系统调用，从不做 PATH 查找**。
// 直接 syscall.Exec("sleep", ...) 会得到 ENOENT，即使容器里存在
// /bin/sleep 且 PATH 正确。PATH 解析是 userspace 的责任——Docker 靠 runc
// 在 exec 前做 LookPath，LiCore 必须自己做同样的事。
//
// 规则（与 Docker / POSIX shell 的习惯一致）：
//   - argv[0] 含 '/'（绝对或相对路径）→ 原样使用，不查 PATH；
//   - 不含 '/' → 按容器内 PATH 逐目录查找可执行文件；
//   - 都找不到 → 明确报错并列出查过的目录，绝不静默降级。
//
// 返回值是**用于 execve 的路径**；argv[0] 保持不变，程序看到的仍是原命令名。
func resolveContainerCmd(cmdline, env []string) (string, error) {
	if len(cmdline) == 0 {
		return "", fmt.Errorf("容器命令为空: %w", ErrBadConfig)
	}
	arg0 := cmdline[0]
	if strings.TrimSpace(arg0) == "" {
		return "", fmt.Errorf("容器命令为空: %w", ErrBadConfig)
	}
	// 含 '/' → 已经是路径（绝对或相对），交给 execve 按 cwd 解析。
	if strings.ContainsRune(arg0, '/') {
		return arg0, nil
	}

	path := envValue(env, "PATH")
	if strings.TrimSpace(path) == "" {
		// 镜像没提供 PATH：用 Docker 默认值兜底，否则裸命令名根本无法解析。
		slog.Debug("容器未提供 PATH，使用默认值", slog.String("path", defaultContainerPath))
		path = defaultContainerPath
	}

	var tried []string
	for _, dir := range strings.Split(path, ":") {
		// PATH 里的空项按 POSIX 语义表示当前目录（"" → "."）。
		if dir == "" {
			dir = "."
		}
		cand := filepath.Join(dir, arg0)
		if isExecutableFile(cand) {
			return cand, nil
		}
		tried = append(tried, cand)
	}
	return "", fmt.Errorf("找不到可执行文件 %q（PATH=%s）\n"+
		"  已查找：%s\n"+
		"  提示：命令名不含 '/' 时按容器内 PATH 查找；请确认该文件存在于镜像中",
		arg0, path, strings.Join(tried, " "))
}

// isExecutableFile 报告路径是否存在、是常规文件且任一执行位已置。
//
// 用 os.Stat（跟随符号链接）：PATH 查找应当认软链——Alpine 的 /bin/sleep
// 就是指向 /bin/busybox 的软链。
func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode()&0o111 != 0
}

// envValue 从 KEY=VALUE 列表里取 KEY 的值；不存在返回 ""。
func envValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			return v
		}
	}
	return ""
}
