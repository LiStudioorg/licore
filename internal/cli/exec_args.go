// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"fmt"
	"strings"

	"github.com/LiStudioorg/licore/internal/runtime"
)

// execArgs 是 `licore exec` 手工解析后的参数。
type execArgs struct {
	// Container 是容器 ID 或名字。
	Container string
	// Cmd 是容器内命令 argv（容器名之后的全部内容，原样）。
	Cmd []string
	// 以下是 exec 自己的选项。
	Interactive bool
	TTY         bool
	User        string
	Workdir     string
	Env         []string
	DataDir     string
	CapDrop     []string
	CapAdd      []string
	// wantHelp 表示用户请求了 --help；由调用方转成帮助输出。
	//
	// 之所以要显式识别：exec 用 DisableFlagParsing 自己解析参数，
	// cobra 不再处理 --help，不识别就会把它当未知选项报错。
	wantHelp bool
}

// execFlagTakesValue 列出「需要取值」的 exec 选项（长/短两种写法）。
//
// 之所以手工维护这张表：exec 的参数语义与 run 不同——容器名之后的内容全部
// 属于容器命令，而容器命令自身可能带 -c/--foo 这类以横杠开头的东西。
// 交给 cobra 的交错解析做不到这一点：
//   - 默认交错解析会把容器命令的 -c 当成 licore 的 flag（报 unknown shorthand flag）；
//   - SetInterspersed(false) 又会在遇到容器名后停止解析，导致
//     `exec 容器 -w /tmp cmd` 里的 -w 被当成容器命令。
//
// 因此改为「扫描到第一个非 flag 即为容器名，其后一律是容器命令」——
// 这与 Docker 的 exec 语义一致，也让 flag 写在容器名前后都可以。
var execFlagTakesValue = map[string]bool{
	"-w": true, "--workdir": true,
	"-u": true, "--user": true,
	"-e": true, "--env": true,
	"--data-dir": true,
	"--cap-drop": true,
	"--cap-add":  true,
}

// execBoolFlags 是「不带值」的 exec 选项。
var execBoolFlags = map[string]bool{
	"-i": true, "--interactive": true,
	"-t": true, "--tty": true,
}

// parseExecArgs 手工解析 exec 的 argv（不含 "exec" 自身）。
//
// 规则：
//   - 第一个非选项参数是容器名；
//   - 容器名之后**仍继续识别 exec 自己的选项**，直到遇到第一个"不是选项"的
//     参数——那一刻起，其后所有内容原样作为容器命令（含横杠开头的）；
//   - `--` 表示"其后第一个参数即容器命令"。
//
// 为什么容器名之后还要继续识别选项：用户很自然会写
//
//	licore exec myapp -w /tmp /bin/sh -c pwd
//
// 若一见容器名就把后面全当命令，-w 会被吞进容器命令里（真机踩过这个坑）。
func parseExecArgs(argv []string) (*execArgs, error) {
	o := &execArgs{}
	var cmd []string
	cmdStarted := false

	for i := 0; i < len(argv); i++ {
		a := argv[i]

		// 已进入"容器命令"阶段：一律原样收集，不再解析。
		if cmdStarted {
			cmd = append(cmd, a)
			continue
		}

		// 帮助选项：DisableFlagParsing 之后 cobra 不再处理 --help，
		// 这里显式识别并交给上层（RunE 里转成 help 输出），否则
		// `licore exec --help` 会被当成未知选项报错。
		if a == "-h" || a == "--help" {
			o.wantHelp = true
			return o, nil
		}

		// `--`：其后全部是容器命令。
		if a == "--" {
			if o.Container == "" {
				return nil, fmt.Errorf("exec: `--` 之前缺少容器名")
			}
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("exec: `--` 之后缺少要执行的命令")
			}
			cmd = append(cmd, argv[i+1:]...)
			cmdStarted = true
			break
		}

		// 带 `=` 的选项形式：-w=/tmp、--workdir=/app。
		name, val, hasEq := strings.Cut(a, "=")
		if strings.HasPrefix(a, "-") && hasEq && execFlagTakesValue[name] {
			if err := applyExecOption(o, name, val); err != nil {
				return nil, err
			}
			continue
		}

		if strings.HasPrefix(a, "-") {
			if execBoolFlags[a] {
				if err := applyExecOption(o, a, ""); err != nil {
					return nil, err
				}
				continue
			}
			if execFlagTakesValue[a] {
				if i+1 >= len(argv) {
					return nil, fmt.Errorf("exec: 选项 %s 缺少取值", a)
				}
				i++
				if err := applyExecOption(o, a, argv[i]); err != nil {
					return nil, err
				}
				continue
			}
			// 合并的短布尔选项（-it / -ti）——Docker 用户习惯写法。
			// 只支持**全由布尔短选项组成**的组合；其中任一需要取值就报错，
			// 避免把 -w 这类吞进组合里造成歧义。
			if isCombinedShortBools(a) {
				for _, r := range a[1:] {
					if err := applyExecOption(o, "-"+string(r), ""); err != nil {
						return nil, err
					}
				}
				continue
			}
			// 未知选项：只在"还没进入容器命令"时才有歧义。
			// 容器名之后遇到的未知横杠参数，视为容器命令的开始（如 -c 只可能
			// 是容器命令的一部分），不再报错。
			if o.Container == "" {
				return nil, fmt.Errorf("exec: 未知选项 %q（exec 的选项需写在容器名之前，"+
					"或改用 -- 分隔）", a)
			}
			cmd = append(cmd, a)
			cmdStarted = true
			continue
		}

		// 非选项参数。
		if o.Container == "" {
			o.Container = a
			continue
		}
		// 容器名之后遇到的第一个非选项参数 → 容器命令从这里开始。
		cmd = append(cmd, a)
		cmdStarted = true
	}

	if o.Container == "" {
		return nil, fmt.Errorf("exec: 缺少容器名")
	}
	if len(cmd) == 0 {
		return nil, fmt.Errorf("exec: 缺少要执行的命令")
	}
	if err := rejectDirectHelperInvocation(cmd[0]); err != nil {
		return nil, err
	}
	o.Cmd = cmd
	return o, nil
}

