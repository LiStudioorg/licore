// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"errors"
	"strings"
	"testing"
)

// TestAddPIDMissingCgroup 验证组不存在时 AddPID 明确报错（正常路径需 root +
// cgroups v2，仅在此断言错误分支的契约）。
func TestAddPIDMissingCgroup(t *testing.T) {
	if !Available() {
		t.Skip("cgroups v2 不可用，跳过")
	}
	err := AddPID("does-not-exist-container", 1)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("期望 ErrUnsupported 包装，实得 %v", err)
	}
}

func TestNewCgroupPath(t *testing.T) {
	c := NewCgroup("cid123")
	if c.ContainerID != "cid123" {
		t.Fatalf("ContainerID 错误: %s", c.ContainerID)
	}
	if c.Path == "" || c.Root == "" {
		t.Fatalf("空路径: %+v", c)
	}
}

// 本组测试守住一条易漏的不变量：**cgroup v2 的 subtree_control 必须包含
// 所有会被写入的控制器**。
//
// 事故背景：`controllers` 常量曾漏掉 cpuset，于是 `--cpuset-cpus` 完全没效果
// ——子组的 cpuset.cpus 不可写，写进去被忽略，而父组仍显示全部 CPU。
// 与 --memory/--cpus 早期"未 enable 控制器导致静默落空"是同一个坑。

// TestControllersCoverAllWritableFiles 把 controllers 常量与 c.write 里
// 实际出现的文件名做交叉核对，避免将来新增限制项时又忘了加控制器。
func TestControllersCoverAllWritableFiles(t *testing.T) {
	enabled := map[string]bool{}
	for _, c := range strings.Fields(controllers) {
		enabled[c] = true
	}
	// cgroup v2 里 "<controller>.<file>" 的控制器前缀。
	// 只列出 cgroup_linux.go 的 writeV2 会写的限制类文件。
	required := []string{"cpu", "memory", "pids", "cpuset"}
	for _, r := range required {
		if !enabled[r] {
			t.Errorf("controllers 缺少 %q：写入 %s.* 会被 cgroup v2 拒绝，对应参数静默失效", r, r)
		}
	}
	// 反向：cpuset 必须真的在里面（这条是本次缺陷的直接回归护栏）。
	if !enabled["cpuset"] {
		t.Error("controllers 必须包含 cpuset，否则 --cpuset-cpus 静默失效")
	}
}

// controllers 的字符串形态必须能被 strings.Fields 正常切分（无多余分隔符）。
func TestControllersWellFormed(t *testing.T) {
	fs := strings.Fields(controllers)
	if len(fs) == 0 {
		t.Fatal("controllers 不能为空")
	}
	for _, f := range fs {
		if strings.TrimSpace(f) != f || f == "" {
			t.Errorf("controllers 含空白/空项: %q", f)
		}
	}
	seen := map[string]bool{}
	for _, f := range fs {
		if seen[f] {
			t.Errorf("controllers 重复项: %q", f)
		}
		seen[f] = true
	}
}
