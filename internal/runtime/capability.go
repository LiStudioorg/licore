// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package runtime

// 本文件是能力裁剪的**纯逻辑**部分（编号表、名称解析、集合运算、环境变量
// 编解码），刻意不加 build tag：CLI 需要在校验 --cap-add/--cap-drop 时
// 复用 ParseCapability，若本文件只在 linux 下编译，darwin/android 交叉
// 编译会失败（AGENTS.md 要求三平台交叉编译通过）。
//
// 真正的系统调用（prctl / capset）在 capability_linux.go，只在 linux 编译。

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ErrUnknownCapability 表示能力名无法识别（拼写错误或内核不认识）。
var ErrUnknownCapability = errors.New("licore/runtime: 未知的 capability")

// allCapName 是 --cap-drop/--cap-add 的全集关键字。
const allCapName = "ALL"

// Linux capability 编号（linux/capability.h）。标准库 syscall 未导出这些常量。
const (
	capChown           = 0
	capDACOverride     = 1
	capDACReadSearch   = 2
	capFowner          = 3
	capFsetid          = 4
	capKill            = 5
	capSetgid          = 6
	capSetuid          = 7
	capSetpcap         = 8
	capLinuxImmutable  = 9
	capNetBindService  = 10
	capNetBroadcast    = 11
	capNetAdmin        = 12
	capNetRaw          = 13
	capIPCLock         = 14
	capIPCOwner        = 15
	capSysModule       = 16
	capSysRawio        = 17
	capSysChroot       = 18
	capSysPtrace       = 19
	capSysPacct        = 20
	capSysAdmin        = 21
	capSysBoot         = 22
	capSysNice         = 23
	capSysResource     = 24
	capSysTime         = 25
	capSysTTYConfig    = 26
	capMknod           = 27
	capLease           = 28
	capAuditWrite      = 29
	capAuditControl    = 30
	capSetfcap         = 31
	capMACOverride     = 32
	capMACAdmin        = 33
	capSyslog          = 34
	capWakeAlarm       = 35
	capBlockSuspend    = 36
	capAuditRead       = 37
	capPerfmon         = 38
	capBPF             = 39
	capCheckpointRstrt = 40

	// capLastCapFallback 是读不到 /proc/sys/kernel/cap_last_cap 时的上界。
	capLastCapFallback = 63

	capUnknownCapNumber = -1
)

// capNames 是编号 → 规范名（不含 CAP_ 前缀）的权威表。
var capNames = map[int]string{
	capChown:           "CHOWN",
	capDACOverride:     "DAC_OVERRIDE",
	capDACReadSearch:   "DAC_READ_SEARCH",
	capFowner:          "FOWNER",
	capFsetid:          "FSETID",
	capKill:            "KILL",
	capSetgid:          "SETGID",
	capSetuid:          "SETUID",
	capSetpcap:         "SETPCAP",
	capLinuxImmutable:  "LINUX_IMMUTABLE",
	capNetBindService:  "NET_BIND_SERVICE",
	capNetBroadcast:    "NET_BROADCAST",
	capNetAdmin:        "NET_ADMIN",
	capNetRaw:          "NET_RAW",
	capIPCLock:         "IPC_LOCK",
	capIPCOwner:        "IPC_OWNER",
	capSysModule:       "SYS_MODULE",
	capSysRawio:        "SYS_RAWIO",
	capSysChroot:       "SYS_CHROOT",
	capSysPtrace:       "SYS_PTRACE",
	capSysPacct:        "SYS_PACCT",
	capSysAdmin:        "SYS_ADMIN",
	capSysBoot:         "SYS_BOOT",
	capSysNice:         "SYS_NICE",
	capSysResource:     "SYS_RESOURCE",
	capSysTime:         "SYS_TIME",
	capSysTTYConfig:    "SYS_TTY_CONFIG",
	capMknod:           "MKNOD",
	capLease:           "LEASE",
	capAuditWrite:      "AUDIT_WRITE",
	capAuditControl:    "AUDIT_CONTROL",
	capSetfcap:         "SETFCAP",
	capMACOverride:     "MAC_OVERRIDE",
	capMACAdmin:        "MAC_ADMIN",
	capSyslog:          "SYSLOG",
	capWakeAlarm:       "WAKE_ALARM",
	capBlockSuspend:    "BLOCK_SUSPEND",
	capAuditRead:       "AUDIT_READ",
	capPerfmon:         "PERFMON",
	capBPF:             "BPF",
	capCheckpointRstrt: "CHECKPOINT_RESTORE",
}

// dockerDefaultCaps 是 Docker 的默认能力集：容器默认只拿到这些，其余全丢。
//
// 刻意不含 CAP_SYS_ADMIN（写 /proc/sysrq-trigger、加载 eBPF、mount 的总开关）、
// 不含 CAP_NET_ADMIN、CAP_SYS_MODULE、CAP_SYS_TIME、CAP_SYS_BOOT、CAP_SYS_PTRACE。
var dockerDefaultCaps = []int{
	capChown,
	capDACOverride,
	capFowner,
	capFsetid,
	capKill,
	capSetgid,
	capSetuid,
	capSetpcap,
	capNetBindService,
	capNetRaw,
	capSysChroot,
	capMknod,
	capAuditWrite,
	capSetfcap,
}

