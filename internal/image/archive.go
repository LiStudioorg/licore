// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package image

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

// IndexName 是镜像清单在外层归档内的固定路径。
const IndexName = "index.json"

// BlobsDir 是小对象目录前缀，条目名 = blobs/<algo>-<digest-hex>（规范 §1 / §3.3）。
const BlobsDir = "blobs/"

// EntryInfo 是外层归档中一个常规文件条目的摘要信息。
type EntryInfo struct {
	SizeBytes int64
}

// Loaded 是 OpenFile 的成功结果：已解析并交叉校验过的镜像文件视图。
type Loaded struct {
	// Path 是本地 .licore 文件路径。
	Path string
	// Manifest 是解析后的 index.json。
	Manifest *Manifest
	// Config 是已校验并解析好的 config 小对象。
	Config *Config

	entries map[string]EntryInfo // 归档内全部常规文件条目
}

// Entry 返回归档条目信息。
func (l *Loaded) Entry(name string) (EntryInfo, bool) {
	e, ok := l.entries[name]
	return e, ok
}

// configBlobPath 返回 config blob 在归档内的预期条目名，如 blobs/sha256-<hex>。
func configBlobPath(digest string) string {
	algo, hexsum, _ := strings.Cut(digest, ":")
	return BlobsDir + algo + "-" + hexsum
}

// OpenFile 打开本地 .licore 文件，流式扫描外层 tar，完成规范第 4 节
// 清单类、结构类与 config blob 校验（层全量摘要在 VerifyLayers 中重算）。
func OpenFile(path string) (*Loaded, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开镜像文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	var (
		entries   = map[string]EntryInfo{}
		indexData []byte
		seenIndex bool
	)
	// 第一遍：扫描条目、读取 index.json（只读清单，config blob 待知道摘要后再读）。
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取归档失败: %w", err)
		}
		// 目录条目先行跳过：任何常规打包工具（`tar -cf x.licore layers/ blobs/
		// index.json` 这类手写归档）都会写入 "layers/" 这种带尾斜杠的目录条目，
		// 而尾斜杠在 SafeArchivePath 眼里是一个空路径段。目录条目不携带文件
		// 数据、也不参与落盘，对它们做路径规范化检查既无安全收益又挡住合法
		// 输入，因此在类型分流里直接 continue，校验只针对有数据的条目。
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if err := SafeArchivePath(hdr.Name); err != nil {
			return nil, err
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			// 继续
		case tar.TypeSymlink, tar.TypeLink:
			target := hdr.Linkname
			if strings.HasPrefix(target, "/") || strings.Contains(target, "..") {
				return nil, fmt.Errorf("条目 %q 链接目标 %q 逃逸: %w", hdr.Name, target, ErrUnsafePath)
			}
			continue
		default:
			return nil, fmt.Errorf("条目 %q 类型 %c 不允许出现在外层归档: %w", hdr.Name, hdr.Typeflag, ErrUnsafePath)
		}
		if _, dup := entries[hdr.Name]; dup {
			return nil, fmt.Errorf("归档条目 %q 重复: %w", hdr.Name, ErrBadManifest)
		}
		entries[hdr.Name] = EntryInfo{SizeBytes: hdr.Size}

		if hdr.Name == IndexName {
			if seenIndex {
				return nil, fmt.Errorf("index.json 重复: %w", ErrBadManifest)
			}
			seenIndex = true
			data, err := io.ReadAll(io.LimitReader(tr, MaxIndexBytes+1))
			if err != nil {
				return nil, fmt.Errorf("读取 index.json 失败: %w", err)
			}
			indexData = data
		}
	}
	if !seenIndex {
		return nil, fmt.Errorf("归档缺少 %s: %w", IndexName, ErrBadManifest)
	}

	m, err := ParseManifest(indexData)
	if err != nil {
		return nil, err
	}

	// 交叉校验：声明的层必须存在且大小一致（规则 3）。
	for _, la := range m.Layers {
		e, ok := entries[la.Path]
		if !ok {
			return nil, fmt.Errorf("index 声明的层 %s 不在归档中: %w", la.Path, ErrLayerMissing)
		}
		if e.SizeBytes != la.SizeBytes {
			return nil, fmt.Errorf("层 %s 大小 %d != index 声明 %d: %w", la.Path, e.SizeBytes, la.SizeBytes, ErrSizeMismatch)
		}
	}

	// config blob 必须存在且大小一致（规则 8）。
	cfgPath := configBlobPath(m.Config.Digest)
	ce, ok := entries[cfgPath]
	if !ok {
		return nil, fmt.Errorf("config blob %s 缺失: %w", cfgPath, ErrConfigMissing)
	}
	if ce.SizeBytes != m.Config.SizeBytes {
		return nil, fmt.Errorf("config blob %s 大小 %d != index 声明 %d: %w", cfgPath, ce.SizeBytes, m.Config.SizeBytes, ErrSizeMismatch)
	}

	// 第二遍：重读 config blob，校验摘要并解析。
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("重读归档失败: %w", err)
	}
	cfgData, err := readEntry(f, cfgPath)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(cfgData)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != m.Config.Digest {
		return nil, fmt.Errorf("config blob 摘要 %s != 声明 %s: %w", got, m.Config.Digest, ErrDigestMismatch)
	}
	cfg, err := ParseConfig(cfgData)
	if err != nil {
		return nil, err
	}

	return &Loaded{Path: path, Manifest: m, Config: cfg, entries: entries}, nil
}

