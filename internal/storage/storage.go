// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package storage

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/LiStudioorg/licore/internal/image"
)

const (
	// whiteoutPrefix 与 opaqueMarker 是 docs/image-spec.md §2.1 的删除约定。
	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh..wh..opq"

	// maxTarEntries 限制单层条目数，防御解压炸弹。
	maxTarEntries = 1 << 20

	doneFileName = ".done" // 解包完成标记，内容为层摘要十六进制
)

var hexDigestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// LayersRoot 返回层内容寻址目录 <storeRoot>/layers/sha256。
func LayersRoot(storeRoot string) string {
	return filepath.Join(storeRoot, "layers", "sha256")
}

// LayerFSDir 返回某层（十六进制摘要）的 fs 目录，不检查存在性。
func LayerFSDir(storeRoot, hexDigest string) string {
	return filepath.Join(LayersRoot(storeRoot), hexDigest, "fs")
}

// LayerUnpacked 报告层是否已完整解包（存在 .done 且摘要一致）。
func LayerUnpacked(storeRoot, hexDigest string) bool {
	data, err := os.ReadFile(filepath.Join(LayersRoot(storeRoot), hexDigest, doneFileName))
	return err == nil && strings.TrimSpace(string(data)) == hexDigest
}

// UnpackResult 是一次解包的产出。
type UnpackResult struct {
	// DigestHex 是层 tar.gz 原始字节的 SHA-256（十六进制小写）。
	DigestHex string
	// FSDir 是层内容目录 layers/sha256/<hex>/fs。
	FSDir string
}

// UnpackFile 把层文件 tar.gz 解压进 storeRoot 的内容寻址存储。
// wantDigest 非空时先流式校验原始字节摘要，不一致返回 ErrBadDigest；
// 为空则自我计算并采用。已解包过的层直接复用。
//
// 解包是差异视图（diff view）：whiteout 文件原样保留，删除语义由 Merge 应用。
func UnpackFile(layerPath, wantDigest, storeRoot string) (*UnpackResult, error) {
	if wantDigest != "" && !hexDigestRE.MatchString(strings.TrimPrefix(wantDigest, "sha256:")) {
		return nil, fmt.Errorf("摘要 %q 非法: %w", wantDigest, ErrBadDigest)
	}
	want := strings.TrimPrefix(wantDigest, "sha256:")

	layers := LayersRoot(storeRoot)
	if err := os.MkdirAll(layers, 0o755); err != nil {
		return nil, fmt.Errorf("创建层目录: %w", err)
	}
	cleanupStaleTmp(layers)

	// 先猜摘要复用：仅当调用方给出摘要且已解包时跳过全量读取。
	if want != "" && LayerUnpacked(storeRoot, want) {
		return &UnpackResult{DigestHex: want, FSDir: LayerFSDir(storeRoot, want)}, nil
	}

	f, err := os.Open(layerPath)
	if err != nil {
		return nil, fmt.Errorf("打开层文件: %w", err)
	}
	defer func() { _ = f.Close() }()

	tmpDir, err := os.MkdirTemp(layers, ".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("创建临时解包目录: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }() // 成功 rename 后为 no-op

	fsDir := filepath.Join(tmpDir, "fs")
	if err := os.Mkdir(fsDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 fs 目录: %w", err)
	}

	hasher := sha256.New()
	gz, err := gzip.NewReader(io.TeeReader(f, hasher))
	if err != nil {
		return nil, fmt.Errorf("gzip 头非法: %w: %w", err, ErrCorruptLayer)
	}
	if err := extract(gz, fsDir); err != nil {
		_ = gz.Close()
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("读取 gzip 尾: %w: %w", err, ErrCorruptLayer)
	}

	hexStr := hex.EncodeToString(hasher.Sum(nil))
	if want != "" && hexStr != want {
		return nil, fmt.Errorf("层摘要 %s 与声明 %s 不符: %w", hexStr, want, ErrBadDigest)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, doneFileName), []byte(hexStr), 0o644); err != nil {
		return nil, fmt.Errorf("写完成标记: %w", err)
	}

	final := filepath.Join(layers, hexStr)
	if LayerUnpacked(storeRoot, hexStr) {
		return &UnpackResult{DigestHex: hexStr, FSDir: filepath.Join(final, "fs")}, nil
	}
	if err := os.Rename(tmpDir, final); err != nil {
		// 并发解包同一层：对手可能抢先落位，摘要一致即复用。
		if LayerUnpacked(storeRoot, hexStr) {
			return &UnpackResult{DigestHex: hexStr, FSDir: filepath.Join(final, "fs")}, nil
		}
		return nil, fmt.Errorf("落位层目录: %w", err)
	}
	return &UnpackResult{DigestHex: hexStr, FSDir: filepath.Join(final, "fs")}, nil
}

