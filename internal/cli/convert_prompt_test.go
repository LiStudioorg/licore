// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// -------- planConvertQuestions（纯逻辑） --------

// TestPlanConvertQuestions 覆盖"何时问哪些问题 / 何时用默认 / 何时报错"的全部组合。
func TestPlanConvertQuestions(t *testing.T) {
	tests := []struct {
		name      string
		in        convertPromptInput
		wantInter bool
		wantDefs  bool
		wantQs    []convertQuestion
	}{
		{
			name:      "TTY 无任何 flag → 问架构+精简+导入",
			in:        convertPromptInput{tty: true},
			wantInter: true,
			wantQs:    []convertQuestion{qArch, qSlim, qImport},
		},
		{
			name:      "非 TTY → 不问且不用默认（旧行为，缺输出报错）",
			in:        convertPromptInput{tty: false},
			wantInter: false,
			wantDefs:  false,
		},
		{
			name:      "--yes → 不问但用默认",
			in:        convertPromptInput{tty: true, assumeYes: true},
			wantInter: false,
			wantDefs:  true,
		},
		{
			name:      "--yes + 非 TTY → 用默认（--yes 优先）",
			in:        convertPromptInput{tty: false, assumeYes: true},
			wantInter: false,
			wantDefs:  true,
		},
		{
			name:      "已传 --arch → 跳过架构问题",
			in:        convertPromptInput{tty: true, archChanged: true},
			wantInter: true,
			wantQs:    []convertQuestion{qSlim, qImport},
		},
		{
			name:      "已传 --slim → 跳过精简问题",
			in:        convertPromptInput{tty: true, slimChanged: true},
			wantInter: true,
			wantQs:    []convertQuestion{qArch, qImport},
		},
		{
			name:      "已传 --import → 跳过导入问题",
			in:        convertPromptInput{tty: true, importChanged: true},
			wantInter: true,
			wantQs:    []convertQuestion{qArch, qSlim},
		},
		{
			name:      "已传 -o → 跳过导入问题（输出方式已定）",
			in:        convertPromptInput{tty: true, outputChanged: true},
			wantInter: true,
			wantQs:    []convertQuestion{qArch, qSlim},
		},
		{
			name:      "全部 flag 传齐 → 不问（交互计划无问题）",
			in:        convertPromptInput{tty: true, archChanged: true, slimChanged: true, importChanged: true},
			wantInter: true,
			wantQs:    nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planConvertQuestions(tt.in)
			if got.Interactive != tt.wantInter {
				t.Fatalf("Interactive = %v，期望 %v", got.Interactive, tt.wantInter)
			}
			if got.ApplyDefaults != tt.wantDefs {
				t.Fatalf("ApplyDefaults = %v，期望 %v", got.ApplyDefaults, tt.wantDefs)
			}
			if len(got.Questions) != len(tt.wantQs) {
				t.Fatalf("Questions = %v，期望 %v", got.Questions, tt.wantQs)
			}
			for i := range tt.wantQs {
				if got.Questions[i] != tt.wantQs[i] {
					t.Errorf("Questions[%d] = %v，期望 %v", i, got.Questions[i], tt.wantQs[i])
				}
			}
		})
	}
}

// -------- 默认值 --------
// TestDefaultConvertChoices 验证默认架构为宿主、默认导入、默认输出路径格式。
func TestDefaultConvertChoices(t *testing.T) {
	got := defaultConvertChoices(convertHint{Image: "nginx:1.27-alpine"})
	if got.Arch != runtime.GOARCH {
		t.Errorf("默认架构 = %q，期望宿主 %q", got.Arch, runtime.GOARCH)
	}
	if !got.Import {
		t.Error("默认应为导入本地")
	}
	if got.Slim {
		t.Error("默认不应精简")
	}
	want := "nginx-1.27-alpine-" + runtime.GOARCH + ".licore"
	if got.OutPath != want {
		t.Errorf("默认输出路径 = %q，期望 %q", got.OutPath, want)
	}
}

// TestComputeDefaultOutputPath 覆盖各种 docker 引用形态的默认文件名派生。
func TestComputeDefaultOutputPath(t *testing.T) {
	cases := []struct {
		image string
		arch  string
		want  string
	}{
		{"alpine:3.20", "amd64", "alpine-3.20-amd64.licore"},
		{"library/nginx:1.27", "arm64", "nginx-1.27-arm64.licore"},
		{"nginx", "amd64", "nginx-latest-amd64.licore"},
		{"registry:5000/x:1", "amd64", "x-1-amd64.licore"},
		{"foo/bar@sha256:abc", "amd64", "bar-latest-amd64.licore"},
	}
	for _, c := range cases {
		if got := computeDefaultOutputPath(c.image, c.arch); got != c.want {
			t.Errorf("computeDefaultOutputPath(%q,%q) = %q，期望 %q", c.image, c.arch, got, c.want)
		}
	}
}

// -------- isTTY --------

// TestIsTTYOnRegularFile 验证普通文件不是 TTY（非交互路径的判据）。
func TestIsTTYOnRegularFile(t *testing.T) {
	f := t.TempDir() + "/f"
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备文件: %v", err)
	}
	fh, err := os.Open(f)
	if err != nil {
		t.Fatalf("打开: %v", err)
	}
	defer func() { _ = fh.Close() }()
	if isTTY(fh) {
		t.Error("普通文件不应被判为 TTY")
	}
}

