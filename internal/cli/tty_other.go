// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux && !android && !darwin

package cli

import "os"

// isTTY 报告文件是否为终端。
//
// 本回退实现仅覆盖非 linux/darwin 的构建目标（LiCore 的运行平台只有 linux /
// android / darwin，见 AGENTS.md《平台后端》）。这些平台没有可移植的 termios
// ioctl 常量，故退而用 ModeCharDevice。注意它会把 /dev/null 也当作"终端"，
// 在真正的交互提示上不精确——但这只影响非受支持平台，且这些平台不提供
// 本机 docker 转换路径。
func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
