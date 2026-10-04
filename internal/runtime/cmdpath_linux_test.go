// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkExec 在 dir 下创建一个可执行占位文件。
func mkExec(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestResolveContainerCmdAbsolutePath 验证含 '/' 的路径原样使用、不查 PATH。
//
// 这是最重要的一条：`/bin/busybox sleep` 之所以能工作、而裸 `sleep` 不能，
// 正是因为前者不走 PATH 查找。若这里误改成"总是查找"，反而会破坏绝对路径语义。
func TestResolveContainerCmdAbsolutePath(t *testing.T) {
	for _, arg0 := range []string{
		"/bin/sh",
		"/nonexistent/whatever", // 不存在的绝对路径也要原样返回（交给 execve 报错）
		"./relative",
		"dir/relative",
	} {
		got, err := resolveContainerCmd([]string{arg0, "-c", "x"}, []string{"PATH=/bin"})
		if err != nil {
			t.Errorf("含 '/' 的 %q 不应报错: %v", arg0, err)
			continue
		}
		if got != arg0 {
			t.Errorf("含 '/' 的 %q 应原样返回，得到 %q", arg0, got)
		}
	}
}

// TestResolveContainerCmdFindsInPath 验证裸命令名按 PATH 查找。
func TestResolveContainerCmdFindsInPath(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	want := mkExec(t, d2, "sleep") // 只在第二个目录里

	got, err := resolveContainerCmd([]string{"sleep", "3600"},
		[]string{"PATH=" + d1 + ":" + d2})
	if err != nil {
		t.Fatalf("应能在 PATH 中找到: %v", err)
	}
	if got != want {
		t.Errorf("解析结果 = %q，期望 %q", got, want)
	}
}

// TestResolveContainerCmdKeepsArgv0 验证用解析后的路径 execve，但 argv[0] 不变。
//
// 程序通过 argv[0] 看到的应当仍是用户写的命令名（与 shell 一致）；
// 若把 argv[0] 也换成绝对路径，`busybox` 这类靠 argv[0] 分派 applet 的
// 程序会行为异常。
func TestResolveContainerCmdKeepsArgv0(t *testing.T) {
	d := t.TempDir()
	mkExec(t, d, "mysleep")
	cmdline := []string{"mysleep", "10"}

	got, err := resolveContainerCmd(cmdline, []string{"PATH=" + d})
	if err != nil {
		t.Fatal(err)
	}
	if got == cmdline[0] {
		t.Error("返回值应是解析后的路径，而不是原命令名")
	}
	// 调用方负责保持 argv[0]；这里断言原切片未被修改。
	if cmdline[0] != "mysleep" {
		t.Errorf("resolveContainerCmd 不应修改入参 cmdline，得到 %q", cmdline[0])
	}
}

// TestResolveContainerCmdNotFound 验证找不到时报错，且错误里列出 PATH 与已查找路径。
//
// 「列出查过的目录」是可用性要求：用户拿到 ENOENT 时最需要知道的是
// "我到底在哪些目录里找过"，而不是一句 no such file。
func TestResolveContainerCmdNotFound(t *testing.T) {
	d := t.TempDir()
	_, err := resolveContainerCmd([]string{"definitely-not-here"}, []string{"PATH=" + d})
	if err == nil {
		t.Fatal("找不到可执行文件时应报错")
	}
	msg := err.Error()
	for _, want := range []string{"definitely-not-here", d, "PATH"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应包含 %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "已查找") {
		t.Errorf("错误应列出已查找的路径:\n%s", msg)
	}
}

// TestResolveContainerCmdDefaultsPathWhenMissing 验证 PATH 缺失时用 Docker 默认值。
//
// 这条是用户明确要求的"缺 PATH 就补默认"。没有它，镜像未提供 PATH 时
// 裸命令名完全无法解析。
func TestResolveContainerCmdDefaultsPathWhenMissing(t *testing.T) {
	// 环境里没有任何 PATH：应回退到默认值。默认值里的目录在测试机上大多
	// 不存在，因此这里断言的是"报错信息里出现默认 PATH"而不是"找得到"。
	_, err := resolveContainerCmd([]string{"definitely-not-here"}, nil)
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), defaultContainerPath) {
		t.Errorf("PATH 缺失时应回退到默认值 %q，错误信息却是:\n%s",
			defaultContainerPath, err.Error())
	}
	// 空字符串与只有冒号的 PATH 同样视为缺失/退化，不应 panic。
	for _, env := range [][]string{{"PATH="}, {"PATH=:"}, {"PATH=   "}} {
		if _, err := resolveContainerCmd([]string{"nope"}, env); err == nil {
			t.Errorf("env=%v 应报错", env)
		}
	}
}

