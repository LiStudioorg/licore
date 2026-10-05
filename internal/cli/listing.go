// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/licore/internal/runtime"
	"github.com/LiStudioorg/licore/internal/store"
)

// newExecCommand 实现 `licore exec`：setns 进入运行中容器的命名空间执行命令。
//
//	licore exec <容器ID|名字> <command...>
//	licore exec -it myapp /bin/sh
//	licore exec myapp -w /tmp /bin/sh          # exec 自己的选项写在容器名之后也可以
//
// **参数解析刻意不走 cobra**（DisableFlagParsing + parseExecArgs）：exec 的
// 语义是"容器名之后全是容器命令"，而容器命令自身可能带 -c/--foo 这类横杠开头
// 的参数。cobra 的两种模式都做不到：
//
//   - 默认交错解析：容器命令的 -c 会被当成 licore 的 flag，报 unknown shorthand flag；
//   - SetInterspersed(false)：遇到容器名就停止解析，于是
//     `exec 容器 -w /tmp cmd` 里的 -w 被当成容器命令（本仓库真机踩过）。
//
// 手工扫描的规则见 parseExecArgs；这也与隐藏命令 exec-setup 的
// DisableFlagParsing 保持一致。
func newExecCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec [flags] <容器ID|名字> <command...>",
		Short: "在运行中的容器里执行命令",
		Long: "在运行中的容器里执行命令。\n\n" +
			"容器名之后的内容一律作为容器内命令原样传入，不再解析为 licore 的选项；\n" +
			"exec 自己的选项写在容器名之前或之后都可以。\n\n" +
			"  licore exec myapp /bin/sh -c 'echo hi'     # -c 属于容器内命令\n" +
			"  licore exec myapp -w /tmp /bin/sh          # -w 属于 exec 自己\n" +
			"  licore exec -it myapp /bin/sh              # 交互式 TTY",
		DisableFlagParsing: true,
		// 参数由 parseExecArgs 自己校验，这里不做 cobra 侧校验。
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := parseExecArgs(args)
			if err != nil {
				return err
			}
			return runExec(cmd, out, opts)
		},
	}
	// 仍然注册 flag 定义，让 --help 能列出可用选项；实际解析由 parseExecArgs 负责。
	cmd.Flags().BoolP("interactive", "i", false, "保持 stdin 打开")
	cmd.Flags().BoolP("tty", "t", false, "分配伪终端")
	cmd.Flags().StringP("user", "u", "", "运行用户 uid[:gid]")
	cmd.Flags().StringP("workdir", "w", "", "工作目录（容器内路径）")
	cmd.Flags().StringArrayP("env", "e", nil, "环境变量 KEY=VALUE，可重复")
	cmd.Flags().String("data-dir", "", "数据目录（默认 $LICORE_HOME 或 ~/.licore）")
	cmd.Flags().StringSlice("cap-drop", nil,
		"在容器已有能力基础上进一步收紧（可重复）；exec **只允许收紧**")
	cmd.Flags().StringSlice("cap-add", nil,
		"（已禁用）exec 不允许放宽能力；该参数会被明确拒绝")
	return cmd
}

// runExec 执行已经解析好的 exec 请求。
func runExec(cmd *cobra.Command, out io.Writer, opts *execArgs) error {
	// --help 由 parseExecArgs 识别后在这里转成帮助输出：
	// exec 用 DisableFlagParsing，cobra 不会自己处理它。
	if opts.wantHelp {
		return cmd.Help()
	}
	// 安全边界：exec 只能进一步收紧能力，不允许放宽。
	// 否则任何能跑 licore exec 的人都能加回 CAP_SYS_ADMIN，
	// 容器 init 的收口就完全白做了。
	if err := rejectCapAddForExec(opts.CapAdd); err != nil {
		return err
	}
	if err := validateCapSpecs(opts.CapDrop, nil); err != nil {
		return err
	}
	st, err := store.Open(opts.DataDir)
	if err != nil {
		return err
	}
	cfg, err := st.FindContainer(opts.Container)
	if err != nil {
		return err
	}
	state, _, err := st.ReadRuntimeState(cfg.ID)
	if err != nil {
		return err
	}
	if !state.Running || state.InitPID <= 0 {
		return fmt.Errorf("exec: 容器 %s（%s）未在运行: %w", cfg.Name, cfg.ID, runtime.ErrNotInit)
	}
	// 校验运行身份格式（uid[:gid]）。
	if opts.User != "" {
		uid, _, _ := strings.Cut(opts.User, ":")
		if uid == "" {
			return fmt.Errorf("exec: 非法 --user %q", opts.User)
		}
	}
	// 收口规格经环境变量下发给容器内的 helper：
	// 默认继承**容器创建时**的 --cap-drop，再叠加本次 exec 的 --cap-drop。
	// 这些 LICORE_* 变量会被 helper 在 execve 用户命令前剥掉，
	// 因此不会泄漏进用户命令的环境。
	env := append(runtime.ExecSetupEnv(cfg.CapDrop, opts.CapDrop), opts.Env...)
	code, err := runtime.Exec(&runtime.ExecOptions{
		TargetPID: state.InitPID,
		Cmd:       opts.Cmd,
		Env:       env,
		Workdir:   opts.Workdir,
		User:      opts.User,
		TTY:       opts.TTY,
		// 把 exec 进程放进容器的 cgroup，否则它会落在本 CLI 进程所在的
		// cgroup（实测 user.slice/...session-N.scope），**绕过 --memory /
		// --pids-limit 等全部资源限额**。
		CgroupID: cfg.ID,
	})
	if err != nil {
		if errors.Is(err, runtime.ErrNotRoot) {
			return fmt.Errorf("exec: 需要 root（setns 进入他人命名空间）: %w", err)
		}
		return err
	}
	if code != 0 {
		return &exitCodeError{code: code}
	}
	return nil
}
