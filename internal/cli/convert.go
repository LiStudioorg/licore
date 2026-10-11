// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/licore/internal/convert"
	"github.com/LiStudioorg/licore/internal/image"
	"github.com/LiStudioorg/licore/internal/store"
)

// newConvertCommand 实现 `licore convert`：把 Docker 镜像转成 .licore。
//
// 两种模式：
//   - 单个：licore convert <docker-image> [-o out.licore | --import]
//   - 批量：licore convert --from-file images.txt --output-dir ./dist/
func newConvertCommand(out io.Writer) *cobra.Command {
	var (
		output    string
		importIt  bool
		arch      string
		slim      bool
		assumeYes bool
		noCleanup bool
		keepImage bool
		dataDir   string
		tag       string
		fromFile  string
		outputDir string
		jobs      int
	)
	cmd := &cobra.Command{
		Use:   "convert [docker-image]",
		Short: "把 Docker 镜像转换为 .licore 镜像",
		Long: "把 Docker 镜像转换为 LiCore 的 .licore 镜像。\n" +
			"转换通过 docker export 导出完整 rootfs，再从 docker inspect 重建\n" +
			"ENTRYPOINT/CMD/ENV/WORKDIR/EXPOSE/VOLUME/LABEL/USER，因此产物是完整的\n" +
			"rootfs（不含 Docker 的存储驱动层），不需要手工制作基础镜像。\n\n" +
			"单个转换：\n" +
			"  licore convert alpine:3.20 -o alpine-3.20.licore\n" +
			"  licore convert nginx:1.27-alpine --import\n\n" +
			"无相关 flag 且 stdin 是终端时会逐项询问（架构 / 精简 / 是否导入 / 输出路径）；\n" +
			"已传的 flag 对应问题会跳过。--yes/-y 取默认值不提问；管道 / 重定向等\n" +
			"非终端输入不提问（缺 -o/--import 时仍报错，保持脚本兼容）。\n\n" +
			"批量转换（images.txt 每行一个镜像，支持 # 注释）：\n" +
			"  licore convert --from-file images.txt --output-dir ./dist/\n\n" +
			"需要本机可用 docker 且当前用户有权访问 docker socket。",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 批量模式与单个模式的参数互斥，先做一致性校验。
			if fromFile != "" {
				if len(args) > 0 {
					return fmt.Errorf("convert: --from-file 与位置参数镜像名不能同时使用")
				}
				if output != "" {
					return fmt.Errorf("convert: 批量模式请用 --output-dir 指定输出目录")
				}
				if importIt {
					return fmt.Errorf("convert: 批量模式不支持 --import（请用 --output-dir）")
				}
				return runBatchConvert(cmd, out, batchOptions{
					fromFile:  fromFile,
					outputDir: outputDir,
					arch:      arch,
					jobs:      jobs,
					noCleanup: noCleanup,
					keepImage: keepImage,
					dataDir:   dataDir,
				})
			}
			if len(args) == 0 {
				return fmt.Errorf("convert: 必须指定 docker 镜像名，或用 --from-file 批量转换")
			}
			if outputDir != "" {
				return fmt.Errorf("convert: --output-dir 只用于批量模式（配合 --from-file）")
			}

			// 交互式询问：仅单个镜像时按 TTY / --yes 决定交互方式（见 planConvertQuestions）。
			// 已显式传入的 flag 对应问题会跳过；--yes 用默认值；纯非 TTY（脚本）
			// 保持旧行为（缺输出方式报错），不会让旧脚本静默写入 store。
			plan := planConvertQuestions(convertPromptInput{
				tty:           isTTY(os.Stdin),
				assumeYes:     assumeYes,
				archChanged:   cmd.Flags().Changed("arch"),
				slimChanged:   cmd.Flags().Changed("slim"),
				importChanged: cmd.Flags().Changed("import"),
				outputChanged: cmd.Flags().Changed("output"),
			})
			if plan.Interactive {
				hint := convertHintFromDocker(cmd, args[0])
				// 默认值需合并已传的 flag：跳过的架构问题不会写回，若不给默认值，
				// 确认行会显示宿主架构而非 --arch 的值（实测显示与结果不一致）。
				base := defaultConvertChoices(hint)
				if arch != "" {
					base.Arch = arch
				}
				if slim {
					base.Slim = true
				}
				if importIt {
					base.Import = true
				}
				if output != "" {
					base.Import = false
					base.OutPath = output
				}
				choices, err := runConvertPrompt(cmd, out, plan, hint, base)
				if err != nil {
					if errors.Is(err, errPromptCancelled) {
						fmt.Fprintln(out, "已取消。")
						return nil
					}
					return err
				}
				// 用户显式输入的答案回填到对应变量（未问到的保持原 flag 值）。
				for _, q := range plan.Questions {
					switch q {
					case qArch:
						arch = choices.Arch
					case qSlim:
						slim = choices.Slim
					case qImport:
						if choices.Import {
							importIt = true
						} else {
							output = choices.OutPath
						}
					}
				}
			} else if plan.ApplyDefaults {
				// --yes：未被 flag 指定的选项取默认值（默认导入本地）。
				base := defaultConvertChoices(convertHint{Image: args[0]})
				if !cmd.Flags().Changed("arch") {
					arch = base.Arch
				}
				if !cmd.Flags().Changed("slim") {
					slim = base.Slim // false
				}
				if !cmd.Flags().Changed("import") && !cmd.Flags().Changed("output") {
					importIt = base.Import // true
				}
			} else if output == "" && !importIt {
				// 纯非 TTY（脚本/管道）：与旧行为一致，必须显式给出输出方式。
				return fmt.Errorf("convert: 请用 -o/--output 指定输出文件，或用 --import 直接导入本地" +
					"（交互式终端下可省略，直接回车走默认；非交互请加 --yes）")
			}

			if output != "" && importIt {
				return fmt.Errorf("convert: -o/--output 与 --import 不能同时使用（--import 不产出文件）")
			}
			if arch != "" && !image.ValidArch(arch) {
				return fmt.Errorf("convert: --arch %q 不受支持，可选值：%s",
					arch, strings.Join(image.SupportedArches(), " / "))
			}

			name, version := "", ""
			if tag != "" {
				name, version = splitImageRef(tag)
				if name == "" || version == "" {
					return fmt.Errorf("convert: --tag 需要 NAME:VERSION 形式，得到 %q", tag)
				}
			}

			// --slim 目前是明确的 no-op（与 build 的 --slim 一致，见 docs/audit-v0.5.0.md）。
			// 诚实告知用户"当前不生效"，避免以为已精简。
			if slim {
				fmt.Fprintln(out, "提示：--slim 当前不生效（转换产物与常规转换一致），已接受该参数。")
			}

			res, err := convert.Convert(cmd.Context(), &convert.Options{
				Image:           args[0],
				Arch:            arch,
				OutPath:         output,
				ImportImage:     importIt,
				Name:            name,
				Version:         version,
				DataDir:         dataDir,
				NoCleanup:       noCleanup,
				KeepDockerImage: keepImage,
				Stderr:          cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			fmt.Fprintf(out, "已转换 %s（%s，%s）\n", res.Ref, res.Arch, humanSize(res.Bytes))
			if !importIt {
				fmt.Fprintf(out, "输出：%s\n", res.Path)
			}

			if importIt {
				// Convert 在 --import 模式下把产物放在 work 之外的临时文件里
				// 交给调用方导入，清理也由调用方负责（见 convert.Result.Path）。
				defer func() { _ = os.RemoveAll(filepath.Dir(res.Path)) }()
				st, err := store.Open(dataDir)
				if err != nil {
					return err
				}
				loaded, err := ImportImage(st, res.Path, res.Ref, ImportOptions{
					Force:             true,
					AllowArchMismatch: arch != "",
				})
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "已导入本地：%s\n", loaded.Manifest.Ref())
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "输出 .licore 文件路径")
	cmd.Flags().BoolVar(&importIt, "import", false, "转换后直接导入本地（不输出文件）")
	cmd.Flags().StringVar(&arch, "arch", "", "目标架构（默认宿主 GOARCH）")
	cmd.Flags().BoolVar(&slim, "slim", false, "构建精简镜像（当前不生效，与常规转换一致）")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "非交互：所有询问项取默认值，不提问")
	cmd.Flags().BoolVar(&noCleanup, "no-cleanup", false, "保留临时容器与导出 tar（调试用）")
	cmd.Flags().BoolVar(&keepImage, "keep-docker-image", false, "转换后不删除 docker 镜像")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "数据目录（默认 $LICORE_HOME 或 ~/.licore）")
	cmd.Flags().StringVar(&tag, "tag", "", "覆盖产物镜像引用 NAME:VERSION（默认由 docker 镜像名派生）")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "批量转换：镜像清单文件（每行一个，支持 # 注释）")
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "批量转换：输出目录（配合 --from-file）")
	cmd.Flags().IntVar(&jobs, "jobs", 1, "批量转换：并发数（默认 1；docker 自身会串行化部分操作，调大不总是更快）")
	return cmd
}

