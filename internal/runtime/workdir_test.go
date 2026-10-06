// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"strings"
	"testing"
)

// 本文件覆盖 --workdir / --user 的**下发与解析**。
//
// 背景：这两个参数此前被 CLI 解析、存进了 config.json，却**从未下发到
// init** —— `runtime.Config` 是冻结接口加不了字段，而当时的代码也没有
// 走环境变量这条路，于是容器 PID 1 永远是 root + 根目录，
// 且因为"恰好等于默认值"而没有任何报错。

func TestWorkdirEnvOmitsEmpty(t *testing.T) {
	for _, s := range []string{"", "   ", "\t"} {
		if got := WorkdirEnv(s); got != nil {
			t.Errorf("WorkdirEnv(%q) 应为 nil（不改动），实得 %v", s, got)
		}
	}
	got := WorkdirEnv("/srv/app")
	if len(got) != 1 || got[0] != "LICORE_WORKDIR=/srv/app" {
		t.Errorf("WorkdirEnv(/srv/app) = %v", got)
	}
}

func TestUserEnvOmitsEmpty(t *testing.T) {
	for _, s := range []string{"", "  "} {
		if got := UserEnv(s); got != nil {
			t.Errorf("UserEnv(%q) 应为 nil（不改动），实得 %v", s, got)
		}
	}
	got := UserEnv("1000:1000")
	if len(got) != 1 || got[0] != "LICORE_USER=1000:1000" {
		t.Errorf("UserEnv(1000:1000) = %v", got)
	}
}

// **核心**：解析 uid:gid，含"只给 uid 时 gid 跟随 uid"的语义。
func TestParseUserSpec(t *testing.T) {
	cases := []struct {
		in      string
		uid     int
		gid     int
		wantErr bool
	}{
		{"1000:1000", 1000, 1000, false},
		{"0:0", 0, 0, false},
		// 只给 uid 时 gid 应跟随 uid（与 Docker 一致），而不是留 0——
		// 留 0 会让容器里的文件属组变成 root，是个隐蔽的坑。
		{"1000", 1000, 1000, false},
		{"0", 0, 0, false},
		{"  1000 : 2000  ", 1000, 2000, false},
		// 非法输入必须报错，不能静默当成 root。
		{"", 0, 0, true},
		{"abc", 0, 0, true},
		{"1000:abc", 0, 0, true},
		{":1000", 0, 0, true},
		{"1000:", 0, 0, true},
		{"-1", 0, 0, true},
		{"+1", 0, 0, true},
		{"1.5", 0, 0, true},
		// 名字形式当前不支持：必须报错，绝不静默忽略
		// （静默忽略 = 用户以为换了身份，其实还是 root）。
		{"nobody", 0, 0, true},
	}
	for _, c := range cases {
		uid, gid, err := ParseUserSpec(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseUserSpec(%q) 应报错，实得 uid=%d gid=%d", c.in, uid, gid)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseUserSpec(%q) 意外报错: %v", c.in, err)
			continue
		}
		if uid != c.uid || gid != c.gid {
			t.Errorf("ParseUserSpec(%q) = (%d,%d)，期望 (%d,%d)", c.in, uid, gid, c.uid, c.gid)
		}
	}
}

// 从 env 切片读回（helper 侧即用此函数取值）。
func TestWorkdirUserFromEnv(t *testing.T) {
	env := []string{"PATH=/bin", "LICORE_WORKDIR=/srv", "LICORE_USER=1000:1000", "FOO=bar"}
	if got := workdirFromEnv(env); got != "/srv" {
		t.Errorf("workdirFromEnv = %q", got)
	}
	if got := userFromEnv(env); got != "1000:1000" {
		t.Errorf("userFromEnv = %q", got)
	}
	if got := workdirFromEnv([]string{"PATH=/bin"}); got != "" {
		t.Errorf("无该变量时应返回空，实得 %q", got)
	}
	if got := userFromEnv(nil); got != "" {
		t.Errorf("nil env 应返回空，实得 %q", got)
	}
}

// **安全约束**：这两个变量必须以 LICORE_ 前缀出现，
// 否则 envWithoutLiCore 剥不掉它们，会泄漏进容器里用户命令的环境。
func TestWorkdirUserEnvAreStripped(t *testing.T) {
	env := append(WorkdirEnv("/srv"), UserEnv("1000:1000")...)
	env = append(env, "PATH=/bin", "HOME=/root")
	for _, kv := range env {
		if !strings.HasPrefix(kv, "LICORE_") {
			continue
		}
		// 确认它们确实会被剥离。
		kept := envWithoutLiCoreFrom([]string{kv, "PATH=/bin"})
		for _, k := range kept {
			if strings.HasPrefix(k, "LICORE_") {
				t.Errorf("%q 未被 envWithoutLiCore 剥离，会泄漏进用户命令", k)
			}
		}
	}
	// 正向确认：剥离后剩下的是用户自己的变量。
	kept := envWithoutLiCoreFrom(append(append(WorkdirEnv("/srv"), UserEnv("1000")...), "PATH=/bin"))
	if len(kept) != 1 || kept[0] != "PATH=/bin" {
		t.Errorf("剥离结果 = %v，期望只剩 PATH=/bin", kept)
	}
}

// exec 侧的 env 必须带上 user（否则 helper 无从降权）。
func TestExecSetupEnvUserCarriesUser(t *testing.T) {
	got := ExecSetupEnvUser([]string{"ALL"}, nil, "1000:1000")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "LICORE_USER=1000:1000") {
		t.Errorf("ExecSetupEnvUser 未带上 LICORE_USER: %v", got)
	}
	if !strings.Contains(joined, "LICORE_CAPS_DROP=ALL") {
		t.Errorf("ExecSetupEnvUser 丢了 caps: %v", got)
	}
	// 空 user 不应产生该变量（保持"不切换身份"语义）。
	for _, kv := range ExecSetupEnvUser(nil, nil, "") {
		if strings.HasPrefix(kv, "LICORE_USER=") {
			t.Errorf("user 为空时不该产生 LICORE_USER: %v", kv)
		}
	}
}
