// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// -------- helper 参数解析 --------

// TestParseExecSetupArgs 验证 helper 的 argv 解析。
//
// helper 在容器内被唤起，出错时几乎无法交互式排查，参数处理必须钉死。
func TestParseExecSetupArgs(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"带分隔符", []string{"exec-setup", "--", "/bin/sh", "-c", "echo hi"}, "/bin/sh,-c,echo hi"},
		{"不带分隔符", []string{"exec-setup", "/bin/sh", "-c", "echo hi"}, "/bin/sh,-c,echo hi"},
		{"单命令", []string{"exec-setup", "--", "/bin/ls"}, "/bin/ls"},
		{"命令以横杠开头", []string{"exec-setup", "--", "-weird"}, "-weird"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseExecSetupArgs(tc.argv)
			if err != nil {
				t.Fatalf("ParseExecSetupArgs(%v): %v", tc.argv, err)
			}
			if strings.Join(got, ",") != tc.want {
				t.Errorf("= %v，期望 %s", got, tc.want)
			}
		})
	}
}

// TestParseExecSetupArgsRejects 验证空 argv / 缺少命令时明确报错。
func TestParseExecSetupArgsRejects(t *testing.T) {
	for _, argv := range [][]string{
		nil,
		{},
		{"exec-setup"},
		{"exec-setup", "--"},
		{"exec-setup", "--", ""},
		{"exec-setup", "--", "   "},
	} {
		if _, err := ParseExecSetupArgs(argv); err == nil {
			t.Errorf("argv=%v 应被拒绝", argv)
		}
	}
}

// -------- 收口规格的环境变量编解码 --------

// TestExecSetupEnvMergesContainerAndExtraDrops 验证容器配置的 drop
// 与本次 exec 的 drop 被合并下发。
func TestExecSetupEnvMergesContainerAndExtraDrops(t *testing.T) {
	env := ExecSetupEnv([]string{"NET_RAW"}, []string{"SYS_CHROOT"})
	joined := strings.Join(env, ";")
	if !strings.Contains(joined, envExecSetup+"=1") {
		t.Errorf("必须带 helper 标记: %v", env)
	}
	if !strings.Contains(joined, "NET_RAW") || !strings.Contains(joined, "SYS_CHROOT") {
		t.Errorf("容器配置与本次额外的 drop 都要下发: %v", env)
	}
}

// TestExecSetupEnvEmptyDrops 验证无删除项时仍带标记（helper 要能识别自己）。
func TestExecSetupEnvEmptyDrops(t *testing.T) {
	env := ExecSetupEnv(nil, nil)
	if len(env) != 1 || env[0] != envExecSetup+"=1" {
		t.Errorf("无 drop 时只应有标记，得到 %v", env)
	}
}

// TestExecSetupEnvHasNoCapAdd 验证**不存在**任何放宽能力的通道。
//
// 这是安全边界：若 helper 能收到 capsAdd，exec 就成了绕过隔离的后门。
func TestExecSetupEnvHasNoCapAdd(t *testing.T) {
	env := ExecSetupEnv([]string{"NET_RAW"}, []string{"SYS_ADMIN"})
	for _, e := range env {
		if strings.HasPrefix(e, envCapsAdd+"=") {
			t.Fatalf("exec 路径**不允许**下发 %s（会放宽能力）: %v", envCapsAdd, env)
		}
	}
}

// TestExecSetupCapsDropParsing 验证从环境变量读回 drop 列表：去空、去重。
func TestExecSetupCapsDropParsing(t *testing.T) {
	t.Setenv(envCapsDrop, " SYS_ADMIN , NET_RAW ,, SYS_ADMIN ,")
	got := ExecSetupCapsDrop()
	if strings.Join(got, ",") != "SYS_ADMIN,NET_RAW" {
		t.Errorf("= %v，期望去空去重后的 [SYS_ADMIN NET_RAW]", got)
	}
	t.Setenv(envCapsDrop, "")
	if got := ExecSetupCapsDrop(); got != nil {
		t.Errorf("空值应返回 nil，得到 %v", got)
	}
}

// TestIsExecSetupProcess 验证 helper 标记识别。
func TestIsExecSetupProcess(t *testing.T) {
	t.Setenv(envExecSetup, "")
	if IsExecSetupProcess() {
		t.Error("未设置标记时不应识别为 helper")
	}
	t.Setenv(envExecSetup, "1")
	if !IsExecSetupProcess() {
		t.Error("设置标记后应识别为 helper")
	}
}

// -------- 只读 bind --------

