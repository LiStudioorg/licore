// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

// Package execns 的非 Linux stub。
//
// exec 依赖 Linux 命名空间（nsenter 只能进入 Linux namespace），macOS /
// Windows 上不可用，返回 ErrUnsupported 明确提示。
package execns

import (
	"errors"
	"fmt"
)

// ErrNoNsenter 与 Linux 实现同名，保证调用方无需按平台分支。
var ErrNoNsenter = errors.New("exec 需要 nsenter")

// ErrUnsupported 表示当前平台的 exec 不受支持。
var ErrUnsupported = errors.New("exec 在当前平台不受支持")

// ErrNoCgoExec 保留为别名：调用方历史上用它判定「此构建不支持 exec」。
var ErrNoCgoExec = ErrUnsupported

// Enabled 报告当前构建是否支持进入命名空间执行。非 Linux 恒为 false。
func Enabled() bool { return false }

// Enter 在非 Linux 平台上不可用。
func Enter(targetPID int, workdir, user string, env []string, inFd, outFd, errFd int, cmd []string) (int, error) {
	return -1, fmt.Errorf("execns: %w（exec 依赖 Linux 命名空间，macOS 需在 VM 内运行）", ErrUnsupported)
}

// Wait 在非 Linux 平台上无意义。
func Wait(pid int) int { return -1 }