// extract 把 tar 流解到 fsDir，执行全部安全规则（见 doc.go）。
func extract(r io.Reader, fsDir string) error {
	tr := tar.NewReader(r)
	seen := make(map[string]bool, 256)
	created := make(map[string]bool, 256) // 本层已落盘的 regular/symlink 路径（硬链接合法域）
	n := 0

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar 流损坏: %w: %w", err, ErrCorruptLayer)
		}
		if n++; n > maxTarEntries {
			return fmt.Errorf("单层条目超过 %d: %w", maxTarEntries, ErrCorruptLayer)
		}
		// tar 惯例：目录条目名以 "/" 结尾。仅去掉这一个尾斜杠做规范化，
		// 不改变语义；其余不安全形式（含 "./" 前缀）一律交给 SafeArchivePath 拒绝。
		name := strings.TrimSuffix(hdr.Name, "/")
		if err := image.SafeArchivePath(name); err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("条目 %q 重复: %w", name, ErrDuplicateEntry)
		}
		seen[name] = true

		target := filepath.Join(fsDir, filepath.FromSlash(name))
		mode := fs.FileMode(hdr.Mode).Perm() &^ 0o7000 // 剥离 setuid/setgid/sticky

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := ensureParents(fsDir, target); err != nil {
				return err
			}
			if err := os.Mkdir(target, mode); err != nil && !os.IsExist(err) {
				return fmt.Errorf("创建目录 %q: %w", name, err)
			} else if err == nil {
				_ = os.Chmod(target, mode) // 绕开 umask，权限以层内为准
			}

		case tar.TypeReg:
			if hdr.Size < 0 {
				return fmt.Errorf("条目 %q 大小为负: %w", name, ErrCorruptLayer)
			}
			if err := ensureParents(fsDir, target); err != nil {
				return err
			}
			if err := writeRegFile(target, mode, name, tr, hdr.Size); err != nil {
				return err
			}
			created[name] = true

		case tar.TypeSymlink:
			if err := ensureParents(fsDir, target); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("创建符号链接 %q: %w", name, err)
			}
			created[name] = true

		case tar.TypeLink:
			lt := hdr.Linkname
			if err := image.SafeArchivePath(lt); err != nil {
				return fmt.Errorf("硬链接目标: %w", err)
			}
			if !created[lt] {
				return fmt.Errorf("硬链接 %q 指向本层未创建或不存在的路径 %q: %w", name, lt, ErrCorruptLayer)
			}
			if err := ensureParents(fsDir, target); err != nil {
				return err
			}
			if err := os.Link(filepath.Join(fsDir, filepath.FromSlash(lt)), target); err != nil {
				return fmt.Errorf("创建硬链接 %q: %w", name, err)
			}
			created[name] = true

		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			slog.Debug("剥离设备节点/FIFO", "entry", name, "type", string(hdr.Typeflag))

		default:
			slog.Debug("忽略未知 tar 类型", "entry", name, "type", string(hdr.Typeflag))
		}
	}
}

// writeRegFile 以剥离危险位后的权限写入普通文件。
func writeRegFile(target string, mode fs.FileMode, name string, r io.Reader, size int64) error {
	if fi, err := os.Lstat(target); err == nil {
		if fi.IsDir() {
			return fmt.Errorf("条目 %q 与已存在目录冲突: %w", name, ErrCorruptLayer)
		}
		_ = os.Remove(target) // 覆盖而非跟随符号链接写穿
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("创建文件 %q: %w", name, err)
	}
	w, err := io.Copy(f, io.LimitReader(r, size))
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("写入 %q: %w: %w", name, err, ErrCorruptLayer)
	}
	if w != size {
		_ = f.Close()
		return fmt.Errorf("条目 %q 截断：期望 %d 字节实得 %d: %w", name, size, w, ErrCorruptLayer)
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("设置权限 %q: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭 %q: %w", name, err)
	}
	return nil
}

