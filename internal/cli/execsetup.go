// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/licore/internal/runtime"
)

// newExecSetupCommand 注册隐藏命令 `licore exec-setup`：仅由 `licore exec`
// 在容器内通过 bind 进来的 helper 路径唤起，不面向用户。
//
// 它做三件事（与容器 init 的收口完全一致），然后 execve 用户命令：
//  1. PR_SET_NO_NEW_PRIVS
//  2. capability 裁剪（**只减不加**）
//  3. seccomp 黑名单
//
// 注意 Args 用 ArbitraryArgs 且 DisableFlagParsing：用户命令可能是
// `/bin/sh -c "..."`，其中 `-c` 会被 cobra 当成自己的 flag 吃掉。
// 因此这里完全不做 flag 解析，其余部分原样交给 runtime。
func newExecSetupCommand(_ io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:                "exec-setup [--] <command> [args...]",
		Short:              "exec 收口入口（内部使用）",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			cmdline, err := runtime.ParseExecSetupArgs(append([]string{"exec-setup"}, args...))
			if err != nil {
				return err
			}
			return runtime.RunExecSetup(cmdline)
		},
	}
}

// rejectCapAddForExec 在 exec 路径上拒绝 --cap-add。
//
// 这是**安全边界**，不是参数风格问题：若允许 `licore exec --cap-add SYS_ADMIN`，
// 任何能执行 licore exec 的人都能把 CAP_SYS_ADMIN 加回来，容器 init 做的
// no_new_privs / cap-drop / seccomp 收口就全部形同虚设。
//
// 需要更多能力时的正确做法是用 `licore run` 起一个配置正确的新容器。
func rejectCapAddForExec(capAdd []string) error {
	if len(capAdd) == 0 {
		return nil
	}
	return fmt.Errorf("exec: 不允许放宽能力，--cap-add 被拒绝（收到 %v）。"+
		"exec 只能进一步收紧；如需更多能力，请用 licore run 重新起一个容器", capAdd)
}