// TestInstallExecHelperBindsReadOnly 验证 helper 被 bind 进 rootfs 且**只读**。
//
// 只读是安全要求：若可写，容器内就能替换 helper 的挂载内容，
// 让 exec 收口失效。这里在真实的 mount namespace 里验证（需要 root 时跳过）。
func TestInstallExecHelperBindsReadOnly(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("bind 挂载需要 root；真机验证见 scripts/verify-capabilities.sh")
	}
	rootfs := t.TempDir()
	if err := InstallExecHelper(rootfs); err != nil {
		t.Fatalf("InstallExecHelper: %v", err)
	}
	dst := rootfs + HelperPathInContainer
	if !execHelperAvailableInRootfs(rootfs) {
		t.Fatalf("helper 未出现在 rootfs 内：%s", dst)
	}
	// 只读断言：以写方式打开必须失败。
	f, err := os.OpenFile(dst, os.O_WRONLY, 0)
	if err == nil {
		_ = f.Close()
		t.Error("helper 必须是只读挂载，却能以写方式打开")
	}
	// 内容应与我们自身的二进制一致（bind 的是 licore 自己）。
	self, err := os.Executable()
	if err != nil {
		t.Skip("无法取自身路径")
	}
	want, err := os.Stat(self)
	if err != nil {
		t.Skip("无法 stat 自身")
	}
	got, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size() != want.Size() {
		t.Errorf("helper 大小 %d != licore 自身 %d（bind 的可能不是同一个文件）",
			got.Size(), want.Size())
	}
}

// TestInstallExecHelperCreatesDir 验证容器内目录被创建。
func TestInstallExecHelperCreatesDir(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("bind 挂载需要 root")
	}
	rootfs := t.TempDir()
	if err := InstallExecHelper(rootfs); err != nil {
		t.Fatalf("InstallExecHelper: %v", err)
	}
	fi, err := os.Stat(filepath.Join(rootfs, strings.TrimPrefix(HelperDirInContainer, "/")))
	if err != nil {
		t.Fatalf("未创建 %s: %v", HelperDirInContainer, err)
	}
	if !fi.IsDir() {
		t.Errorf("%s 应为目录", HelperDirInContainer)
	}
}

// -------- 收口的实际生效（不需要特权）--------

// execSetupProbeHelper 是子进程入口：装收口，然后 execve 一个打印状态的命令。
//
// RunExecSetup 以 execve 结尾、不返回，因此必须在子进程里跑。
const (
	envExecSetupProbe        = "LICORE_EXECSETUP_PROBE"
	envExecSetupUnshareProbe = "LICORE_EXECSETUP_UNSHARE_PROBE"
)

