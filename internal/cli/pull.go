// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/licore/internal/service"
	"github.com/LiStudioorg/licore/internal/store"
)

// newPullCommand 实现 `licore pull`。
//
//	licore pull ./myapp-1.0.licore     # 从本地 .licore 文件导入
//	licore pull alice/myapp:v1         # 从 Hub 拉取（需先 licore login）
func newPullCommand(out io.Writer) *cobra.Command {
	var (
		force   bool
		rootDir string
		hubFlag string
	)
	cmd := &cobra.Command{
		Use:   "pull <file.licore|NAME:VERSION>",
		Short: "导入本地 .licore 文件或从 Hub 拉取镜像",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if isHubRef(args[0]) {
				return runHubPull(out, args[0], hubFlag, rootDir)
			}
			return runPull(out, args[0], force, rootDir)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "同 name/version 已存在时覆盖（本地文件）")
	cmd.Flags().StringVar(&rootDir, "data-dir", "", "数据目录（默认 $LICORE_HOME 或 ~/.licore）")
	cmd.Flags().StringVar(&hubFlag, "hub", "", "Hub 地址（默认 $LICORE_HUB 或 http://127.0.0.1:3727）")
	return cmd
}

// isHubRef 判断参数是否为 Hub 镜像引用（name:version），而非本地文件路径。
func isHubRef(s string) bool {
	// 已存在的文件路径视为本地文件。
	if _, err := os.Stat(s); err == nil {
		return false
	}
	// 形如 <name>:<version>，且 name 不含路径分隔/反斜杠，version 非空。
	i := strings.LastIndex(s, ":")
	return i > 0 && i < len(s)-1 && !strings.ContainsAny(s, "\\/")
}

// runHubPull 从 Hub 拉取镜像落地为 .licore 文件，再走本地导入链路。
func runHubPull(out io.Writer, ref, hubFlag, rootDir string) error {
	base := hubBaseURL(hubFlag)
	c := newHubClient(base, rootDir)
	if c.Token == "" {
		return fmt.Errorf("pull: 未登录 %s，请先 licore login", base)
	}
	dst := filepath.Join(os.TempDir(), "licore-pull-"+ref+".licore")
	if err := c.Pull(ref, dst); err != nil {
		return err
	}
	defer os.Remove(dst)
	fmt.Fprintf(out, "已从 %s 拉取 %s\n", base, ref)
	return runPull(out, dst, false, rootDir)
}

func runPull(out io.Writer, srcPath string, force bool, rootDir string) error {
	st, err := store.Open(rootDir)
	if err != nil {
		return err
	}
	loaded, err := st.Put(srcPath, force, false)
	if err != nil {
		return fmt.Errorf("pull %s: %w", srcPath, err)
	}
	m := loaded.Manifest
	fmt.Fprintf(out, "已导入镜像 %s（%s/%s，%d 层，全部摘要校验通过）\n",
		m.Ref(), m.OS, m.Architecture, len(m.Layers))
	fmt.Fprintf(out, "落地目录：%s\n", st.ImageDir(m.Name, m.Version))

	suggestBoot(out, st)
	return nil
}

// suggestBoot 实现"首次使用引导"（AGENTS.md）：数据目录中没有 boot 标记文件时询问用户，
// 确认后直接执行 boot enable（失败仅提示，不影响本次操作；标记防止重复打断）。
func suggestBoot(out io.Writer, st *store.Store) {
	marker := st.BootMarker()
	if marker == "" {
		return
	}
	if _, err := os.Stat(marker); err == nil {
		return // 已启用或已询问过
	}
	fmt.Fprint(out, "\n检测到 LiCore 尚未启用开机自启\n是否启用？启用后开机会自动拉起设置了 restart=always 的容器\n[y/N]: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	ack := "declined\n"
	switch {
	case err != nil && line == "":
		// 非交互输入（EOF）：视为默认拒绝，记录标记避免反复打断脚本执行。
		fmt.Fprintln(out, "已跳过。")
	case strings.EqualFold(strings.TrimSpace(line), "y"), strings.EqualFold(strings.TrimSpace(line), "yes"):
		res, err := service.Enable(context.Background(), serviceOptions(st))
		switch {
		case err != nil:
			fmt.Fprintf(out, "启用失败：%v\n", err)
			ack = "failed\n"
		case res == nil:
			ack = "failed\n"
		default:
			fmt.Fprintf(out, "服务文件：%s\n", res.UnitPath)
			if res.Systemd {
				fmt.Fprintf(out, "已注册 systemd 开机自启。\n")
				ack = "enabled\n"
			} else {
				fmt.Fprintf(out, "提示：%s\n", res.Note)
				ack = "file-only\n"
			}
		}
	default:
		fmt.Fprintln(out, "已跳过。可随时执行 licore boot enable 启用。")
	}
	if err := st.EnsureBootDir(); err != nil {
		return
	}
	_ = os.WriteFile(marker, []byte(ack), 0o644)
}
