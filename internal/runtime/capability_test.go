// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"errors"
	"sort"
	"strings"
	"testing"
)

// capSetOf 把 resolveCapabilities 的结果转成便于断言的名称集合。
func capSetOf(t *testing.T, drop, add []string) map[string]bool {
	t.Helper()
	nums, err := resolveCapabilities(drop, add)
	if err != nil {
		t.Fatalf("resolveCapabilities(drop=%v, add=%v): %v", drop, add, err)
	}
	out := make(map[string]bool, len(nums))
	for _, n := range CapabilityNames(nums) {
		out[n] = true
	}
	return out
}

// names 返回集合里排序后的名称列表（用于稳定断言）。
func names(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// TestDefaultCapsIsDockerSet 锁定默认能力集就是 Docker 默认集。
//
// 这是**安全属性**的回归测试：默认集一旦被无意放宽（比如有人加了
// CAP_SYS_ADMIN "方便调试"），这里必须失败。
func TestDefaultCapsIsDockerSet(t *testing.T) {
	got := capSetOf(t, nil, nil)
	want := []string{
		"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID",
		"KILL", "MKNOD", "NET_BIND_SERVICE", "NET_RAW",
		"SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT",
	}
	gotList := names(got)
	if strings.Join(gotList, ",") != strings.Join(want, ",") {
		t.Errorf("默认能力集不符：\n got = %v\nwant = %v", gotList, want)
	}
}

// TestDefaultCapsExcludesDangerousCaps 断言默认集**不含**危险能力。
//
// 这是本次安全修复的核心断言：这些能力正是攻击路径所依赖的。
func TestDefaultCapsExcludesDangerousCaps(t *testing.T) {
	got := capSetOf(t, nil, nil)
	// 每一条都对应一个具体的攻击路径，注释写清"为什么必须没有它"。
	for _, n := range []string{
		"SYS_ADMIN",       // 写 /proc/sysrq-trigger 重启宿主、mount、加载 eBPF 的总开关
		"SYS_MODULE",      // 加载内核模块
		"SYS_RAWIO",       // 直接访问 /dev/mem、iopl
		"SYS_BOOT",        // reboot(2)
		"SYS_TIME",        // settimeofday / clock_settime
		"SYS_PTRACE",      // ptrace 任意进程
		"SYS_NICE",        // 改调度优先级
		"SYS_RESOURCE",    // 突破 RLIMIT
		"NET_ADMIN",       // 改宿主网络（若共享 netns）
		"DAC_READ_SEARCH", // 绕过目录读权限
		"SYSLOG",          // 读内核环形缓冲
		"BPF",             // 加载 eBPF
		"PERFMON",         // 性能监控，可侧信道
		"AUDIT_CONTROL",   // 关审计
		"MAC_ADMIN",       // 改 SELinux/AppArmor 策略
		"MAC_OVERRIDE",
		"LINUX_IMMUTABLE",
		"LEASE",
		"WAKE_ALARM",
		"BLOCK_SUSPEND",
		"CHECKPOINT_RESTORE",
		"IPC_OWNER",
		"NET_BROADCAST",
		"AUDIT_READ",
		"DAC_READ_SEARCH",
	} {
		if got[n] {
			t.Errorf("默认能力集**不应**包含 %s（对应真实攻击路径）", n)
		}
	}
}

// TestCapDropRemovesFromDefault 验证 --cap-drop 从默认集里减。
func TestCapDropRemovesFromDefault(t *testing.T) {
	got := capSetOf(t, []string{"CHOWN", "MKNOD"}, nil)
	if got["CHOWN"] || got["MKNOD"] {
		t.Errorf("被 drop 的能力仍存在: %v", names(got))
	}
	// 其余默认项必须保留（drop 是减法，不是替换）。
	if !got["KILL"] || !got["NET_RAW"] {
		t.Errorf("未 drop 的默认能力不应丢失: %v", names(got))
	}
}

// TestCapAddAddsToDefault 验证 --cap-add 在默认集之上加。
func TestCapAddAddsToDefault(t *testing.T) {
	got := capSetOf(t, nil, []string{"SYS_PTRACE"})
	if !got["SYS_PTRACE"] {
		t.Errorf("--cap-add 未生效: %v", names(got))
	}
	// 默认项仍在。
	if !got["CHOWN"] || !got["NET_RAW"] {
		t.Errorf("--cap-add 不应清掉默认集: %v", names(got))
	}
}

// TestCapDropAllLeavesOnlyExplicitAdds 验证 --cap-drop ALL 清空后只留显式 add。
func TestCapDropAllLeavesOnlyExplicitAdds(t *testing.T) {
	got := capSetOf(t, []string{"ALL"}, []string{"NET_ADMIN", "SYS_PTRACE"})
	if strings.Join(names(got), ",") != "NET_ADMIN,SYS_PTRACE" {
		t.Errorf("drop ALL + 显式 add 的结果 = %v，期望只有 NET_ADMIN,SYS_PTRACE", names(got))
	}
}

// TestCapDropAllAloneYieldsEmptySet 验证 --cap-drop ALL 且无 add 时得到空集。
func TestCapDropAllAloneYieldsEmptySet(t *testing.T) {
	got := capSetOf(t, []string{"ALL"}, nil)
	if len(got) != 0 {
		t.Errorf("drop ALL 且无 add 应为空集，得到 %v", names(got))
	}
}

// TestCapAddAllAddsEverything 验证 --cap-add ALL 补齐全部已知能力。
func TestCapAddAllAddsEverything(t *testing.T) {
	got := capSetOf(t, nil, []string{"ALL"})
	if len(got) != len(capNames) {
		t.Errorf("--cap-add ALL 应得到全部 %d 项，得到 %d 项", len(capNames), len(got))
	}
}

// TestCapNamesCaseInsensitiveAndPrefixOptional 验证写法放宽。
func TestCapNamesCaseInsensitiveAndPrefixOptional(t *testing.T) {
	for _, form := range []string{
		"CAP_NET_ADMIN", "cap_net_admin", "NET_ADMIN", "net_admin", " Net_Admin ",
	} {
		n, err := ParseCapability(form)
		if err != nil {
			t.Errorf("ParseCapability(%q) 应被接受: %v", form, err)
			continue
		}
		if n != capNetAdmin {
			t.Errorf("ParseCapability(%q) = %d, want %d", form, n, capNetAdmin)
		}
	}
}

// TestParseCapabilityUnknown 验证非法能力名报错，且错误里带可选值提示。
func TestParseCapabilityUnknown(t *testing.T) {
	for _, bad := range []string{"", "   ", "CAP_NOT_A_REAL_CAP", "SYS_ADM", "1234"} {
		_, err := ParseCapability(bad)
		if err == nil {
			t.Errorf("ParseCapability(%q) 应报错", bad)
			continue
		}
		if !errors.Is(err, ErrUnknownCapability) {
			t.Errorf("ParseCapability(%q) 应为 ErrUnknownCapability，得到 %v", bad, err)
		}
	}
	// 错误信息应给出可选值，否则用户不知道能写什么。
	_, err := ParseCapability("NOPE")
	if err == nil || !strings.Contains(err.Error(), "NET_ADMIN") {
		t.Errorf("错误应列出可选能力名: %v", err)
	}
}

// TestResolveCapabilitiesRejectsUnknown 验证非法名字在集合运算阶段也被拒绝。
func TestResolveCapabilitiesRejectsUnknown(t *testing.T) {
	if _, err := resolveCapabilities([]string{"BOGUS"}, nil); !errors.Is(err, ErrUnknownCapability) {
		t.Errorf("drop 里的非法名应报错，得到 %v", err)
	}
	if _, err := resolveCapabilities(nil, []string{"BOGUS"}); !errors.Is(err, ErrUnknownCapability) {
		t.Errorf("add 里的非法名应报错，得到 %v", err)
	}
}

// TestIsAllCap 验证 ALL 关键字的识别（大小写、前缀均可）。
func TestIsAllCap(t *testing.T) {
	for _, s := range []string{"ALL", "all", "CAP_ALL", "cap_all", " All "} {
		if !IsAllCap(s) {
			t.Errorf("IsAllCap(%q) 应为 true", s)
		}
	}
	for _, s := range []string{"", "SYS_ADMIN", "ALLL"} {
		if IsAllCap(s) {
			t.Errorf("IsAllCap(%q) 应为 false", s)
		}
	}
}

// TestCapsEnvRoundTrip 验证环境变量序列化/反序列化往返。
func TestCapsEnvRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		drop []string
		add  []string
	}{
		{"都为空", nil, nil},
		{"只有 drop", []string{"SYS_ADMIN", "NET_ADMIN"}, nil},
		{"只有 add", nil, []string{"SYS_PTRACE"}},
		{"都有", []string{"ALL"}, []string{"NET_ADMIN"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := CapsEnv(tc.drop, tc.add)
			gotDrop, gotAdd := capsFromEnv(env)
			if strings.Join(gotDrop, ",") != strings.Join(tc.drop, ",") {
				t.Errorf("drop 往返不一致: %v → %v", tc.drop, gotDrop)
			}
			if strings.Join(gotAdd, ",") != strings.Join(tc.add, ",") {
				t.Errorf("add 往返不一致: %v → %v", tc.add, gotAdd)
			}
		})
	}
}

