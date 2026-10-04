// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"strings"
	"testing"
)

// TestValidateCapSpecsAccepts 验证合法写法（含大小写与 CAP_ 前缀变体）被接受。
func TestValidateCapSpecsAccepts(t *testing.T) {
	cases := []struct {
		name string
		drop []string
		add  []string
	}{
		{"空", nil, nil},
		{"标准写法", []string{"CAP_SYS_ADMIN"}, []string{"CAP_NET_ADMIN"}},
		{"小写", []string{"cap_sys_admin"}, nil},
		{"无前缀", []string{"SYS_ADMIN"}, []string{"NET_ADMIN"}},
		{"混合大小写无前缀", []string{"net_admin"}, nil},
		{"ALL 关键字", []string{"ALL"}, []string{"NET_ADMIN"}},
		{"小写 all", []string{"all"}, nil},
		{"多个", []string{"SYS_ADMIN", "NET_ADMIN", "SYS_PTRACE"}, []string{"CHOWN"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateCapSpecs(tc.drop, tc.add); err != nil {
				t.Errorf("应被接受，却报错: %v", err)
			}
		})
	}
}

// TestValidateCapSpecsRejects 验证非法名字在**启动前**就被拒绝。
//
// 时机很重要：若拖到容器 init 的 exec 前才失败，用户面对的是一个已经建好
// rootfs、联了网、占用了名字却起不来的容器，排查成本高得多。
func TestValidateCapSpecsRejects(t *testing.T) {
	cases := []struct {
		name string
		drop []string
		add  []string
	}{
		{"drop 里拼错", []string{"CAP_SYS_ADMI"}, nil},
		{"add 里拼错", nil, []string{"NOT_A_CAP"}},
		{"空字符串项", []string{""}, nil},
		{"纯空白项", nil, []string{"   "}},
		{"数字", []string{"1234"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCapSpecs(tc.drop, tc.add)
			if err == nil {
				t.Fatalf("非法能力名应报错：drop=%v add=%v", tc.drop, tc.add)
			}
			if !strings.Contains(err.Error(), "run:") {
				t.Errorf("错误应带 run: 前缀便于定位: %v", err)
			}
		})
	}
}

// TestValidateCapSpecsErrorListsOptions 验证错误信息给出可选值，
// 否则用户不知道能写什么。
func TestValidateCapSpecsErrorListsOptions(t *testing.T) {
	err := validateCapSpecs(nil, []string{"BOGUS"})
	if err == nil {
		t.Fatal("应报错")
	}
	for _, want := range []string{"NET_ADMIN", "SYS_ADMIN", "ALL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含可选值 %q: %v", want, err)
		}
	}
}
