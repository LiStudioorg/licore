// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/licore/internal/convert"
	"github.com/LiStudioorg/licore/internal/image"
)

// 交互式转换（`licore convert` 无相关 flag 时逐项询问）的实现。
//
// 设计要点：
//   - **触发条件**：stdin 是 TTY 且未传 --yes/-y 且对应问题所需的 flag 未显式传入。
//     已传的 flag → 对应问题跳过；非 TTY 或 --yes → 一律用默认值，不询问。
//   - **不问的一律与旧行为一致**：不改变任何既有 flag 的语义，脚本调用（无 TTY）
//     永远走默认值路径，不会因为引入交互而卡住或改变产物。
//   - **纯逻辑与 I/O 分离**：planConvertQuestions 是纯函数（给定 flag 状态与检测
//     信息返回"要问哪些问题"），便于无 TTY 环境下用单测覆盖全部条件组合；
//     runConvertPrompt 只负责打印与读输入。

// convertHint 是询问前展示的检测信息（尽力而为，拿不到就留空/显示"未知"）。
type convertHint struct {
	Image      string // docker 镜像引用
	SizeBytes  int64  // 镜像大小（0 表示未知）
	SizeKnown  bool   // SizeBytes 是否有效
	Entrypoint string // 镜像 ENTRYPOINT（空表示无 / 未知）
}

// convertChoices 是一次转换最终确定的选项（交互结果或默认值）。
type convertChoices struct {
	Arch    string // 目标架构（空 → 宿主 GOARCH）
	Slim    bool   // --slim（当前为 no-op，见 newConvertCommand 说明）
	Import  bool   // --import
	OutPath string // -o/--output（与 Import 互斥）
}

// convertQuestion 标识一个可询问的问题。
type convertQuestion int

const (
	qArch   convertQuestion = iota // 目标架构
	qSlim                          // 是否精简
	qImport                        // 是否导入本地（答"否"时在 I/O 层追问输出路径）
)

// convertPromptInput 描述"决定是否需要交互"的全部输入。
type convertPromptInput struct {
	// tty 表示 stdin 是交互终端。
	tty bool
	// assumeYes 表示用户传了 --yes/-y。
	assumeYes bool
	// archChanged / slimChanged / importChanged / outputChanged 表示对应 flag
	// 是否被显式传入（用 cmd.Flags().Changed 取得，比"值非零"更准确）。
	archChanged   bool
	slimChanged   bool
	importChanged bool
	outputChanged bool
}

// convertPromptPlan 是要执行的交互计划：需要问哪些问题，以及不问时如何取默认。
type convertPromptPlan struct {
	// Interactive 为 true 时逐项询问。
	Interactive bool
	// Questions 是待问的问题序列（按顺序）。
	Questions []convertQuestion
	// ApplyDefaults 为 true 时，未被 flag 指定的选项一律取默认值
	// （即"--yes / 非交互但用默认"路径）；为 false 时保持旧行为：
	// 既没 -o 也没 --import 就报错（纯脚本/管道路径）。
	ApplyDefaults bool
}

// planConvertQuestions 是**纯函数**：根据 flag 状态、TTY 与 --yes 决定交互方式。
//
// 三条路径，语义明确区分：
//   - **交互**（TTY 且未 --yes）：逐项询问，已传 flag 的问题跳过。
//   - **--yes**（显式非交互）：不问，未被 flag 指定的选项取默认值（默认导入本地），
//     可直接完成一次转换。
//   - **纯非 TTY**（脚本/管道，未 --yes）：不问，且**保持旧行为**——既没 -o 也没
//     --import 时报错。这样不会让忘记给输出方式的旧脚本静默写入用户 store。
func planConvertQuestions(in convertPromptInput) convertPromptPlan {
	if in.assumeYes {
		return convertPromptPlan{Interactive: false, ApplyDefaults: true}
	}
	if !in.tty {
		return convertPromptPlan{Interactive: false, ApplyDefaults: false}
	}
	var qs []convertQuestion
	if !in.archChanged {
		qs = append(qs, qArch)
	}
	if !in.slimChanged {
		qs = append(qs, qSlim)
	}
	if !in.importChanged && !in.outputChanged {
		qs = append(qs, qImport)
		// qOutput 视 qImport 的答案在 I/O 层追加。
	}
	return convertPromptPlan{Interactive: true, Questions: qs}
}

// defaultConvertChoices 计算各问题的默认值（回车即取该值）。
func defaultConvertChoices(hint convertHint) convertChoices {
	arch := runtime.GOARCH
	return convertChoices{
		Arch:    arch,
		Slim:    false,
		Import:  true,
		OutPath: computeDefaultOutputPath(hint.Image, arch),
	}
}

// deriveRefForDefault 从 docker 引用派生默认输出文件名用的 name/version。
// 与 convert 包的 deriveRef 语义一致，但此处只是给默认路径用，保持独立避免
// 把内部实现暴露到 CLI 层。derived 失败时退回 "image"/"latest"。
func deriveRefForDefault(ref string) (name, version string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "image", "latest"
	}
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndexByte(ref, ':'); i >= 0 && !strings.Contains(ref[i:], "/") {
		version = ref[i+1:]
		ref = ref[:i]
	}
	if i := strings.LastIndexByte(ref, '/'); i >= 0 {
		ref = ref[i+1:]
	}
	if ref == "" {
		ref = "image"
	}
	if version == "" {
		version = "latest"
	}
	return ref, version
}

