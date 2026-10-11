// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/LiStudioorg/licore/internal/network"
	"github.com/LiStudioorg/licore/internal/store"
)

func TestSplitImageRef(t *testing.T) {
	n, v, err := splitImageRef("alice/myapp:v1")
	if err != nil || n != "alice/myapp" || v != "v1" {
		t.Fatalf("name 含斜杠解析错误: %s %s %v", n, v, err)
	}
	if _, _, err := splitImageRef("noversion"); err == nil {
		t.Fatal("缺 version 应报错")
	}
	if _, _, err := splitImageRef(":v1"); err == nil {
		t.Fatal("空 name 应报错")
	}
	if _, _, err := splitImageRef("name:"); err == nil {
		t.Fatal("空 version 应报错")
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2"}, []string{"B=9", "C=3"})
	want := []string{"A=1", "B=9", "C=3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mergeEnv = %v 期望 %v", got, want)
	}
}

func TestAutoNameUnique(t *testing.T) {
	st := &store.Store{Root: t.TempDir()}
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		id, _ := store.NewContainerID()
		n := autoName(st, id)
		if n == "" {
			t.Fatal("autoName 返回空")
		}
		seen[n] = true
	}
	if len(seen) < 2 {
		t.Fatalf("20 次生成的名字过于单一: %v", seen)
	}
}

func TestRunImageNotFound(t *testing.T) {
	st := &store.Store{Root: t.TempDir()}
	_, err := Run(context.Background(), st, &RunSpec{
		ImageRef: "ghost:v1", Restart: store.RestartNo, Cmd: []string{"/bin/sh"},
	})
	if !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("期望 ErrImageNotFound，实得 %v", err)
	}
}

