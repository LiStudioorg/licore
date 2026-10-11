// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux || android

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

// tcgets 是 TCGETS 的 ioctl 请求号（linux/amd64、arm64、386、arm、riscv64 均相同）。
const tcgets = 0x5401

// isTTY 报告文件是否为**真正的终端**（而非管道 / 重定向文件 / /dev/null）。
//
// 为什么不能用 os.ModeCharDevice：/dev/null 也是字符设备，"重定向到 /dev/null
// 或 /dev/tty" 与"交互终端"无法区分——真机反向验证时正是踩到了这一点
// （`licore convert x </dev/null` 误入交互并读 EOF 报错）。唯一可靠判据是
// ioctl(TCGETS)：内核把 termios 写入我们提供的缓冲区，成功即说明是终端。
//
// 必须传**有效指针**：传 nil 得到 EFAULT，与"不是终端"的 ENOTTY 无法区分。
// 不引入 golang.org/x/term（依赖白名单只批 cobra）。
func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	fd := f.Fd()
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, tcgets, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
