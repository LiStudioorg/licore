// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

// /proc/sys 只读挂载（R-7）的单测。
//
// 背景（真机实测，2026-10-05）：容器 init 是真正的宿主 uid 0，而
// /proc/sysrq-trigger / kernel/core_pattern / kernel/modprobe 这三个文件的
// 内核 handler 走 proc_dostring、**不做 capable() 检查**，只依赖 inode 的
// DAC 权限。于是容器内可以写穿宿主——实测 core_pattern 写入后宿主可见。
//
// capability 裁剪对它们无效（CapEff 里 CAP_SYS_ADMIN 确已为 0），seccomp 也
// 无效（这是 open+write，不是专用系统调用）。唯一可靠的封堵点是挂载层。
//
// 本文件锁死挂载层的两个关键细节：
//  1. 必须先 MS_BIND 再 MS_REMOUNT|MS_BIND|MS_RDONLY——
//     单次 mount(MS_BIND|MS_RDONLY) 的 MS_RDONLY 会被内核忽略（v0.6.0 教训）；
//  2. 任一步失败必须返回错误，不允许降级启动（否则容器带着 P0 逃逸路径运行）。

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/LiStudioorg/licore/internal/resource"
)

// mkdirAllProcSys 在测试用 rootfs 下造出 proc/sys 目录。
// 真实启动流程里 /proc/sys 由刚挂上的 procfs 提供，测试里用普通目录代替
// ——本测试只验证挂载调用的参数与顺序，不依赖它真的是 procfs。
func mkdirAllProcSys(rootfs string) error {
	return os.MkdirAll(filepath.Join(rootfs, "proc", "sys"), 0o755)
}

// TestMountProcSysReadOnlyUsesBindThenRemount 断言调用序列正确。
//
// 这是本修复的核心不变式：**两步，且顺序固定**。写成一步（MS_BIND|MS_RDONLY）
// 是 v0.6.0 踩过的坑——挂载会成功，但仍是可写，而且没有任何报错。
func TestMountProcSysReadOnlyUsesBindThenRemount(t *testing.T) {
	rootfs := t.TempDir()
	if err := mkdirAllProcSys(rootfs); err != nil {
		t.Fatalf("准备 /proc/sys 目录失败: %v", err)
	}

	fm := &fakeMount{}
	fm.install(t)

	if err := mountProcSysReadOnly(rootfs); err != nil {
		t.Fatalf("应成功，却报错: %v", err)
	}

	if len(fm.calls) != 2 {
		t.Fatalf("应恰好 2 次 mount 调用（bind + remount），实际 %d 次: %+v",
			len(fm.calls), fm.calls)
	}

	// 第 1 步：纯 bind，不带 MS_RDONLY。
	first := fm.calls[0]
	if first.flags&msBind == 0 {
		t.Errorf("第 1 次调用应带 MS_BIND，实际 flags=%#x", first.flags)
	}
	if first.flags&syscall.MS_RDONLY != 0 {
		t.Errorf("第 1 次调用**不应**带 MS_RDONLY（内核会忽略它，造成'以为只读其实可写'）")
	}

	// 第 2 步：remount，必须同时带 MS_BIND 和 MS_RDONLY。
	second := fm.calls[1]
	if second.flags&msBind == 0 {
		t.Errorf("第 2 次（remount）应带 MS_BIND，实际 flags=%#x", second.flags)
	}
	if second.flags&syscall.MS_REMOUNT == 0 {
		t.Errorf("第 2 次应带 MS_REMOUNT，实际 flags=%#x", second.flags)
	}
	if second.flags&syscall.MS_RDONLY == 0 {
		t.Errorf("第 2 次必须带 MS_RDONLY —— 否则整个修复失效，实际 flags=%#x", second.flags)
	}

	// 目标必须是 rootfs 内的 /proc/sys，且 source == target（bind 自身）。
	if !strings.HasSuffix(first.target, "/proc/sys") {
		t.Errorf("挂载目标应是 <rootfs>/proc/sys，实际 %q", first.target)
	}
	if first.source != first.target {
		t.Errorf("bind 自身要求 source == target，实际 source=%q target=%q",
			first.source, first.target)
	}
}

// TestMountProcSysReadOnlyFailsLoudly 断言失败时返回错误而不是静默降级。
//
// 这条很重要：封堵失败 = 容器带着能写 /proc/sysrq-trigger 的 P0 路径启动。
// 静默跳过会让用户以为隔离生效了，比启动失败危险得多。
func TestMountProcSysReadOnlyFailsLoudly(t *testing.T) {
	cases := []struct {
		name string
		fm   *fakeMount
	}{
		{"bind 失败", &fakeMount{failFirst: true, err: syscall.EPERM}},
		{"remount 失败", &fakeMount{failAll: true, err: syscall.EPERM}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rootfs := t.TempDir()
			if err := mkdirAllProcSys(rootfs); err != nil {
				t.Fatalf("准备目录失败: %v", err)
			}
			tc.fm.install(t)

			err := mountProcSysReadOnly(rootfs)
			if err == nil {
				t.Fatal("挂载失败时必须返回错误（否则容器会在未封堵状态下启动）")
			}
			if !strings.Contains(err.Error(), "proc/sys") {
				t.Errorf("错误信息应点名 /proc/sys 便于排查，实际: %v", err)
			}
		})
	}
}

