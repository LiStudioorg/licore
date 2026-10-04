// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/LiStudioorg/licore/internal/image"
	"github.com/LiStudioorg/licore/internal/storage"
)

// mustParse 解析 Boxfile 文本，失败即终止用例。
func mustParse(t *testing.T, src string) *Boxfile {
	t.Helper()
	src = strings.ReplaceAll(src, "FROM scratch", "FROM licore/scratch:v1")
	bf, err := ParseBoxfile([]byte(src))
	if err != nil {
		t.Fatalf("ParseBoxfile(%q) 失败: %v", src, err)
	}
	return bf
}

// writeFile 在 dir 下写一个带权限位的文件，自动创建父目录。
func writeFile(t *testing.T, path, content string, perm fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("Chmod(%s): %v", path, err)
	}
}

// baseOpts 组装一份最小可用的 scratch 构建参数（OutPath 落在 t.TempDir 内）。
func baseOpts(t *testing.T, dir, boxfile string) *Options {
	t.Helper()
	return &Options{
		ContextDir: dir,
		Boxfile:    mustParse(t, boxfile),
		OutPath:    filepath.Join(dir, "out.licore"),
	}
}

// unpackAllLayers 把产物里所有层解进内容寻址存储，返回每层 fs 目录的宿主路径。
// 返回的路径与 loaded.Manifest.Layers 一一对应。
func unpackAllLayers(t *testing.T, imgPath string, storeRoot string) []string {
	t.Helper()
	loaded, err := image.OpenFile(imgPath)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", imgPath, err)
	}
	var paths []string
	for i, la := range loaded.Manifest.Layers {
		tmp := filepath.Join(t.TempDir(), fmt.Sprintf("layer-%d.tar.gz", i+1))
		if err := loaded.ExtractFile(la.Path, tmp); err != nil {
			t.Fatalf("ExtractFile(%s): %v", la.Path, err)
		}
		got, err := storage.UnpackFile(tmp, la.Digest, storeRoot)
		if err != nil {
			t.Fatalf("第 %d 层 %s 不是合法 tar.gz: %v", i+1, la.Path, err)
		}
		paths = append(paths, got.FSDir)
	}
	return paths
}

