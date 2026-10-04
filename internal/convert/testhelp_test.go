// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"archive/tar"
	"os"
	"testing"

	"github.com/LiStudioorg/licore/internal/build"
)

// parseBoxfileText 用 LiCore 真实的 Boxfile 解析器解析一段文本。
//
// 特意走 build.ParseBoxfile 而不是自己写个宽松校验：生成逻辑的价值就在于
// 「产物能被真实解析器接受」，用真实解析器才有意义。
func parseBoxfileText(_ *testing.T, text string) (*build.Boxfile, error) {
	return build.ParseBoxfile([]byte(text))
}

// writeTar 构造一个含指定条目的未压缩 tar，用于测试解压与逃逸防护。
func writeTar(path string, entries map[string]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	for name, content := range entries {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = f.Close()
			return err
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