// TestIsTTYNil 验证 nil 安全返回 false。
func TestIsTTYNil(t *testing.T) {
	if isTTY(nil) {
		t.Error("nil 不应被判为 TTY")
	}
}

// -------- runConvertPrompt（I/O 层） --------

// promptCmd 构造一个可注入 stdin 的 cobra 命令。
func promptCmd(stdin string) (*cobra.Command, *bytes.Buffer) {
	c := &cobra.Command{}
	c.SetIn(strings.NewReader(stdin))
	var out bytes.Buffer
	return c, &out
}

// TestRunConvertPromptAllDefaults 验证全部回车取默认值 + 最终确认默认 Y。
func TestRunConvertPromptAllDefaults(t *testing.T) {
	// 架构回车、精简回车、导入回车（默认 yes）、确认回车（默认 Y）。
	cmd, out := promptCmd("\n\n\n\n")
	plan := planConvertQuestions(convertPromptInput{tty: true})
	hint := convertHint{Image: "nginx:1.27-alpine", SizeKnown: true, SizeBytes: 50 * 1024 * 1024, Entrypoint: "/docker-entrypoint.sh"}
	got, err := runConvertPrompt(cmd, out, plan, hint, defaultConvertChoices(hint))
	if err != nil {
		t.Fatalf("runConvertPrompt: %v", err)
	}
	if got.Arch != runtime.GOARCH || !got.Import || got.Slim {
		t.Errorf("默认结果错误: %+v", got)
	}
	// 展示块应含检测信息。
	s := out.String()
	for _, want := range []string{"源镜像 nginx:1.27-alpine", "大小 50.0 MiB", "ENTRYPOINT /docker-entrypoint.sh"} {
		if !strings.Contains(s, want) {
			t.Errorf("输出缺少 %q\n%s", want, s)
		}
	}
}

// TestRunConvertPromptNoImportAsksOutput 验证选"不导入"会追问输出路径。
func TestRunConvertPromptNoImportAsksOutput(t *testing.T) {
	// 架构回车、精简回车、导入答 n、输出路径 /tmp/x.licore、确认回车。
	cmd, out := promptCmd("\n\nn\n/tmp/x.licore\n\n")
	plan := planConvertQuestions(convertPromptInput{tty: true})
	hint := convertHint{Image: "alpine:3.20"}
	got, err := runConvertPrompt(cmd, out, plan, hint, defaultConvertChoices(hint))
	if err != nil {
		t.Fatalf("runConvertPrompt: %v", err)
	}
	if got.Import {
		t.Error("用户选不导入，Import 应为 false")
	}
	if got.OutPath != "/tmp/x.licore" {
		t.Errorf("输出路径 = %q，期望 /tmp/x.licore", got.OutPath)
	}
	if !strings.Contains(out.String(), "输出路径") {
		t.Error("应在选不导入后追问输出路径")
	}
}

// TestRunConvertPromptSkipQuestions 验证已传 flag 对应问题被跳过：
// 只问架构（已传 --slim --import），答案里只应有一条架构输入 + 确认。
func TestRunConvertPromptSkipQuestions(t *testing.T) {
	// 仅架构（答 arm64）+ 确认（回车）。
	cmd, out := promptCmd("arm64\n\n")
	plan := planConvertQuestions(convertPromptInput{tty: true, slimChanged: true, importChanged: true})
	hint := convertHint{Image: "alpine:3.20"}
	got, err := runConvertPrompt(cmd, out, plan, hint, defaultConvertChoices(hint))
	if err != nil {
		t.Fatalf("runConvertPrompt: %v", err)
	}
	if got.Arch != "arm64" {
		t.Errorf("架构 = %q，期望 arm64", got.Arch)
	}
	s := out.String()
	if strings.Contains(s, "是否精简") || strings.Contains(s, "是否立即导入") {
		t.Errorf("已传 flag 的问题不应被问\n%s", s)
	}
}

// TestRunConvertPromptCancel 验证最终确认答 n 返回 errPromptCancelled。
func TestRunConvertPromptCancel(t *testing.T) {
	cmd, out := promptCmd("\n\n\nn\n")
	plan := planConvertQuestions(convertPromptInput{tty: true})
	hint := convertHint{Image: "alpine:3.20"}
	_, err := runConvertPrompt(cmd, out, plan, hint, defaultConvertChoices(hint))
	if !errors.Is(err, errPromptCancelled) {
		t.Fatalf("期望 errPromptCancelled，得到 %v", err)
	}
}

// TestRunConvertPromptEOFCancels 验证确认处 EOF 视为取消（非交互兜底）。
func TestRunConvertPromptEOFCancels(t *testing.T) {
	// 架构/精简/导入都给默认，但确认前输入流结束。
	cmd, out := promptCmd("\n\n\n")
	plan := planConvertQuestions(convertPromptInput{tty: true})
	hint := convertHint{Image: "alpine:3.20"}
	_, err := runConvertPrompt(cmd, out, plan, hint, defaultConvertChoices(hint))
	if !errors.Is(err, errPromptCancelled) {
		t.Fatalf("确认处 EOF 应视为取消，得到 %v", err)
	}
}
