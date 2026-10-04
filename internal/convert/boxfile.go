// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// inspectResult 是 `docker inspect` 中我们关心的字段子集。
// 未列出的字段一律忽略（Docker 的 inspect 输出字段极多且随版本增多）。
type inspectResult struct {
	Config struct {
		User         string              `json:"User"`
		Env          []string            `json:"Env"`
		Entrypoint   []string            `json:"Entrypoint"`
		Cmd          []string            `json:"Cmd"`
		WorkingDir   string              `json:"WorkingDir"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Volumes      map[string]struct{} `json:"Volumes"`
		Labels       map[string]string   `json:"Labels"`
	} `json:"Config"`
	Os string `json:"Os"`
}

// parseInspect 解析 `docker inspect <image>` 的 JSON 数组输出。
// Docker 返回的是数组（按参数个数），转换只针对单个镜像，取第一个元素。
func parseInspect(data []byte) (*inspectResult, error) {
	var arr []inspectResult
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, fmt.Errorf("解析 docker inspect 输出失败: %w", err)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("docker inspect 未返回任何条目")
	}
	return &arr[0], nil
}

// genOptions 控制 Boxfile 生成。
type genOptions struct {
	// TopDirs 是 rootfs 的顶层目录名（已排序），逐个生成 COPY。
	TopDirs []string
	// TopFiles 是 rootfs 根下的常规文件名（已排序），逐个生成 COPY。
	TopFiles []string
}

// generateBoxfile 依据 inspect 结果与 rootfs 顶层结构生成 Boxfile 文本。
//
// 生成的顺序遵循「先铺文件、再配元数据」，与 Dockerfile 的常见书写习惯一致：
//
//	FROM scratch
//	COPY <每个顶层目录/文件>
//	ENV / WORKDIR / USER / EXPOSE / VOLUME / LABEL
//	ENTRYPOINT / CMD
//
// 各类值按 LiCore 解析器的约束处理：路径型参数转义（escapeArg），
// 无法无损表达的 ENV/LABEL 跳过（envValueExpressible）。
func generateBoxfile(ins *inspectResult, o genOptions) string {
	var b strings.Builder
	b.WriteString("FROM scratch\n")

	// COPY 逐条列出：LiCore 有意不支持 `COPY . /`（见 docs/image-spec.md 6.1）。
	if len(o.TopDirs) > 0 || len(o.TopFiles) > 0 {
		b.WriteString("\n")
	}
	for _, d := range o.TopDirs {
		fmt.Fprintf(&b, "COPY %s /%s\n", d, d)
	}
	for _, f := range o.TopFiles {
		fmt.Fprintf(&b, "COPY %s /%s\n", f, f)
	}

	// ENV：Docker 里是 "KEY=VALUE" 列表，逐条写成 ENV KEY=VALUE。
	//
	// LiCore 的 ENV 语法有三条硬约束，且**无法绕过**：
	//   - 值不能为空
	//   - 值里不能出现 '='（解析器把第二个 '=' 视为错误）
	//   - 值里不能出现 "${"（解析器无条件做变量展开，且没有转义语法：
	//     `\${X}` 与 `$${X}` 实测同样报"未声明的变量"）
	//
	// Docker 镜像里这三类都真实存在（base64、URL query、JAVA_OPTS=-Xmx${MEMORY}
	// 等）。这类值**无法无损表达**，因此跳过并计数，由调用方警告用户；
	// 绝不静默改写成别的值——悄悄改掉用户的环境变量比丢掉它更危险。
	var envLines []string
	for _, kv := range ins.Config.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || !envValueExpressible(v) {
			continue
		}
		envLines = append(envLines, fmt.Sprintf("ENV %s=%s", k, v))
	}
	if len(envLines) > 0 {
		b.WriteString("\n")
		for _, l := range envLines {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}

	// WORKDIR：必须是绝对路径（LiCore 校验），相对路径直接丢弃而不是猜。
	if wd := ins.Config.WorkingDir; wd != "" && strings.HasPrefix(wd, "/") {
		fmt.Fprintf(&b, "\nWORKDIR %s\n", escapeArg(wd))
	}

	// USER：Docker 允许 "1000"、"1000:1000"、"user"、"user:group"，
	// LiCore 的 USER 同样接受这几种形式，直接透传（去掉空白）。
	if u := strings.TrimSpace(ins.Config.User); u != "" && !strings.ContainsAny(u, " \t") {
		fmt.Fprintf(&b, "USER %s\n", escapeArg(u))
	}

	// EXPOSE：Docker 的键是 "8080/tcp"、"53/udp"，LiCore 的 EXPOSE 接受
	// "8080/tcp" 这一形式，直接透传；排序保证输出稳定可复现。
	if len(ins.Config.ExposedPorts) > 0 {
		b.WriteString("\n")
		for _, p := range sortedKeys(ins.Config.ExposedPorts) {
			fmt.Fprintf(&b, "EXPOSE %s\n", p)
		}
	}

	// VOLUME：Docker 的键就是容器内绝对路径。
	if len(ins.Config.Volumes) > 0 {
		b.WriteString("\n")
		for _, v := range sortedKeys(ins.Config.Volumes) {
			fmt.Fprintf(&b, "VOLUME %s\n", escapeArg(v))
		}
	}

	// LABEL：值里不能出现 '='，且不能为空；Docker 镜像里常有空标签
	// （如 org.opencontainers.image.* 空值）与含 '=' 的值（base64、URL 参数），
	// 这些无法用 LiCore 的 LABEL 语法表达，跳过并在调用方汇总警告。
	// Docker 自带的 buildkit/maintainer 一类标签也跳过，减少噪音。
	var labelLines []string
	for _, k := range sortedLabelKeys(ins.Config.Labels) {
		v := ins.Config.Labels[k]
		if v == "" || strings.Contains(v, "=") || strings.ContainsAny(v, "\r\n") {
			continue
		}
		if !validLabelKey(k) {
			continue
		}
		labelLines = append(labelLines, fmt.Sprintf("LABEL %s=%s", k, escapeArg(v)))
	}
	if len(labelLines) > 0 {
		b.WriteString("\n")
		for _, l := range labelLines {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}

	// ENTRYPOINT / CMD：LiCore 只接受 JSON 数组（exec 形式），
	// 而 Docker 的 inspect 本来就返回数组，重新序列化即可——注意不能
	// 直接拼接字符串，必须走 JSON 编码以正确转义引号与反斜杠。
	if len(ins.Config.Entrypoint) > 0 {
		fmt.Fprintf(&b, "\nENTRYPOINT %s\n", jsonArray(ins.Config.Entrypoint))
	}
	if len(ins.Config.Cmd) > 0 {
		fmt.Fprintf(&b, "\nCMD %s\n", jsonArray(ins.Config.Cmd))
	}
	return b.String()
}

// unexpressibleCommand 检查 ENTRYPOINT/CMD 里是否有 LiCore 无法表达的参数。
//
// 解析器对参数无条件做 ${NAME} 展开，且实测没有任何转义语法
// （`\${X}` 会被 JSON 解析拒绝，`$${X}` 同样展开）。因此含 "${" 的参数
// 无法写进 Boxfile。
//
// 与 ENV/LABEL 的处理不同，这里**不能跳过**：丢掉 ENTRYPOINT 会让镜像
// 根本跑不起来（或跑成错误的进程），比转换失败更糟。因此返回错误让调用方
// 明确失败。
func unexpressibleCommand(ins *inspectResult) (op, arg string, found bool) {
	for _, a := range ins.Config.Entrypoint {
		if strings.Contains(a, "${") {
			return "ENTRYPOINT", a, true
		}
	}
	for _, a := range ins.Config.Cmd {
		if strings.Contains(a, "${") {
			return "CMD", a, true
		}
	}
	return "", "", false
}

// jsonArray 把 argv 编码为 JSON 数组字面量。
// 用 json.Marshal 而非手工拼接，确保引号、反斜杠、非 ASCII 都正确转义。
func jsonArray(argv []string) string {
	out, err := json.Marshal(argv)
	if err != nil {
		// []string 的 Marshal 不会失败；保守回退到空数组而不是产出坏 Boxfile。
		return "[]"
	}
	return string(out)
}

// envValueExpressible 判断一个 ENV 值能否用 LiCore 的 Boxfile 语法无损表达。
//
// 不能表达的情况（见 generateBoxfile 的说明）：空值、含 '='、含 "${"。
func envValueExpressible(v string) bool {
	if v == "" {
		return false
	}
	if strings.Contains(v, "=") {
		return false
	}
	// 解析器对 "${" 无条件展开且没有转义语法，含它的值写进去必然构建失败。
	if strings.Contains(v, "${") {
		return false
	}
	return !strings.ContainsAny(v, "\r\n")
}

// escapeArg 转义 LiCore Boxfile 解析器会特殊处理的字符。
//
// 仅用于 COPY/WORKDIR/USER/VOLUME 这类「路径型」参数：它们同样会做 ${NAME}
// 展开，但取值来自文件系统（不含 '$'），这里做一次防御性转义，避免镜像里
// 恰好存在形如 `${x}` 的目录名时构建莫名其妙地失败。
//
// 注意：ENV/LABEL 的值不走这里——解析器没有转义语法，含 "${" 的值只能跳过
// （见 envValueExpressible）。
func escapeArg(s string) string {
	return strings.ReplaceAll(s, "$", `\$`)
}

// sortedKeys 返回 map 的键并排序，保证生成结果可复现。
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedLabelKeys 返回标签键并排序。
func sortedLabelKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validLabelKey 判断标签键能否用 LiCore 的 LABEL 语法表达。
// 允许字母数字开头的点分/斜杠/下划线/连字符形式（与 OCI 标签习惯一致）。
func validLabelKey(k string) bool {
	if k == "" || strings.ContainsAny(k, " \t=$\n") {
		return false
	}
	for i, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == '/':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