// readEntry 在归档中查找并完整读取指定条目。
func readEntry(f *os.File, name string) ([]byte, error) {
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("归档条目 %q 缺失: %w", name, ErrConfigMissing)
		}
		if err != nil {
			return nil, fmt.Errorf("读取归档失败: %w", err)
		}
		if hdr.Name != name {
			continue
		}
		if hdr.Size > MaxConfigBytes {
			return nil, fmt.Errorf("条目 %s 大小 %d 超上限: %w", name, hdr.Size, ErrIndexTooLarge)
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxConfigBytes+1))
		if err != nil {
			return nil, fmt.Errorf("读取条目 %s 失败: %w", name, err)
		}
		return data, nil
	}
}

// VerifyLayers 重算全部层摘要并与 index 比对（规则 4）。流式读取，内存占用 O(1)。
func (l *Loaded) VerifyLayers() error {
	f, err := os.Open(l.Path)
	if err != nil {
		return fmt.Errorf("打开镜像文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	want := make(map[string]string, len(l.Manifest.Layers))
	for _, la := range l.Manifest.Layers {
		want[la.Path] = la.Digest
	}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取归档失败: %w", err)
		}
		digestWant, ok := want[hdr.Name]
		if !ok {
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, tr); err != nil {
			return fmt.Errorf("摘要层 %s 失败: %w", hdr.Name, err)
		}
		got := "sha256:" + hex.EncodeToString(h.Sum(nil))
		if got != digestWant {
			return fmt.Errorf("层 %s 摘要 %s != 声明 %s: %w", hdr.Name, got, digestWant, ErrDigestMismatch)
		}
		delete(want, hdr.Name)
	}
	for p := range want {
		return fmt.Errorf("层 %s 缺失: %w", p, ErrLayerMissing)
	}
	return nil
}

// CheckPlatform 比较镜像声明的平台与当前运行平台（规则 6）。
// 按规范：android 宿主可运行 os=linux 镜像。
func (l *Loaded) CheckPlatform() error {
	hostOS, hostArch := runtime.GOOS, runtime.GOARCH
	m := l.Manifest
	if m.Architecture != hostArch {
		return fmt.Errorf("镜像 %s/%s 与宿主 %s/%s 不匹配: %w", m.OS, m.Architecture, hostOS, hostArch, ErrArchMismatch)
	}
	if m.OS == hostOS {
		return nil
	}
	if hostOS == "android" && m.OS == "linux" {
		return nil
	}
	return fmt.Errorf("镜像 %s/%s 与宿主 %s/%s 不匹配: %w", m.OS, m.Architecture, hostOS, hostArch, ErrArchMismatch)
}

// ExtractFile 把外层归档里的条目 name 流式解出到目标路径 dst（覆盖写）。
// 供 `licore run` 把层 tar.gz 取出交给 storage.UnpackFile；条目不存在报 ErrConfigMissing。
func (l *Loaded) ExtractFile(name, dst string) error {
	if _, ok := l.entries[name]; !ok {
		return fmt.Errorf("归档条目 %q 缺失: %w", name, ErrConfigMissing)
	}
	f, err := os.Open(l.Path)
	if err != nil {
		return fmt.Errorf("打开镜像文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("归档条目 %q 缺失: %w", name, ErrConfigMissing)
		}
		if err != nil {
			return fmt.Errorf("扫描归档失败: %w", err)
		}
		if hdr.Name != name {
			continue
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("创建 %s 失败: %w", dst, err)
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("解出条目 %q 失败: %w", name, err)
		}
		return nil
	}
}
