// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// createFile 创建（或截断）一个用于接收 docker export 输出的文件。
func createFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("convert: 创建目录失败: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("convert: 创建文件 %s 失败: %w", path, err)
	}
	return f, nil
}

// copyFile 把 src 复制到 dst（覆盖已存在的目标）。
// 先写同目录临时文件再 rename，避免写出半个文件被当成有效产物。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开源文件失败: %w", err)
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("创建目标目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-"+filepath.Base(dst)+"-")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入目标文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭目标文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("设置目标文件权限失败: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("替换目标文件失败: %w", err)
	}
	return nil
}
