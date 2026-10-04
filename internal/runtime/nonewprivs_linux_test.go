// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 子进程 helper 的约定（与 exec_linux_more_test.go 的 PTY helper 同一手法）：
// PR_SET_NO_NEW_PRIVS 单向不可逆，一旦在主测试进程里设置就无法清除，
// 会污染同包后续所有测试，因此必须在子进程里验证。
const (
	helperEnvNNP  = "LICORE_NNP_HELPER" // 为 "1" 时当前进程是 helper
	helperEnvMode = "LICORE_NNP_MODE"   // set | get | set-exec
)

// readNoNewPrivsFromSelf 读 /proc/self/status 的 NoNewPrivs 字段（"0"/"1"）。
func readNoNewPrivsFromSelf() (string, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "NoNewPrivs:"); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("/proc/self/status 里没有 NoNewPrivs 字段（内核过旧？）")
}

// envWith 在 base 之上覆盖环境变量：先剔除同名旧值再追加。
//
// 不能简单地 append(os.Environ(), "K=V")——那样会出现重复的 K，
// 而 os.Getenv 只认**第一个**匹配项，于是"覆盖"实际不生效。
// helper 的 set-exec 模式正是踩了这个坑：子进程仍读到旧的 mode，
// 再次 exec 自己，形成无限重执行。
func envWith(base []string, kv ...string) []string {
	out := make([]string, 0, len(base)+len(kv))
	for _, e := range base {
		drop := false
		for _, n := range kv {
			if key, _, ok := strings.Cut(n, "="); ok && strings.HasPrefix(e, key+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return append(out, kv...)
}

// TestNoNewPrivsHelperProcess 是 runNNPHelper 的子进程入口。
// 未设置 helper 环境变量时立即返回，正常测试运行时为空操作。
func TestNoNewPrivsHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvNNP) != "1" {
		return
	}
	mode := os.Getenv(helperEnvMode)

	if mode == "set" || mode == "set-exec" {
		if err := setNoNewPrivs(); err != nil {
			fmt.Printf("SET_FAILED:%v\n", err)
			os.Exit(1)
		}
	}

	if mode == "set-exec" {
		// 关键验证：no_new_privs 必须在 execve 之后依然生效——否则它对
		// 容器毫无意义（我们正是要在 exec 用户命令前设上它）。
		exe, err := os.Executable()
		if err != nil {
			fmt.Printf("EXE_FAILED:%v\n", err)
			os.Exit(1)
		}
		// 必须覆盖 mode（而不是追加），否则子进程读到的仍是 set-exec。
		env := envWith(os.Environ(), helperEnvNNP+"=1", helperEnvMode+"=get")
		// syscall.Exec 成功则不返回。
		if err := syscall.Exec(exe, []string{exe, "-test.run=TestNoNewPrivsHelperProcess"}, env); err != nil {
			fmt.Printf("EXEC_FAILED:%v\n", err)
			os.Exit(1)
		}
	}

	v, err := readNoNewPrivsFromSelf()
	if err != nil {
		fmt.Printf("READ_FAILED:%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("NNP=%s\n", v)
	os.Exit(0)
}

// runNNPHelper 在子进程里以指定 mode 跑 helper，返回它的输出。
//
// 带超时是刻意的安全网：helper 会用 syscall.Exec 重执行自己，一旦环境变量
// 传递有误就会变成无限重执行（开发时就踩过一次），超时能让它以"失败"而不是
// "挂住整个测试套件"的形式暴露出来。
func runNNPHelper(t *testing.T, mode string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("取可执行文件路径失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe,
		"-test.run=TestNoNewPrivsHelperProcess", "-test.timeout=20s")
	cmd.Env = envWith(os.Environ(), helperEnvNNP+"=1", helperEnvMode+"="+mode)
	// 子进程超时后 CommandContext 只 kill 直接子进程；这里 helper 是 exec
	// 自替换，不会有更深的后代，因此足够。
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		t.Fatalf("helper(mode=%s) 超时未退出——疑似无限重执行\n输出:\n%s", mode, text)
	}
	if err != nil {
		t.Fatalf("helper(mode=%s) 失败: %v\n输出:\n%s", mode, err, text)
	}
	return text
}

// nnpValue 从 helper 输出里取出 NNP= 后面那个值。
func nnpValue(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "NNP="); ok {
			return v
		}
	}
	t.Fatalf("输出里没有 NNP= 字段:\n%s", out)
	return ""
}

// TestNoNewPrivsIsClearByDefault 是**对照组**：不调用 setNoNewPrivs 时
// 该标志必须为 0。
//
// 没有这条断言，"设为 1"的测试就无法排除"内核/环境本来就让它为 1"的可能，
// 也就证明不了真的是我们设置的。
func TestNoNewPrivsIsClearByDefault(t *testing.T) {
	out := runNNPHelper(t, "get")
	if got := nnpValue(t, out); got != "0" {
		t.Fatalf("对照组 NoNewPrivs = %s，期望 0（该标志默认必须为关）", got)
	}
}

// TestSetNoNewPrivs 验证 setNoNewPrivs 真的把标志置上了。
func TestSetNoNewPrivs(t *testing.T) {
	out := runNNPHelper(t, "set")
	if strings.Contains(out, "SET_FAILED") {
		// 极旧内核（< 3.5）不支持该 prctl；这不是代码缺陷，明确跳过。
		if strings.Contains(out, "invalid argument") {
			t.Skipf("内核不支持 PR_SET_NO_NEW_PRIVS: %s", out)
		}
		t.Fatalf("setNoNewPrivs 失败:\n%s", out)
	}
	if got := nnpValue(t, out); got != "1" {
		t.Fatalf("设置后 NoNewPrivs = %s，期望 1", got)
	}
}

// TestNoNewPrivsSurvivesExecve 验证该标志跨 execve 继承。
//
// 这是它对本项目**唯一有意义**的性质：我们的用法是"在 execve 用户命令之前
// 设上"，若不能跨 exec 继承，那容器里的用户命令就完全不受保护。
func TestNoNewPrivsSurvivesExecve(t *testing.T) {
	out := runNNPHelper(t, "set-exec")
	if got := nnpValue(t, out); got != "1" {
		t.Fatalf("execve 之后 NoNewPrivs = %s，期望 1（该标志必须跨 exec 继承）\n输出:\n%s", got, out)
	}
}