// runConvertPrompt 展示检测信息并逐项询问，返回最终选项。
//
// hint 的检测信息尽力而为：拿不到就不显示对应行，绝不因探测失败而中断。
// defaults 给出每个问题的默认值（回车即取默认）。
func runConvertPrompt(cmd *cobra.Command, out io.Writer, plan convertPromptPlan, hint convertHint, defaults convertChoices) (convertChoices, error) {
	in := cmd.InOrStdin()
	rd := bufio.NewReader(in)
	res := defaults

	// 展示检测信息块。
	fmt.Fprintf(out, "源镜像 %s\n", hint.Image)
	if hint.SizeKnown {
		fmt.Fprintf(out, "大小 %s\n", humanSize(hint.SizeBytes))
	} else {
		fmt.Fprintf(out, "大小 未知\n")
	}
	fmt.Fprintf(out, "架构 %s\n", strings.Join(image.SupportedArches(), " / "))
	if hint.Entrypoint != "" {
		fmt.Fprintf(out, "ENTRYPOINT %s\n", hint.Entrypoint)
	}
	fmt.Fprintln(out)

	for _, q := range plan.Questions {
		switch q {
		case qArch:
			ans, err := askString(rd, out, "目标架构", defaults.Arch)
			if err != nil {
				return res, err
			}
			res.Arch = ans
		case qSlim:
			yes, err := askYesNo(rd, out, "是否精简（--slim，删 docs/man/apt cache）", false)
			if err != nil {
				return res, err
			}
			res.Slim = yes
		case qImport:
			yes, err := askYesNo(rd, out, "是否立即导入本地（--import）", true)
			if err != nil {
				return res, err
			}
			res.Import = yes
			if !yes {
				// 选了不导入 → 追问输出路径（问题 4，与问题 3 合并的分支）。
				p, err := askString(rd, out, "输出路径", defaults.OutPath)
				if err != nil {
					return res, err
				}
				res.OutPath = p
			}
		}
	}

	// 最终确认。
	mode := "导入本地"
	if !res.Import {
		mode = "输出 " + res.OutPath
	}
	extra := ""
	if res.Slim {
		extra = " + 精简"
	}
	prompt := fmt.Sprintf("\n转换 %s → %s%s，%s？\n[Y/n]: ", hint.Image, res.Arch, extra, mode)
	fmt.Fprint(out, prompt)
	line, err := rd.ReadString('\n')
	if err != nil && line == "" {
		// 非交互输入在确认处 EOF：视为拒绝（与 suggestBoot 的默认拒绝一致）。
		return res, errPromptCancelled
	}
	if s := strings.TrimSpace(line); s != "" &&
		!strings.EqualFold(s, "y") && !strings.EqualFold(s, "yes") {
		return res, errPromptCancelled
	}
	return res, nil
}

// errPromptCancelled 表示用户在最终确认处选择取消。CLI 据此静默退出（非错误）。
var errPromptCancelled = fmt.Errorf("convert: 已取消")

// askString 打印问题并读一行答案；空输入取默认值。
func askString(rd *bufio.Reader, out io.Writer, question, def string) (string, error) {
	fmt.Fprintf(out, "%s？[%s]: ", question, def)
	line, err := rd.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("读取输入失败: %w", err)
	}
	if s := strings.TrimSpace(line); s != "" {
		return s, nil
	}
	return def, nil
}

// askYesNo 打印 y/N 问题并读一行答案；空输入取默认值。
func askYesNo(rd *bufio.Reader, out io.Writer, question string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Fprintf(out, "%s？[%s]: ", question, hint)
	line, err := rd.ReadString('\n')
	if err != nil && line == "" {
		return false, fmt.Errorf("读取输入失败: %w", err)
	}
	s := strings.ToLower(strings.TrimSpace(line))
	if s == "" {
		return def, nil
	}
	return s == "y" || s == "yes", nil
}

// convertHintFromDocker 尽力探测展示信息。任何一步失败都不阻断转换——
// 探测只是"给用户看的提示"，缺失时显示"未知"即可。
//
// 注意：本实现**不探测可用架构列表**（决策：避免额外网络依赖，总是询问架构，
// 展示行改用 LiCore 支持的架构枚举）。
func convertHintFromDocker(cmd *cobra.Command, imageRef string) convertHint {
	h := convertHint{Image: imageRef}
	d := &convert.CLIDocker{}
	ctx := cmd.Context()

	// 大小：docker image inspect --format {{.Size}}
	if out, err := d.Run(ctx, "image", "inspect", "--format", "{{.Size}}", imageRef); err == nil {
		if n, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); perr == nil && n > 0 {
			h.SizeBytes = n
			h.SizeKnown = true
		}
	}
	// ENTRYPOINT：docker image inspect --format {{json .Config.Entrypoint}}
	if out, err := d.Run(ctx, "image", "inspect", "--format", "{{json .Config.Entrypoint}}", imageRef); err == nil {
		var ep []string
		if jerr := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &ep); jerr == nil && len(ep) > 0 {
			h.Entrypoint = strings.Join(ep, " ")
		}
	}
	return h
}

// computeDefaultOutputPath 派生 `<name>-<tag>-<arch>.licore`（供询问默认值与测试）。
func computeDefaultOutputPath(image, arch string) string {
	if arch == "" {
		arch = runtime.GOARCH
	}
	name, version := deriveRefForDefault(image)
	return filepath.Join(".", sanitizeFileBase(fmt.Sprintf("%s-%s-%s", name, version, arch))+".licore")
}
