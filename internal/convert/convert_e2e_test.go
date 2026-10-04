// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// newConvertFixture 构造一个「docker 可用 + 导出一个最小 rootfs」的 fake，
// 让 Convert 能在没有任何 docker 的机器上走完整流程。
func newConvertFixture(t *testing.T) (*mockDocker, string) {
	t.Helper()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "in.tar")
	if err := writeTar(tarPath, map[string]string{
		"etc/os-release": "NAME=alpine",
		"bin/sh":         "#!/bin/sh",
	}); err != nil {
		t.Fatal(err)
	}
	tarBytes, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockDocker{
		exportTar: tarBytes,
		responses: map[string]mockResponse{
			"version": {out: []byte("29.8.1\n")},
			"inspect": {out: []byte(`[{"Os":"linux","Config":{"Cmd":["/bin/sh"],"Env":["PATH=/usr/bin"]}}]`)},
		},
	}
	return m, dir
}

// TestConvertKeepsArtifactForImport 回归：--import 模式下 Convert 返回后
// 产物必须仍然存在。
//
// 历史缺陷：产物创建在 work 临时目录内，而 Convert 的 defer 会删掉 work；
// CLI 拿到 res.Path 时文件已被删除，`licore convert <image> --import` 100% 失败，
// 且错误是误导性的 "no such file"。这里直接断言返回后的可读性。
func TestConvertKeepsArtifactForImport(t *testing.T) {
	m, dir := newConvertFixture(t)

	res, err := Convert(context.Background(), &Options{
		Image:       "alpine:3.20",
		ImportImage: true, // 不写 OutPath：走「留给调用方导入」这条路径
		Docker:      m,
		WorkDir:     dir,
	})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if res.Path == "" {
		t.Fatal("ImportImage 模式下 Path 不能为空，否则调用方无从导入")
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("Convert 返回后产物必须仍可读（调用方要靠它导入）: %v", err)
	}
	if res.Bytes <= 0 {
		t.Errorf("Bytes = %d, 应为正数", res.Bytes)
	}
	// 清理是调用方的责任。
	_ = os.RemoveAll(filepath.Dir(res.Path))
}

// TestConvertWritesOutPath 覆盖 -o 路径：产物写到用户指定位置且可读。
func TestConvertWritesOutPath(t *testing.T) {
	m, dir := newConvertFixture(t)

	out := filepath.Join(dir, "out", "alpine.licore")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Convert(context.Background(), &Options{
		Image:   "alpine:3.20",
		OutPath: out,
		Docker:  m,
		WorkDir: dir,
	})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if res.Path != out {
		t.Errorf("Path = %q, want %q", res.Path, out)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("产物不存在: %v", err)
	}
	if res.Ref != "alpine:3.20" {
		t.Errorf("Ref = %q, want alpine:3.20", res.Ref)
	}
}

// TestConvertOutPathSurvivesWorkCleanup 确认 -o 的产物不在临时目录里，
// 因此不随 work 一起被清理。
func TestConvertOutPathSurvivesWorkCleanup(t *testing.T) {
	m, dir := newConvertFixture(t)

	out := filepath.Join(dir, "keep", "a.licore")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Convert(context.Background(), &Options{
		Image: "alpine:3.20", OutPath: out, Docker: m, WorkDir: dir,
	}); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("-o 产物不应被临时目录清理影响: %v", err)
	}
}
