// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"strings"
	"testing"

	"github.com/LiStudioorg/licore/internal/runtime"
)

// TestParseExecArgsUserNaturalForms 覆盖用户**自然的写法**。
//
// 这是本文件最重要的用例：真机上曾出现
//
//	licore exec 容器 -w /tmp /bin/sh -c pwd   →  -w 被吞进容器命令
//
// 因为当时用 cobra 的 SetInterspersed(false)，遇到容器名就停止解析选项。
// 现在改为手工扫描，容器名之后的 exec 选项仍被识别，直到第一个「不是选项」
// 的参数为止。下表就是这条规则的完整契约。
func TestParseExecArgsUserNaturalForms(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want execArgs
	}{
		{
			name: "选项在容器名之后（用户自然写法）",
			argv: []string{"myapp", "-w", "/tmp", "/bin/sh", "-c", "pwd"},
			want: execArgs{Container: "myapp", Cmd: []string{"/bin/sh", "-c", "pwd"}, Workdir: "/tmp"},
		},
		{
			name: "选项在容器名之前",
			argv: []string{"-w", "/tmp", "myapp", "/bin/sh"},
			want: execArgs{Container: "myapp", Cmd: []string{"/bin/sh"}, Workdir: "/tmp"},
		},
		{
			name: "容器命令的 -c 原样透传（P2 的回归）",
			argv: []string{"myapp", "/bin/sh", "-c", "echo hi"},
			want: execArgs{Container: "myapp", Cmd: []string{"/bin/sh", "-c", "echo hi"}},
		},
		{
			name: "选项与命令混排",
			argv: []string{"-i", "myapp", "-w", "/app", "/bin/sh", "-c", "pwd"},
			want: execArgs{Container: "myapp", Cmd: []string{"/bin/sh", "-c", "pwd"},
				Workdir: "/app", Interactive: true},
		},
		{
			name: "合并短布尔选项 -it",
			argv: []string{"-it", "myapp", "/bin/sh"},
			want: execArgs{Container: "myapp", Cmd: []string{"/bin/sh"}, Interactive: true, TTY: true},
		},
		{
			name: "合并短布尔选项写在容器名之后",
			argv: []string{"myapp", "-it", "/bin/sh"},
			want: execArgs{Container: "myapp", Cmd: []string{"/bin/sh"}, Interactive: true, TTY: true},
		},
		{
			name: "-- 分隔符",
			argv: []string{"myapp", "--", "/bin/sh", "-c", "pwd"},
			want: execArgs{Container: "myapp", Cmd: []string{"/bin/sh", "-c", "pwd"}},
		},
		{
			name: "等号形式 --workdir=/app",
			argv: []string{"myapp", "--workdir=/app", "/bin/sh"},
			want: execArgs{Container: "myapp", Cmd: []string{"/bin/sh"}, Workdir: "/app"},
		},
		{
			name: "可重复的 -e",
			argv: []string{"-e", "A=1", "-e", "B=2", "myapp", "/bin/sh"},
			want: execArgs{Container: "myapp", Cmd: []string{"/bin/sh"}, Env: []string{"A=1", "B=2"}},
		},
		{
			name: "可重复的 --cap-drop",
			argv: []string{"myapp", "--cap-drop", "SYS_ADMIN", "--cap-drop", "NET_RAW", "/bin/sh"},
			want: execArgs{Container: "myapp", Cmd: []string{"/bin/sh"},
				CapDrop: []string{"SYS_ADMIN", "NET_RAW"}},
		},
		{
			name: "容器名之后再次出现横杠参数属于容器命令",
			argv: []string{"myapp", "/bin/ls", "-la"},
			want: execArgs{Container: "myapp", Cmd: []string{"/bin/ls", "-la"}},
		},
		{
			name: "列出 helper 所在目录是允许的",
			argv: []string{"myapp", "/bin/busybox", "ls", "/.licore/"},
			want: execArgs{Container: "myapp", Cmd: []string{"/bin/busybox", "ls", "/.licore/"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseExecArgs(tc.argv)
			if err != nil {
				t.Fatalf("parseExecArgs(%v): %v", tc.argv, err)
			}
			if got.Container != tc.want.Container {
				t.Errorf("Container = %q, want %q", got.Container, tc.want.Container)
			}
			if strings.Join(got.Cmd, " ") != strings.Join(tc.want.Cmd, " ") {
				t.Errorf("Cmd = %v, want %v", got.Cmd, tc.want.Cmd)
			}
			if got.Workdir != tc.want.Workdir {
				t.Errorf("Workdir = %q, want %q", got.Workdir, tc.want.Workdir)
			}
			if got.Interactive != tc.want.Interactive {
				t.Errorf("Interactive = %v, want %v", got.Interactive, tc.want.Interactive)
			}
			if got.TTY != tc.want.TTY {
				t.Errorf("TTY = %v, want %v", got.TTY, tc.want.TTY)
			}
			if strings.Join(got.Env, ",") != strings.Join(tc.want.Env, ",") {
				t.Errorf("Env = %v, want %v", got.Env, tc.want.Env)
			}
			if strings.Join(got.CapDrop, ",") != strings.Join(tc.want.CapDrop, ",") {
				t.Errorf("CapDrop = %v, want %v", got.CapDrop, tc.want.CapDrop)
			}
		})
	}
}

