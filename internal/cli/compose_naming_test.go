// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package cli

import (
	"testing"

	"github.com/LiStudioorg/licore/internal/compose"
	"github.com/LiStudioorg/licore/internal/store"
)

// TestServiceContainerName 固定副本命名规则：首副本无序号、其余带 _<n>。
//
// 这条规则是 scale / up / stopService / countService 的公共契约。
// 真机实测的缺陷正是 startService 无视序号、所有副本都取
// `<project>_<service>`，于是 `compose scale web=3` 第二个副本就撞名失败。
func TestServiceContainerName(t *testing.T) {
	cc := &composeCmd{}
	p := &compose.Project{Name: "proj"}
	cases := []struct {
		idx  int
		want string
	}{
		{0, "proj_web"},
		{1, "proj_web_1"},
		{2, "proj_web_2"},
		{10, "proj_web_10"},
	}
	for _, c := range cases {
		if got := cc.serviceContainerName(p, "web", c.idx); got != c.want {
			t.Errorf("idx=%d: 容器名 = %q，期望 %q", c.idx, got, c.want)
		}
	}
}

// TestCountServiceMatchesOnlyOwnReplicas 覆盖副本计数：
// 只数本服务的副本，且不能被名字相似的**兄弟服务**污染。
func TestCountServiceMatchesOnlyOwnReplicas(t *testing.T) {
	st := newTempStore(t)
	mk := func(name string) {
		if err := st.CreateContainer(&store.ContainerConfig{
			ConfigVersion: 1,
			ID:            name,
			Name:          name,
			ImageRef:      "x:1",
			Rootfs:        "/tmp/" + name,
			Restart:       store.RestartNo,
			Cmd:           []string{"/bin/true"},
		}); err != nil {
			t.Fatalf("创建容器 %s: %v", name, err)
		}
	}
	// proj_web 的两个副本 + 会与前缀匹配混淆的兄弟服务。
	mk("proj_web")
	mk("proj_web_1")
	mk("proj_web_2")
	mk("proj_web_2x")   // 兄弟服务 "web_2x"，不是 web 的副本
	mk("proj_webother") // 兄弟服务 "webother"，不是 web 的副本
	mk("other_web")     // 别的项目

	cc := &composeCmd{}
	p := &compose.Project{Name: "proj"}
	if got := cc.countService(st, p, "web"); got != 3 {
		t.Errorf("countService(web) = %d，期望 3（proj_web/_1/_2）", got)
	}
	if got := cc.countService(st, p, "webother"); got != 1 {
		t.Errorf("countService(webother) = %d，期望 1", got)
	}
}

// newTempStore 造一个指向临时目录的 store。
func newTempStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return st
}