// applyExecOption 把「选项名 + 取值」写入 execArgs。
func applyExecOption(o *execArgs, name, val string) error {
	switch name {
	case "-w", "--workdir":
		o.Workdir = val
	case "-u", "--user":
		o.User = val
	case "-e", "--env":
		o.Env = append(o.Env, val)
	case "--data-dir":
		o.DataDir = val
	case "--cap-drop":
		o.CapDrop = append(o.CapDrop, val)
	case "--cap-add":
		o.CapAdd = append(o.CapAdd, val)
	case "-i", "--interactive":
		o.Interactive = true
	case "-t", "--tty":
		o.TTY = true
	default:
		return fmt.Errorf("exec: 未知选项 %q", name)
	}
	return nil
}

// rejectDirectHelperInvocation 拒绝把 helper 路径当作容器命令来执行。
//
// `/.licore/exec-helper` 是 LiCore 的内部实现路径（容器启动时只读 bind 进去，
// 供 exec 做权限收口）。用户直接调它没有意义：
//   - 它会先做收口再 execve 目标命令，而"目标命令"就是它自己，形成自调用；
//   - 半路失败时报出来的是内部错误（如 ErrNotInit），对用户毫无指引。
//
// **只做精确匹配**：容器里 `/.licore/` 目录本身不危险，列目录、看属性都应当允许；
// 只有把 helper 可执行文件**直接当作命令**才拒绝。
func rejectDirectHelperInvocation(cmd0 string) error {
	if cmd0 != runtime.HelperPathInContainer {
		return nil
	}
	return fmt.Errorf("exec: %s 是 LiCore 内部实现路径，不能直接调用。\n"+
		"  如需执行容器命令，请直接用目标命令，例如：\n"+
		"    licore exec <容器> /bin/sh", runtime.HelperPathInContainer)
}

// isCombinedShortBools 判断参数是否为「纯布尔短选项」的组合形式，如 -it / -ti。
//
// 只接受每个字符都对应一个已注册的布尔短选项；出现取值型短选项（-w/-u/-e）
// 或未知字符一律返回 false，交给调用方按未知选项处理——避免 -w 被当成
// "-w 后跟值"以外的解读方式。
func isCombinedShortBools(a string) bool {
	if len(a) < 3 || !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
		return false
	}
	for _, r := range a[1:] {
		if !isShortBoolFlag("-" + string(r)) {
			return false
		}
	}
	return true
}

// isShortBoolFlag 报告单个短选项是否是「不带值」的选项。
func isShortBoolFlag(f string) bool {
	if len(f) != 2 {
		return false
	}
	return execBoolFlags[f]
}
