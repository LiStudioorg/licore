// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !unix

package build

import "os"

// inodeOf 在非 Unix 平台上无法获取 inode 信息，恒返回 ok=false，
// 由调用方退化为「每个文件各写一份内容」——语义仍然正确，只是不省体积。
// LiCore 的目标平台（Linux / Android / macOS）都在 unix 构建标签内，
// 这里只是为了保证全仓库仍可交叉编译到其他 GOOS。
func inodeOf(os.FileInfo) (dev, ino, nlink uint64, ok bool) {
	return 0, 0, 0, false
}
