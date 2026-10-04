// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/LiStudioorg/licore/internal/image"
	"github.com/LiStudioorg/licore/internal/store"
)

// 本文件实现 `licore tag` / `save` / `load` / `commit` 的纯逻辑层：
// 不依赖任何 syscall、不 shell out、不启动容器，全部可在任意平台上测试。
// cobra 命令只负责参数绑定与输出渲染，业务语义都在这里。

// imageNameRE 与 image 包保持一致（docs/image-spec.md §3.1：`^[a-z0-9][a-z0-9._/-]{0,254}$`）。
var imageNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,254}$`)

// layerAppName 是 commit 产物唯一层的文件名中段：layers/000001.app.tar.gz。
const layerAppName = "app"

// commitLayerPath 是 commit 产物唯一层在归档内的固定路径（规范 §2 的 NNNNNN 序号）。
const commitLayerPath = "layers/000001." + layerAppName + ".tar.gz"

// ImageRef 是一个镜像引用 name:version。
type ImageRef struct {
	// Name 是仓库名，允许 alice/myapp 形式的多级路径。
	Name string
	// Version 是 tag（规范中称 version），非空且不含空白。
	Version string
}

// String 返回 name:version 形式。
func (r ImageRef) String() string { return r.Name + ":" + r.Version }

// ParseImageRef 解析 name:version 引用并严格校验。
// 名称规则与规范 §3.1 一致；version 非空、不含空白，允许字母数字与 . _ - 以及
// 仓库常用的 @ : /（用于 digest 式 tag），保证 `licore tag` 不会写出清单无法承载的引用。
func ParseImageRef(s string) (ImageRef, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return ImageRef{}, fmt.Errorf("镜像引用为空")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return ImageRef{}, fmt.Errorf("镜像引用 %q 含空白字符", s)
	}
	i := strings.LastIndex(raw, ":")
	if i < 0 {
		return ImageRef{}, fmt.Errorf("镜像引用 %q 缺少 :version 部分（期望 name:version）", s)
	}
	name, version := raw[:i], raw[i+1:]
	if name == "" {
		return ImageRef{}, fmt.Errorf("镜像引用 %q 的 name 为空", s)
	}
	if !imageNameRE.MatchString(name) {
		return ImageRef{}, fmt.Errorf("镜像 name %q 非法（需匹配 %s）", name, imageNameRE.String())
	}
	for _, seg := range strings.Split(name, "/") { // 正则允许 "//"，实际路径层级不可为空
		if seg == "" {
			return ImageRef{}, fmt.Errorf("镜像 name %q 含空路径段", name)
		}
	}
	if version == "" {
		return ImageRef{}, fmt.Errorf("镜像引用 %q 的 version 为空", s)
	}
	if strings.ContainsAny(version, " \t\r\n/:") {
		return ImageRef{}, fmt.Errorf("镜像 version %q 非法（含空白或分隔符）", version)
	}
	return ImageRef{Name: name, Version: version}, nil
}

// Retag 把 srcRef 指向的已落地镜像复制成 dstRef，不触碰 internal/store 与 internal/image。
// 复制内容是整个镜像目录（source.licore + index.json + 新写的 state.json），
// 全程"同父目录临时目录 + rename"，失败不留下半成品。
// 目标已存在且 force=false 时返回包装了 store.ErrExists 的错误。
func Retag(st *store.Store, srcRef, dstRef string, force bool) error {
	if st == nil {
		return fmt.Errorf("store 为空")
	}
	src, err := ParseImageRef(srcRef)
	if err != nil {
		return fmt.Errorf("tag 源: %w", err)
	}
	dst, err := ParseImageRef(dstRef)
	if err != nil {
		return fmt.Errorf("tag 目标: %w", err)
	}
	if src == dst {
		return fmt.Errorf("源与目标引用相同 %s，无需打标签", src)
	}

	// 先把源镜像读进内存：source.licore 之后直接复用字节，避免二次打开。
	state, manifest, srcFile, err := readLocalImage(st, src)
	if err != nil {
		return err
	}

	if err := writeImageDir(st, dst, srcFile, manifest, state, src, force); err != nil {
		return err
	}
	slog.Info("镜像已打标签", "src", src.String(), "dst", dst.String())
	return nil
}

// ExportImage 把已落地镜像的 source.licore 原样导出到 dst。
// 写出是原子的：在目标同目录建临时文件，成功后 rename；
// 目标已存在且 force=false 时返回包装了 store.ErrExists 的错误。
func ExportImage(st *store.Store, ref, dst string, force bool) error {
	if st == nil {
		return fmt.Errorf("store 为空")
	}
	r, err := ParseImageRef(ref)
	if err != nil {
		return err
	}
	if strings.TrimSpace(dst) == "" {
		return fmt.Errorf("导出目标路径为空")
	}
	exists, err := st.Exists(r.Name, r.Version)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("本地镜像 %s 不存在（先用 licore images 确认）", r)
	}
	srcPath := filepath.Join(st.ImageDir(r.Name, r.Version), "source.licore")
	if _, err := os.Stat(srcPath); err != nil {
		return fmt.Errorf("读取镜像文件 %s 失败: %w", srcPath, err)
	}
	if err := writeFileAtomic(dst, srcPath, force); err != nil {
		return err
	}
	slog.Info("镜像已导出", "ref", r.String(), "dst", dst)
	return nil
}

// SaveImage 是 `licore save` 的实现：与 ExportImage 相同，但校验目标以 .licore 结尾
// （规范 §1：`.licore` 是自研镜像格式的唯一后缀，避免导出成会被误认的文件）。
func SaveImage(st *store.Store, ref, dst string, force bool) error {
	if !strings.HasSuffix(dst, ".licore") {
		return fmt.Errorf("导出目标 %q 必须以 .licore 结尾（LiCore 镜像格式与 Docker/OCI 不兼容）", dst)
	}
	return ExportImage(st, ref, dst, force)
}

// ImportOptions 是 ImportImage 的可选项，零值即"按当前平台校验、不覆盖已有镜像"。
type ImportOptions struct {
	// Force 表示同名镜像已存在时覆盖重构建。
	Force bool
	// AllowArchMismatch 跳过宿主平台校验，供交叉构建（`licore build --arch`）
	// 与显式导入异构镜像使用，对应规范第 4 节规则 6 的 --allow-arch-mismatch。
	AllowArchMismatch bool
}

// ImportImage 校验并落地一个本地 .licore 文件（等价 `licore pull`，供 `licore load` 使用）。
// dstRef 非空且与清单自带引用不同时，额外在 store 中登记一份 dstRef 的副本。
//
// 幂等语义：清单自带引用已存在时不再重复落地（除非 Force），
// 这样"load 一个已导入的文件只是为了加上另一个 tag"不会被误判成冲突。
func ImportImage(st *store.Store, src, dstRef string, opts ImportOptions) (*image.Loaded, error) {
	if st == nil {
		return nil, fmt.Errorf("store 为空")
	}
	loaded, err := image.OpenFile(src)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", src, err)
	}
	m := loaded.Manifest

	exists, err := st.Exists(m.Name, m.Version)
	if err != nil {
		return nil, err
	}
	switch {
	case !exists:
		loaded, err = st.Put(src, opts.Force, opts.AllowArchMismatch)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", src, err)
		}
	case opts.Force:
		loaded, err = st.Put(src, true, opts.AllowArchMismatch)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", src, err)
		}
	default:
		// 已落地：校验层摘要仍然可信，避免拿一个损坏文件去加标签。
		if err := loaded.VerifyLayers(); err != nil {
			return nil, fmt.Errorf("load %s（本地已有同名镜像，但该文件校验失败）: %w", src, err)
		}
		slog.Info("镜像已存在，跳过落地", "ref", m.Ref(), "path", st.ImageDir(m.Name, m.Version))
	}

	if dstRef == "" || dstRef == m.Ref() {
		slog.Info("镜像已导入", "ref", m.Ref(), "path", st.ImageDir(m.Name, m.Version))
		return loaded, nil
	}
	if err := Retag(st, m.Ref(), dstRef, opts.Force); err != nil {
		return nil, fmt.Errorf("load --tag %s: %w", dstRef, err)
	}
	return loaded, nil
}

// CommitOptions 是 CommitRootfs 的可选项，零值即"按当前平台与空标签提交"。
type CommitOptions struct {
	// Architecture 覆盖清单的 architecture（默认 runtime.GOARCH）。
	Architecture string
	// OS 覆盖清单的 os（默认 runtime.GOOS）。
	OS string
	// Labels 写入 config 小对象的 labels。
	Labels map[string]string
	// Message 作为 org.licore.commit.message 注释写入 index.json。
	Message string
}

// CommitResult 是一次 commit 的产出。
type CommitResult struct {
	// Ref 是产物的 name:version。
	Ref string
	// Path 是产物 .licore 文件路径。
	Path string
	// Bytes 是产物文件字节数。
	Bytes int64
	// LayerDigest 是唯一层的 sha256 摘要。
	LayerDigest string
	// Skipped 是被跳过的非常规文件数（socket / 设备 / FIFO / 白out 装饰件）。
	Skipped int
}

// CommitRootfs 把容器的可写 rootfs 打包成一个新的单层 .licore 镜像（规范 §1 / §3）。
//
// 产物布局：
//
//	<root>/images/<name>/<version>/
//	├── source.licore            外层未压缩 tar：index.json + layers/000001.app.tar.gz + blobs/sha256-<hex>
//	├── index.json              解析出的清单（与 source.licore 内一致）
//	└── state.json              落地状态（sourcePath 指向产物自身）
//
// 层的摘要取 .gz 原始字节，config 的摘要取 blob 精确字节，两者都与清单声明严格一致；
// 写完后立刻用 image.OpenFile 自检，任何不一致都在这里暴露而不是等到 run 才发现。
// 本函数不启动容器、不依赖 Linux 内核特性，可在任意平台执行。
func CommitRootfs(st *store.Store, cfg *store.ContainerConfig, ref string, opts *CommitOptions) (*CommitResult, error) {
	if st == nil {
		return nil, fmt.Errorf("store 为空")
	}
	if cfg == nil {
		return nil, fmt.Errorf("容器配置为空")
	}
	r, err := ParseImageRef(ref)
	if err != nil {
		return nil, fmt.Errorf("commit 目标: %w", err)
	}
	root := cfg.Rootfs
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("容器 %s 没有可写 rootfs 路径", cfg.ID)
	}
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("读取容器 rootfs %s 失败: %w", root, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("容器 rootfs %s 不是目录", root)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析 rootfs 路径失败: %w", err)
	}

	if opts == nil {
		opts = &CommitOptions{}
	}
	arch, osName := opts.Architecture, opts.OS
	if arch == "" {
		arch = runtime.GOARCH
	}
	if osName == "" {
		osName = runtime.GOOS
	}

	dir := st.ImageDir(r.Name, r.Version)
	exists, err := st.Exists(r.Name, r.Version)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("镜像 %s 已存在: %w", r, store.ErrExists)
	}
	// 目标目录若已存在（例如上次 commit 留下的空目录），后续 rename 会因非空失败；
	// 先检查再让错误在 rename 处显式暴露，不做静默覆盖。
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("镜像目录 %s 已存在: %w", dir, store.ErrExists)
	}

	// 段一：把 rootfs 打成临时 tar（未压缩），同时统计跳过项。
	// 跳过产物自身所在目录，避免把 source.licore 打回镜像里。
	tmpDir, err := os.MkdirTemp("", "licore-commit-")
	if err != nil {
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tarPath := filepath.Join(tmpDir, "layer.tar")
	skipped, err := writeRootfsTar(root, tarPath, collectSkipRoots(st, dir))
	if err != nil {
		return nil, err
	}
	if skipped > 0 {
		slog.Warn("commit 跳过不支持的文件类型", "rootfs", root, "skipped", skipped)
	}

	layerPath := filepath.Join(tmpDir, "layer.tar.gz")
	layerDigest, layerSize, err := gzipFile(tarPath, layerPath)
	if err != nil {
		return nil, err
	}

	// 段二：构造 config blob 与 index.json，保证两个摘要都是"所写字节"的摘要。
	cfgObj := image.Config{Labels: opts.Labels}
	cfgBytes, err := json.Marshal(&cfgObj)
	if err != nil {
		return nil, fmt.Errorf("序列化 config 失败: %w", err)
	}
	if len(cfgBytes) > image.MaxConfigBytes {
		return nil, fmt.Errorf("config %d 字节超过上限 %d", len(cfgBytes), image.MaxConfigBytes)
	}
	cfgSum := sha256.Sum256(cfgBytes)
	cfgDigest := "sha256:" + hex.EncodeToString(cfgSum[:])

	annotations := map[string]string{}
	if opts.Message != "" {
		annotations["org.licore.commit.message"] = opts.Message
	}
	if len(annotations) == 0 {
		annotations = nil
	}
	manifest := &image.Manifest{
		MediaType:     image.MediaTypeManifest,
		SpecVersion:   image.SpecVersionV1,
		SchemaVersion: image.SchemaVersionV1,
		Architecture:  arch,
		OS:            osName,
		Created:       time.Now().UTC().Format(time.RFC3339),
		Name:          r.Name,
		Version:       r.Version,
		Config:        image.ConfigRef{Digest: cfgDigest, SizeBytes: int64(len(cfgBytes))},
		Layers: []image.Layer{{
			Path:       commitLayerPath,
			Digest:     layerDigest,
			SizeBytes:  layerSize,
			ApplyOrder: 1,
		}},
		Annotations: annotations,
	}
	if err := manifest.Validate(); err != nil {
		return nil, fmt.Errorf("构造清单失败: %w", err)
	}
	idxBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化 index.json 失败: %w", err)
	}

	// 段三：写外层未压缩 tar（条目名由清单摘要派生，内容就是刚算过摘要的那份字节）。
	blobName, err := blobEntryName(cfgDigest)
	if err != nil {
		return nil, err
	}
	layerBytes, err := os.ReadFile(layerPath)
	if err != nil {
		return nil, fmt.Errorf("读取层文件失败: %w", err)
	}
	licorePath := filepath.Join(tmpDir, "source.licore")
	if err := writeLiCoreArchive(licorePath, idxBytes, cfgBytes, layerBytes, blobName); err != nil {
		return nil, err
	}

	// 段四：自检 + 落盘（临时目录 → rename，原子）。
	selfCheck, err := image.OpenFile(licorePath)
	if err != nil {
		return nil, fmt.Errorf("commit 自检失败（产物不可用）: %w", err)
	}
	if err := selfCheck.VerifyLayers(); err != nil {
		return nil, fmt.Errorf("commit 自检失败（层摘要）: %w", err)
	}
	if err := selfCheck.Manifest.Validate(); err != nil {
		return nil, fmt.Errorf("commit 自检失败（清单）: %w", err)
	}

	licore, err := os.Open(licorePath)
	if err != nil {
		return nil, fmt.Errorf("打开产物失败: %w", err)
	}
	defer func() { _ = licore.Close() }()
	licoreInfo, err := licore.Stat()
	if err != nil {
		return nil, fmt.Errorf("读取产物信息失败: %w", err)
	}
	state := stateOnDisk{State: store.State{
		Ref:             manifest.Ref(),
		PulledAt:        time.Now().UTC().Format(time.RFC3339),
		SourcePath:      filepath.Join(dir, "source.licore"),
		SourceFileBytes: licoreInfo.Size(),
		LayersVerified:  true,
	}}
	if err := putImageDir(st, r, licore, idxBytes, state); err != nil {
		return nil, err
	}

	slog.Info("已从容器 rootfs 提交镜像",
		"container", cfg.ID, "ref", manifest.Ref(), "bytes", licoreInfo.Size(), "skipped", skipped)
	return &CommitResult{
		Ref:         manifest.Ref(),
		Path:        filepath.Join(dir, "source.licore"),
		Bytes:       licoreInfo.Size(),
		LayerDigest: layerDigest,
		Skipped:     skipped,
	}, nil
}

// readLocalImage 读取已落地镜像的 state.json / index.json / source.licore 字节。
// state.json 缺失是致命错误；index.json 损坏时降级为从 source.licore 内部重新解析
// （复制产物必须带上一份可用的清单展开副本）。
func readLocalImage(st *store.Store, ref ImageRef) (*store.State, *image.Manifest, []byte, error) {
	dir := st.ImageDir(ref.Name, ref.Version)
	state, err := st.ReadState(ref.Name, ref.Version)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("本地镜像 %s 状态不可读: %w", ref, err)
	}
	srcPath := filepath.Join(dir, "source.licore")
	srcBytes, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("读取镜像文件 %s 失败: %w", srcPath, err)
	}

	var manifest *image.Manifest
	idxBytes, ierr := os.ReadFile(filepath.Join(dir, "index.json"))
	switch {
	case ierr == nil:
		manifest, err = image.ParseManifest(idxBytes)
		if err != nil {
			slog.Warn("镜像目录 index.json 非法，改用 source.licore 内清单", "dir", dir, "err", err)
			manifest = nil
		}
	case errors.Is(ierr, fs.ErrNotExist):
		slog.Warn("镜像目录缺少 index.json，改用 source.licore 内清单", "dir", dir)
	default:
		return nil, nil, nil, fmt.Errorf("读取 index.json 失败: %w", ierr)
	}
	if manifest == nil {
		loaded, err := image.OpenFile(srcPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("解析 %s 失败: %w", srcPath, err)
		}
		manifest = loaded.Manifest
	}
	return state, manifest, srcBytes, nil
}

// stateOnDisk 是 state.json 的线格式：store.State 的字段 + 本模块新增的 originalRef。
// 用内嵌而非改 store.State（本分支不得触碰 internal/store），内嵌同时保证
// 既有字段的 JSON 名与 store 完全一致，store.ReadState 读回去不会丢字段。
type stateOnDisk struct {
	store.State
	// OriginalRef 记录打标签前的来源引用（如 alice/myapp:v1）。
	OriginalRef string `json:"originalRef,omitempty"`
}

// newStateFor 基于源状态生成目标镜像的状态：Ref 更新、PulledAt 刷新为当前 UTC、
// OriginalRef 记录来源引用（沿用源上已有的 OriginalRef，保证多次打标签仍指向最初的引用），
// 其余（来源路径、层校验态等）沿用。
func newStateFor(src *store.State, dst, srcRef ImageRef) stateOnDisk {
	out := stateOnDisk{State: store.State{
		Ref:      dst.String(),
		PulledAt: time.Now().UTC().Format(time.RFC3339),
	}}
	if src != nil {
		out.SourcePath = src.SourcePath
		out.SourceFileBytes = src.SourceFileBytes
		out.LayersVerified = src.LayersVerified
	}
	out.OriginalRef = srcRef.String()
	return out
}

// writeImageDir 在 st 中新建 dst 引用目录（同父临时目录 → rename），内容为
// 给定的 source.licore 字节 + manifest 展开出的 index.json + 新写的 state.json。
// index.json 必须写入：store.ListImages 依赖它读取架构与层数，缺失会被判为损坏镜像。
func writeImageDir(st *store.Store, dst ImageRef, srcBytes []byte, manifest *image.Manifest, srcState *store.State, srcRef ImageRef, force bool) error {
	dir := st.ImageDir(dst.Name, dst.Version)
	exists, err := st.Exists(dst.Name, dst.Version)
	if err != nil {
		return err
	}
	if exists && !force {
		return fmt.Errorf("镜像 %s 已存在，使用 --force 覆盖: %w", dst, store.ErrExists)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("创建镜像目录失败: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".tmp-"+filepath.Base(dir)+"-")
	if err != nil {
		return fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }() // 成功后 RemoveAll 对不存在路径静默

	if err := os.WriteFile(filepath.Join(tmp, "source.licore"), srcBytes, 0o644); err != nil {
		return fmt.Errorf("写 source.licore 失败: %w", err)
	}
	// index.json 是清单的展开副本：重排成目标引用的 name/version，
	// 否则 store.images 会把新 tag 显示成源引用的名字与标签。
	idxBytes, err := json.MarshalIndent(retargetManifest(manifest, dst), "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 index.json 失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "index.json"), idxBytes, 0o644); err != nil {
		return fmt.Errorf("写 index.json 失败: %w", err)
	}
	if err := writeStateFile(tmp, newStateFor(srcState, dst, srcRef)); err != nil {
		return err
	}
	return swapDir(tmp, dir, exists)
}

// retargetManifest 返回清单的浅拷贝，name/version 换成目标引用；
// 层与 config 摘要不变（source.licore 是逐字节复制的，摘要仍然成立）。
func retargetManifest(m *image.Manifest, dst ImageRef) *image.Manifest {
	if m == nil {
		return &image.Manifest{Name: dst.Name, Version: dst.Version}
	}
	out := *m
	out.Name = dst.Name
	out.Version = dst.Version
	return &out
}

// putImageDir 与 writeImageDir 同理，但直接以"已打开的产物文件 + 指定 index.json 字节"
// 落地 commit 结果；index.json 与 source.licore 内部清单逐字节一致。
func putImageDir(st *store.Store, dst ImageRef, src *os.File, idxBytes []byte, state stateOnDisk) error {
	dir := st.ImageDir(dst.Name, dst.Version)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("创建镜像目录失败: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".tmp-"+filepath.Base(dir)+"-")
	if err != nil {
		return fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	if err := copyOpenFile(src, filepath.Join(tmp, "source.licore"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "index.json"), idxBytes, 0o644); err != nil {
		return fmt.Errorf("写 index.json 失败: %w", err)
	}
	if err := writeStateFile(tmp, state); err != nil {
		return err
	}
	return swapDir(tmp, dir, false)
}

// swapDir 把临时目录原子地搬到最终位置。replace=true 时先把旧目录挪到同父的
// 旁路名字下，rename 成功后才删除；这样即使 rename 失败，旧镜像也已被放回原位，
// 不存在"旧目录已删、新目录没上去"的丢数据窗口。
func swapDir(tmp, dir string, replace bool) error {
	backup := ""
	if replace {
		if _, err := os.Stat(dir); err == nil {
			// 备份名必须是"尚未存在的路径"，否则 os.Rename 会因目标已存在而失败：
			// 用 MkdirTemp 只为拿到唯一的随机名，随即把它删掉让出路径。
			b, err := os.MkdirTemp(filepath.Dir(dir), ".old-"+filepath.Base(dir)+"-")
			if err != nil {
				return fmt.Errorf("创建备份目录失败: %w", err)
			}
			if err := os.Remove(b); err != nil {
				return fmt.Errorf("释放备份路径失败: %w", err)
			}
			backup = b
			if err := os.Rename(dir, backup); err != nil {
				return fmt.Errorf("移开旧镜像目录失败: %w", err)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("检查镜像目录失败: %w", err)
		}
	} else if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("镜像目录 %s 已存在: %w", dir, store.ErrExists)
	}

	if err := os.Rename(tmp, dir); err != nil {
		if backup != "" {
			// 尽力回滚；回滚也失败时两个错误都要报出来。
			if rerr := os.Rename(backup, dir); rerr != nil {
				return fmt.Errorf("落地镜像目录失败: %w（且回滚旧目录失败: %v）", err, rerr)
			}
		}
		return fmt.Errorf("落地镜像目录失败: %w", err)
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			slog.Warn("清理旧镜像目录失败", "backup", backup, "err", err)
		}
	}
	return nil
}

// writeStateFile 以缩进 JSON 写入 state.json（与 store.Put 的落地格式一致）。
func writeStateFile(dir string, state stateOnDisk) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 state.json 失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o644); err != nil {
		return fmt.Errorf("写 state.json 失败: %w", err)
	}
	return nil
}

// writeFileAtomic 把 src 复制到 dst：目标同目录临时文件 → rename，失败不留半成品。
func writeFileAtomic(dst, src string, force bool) error {
	dst, err := filepath.Abs(dst)
	if err != nil {
		return fmt.Errorf("解析目标路径失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("创建目标目录失败: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开源文件失败: %w", err)
	}
	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-"+filepath.Base(dst)+"-")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后为 no-op

	if err := copyOpenFile(in, tmpName, 0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("设置文件权限失败: %w", err)
	}
	if !force {
		// rename 会静默覆盖，非 force 时必须先判存在。
		if _, err := os.Stat(dst); err == nil {
			return fmt.Errorf("目标文件 %s 已存在，使用 --force 覆盖: %w", dst, store.ErrExists)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("检查目标文件失败: %w", err)
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", dst, err)
	}
	return nil
}

// copyOpenFile 把已打开的 src 内容复制到 dst（新建或截断），保留给定权限位。
func copyOpenFile(src *os.File, dst string, mode os.FileMode) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("重定位源文件失败: %w", err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("创建 %s 失败: %w", dst, err)
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return fmt.Errorf("复制到 %s 失败: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("关闭 %s 失败: %w", dst, err)
	}
	return nil
}

// collectSkipRoots 返回打层时需要整棵跳过的目录：目标镜像目录与其父目录
// （父目录内含同仓库其他 tag，一并跳过以免把镜像文件打进镜像）。
func collectSkipRoots(st *store.Store, imageDir string) []string {
	roots := []string{imageDir, filepath.Dir(imageDir)}
	if st.ImagesRoot() != "" {
		roots = append(roots, st.ImagesRoot())
	}
	return roots
}

// writeRootfsTar 把 root fs 目录树写成未压缩 tar，返回跳过的非常规文件数。
// 语义按任务约定：符号链接原样保留，硬链接按普通文件展开（读其内容），
// socket / 设备节点 / FIFO 跳过并计数，mode 保留但剥掉 setuid/setgid/sticky。
func writeRootfsTar(root, dst string, skipDirs []string) (int, error) {
	skip := make(map[string]bool, len(skipDirs))
	for _, d := range skipDirs {
		if d == "" {
			continue
		}
		if abs, err := filepath.Abs(d); err == nil {
			skip[abs] = true
		}
	}

	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("创建层 tar 失败: %w", err)
	}
	tw := tar.NewWriter(f)

	skipped := 0
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("遍历 rootfs: %w", err)
		}
		abs, aerr := filepath.Abs(p)
		if aerr != nil {
			return fmt.Errorf("解析路径 %s 失败: %w", p, aerr)
		}
		if skip[abs] {
			return fs.SkipDir
		}
		if p == root {
			return nil // rootfs 自身作为层内的 "/"，不写条目
		}
		name, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return fmt.Errorf("计算相对路径失败: %w", rerr)
		}
		name = filepath.ToSlash(name)
		if err := image.SafeArchivePath(name); err != nil {
			return fmt.Errorf("rootfs 条目 %s: %w", p, err)
		}

		info, ierr := d.Info() // 不跟随符号链接
		if ierr != nil {
			return fmt.Errorf("读取 %s 信息失败: %w", p, ierr)
		}
		link := ""
		switch {
		case d.Type()&os.ModeSymlink != 0:
			link, err = os.Readlink(p)
			if err != nil {
				return fmt.Errorf("读取符号链接 %s 失败: %w", p, err)
			}
			if link == "" {
				skipped++
				slog.Warn("跳过空目标符号链接", "path", p)
				return nil
			}
		case d.IsDir() || d.Type().IsRegular():
			// 正常条目
		default:
			skipped++
			slog.Warn("跳过不支持的文件类型", "path", p, "mode", info.Mode().String())
			return nil
		}

		hdr, herr := tar.FileInfoHeader(info, link)
		if herr != nil {
			return fmt.Errorf("构造 %s 的 tar 头失败: %w", p, herr)
		}
		hdr.Name = name
		hdr.Mode = int64(info.Mode().Perm()) // 剥掉 setuid/setgid/sticky
		hdr.Uname, hdr.Gname = "", ""
		hdr.Uid, hdr.Gid = 0, 0
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("写入 %s 的 tar 头失败: %w", p, err)
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			in, oerr := os.Open(p)
			if oerr != nil {
				return fmt.Errorf("打开 %s 失败: %w", p, oerr)
			}
			_, cerr := io.Copy(tw, in)
			closeErr := in.Close()
			if cerr != nil {
				return fmt.Errorf("写入 %s 内容失败: %w", p, cerr)
			}
			if closeErr != nil {
				return fmt.Errorf("关闭 %s 失败: %w", p, closeErr)
			}
		case tar.TypeDir:
			// 仅头
		default:
			// 符号链接：目标已在 hdr.Linkname
		}
		return nil
	})
	if walkErr != nil {
		_ = tw.Close()
		_ = f.Close()
		return 0, walkErr
	}
	if err := tw.Close(); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("收尾层 tar 失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("关闭层 tar 失败: %w", err)
	}
	return skipped, nil
}

// gzipFile 把 src 压缩为 dst（gzip RFC 1952，method=deflate），
// 返回 dst 原始字节的 sha256 摘要与精确字节数。
func gzipFile(src, dst string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, fmt.Errorf("打开层 tar 失败: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, fmt.Errorf("创建层 gz 失败: %w", err)
	}
	hasher := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(out, hasher))
	if _, err := io.Copy(gz, in); err != nil {
		_ = gz.Close()
		_ = out.Close()
		return "", 0, fmt.Errorf("压缩层失败: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = out.Close()
		return "", 0, fmt.Errorf("收尾 gzip 失败: %w", err)
	}
	if err := out.Close(); err != nil {
		return "", 0, fmt.Errorf("关闭层 gz 失败: %w", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return "", 0, fmt.Errorf("读取层大小失败: %w", err)
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), fi.Size(), nil
}

// writeLiCoreArchive 写出外层未压缩 tar：index.json（首条）→ 层 → config blob。
// 三个条目的路径与 index.json 声明逐字一致，字节数为精确值，
// 保证 image.OpenFile 的结构 / 大小 / 摘要三类校验全部通过。
func writeLiCoreArchive(dst string, idxBytes, cfgBytes, layerBytes []byte, blobName string) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("创建镜像文件失败: %w", err)
	}
	tw := tar.NewWriter(f)
	fail := func(err error) error {
		_ = tw.Close()
		_ = f.Close()
		return err
	}

	if err := addFileEntry(tw, image.IndexName, idxBytes); err != nil {
		return fail(err)
	}
	if err := addFileEntry(tw, commitLayerPath, layerBytes); err != nil {
		return fail(err)
	}
	if err := addFileEntry(tw, blobName, cfgBytes); err != nil {
		return fail(err)
	}

	if err := tw.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("收尾镜像 tar 失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭镜像文件失败: %w", err)
	}
	return nil
}

// blobEntryName 把 sha256:<hex> 摘要转成归档内的 blob 条目名 blobs/sha256-<hex>（规范 §1）。
func blobEntryName(digest string) (string, error) {
	algo, hexSum, ok := strings.Cut(digest, ":")
	if !ok || algo == "" || hexSum == "" {
		return "", fmt.Errorf("摘要 %q 格式非法: %w", digest, image.ErrBadManifest)
	}
	return image.BlobsDir + algo + "-" + hexSum, nil
}

// addFileEntry 向后缀为 dst 的 tar 追加一个常规文件条目。
func addFileEntry(tw *tar.Writer, name string, data []byte) error {
	if err := image.SafeArchivePath(name); err != nil {
		return fmt.Errorf("归档条目 %s: %w", name, err)
	}
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		ModTime:  time.Unix(0, 0).UTC(),
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("写入条目 %s 头失败: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("写入条目 %s 内容失败: %w", name, err)
	}
	return nil
}
