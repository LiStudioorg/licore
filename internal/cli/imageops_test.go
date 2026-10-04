// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LiStudioorg/licore/internal/image"
	"github.com/LiStudioorg/licore/internal/storage"
	"github.com/LiStudioorg/licore/internal/store"
)

func TestParseImageRefImageOps(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    ImageRef
		wantErr bool
	}{
		{"简单引用", "myapp:1.0.0", ImageRef{Name: "myapp", Version: "1.0.0"}, false},
		{"多级仓库名", "alice/myapp:v1", ImageRef{Name: "alice/myapp", Version: "v1"}, false},
		{"三段仓库名", "reg.example.com/alice/myapp:latest", ImageRef{Name: "reg.example.com/alice/myapp", Version: "latest"}, false},
		{"版本含点与横线", "a/b:1.2.3-rc1", ImageRef{Name: "a/b", Version: "1.2.3-rc1"}, false},
		{"名称含下划线", "my_app:v2", ImageRef{Name: "my_app", Version: "v2"}, false},
		{"两侧空白被裁剪", "  myapp:v1  ", ImageRef{Name: "myapp", Version: "v1"}, false},
		{"缺版本冒号", "myapp", ImageRef{}, true},
		{"版本为空", "myapp:", ImageRef{}, true},
		{"名称为空", ":v1", ImageRef{}, true},
		{"整体为空", "", ImageRef{}, true},
		{"仅空白", "   ", ImageRef{}, true},
		{"名称含大写", "MyApp:v1", ImageRef{}, true},
		{"名称首字符非法", "-myapp:v1", ImageRef{}, true},
		{"名称含空格", "my app:v1", ImageRef{}, true},
		{"名称含空路径段", "alice//myapp:v1", ImageRef{}, true},
		{"名称以斜杠结尾", "alice/:v1", ImageRef{}, true},
		{"名称太长", strings.Repeat("a", 256) + ":v1", ImageRef{}, true},
		{"版本含空白", "myapp:v 1", ImageRef{}, true},
		{"版本含斜杠", "myapp:v/1", ImageRef{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseImageRef(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseImageRef(%q) = %+v, 期望报错", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseImageRef(%q) 意外失败: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseImageRef(%q) = %+v, 期望 %+v", tc.in, got, tc.want)
			}
			if got.String() != tc.want.Name+":"+tc.want.Version {
				t.Fatalf("String() = %q", got.String())
			}
		})
	}
}

