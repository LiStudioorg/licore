// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package execns

import (
	"syscall"
	"time"
)

// wait4 回收指定 pid 并返回退出码（信号死亡为 128+signum）。
//
// 用 syscall.Wait4 而非 os/exec 的 Wait：Enter 返回后 *exec.Cmd 已不可达，
// 这里按 pid 直接回收。WUNTRACED 不设——只关心终止。
func wait4(pid int) (int, error) {
	var ws syscall.WaitStatus
	for {
		_, err := syscall.Wait4(pid, &ws, 0, nil)
		if err == syscall.EINTR {
			continue // 被信号打断，重试
		}
		if err != nil {
			return -1, err
		}
		return exitCodeOf(ws), nil
	}
}

// exitCodeOf 把 wait status 转成退出码，信号死亡沿用 shell 约定 128+signum。
func exitCodeOf(ws syscall.WaitStatus) int {
	if ws.Exited() {
		return ws.ExitStatus()
	}
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return -1
}

// sleepBriefly 短暂休眠；返回 false 表示调用方应停止等待。
func sleepBriefly() bool {
	time.Sleep(5 * time.Millisecond)
	return true
}
