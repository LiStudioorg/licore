// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build darwin

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

// tiocgeta 是 TIOCGETA 的 ioctl 请求号（darwin 上取终端属性，等价于 linux 的 TCGETS）。
const tiocgeta = 0x40487413

// isTTY 报告文件是否为真正的终端。判据与 linux 侧一致：ioctl 取 termios 成功。
// 详见 tty_linux.go 的说明（os.ModeCharDevice 无法区分 /dev/null 与终端）。
func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	fd := f.Fd()
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, tiocgeta, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
