// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

// 本文件是能力裁剪的**系统调用**部分（prctl 清边界集 + capset 收紧集合）。
// 纯逻辑（编号表、名称解析、集合运算）在 capability.go，那份不带 build tag。

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// prCapbsetDrop 是 prctl(PR_CAPBSET_DROP)：把某个能力从边界集里移除。
const prCapbsetDrop = 24

// capLastCap 读内核支持的最大能力编号。
//
// 用 /proc/sys/kernel/cap_last_cap 而不是硬编码：不同内核这个值不同
// （较新内核为 40），硬编码会在更新的内核上漏掉编号更大的能力。
// 读不到时退化为一个足够大的上界——多试的编号会被内核以 EINVAL 拒绝，无害。
func capLastCap() int {
	data, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return capLastCapFallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 {
		return capLastCapFallback
	}
	return n
}

// dropBoundingSet 把不在 keep 里的能力从边界集（bounding set）中移除。
//
// 必须在 capset **之前**做：capset 会把有效集降到 keep，之后就不再持有
// CAP_SETPCAP，而 PR_CAPBSET_DROP 要求有效集里有 CAP_SETPCAP。顺序反了
// 会导致剩余能力永远留在边界集里（子进程可通过 exec 重新获得）。
//
// 边界集只限制"将来能否再获得"该能力，不影响当前有效集，因此循环内部
// 即使先把 CAP_SETPCAP 从边界集移除，也不影响后续的 drop 调用。
func dropBoundingSet(keep map[int]bool) error {
	last := capLastCap()
	for c := 0; c <= last; c++ {
		if keep[c] {
			continue
		}
		_, _, errno := syscall.Syscall(syscall.SYS_PRCTL, uintptr(prCapbsetDrop), uintptr(c), 0)
		switch errno {
		case 0:
		case syscall.EINVAL:
			// 该编号此内核未定义（读不到 cap_last_cap 时会遇到），跳过。
		default:
			return fmt.Errorf("prctl(PR_CAPBSET_DROP, %d): %w", c, errno)
		}
	}
	return nil
}

// capability ABI 结构（linux/capability.h）。
//
//	_LINUX_CAPABILITY_VERSION_3 = 0x20080522，配套 2 个 capData（覆盖 64 位）。
//
// 字段顺序与宽度必须与内核结构逐字节一致；测试用 capget 交叉校验
// （capget 读回的值与 /proc/self/status 的 CapEff 比对）来保证这一点。
type capHeader struct {
	Version uint32
	PID     int32
}

type capData struct {
	Effective   uint32
	Permitted   uint32
	Inheritable uint32
}

var capVersion3 = uint32(0x20080522)

// capset 设置当前进程的有效/允许/继承能力集。
//
// 只能收紧不能放宽：内核要求新的 permitted ⊆ 旧的 permitted，否则 EPERM。
func capset(caps []int) error {
	hdr := capHeader{Version: capVersion3, PID: 0} // PID 0 = 当前进程
	var data [2]capData
	for _, c := range caps {
		idx := c / 32
		if idx < 0 || idx >= len(data) {
			return fmt.Errorf("capability 编号 %d 超出 ABI v3 可表达范围", c)
		}
		bit := uint32(1) << (uint(c) % 32)
		data[idx].Effective |= bit
		data[idx].Permitted |= bit
		data[idx].Inheritable |= bit
	}
	_, _, errno := syscall.Syscall(syscall.SYS_CAPSET,
		uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0)
	if errno != 0 {
		return fmt.Errorf("capset: %w", errno)
	}
	return nil
}

// ApplyCapabilities 按 drop/add 规格裁剪当前进程的能力，返回最终保留的能力名。
//
// 顺序不可交换（安全属性）：
//  1. 先从边界集移除不要的能力——此时还持有 CAP_SETPCAP；
//  2. 再 capset 收紧有效/允许/继承集。
func ApplyCapabilities(drop, add []string) ([]string, error) {
	keepList, err := resolveCapabilities(drop, add)
	if err != nil {
		return nil, err
	}
	keep := make(map[int]bool, len(keepList))
	for _, c := range keepList {
		keep[c] = true
	}
	if err := dropBoundingSet(keep); err != nil {
		return nil, err
	}
	if err := capset(keepList); err != nil {
		return nil, err
	}
	return CapabilityNames(keepList), nil
}

// applyCapabilitiesFromEnv 是容器 init 的入口：从自身环境变量读取裁剪规格并应用。
func applyCapabilitiesFromEnv() ([]string, error) {
	drop, add := capsFromEnv(os.Environ())
	return ApplyCapabilities(drop, add)
}