// TestResolveContainerCmdUsesDefaultPathToFindRealBinary 用真实存在的文件验证
// 「缺 PATH → 默认值 → 真的能找到」这条完整链路。
//
// 用一个必然存在于默认 PATH 目录里的可执行文件（这里选 /bin/sh）；
// 若测试机没有它就跳过。
func TestResolveContainerCmdUsesDefaultPathToFindRealBinary(t *testing.T) {
	if !isExecutableFile("/bin/sh") && !isExecutableFile("/usr/bin/sh") {
		t.Skip("测试机没有 /bin/sh，跳过")
	}
	got, err := resolveContainerCmd([]string{"sh"}, nil)
	if err != nil {
		t.Fatalf("应能用默认 PATH 找到 sh: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("解析结果应是绝对路径，得到 %q", got)
	}
	if !isExecutableFile(got) {
		t.Errorf("解析结果应可执行: %q", got)
	}
}

// TestResolveContainerCmdSkipsNonExecutable 验证 PATH 里同名但不可执行的文件被跳过。
func TestResolveContainerCmdSkipsNonExecutable(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	// d1 里有同名文件但不可执行。
	if err := os.WriteFile(filepath.Join(d1, "tool"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := mkExec(t, d2, "tool")

	got, err := resolveContainerCmd([]string{"tool"}, []string{"PATH=" + d1 + ":" + d2})
	if err != nil {
		t.Fatalf("应跳过不可执行项并继续查找: %v", err)
	}
	if got != want {
		t.Errorf("解析结果 = %q，期望跳过不可执行项后得到 %q", got, want)
	}
}

// TestResolveContainerCmdSkipsDirectory 验证 PATH 里同名**目录**被跳过（不是可执行文件）。
func TestResolveContainerCmdSkipsDirectory(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	// d1 里有个同名目录，且带执行位（目录天然有 x 位）——不能被误判为可执行文件。
	if err := os.MkdirAll(filepath.Join(d1, "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := mkExec(t, d2, "tool")

	got, err := resolveContainerCmd([]string{"tool"}, []string{"PATH=" + d1 + ":" + d2})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("目录不应被当作可执行文件，得到 %q 期望 %q", got, want)
	}
}

// TestResolveContainerCmdFollowsSymlink 验证 PATH 查找会跟随符号链接。
//
// 这是 Alpine 的真实形态：/bin/sleep 是指向 /bin/busybox 的软链。
func TestResolveContainerCmdFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := mkExec(t, dir, "busybox")
	link := filepath.Join(dir, "sleep")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	got, err := resolveContainerCmd([]string{"sleep"}, []string{"PATH=" + dir})
	if err != nil {
		t.Fatalf("应跟随软链找到目标: %v", err)
	}
	if got != link {
		t.Errorf("解析结果 = %q，期望软链路径 %q", got, link)
	}
}

// TestResolveContainerCmdEmptyPathEntryMeansCwd 验证 PATH 中的空项按 POSIX 语义
// 表示当前目录。
func TestResolveContainerCmdEmptyPathEntryMeansCwd(t *testing.T) {
	dir := t.TempDir()
	mkExec(t, dir, "localtool")
	old, err := os.Getwd()
	if err != nil {
		t.Skip("无法取 cwd")
	}
	if err := os.Chdir(dir); err != nil {
		t.Skip("无法 chdir")
	}
	defer func() { _ = os.Chdir(old) }()

	got, err := resolveContainerCmd([]string{"localtool"}, []string{"PATH=/nonexistent:"})
	if err != nil {
		t.Fatalf("PATH 空项应表示当前目录: %v", err)
	}
	if got == "" {
		t.Error("应解析出路径")
	}
}

// TestResolveContainerCmdRejectsEmpty 验证空命令明确报错。
func TestResolveContainerCmdRejectsEmpty(t *testing.T) {
	for _, cmdline := range [][]string{nil, {}, {""}, {"   "}} {
		if _, err := resolveContainerCmd(cmdline, nil); err == nil {
			t.Errorf("cmdline=%v 应被拒绝", cmdline)
		}
	}
}

// TestEnvValue 验证 env 取值（含重复键时取第一个，与 os.Getenv 语义一致）。
func TestEnvValue(t *testing.T) {
	env := []string{"A=1", "PATH=/bin:/usr/bin", "A=2"}
	if got := envValue(env, "PATH"); got != "/bin:/usr/bin" {
		t.Errorf("PATH = %q", got)
	}
	if got := envValue(env, "A"); got != "1" {
		t.Errorf("重复键应取第一个（与 os.Getenv 一致），得到 %q", got)
	}
	if got := envValue(env, "MISSING"); got != "" {
		t.Errorf("不存在的键应返回空串，得到 %q", got)
	}
}