// TestMountProcSysReadOnlyRejectsMissingDir 断言 procfs 异常时明确报错。
//
// /proc 是 RunInit 里刚挂上的真 procfs，/proc/sys 必然存在。走到"不存在"
// 说明 procfs 挂载本身出了故障——那是真实故障，不能当作"无需封堵"跳过。
func TestMountProcSysReadOnlyRejectsMissingDir(t *testing.T) {
	rootfs := t.TempDir() // 故意不建 proc/sys
	fm := &fakeMount{}
	fm.install(t)

	err := mountProcSysReadOnly(rootfs)
	if err == nil {
		t.Fatal("缺少 /proc/sys 时应报错，而不是静默跳过封堵")
	}
	if len(fm.calls) != 0 {
		t.Errorf("目录不存在时不应发起任何 mount 调用，实际 %d 次", len(fm.calls))
	}
}

// -------- /proc 根下危险文件的 mask（sysrq-trigger） --------

// TestMaskProcRootFilesBindsSysrqTrigger 断言 sysrq-trigger 被 bind+remount ro 覆盖。
//
// 为什么单独一个测试：/proc/sysrq-trigger **不在** /proc/sys 目录内，
// mountProcSysReadOnly 覆盖不到它。真机上正是这个疏漏导致第一版修复后
// sysrq-trigger 仍可写。本测试锁死它有独立的 mask 路径。
func TestMaskProcRootFilesBindsSysrqTrigger(t *testing.T) {
	rootfs := t.TempDir()
	procDir := filepath.Join(rootfs, "proc")
	if err := os.MkdirAll(procDir, 0o755); err != nil {
		t.Fatalf("建 proc 目录: %v", err)
	}
	// 模拟 procfs 提供的 sysrq-trigger（普通文件即可，本测试只验挂载调用）。
	if err := os.WriteFile(filepath.Join(procDir, "sysrq-trigger"), nil, 0o200); err != nil {
		t.Fatalf("建 sysrq-trigger: %v", err)
	}

	fm := &fakeMount{}
	fm.install(t)

	if err := maskProcRootFiles(rootfs); err != nil {
		t.Fatalf("应成功，却报错: %v", err)
	}
	if len(fm.calls) != 2 {
		t.Fatalf("应恰好 2 次 mount（bind + remount ro），实际 %d: %+v", len(fm.calls), fm.calls)
	}
	if !strings.HasSuffix(fm.calls[0].target, "/proc/sysrq-trigger") {
		t.Errorf("目标应为 <rootfs>/proc/sysrq-trigger，实际 %q", fm.calls[0].target)
	}
	if fm.calls[0].flags&msBind == 0 {
		t.Errorf("第 1 次应带 MS_BIND，实际 %#x", fm.calls[0].flags)
	}
	if fm.calls[1].flags&syscall.MS_RDONLY == 0 {
		t.Errorf("第 2 次必须带 MS_RDONLY（否则 sysrq-trigger 仍可写会重启宿主），实际 %#x",
			fm.calls[1].flags)
	}
}

// TestMaskProcRootFilesSkipsMissingFile 断言内核未提供该文件时跳过而非报错。
//
// 未启用 CONFIG_MAGIC_SYSRQ 的内核没有 sysrq-trigger，那是正常情况，
// 不该让容器启动失败。
func TestMaskProcRootFilesSkipsMissingFile(t *testing.T) {
	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "proc"), 0o755); err != nil {
		t.Fatalf("建 proc 目录: %v", err)
	}
	fm := &fakeMount{}
	fm.install(t)

	if err := maskProcRootFiles(rootfs); err != nil {
		t.Fatalf("文件不存在时应跳过，不该报错: %v", err)
	}
	if len(fm.calls) != 0 {
		t.Errorf("文件不存在时不应发起 mount 调用，实际 %d 次", len(fm.calls))
	}
}

// TestMaskProcRootFilesFailsLoudly 断言 mask 失败时返回错误。
func TestMaskProcRootFilesFailsLoudly(t *testing.T) {
	rootfs := t.TempDir()
	procDir := filepath.Join(rootfs, "proc")
	if err := os.MkdirAll(procDir, 0o755); err != nil {
		t.Fatalf("建 proc 目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "sysrq-trigger"), nil, 0o200); err != nil {
		t.Fatalf("建 sysrq-trigger: %v", err)
	}
	fm := &fakeMount{failFirst: true, err: syscall.EPERM}
	fm.install(t)

	if err := maskProcRootFiles(rootfs); err == nil {
		t.Fatal("mask 失败时必须报错（否则容器带着可写的 sysrq-trigger 启动）")
	}
}

// -------- exec 进程的 cgroup 路径构造（v0.9.2） --------

// TestContainerCgroupDirMatchesResourceLayout 断言路径与 internal/resource 一致。
//
// 这是**跨模块契约**：internal/resource 是 cgroup 的写方（创建 /licore/<id>
// 并写 memory.max），execns 是使用者。两边路径一旦不一致，exec 会要么写进
// 一个不存在的目录（报错），要么悄悄归置到别处（失去限额）。
func TestContainerCgroupDirMatchesResourceLayout(t *testing.T) {
	got := containerCgroupDir("abc123")
	want := resource.CgroupV2Mount + "/" + resource.LiCoreGroup + "/abc123"
	if got != want {
		t.Errorf("cgroup 路径 = %q，期望 %q", got, want)
	}
	// 钉死字面量，防止常量本身被误改后测试跟着一起"通过"。
	if want != "/sys/fs/cgroup/licore/abc123" {
		t.Errorf("路径布局应为 /sys/fs/cgroup/licore/<id>，实际契约值 %q", want)
	}
}

// TestContainerCgroupDirEmptyIDIsEmpty 断言空 ID 返回空串。
//
// 空串是"跳过归置"的信号，必须与"路径拼出来但目录不存在"区分开——
// 后者会报错（这是刻意的），前者静默保持旧行为。
func TestContainerCgroupDirEmptyIDIsEmpty(t *testing.T) {
	if got := containerCgroupDir(""); got != "" {
		t.Errorf("空容器 ID 应返回空串，实际 %q", got)
	}
}