// TestParseExecArgsRejectsDirectHelper 验证直接调用 helper 被拒绝。
//
// helper 是内部实现（容器启动时只读 bind 进去、供 exec 做收口），用户直接调
// 没有意义：它会先收口再 execve "目标命令"——而目标命令就是它自己。
func TestParseExecArgsRejectsDirectHelper(t *testing.T) {
	_, err := parseExecArgs([]string{"myapp", runtime.HelperPathInContainer, "--version"})
	if err == nil {
		t.Fatal("直接调用 helper 应被拒绝")
	}
	for _, want := range []string{runtime.HelperPathInContainer, "内部实现路径", "licore exec"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q: %v", want, err)
		}
	}
}

// TestParseExecArgsAllowsHelperDirAccess 验证**只做精确匹配**：
// /.licore/ 目录本身不危险，列目录、看属性都应放行。
func TestParseExecArgsAllowsHelperDirAccess(t *testing.T) {
	for _, argv := range [][]string{
		{"myapp", "/bin/busybox", "ls", "-la", "/.licore/"},
		{"myapp", "/bin/ls", "/.licore"},
		{"myapp", "/.licore/other-thing"},
		{"myapp", "/bin/sh", "-c", "ls /.licore/"},
	} {
		if _, err := parseExecArgs(argv); err != nil {
			t.Errorf("%v 应被允许（只精确拒绝 helper 本身）: %v", argv, err)
		}
	}
}

// TestParseExecArgsRejectsBadInput 验证缺参/未知选项明确报错。
func TestParseExecArgsRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"空", nil, "缺少容器名"},
		{"只有容器名", []string{"myapp"}, "缺少要执行的命令"},
		{"未知选项在容器名前", []string{"--bogus", "myapp", "/bin/sh"}, "未知选项"},
		{"选项缺取值", []string{"myapp", "-w"}, "缺少取值"},
		{"-- 之前没有容器名", []string{"--", "/bin/sh"}, "缺少容器名"},
		{"-- 之后没有命令", []string{"myapp", "--"}, "缺少要执行的命令"},
		// `-w myapp`：myapp 被当成 -w 的取值，于是没有容器名。
		// 报"缺少容器名"而非"缺少命令"——用户真正缺的是容器名。
		{"取值型选项吃掉了容器名", []string{"-w", "myapp"}, "缺少容器名"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseExecArgs(tc.argv)
			if err == nil {
				t.Fatalf("argv=%v 应报错", tc.argv)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("argv=%v 的错误应包含 %q，得到: %v", tc.argv, tc.want, err)
			}
		})
	}
}

// TestIsCombinedShortBools 验证合并短选项的识别边界。
func TestIsCombinedShortBools(t *testing.T) {
	for _, s := range []string{"-it", "-ti", "-ii"} {
		if !isCombinedShortBools(s) {
			t.Errorf("%q 应被识别为布尔短选项组合", s)
		}
	}
	for _, s := range []string{
		"-i", "-t", // 单个，不算组合
		"-w",   // 需要取值，不能合并
		"-iw",  // 混入取值型
		"--it", // 长选项
		"-x",   // 未知
		"-itw", // 混入取值型
	} {
		if isCombinedShortBools(s) {
			t.Errorf("%q 不应被识别为布尔短选项组合", s)
		}
	}
}

// TestParseExecArgsRecognizesHelp 验证 --help / -h 被识别。
//
// exec 用 DisableFlagParsing 自己解析参数，cobra 不再处理 --help；
// 不显式识别就会把 `licore exec --help` 报成未知选项（曾经如此）。
func TestParseExecArgsRecognizesHelp(t *testing.T) {
	for _, a := range []string{"-h", "--help"} {
		got, err := parseExecArgs([]string{a})
		if err != nil {
			t.Errorf("%s 应被识别为帮助请求，却报错: %v", a, err)
			continue
		}
		if !got.wantHelp {
			t.Errorf("%s 应设置 wantHelp", a)
		}
	}
	// 不带 --help 时不应置位。
	got, err := parseExecArgs([]string{"myapp", "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	if got.wantHelp {
		t.Error("普通调用不应设置 wantHelp")
	}
}
