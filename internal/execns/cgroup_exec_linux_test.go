// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package execns

// exec 进程 cgroup 归置的单测（v0.9.2）。
//
// 背景（真机实测发现）：`licore exec` 走宿主侧 nsenter，新进程默认落在
// **调用者（CLI）自己的 cgroup**——实测为 user.slice/user-0.slice/session-7.scope，
// 而不是容器的 /licore/<id>。后果是 exec 完全绕过 --memory / --pids-limit：
// 实测容器限额 256 MiB，exec 进去的进程吃到 400 MiB 也不被拦，宿主 OOM
// 风险因此回归。
//
// 修复采用两段式：
//  1. **首选** clone3(CLONE_INTO_CGROUP)：SysProcAttr.CgroupFD + UseCgroupFD，
//     新进程出生即在目标 cgroup，无竞态；
//  2. **回退** 启动后写 cgroup.procs：clone3 不可用时使用，有竞态（见下）。
//
// 本文件锁死两者共有的关键不变量。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestOpenCgroupForCloneEmptyMeansNoOp 断言 cgroupDir 为空时不打开任何 fd。
//
// 空串表示调用方不需要归置（保持旧行为）。此时必须返回 (nil, nil)，
// 让 Enter 跳过 SysProcAttr 设置——而不是去打开一个空路径（会失败）。
func TestOpenCgroupForCloneEmptyMeansNoOp(t *testing.T) {
	f, err := openCgroupForClone("")
	if err != nil {
		t.Fatalf("空路径不应报错: %v", err)
	}
	if f != nil {
		t.Fatalf("空路径应返回 nil fd，实际 %v", f)
	}
}

// TestOpenCgroupForCloneOpensExistingDir 断言真实目录被成功打开。
//
// CLONE_INTO_CGROUP 要的是指向 **cgroup v2 目录** 的 fd，因此这里必须打开
// 目录本身（O_RDONLY），而不是目录下的某个文件。
func TestOpenCgroupForCloneOpensExistingDir(t *testing.T) {
	dir := t.TempDir()
	f, err := openCgroupForClone(dir)
	if err != nil {
		t.Fatalf("打开已存在目录应成功: %v", err)
	}
	if f == nil {
		t.Fatal("应返回有效 fd")
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !fi.IsDir() {
		t.Error("打开的必须是目录本身（cgroup v2 目录）")
	}
}

// TestOpenCgroupForCloneFailsOnMissingDir 断言目录不存在时**报错而不是静默跳过**。
//
// 这条很重要：静默跳过会让 exec 在没有任何告警的情况下失去资源限额，
// 正是本修复要消除的状态。宁可启动失败。
func TestOpenCgroupForCloneFailsOnMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	f, err := openCgroupForClone(missing)
	if err == nil {
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("目录不存在时必须报错（否则 exec 会静默失去限额）")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("错误应点名缺失路径便于排查，实际: %v", err)
	}
}

// TestIsClone3UnsupportedClassifiesFallbackCases 断言只有"环境不支持"类
// 错误才触发回退。
//
// 分类错了会有两种坏结果：
//   - 该回退却不回退 → 老内核上 exec 直接不可用；
//   - 不该回退却回退 → 把真实错误（如可执行文件缺失）掩盖成一次无谓重试。
func TestIsClone3UnsupportedClassifiesFallbackCases(t *testing.T) {
	fallback := []error{syscall.ENOSYS, syscall.EINVAL, syscall.EPERM, syscall.EOPNOTSUPP}
	for _, e := range fallback {
		if !isClone3Unsupported(e) {
			t.Errorf("%v 应触发回退（clone3/CLONE_INTO_CGROUP 不可用的典型 errno）", e)
		}
	}
	// 这些**不该**回退：重试只会得到同样的失败。
	noFallback := []error{syscall.ENOENT, syscall.EACCES, syscall.ENOMEM}
	for _, e := range noFallback {
		if isClone3Unsupported(e) {
			t.Errorf("%v 不应触发回退（不是环境不支持 clone3）", e)
		}
	}
	if isClone3Unsupported(nil) {
		t.Error("nil 不应触发回退")
	}
}

// TestMovePidToCgroupWritesPid 断言回退路径真的把 pid 写进 cgroup.procs。
//
// 用临时目录模拟 cgroup 目录。真实 cgroupfs 的 cgroup.procs 不是普通文件，
// 但"打开并以十进制文本写入 pid"这一交互契约是相同的——这正是被测逻辑。
func TestMovePidToCgroupWritesPid(t *testing.T) {
	dir := t.TempDir()
	procs := filepath.Join(dir, "cgroup.procs")
	if err := os.WriteFile(procs, nil, 0o644); err != nil {
		t.Fatalf("准备 cgroup.procs: %v", err)
	}

	if err := movePidToCgroup(4242, dir); err != nil {
		t.Fatalf("写入应成功: %v", err)
	}

	got, err := os.ReadFile(procs)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	// cgroup.procs 的写入格式是**不带换行的十进制 pid**；带换行虽也能被内核
	// 接受，但保持与内核文档一致的规范形式更稳妥。
	if string(got) != strconv.Itoa(4242) {
		t.Errorf("cgroup.procs 内容 = %q，期望 %q", string(got), strconv.Itoa(4242))
	}
}

// TestMovePidToCgroupFailsOnMissingProcs 断言目标不可写时返回错误。
func TestMovePidToCgroupFailsOnMissingProcs(t *testing.T) {
	dir := t.TempDir() // 故意不建 cgroup.procs
	err := movePidToCgroup(1, dir)
	if err == nil {
		t.Fatal("cgroup.procs 不存在时应报错")
	}
	if !strings.Contains(err.Error(), "cgroup.procs") {
		t.Errorf("错误应点名 cgroup.procs 便于排查，实际: %v", err)
	}
}
