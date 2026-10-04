// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build (linux && arm64) || (linux && riscv64)

package runtime

// 64 位 RISC 平台（arm64 / riscv64）的 seccomp 拦截项补充。
//
// 这些平台走 asm-generic 系统调用表，**没有** iopl/ioperm/sysfs（x86 专属）
// 与 uselib/ustat（32 位遗留接口），因此没有额外的架构专属项——
// 公共黑名单已覆盖全部主要危险调用。

// archSpecificRules 返回本平台额外的拦截项；arm64/riscv64 为空。
func archSpecificRules() []seccompRule { return nil }
