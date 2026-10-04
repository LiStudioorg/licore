// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package execns

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// fakeLookPath 返回一个可注入的 lookPath 替身，并恢复原值。
//
// 这就是需求里说的 seam：探测逻辑（LookPath）与命令构造分离，
// 于是「nsenter 存在 / 只有 busybox / 都没有」三种情况都能在无 docker、
// 无 util-linux 的机器上确定性复现。
func fakeLookPath(t *testing.T, available map[string]string) {
	t.Helper()
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(file string) (string, error) {
		if p, ok := available[file]; ok {
			return p, nil
		}
		return "", fmt.Errorf("exec: %q: %w", file, exec.ErrNotFound)
	}
}

// TestFindNsenterPrefersSystemBinary 系统自带 nsenter 优先于 busybox。
func TestFindNsenterPrefersSystemBinary(t *testing.T) {
	fakeLookPath(t, map[string]string{
		"nsenter": "/usr/bin/nsenter",
		"busybox": "/bin/busybox",
	})
	got := findNsenter()
	if got == nil {
		t.Fatal("应找到 nsenter")
	}
	if got.path != "/usr/bin/nsenter" {
		t.Errorf("path = %q, want /usr/bin/nsenter", got.path)
	}
	if len(got.args) != 0 {
		t.Errorf("直接可执行时不应有前置参数，得到 %v", got.args)
	}
}

// TestFindNsenterFallsBackToBusybox Magisk / 精简 Android 环境常见：
// 没有独立 nsenter，但 busybox 是多调用二进制。
func TestFindNsenterFallsBackToBusybox(t *testing.T) {
	fakeLookPath(t, map[string]string{"busybox": "/data/adb/magisk/busybox"})
	got := findNsenter()
	if got == nil {
		t.Fatal("应回退到 busybox")
	}
	if got.path != "/data/adb/magisk/busybox" {
		t.Errorf("path = %q", got.path)
	}
	if len(got.args) != 1 || got.args[0] != "nsenter" {
		t.Errorf("busybox 后备应带 nsenter 子命令参数，得到 %v", got.args)
	}
}

// TestFindNsenterNone 两者都没有时返回 nil，Enabled 为 false。
func TestFindNsenterNone(t *testing.T) {
	fakeLookPath(t, map[string]string{})
	if got := findNsenter(); got != nil {
		t.Errorf("应返回 nil，得到 %+v", got)
	}
}

// TestEnterWithoutNsenterReportsInstallHint 缺 nsenter 时必须给出可执行的
// 安装指引，而不是一句无来由的失败。
func TestEnterWithoutNsenterReportsInstallHint(t *testing.T) {
	fakeLookPath(t, map[string]string{})
	_, err := Enter(1234, "", "", nil, 0, 1, 2, []string{"/bin/sh"})
	if err == nil {
		t.Fatal("缺 nsenter 应报错")
	}
	if !errors.Is(err, ErrNoNsenter) {
		t.Errorf("应包装 ErrNoNsenter: %v", err)
	}
	for _, want := range []string{"util-linux", "Magisk", "busybox"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("安装提示缺少 %q:\n%v", want, err)
		}
	}
}

