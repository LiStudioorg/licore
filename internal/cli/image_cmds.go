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

// newSaveCommand 实现 `licore save IMAGE -o out.licore`。
func newSaveCommand(out io.Writer) *cobra.Command {
	var root, dst string
	cmd := &cobra.Command{
		Use:   "save IMAGE",
		Short: "把本地镜像保存为 .licore 文件",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(root)
			if err != nil {
				return err
			}
			if err := SaveImage(st, args[0], dst, true); err != nil {
				return err
			}
			fmt.Fprintln(out, dst)
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "data-dir", "", "数据目录")
	cmd.Flags().StringVarP(&dst, "output", "o", "", "输出 .licore 文件（必填）")
	cmd.MarkFlagRequired("output")
	return cmd
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

// newExportCommand 实现 `licore export IMAGE -o out.licore`（与 save 等价，方向同源）。
func newExportCommand(out io.Writer) *cobra.Command {
	var root, dst string
	cmd := &cobra.Command{
		Use:   "export IMAGE",
		Short: "导出本地镜像为 .licore 文件",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(root)
			if err != nil {
				return err
			}
			if err := ExportImage(st, args[0], dst, true); err != nil {
				return err
			}
			fmt.Fprintln(out, dst)
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "data-dir", "", "数据目录")
	cmd.Flags().StringVarP(&dst, "output", "o", "", "导出文件（必填）")
	cmd.MarkFlagRequired("output")
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