// TestExecSetupProbeHelperProcess 是 runExecSetupProbe 的子进程入口。
func TestExecSetupProbeHelperProcess(t *testing.T) {
	if os.Getenv(envExecSetupProbe) != "1" {
		return
	}
	// 打印收口后的状态。注意：/bin/sh 会在收口之后才被 execve。
	cmd := []string{"/bin/sh", "-c",
		`echo "NO_NEW_PRIVS=$(awk '/^NoNewPrivs:/{print $2}' /proc/self/status)"; ` +
			`echo "SECCOMP=$(awk '/^Seccomp:/{print $2}' /proc/self/status)"; ` +
			`echo "CMD_RAN=yes"`}
	if err := RunExecSetup(cmd); err != nil {
		// 收口失败必须显式报告，不能静默当作"命令没跑"。
		fmt.Printf("EXEC_SETUP_FAILED:%v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestExecSetupProbeUnshareHelper 是另一个子进程入口：收口后**用 Go 直接**
// 调 unshare(0) 并报告结果。
//
// 为什么不用 shell：`unshare` 命令依赖 util-linux 存在，而这里要测的是
// **系统调用是否被 seccomp 拦下**。直接用 syscall 才不引入额外依赖。
func TestExecSetupProbeUnshareHelper(t *testing.T) {
	if os.Getenv(envExecSetupUnshareProbe) != "1" {
		return
	}
	// 先做收口，再执行一个"报告 unshare 结果"的小 Go 程序。
	// 由于 RunExecSetup 以 execve 结尾，这里让目标命令是**测试二进制自身**，
	// 并带上一个标记环境变量进入下面的分支。
	if os.Getenv("LICORE_EXECSETUP_INNER") != "1" {
		cmd := []string{os.Args[0], "-test.run=TestExecSetupProbeUnshareHelper"}
		os.Setenv("LICORE_EXECSETUP_INNER", "1")
		if err := RunExecSetup(cmd); err != nil {
			fmt.Printf("EXEC_SETUP_FAILED:%v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	// 此时已经在收口之后（helper 已 execve 本二进制）。
	err := syscall.Unshare(0)
	fmt.Printf("UNSHARE_ERR=%v\n", err)
	fmt.Printf("UNSHARE_IS_EPERM=%v\n", err == syscall.EPERM)
	os.Exit(0)
}

// runExecSetupProbe 在子进程里跑完整收口序列并返回其输出。
func runExecSetupProbe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("无法取自身路径: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe,
		"-test.run=TestExecSetupProbeHelperProcess", "-test.timeout=20s")
	cmd.Env = envWith(os.Environ(), envExecSetupProbe+"=1")
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		t.Fatalf("helper 超时未退出\n输出:\n%s", text)
	}
	// 无 CAP_SETPCAP 的环境（普通用户/受限沙箱）无法清边界集，
	// 收口会**正确地**失败而不是静默降级。这属于环境限制，跳过而非判失败。
	if strings.Contains(text, "PR_CAPBSET_DROP") && strings.Contains(text, "not permitted") {
		t.Skipf("当前进程无 CAP_SETPCAP，无法验证真实裁剪（真机验证见 scripts/verify-capabilities.sh）:\n%s", text)
	}
	if err != nil {
		t.Fatalf("helper 失败: %v\n输出:\n%s", err, text)
	}
	return text
}

// TestExecSetupProbe 验证 exec 收口序列真的生效：
//   - NoNewPrivs = 1（P0-1 的收口在 exec 路径上也做了）
//   - Seccomp = 2（filter，P1 的收口在 exec 路径上也做了）
//   - 用户命令确实被执行（收口没有把命令本身挡掉）
//
// 这两项在**无特权**环境即可验证：装 seccomp 过滤器只要求 no_new_privs，
// 不需要任何 capability。capability 裁剪需要 CAP_SETPCAP，故不在本用例内。
func TestExecSetupProbe(t *testing.T) {
	out := runExecSetupProbe(t)
	if strings.Contains(out, "EXEC_SETUP_FAILED") {
		t.Fatalf("收口失败:\n%s", out)
	}
	if !strings.Contains(out, "NO_NEW_PRIVS=1") {
		t.Errorf("exec 收口后 NoNewPrivs 应为 1，输出:\n%s", out)
	}
	if !strings.Contains(out, "SECCOMP=2") {
		t.Errorf("exec 收口后 Seccomp 应为 2（filter），输出:\n%s", out)
	}
	if !strings.Contains(out, "CMD_RAN=yes") {
		t.Errorf("收口后应执行用户命令，输出:\n%s", out)
	}
}

// TestExecSetupProbeBlocksDangerousSyscall 验证收口后的进程**真的被拦**，
// 而不只是 /proc 里显示了个数字。
//
// 探针用 unshare(0)：不需要任何 capability、正常情况下成功，
// 因此 EPERM 只可能来自 seccomp 过滤器。
func TestExecSetupProbeBlocksDangerousSyscall(t *testing.T) {
	if !probeUsable(t) {
		t.Skip("本环境 unshare(0) 本来就不可用，探针无判别力")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("无法取自身路径: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe,
		"-test.run=TestExecSetupProbeUnshareHelper", "-test.timeout=20s")
	cmd.Env = envWith(os.Environ(), envExecSetupUnshareProbe+"=1")
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		t.Fatalf("helper 超时未退出\n输出:\n%s", text)
	}
	// 同 runExecSetupProbe：无 CAP_SETPCAP 时收口会正确地失败，
	// 属于环境限制而非代码缺陷。seccomp 那一环另有
	// TestSeccompGateInExecSetupIndependentOfCaps 单独覆盖。
	if strings.Contains(text, "PR_CAPBSET_DROP") && strings.Contains(text, "not permitted") {
		t.Skipf("当前进程无 CAP_SETPCAP，无法走完整收口路径:\n%s", text)
	}
	if err != nil {
		t.Fatalf("helper 失败: %v\n输出:\n%s", err, text)
	}
	if !strings.Contains(text, "UNSHARE_IS_EPERM=true") {
		t.Errorf("exec 收口后的进程应被 seccomp 拦住 unshare，输出:\n%s", text)
	}
}

// TestSeccompGateInExecSetupIndependentOfCaps 验证「seccomp 收口」这一环
// 不依赖 capability 裁剪是否成功。
//
// 为什么单独测：真实环境里两步都会成功，但在无 CAP_SETPCAP 的环境里能力裁剪
// 会先失败，把 seccomp 的效果一起掩盖掉——那样这两个测试在本机永远是 skip，
// 就失去了回归价值。这里直接调用 installSeccompFilter（与 RunExecSetup 用的是
// 同一个函数），确认它在 no_new_privs 之后确实生效。
func TestSeccompGateInExecSetupIndependentOfCaps(t *testing.T) {
	if !probeUsable(t) {
		t.Skip("本环境 unshare(0) 本来就不可用，探针无判别力")
	}
	// 复用 seccomp 包里的 helper：它做的就是 no_new_privs + installSeccompFilter，
	// 与 RunExecSetup 的第 1、3 步完全一致。
	out := runSeccompHelper(t, "install")
	if got := lineWith(t, out, "SECCOMP="); got != "SECCOMP=2" {
		t.Errorf("Seccomp 模式 = %s，期望 2", got)
	}
	if got := lineWith(t, out, "UNSHARE_IS_EPERM="); got != "UNSHARE_IS_EPERM=true" {
		t.Errorf("seccomp 收口应拦住 unshare，得到 %s", got)
	}
}