// TestBuildNsenterArgvSystemBinary 命令构造的核心断言。
func TestBuildNsenterArgvSystemBinary(t *testing.T) {
	prog := &nsenterProg{path: "/usr/bin/nsenter"}
	got, err := buildNsenterArgv(prog, 4242, "", []string{"/bin/sh", "-c", "hostname"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/bin/nsenter",
		"-t", "4242",
		"-m", "-u", "-i", "-n", "-p",
		"-r/", "-w/",
		"--",
		"/bin/sh", "-c", "hostname",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv =\n  %v\nwant\n  %v", got, want)
	}
}

// TestBuildNsenterArgvBusybox 后备时要有 busybox nsenter 前缀。
func TestBuildNsenterArgvBusybox(t *testing.T) {
	prog := &nsenterProg{path: "/bin/busybox", args: []string{"nsenter"}}
	got, err := buildNsenterArgv(prog, 7, "", []string{"/bin/echo", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/bin/busybox", "nsenter", "-t", "7", "-m", "-u", "-i", "-n", "-p", "-r/", "-w/", "--", "/bin/echo", "hi"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv =\n  %v\nwant\n  %v", got, want)
	}
}

// TestBuildNsenterArgvUsesShortOptions 回归：必须用短选项。
// busybox 的 nsenter **不支持任何长选项**（--target 直接报错），
// 所以长选项虽然更可读却不是可移植选择。
func TestBuildNsenterArgvUsesShortOptions(t *testing.T) {
	prog := &nsenterProg{path: "/usr/bin/nsenter"}
	got, err := buildNsenterArgv(prog, 1, "", []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if strings.HasPrefix(a, "--") && a != "--" {
			t.Errorf("出现了长选项 %q（busybox 不支持）: %v", a, got)
		}
	}
}

// TestBuildNsenterArgvWorkdirAttached 回归：-w 必须紧贴路径。
// -w 的参数在 util-linux 与 busybox 上都是可选的，写成 "-w" "/app"
// 会把 /app 当成要执行的命令（实测报 failed to execute）。
func TestBuildNsenterArgvWorkdirAttached(t *testing.T) {
	prog := &nsenterProg{path: "/usr/bin/nsenter"}
	got, err := buildNsenterArgv(prog, 1, "/app", []string{"/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-w/app") {
		t.Errorf("workdir 应写成紧贴形式 -w/app，得到 %v", got)
	}
	for i, a := range got {
		if a == "-w" {
			t.Errorf("-w 与路径分开了（会被当成命令）: %v", got)
			_ = i
		}
	}
}

// TestBuildNsenterArgvAlwaysSetsRootAndWorkdir 回归：-r/ 与 -w 必须总是出现。
//
// 两个都是实测踩出来的坑：
//   - 不下发 -r/：nsenter 进 mount namespace 后 root 仍是宿主视图，-w 的绝对路径
//     解析基准不对；
//   - 不下发 -w：容器内进程继承宿主的 cwd，而该目录在容器里通常不存在，
//     `sh -c pwd` 报 getcwd() failed。
func TestBuildNsenterArgvAlwaysSetsRootAndWorkdir(t *testing.T) {
	prog := &nsenterProg{path: "/usr/bin/nsenter"}
	for _, workdir := range []string{"", "/app"} {
		got, err := buildNsenterArgv(prog, 1, workdir, []string{"/bin/sh"})
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(got, " ")
		if !strings.Contains(joined, "-r/ ") {
			t.Errorf("workdir=%q 时应总是带 -r/，得到 %v", workdir, got)
		}
		wantW := "-w" + workdir
		if workdir == "" {
			wantW = "-w/" // 缺省落到容器根
		}
		if !strings.Contains(joined, wantW+" ") {
			t.Errorf("workdir=%q 时应带 %s，得到 %v", workdir, wantW, got)
		}
	}
}

// TestBuildNsenterArgvDoubleDashProtectsDashCommand 目标命令以 '-' 开头时
// 必须靠 "--" 终止选项解析，否则会被 nsenter 当成自己的选项。
func TestBuildNsenterArgvDoubleDashProtectsDashCommand(t *testing.T) {
	prog := &nsenterProg{path: "/usr/bin/nsenter"}
	got, err := buildNsenterArgv(prog, 1, "", []string{"-weird-binary", "-x"})
	if err != nil {
		t.Fatal(err)
	}
	idx := -1
	for i, a := range got {
		if a == "--" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("缺少 -- 分隔符: %v", got)
	}
	if got[idx+1] != "-weird-binary" {
		t.Errorf("-- 之后应是目标命令，得到 %v", got[idx:])
	}
}

// TestBuildNsenterArgvRejectsBadInput 非法输入必须报错而不是构造出坏命令。
func TestBuildNsenterArgvRejectsBadInput(t *testing.T) {
	prog := &nsenterProg{path: "/usr/bin/nsenter"}
	for _, tc := range []struct {
		name string
		pid  int
		cmd  []string
		prog *nsenterProg
	}{
		{"pid 为 0", 0, []string{"/bin/sh"}, prog},
		{"pid 为负", -1, []string{"/bin/sh"}, prog},
		{"命令为空", 1, nil, prog},
		{"prog 为空", 1, []string{"/bin/sh"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildNsenterArgv(tc.prog, tc.pid, "", tc.cmd); err == nil {
				t.Error("应报错")
			}
		})
	}
}

// TestNsFlagsAreShortAndComplete 保证命名空间开关完整且都是短选项。
func TestNsFlagsAreShortAndComplete(t *testing.T) {
	want := []string{"-m", "-u", "-i", "-n", "-p"}
	if strings.Join(nsFlags, " ") != strings.Join(want, " ") {
		t.Errorf("nsFlags = %v, want %v", nsFlags, want)
	}
}

// TestParseExecUser 覆盖 uid/gid 解析。
func TestParseExecUser(t *testing.T) {
	for _, tc := range []struct {
		in      string
		uid     int
		gid     int
		ok      bool
		wantErr bool
	}{
		{"", 0, -1, false, false},
		{"1000", 1000, -1, true, false},
		{"1000:1000", 1000, 1000, true, false},
		{"0:0", 0, 0, true, false},
		{"abc", 0, -1, false, true},
		{"-5", 0, -1, false, true},
		{"1000:abc", 0, -1, false, true},
		{"1000:-1", 0, -1, false, true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			uid, gid, ok, err := parseExecUser(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseExecUser(%q) 应报错", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseExecUser(%q): %v", tc.in, err)
			}
			if uid != tc.uid || gid != tc.gid || ok != tc.ok {
				t.Errorf("parseExecUser(%q) = (%d,%d,%v), want (%d,%d,%v)",
					tc.in, uid, gid, ok, tc.uid, tc.gid, tc.ok)
			}
		})
	}
}

