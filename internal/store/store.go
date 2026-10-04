// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

// Package store 实现镜像在本地数据目录的落地与列举（规范第 5 节）。
// 阶段 1 只做 pull 落地与最小状态记录；层解包合并、引用计数在阶段 2。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/LiStudioorg/licore/internal/image"
)

// Store 是 LiCore 本地数据目录（默认 ~/.licore，可用 --data-dir 或 $LICORE_HOME 覆盖）。
type Store struct {
	// Root 是数据目录根路径。
	Root string
}

// Open 返回数据目录，Root 为空时依次取 $LICORE_HOME、~/.licore。
func Open(root string) (*Store, error) {
	if root == "" {
		root = os.Getenv("LICORE_HOME")
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("确定数据目录失败: %w", err)
		}
		root = filepath.Join(home, ".licore")
	}
	return &Store{Root: root}, nil
}

// State 是镜像落地后的状态文件 state.json 内容。
type State struct {
	// Ref 是镜像引用 name:version。
	Ref string `json:"ref"`
	// PulledAt 是落地时间（UTC，RFC 3339）。
	PulledAt string `json:"pulledAt"`
	// SourcePath 是来源 .licore 文件路径（本地 pull 记录）。
	SourcePath string `json:"sourcePath"`
	// SourceSizeBytes 是来源文件字节数。
	SourceFileBytes int64 `json:"sourceSizeBytes"`
	// LayersVerified 表示落地时全部层摘要校验通过。
	LayersVerified bool `json:"layersVerified"`
}

// ImageDir 返回镜像落地目录 <root>/images/<name>/<version>。
func (s *Store) ImageDir(name, version string) string {
	return filepath.Join(s.Root, "images", name, version)
}

// Exists 报告镜像是否已落地。
func (s *Store) Exists(name, version string) (bool, error) {
	_, err := os.Stat(filepath.Join(s.ImageDir(name, version), "state.json"))
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, fmt.Errorf("检查镜像状态失败: %w", err)
	}
}

// Put 校验并落地一个本地 .licore 文件。已存在且 force=false 时返回 ErrExists。
// 落地是原子的：先写入临时目录，全部成功后 rename。
//
// allowArchMismatch 对应规范第 4 节规则 6 的 `--allow-arch-mismatch` 逃生口：
// 为 false 时镜像 architecture/os 必须与宿主匹配（默认，防交叉镜像被误跑）；
// 为 true 时跳过该校验，供 `licore build --arch <其他架构>` 交叉构建与显式
// 导入交叉镜像使用——产物仍会被完整校验层摘要，只是不再比对宿主平台。
func (s *Store) Put(srcPath string, force, allowArchMismatch bool) (*image.Loaded, error) {
	loaded, err := image.OpenFile(srcPath)
	if err != nil {
		return nil, err
	}
	if !allowArchMismatch {
		if err := loaded.CheckPlatform(); err != nil {
			return nil, err
		}
	}
	if err := loaded.VerifyLayers(); err != nil {
		return nil, err
	}

	m := loaded.Manifest
	dir := s.ImageDir(m.Name, m.Version)
	ok, err := s.Exists(m.Name, m.Version)
	if err != nil {
		return nil, err
	}
	if ok && !force {
		return nil, fmt.Errorf("镜像 %s 已存在，使用 --force 覆盖: %w", m.Ref(), ErrExists)
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("打开源文件失败: %w", err)
	}
	defer func() { _ = src.Close() }()
	fi, err := src.Stat()
	if err != nil {
		return nil, fmt.Errorf("读取源文件信息失败: %w", err)
	}

	// 确保 <root>/images/<name> 存在，临时目录与最终目录同父，保证 rename 原子。
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, fmt.Errorf("创建镜像目录失败: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".tmp-"+filepath.Base(dir)+"-")
	if err != nil {
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }() // 成功后 RemoveAll 对不存在路径静默

	if err := copyFile(src, filepath.Join(tmp, "source.licore"), 0o644); err != nil {
		return nil, err
	}
	idxBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化 index.json 失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "index.json"), idxBytes, 0o644); err != nil {
		return nil, fmt.Errorf("写 index.json 失败: %w", err)
	}
	state := State{
		Ref:             m.Ref(),
		PulledAt:        time.Now().UTC().Format(time.RFC3339),
		SourcePath:      srcPath,
		SourceFileBytes: fi.Size(),
		LayersVerified:  true,
	}
	stBytes, err := json.MarshalIndent(&state, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化 state.json 失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "state.json"), stBytes, 0o644); err != nil {
		return nil, fmt.Errorf("写 state.json 失败: %w", err)
	}

	if ok { // --force：先移除旧目录再原子替换
		if err := os.RemoveAll(dir); err != nil {
			return nil, fmt.Errorf("移除旧镜像目录失败: %w", err)
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return nil, fmt.Errorf("落地镜像目录失败: %w", err)
	}
	return loaded, nil
}

// ReadState 读取已落地镜像的状态。
func (s *Store) ReadState(name, version string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(s.ImageDir(name, version), "state.json"))
	if err != nil {
		return nil, fmt.Errorf("读取 state.json 失败: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("解析 state.json 失败: %w", err)
	}
	return &st, nil
}

// ImagesRoot 返回镜像根目录。
func (s *Store) ImagesRoot() string { return filepath.Join(s.Root, "images") }

// BootMarker 返回开机自启引导标记文件路径 <root>/boot/marker。
// 该文件存在即表示已启用或已询问过，CLI 不再重复引导。
func (s *Store) BootMarker() string { return filepath.Join(s.Root, "boot", "marker") }

// EnsureBootDir 创建 boot 标记所在目录，供写标记前调用。
func (s *Store) EnsureBootDir() error {
	if err := os.MkdirAll(filepath.Dir(s.BootMarker()), 0o755); err != nil {
		return fmt.Errorf("创建 boot 目录失败: %w", err)
	}
	return nil
}

func copyFile(src *os.File, dst string, mode os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("创建 %s 失败: %w", dst, err)
	}
	if _, err := out.ReadFrom(src); err != nil {
		_ = out.Close()
		return fmt.Errorf("复制失败: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("关闭文件失败: %w", err)
	}
	return nil
}
