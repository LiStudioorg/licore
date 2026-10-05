// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

// 未设置内存上限时的告警回归测试（I7）。
//
// 背景：没有 cgroup 内存限额的容器可以把内存吃到触发**宿主 OOM killer**，
// 杀死宿主上的其它进程。这是真实的"容器影响宿主机"路径。
//
// 决策（用户 2026-10-05 确认）：**不改默认限额**（会偏离 Docker 语义，
// 并可能破坏合法的大内存负载），改为在 run 时明确告警 + 文档说明。
//
// 本测试的作用是**锁死这条告警**：它是这次决策的产品化落点，
// 静默删掉就等于把用户重新丢回一个看不见的默认值。

import (
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// captureSlog 捕获一段 slog 输出，返回日志文本。
//
// 用 slog.SetDefault 而不是替换 handler 之外的机制：被测代码直接调用
// 包级 slog.Warn，只有换掉默认 logger 才能观察到。
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()

	var mu sync.Mutex
	var sb strings.Builder

	// 恢复原默认 logger，避免污染同一测试进程里的其它用例。
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &sb}, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	fn()

	mu.Lock()
	defer mu.Unlock()
	return sb.String()
}

// lockedWriter 让 TextHandler 的写入与读取之间没有数据竞争。
type lockedWriter struct {
	mu *sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestWarnWhenMemoryUnlimited 断言未指定 --memory 时出现告警，且内容点明宿主风险。
func TestWarnWhenMemoryUnlimited(t *testing.T) {
	out := captureSlog(t, func() { warnIfMemoryUnlimited(0, "alpine:3.20") })

	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("未设置内存上限时应打 WARN 级日志，实际输出：%q", out)
	}
	// 必须点明"会影响宿主"，否则用户会以为只是容器自己受限。
	if !strings.Contains(out, "OOM") {
		t.Errorf("告警应说明宿主 OOM 风险，实际：%q", out)
	}
	if !strings.Contains(out, "--memory") {
		t.Errorf("告警应给出可执行的建议（--memory），实际：%q", out)
	}
}

// TestNoWarnWhenMemoryLimited 断言设了 --memory 就**不该**再告警。
//
// 反向用例是必要的：只测"该响的时候响"，一个无条件告警的实现也能通过，
// 那样用户很快会学会忽略它——告警就失效了。
func TestNoWarnWhenMemoryLimited(t *testing.T) {
	out := captureSlog(t, func() { warnIfMemoryUnlimited(256, "alpine:3.20") })
	if strings.Contains(out, "level=WARN") {
		t.Errorf("已设置 --memory 时不应告警，实际输出：%q", out)
	}
}

// TestWarnNegativeMemoryTreatedAsUnlimited 断言负值也按"未限制"处理。
//
// --memory 的语义是"0 = 不限制"（见 flag 帮助文本），而负值同属非法/未设置，
// 不能因为没走 ==0 分支就静默放过。
func TestWarnNegativeMemoryTreatedAsUnlimited(t *testing.T) {
	out := captureSlog(t, func() { warnIfMemoryUnlimited(-1, "alpine:3.20") })
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("负数应等同于未限制并告警，实际输出：%q", out)
	}
}
