// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package build

import (
	"os"
	"syscall"
)

// inodeOf 返回文件底层的 (dev, ino) 与硬链接数，用于识别同一 inode 的多个名字。
//
// nlink 是判断"是否值得查表"的关键：绝大多数文件 nlink == 1，
// 调用方据此短路，避免为每个文件做一次 map 查找。
func inodeOf(fi os.FileInfo) (dev, ino, nlink uint64, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), uint64(st.Nlink), true
}
