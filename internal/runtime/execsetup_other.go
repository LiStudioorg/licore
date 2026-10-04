// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package runtime

import "fmt"

// RunExecSetup 在非 Linux 平台不可用。
//
// 提供同名 stub 是为了让 `internal/cli` 能在所有平台编译——
// 隐藏子命令 `exec-setup` 的注册代码不带 build tag（与 init 同一情况）。
// 运行期在 macOS 上 `licore exec` 本来就返回 ErrUnsupported（需在 VM 内运行），
// 所以这个 stub 不会被正常路径触达。
func RunExecSetup(_ []string) error {
	return fmt.Errorf("exec-setup: 仅 Linux 支持: %w", ErrUnsupported)
}

// InstallExecHelper 在非 Linux 平台是空操作：容器后端不存在，无需注入 helper。
func InstallExecHelper(_ string) error { return nil }