func TestRunWritesContainerState(t *testing.T) {
	st := &store.Store{Root: t.TempDir()}
	imgDir := st.ImageDir("demo", "v1")
	if err := writeFakeImage(t, imgDir); err != nil {
		t.Fatal(err)
	}
	// fake 启动：不 fork 进程，只标记结果。
	old := prepareAndStartFn
	prepareAndStartFn = func(_ context.Context, _ *store.Store, cfg *store.ContainerConfig, r *RunResult) error {
		r.ExitCode = 0
		return nil
	}
	t.Cleanup(func() { prepareAndStartFn = old })
	// 本用例只关心状态落盘，与网络无关：挡掉真实网络接入，避免非 root /
	// 无 nft 环境下被拖垮（网络本身由 network 包与 TestWireNetwork* 覆盖）。
	stubNetwork(t)

	res, err := Run(context.Background(), st, &RunSpec{
		ImageRef: "demo:v1", Name: "web", Restart: store.RestartAlways,
		Cmd: []string{"/bin/sh"}, Env: []string{"K=V"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.FindContainer("web")
	if err != nil {
		t.Fatalf("容器状态未落盘: %v", err)
	}
	if got.ImageRef != "demo:v1" || got.Restart != store.RestartAlways ||
		got.Hostname != "web" || len(got.Cmd) != 1 || res.Container.ID != got.ID {
		t.Fatalf("配置字段错误: %+v", got)
	}
	if res.ShortID != got.ID {
		t.Fatalf("ShortID 错误: %s", res.ShortID)
	}
}

func TestRunNameConflict(t *testing.T) {
	st := &store.Store{Root: t.TempDir()}
	if err := writeFakeImage(t, st.ImageDir("demo", "v1")); err != nil {
		t.Fatal(err)
	}
	old := prepareAndStartFn
	prepareAndStartFn = func(context.Context, *store.Store, *store.ContainerConfig, *RunResult) error { return nil }
	t.Cleanup(func() { prepareAndStartFn = old })
	stubNetwork(t)

	spec := &RunSpec{ImageRef: "demo:v1", Name: "web", Restart: store.RestartNo, Cmd: []string{"/x"}}
	if _, err := Run(context.Background(), st, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), st, spec); !errors.Is(err, store.ErrContainerExists) {
		t.Fatalf("同名容器应报 ErrContainerExists，实得 %v", err)
	}
}

// stubNetwork 把网络接入替换为 no-op，供"与网络无关"的 run 用例使用。
// 真实网络接入需要 root + nft/iptables；这些用例不验证网络，
// 不应因此依赖特权环境（也避免在有特权的机器上误建网桥/veth）。
func stubNetwork(t *testing.T) {
	t.Helper()
	orig := wireNetworkFn
	wireNetworkFn = func(*store.Store, *store.ContainerConfig, string, string, []*network.PortMapping) error {
		return nil
	}
	t.Cleanup(func() { wireNetworkFn = orig })
}

// writeFakeImage 在 dir 里生成最小合法 .licore（单层 + config blob）并配
// state.json，让 run 流程在 fake 启动器下完整走通（不真解包层）。
func writeFakeImage(t *testing.T, dir string) error {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// 层内容：bin/echo 占位（0 字节可执行）
	var lb bytes.Buffer
	gw := gzip.NewWriter(&lb)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755, Format: tar.FormatUSTAR})
	_ = tw.WriteHeader(&tar.Header{Name: "bin/sh", Size: 0, Mode: 0o755, Format: tar.FormatUSTAR})
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	layer := lb.Bytes()
	dh := sha256.Sum256(layer)

	cfg, _ := json.Marshal(map[string]any{"entrypoint": []string{"/bin/sh"}})
	ch := sha256.Sum256(cfg)

	idx := map[string]any{
		"mediaType":     "application/x.licore.manifest+json",
		"specVersion":   "licore/image-spec/v1",
		"schemaVersion": 1,
		"architecture":  runtime.GOARCH,
		"os":            runtime.GOOS,
		"created":       "2026-10-01T10:00:00Z",
		"name":          "demo",
		"version":       "v1",
		"config":        map[string]any{"digest": "sha256:" + hex.EncodeToString(ch[:]), "sizeBytes": len(cfg)},
		"layers":        []map[string]any{{"path": "layers/000001.base.tar.gz", "digest": "sha256:" + hex.EncodeToString(dh[:]), "sizeBytes": len(layer), "applyOrder": 1}},
	}
	idxB, _ := json.MarshalIndent(idx, "", " ")

	f, err := os.Create(filepath.Join(dir, "source.licore"))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	ow := tar.NewWriter(f)
	put := func(name string, b []byte) error {
		if err := ow.WriteHeader(&tar.Header{Name: name, Size: int64(len(b)), Mode: 0o644, Format: tar.FormatUSTAR}); err != nil {
			return err
		}
		_, err := ow.Write(b)
		return err
	}
	if err := put("index.json", idxB); err != nil {
		return err
	}
	if err := put("layers/000001.base.tar.gz", layer); err != nil {
		return err
	}
	if err := put("blobs/sha256-"+hex.EncodeToString(ch[:]), cfg); err != nil {
		return err
	}
	if err := ow.Close(); err != nil {
		return err
	}
	stByte, _ := json.Marshal(map[string]any{"ref": "demo:v1"})
	return os.WriteFile(filepath.Join(dir, "state.json"), stByte, 0o644)
}

// TestBuildRootfsMergesImage 验证 BuildRootfs 能把假镜像的层合并到 targetDir。
func TestBuildRootfsMergesImage(t *testing.T) {
	st := &store.Store{Root: t.TempDir()}
	imgDir := st.ImageDir("demo", "v1")
	if err := writeFakeImage(t, imgDir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "rootfs")
	if err := BuildRootfs(st, "demo:v1", target); err != nil {
		t.Fatalf("BuildRootfs: %v", err)
	}
	// 假镜像层含 bin/sh 目录。
	if fi, err := os.Stat(filepath.Join(target, "bin", "sh")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("合并后应存在 bin/sh: %v (mode=%v)", err, fi)
	}
}
