// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/LiStudioorg/licore/internal/image"
)

// newConvertFixture 构造一个「docker 可用 + 导出一个最小 rootfs」的 fake，
// 让 Convert 能在没有任何 docker 的机器上走完整流程。
func newConvertFixture(t *testing.T) (*mockDocker, string) {
	t.Helper()
	return newConvertFixtureWith(t, `[{"Os":"linux","Config":{"Cmd":["/bin/sh"],"Env":["PATH=/usr/bin"]}}]`)
}

// newConvertFixtureWith 与 newConvertFixture 相同，但可指定 inspect 输出，
// 用于验证不同镜像元数据被正确翻译进 Boxfile。
func newConvertFixtureWith(t *testing.T, inspectJSON string) (*mockDocker, string) {
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
			"inspect": {out: []byte(inspectJSON)},
		},
	}
	return m, dir
}

// dockerSeq 返回 mock 收到的子命令序列（args[0]），便于断言调用顺序。
func dockerSeq(m *mockDocker) []string {
	seq := make([]string, 0, len(m.calls))
	for _, c := range m.calls {
		if len(c) > 0 {
			seq = append(seq, c[0])
		}
	}
	return seq
}

// loadImageConfig 读回产物 .licore 里的运行配置小对象。
//
// 刻意走真实的 image 解析器而不是解析中间产物：转换的价值就在于
// 「产出的 .licore 里元数据是对的」，只有读回最终产物才算验证到位。
func loadImageConfig(t *testing.T, path string) *image.Config {
	t.Helper()
	loaded, err := image.OpenFile(path)
	if err != nil {
		t.Fatalf("打开产物 %s: %v", path, err)
	}
	blob := fmt.Sprintf("blobs/sha256-%s", strings.TrimPrefix(loaded.Manifest.Config.Digest, "sha256:"))
	dst := filepath.Join(t.TempDir(), "config.json")
	if err := loaded.ExtractFile(blob, dst); err != nil {
		t.Fatalf("提取 config blob %s: %v", blob, err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := image.ParseConfig(data)
	if err != nil {
		t.Fatalf("解析 config: %v", err)
	}
	return cfg
}

// indexOf 返回子串 s 在 seq 中的位置，不存在返回 -1。
func indexOf(seq []string, s string) int {
	for i, v := range seq {
		if v == s {
			return i
		}
	}
	return -1
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

// -------- docker 调用序列 --------

// TestConvertDockerCallSequence 断言主流程按 pull → create → export → rmi 顺序
// 调用 docker，且 export 在 create 之后、rmi 在最后。
//
// 顺序错了会实际出问题：export 早于 create 拿不到容器，
// 清理临时容器晚于 export 会白占一个容器名额。
func TestConvertDockerCallSequence(t *testing.T) {
	m, dir := newConvertFixture(t)

	out := filepath.Join(dir, "a.licore")
	if _, err := Convert(context.Background(), &Options{
		Image: "alpine:3.20", OutPath: out, Docker: m, WorkDir: dir,
	}); err != nil {
		t.Fatalf("Convert: %v", err)
	}

	seq := dockerSeq(m)
	t.Logf("调用序列: %v", seq)
	want := []string{"pull", "create", "export", "rmi"}
	for _, w := range want {
		if indexOf(seq, w) < 0 {
			t.Fatalf("序列 %v 缺少 %q", seq, w)
		}
	}
	if !(indexOf(seq, "pull") < indexOf(seq, "create") &&
		indexOf(seq, "create") < indexOf(seq, "export") &&
		indexOf(seq, "export") < indexOf(seq, "rmi")) {
		t.Errorf("调用顺序不符合 pull<create<export<rmi: %v", seq)
	}

	// pull/create 必须显式带平台，否则异构机器上会拿到错架构。
	foundPlatform := false
	for _, c := range m.calls {
		if len(c) > 0 && c[0] == "pull" {
			for i, a := range c {
				if a == "--platform" && i+1 < len(c) && c[i+1] == "linux/"+runtime.GOARCH {
					foundPlatform = true
				}
			}
		}
	}
	if !foundPlatform {
		t.Errorf("pull 应显式指定 --platform linux/%s: %v", runtime.GOARCH, m.calls)
	}
}

// -------- NoCleanup / KeepDockerImage --------

// TestConvertKeepDockerImageSkipsRmi 断言 --keep-docker-image 时不调 rmi。
func TestConvertKeepDockerImageSkipsRmi(t *testing.T) {
	m, dir := newConvertFixture(t)

	if _, err := Convert(context.Background(), &Options{
		Image:           "alpine:3.20",
		OutPath:         filepath.Join(dir, "a.licore"),
		Docker:          m,
		WorkDir:         dir,
		KeepDockerImage: true,
	}); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if indexOf(dockerSeq(m), "rmi") >= 0 {
		t.Errorf("KeepDockerImage 时不应调用 rmi: %v", dockerSeq(m))
	}
}

// TestConvertNoCleanupKeepsWorkDir 断言 --no-cleanup 保留 work 临时目录，
// 便于用户排查失败的转换（默认必须清理干净）。
func TestConvertNoCleanupKeepsWorkDir(t *testing.T) {
	m, dir := newConvertFixture(t)

	if _, err := Convert(context.Background(), &Options{
		Image:     "alpine:3.20",
		OutPath:   filepath.Join(dir, "a.licore"),
		Docker:    m,
		WorkDir:   dir,
		NoCleanup: true,
	}); err != nil {
		t.Fatalf("Convert: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "licore-convert-") && !strings.Contains(e.Name(), "-out-") {
			kept = true
		}
	}
	if !kept {
		t.Errorf("--no-cleanup 应保留 work 目录，实际目录内容: %v", entries)
	}
}

// TestConvertDefaultCleansWorkDir 断言默认（不传 --no-cleanup）不残留 work 目录。
func TestConvertDefaultCleansWorkDir(t *testing.T) {
	m, dir := newConvertFixture(t)

	if _, err := Convert(context.Background(), &Options{
		Image: "alpine:3.20", OutPath: filepath.Join(dir, "a.licore"), Docker: m, WorkDir: dir,
	}); err != nil {
		t.Fatalf("Convert: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "licore-convert-") && !strings.Contains(e.Name(), "-out-") {
			t.Errorf("默认应清理 work 目录，却残留 %q", e.Name())
		}
	}
}

// -------- Boxfile 元数据正确性 --------

// TestConvertTranslatesMetadata 验证 inspect 里的运行配置被正确翻译进
// 产出的 .licore（读回最终产物，而不是只看中间 Boxfile 文本）。
func TestConvertTranslatesMetadata(t *testing.T) {
	inspect := `[{
	  "Os": "linux",
	  "Config": {
	    "Entrypoint": ["/docker-entrypoint.sh"],
	    "Cmd": ["nginx", "-g", "daemon off;"],
	    "Env": ["PATH=/usr/local/bin", "NGINX_VERSION=1.27.0"],
	    "WorkingDir": "/app",
	    "User": "1000:1000",
	    "ExposedPorts": {"80/tcp": {}, "443/tcp": {}},
	    "Volumes": {"/data": {}},
	    "Labels": {"maintainer": "team"}
	  }
	}]`
	m, dir := newConvertFixtureWith(t, inspect)

	out := filepath.Join(dir, "nginx.licore")
	if _, err := Convert(context.Background(), &Options{
		Image: "nginx:1.27-alpine", OutPath: out, Docker: m, WorkDir: dir,
	}); err != nil {
		t.Fatalf("Convert: %v", err)
	}

	cfg := loadImageConfig(t, out)
	if len(cfg.Entrypoint) != 1 || cfg.Entrypoint[0] != "/docker-entrypoint.sh" {
		t.Errorf("entrypoint = %v", cfg.Entrypoint)
	}
	if len(cfg.Cmd) != 3 || cfg.Cmd[0] != "nginx" || cfg.Cmd[2] != "daemon off;" {
		t.Errorf("cmd = %v", cfg.Cmd)
	}
	if cfg.Env["NGINX_VERSION"] != "1.27.0" || cfg.Env["PATH"] != "/usr/local/bin" {
		t.Errorf("env = %v", cfg.Env)
	}
	if cfg.WorkingDir != "/app" {
		t.Errorf("workingDir = %q", cfg.WorkingDir)
	}
	if cfg.User != "1000:1000" {
		t.Errorf("user = %q", cfg.User)
	}
	if len(cfg.Expose) != 2 {
		t.Errorf("expose = %v", cfg.Expose)
	}
	if len(cfg.Volumes) != 1 || cfg.Volumes[0] != "/data" {
		t.Errorf("volumes = %v", cfg.Volumes)
	}
	if cfg.Labels["maintainer"] != "team" {
		t.Errorf("labels = %v", cfg.Labels)
	}
}

// TestConvertMetadataRefDerivation 验证 --tag 之外的名字派生：
// docker 镜像引用（含 registry 前缀）应派生出正确的 name:version。
func TestConvertMetadataRefDerivation(t *testing.T) {
	cases := []struct{ image, wantRef string }{
		{"alpine:3.20", "alpine:3.20"},
		{"nginx:1.27-alpine", "nginx:1.27-alpine"},
		{"library/nginx:1.27", "library/nginx:1.27"},
	}
	for _, tc := range cases {
		t.Run(tc.image, func(t *testing.T) {
			m, dir := newConvertFixture(t)
			out := filepath.Join(dir, "a.licore")
			res, err := Convert(context.Background(), &Options{
				Image: tc.image, OutPath: out, Docker: m, WorkDir: dir,
			})
			if err != nil {
				t.Fatalf("Convert: %v", err)
			}
			if res.Ref != tc.wantRef {
				t.Errorf("Ref = %q, want %q", res.Ref, tc.wantRef)
			}
			loaded, err := image.OpenFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if got := loaded.Manifest.Ref(); got != tc.wantRef {
				t.Errorf("产物 index.json 的 Ref = %q, want %q", got, tc.wantRef)
			}
		})
	}
}

// TestConvertArchOverridesIndex 验证 --arch 写进产物 index.json。
func TestConvertArchOverridesIndex(t *testing.T) {
	m, dir := newConvertFixture(t)
	out := filepath.Join(dir, "a.licore")
	if _, err := Convert(context.Background(), &Options{
		Image: "alpine:3.20", Arch: "arm64", OutPath: out, Docker: m, WorkDir: dir,
	}); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	loaded, err := image.OpenFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Manifest.Architecture != "arm64" {
		t.Errorf("architecture = %q, want arm64", loaded.Manifest.Architecture)
	}
	// pull/create 也应带上目标平台。
	for _, c := range m.calls {
		if len(c) > 0 && (c[0] == "pull" || c[0] == "create") {
			ok := false
			for i, a := range c {
				if a == "--platform" && i+1 < len(c) && c[i+1] == "linux/arm64" {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s 应带 --platform linux/arm64: %v", c[0], c)
			}
		}
	}
}

// -------- 失败路径 --------

// TestConvertPullFailure 验证镜像不存在/拉取失败时返回 ErrDockerPull。
func TestConvertPullFailure(t *testing.T) {
	m, dir := newConvertFixture(t)
	m.responses["pull"] = mockResponse{err: fmt.Errorf("manifest unknown: no such image")}

	_, err := Convert(context.Background(), &Options{
		Image: "nosuch/image:v9", OutPath: filepath.Join(dir, "a.licore"), Docker: m, WorkDir: dir,
	})
	if err == nil {
		t.Fatal("拉取失败应报错")
	}
	if !errors.Is(err, ErrDockerPull) {
		t.Errorf("应为 ErrDockerPull，得到: %v", err)
	}
	if !strings.Contains(err.Error(), "nosuch/image:v9") {
		t.Errorf("错误应包含镜像名: %v", err)
	}
}

// TestConvertWindowsImageRejected 验证 Windows 容器镜像被明确拒绝。
func TestConvertWindowsImageRejected(t *testing.T) {
	m, dir := newConvertFixtureWith(t, `[{"Os":"windows","Config":{"Cmd":["cmd"]}}]`)

	_, err := Convert(context.Background(), &Options{
		Image:   "mcr.microsoft.com/windows:ltsc2022",
		OutPath: filepath.Join(dir, "w.licore"), Docker: m, WorkDir: dir,
	})
	if err == nil {
		t.Fatal("Windows 镜像应被拒绝")
	}
	if !errors.Is(err, ErrWindowsImage) {
		t.Errorf("应为 ErrWindowsImage，得到: %v", err)
	}
	// 拒绝要发生在 export 之前，避免白导出一遍大文件。
	if indexOf(dockerSeq(m), "export") >= 0 {
		t.Errorf("Windows 镜像应在 export 之前就被拒绝: %v", dockerSeq(m))
	}
}

// TestConvertEmptyRootfsRejected 验证导出的 rootfs 为空时报 ErrNoRootfs。
func TestConvertEmptyRootfsRejected(t *testing.T) {
	m, dir := newConvertFixture(t)
	m.exportTar = nil // RunToFile 会写出空文件

	_, err := Convert(context.Background(), &Options{
		Image: "empty:1", OutPath: filepath.Join(dir, "a.licore"), Docker: m, WorkDir: dir,
	})
	if err == nil {
		t.Fatal("空 rootfs 应报错")
	}
	if !errors.Is(err, ErrNoRootfs) {
		t.Errorf("应为 ErrNoRootfs，得到: %v", err)
	}
}

// TestConvertUnexpressibleCmdRejected 验证 CMD 含 ${} 时明确失败而不是静默丢弃。
func TestConvertUnexpressibleCmdRejected(t *testing.T) {
	m, dir := newConvertFixtureWith(t, `[{"Os":"linux","Config":{"Cmd":["/bin/sh","-c","echo ${HOME}"]}}]`)

	_, err := Convert(context.Background(), &Options{
		Image: "bad:1", OutPath: filepath.Join(dir, "a.licore"), Docker: m, WorkDir: dir,
	})
	if err == nil {
		t.Fatal("含 ${} 的 CMD 应报错")
	}
	if !errors.Is(err, ErrUnsupportedMeta) {
		t.Errorf("应为 ErrUnsupportedMeta，得到: %v", err)
	}
}
