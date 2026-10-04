// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/licore/internal/store"
)

// wireImageCommands 把 imageops 的各产物命令挂在父命令下。
// 返回 [tag, commit, save, load, export, import] 供 root 注册。
func buildImageCommands(out io.Writer) []*cobra.Command {
	return []*cobra.Command{
		newTagCommand(out),
		newCommitCommand(out),
		newSaveCommand(out),
		newLoadCommand(out),
		newExportCommand(out),
		newImportCommand(out),
	}
}

// newTagCommand 实现 `licore tag SRC[:VER] DST[:VER]`：给已落地镜像打新标签。
func newTagCommand(out io.Writer) *cobra.Command {
	var root string
	var force bool
	cmd := &cobra.Command{
		Use:   "tag SRC[:VERSION] DST[:VERSION]",
		Short: "给本地镜像打标签（复制引用）",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(root)
			if err != nil {
				return err
			}
			if err := Retag(st, args[0], args[1], force); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s\n", args[1])
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "data-dir", "", "数据目录")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "目标已存在时覆盖")
	return cmd
}

// newCommitCommand 实现 `licore commit CONTAINER NAME[:VERSION]`。
func newCommitCommand(out io.Writer) *cobra.Command {
	var root, message string
	cmd := &cobra.Command{
		Use:   "commit CONTAINER NAME[:VERSION]",
		Short: "把容器的可写层提交为新镜像",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(root)
			if err != nil {
				return err
			}
			cfg, err := st.FindContainer(args[0])
			if err != nil {
				return err
			}
			res, err := CommitRootfs(st, cfg, args[1], &CommitOptions{Message: message})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "%s\n", res.Ref)
			if res.Path != "" {
				fmt.Fprintf(out, "已写入 %s\n", res.Path)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "data-dir", "", "数据目录")
	cmd.Flags().StringVarP(&message, "message", "m", "", "提交说明")
	return cmd
}

// newSaveCommand 实现 `licore save IMAGE [OUT]` / `licore save IMAGE -o OUT`。
//
// 输出路径两种写法都支持：位置参数（`save alpine:3.20 out.licore`，与
// `licore export` 对称）和 -o/--output（脚本里更显式）。两者同时给且不一致
// 时报错，避免"到底写去了哪"靠猜。
func newSaveCommand(out io.Writer) *cobra.Command {
	var root, dst string
	cmd := &cobra.Command{
		Use:   "save IMAGE [OUT]",
		Short: "把本地镜像保存为 .licore 文件",
		Long: "把本地镜像保存为 .licore 文件。\n" +
			"输出路径可以写成位置参数，也可以用 -o/--output；两者同时给出时必须一致。",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveOutputArg(args, dst, "save")
			if err != nil {
				return err
			}
			st, err := store.Open(root)
			if err != nil {
				return err
			}
			if err := SaveImage(st, args[0], target, true); err != nil {
				return err
			}
			fmt.Fprintln(out, target)
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "data-dir", "", "数据目录")
	cmd.Flags().StringVarP(&dst, "output", "o", "", "输出 .licore 文件（也可写成位置参数）")
	return cmd
}

// resolveOutputArg 统一"位置参数 vs -o/--output"两种输出路径写法。
//
//	name   命令名，用于错误信息
//	args   cobra 收到的位置参数（args[1] 若存在即为输出路径）
//	flag  -o/--output 的值
//
// 两者都给时逐个比较原始值：故意不做 filepath.Clean 归一化——写出的就是用户
// 字面给的路径，若字面不同却"清理后相同"，说明用户自己也不确定，报错更安全。
func resolveOutputArg(args []string, flag, name string) (string, error) {
	var positional string
	if len(args) > 1 {
		positional = args[1]
	}
	switch {
	case positional != "" && flag != "":
		if positional != flag {
			return "", fmt.Errorf("%s: 输出路径冲突：位置参数 %q 与 --output %q 不一致",
				name, positional, flag)
		}
		return positional, nil
	case positional != "":
		return positional, nil
	case flag != "":
		return flag, nil
	default:
		return "", fmt.Errorf("%s: 必须指定输出文件：%s IMAGE <OUT> 或 %s IMAGE -o <OUT>",
			name, name, name)
	}
}

// newLoadCommand 实现 `licore load -i file.licore`。
func newLoadCommand(out io.Writer) *cobra.Command {
	var root, src string
	cmd := &cobra.Command{
		Use:   "load",
		Short: "导入 .licore 文件为本地镜像",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := store.Open(root)
			if err != nil {
				return err
			}
			loaded, err := ImportImage(st, src, "", ImportOptions{Force: true})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "%s\n", loaded.Manifest.Ref())
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "data-dir", "", "数据目录")
	cmd.Flags().StringVarP(&src, "input", "i", "", "要导入的 .licore 文件（必填）")
	cmd.MarkFlagRequired("input")
	return cmd
}

// newExportCommand 实现 `licore export IMAGE [OUT]`（与 save 等价，方向同源）。
// 输出路径写法与 save 完全一致（位置参数或 -o），两者是同一套语法。
func newExportCommand(out io.Writer) *cobra.Command {
	var root, dst string
	cmd := &cobra.Command{
		Use:   "export IMAGE [OUT]",
		Short: "导出本地镜像为 .licore 文件",
		Long: "导出本地镜像为 .licore 文件（与 save 等价）。\n" +
			"输出路径可以写成位置参数，也可以用 -o/--output；两者同时给出时必须一致。",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveOutputArg(args, dst, "export")
			if err != nil {
				return err
			}
			st, err := store.Open(root)
			if err != nil {
				return err
			}
			if err := ExportImage(st, args[0], target, true); err != nil {
				return err
			}
			fmt.Fprintln(out, target)
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "data-dir", "", "数据目录")
	cmd.Flags().StringVarP(&dst, "output", "o", "", "导出文件（也可写成位置参数）")
	return cmd
}

// newImportCommand 实现 `licore import file.licore`。
func newImportCommand(out io.Writer) *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "import FILE",
		Short: "导入 .licore 文件为本地镜像",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := os.Stat(args[0]); err != nil {
				return fmt.Errorf("import: 打开 %s: %w", args[0], err)
			}
			st, err := store.Open(root)
			if err != nil {
				return err
			}
			loaded, err := ImportImage(st, args[0], "", ImportOptions{Force: true})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "%s\n", loaded.Manifest.Ref())
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "data-dir", "", "数据目录")
	return cmd
}