// ensureParents 保证 target 的所有祖先组件要么是真实目录、要么被本函数
// 创建为目录；任何组件若是符号链接或普通文件即拒绝（防符号链接穿透写）。
//
// 父链判定委托给 image.SafeExtractPath —— 与 internal/convert 共用同一份
// 实现。历史上这里是私有实现，而 convert 那份是空白的，于是 convert 漏了
// 符号链接穿透（见 docs/security-audit-v2.md H-1）。**不要再复制这段逻辑**，
// 解压类代码一律走 image 包的公共校验。
func ensureParents(base, target string) error {
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("目标 %q 逃逸出 %q: %w", target, base, image.ErrUnsafePath)
	}
	if _, err := image.SafeExtractPath(base, filepath.ToSlash(rel), false); err != nil {
		return err
	}
	// SafeExtractPath 只校验、不创建；这里补齐创建语义。
	comps := strings.Split(rel, string(filepath.Separator))
	cur := base
	for _, comp := range comps[:len(comps)-1] {
		cur = filepath.Join(cur, comp)
		if _, err := os.Lstat(cur); errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(cur, 0o755); err != nil && !os.IsExist(err) {
				return fmt.Errorf("创建中间目录 %q: %w", cur, err)
			}
		}
	}
	return nil
}

// MergeLayers 按顺序把已解包的层叠加进 targetDir（后层覆盖前层），
// 应用 whiteout 与 opaque 目录删除语义，并再次剥离危险权限位。
// orderedDigests 为 64 位十六进制摘要序列（index.json 的 applyOrder 顺序）。
func MergeLayers(storeRoot string, orderedDigests []string, targetDir string) error {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("创建 rootfs 目录: %w", err)
	}
	for i, d := range orderedDigests {
		if !hexDigestRE.MatchString(d) {
			return fmt.Errorf("第 %d 层摘要 %q 非法: %w", i+1, d, ErrBadDigest)
		}
		if !LayerUnpacked(storeRoot, d) {
			return fmt.Errorf("第 %d 层 sha256:%s 尚未解包: %w", i+1, d, ErrLayerMissingLocal)
		}
		if err := mergeOne(LayerFSDir(storeRoot, d), targetDir); err != nil {
			return fmt.Errorf("合并第 %d 层 sha256:%s: %w", i+1, d, err)
		}
	}
	return nil
}

// mergeOne 把单层 diff 视图应用到 rootfs。WalkDir 的字典序保证父目录先于
// 子项；opaque 标记排序在同目录 .wh.* 之前，清除后残留的同目录早期写入一并失效。
func mergeOne(fsDir, targetDir string) error {
	return filepath.WalkDir(fsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(fsDir, p)
		if err != nil || rel == "." {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		base := path.Base(relSlash)
		dirSlash := path.Dir(relSlash)
		target := filepath.Join(targetDir, filepath.FromSlash(relSlash))
		targetParent := filepath.Join(targetDir, filepath.FromSlash(dirSlash))

		// whiteout 与 opaque 只作用于目标 rootfs，不写入。操作上点
		// （父目录）各组件必须是真实目录。
		if base == opaqueMarker {
			if err := chainOK(targetDir, dirSlash, true); err != nil {
				return err
			}
			return clearChildren(targetParent)
		}
		if strings.HasPrefix(base, whiteoutPrefix) && len(base) > len(whiteoutPrefix) {
			if err := chainOK(targetDir, dirSlash, true); err != nil {
				return err
			}
			return deleteEntry(targetParent, base[len(whiteoutPrefix):])
		}

		// 写入类条目：先校验目标路径链，符号链接组件一律拒绝（finalMustDir
		// 对 regular/symlink 为 false：替换链接本身是安全的原子覆盖）。
		if err := chainOK(targetDir, relSlash, d.IsDir()); err != nil {
			return err
		}

		switch {
		case d.IsDir():
			if fi, err := os.Lstat(target); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
				return fmt.Errorf("目录 %q 目标是符号链接: %w", relSlash, image.ErrUnsafePath)
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("创建目录 %q: %w", relSlash, err)
			}
			fi, err := d.Info()
			if err == nil {
				_ = os.Chmod(target, fi.Mode().Perm()&^0o7000)
			}
			return nil
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return fmt.Errorf("读取符号链接 %q: %w", relSlash, err)
			}
			if err := removeAny(target); err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			src, err := os.Open(p)
			if err != nil {
				return fmt.Errorf("打开层文件 %q: %w", relSlash, err)
			}
			defer func() { _ = src.Close() }()
			fi, err := d.Info()
			if err != nil {
				return fmt.Errorf("读取层文件信息 %q: %w", relSlash, err)
			}
			tmp := target + ".tmp-merge"
			dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return fmt.Errorf("创建目标文件 %q: %w", relSlash, err)
			}
			if _, err := io.Copy(dst, src); err != nil {
				_ = dst.Close()
				_ = os.Remove(tmp)
				return fmt.Errorf("写入 %q: %w", relSlash, err)
			}
			_ = dst.Chmod(fi.Mode().Perm() &^ 0o7000)
			if err := dst.Close(); err != nil {
				return fmt.Errorf("关闭 %q: %w", relSlash, err)
			}
			if err := removeAny(target); err != nil {
				return err
			}
			return os.Rename(tmp, target)
		default:
			slog.Debug("合并时跳过非普通条目", "entry", relSlash)
			return nil
		}
	})
}