// TestInsertUserFlagsPlacement -S/-G 必须插在 "--" 之前。
func TestInsertUserFlagsPlacement(t *testing.T) {
	prog := &nsenterProg{path: "/usr/bin/nsenter"}
	argv, err := buildNsenterArgv(prog, 1, "", []string{"/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	got := insertUserFlags(argv, prog, 1000, 1000)
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-S 1000") || !strings.Contains(joined, "-G 1000") {
		t.Errorf("缺少 -S/-G: %v", got)
	}
	// -S/-G 必须在 -- 之前，否则会被当成目标命令的参数。
	dash := -1
	for i, a := range got {
		if a == "--" {
			dash = i
		}
	}
	for i, a := range got {
		if (a == "-S" || a == "-G") && i > dash {
			t.Errorf("-S/-G 出现在 -- 之后: %v", got)
		}
	}
}

// TestInsertUserFlagsOmitsGid 只给 uid 时不应出现 -G。
func TestInsertUserFlagsOmitsGid(t *testing.T) {
	prog := &nsenterProg{path: "/usr/bin/nsenter"}
	argv, _ := buildNsenterArgv(prog, 1, "", []string{"/bin/sh"})
	got := insertUserFlags(argv, prog, 1000, -1)
	if strings.Contains(strings.Join(got, " "), "-G") {
		t.Errorf("未指定 gid 时不应有 -G: %v", got)
	}
}

// TestEnabledMatchesDiscovery Enabled 应与探测结果一致。
func TestEnabledMatchesDiscovery(t *testing.T) {
	fakeLookPath(t, map[string]string{"nsenter": "/usr/bin/nsenter"})
	if !Enabled() {
		t.Error("有 nsenter 时 Enabled 应为 true")
	}
	fakeLookPath(t, map[string]string{})
	if Enabled() {
		t.Error("无 nsenter 时 Enabled 应为 false")
	}
}

// TestRealNsenterIfPresent 在真的装了 nsenter 的机器上做一次端到端冒烟：
// 进入自己的命名空间执行 true（不需 root，因为是自己已有的 ns）。
func TestRealNsenterIfPresent(t *testing.T) {
	if findNsenterReal() == nil {
		t.Skip("本机没有 nsenter，跳过真实调用冒烟")
	}
	// -t <自身 pid> 进入「自己已在的」命名空间，无需特权即可成功。
	prog := findNsenterReal()
	argv, err := buildNsenterArgv(prog, selfPID(), "", []string{"/bin/echo", "nsenter-smoke"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		// setns 连「进入自己所在命名空间」都需要 CAP_SYS_ADMIN。
		// 非 root 环境下这是预期结果，跳过而不是判失败——否则这个测试
		// 会在普通开发者机器上恒红，变成噪音。
		if strings.Contains(string(out), "Operation not permitted") {
			t.Skipf("非 root，setns 需要 CAP_SYS_ADMIN，跳过真实调用: %s", strings.TrimSpace(string(out)))
		}
		t.Fatalf("真实 nsenter 调用失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "nsenter-smoke") {
		t.Errorf("输出不含预期内容: %s", out)
	}
}

// findNsenterReal 是**不经过 seam** 的真实探测，仅供「本机是否真有 nsenter」
// 的冒烟测试使用。
func findNsenterReal() *nsenterProg {
	if p, err := exec.LookPath("nsenter"); err == nil && p != "" {
		return &nsenterProg{path: p}
	}
	if p, err := exec.LookPath("busybox"); err == nil && p != "" {
		return &nsenterProg{path: p, args: []string{"nsenter"}}
	}
	return nil
}

// selfPID 返回当前进程 pid，用作 nsenter 的 target（进入自己所在的命名空间）。
func selfPID() int { return os.Getpid() }