// TestCapsEnvEmptyYieldsNoVars 验证空规格不产生环境变量（空值即"用默认集"语义）。
func TestCapsEnvEmptyYieldsNoVars(t *testing.T) {
	if env := CapsEnv(nil, nil); len(env) != 0 {
		t.Errorf("空规格不应产生环境变量，得到 %v", env)
	}
	// 反序列化端：无变量时应得到空规格（→ 默认集），而不是报错。
	drop, add := capsFromEnv(nil)
	if len(drop) != 0 || len(add) != 0 {
		t.Errorf("无变量时应为空规格，得到 drop=%v add=%v", drop, add)
	}
}

// TestCapsFromEnvHandlesSpacesAndEmptyItems 验证 "A, B,,C" 这类输入的健壮性。
func TestCapsFromEnvHandlesSpacesAndEmptyItems(t *testing.T) {
	drop, add := capsFromEnv([]string{
		envCapsDrop + "=SYS_ADMIN, NET_ADMIN ,,",
		envCapsAdd + "= SYS_PTRACE ",
	})
	if strings.Join(drop, ",") != "SYS_ADMIN,NET_ADMIN" {
		t.Errorf("drop = %v，期望 [SYS_ADMIN NET_ADMIN]", drop)
	}
	if strings.Join(add, ",") != "SYS_PTRACE" {
		t.Errorf("add = %v，期望 [SYS_PTRACE]", add)
	}
}