// normalizeCapName 归一化能力名：去空白、转大写、去掉 CAP_ 前缀。
func normalizeCapName(name string) string {
	return strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(name)), "CAP_")
}

// IsAllCap 判断一个 spec 项是否是 ALL 关键字（大小写不敏感，可带 CAP_ 前缀）。
func IsAllCap(s string) bool {
	return normalizeCapName(s) == allCapName
}

// ParseCapability 把用户写的名字解析为能力编号。
//
// 放宽接受这几种写法（大小写不敏感，CAP_ 前缀可带可不带）：
//
//	CAP_NET_ADMIN / cap_net_admin / NET_ADMIN / net_admin
//
// 特例：ALL 不在此处理——它的语义依赖上下文（drop ALL 与 add ALL 含义不同），
// 由调用方先用 IsAllCap 判断。
func ParseCapability(name string) (int, error) {
	norm := normalizeCapName(name)
	if norm == "" {
		return capUnknownCapNumber, fmt.Errorf("%w：能力名为空", ErrUnknownCapability)
	}
	if norm == allCapName {
		return capUnknownCapNumber, fmt.Errorf("%w：ALL 是集合关键字，请先用 IsAllCap 判断", ErrUnknownCapability)
	}
	for num, n := range capNames {
		if n == norm {
			return num, nil
		}
	}
	return capUnknownCapNumber, fmt.Errorf("%w：%q（可选：%s，或 ALL）",
		ErrUnknownCapability, name, strings.Join(SupportedCapabilities(), " / "))
}

// SupportedCapabilities 返回全部已知能力名（已排序），用于错误提示。
func SupportedCapabilities() []string {
	out := make([]string, 0, len(capNames))
	for _, n := range capNames {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// resolveCapabilities 计算容器最终应持有的能力集。
//
// 语义（与 Docker 对齐）：
//   - 基准是 Docker 默认集；
//   - drop 含 ALL 时基准清空，否则从基准里移除 drop 列出的项；
//   - add 在最后追加（显式要求优先于 drop）；
//   - add 含 ALL 时追加全部已知能力。
//
// 因此常见用法 `--cap-drop ALL --cap-add NET_ADMIN` 得到"只有 NET_ADMIN"。
func resolveCapabilities(drop, add []string) ([]int, error) {
	keep := make(map[int]bool, len(dockerDefaultCaps))
	dropAll := false
	for _, d := range drop {
		if IsAllCap(d) {
			dropAll = true
		}
	}
	if !dropAll {
		for _, c := range dockerDefaultCaps {
			keep[c] = true
		}
	}
	for _, d := range drop {
		if IsAllCap(d) {
			continue
		}
		c, err := ParseCapability(d)
		if err != nil {
			return nil, err
		}
		delete(keep, c)
	}
	for _, a := range add {
		if IsAllCap(a) {
			for c := range capNames {
				keep[c] = true
			}
			continue
		}
		c, err := ParseCapability(a)
		if err != nil {
			return nil, err
		}
		keep[c] = true
	}

	out := make([]int, 0, len(keep))
	for c := range keep {
		out = append(out, c)
	}
	sort.Ints(out)
	return out, nil
}

// CapabilityNames 把编号列表转回规范名（用于日志与测试断言）。
func CapabilityNames(caps []int) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if n, ok := capNames[c]; ok {
			out = append(out, n)
			continue
		}
		out = append(out, strconv.Itoa(c))
	}
	return out
}

// 环境变量名：与 LICORE_NET_* / LICORE_MOUNT_* / LICORE_CGROUP_ID 同一约定，
// 由父进程（engine）注入，init 读取后在 execve 前应用；envWithoutLiCore
// 会把它们从容器环境里剥掉，不会泄漏给用户命令。
const (
	envCapsDrop = "LICORE_CAPS_DROP"
	envCapsAdd  = "LICORE_CAPS_ADD"
)

// CapsEnv 生成能力裁剪的环境变量。
//
// 两者都为空时返回 nil——此时 init 走 Docker 默认集（空值即默认语义）。
func CapsEnv(drop, add []string) []string {
	var out []string
	if len(drop) > 0 {
		out = append(out, envCapsDrop+"="+strings.Join(drop, ","))
	}
	if len(add) > 0 {
		out = append(out, envCapsAdd+"="+strings.Join(add, ","))
	}
	return out
}

// splitCapList 把 "A, B ,C" 切成 ["A","B","C"]，丢弃空项。
func splitCapList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// capsFromEnv 从环境变量切片读取能力裁剪配置。
func capsFromEnv(env []string) (drop, add []string) {
	get := func(key string) string {
		prefix := key + "="
		for _, e := range env {
			if strings.HasPrefix(e, prefix) {
				return strings.TrimPrefix(e, prefix)
			}
		}
		return ""
	}
	return splitCapList(get(envCapsDrop)), splitCapList(get(envCapsAdd))
}
