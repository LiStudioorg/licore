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
	_, err := Enter(1234, "", "", nil, 0, 1, 2, []string{"/bin/sh"}, "")
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
	got, err := buildNsenterArgv(prog, 4242, []string{"/bin/sh", "-c", "hostname"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/bin/nsenter",
		"-t", "4242",
		"-m", "-u", "-i", "-n", "-p",
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
	got, err := buildNsenterArgv(prog, 7, []string{"/bin/echo", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/bin/busybox", "nsenter", "-t", "7", "-m", "-u", "-i", "-n", "-p", "--", "/bin/echo", "hi"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv =\n  %v\nwant\n  %v", got, want)
	}
}

// TestBuildNsenterArgvUsesShortOptions 回归：必须用短选项。
// busybox 的 nsenter **不支持任何长选项**（--target 直接报错），
// 所以长选项虽然更可读却不是可移植选择。
func TestBuildNsenterArgvUsesShortOptions(t *testing.T) {
	prog := &nsenterProg{path: "/usr/bin/nsenter"}
	got, err := buildNsenterArgv(prog, 1, []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if strings.HasPrefix(a, "--") && a != "--" {
			t.Errorf("出现了长选项 %q（busybox 不支持）: %v", a, got)
		}
	}
}

// TestBuildNsenterArgvNeverSetsRootOrWorkdir 回归：命令行里**不得**出现 -r 与 -w。
//
// 两条都来自真机实测的教训，而且本仓库曾经把结论写反过：
//
//	setns(CLONE_NEWNS) 之后进程根目录已经是容器 root，nsenter 再 chroot
//	一次属于多余操作，并且会把 cwd 搞坏；
//
//	nsenter 的 -w 是"先 chdir 再 setns"——chdir 发生在**宿主** mount
//	namespace，随后切到容器 mount ns，cwd 指向的 inode 在新视图里可能不存在，
//	getcwd 直接失败。实测（真机，容器 PID 1）：
//
//	  nsenter -t <pid> -m -u -i -n -p      -- /bin/busybox pwd  → /
//	  nsenter -t <pid> -m -u -i -n -p -w/tmp -- /bin/busybox pwd → getcwd 报错
//
// 工作目录改由容器内的 helper 自己 chdir（见 wrapWithHelper 与
// TestWrapWithHelperCarriesWorkdir）。这条用例把"不许再加回 -r/-w"钉死。
func TestBuildNsenterArgvNeverSetsRootOrWorkdir(t *testing.T) {
	prog := &nsenterProg{path: "/usr/bin/nsenter"}
	got, err := buildNsenterArgv(prog, 1, []string{"/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a == "-r/" || strings.HasPrefix(a, "-r") {
			t.Errorf("不应出现 -r（会导致 cwd 失效）: %v", got)
		}
		if a == "-w" || strings.HasPrefix(a, "-w") {
			t.Errorf("不应出现 -w（nsenter 的 -w 在 mount ns 切换后失效）: %v", got)
		}
	}
}

// TestBuildNsenterArgvDoubleDashProtectsDashCommand 目标命令以 '-' 开头时
// 必须靠 "--" 终止选项解析，否则会被 nsenter 当成自己的选项。
func TestBuildNsenterArgvDoubleDashProtectsDashCommand(t *testing.T) {
	prog := &nsenterProg{path: "/usr/bin/nsenter"}
	got, err := buildNsenterArgv(prog, 1, []string{"-weird-binary", "-x"})
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
			if _, err := buildNsenterArgv(tc.prog, tc.pid, tc.cmd); err == nil {
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
	argv, err := buildNsenterArgv(prog, 1, []string{"/bin/sh"})
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
	argv, _ := buildNsenterArgv(prog, 1, []string{"/bin/sh"})
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
	argv, err := buildNsenterArgv(prog, selfPID(), []string{"/bin/echo", "nsenter-smoke"})
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

// TestWrapWithHelper 验证目标命令被包成「helper 收口 → 用户命令」。
//
// 这是 exec 隔离成立的关键：nsenter 执行的是 helper，helper 做完收口
// 再 execve 用户命令。若包装丢了，"exec 只允许收紧"就无从谈起。
func TestWrapWithHelper(t *testing.T) {
	// helperPath 为空：保持原行为（不包）。
	got := wrapWithHelper("", "", []string{"/bin/sh", "-c", "hi"})
	if strings.Join(got, " ") != "/bin/sh -c hi" {
		t.Errorf("空 helperPath 应原样返回，得到 %v", got)
	}

	// 无 workdir：包成 <helper> exec-setup -- <cmd...>
	got = wrapWithHelper("/.licore/exec-helper", "", []string{"/bin/sh", "-c", "hi"})
	want := "/.licore/exec-helper exec-setup -- /bin/sh -c hi"
	if strings.Join(got, " ") != want {
		t.Errorf("argv =\n  %v\nwant\n  %v", got, want)
	}
}

// TestWrapWithHelperCarriesWorkdir 验证 workdir 经 argv 传给 helper。
//
// 这是 -w 的**替代实现**：nsenter 的 -w 在 mount ns 切换后会失效
// （先 chdir 再 setns），因此改由容器内的 helper 自己 chdir。
// 若 workdir 没被带进 argv，`licore exec 容器 -w /app cmd` 会静默落在 /。
func TestWrapWithHelperCarriesWorkdir(t *testing.T) {
	got := wrapWithHelper("/.licore/exec-helper", "/app", []string{"/bin/sh"})
	want := "/.licore/exec-helper exec-setup --workdir /app -- /bin/sh"
	if strings.Join(got, " ") != want {
		t.Errorf("argv =\n  %v\nwant\n  %v", got, want)
	}
	// workdir 必须在 -- 之前，否则会被当成用户命令的一部分。
	iWD, iDD := indexOfStr(got, "--workdir"), indexOfStr(got, "--")
	if iWD < 0 || iDD < 0 || iWD > iDD {
		t.Errorf("--workdir 必须在 -- 之前: %v", got)
	}

	// 空或 "/"：不下发 --workdir（进容器后 cwd 天然是 /）。
	for _, wd := range []string{"", "/"} {
		if indexOfStr(wrapWithHelper("/.licore/exec-helper", wd, []string{"/bin/sh"}), "--workdir") >= 0 {
			t.Errorf("workdir=%q 时不应下发 --workdir", wd)
		}
	}
}

// indexOfStr 返回 s 在切片中的下标，不存在返回 -1。
func indexOfStr(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}

// TestBuildNsenterArgvWithHelper 验证经 nsenter 之后的完整命令行。
//
// 断言完整形态，确保 -- 分隔符与 helper 位置正确：
// 少了 -- 时，以 '-' 开头的用户命令会被 nsenter 当成自己的选项。
func TestBuildNsenterArgvWithHelper(t *testing.T) {
	prog := &nsenterProg{path: "/usr/bin/nsenter"}
	cmd := wrapWithHelper("/.licore/exec-helper", "", []string{"/bin/sh", "-c", "hostname"})
	got, err := buildNsenterArgv(prog, 4242, cmd)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/bin/nsenter",
		"-t", "4242",
		"-m", "-u", "-i", "-n", "-p",
		"--",
		"/.licore/exec-helper", "exec-setup", "--", "/bin/sh", "-c", "hostname",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv =\n  %v\nwant\n  %v", got, want)
	}
}

// TestBuildNsenterArgvWithHelperAndWorkdir 验证带 workdir 的完整形态：
// -w 不在 nsenter 侧，workdir 落在 helper 的 argv 里。
func TestBuildNsenterArgvWithHelperAndWorkdir(t *testing.T) {
	prog := &nsenterProg{path: "/usr/bin/nsenter"}
	cmd := wrapWithHelper("/.licore/exec-helper", "/tmp", []string{"/bin/sh", "-c", "pwd"})
	got, err := buildNsenterArgv(prog, 4242, cmd)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/bin/nsenter",
		"-t", "4242",
		"-m", "-u", "-i", "-n", "-p",
		"--",
		"/.licore/exec-helper", "exec-setup", "--workdir", "/tmp", "--",
		"/bin/sh", "-c", "pwd",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv =\n  %v\nwant\n  %v", got, want)
	}
	for _, a := range got {
		if strings.HasPrefix(a, "-w") {
			t.Errorf("nsenter 侧不应出现 -w: %v", got)
		}
	}
}
