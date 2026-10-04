// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestOpenptySetsidTIOCSCTTYOrder 是控制终端顺序的回归测试。
//
// 背景：仅把 pty 从端 dup 到子进程 stdio 并不够——它必须成为子进程的
// **控制终端**，否则 job control 与 Ctrl+C 的信号投递都会失效。
// 建立控制终端要求先 setsid()（成为会话首进程）再 TIOCSCTTY；
// 顺序反过来 TIOCSCTTY 会失败。
//
// 用 re-exec 子进程验证两个方向，避免在本进程调用 setsid 影响测试进程：
//   - mode=setsid-first ：setsid() 后 TIOCSCTTY → 必须成功，且 /proc/self/stat
//     的 tty 字段非 0（真的拿到了控制终端）；
//   - mode=no-setsid    ：直接 TIOCSCTTY → 必须失败（证明顺序是硬约束）。
func TestOpenptySetsidTIOCSCTTYOrder(t *testing.T) {
	if !ptyAvailable() {
		t.Skipf("%s 不可用", devPtmxPath)
	}
	for _, mode := range []string{"setsid-first", "no-setsid"} {
		t.Run(mode, func(t *testing.T) {
			_, slave, err := openpty()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = slave.Close() }()

			out := runPTYHelper(t, mode, slave)
			switch mode {
			case "setsid-first":
				if out != "ok" {
					t.Fatalf("setsid 后 TIOCSCTTY 应成功，得到 %q", out)
				}
			case "no-setsid":
				if out == "ok" {
					t.Fatal("未 setsid 时 TIOCSCTTY 竟成功：顺序约束不成立，说明测试环境特殊")
				}
				t.Logf("未 setsid 时 TIOCSCTTY 如预期失败: %s", out)
			}
		})
	}
}

// runPTYHelper 以 re-exec 方式在子进程里执行 pty 会话辅助逻辑，返回其输出。
func runPTYHelper(t *testing.T, mode string, slave *os.File) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("取可执行文件路径失败: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestPTYHelperProcess", "--", mode)
	cmd.Env = append(os.Environ(),
		"LICORE_PTY_HELPER=1",
		"LICORE_PTY_MODE="+mode,
		"LICORE_PTY_SLAVE="+strconv.Itoa(int(slave.Fd())),
	)
	// 把从端作为 stdin 传下去，子进程里它就是 fd 0。
	cmd.Stdin = slave
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

// TestPTYHelperProcess 是 runPTYHelper 的子进程入口：仅在设置了
// LICORE_PTY_HELPER 时才做事，否则立即返回（正常测试运行时为空操作）。
func TestPTYHelperProcess(t *testing.T) {
	if os.Getenv("LICORE_PTY_HELPER") != "1" {
		return
	}
	mode := os.Getenv("LICORE_PTY_MODE")

	// stdin（fd 0）是 openpty 的从端。
	stdin := os.Stdin
	if !isTerminal(stdin.Fd()) {
		fmt.Println("stdin-not-a-tty")
		os.Exit(3)
	}

	if mode == "setsid-first" {
		// setsid() 使自己成为新会话的首进程，之后才能 TIOCSCTTY。
		if _, err := syscall.Setsid(); err != nil {
			fmt.Printf("setsid-failed:%v\n", err)
			os.Exit(4)
		}
	}
	// 无论哪种模式都尝试设置控制终端。
	if errno := ioctlInt(stdin.Fd(), tiocsctty, 1); errno != 0 {
		fmt.Printf("tiocsctty-errno-%d\n", int(errno))
		os.Exit(5)
	}
	fmt.Println("ok")
	os.Exit(0)
}

// tiocsctty 是 TIOCSCTTY 的 ioctl 号（把 fd 设为控制终端）。
const tiocsctty = 0x540E

// ioctlInt 以 int 参数调用 ioctl，返回 errno（0 表示成功）。
func ioctlInt(fd uintptr, req, arg uintptr) syscall.Errno {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	return errno
}