// deleteEntry 在 parent 下删除 name（whiteout）。要求 parent 各组件为真实
// 目录（防经由符号链接删除 rootfs 之外的内容）。
func deleteEntry(parent, name string) error {
	if err := realDirChain(parent); err != nil {
		return err
	}
	p := filepath.Join(parent, name)
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return nil // 删除不存在的条目：合法 no-op
	}
	return os.RemoveAll(p)
}

// clearChildren 清空目录内容（opaque 目录语义），目录不存在则创建。
func clearChildren(dir string) error {
	if err := realDirChain(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("清空目录 %q: %w", dir, err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("读取目录 %q: %w", dir, err)
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("清空 %q/%q: %w", dir, e.Name(), err)
		}
	}
	return nil
}

// realDirChain 校验 dir 的每一级组件为真实目录（不存在视为通过）。
func realDirChain(dir string) error {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("检查 %q: %w", dir, err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("whiteout 路径经由符号链接 %q: %w", dir, image.ErrUnsafePath)
	case !fi.IsDir():
		return fmt.Errorf("whiteout 路径 %q 不是目录: %w", dir, ErrCorruptLayer)
	}
	return nil
}

// confirmChildren 与 realDirChain 等共享的最小目录链判定。
//
// realDirChainOK 是 realDirChain 的内部实现别名占位。
//
// chainOK 校验 rootfs 内相对路径 relSlash 的每一级组件：
// 不存在（将创建）→ 合法；符号链接 → 拒绝；普通文件 → 拒绝。
// finalMustDir=false 时末级允许任意已存在类型（覆盖替换是安全的）。
func chainOK(base, relSlash string, finalMustDir bool) error {
	if relSlash == "." || relSlash == "" {
		return nil
	}
	comps := strings.Split(relSlash, "/")
	cur := base
	for i, comp := range comps {
		cur = filepath.Join(cur, comp)
		fi, err := os.Lstat(cur)
		final := i == len(comps)-1
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil // 后续层级尚不存在，Mkdir 时才创建
		case err != nil:
			return fmt.Errorf("检查路径组件 %q: %w", cur, err)
		case fi.Mode()&fs.ModeSymlink != 0:
			if final && !finalMustDir {
				return nil // 覆盖符号链接本身：先删后写，不跟随
			}
			return fmt.Errorf("拒绝经由符号链接合并写出 %q: %w", cur, image.ErrUnsafePath)
		case !fi.IsDir():
			if final && !finalMustDir {
				return nil
			}
			return fmt.Errorf("路径组件 %q 不是目录: %w", cur, ErrCorruptLayer)
		}
	}
	return nil
}

// removeAny 删除路径（文件或符号链接），目录整体移除；不存在为 no-op。
func removeAny(p string) error {
	if err := os.RemoveAll(p); err != nil {
		return fmt.Errorf("移除 %q: %w", p, err)
	}
	return nil
}

// cleanupStaleTmp 清理崩溃残留的临时目录（仅 mtime 超过 1 小时的 .tmp-*，
// 避免误删并发解包正在进行中的目录）。
func cleanupStaleTmp(layers string) {
	ents, err := os.ReadDir(layers)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < time.Hour {
			continue
		}
		_ = os.RemoveAll(filepath.Join(layers, e.Name()))
	}
}