// humanSize 把字节数格式化为便于阅读的形式（与 build 侧的展示风格一致）。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// batchOutputPath 计算批量模式下单个镜像的输出文件名。
//
//	alpine:3.20          arch=amd64 → dist/alpine-amd64.licore
//	library/nginx:1.27   arch=arm64 → dist/nginx-arm64.licore
//	registry:5000/x:1    arch=amd64 → dist/x-amd64.licore
//
// 只取仓库名的最后一段并替换掉不适合做文件名的字符，避免 `library/` 之类
// 在 dist/ 下建出多余的目录层级。
//
// 注意：文件名按约定**不含 tag**（同一仓库的不同版本会映射到同一个名字）。
// 因此批量模式必须先做重名检查——见 checkOutputCollisions。
func batchOutputPath(dir, image, arch string) string {
	name := image
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	// 去掉 tag（最后一个 ':' 且其后无 '/'）。
	if i := strings.LastIndexByte(name, ':'); i >= 0 && !strings.Contains(name[i:], "/") {
		name = name[:i]
	}
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	base := sanitizeFileBase(name + "-" + arch)
	return filepath.Join(dir, base+".licore")
}

// checkOutputCollisions 在开始转换前检查产物重名。
//
// 为什么必须前置检查：产物文件名是 `<repo>-<arch>`，不含 tag。若清单里同时
// 有 nginx:1.26 与 nginx:1.27，两者会映射到同一个 dist/nginx-amd64.licore，
// 后转换的会**静默覆盖**前一个——用户拿到一个文件却以为是两个，属于静默数据
// 丢失。宁可一开始就报错让人改清单，也不要转完一半才发现只有一个产物。
func checkOutputCollisions(items []batchItem, dir, arch string) error {
	seen := make(map[string]string, len(items)) // 输出路径 → 首个镜像
	for _, it := range items {
		p := batchOutputPath(dir, it.image, arch)
		if prev, ok := seen[p]; ok {
			return fmt.Errorf("convert: 清单里 %q 与 %q 会产出同一个文件 %s；"+
				"批量产物名不含 tag，请拆成两次运行，或先用 --output-dir 分开",
				prev, it.image, p)
		}
		seen[p] = it.image
	}
	return nil
}

// sanitizeFileBase 把字符串里不适合出现在文件名中的字符替换为 '-'。
func sanitizeFileBase(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