// TestExecIsolationEnvNotLeaked 覆盖 -e 环境变量传递不会把 LICORE_* 内部变量
// 泄漏进容器（envWithoutLiCore 的职责）。
func TestExecIsolationEnvNotLeaked(t *testing.T) {
	// 构造含内部变量与用户变量的环境。
	in := []string{
		"LICORE_NET_MODE=bridge",
		"LICORE_CGROUP_ID=abc",
		"LICORE_CONTAINER=x",
		"PATH=/usr/bin",
		"FOO=bar",
	}
	got := envWithoutLiCoreFrom(in)
	for _, kv := range got {
		if len(kv) >= 6 && kv[:6] == "LICORE_" {
			t.Errorf("内部变量泄漏进容器环境: %q", kv)
		}
	}
	// 用户变量必须保留。
	var hasPath, hasFoo bool
	for _, kv := range got {
		if kv == "PATH=/usr/bin" {
			hasPath = true
		}
		if kv == "FOO=bar" {
			hasFoo = true
		}
	}
	if !hasPath || !hasFoo {
		t.Errorf("用户环境变量被误删: %v", got)
	}
}

// TestIsTerminalDistinguishes 验证 isTerminal 能区分终端与普通文件。
// 这条断言本身也在守护"必须传有效指针"这一教训：传 nil 会让两者
// 都返回 false（无从区分），传有效指针才能正确区分。
func TestIsTerminalDistinguishes(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if isTerminal(f.Fd()) {
		t.Error("普通文件不应被判定为终端")
	}
	if !ptyAvailable() {
		return
	}
	m, s, err := openpty()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close(); _ = s.Close() }()
	if !isTerminal(s.Fd()) {
		t.Error("pty 从端应被判定为终端")
	}
}

// TestDevPtsDirSeamIsUsed 验证 openpty 确实通过 seam 解析从端路径，
// 使测试注入生效（否则上面的故障注入测试都是空跑）。
func TestDevPtsDirSeamIsUsed(t *testing.T) {
	if !ptyAvailable() {
		t.Skipf("%s 不可用", devPtmxPath)
	}
	custom := filepath.Join(t.TempDir(), "pts")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	oldD := devPtsDir
	devPtsDir = custom
	t.Cleanup(func() { devPtsDir = oldD })

	// 自定义目录下没有对应从端节点 → 打开必然失败，说明路径确实被使用。
	_, _, err := openpty()
	if err == nil {
		t.Fatal("从端路径未走 seam（自定义空目录下不应成功）")
	}
	_ = strconv.Itoa(0)
}

// TestExecNoNsenterKeepsInstallHint 回归：exec 在缺 nsenter 时，报错必须
// **保留安装指引**，而不能退化成裸哨兵。
//
// 背景：Exec 曾在此处预判 execns.Enabled() 并直接返回 fmt.Errorf("%w",
// ErrNoNsenter)，于是用户只看到 "exec: exec 需要 nsenter"，丢掉了
// nsenterMissingError 里「sudo apt install util-linux / 装 Magisk、busybox」
// 这条唯一能指导用户脱困的信息。现在改为把判断交给 Enter，由它产出完整文案。
//
// 用一个不带 nsenter 的 PATH 复用真实代码路径；需要 root 才能走到这步以外，
// 但缺 nsenter 的判断在 root 检查之后、Enter 之前，因此非 root 下会先返回
// ErrNotRoot，此时跳过（该分支由 root 环境的 CI 任务覆盖）。
func TestExecNoNsenterKeepsInstallHint(t *testing.T) {
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir()) // 空目录：nsenter 与 busybox 都找不到
	defer func() { _ = os.Setenv("PATH", origPath) }()

	_, err := Exec(&ExecOptions{TargetPID: os.Getpid(), Cmd: []string{"/bin/true"}})
	if err == nil {
		t.Fatal("缺 nsenter 应报错")
	}
	if errors.Is(err, ErrNotRoot) {
		t.Skip("非 root：Exec 在缺 nsenter 判定之前先返回 ErrNotRoot，该分支由 root CI 覆盖")
	}
	for _, want := range []string{"util-linux", "Magisk", "busybox"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("缺 nsenter 的报错丢了安装指引，缺少 %q：\n%v", want, err)
		}
	}
}
