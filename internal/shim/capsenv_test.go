// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package shim

import (
	"context"
	"strings"
	"testing"

	"github.com/LiStudioorg/licore/internal/runtime"
	"github.com/LiStudioorg/licore/internal/store"
)

// 本文件守住一条不变量：**所有启动容器的路径必须下发给 init 同一份装配环境**。
//
// 事故：`licore run -d` 走 shim 路径，而 shim 构造 runtime.Config.Env 时漏了
// runtime.CapsEnv —— 于是 `--cap-add` / `--cap-drop` 在 -d 容器上静默失效
// （config.json 里值是对的，init 环境里却没有 LICORE_CAPS_*，capsFromEnv
// 读空后退回默认集，恰好等于没传参数，所以不报错）。
// 前台路径 engine.go 一直有这一行，两条路各写各的才漏掉。
//
// 这类"某一条路径忘了下发某项装配参数"的缺陷不会有编译错误、不会有运行时报错，
// 只能靠断言"两条路径产出的 env 一致"来拦。

// captureStartEnv 跑一次 shim 启动流程，捕获它交给 runtime 的 env。
func captureStartEnv(t *testing.T, cfg *store.ContainerConfig) []string {
	t.Helper()
	old := startWithFn
	var got []string
	startWithFn = func(c *runtime.Config, onChild func(int), _ *runtime.StartOptions) (*runtime.StartResult, error) {
		got = append([]string{}, c.Env...)
		if onChild != nil {
			onChild(4242)
		}
		return &runtime.StartResult{ChildPID: 4242, ExitCode: 0}, nil
	}
	t.Cleanup(func() { startWithFn = old })

	st := &store.Store{Root: t.TempDir()}
	if err := st.CreateContainer(cfg); err != nil {
		t.Fatal(err)
	}
	// restart=no：init 退出后 shim 不再拉起，Run 会正常返回。
	if err := Run(context.Background(), &Options{Store: st, Cfg: cfg}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got == nil {
		t.Fatal("未捕获到 env（startWithFn 未被调用？）")
	}
	return got
}

// envValue 从 KEY=VALUE 切片里取 KEY 的值。
func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// **核心回归护栏**：--cap-drop / --cap-add 必须真的下发到 init 的环境。
//
// 删掉 shim 里那行 runtime.CapsEnv 后，本用例失败并报
// "LICORE_CAPS_DROP 未下发"。
func TestStartEnvCarriesCapabilitySpec(t *testing.T) {
	cases := []struct {
		name     string
		drop     []string
		add      []string
		wantDrop string // "" 表示不应出现该变量
		wantAdd  string
	}{
		{"不传参数", nil, nil, "", ""},
		{"只 drop ALL", []string{"ALL"}, nil, "ALL", ""},
		{"只 add SYS_ADMIN", nil, []string{"SYS_ADMIN"}, "", "SYS_ADMIN"},
		{"drop ALL + add NET_BIND_SERVICE", []string{"ALL"}, []string{"NET_BIND_SERVICE"}, "ALL", "NET_BIND_SERVICE"},
		{"多个能力", []string{"NET_RAW", "SYS_CHROOT"}, []string{"SYS_ADMIN", "NET_ADMIN"}, "NET_RAW,SYS_CHROOT", "SYS_ADMIN,NET_ADMIN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := captureStartEnv(t, &store.ContainerConfig{
				ConfigVersion: 1, ID: "cap123456789", Name: "cap", ImageRef: "x:v1",
				Rootfs: "/tmp/x", Restart: store.RestartNo, Cmd: []string{"/x"},
				CapDrop: c.drop, CapAdd: c.add,
			})

			gotDrop, hasDrop := envValue(env, "LICORE_CAPS_DROP")
			if c.wantDrop == "" {
				if hasDrop {
					t.Errorf("不该出现 LICORE_CAPS_DROP，却得到 %q", gotDrop)
				}
			} else if !hasDrop {
				t.Errorf("LICORE_CAPS_DROP 未下发！--cap-drop %v 会在容器上静默失效", c.drop)
			} else if gotDrop != c.wantDrop {
				t.Errorf("LICORE_CAPS_DROP = %q，期望 %q", gotDrop, c.wantDrop)
			}

			gotAdd, hasAdd := envValue(env, "LICORE_CAPS_ADD")
			if c.wantAdd == "" {
				if hasAdd {
					t.Errorf("不该出现 LICORE_CAPS_ADD，却得到 %q", gotAdd)
				}
			} else if !hasAdd {
				t.Errorf("LICORE_CAPS_ADD 未下发！--cap-add %v 会在容器上静默失效", c.add)
			} else if gotAdd != c.wantAdd {
				t.Errorf("LICORE_CAPS_ADD = %q，期望 %q", gotAdd, c.wantAdd)
			}
		})
	}
}

// 与 engine 的**最终产物**对齐：CapsEnv 的输出必须原样出现在 shim 的 env 里。
// 这样即便将来 engine/shim 各自改写 env 构造，只要 CapsEnv 语义不变，
// 本用例仍能指出"某条路径没带上它"。
func TestStartEnvIncludesCapsEnvVerbatim(t *testing.T) {
	drop := []string{"ALL"}
	add := []string{"NET_BIND_SERVICE", "CHOWN"}
	env := captureStartEnv(t, &store.ContainerConfig{
		ConfigVersion: 1, ID: "cap000000001", Name: "cap2", ImageRef: "x:v1",
		Rootfs: "/tmp/x", Restart: store.RestartNo, Cmd: []string{"/x"},
		CapDrop: drop, CapAdd: add,
	})

	for _, want := range runtime.CapsEnv(drop, add) {
		found := false
		for _, kv := range env {
			if kv == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("shim 的 env 缺少 %q（engine 前台路径会带上它）—— 两条路径不一致", want)
		}
	}
}

// 反向约束：装配参数不止 cap 一项，其余几项也必须仍在（防止修 cap 时误删）。
func TestStartEnvKeepsOtherAssemblyParams(t *testing.T) {
	env := captureStartEnv(t, &store.ContainerConfig{
		ConfigVersion: 1, ID: "cap000000002", Name: "cap3", ImageRef: "x:v1",
		Rootfs: "/tmp/x", Restart: store.RestartNo, Cmd: []string{"/x"},
		CapDrop: []string{"ALL"},
	})
	// 容器 ID 一定经 CgroupEnv 下发；这条同时证明"其它装配项没被 cap 的修改挤掉"。
	if v, ok := envValue(env, "LICORE_CGROUP_ID"); !ok || v != "cap000000002" {
		t.Errorf("LICORE_CGROUP_ID 应下发为容器 ID，实得 %q ok=%v", v, ok)
	}
}