// TestScratchBuildCreatesValidImage 覆盖 scratch 构建：产物必须能被自家
// image.OpenFile 打开、层摘要可重算、COPY 文件落在拓扑正确的层里。
func TestScratchBuildCreatesValidImage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hello.txt"), "hello licore\n", 0o644)
	if err := os.Symlink("hello.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	opts := baseOpts(t, dir, `
FROM scratch
COPY hello.txt /app/hello.txt
COPY link.txt /app/link.txt
WORKDIR /app
`)
	opts.OutPath = filepath.Join(dir, "demo.licore")

	res, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Result 字段自洽。
	fi, err := os.Stat(res.Path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", res.Path, err)
	}
	if res.Bytes != fi.Size() {
		t.Errorf("Result.Bytes=%d，磁盘上=%d", res.Bytes, fi.Size())
	}
	if res.LayerCount != 1 {
		t.Errorf("Result.LayerCount=%d，期望 1", res.LayerCount)
	}
	if res.Name != "demo" || res.Version != "latest" {
		t.Errorf("Result 引用=%s:%s，期望 demo:latest", res.Name, res.Version)
	}
	if !strings.HasPrefix(res.LayerDigest, "sha256:") || len(res.LayerDigest) != len("sha256:")+64 {
		t.Errorf("LayerDigest=%q 形状非法", res.LayerDigest)
	}

	// 自检：产物必须能被 image.OpenFile 打开，且层摘要可全量重算。
	loaded, err := image.OpenFile(res.Path)
	if err != nil {
		t.Fatalf("image.OpenFile(%s): %v", res.Path, err)
	}
	if err := loaded.VerifyLayers(); err != nil {
		t.Fatalf("VerifyLayers: %v", err)
	}
	if got := loaded.Manifest.Layers[0]; got.Digest != res.LayerDigest {
		t.Errorf("index 层摘要=%s，Result.LayerDigest=%s", got.Digest, res.LayerDigest)
	}
	if loaded.Manifest.OS != runtime.GOOS || loaded.Manifest.Architecture != runtime.GOARCH {
		t.Errorf("平台=%s/%s，期望 %s/%s",
			loaded.Manifest.OS, loaded.Manifest.Architecture, runtime.GOOS, runtime.GOARCH)
	}

	// COPY 的内容必须能从层里还原出来。
	layers := unpackAllLayers(t, res.Path, filepath.Join(dir, "unpack"))
	got, err := os.ReadFile(filepath.Join(layers[0], "app", "hello.txt"))
	if err != nil {
		t.Fatalf("读取层内 app/hello.txt: %v", err)
	}
	if string(got) != "hello licore\n" {
		t.Errorf("层内 hello.txt=%q，期望 %q", got, "hello licore\n")
	}
	link, err := os.Readlink(filepath.Join(layers[0], "app", "link.txt"))
	if err != nil || link != "hello.txt" {
		t.Errorf("层内 link.txt 符号链接=%q, err=%v，期望 hello.txt", link, err)
	}
}

// TestConfigBlobCarriesInstructions 断言 ENV / WORKDIR / ENTRYPOINT / CMD /
// EXPOSE / VOLUME / LABEL / USER 都落进 config blob，且 blob 摘要与 index 一致。
func TestConfigBlobCarriesInstructions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app"), "#!/bin/sh\n", 0o755)

	opts := baseOpts(t, dir, `
FROM scratch
COPY app /usr/bin/app
ENV LANG=C.UTF-8
ENV PATH /usr/bin
WORKDIR /srv/app
ENTRYPOINT ["/usr/bin/app"]
CMD ["--serve", "--port=8080"]
EXPOSE 8080/tcp
EXPOSE 9090/udp
VOLUME /data
LABEL org.licore.maintainer=alice
USER 1000:1000
`)
	opts.Labels = map[string]string{"org.licore.extra": "lead"}
	res, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	_ = res

	blob := readConfigBlob(t, res.Path)
	cfg, err := image.ParseConfig(blob)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.WorkingDir != "/srv/app" {
		t.Errorf("workingDir=%q，期望 /srv/app", cfg.WorkingDir)
	}
	if cfg.User != "1000:1000" {
		t.Errorf("user=%q，期望 1000:1000", cfg.User)
	}
	if got := cfg.Env["LANG"]; got != "C.UTF-8" {
		t.Errorf("env[LANG]=%q，期望 C.UTF-8", got)
	}
	if got := cfg.Env["PATH"]; got != "/usr/bin" {
		t.Errorf("env[PATH]=%q，期望 /usr/bin", got)
	}
	if len(cfg.Entrypoint) != 1 || cfg.Entrypoint[0] != "/usr/bin/app" {
		t.Errorf("entrypoint=%v，期望 [/usr/bin/app]", cfg.Entrypoint)
	}
	if len(cfg.Cmd) != 2 || cfg.Cmd[1] != "--port=8080" {
		t.Errorf("cmd=%v，期望 [--serve --port=8080]", cfg.Cmd)
	}
	if len(cfg.Expose) != 2 || cfg.Expose[0] != "8080/tcp" || cfg.Expose[1] != "9090/udp" {
		t.Errorf("expose=%v，期望 [8080/tcp 9090/udp]", cfg.Expose)
	}
	if len(cfg.Volumes) != 1 || cfg.Volumes[0] != "/data" {
		t.Errorf("volumes=%v，期望 [/data]", cfg.Volumes)
	}
	if cfg.Labels["org.licore.maintainer"] != "alice" {
		t.Errorf("labels[org.licore.maintainer]=%q，期望 alice", cfg.Labels["org.licore.maintainer"])
	}
	if cfg.Labels["org.licore.extra"] != "lead" {
		t.Errorf("labels[org.licore.extra]=%q，期望 lead（Options.Labels 未合并）", cfg.Labels["org.licore.extra"])
	}

	// WORKDIR 必须在 rootfs 里真实建出来。
	layers := unpackAllLayers(t, res.Path, filepath.Join(dir, "unpack"))
	if fi, err := os.Stat(filepath.Join(layers[0], "srv", "app")); err != nil || !fi.IsDir() {
		t.Errorf("层内 srv/app 不是目录: %v", err)
	}
	// COPY 的 755 权限必须保留。
	if fi, err := os.Stat(filepath.Join(layers[0], "usr", "bin", "app")); err != nil {
		t.Errorf("层内 usr/bin/app 缺失: %v", err)
	} else if fi.Mode().Perm() != 0o755 {
		t.Errorf("层内 usr/bin/app 权限=%v，期望 0755", fi.Mode().Perm())
	}
}

// readConfigBlob 从产物中取出 config blob 的原始字节。
func readConfigBlob(t *testing.T, imgPath string) []byte {
	t.Helper()
	loaded, err := image.OpenFile(imgPath)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", imgPath, err)
	}
	algo, sum, _ := strings.Cut(loaded.Manifest.Config.Digest, ":")
	name := image.BlobsDir + algo + "-" + sum
	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatalf("Open(%s): %v", imgPath, err)
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("产物缺少条目 %s", name)
		}
		if err != nil {
			t.Fatalf("扫描归档: %v", err)
		}
		if hdr.Name != name {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("读取 %s: %v", name, err)
		}
		if int64(len(data)) != loaded.Manifest.Config.SizeBytes {
			t.Errorf("config blob 实际 %d 字节，index 声明 %d", len(data), loaded.Manifest.Config.SizeBytes)
		}
		got := sha256.Sum256(data)
		if "sha256:"+hex.EncodeToString(got[:]) != loaded.Manifest.Config.Digest {
			t.Errorf("config blob 摘要与 index 不符")
		}
		return data
	}
}

// TestBuildOnBasePreservesBaseLayers 覆盖"在基础镜像之上构建"：
// 基础层必须逐字节搬运、摘要 / 大小 / applyOrder 全部保持不变，且只追加一层。
func TestBuildOnBasePreservesBaseLayers(t *testing.T) {
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "base")
	writeFile(t, filepath.Join(baseDir, "base.txt"), "from base\n", 0o644)

	// 基础镜像的文件名决定镜像引用（demo.licore → demo:latest），
	// 子 Boxfile 的 FROM 必须与之一致。
	baseOpts := baseOpts(t, baseDir, `
FROM scratch
COPY base.txt /etc/base.txt
`)
	baseOpts.OutPath = filepath.Join(baseDir, "demo.licore")
	baseRes, err := Build(context.Background(), baseOpts)
	if err != nil {
		t.Fatalf("构建基础镜像: %v", err)
	}
	baseLoaded, err := image.OpenFile(baseRes.Path)
	if err != nil {
		t.Fatalf("OpenFile(基础): %v", err)
	}
	if baseRes.Name != "demo" || baseRes.Version != "latest" {
		t.Fatalf("基础镜像引用=%s:%s，期望 demo:latest", baseRes.Name, baseRes.Version)
	}
	baseLayer := baseLoaded.Manifest.Layers[0]

	// 记录基础层字节，稍后逐字节比对。
	baseBytes := readArchiveEntry(t, baseRes.Path, baseLayer.Path)

	ctxDir := filepath.Join(dir, "ctx")
	writeFile(t, filepath.Join(ctxDir, "app.txt"), "from child\n", 0o644)
	childOpts := &Options{
		ContextDir: ctxDir,
		Boxfile: mustParse(t, `
FROM demo:latest
COPY app.txt /etc/app.txt
ENV APP=1
`),
		BaseImage: baseRes.Path,
		OutPath:   filepath.Join(dir, "child.licore"),
	}
	childRes, err := Build(context.Background(), childOpts)
	if err != nil {
		t.Fatalf("基于基础镜像构建: %v", err)
	}

	child, err := image.OpenFile(childRes.Path)
	if err != nil {
		t.Fatalf("OpenFile(子): %v", err)
	}
	if childRes.LayerCount != 2 {
		t.Fatalf("LayerCount=%d，期望 2", childRes.LayerCount)
	}
	if len(child.Manifest.Layers) != 2 {
		t.Fatalf("index 层数=%d，期望 2", len(child.Manifest.Layers))
	}

	got := child.Manifest.Layers[0]
	if got.Digest != baseLayer.Digest {
		t.Errorf("基础层 digest=%s，期望原样保留 %s", got.Digest, baseLayer.Digest)
	}
	if got.SizeBytes != baseLayer.SizeBytes {
		t.Errorf("基础层 sizeBytes=%d，期望原样保留 %d", got.SizeBytes, baseLayer.SizeBytes)
	}
	if got.ApplyOrder != 1 {
		t.Errorf("基础层 applyOrder=%d，期望 1", got.ApplyOrder)
	}
	if !bytes.Equal(readArchiveEntry(t, childRes.Path, got.Path), baseBytes) {
		t.Errorf("基础层字节未逐字节保留")
	}

	// 追加层必须是 applyOrder=2 的新层，且与基础层摘要不同。
	app := child.Manifest.Layers[1]
	if app.ApplyOrder != 2 {
		t.Errorf("追加层 applyOrder=%d，期望 2", app.ApplyOrder)
	}
	if app.Digest != childRes.LayerDigest {
		t.Errorf("追加层 digest=%s，Result.LayerDigest=%s", app.Digest, childRes.LayerDigest)
	}
	if app.Path == got.Path {
		t.Errorf("追加层路径与基础层相同: %s", app.Path)
	}

	// config 必须是基础 config 的延续：基础 COPY 的文件还在，新 COPY 也在。
	if err := child.VerifyLayers(); err != nil {
		t.Fatalf("VerifyLayers: %v", err)
	}
	layers := unpackAllLayers(t, childRes.Path, filepath.Join(dir, "unpack"))
	if _, err := os.Stat(filepath.Join(layers[0], "etc", "base.txt")); err != nil {
		t.Errorf("基础层内容丢失: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layers[1], "etc", "app.txt")); err != nil {
		t.Errorf("追加层缺少 etc/app.txt: %v", err)
	}
	cfg, err := image.ParseConfig(readConfigBlob(t, childRes.Path))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Env["APP"] != "1" {
		t.Errorf("env[APP]=%q，期望 1", cfg.Env["APP"])
	}
	if childRes.Name != "demo" || childRes.Version != "latest" {
		t.Errorf("子镜像引用=%s:%s，期望沿用基础镜像 demo:latest", childRes.Name, childRes.Version)
	}
}

// readArchiveEntry 取出外层归档里某个条目的原始字节。
func readArchiveEntry(t *testing.T, imgPath, name string) []byte {
	t.Helper()
	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatalf("Open(%s): %v", imgPath, err)
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("产物 %s 缺少条目 %s", imgPath, name)
		}
		if err != nil {
			t.Fatalf("扫描归档 %s: %v", imgPath, err)
		}
		if hdr.Name != name {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("读取条目 %s: %v", name, err)
		}
		return data
	}
}

// TestCopyRejectsContextEscape 覆盖 COPY 源逃逸构建上下文的拒绝路径：
// 语法层（.. / 绝对路径）、目录组件经符号链接跳出去，以及目标逃逸都必须报错。
// 注意：末段本身是符号链接不算逃逸——它按"链接数据"原样搬进 rootfs，不被跟随读取。
func TestCopyRejectsContextEscape(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "top secret\n", 0o644)

	cases := []struct {
		name    string
		boxfile string
		prep    func(t *testing.T, dir string)
	}{
		{
			name:    "dotdot",
			boxfile: "FROM scratch\nCOPY ../secret.txt /etc/secret.txt\n",
		},
		{
			name:    "deep_dotdot",
			boxfile: "FROM scratch\nCOPY a/../../secret.txt /etc/secret.txt\n",
		},
		{
			name:    "absolute",
			boxfile: "FROM scratch\nCOPY /etc/passwd /etc/passwd\n",
		},
		{
			name:    "symlink_dir_escape",
			boxfile: "FROM scratch\nCOPY out/secret.txt /etc/secret.txt\n",
			prep: func(t *testing.T, dir string) {
				if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
					t.Fatalf("Symlink: %v", err)
				}
			},
		},
		{
			name:    "symlink_dir_deep_escape",
			boxfile: "FROM scratch\nCOPY a/b/secret.txt /etc/secret.txt\n",
			prep: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				if err := os.Symlink(outside, filepath.Join(dir, "a", "b")); err != nil {
					t.Fatalf("Symlink: %v", err)
				}
			},
		},
		{
			name:    "copy_dst_escape",
			boxfile: "FROM scratch\nCOPY secret.txt /../etc/secret.txt\n",
			prep: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "secret.txt"), "x\n", 0o644)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.prep != nil {
				tc.prep(t, dir)
			}
			opts := baseOpts(t, dir, tc.boxfile)
			_, err := Build(context.Background(), opts)
			if err == nil {
				t.Fatalf("Build 未拒绝逃逸的 COPY 源")
			}
			if !errors.Is(err, ErrNoContext) {
				t.Fatalf("错误 %v 未包装 ErrNoContext", err)
			}
			if _, statErr := os.Stat(opts.OutPath); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("构建失败后不应留下产物 %s（stat err=%v）", opts.OutPath, statErr)
			}
		})
	}
}

// TestCopyPreservesLeafSymlinkWithoutFollowing 断言末段符号链接按链接原样搬进
// rootfs，而不是被跟随读取目标内容（构建语义：链接是数据）。
func TestCopyPreservesLeafSymlinkWithoutFollowing(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "top secret\n", 0o644)

	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	opts := baseOpts(t, dir, "FROM scratch\nCOPY link.txt /etc/link.txt\n")
	res, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	headers := layerHeaders(t, res.Path, res.LayerDigest)
	hdr, ok := headers["etc/link.txt"]
	if !ok {
		t.Fatalf("层里缺少 etc/link.txt")
	}
	if hdr.Typeflag != tar.TypeSymlink {
		t.Fatalf("etc/link.txt 类型=%c，期望符号链接 %c（内容未被跟随拷贝）", hdr.Typeflag, tar.TypeSymlink)
	}
	if hdr.Linkname != filepath.Join(outside, "secret.txt") {
		t.Errorf("链接目标=%q，期望原样保留 %q", hdr.Linkname, filepath.Join(outside, "secret.txt"))
	}
	if hdr.Size != 0 {
		t.Errorf("符号链接条目大小=%d，期望 0（未写入目标内容）", hdr.Size)
	}
}

// TestCopySourceMustExist 断言 COPY 源不存在时报错。
func TestCopySourceMustExist(t *testing.T) {
	dir := t.TempDir()
	opts := baseOpts(t, dir, "FROM scratch\nCOPY missing.txt /etc/missing.txt\n")
	if _, err := Build(context.Background(), opts); err == nil {
		t.Fatal("Build 未拒绝不存在的 COPY 源")
	} else if !errors.Is(err, ErrNoContext) {
		t.Fatalf("错误 %v 未包装 ErrNoContext", err)
	}
}

// TestCopyRequiresContext 断言含 COPY 的构建必须提供构建上下文。
func TestCopyRequiresContext(t *testing.T) {
	dir := t.TempDir()
	opts := baseOpts(t, dir, "FROM scratch\nCOPY a.txt /a.txt\n")
	opts.ContextDir = ""
	if _, err := Build(context.Background(), opts); !errors.Is(err, ErrNoContext) {
		t.Fatalf("错误 %v 未包装 ErrNoContext", err)
	}
}

// TestCopyStripsSetuidBits 断言 setuid / setgid 位被剥离。
func TestCopyStripsSetuidBits(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "suid.bin"), "binary\n", 0o4755)
	writeFile(t, filepath.Join(dir, "sgid.bin"), "binary\n", 0o2755)

	opts := baseOpts(t, dir, `
FROM scratch
COPY suid.bin /usr/bin/suid.bin
COPY sgid.bin /usr/bin/sgid.bin
`)
	res, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	layers := unpackAllLayers(t, res.Path, filepath.Join(dir, "unpack"))
	for _, name := range []string{"suid.bin", "sgid.bin"} {
		fi, err := os.Stat(filepath.Join(layers[0], "usr", "bin", name))
		if err != nil {
			t.Fatalf("层内 %s: %v", name, err)
		}
		if fi.Mode().Perm()&0o7000 != 0 {
			t.Errorf("%s 权限=%v，setuid/setgid/sticky 位未剥离", name, fi.Mode())
		}
	}

	// 层 tar 头里的 mode 也必须干净。
	headers := layerHeaders(t, res.Path, res.LayerDigest)
	for name, hdr := range headers {
		if strings.HasPrefix(name, "usr/bin/") && hdr.Mode&0o7000 != 0 {
			t.Errorf("层内 %s 的 tar 头 mode=%#o 仍含危险位", name, hdr.Mode)
		}
	}
}

// TestSkipsSocketsFifos 断言 socket / FIFO 被跳过且不阻断构建。
func TestSkipsSocketsFifos(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ok.txt"), "ok\n", 0o644)
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Skipf("本平台不支持 Mkfifo: %v", err)
	}

	opts := baseOpts(t, dir, `
FROM scratch
COPY ok.txt /ok.txt
COPY pipe /pipe
`)
	res, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Skipped != 1 {
		t.Errorf("Result.Skipped=%d，期望 1", res.Skipped)
	}
	headers := layerHeaders(t, res.Path, res.LayerDigest)
	if _, ok := headers["pipe"]; ok {
		t.Errorf("FIFO 不应出现在层里")
	}
	if _, ok := headers["ok.txt"]; !ok {
		t.Errorf("层里缺少 ok.txt")
	}
}

// layerHeaders 返回某层 tar.gz 内 条目名 → tar 头。
func layerHeaders(t *testing.T, imgPath, wantDigest string) map[string]*tar.Header {
	t.Helper()
	loaded, err := image.OpenFile(imgPath)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", imgPath, err)
	}
	var path string
	for _, la := range loaded.Manifest.Layers {
		if la.Digest == wantDigest {
			path = la.Path
		}
	}
	if path == "" {
		t.Fatalf("产物中没有摘要 %s 的层", wantDigest)
	}
	raw := readArchiveEntry(t, imgPath, path)
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()
	out := map[string]*tar.Header{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("读层 tar: %v", err)
		}
		out[strings.TrimSuffix(hdr.Name, "/")] = hdr
	}
}

// TestBuildRejectsBadInput 覆盖输入校验：nil Boxfile、空输出、坏基础镜像、层数上限。
func TestBuildRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a\n", 0o644)

	t.Run("nil_boxfile", func(t *testing.T) {
		_, err := Build(context.Background(), &Options{OutPath: filepath.Join(dir, "x.licore")})
		if !errors.Is(err, ErrBadBoxfile) {
			t.Fatalf("错误 %v 未包装 ErrBadBoxfile", err)
		}
	})
	t.Run("empty_outpath", func(t *testing.T) {
		_, err := Build(context.Background(), &Options{Boxfile: mustParse(t, "FROM scratch\n")})
		if !errors.Is(err, ErrBadInstruction) {
			t.Fatalf("错误 %v 未包装 ErrBadInstruction", err)
		}
	})
	t.Run("missing_base", func(t *testing.T) {
		opts := baseOpts(t, dir, "FROM scratch\n")
		opts.BaseImage = filepath.Join(dir, "nope.licore")
		if _, err := Build(context.Background(), opts); err == nil {
			t.Fatal("Build 未拒绝不存在的基础镜像")
		}
	})
	t.Run("corrupt_base", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.licore")
		writeFile(t, bad, "not a tar at all", 0o644)
		opts := baseOpts(t, dir, "FROM scratch\n")
		opts.BaseImage = bad
		if _, err := Build(context.Background(), opts); err == nil {
			t.Fatal("Build 未拒绝损坏的基础镜像")
		}
	})
	t.Run("cancelled_context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		opts := baseOpts(t, dir, "FROM scratch\nCOPY a.txt /a.txt\n")
		if _, err := Build(ctx, opts); !errors.Is(err, context.Canceled) {
			t.Fatalf("错误 %v 未包装 context.Canceled", err)
		}
	})
	t.Run("derived_name_invalid", func(t *testing.T) {
		writeFile(t, filepath.Join(dir, "a.txt"), "a\n", 0o644)
		opts := baseOpts(t, dir, "FROM scratch\nCOPY a.txt /a.txt\n")
		opts.OutPath = filepath.Join(dir, "UPPER CASE.licore")
		if _, err := Build(context.Background(), opts); !errors.Is(err, ErrBadInstruction) {
			t.Fatalf("错误 %v 未包装 ErrBadInstruction（镜像名非法）", err)
		}
	})
}

// TestBuildOverwritesExistingOutput 断言重复构建覆盖旧产物而不是追加。
func TestBuildOverwritesExistingOutput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a\n", 0o644)
	opts := baseOpts(t, dir, "FROM scratch\nCOPY a.txt /a.txt\n")

	first, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatalf("首次 Build: %v", err)
	}
	second, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatalf("二次 Build: %v", err)
	}
	if first.LayerDigest != second.LayerDigest {
		t.Errorf("相同输入产生了不同层摘要: %s vs %s", first.LayerDigest, second.LayerDigest)
	}
	if _, err := image.OpenFile(second.Path); err != nil {
		t.Fatalf("覆盖后的产物非法: %v", err)
	}
}

// TestBuildRespectsPlatformOverrides 断言 Architecture / OS 覆盖生效。
// TestBuildArchTable 逐个架构参数构建，断言产出的 index.json
// architecture 字段正确——`licore build --arch` 的核心行为。
func TestBuildArchTable(t *testing.T) {
	for _, arch := range image.SupportedArches() {
		t.Run(arch, func(t *testing.T) {
			dir := t.TempDir()
			opts := baseOpts(t, dir, "FROM scratch\n")
			opts.Architecture = arch
			res, err := Build(context.Background(), opts)
			if err != nil {
				t.Fatalf("Build(--arch %s): %v", arch, err)
			}
			loaded, err := image.OpenFile(res.Path)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			if loaded.Manifest.Architecture != arch {
				t.Errorf("architecture = %q, want %q", loaded.Manifest.Architecture, arch)
			}
			// 产物必须能被自己的校验器接受：这正是旧实现缺 --arch 时
			// 手工改 index.json 才能绕过的坏包风险。
			if err := loaded.Manifest.Validate(); err != nil {
				t.Errorf("产物清单未通过 Validate: %v", err)
			}
		})
	}
}

// TestBuildArchDefaultsToHost 断言不传 --arch 时跟随宿主 GOARCH（向后兼容）。
func TestBuildArchDefaultsToHost(t *testing.T) {
	dir := t.TempDir()
	opts := baseOpts(t, dir, "FROM scratch\n")
	res, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	loaded, err := image.OpenFile(res.Path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if loaded.Manifest.Architecture != runtime.GOARCH {
		t.Errorf("architecture = %q, want 宿主 %q", loaded.Manifest.Architecture, runtime.GOARCH)
	}
}

// TestBuildOSTable 覆盖 --os 参数。
func TestBuildOSTable(t *testing.T) {
	for _, osName := range image.SupportedOSes() {
		t.Run(osName, func(t *testing.T) {
			dir := t.TempDir()
			opts := baseOpts(t, dir, "FROM scratch\n")
			opts.OS = osName
			res, err := Build(context.Background(), opts)
			if err != nil {
				t.Fatalf("Build(--os %s): %v", osName, err)
			}
			loaded, err := image.OpenFile(res.Path)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			if loaded.Manifest.OS != osName {
				t.Errorf("os = %q, want %q", loaded.Manifest.OS, osName)
			}
		})
	}
}

// TestBuildRespectsPlatformOverrides 断言 arm64 + linux 组合同时生效
// （交叉构建 arm64 镜像的主用例）。
func TestBuildRespectsPlatformOverrides(t *testing.T) {
	dir := t.TempDir()
	opts := baseOpts(t, dir, "FROM scratch\n")
	opts.Architecture = "arm64"
	opts.OS = "linux"
	res, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	loaded, err := image.OpenFile(res.Path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if loaded.Manifest.Architecture != "arm64" || loaded.Manifest.OS != "linux" {
		t.Errorf("平台=%s/%s，期望 linux/arm64", loaded.Manifest.OS, loaded.Manifest.Architecture)
	}
	if loaded.Manifest.Annotations["org.licore.build.tool"] == "" {
		t.Errorf("annotations 缺少 org.licore.build.tool")
	}
}

// TestBuildCtxDirAsDestPreservesModes 断言 COPY 目录时的权限位保留与层级结构。
func TestBuildCtxDirAsDestPreservesModes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "tree", "sub", "deep.txt"), "deep\n", 0o640)
	writeFile(t, filepath.Join(dir, "tree", "exec.sh"), "#!/bin/sh\n", 0o755)

	opts := baseOpts(t, dir, "FROM scratch\nCOPY tree /opt/tree\n")
	opts.OutPath = filepath.Join(dir, "tree.licore")
	res, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	layers := unpackAllLayers(t, res.Path, filepath.Join(dir, "unpack"))
	deep := filepath.Join(layers[0], "opt", "tree", "sub", "deep.txt")
	if fi, err := os.Stat(deep); err != nil {
		t.Fatalf("层内 %s: %v", deep, err)
	} else if fi.Mode().Perm() != 0o640 {
		t.Errorf("deep.txt 权限=%v，期望 0640", fi.Mode().Perm())
	}
	sh := filepath.Join(layers[0], "opt", "tree", "exec.sh")
	if fi, err := os.Stat(sh); err != nil {
		t.Fatalf("层内 %s: %v", sh, err)
	} else if fi.Mode().Perm() != 0o755 {
		t.Errorf("exec.sh 权限=%v，期望 0755", fi.Mode().Perm())
	}
}

// TestTarTreeIsDeterministic 断言同一 rootfs 两次打包产生完全相同的字节。
func TestTarTreeIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs")
	writeFile(t, filepath.Join(rootfs, "b.txt"), "b\n", 0o644)
	writeFile(t, filepath.Join(rootfs, "a.txt"), "a\n", 0o644)
	writeFile(t, filepath.Join(rootfs, "sub", "c.txt"), "c\n", 0o644)

	first := filepath.Join(dir, "1.tar.gz")
	second := filepath.Join(dir, "2.tar.gz")
	d1, s1, err := writeLayer(rootfs, first)
	if err != nil {
		t.Fatalf("writeLayer: %v", err)
	}
	d2, s2, err := writeLayer(rootfs, second)
	if err != nil {
		t.Fatalf("writeLayer: %v", err)
	}
	if d1 != d2 || s1 != s2 {
		t.Fatalf("两次打包不一致: %s/%d vs %s/%d", d1, s1, d2, s2)
	}
	raw1, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("读取 %s: %v", first, err)
	}
	raw2, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("读取 %s: %v", second, err)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Errorf("两次打包的字节不同")
	}
	if len(raw1) != int(s1) {
		t.Errorf("摘要大小 %d 与磁盘 %d 不符", s1, len(raw1))
	}
}

// TestImageRefOverride 验证 build.Options.Name/Version 覆盖写入 index.json。
func TestImageRefOverride(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "x\n", 0o644)
	opts := baseOpts(t, dir, "FROM scratch\nCOPY f.txt /f.txt\n")
	opts.OutPath = filepath.Join(dir, "whatever.licore")
	opts.Name = "myns/demo"
	opts.Version = "v7"
	res, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "myns/demo" || res.Version != "v7" {
		t.Fatalf("结果引用错误: %s:%s", res.Name, res.Version)
	}
	loaded, err := image.OpenFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Manifest.Name != "myns/demo" || loaded.Manifest.Version != "v7" {
		t.Fatalf("清单引用错误: %s:%s", loaded.Manifest.Name, loaded.Manifest.Version)
	}
}

// TestBuildDeltaLayerOnlyContainsChanges 关键回归：FROM 基础镜像构建时，
// 追加层只能包含本次改动，不得把基础层内容重新打包一遍。
// 若重打，每个派生镜像都会带一份完整基础 rootfs，分层复用形同虚设。
func TestBuildDeltaLayerOnlyContainsChanges(t *testing.T) {
	dir := t.TempDir()

	// 基础镜像：一个 200 KiB 大文件 + 一个标记文件（足够大，重打会明显变大）。
	baseDir := filepath.Join(dir, "base")
	big := bytes.Repeat([]byte("B"), 200*1024)
	writeFile(t, filepath.Join(baseDir, "big.bin"), string(big), 0o644)
	writeFile(t, filepath.Join(baseDir, "base.txt"), "from base\n", 0o644)

	bo := baseOpts(t, baseDir, `
FROM scratch
COPY big.bin /etc/big.bin
COPY base.txt /etc/base.txt
`)
	bo.OutPath = filepath.Join(baseDir, "demo.licore")
	baseRes, err := Build(context.Background(), bo)
	if err != nil {
		t.Fatalf("构建基础镜像: %v", err)
	}
	baseLoaded, err := image.OpenFile(baseRes.Path)
	if err != nil {
		t.Fatal(err)
	}
	baseLayerSize := baseLoaded.Manifest.Layers[0].SizeBytes

	// 派生镜像：只加一个小文件。
	ctxDir := filepath.Join(dir, "ctx")
	writeFile(t, filepath.Join(ctxDir, "run.sh"), "#!/bin/sh\necho hi\n", 0o755)
	co := &Options{
		ContextDir: ctxDir,
		Boxfile: mustParse(t, `
FROM demo:latest
COPY run.sh /app/run.sh
`),
		BaseImage: baseRes.Path,
		OutPath:   filepath.Join(dir, "child.licore"),
	}
	childRes, err := Build(context.Background(), co)
	if err != nil {
		t.Fatalf("基于基础镜像构建: %v", err)
	}
	if childRes.LayerCount != 2 {
		t.Fatalf("层数 = %d, want 2（基础层引用 + 追加层）", childRes.LayerCount)
	}

	childLoaded, err := image.OpenFile(childRes.Path)
	if err != nil {
		t.Fatal(err)
	}
	layers := append([]image.Layer(nil), childLoaded.Manifest.Layers...)
	sort.Slice(layers, func(i, j int) bool { return layers[i].ApplyOrder < layers[j].ApplyOrder })

	// 第 1 层必须与基础层同 digest（引用而非复制）。
	if layers[0].Digest != baseLoaded.Manifest.Layers[0].Digest {
		t.Errorf("基础层 digest 被改写：got %s want %s",
			layers[0].Digest, baseLoaded.Manifest.Layers[0].Digest)
	}

	// 追加层必须远小于基础层：重打整个 rootfs 会让两者相当。
	if layers[1].SizeBytes >= baseLayerSize/2 {
		t.Errorf("追加层 %d 字节，基础层 %d 字节：追加层过大，疑似重打了整个 rootfs",
			layers[1].SizeBytes, baseLayerSize)
	}

	// 追加层里只能有这次的改动，不得混入基础层内容。
	hdrs := layerHeaders(t, childRes.Path, layers[1].Digest)
	if _, bad := hdrs["etc/big.bin"]; bad {
		t.Error("追加层混入了基础层的 etc/big.bin")
	}
	if _, bad := hdrs["etc/base.txt"]; bad {
		t.Error("追加层混入了基础层的 etc/base.txt")
	}
	if _, ok := hdrs["app/run.sh"]; !ok {
		t.Errorf("追加层缺少 app/run.sh，实际条目: %v", keysOf(hdrs))
	}
}

// TestBuildDeltaLayerSurvivesCopyOverBase 覆盖「COPY 覆盖基础层同名文件」：
// 增量层必须带上被覆盖后的内容，运行时以增量层为准。
func TestBuildDeltaLayerSurvivesCopyOverBase(t *testing.T) {
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "base")
	writeFile(t, filepath.Join(baseDir, "conf"), "base-version\n", 0o644)

	bo := baseOpts(t, baseDir, `
FROM scratch
COPY conf /etc/conf
`)
	bo.OutPath = filepath.Join(baseDir, "demo.licore")
	baseRes, err := Build(context.Background(), bo)
	if err != nil {
		t.Fatal(err)
	}

	ctxDir := filepath.Join(dir, "ctx")
	writeFile(t, filepath.Join(ctxDir, "conf"), "child-version\n", 0o644)
	co := &Options{
		ContextDir: ctxDir,
		Boxfile:    mustParse(t, "FROM demo:latest\nCOPY conf /etc/conf\n"),
		BaseImage:  baseRes.Path,
		OutPath:    filepath.Join(dir, "child.licore"),
	}
	childRes, err := Build(context.Background(), co)
	if err != nil {
		t.Fatalf("构建派生镜像: %v", err)
	}
	childLoaded, err := image.OpenFile(childRes.Path)
	if err != nil {
		t.Fatal(err)
	}
	layers := append([]image.Layer(nil), childLoaded.Manifest.Layers...)
	sort.Slice(layers, func(i, j int) bool { return layers[i].ApplyOrder < layers[j].ApplyOrder })

	// 增量层必须包含被覆盖的 /etc/conf。
	hdrs := layerHeaders(t, childRes.Path, layers[1].Digest)
	if _, ok := hdrs["etc/conf"]; !ok {
		t.Errorf("增量层缺少被覆盖的 etc/conf，实际条目: %v", keysOf(hdrs))
	}

	// 解包两层并按 applyOrder 合并后，/etc/conf 必须是新内容。
	storeRoot := t.TempDir()
	fsDirs := unpackAllLayers(t, childRes.Path, storeRoot)
	merged := t.TempDir()
	if err := storage.MergeLayers(storeRoot, []string{
		strings.TrimPrefix(layers[0].Digest, "sha256:"),
		strings.TrimPrefix(layers[1].Digest, "sha256:"),
	}, merged); err != nil {
		t.Fatalf("MergeLayers: %v", err)
	}
	_ = fsDirs
	got, err := os.ReadFile(filepath.Join(merged, "etc", "conf"))
	if err != nil {
		t.Fatalf("读取合并后的 /etc/conf: %v", err)
	}
	if string(got) != "child-version\n" {
		t.Errorf("合并后 /etc/conf = %q, want %q（增量层未覆盖基础层）", got, "child-version\n")
	}
}

// TestBuildScratchStillPacksWholeRootfs scratch 构建没有基础层可引用，
// 必须仍旧整树打包，否则镜像会是空的。
func TestBuildScratchStillPacksWholeRootfs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "tool.sh"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(dir, "conf"), "x\n", 0o644)

	opts := baseOpts(t, dir, `
FROM scratch
COPY tool.sh /bin/tool.sh
COPY conf /etc/conf
`)
	opts.OutPath = filepath.Join(dir, "scratch.licore")
	res, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.LayerCount != 1 {
		t.Fatalf("层数 = %d, want 1", res.LayerCount)
	}
	loaded, err := image.OpenFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	hdrs := layerHeaders(t, res.Path, loaded.Manifest.Layers[0].Digest)
	for _, want := range []string{"bin/tool.sh", "etc/conf"} {
		if _, ok := hdrs[want]; !ok {
			t.Errorf("scratch 层缺少 %s，实际条目: %v", want, keysOf(hdrs))
		}
	}
}

// keysOf 返回 map 的键，仅用于测试失败时打印可读信息。
func keysOf(m map[string]*tar.Header) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
