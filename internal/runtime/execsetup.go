// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package runtime

// exec 收口（`licore exec` 进入容器后的权限收紧）的跨平台常量与参数编解码。
//
// 背景：`licore exec` 走宿主侧 nsenter，其进程继承的是**宿主 root 的完整能力**，
// 因此容器 init 里做的那套收口（no_new_privs + capability 裁剪 + seccomp）
// 对 exec 完全不起作用——任何能跑 `licore exec` 的人都能拿到满能力，
// 让整套隔离形同虚设。
//
// 解法：容器启动时把 licore 二进制**只读 bind** 到容器内固定路径
// （HelperPathInContainer），exec 时让 nsenter 在容器内执行它，
// 由它做完收口再 execve 用户命令。
//
// 为什么用 bind 而不是 /proc/self/fd：
//   后者依赖容器内有 procfs 且 /proc/self/fd 可读；Android/Magisk 环境下
//   procfs 的参数与可见性不确定。bind 进来的路径在容器内**一定**可达，
//   行为可预测，跨平台更安全。代价是每个容器多一个只读文件。

import (
	"fmt"
	"os"
	"strings"
)

// HelperDirInContainer 是容器内放置 exec helper 的目录。
const HelperDirInContainer = "/.licore"

// HelperPathInContainer 是容器内 exec helper 的固定路径。
//
// 必须是**绝对路径**：nsenter 的 -r/ 已把 root 切到容器，exec 的命令按
// 容器视图解析；固定路径让 execns 不必探测。
const HelperPathInContainer = HelperDirInContainer + "/exec-helper"

// envExecSetup 标记当前进程是「exec 收口 helper」。
//
// 与容器 init 的 LICORE_INIT 同一思路——用环境变量而非子命令名判定，
// 因为 helper 就是 licore 自己的二进制，两者共用 main 分流。
const envExecSetup = "LICORE_EXEC_SETUP"

// IsExecSetupProcess 报告当前进程是否由 exec 路径唤起的收口 helper。
func IsExecSetupProcess() bool { return os.Getenv(envExecSetup) == "1" }

// ExecSetupEnv 生成 helper 需要的环境变量（由 CLI 侧注入到 nsenter 子进程）。
//
// capsDrop 是**容器创建时**记录的 --cap-drop（exec 默认继承容器配置），
// extraDrop 是本次 exec 额外要求收紧的 --cap-drop。两者都是"要丢掉的能力"，
// 合并成一个列表即可。
//
// 刻意**没有** capsAdd 参数：exec 不允许放宽能力。若允许
// `licore exec --cap-add SYS_ADMIN`，这套隔离就完全可绕过。
func ExecSetupEnv(capsDrop, extraDrop []string) []string {
	merged := make([]string, 0, len(capsDrop)+len(extraDrop))
	merged = append(merged, capsDrop...)
	merged = append(merged, extraDrop...)

	out := []string{envExecSetup + "=1"}
	if len(merged) > 0 {
		out = append(out, envCapsDrop+"="+strings.Join(merged, ","))
	}
	return out
}

// ExecSetupCapsDrop 从环境变量取出本次 exec 需要丢弃的能力列表。
// 返回的切片已去空、去重并保持顺序稳定。
func ExecSetupCapsDrop() []string {
	raw := os.Getenv(envCapsDrop)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0, 4)
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// ParseExecSetupArgs 解析 helper 的 argv（丢掉 argv[0] 与可选 "--"）。
//
// **刻意不加 build tag**：CLI 的隐藏子命令 `exec-setup` 在所有平台都要注册
// 并解析参数，只有真正执行收口的 RunExecSetup 是 Linux 专属。
// 与 capability.go 同一教训——纯逻辑放公共文件，否则 darwin 交叉编译会失败。
//
// 独立成函数是为了让参数处理可被单测覆盖：helper 在容器内被唤起，
// 出错时几乎无法交互式排查，必须在启动前用测试钉住。
func ParseExecSetupArgs(argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("exec-setup: argv 为空: %w", ErrBadConfig)
	}
	rest := argv[1:] // 丢掉 argv[0]（helper 自身路径）
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return nil, fmt.Errorf("exec-setup: 缺少要执行的命令: %w", ErrBadConfig)
	}
	if strings.TrimSpace(rest[0]) == "" {
		return nil, fmt.Errorf("exec-setup: 命令为空: %w", ErrBadConfig)
	}
	return rest, nil
}
