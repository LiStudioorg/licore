// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"strings"
	"testing"
)

// TestRejectCapAddForExec 验证 exec 路径**拒绝** --cap-add。
//
// 这是安全边界而非参数风格：若允许 `licore exec --cap-add SYS_ADMIN`，
// 任何能执行 licore exec 的人都能把 CAP_SYS_ADMIN 加回来，
// 容器 init 里的 no_new_privs / cap-drop / seccomp 收口就全部形同虚设。
func TestRejectCapAddForExec(t *testing.T) {
	if err := rejectCapAddForExec(nil); err != nil {
		t.Errorf("不带 --cap-add 时应放行: %v", err)
	}
	for _, add := range [][]string{
		{"SYS_ADMIN"},
		{"CAP_SYS_ADMIN"},
		{"NET_RAW", "SYS_PTRACE"},
		{"ALL"},
	} {
		err := rejectCapAddForExec(add)
		if err == nil {
			t.Errorf("--cap-add %v 必须被拒绝", add)
			continue
		}
		// 错误信息要能让用户知道「怎么才能达到目的」，而不只是"不行"。
		for _, want := range []string{"不允许放宽", "cap-add", "licore run"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("错误应包含 %q 以给出可执行指引: %v", want, err)
			}
		}
	}
}

// TestExecCommandRejectsCapAddViaCLI 从 CLI 层验证拒绝发生在**做任何事之前**。
//
// 只测 rejectCapAddForExec 不够：如果它没被接进 RunE，参数照样能通过。
// 这里跑真实的命令构造路径。
func TestExecCommandRejectsCapAddViaCLI(t *testing.T) {
	cmd := newExecCommand(&strings.Builder{})
	cmd.SetArgs([]string{"--cap-add", "SYS_ADMIN", "somecontainer", "/bin/sh"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("CLI 层应拒绝 --cap-add")
	}
	if !strings.Contains(err.Error(), "不允许放宽") {
		t.Errorf("错误应说明不允许放宽能力: %v", err)
	}
	// 必须**早于**任何容器查找失败——否则用户会以为是自己容器名写错了。
	// 判据用「没有出现查找失败的措辞」而不是「没有出现'容器'二字」：
	// 拒绝信息本身就会提到"用 licore run 重新起一个容器"。
	for _, notWant := range []string{"未找到", "未在运行", "请先 licore pull"} {
		if strings.Contains(err.Error(), notWant) {
			t.Errorf("拒绝应发生在查找容器之前，却出现 %q: %v", notWant, err)
		}
	}
}

// TestExecCommandRejectsBadCapDropViaCLI 验证 exec 的 --cap-drop 也做名字校验。
func TestExecCommandRejectsBadCapDropViaCLI(t *testing.T) {
	cmd := newExecCommand(&strings.Builder{})
	cmd.SetArgs([]string{"--cap-drop", "NOT_A_REAL_CAP", "somecontainer", "/bin/sh"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("非法 --cap-drop 应被拒绝")
	}
	if !strings.Contains(err.Error(), "capability") {
		t.Errorf("错误应指出能力名非法: %v", err)
	}
}

// TestExecSetupCommandParsesArgs 验证隐藏命令把参数原样传给 runtime。
//
// 重点：用户命令可能含 -c 这类会被 cobra 吃掉的 flag，因此该命令必须
// DisableFlagParsing。这里断言 -c 没有被当成 licore 自己的参数。
func TestExecSetupCommandParsesArgs(t *testing.T) {
	cmd := newExecSetupCommand(&strings.Builder{})
	if !cmd.DisableFlagParsing {
		t.Fatal("exec-setup 必须禁用 flag 解析，否则用户命令里的 -c 会被吃掉")
	}
	// 没有命令时应报错而不是静默成功。
	cmd.SetArgs([]string{"--"})
	if err := cmd.Execute(); err == nil {
		t.Error("缺少命令时应报错")
	}
}

// TestExecCommandPassesFlagsAfterContainerName 回归：容器名之后的参数
// 必须原样作为容器内命令，不被解析为 licore 的 flag。
//
// 历史缺陷：exec 缺少 SetInterspersed(false)，于是
//
//	licore exec captest /bin/sh -c "echo hi"
//
// 报 `unknown shorthand flag: 'c' in -c`，用户被迫加 `--`。
// run 命令一直有这一行，exec 从来没有——所以这条用例也钉住"两个命令行为一致"。
func TestExecCommandPassesFlagsAfterContainerName(t *testing.T) {
	cmd := newExecCommand(&strings.Builder{})
	// 容器名（这里必然不存在）之后的 -c 不该被 cobra 吃掉；
	// 若被吃掉，错误会是 "unknown shorthand flag"，而不是"容器不存在"。
	cmd.SetArgs([]string{"no-such-container", "/bin/sh", "-c", "echo hi"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("容器不存在时应报错")
	}
	if strings.Contains(err.Error(), "unknown shorthand flag") {
		t.Errorf("容器名之后的 -c 被当成 licore 的 flag 解析了: %v", err)
	}
	if !strings.Contains(err.Error(), "容器不存在") {
		t.Errorf("期望解析通过、进到查找容器，实际错误: %v", err)
	}
}

// TestExecCommandFlagsBeforeContainerNameStillWork 确认修复没有把
// exec 自己的 flag 也一起禁掉（flag 必须写在容器名**之前**）。
func TestExecCommandFlagsBeforeContainerNameStillWork(t *testing.T) {
	cmd := newExecCommand(&strings.Builder{})
	cmd.SetArgs([]string{"-i", "no-such-container", "/bin/sh"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("容器不存在时应报错")
	}
	if strings.Contains(err.Error(), "unknown shorthand flag") {
		t.Errorf("容器名之前的 -i 应被正常识别: %v", err)
	}
	if !strings.Contains(err.Error(), "容器不存在") {
		t.Errorf("-i 应被识别后进到查找容器: %v", err)
	}
}

// TestExecCommandDoubleDashStillWorks 确认 `--` 写法保持兼容（不因修复而回归）。
func TestExecCommandDoubleDashStillWorks(t *testing.T) {
	cmd := newExecCommand(&strings.Builder{})
	cmd.SetArgs([]string{"no-such-container", "--", "/bin/sh", "-c", "echo hi"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("容器不存在时应报错")
	}
	if strings.Contains(err.Error(), "unknown shorthand flag") {
		t.Errorf("`--` 写法应被正确处理: %v", err)
	}
}