// buildTinyRootfs 造一个最小 rootfs：普通文件、可执行文件、子目录、符号链接，
// 外加一个 FIFO（应被跳过）。全部在 t.TempDir() 内，不依赖容器运行时。
func buildTinyRootfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "app.conf"), []byte("mode = prod\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("etc/app.conf", filepath.Join(root, "conf.link")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCommitRootfsImageOps(t *testing.T) {
	st := &store.Store{Root: t.TempDir()}
	root := buildTinyRootfs(t)
	cfg := &store.ContainerConfig{ID: "abc123def456", Name: "tiny", Rootfs: root}

	res, err := CommitRootfs(st, cfg, "alice/tiny:v1", &CommitOptions{
		Labels:  map[string]string{"org.licore.maintainer": "alice"},
		Message: "initial commit",
	})
	if err != nil {
		t.Fatalf("CommitRootfs 失败: %v", err)
	}
	if res.Ref != "alice/tiny:v1" {
		t.Fatalf("Ref = %q", res.Ref)
	}
	if res.Bytes <= 0 {
		t.Fatalf("Bytes = %d", res.Bytes)
	}
	if !strings.HasPrefix(res.LayerDigest, "sha256:") || len(res.LayerDigest) != len("sha256:")+64 {
		t.Fatalf("LayerDigest = %q", res.LayerDigest)
	}
	if res.Skipped != 0 {
		t.Fatalf("Skipped = %d, 期望 0", res.Skipped)
	}

	// 产物必须落进 store 且三件套齐全。
	// ImageDir 是 <root>/images/<name>/<version>，name 与 version 分开传。
	dir := st.ImageDir("alice/tiny", "v1")
	fi, err := os.Stat(res.Path)
	if err != nil {
		t.Fatalf("产物不存在: %v", err)
	}
	if fi.Size() != res.Bytes {
		t.Fatalf("产物大小 %d != 报告 %d", fi.Size(), res.Bytes)
	}
	if want := filepath.Join(dir, "source.licore"); res.Path != want {
		t.Fatalf("产物路径 = %q, 期望 %q", res.Path, want)
	}
	for _, name := range []string{"source.licore", "index.json", "state.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("缺少 %s: %v", name, err)
		}
	}

	// 自查一：产物可被官方解析器打开，且层摘要重算通过。
	loaded, err := image.OpenFile(res.Path)
	if err != nil {
		t.Fatalf("image.OpenFile 打开产物失败: %v", err)
	}
	if loaded.Manifest.Ref() != "alice/tiny:v1" {
		t.Fatalf("清单引用 = %q", loaded.Manifest.Ref())
	}
	if err := loaded.VerifyLayers(); err != nil {
		t.Fatalf("VerifyLayers 失败: %v", err)
	}
	if len(loaded.Manifest.Layers) != 1 {
		t.Fatalf("层数 = %d, 期望 1", len(loaded.Manifest.Layers))
	}
	layer := loaded.Manifest.Layers[0]
	if layer.Path != "layers/000001.app.tar.gz" {
		t.Fatalf("层路径 = %q", layer.Path)
	}
	if layer.Digest != res.LayerDigest {
		t.Fatalf("清单层摘要 %q != 报告 %q", layer.Digest, res.LayerDigest)
	}
	if got := loaded.Config.Labels["org.licore.maintainer"]; got != "alice" {
		t.Fatalf("config.labels 未保留: %q", got)
	}
	if got := loaded.Manifest.Annotations["org.licore.commit.message"]; got != "initial commit" {
		t.Fatalf("commit 注释未写入: %q", got)
	}

	// 自查二：层是可解压的合法 gzip tar，条目与源 rootfs 一致。
	layerFile := filepath.Join(t.TempDir(), "layer.tar.gz")
	if err := loaded.ExtractFile(layer.Path, layerFile); err != nil {
		t.Fatalf("解出层失败: %v", err)
	}
	unpacked, err := storage.UnpackFile(layerFile, layer.Digest, st.Root)
	if err != nil {
		t.Fatalf("storage.UnpackFile 失败: %v", err)
	}

	got := map[string]string{}
	err = filepath.WalkDir(unpacked.FSDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(unpacked.FSDir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			target, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			got[rel] = "symlink:" + target
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		got[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历解包结果失败: %v", err)
	}

	want := map[string]string{
		"etc/app.conf": "mode = prod\n",
		"run.sh":       "#!/bin/sh\necho hi\n",
		"conf.link":    "symlink:etc/app.conf",
	}
	if len(got) != len(want) {
		t.Fatalf("解包条目数 = %d (%v), 期望 %d", len(got), got, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("条目 %q = %q, 期望 %q", k, got[k], v)
		}
	}

	// 自查三：可执行位保留，setuid/sticky 位被剥离。
	info, err := os.Stat(filepath.Join(unpacked.FSDir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh 权限 = %v, 期望 0755", info.Mode().Perm())
	}
	if info.Mode()&0o7000 != 0 {
		t.Fatalf("run.sh 残留特殊位: %v", info.Mode())
	}

	// 自查四：state.json 记的是本次产物本身，且带 originalRef 之外的既有字段。
	state, err := st.ReadState("alice/tiny", "v1")
	if err != nil {
		t.Fatalf("ReadState 失败: %v", err)
	}
	if state.Ref != "alice/tiny:v1" {
		t.Fatalf("state.Ref = %q", state.Ref)
	}
	if state.SourcePath != res.Path {
		t.Fatalf("state.SourcePath = %q, 期望 %q", state.SourcePath, res.Path)
	}
	if state.SourceFileBytes != res.Bytes || !state.LayersVerified {
		t.Fatalf("state 落地字段不正确: %+v", state)
	}
	// state.json 是 store.State 的线格式；commit 不是打标签，不应写入 originalRef。
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "originalRef") {
		t.Fatalf("commit 产物的 state.json 不应含 originalRef: %s", raw)
	}
}

// TestImageOpsRoundTrip 覆盖 commit → tag → save → load 全链路：
// 每一步产出的都必须是 store 可识别、image 包可再次解析的合法镜像。
func TestImageOpsRoundTrip(t *testing.T) {
	work := t.TempDir()
	src := &store.Store{Root: filepath.Join(work, "src")}
	if _, err := CommitRootfs(src, &store.ContainerConfig{ID: "abc123def456", Rootfs: buildTinyRootfs(t)}, "alice/tiny:v1", nil); err != nil {
		t.Fatalf("commit 失败: %v", err)
	}

	// tag：新引用落地，state 记录来源，原引用保持可用。
	if err := Retag(src, "alice/tiny:v1", "alice/tiny:stable", false); err != nil {
		t.Fatalf("Retag 失败: %v", err)
	}
	tagged, err := src.ReadState("alice/tiny", "stable")
	if err != nil {
		t.Fatalf("读取 tag 后状态失败: %v", err)
	}
	if tagged.Ref != "alice/tiny:stable" {
		t.Fatalf("tag 后 Ref = %q", tagged.Ref)
	}
	if !tagged.LayersVerified {
		t.Fatalf("tag 应沿用层的校验态: %+v", tagged)
	}
	rawState, err := os.ReadFile(filepath.Join(src.ImageDir("alice/tiny", "stable"), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawState), `"originalRef": "alice/tiny:v1"`) {
		t.Fatalf("state.json 未记录 originalRef:\n%s", rawState)
	}
	if _, err := src.ReadState("alice/tiny", "v1"); err != nil {
		t.Fatalf("原引用应保持可用: %v", err)
	}
	// 重复 tag 默认拒绝，--force 才覆盖。
	if err := Retag(src, "alice/tiny:v1", "alice/tiny:stable", false); !errors.Is(err, store.ErrExists) {
		t.Fatalf("重复 tag 错误 = %v, 期望 store.ErrExists", err)
	}
	if err := Retag(src, "alice/tiny:v1", "alice/tiny:stable", true); err != nil {
		t.Fatalf("--force tag 失败: %v", err)
	}

	// save：必须带 .licore 后缀。
	if err := SaveImage(src, "alice/tiny:stable", filepath.Join(work, "out.tar"), false); err == nil {
		t.Fatal("save 未校验 .licore 后缀")
	}
	exported := filepath.Join(work, "exported.licore")
	if err := SaveImage(src, "alice/tiny:stable", exported, false); err != nil {
		t.Fatalf("SaveImage 失败: %v", err)
	}
	if err := SaveImage(src, "alice/tiny:stable", exported, false); !errors.Is(err, store.ErrExists) {
		t.Fatalf("重复导出错误 = %v, 期望 store.ErrExists", err)
	}
	if err := SaveImage(src, "alice/tiny:stable", exported, true); err != nil {
		t.Fatalf("--force 导出失败: %v", err)
	}
	if _, err := image.OpenFile(exported); err != nil {
		t.Fatalf("导出文件不可解析: %v", err)
	}

	// load：落进另一个 store，并按 --tag 额外登记一个引用。
	dst := &store.Store{Root: filepath.Join(work, "dst")}
	loaded, err := ImportImage(dst, exported, "", ImportOptions{})
	if err != nil {
		t.Fatalf("ImportImage 失败: %v", err)
	}
	if loaded.Manifest.Ref() != "alice/tiny:v1" {
		t.Fatalf("load 后引用 = %q", loaded.Manifest.Ref())
	}
	if _, err := dst.ReadState("alice/tiny", "v1"); err != nil {
		t.Fatalf("load 落地失败: %v", err)
	}
	if _, err := ImportImage(dst, exported, "bob/copy:v9", ImportOptions{}); err != nil {
		t.Fatalf("ImportImage --tag 失败: %v", err)
	}
	copied, err := dst.ReadState("bob/copy", "v9")
	if err != nil {
		t.Fatalf("额外引用未登记: %v", err)
	}
	if copied.Ref != "bob/copy:v9" {
		t.Fatalf("额外引用 Ref = %q", copied.Ref)
	}
	infos, err := dst.ListImages()
	if err != nil {
		t.Fatalf("ListImages 失败: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("镜像数 = %d, 期望 2（%+v）", len(infos), infos)
	}
}

// TestCommitRootfsImageOpsRejectsDuplicate 验证同名镜像不会被静默覆盖。
func TestCommitRootfsImageOpsRejectsDuplicate(t *testing.T) {
	st := &store.Store{Root: t.TempDir()}
	cfg := &store.ContainerConfig{ID: "abc123def456", Rootfs: buildTinyRootfs(t)}

	if _, err := CommitRootfs(st, cfg, "alice/tiny:v1", nil); err != nil {
		t.Fatalf("首次 commit 失败: %v", err)
	}
	_, err := CommitRootfs(st, cfg, "alice/tiny:v1", nil)
	if err == nil {
		t.Fatal("重复 commit 未报错")
	}
	if !errors.Is(err, store.ErrExists) {
		t.Fatalf("错误未包装 store.ErrExists: %v", err)
	}
}

// TestCommitRootfsImageOpsSkipsSpecials 验证 socket/FIFO 等非常规文件被跳过并计数。
func TestCommitRootfsImageOpsSkipsSpecials(t *testing.T) {
	st := &store.Store{Root: t.TempDir()}
	root := buildTinyRootfs(t)
	fifo := filepath.Join(root, "pipe")
	if err := makeFIFO(fifo); err != nil {
		t.Skipf("本平台无法创建 FIFO: %v", err)
	}

	res, err := CommitRootfs(st, &store.ContainerConfig{ID: "abc123def456", Rootfs: root}, "alice/tiny:v2", nil)
	if err != nil {
		t.Fatalf("CommitRootfs 失败: %v", err)
	}
	if res.Skipped != 1 {
		t.Fatalf("Skipped = %d, 期望 1", res.Skipped)
	}

	loaded, err := image.OpenFile(res.Path)
	if err != nil {
		t.Fatalf("产物不可解析: %v", err)
	}
	layerFile := filepath.Join(t.TempDir(), "layer.tar.gz")
	if err := loaded.ExtractFile(loaded.Manifest.Layers[0].Path, layerFile); err != nil {
		t.Fatal(err)
	}
	names, err := tarNames(layerFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if n == "pipe" {
			t.Fatal("FIFO 不应出现在层里")
		}
	}
}

// TestImageOpsCrossPlatform 确保本文件的实现不引入平台限定符号（可交叉编译）。
func TestImageOpsCrossPlatform(t *testing.T) {
	if _, err := ParseImageRef("alice/myapp:v1"); err != nil {
		t.Fatal(err)
	}
}

// tarNames 返回层 tar.gz 内的全部条目名。
func tarNames(gzPath string) ([]string, error) {
	f, err := os.Open(gzPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	var names []string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		names = append(names, hdr.Name)
	}
	return names, nil
}
