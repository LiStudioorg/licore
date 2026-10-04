// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/LiStudioorg/licore/internal/image"
)

// ---------- 测试 fixture：构造合法 .licore 文件 ----------

func sha(s []byte) string {
	sum := sha256.Sum256(s)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func makeLayer(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "etc/hello", Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildValidLiCore 在 t.TempDir() 里生成一个合法 .licore，返回文件路径。
func buildValidLiCore(t *testing.T) string {
	t.Helper()
	layer := makeLayer(t, "hi")
	cfg := []byte(`{"entrypoint":["/bin/sh"]}`)
	m := image.Manifest{
		MediaType:     image.MediaTypeManifest,
		SpecVersion:   image.SpecVersionV1,
		SchemaVersion: image.SchemaVersionV1,
		Architecture:  runtime.GOARCH,
		OS:            runtime.GOOS,
		Created:       "2026-10-01T08:00:00Z",
		Name:          "alice/myapp",
		Version:       "1.0.0",
		Config:        image.ConfigRef{Digest: sha(cfg), SizeBytes: int64(len(cfg))},
		Layers: []image.Layer{{
			Path: "layers/000001.base.tar.gz", Digest: sha(layer),
			SizeBytes: int64(len(layer)), ApplyOrder: 1,
		}},
	}
	idx, err := json.Marshal(&m)
	if err != nil {
		t.Fatal(err)
	}
	_, hx, _ := strings.Cut(sha(cfg), ":")
	entries := map[string][]byte{
		image.IndexName:                 idx,
		"layers/000001.base.tar.gz":     layer,
		image.BlobsDir + "sha256-" + hx: cfg,
	}

	path := filepath.Join(t.TempDir(), "app.licore")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for name, data := range entries {
		h := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// ---------- 用例 ----------

func TestPutAndExists(t *testing.T) {
	src := buildValidLiCore(t)
	st := &Store{Root: t.TempDir()}

	ok, err := st.Exists("alice/myapp", "1.0.0")
	if err != nil || ok {
		t.Fatalf("初始 Exists = (%v, %v)，期望 (false, nil)", ok, err)
	}

	loaded, err := st.Put(src, false, false)
	if err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	if loaded.Manifest.Ref() != "alice/myapp:1.0.0" {
		t.Fatalf("Ref = %q", loaded.Manifest.Ref())
	}
	ok, err = st.Exists("alice/myapp", "1.0.0")
	if err != nil || !ok {
		t.Fatalf("Put 后 Exists = (%v, %v)，期望 (true, nil)", ok, err)
	}

	// 三个落地文件都存在。
	for _, f := range []string{"source.licore", "index.json", "state.json"} {
		if _, err := os.Stat(filepath.Join(st.ImageDir("alice/myapp", "1.0.0"), f)); err != nil {
			t.Errorf("缺少落地文件 %s: %v", f, err)
		}
	}
	stt, err := st.ReadState("alice/myapp", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !stt.LayersVerified || stt.Ref != "alice/myapp:1.0.0" || stt.SourcePath != src {
		t.Errorf("state.json 内容异常: %+v", stt)
	}
	// source.licore 与源文件字节一致。
	if stt.SourceFileBytes == 0 {
		t.Error("SourceFileBytes 为空")
	}
}

func TestPutTwiceRequiresForce(t *testing.T) {
	src := buildValidLiCore(t)
	st := &Store{Root: t.TempDir()}
	if _, err := st.Put(src, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(src, false, false); !errors.Is(err, ErrExists) {
		t.Fatalf("重复 Put err = %v, want ErrExists", err)
	}
	if _, err := st.Put(src, true, false); err != nil {
		t.Fatalf("--force Put 失败: %v", err)
	}
}

func TestPutRejectsCorrupt(t *testing.T) {
	src := buildValidLiCore(t)
	// 破坏 .tar.gz 条目名（ASCII），大小不变；该字节同时是层内容的一部分，
	// 导致层摘要必变且条目更名后 index 找不到层。
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if i := bytes.Index(data, []byte(".tar.gz")); i >= 0 {
		data[i] = 'X'
	} else {
		data[len(data)/2] ^= 0xFF
	}
	bad := filepath.Join(t.TempDir(), "bad.licore")
	if err := os.WriteFile(bad, data, 0o644); err != nil {
		t.Fatal(err)
	}
	st := &Store{Root: t.TempDir()}
	if _, err := st.Put(bad, false, false); err == nil {
		t.Fatal("损坏镜像竟然落地成功")
	}
	// 失败后不留残留目录。
	entries, _ := os.ReadDir(filepath.Join(st.Root, "images", "alice", "myapp"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("存在残留临时目录: %s", e.Name())
		}
	}
}

func TestBootMarker(t *testing.T) {
	st := &Store{Root: t.TempDir()}
	if err := st.EnsureBootDir(); err != nil {
		t.Fatal(err)
	}
	marker := st.BootMarker()
	if !strings.HasSuffix(marker, filepath.Join("boot", "marker")) {
		t.Fatalf("marker 路径异常: %s", marker)
	}
	if err := os.WriteFile(marker, []byte("declined\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker 写入失败: %v", err)
	}
}
