// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecCommandNotFound 验证 exec 对未知容器给出清晰错误。
func TestExecCommandNotFound(t *testing.T) {
	var out bytes.Buffer
	root := NewRootCommand(&out, &out)
	dir := t.TempDir()
	root.SetArgs([]string{"exec", "ghost-container", "/bin/sh", "--data-dir", dir})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("未知容器应报错: %v", err)
	}
}

// TestExecCommandNotRunning 验证指向一个未运行的容器的 exec 明确报错（无需 root）。
func TestExecCommandNotRunning(t *testing.T) {
	var out bytes.Buffer
	root := NewRootCommand(&out, &out)
	dir := t.TempDir()
	// 写入一个运行状态 running=false 的容器目录。
	cdir := filepath.Join(dir, "containers", "abc123def456")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"configVersion":1,"id":"abc123def456","name":"web","imageRef":"x:v1","rootfs":"/tmp/x","restart":"no","cmd":["/bin/sh"],"createdAt":"2026-01-01T00:00:00Z"}`
	rt := `{"shimPid":1,"initPid":0,"running":false,"exitCode":-1}`
	if err := os.WriteFile(filepath.Join(cdir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "runtime.json"), []byte(rt), 0o644); err != nil {
		t.Fatal(err)
	}
	// --data-dir 是 exec **自己的** flag，必须写在容器名之前：
	// 容器名之后的参数一律原样作为容器内命令（与 run / Docker 一致）。
	// 这条曾依赖旧的"交错解析"行为（flag 可写在任意位置），修复后被正确地
	// 当成容器命令了——所以这里把 flag 挪到前面。
	root.SetArgs([]string{"exec", "--data-dir", dir, "abc123def456", "/bin/echo", "hi"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "未在运行") {
		t.Fatalf("未运行容器应报错: %v", err)
	}
}
