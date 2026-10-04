// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LiStudioorg/licore/internal/store"
)

// runCLI 在给定数据目录下执行任意 licore 子命令，返回合并输出与错误。
func runCLI(t *testing.T, dataDir string, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	root := NewRootCommand(&buf, &buf)
	root.SetArgs(append(args, "--data-dir", dataDir))
	err := root.Execute()
	return buf.String(), err
}

// buildOne 用一个最小上下文构建一个镜像，返回其 tag。
func buildOne(t *testing.T, dataDir, tag string) string {
	t.Helper()
	dir := t.TempDir()
	box := "FROM scratch\nCOPY payload.txt /payload.txt\n"
	if err := os.WriteFile(filepath.Join(dir, "payload.txt"), []byte(tag), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Boxfile"), []byte(box), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, dataDir, "build", "-t", tag, dir); err != nil {
		t.Fatalf("构建 %s 失败: %v", tag, err)
	}
	return tag
}

// TestRmiRemovesImage 断言 rmi 删掉镜像目录，且 images 不再列出它。
func TestRmiRemovesImage(t *testing.T) {
	home := t.TempDir()
	buildOne(t, home, "rmitest/img:v1")

	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.Exists("rmitest/img", "v1"); !ok {
		t.Fatal("前置条件失败：镜像未落地")
	}

	out, err := runCLI(t, home, "rmi", "rmitest/img:v1")
	if err != nil {
		t.Fatalf("rmi: %v", err)
	}
	if !strings.Contains(out, "已删除") {
		t.Errorf("输出应确认删除: %q", out)
	}
	if ok, _ := st.Exists("rmitest/img", "v1"); ok {
		t.Error("rmi 后镜像目录仍存在")
	}
	// 空掉的 name 目录也应被清理，避免 images 扫到空壳层级。
	if _, err := os.Stat(filepath.Join(st.ImagesRoot(), "rmitest", "img")); !os.IsNotExist(err) {
		t.Errorf("空的 name 目录未清理: %v", err)
	}
}

// TestRmiMissingImageReportsNotFound 删除不存在的镜像必须报错，
// 不能静默成功（否则脚本无法感知打错的名字）。
func TestRmiMissingImageReportsNotFound(t *testing.T) {
	home := t.TempDir()
	_, err := runCLI(t, home, "rmi", "nope/missing:v1")
	if err == nil {
		t.Fatal("不存在的镜像必须报错")
	}
	if !errors.Is(err, store.ErrImageNotFound) {
		t.Errorf("err = %v, want ErrImageNotFound", err)
	}
}

// TestRmiBareNameRequiresVersionWhenAmbiguous 只给 name 且有多个 tag 时
// 要求写全版本，而不是随便挑一个删。
func TestRmiBareNameRequiresVersionWhenAmbiguous(t *testing.T) {
	home := t.TempDir()
	buildOne(t, home, "multi/img:v1")
	buildOne(t, home, "multi/img:v2")

	_, err := runCLI(t, home, "rmi", "multi/img")
	if err == nil {
		t.Fatal("多个版本时必须报错要求指定版本")
	}
	if !strings.Contains(err.Error(), "版本") {
		t.Errorf("错误应提示指定版本: %v", err)
	}
	// 两个镜像都必须还在（不能误删）。
	st, _ := store.Open(home)
	for _, v := range []string{"v1", "v2"} {
		if ok, _ := st.Exists("multi/img", v); !ok {
			t.Errorf("镜像 %s 被误删", v)
		}
	}
}

// TestRmiBareNameSingleVersionSucceeds 只给 name 且只有一个 tag 时直接删。
func TestRmiBareNameSingleVersionSucceeds(t *testing.T) {
	home := t.TempDir()
	buildOne(t, home, "single/img:v9")

	if _, err := runCLI(t, home, "rmi", "single/img"); err != nil {
		t.Fatalf("唯一版本应可直接删除: %v", err)
	}
	st, _ := store.Open(home)
	if ok, _ := st.Exists("single/img", "v9"); ok {
		t.Error("镜像未被删除")
	}
}

// TestRmiKeepsOtherTags 删除一个 tag 不应影响同仓库的其他 tag。
func TestRmiKeepsOtherTags(t *testing.T) {
	home := t.TempDir()
	buildOne(t, home, "keep/img:v1")
	buildOne(t, home, "keep/img:v2")

	if _, err := runCLI(t, home, "rmi", "keep/img:v1"); err != nil {
		t.Fatalf("rmi: %v", err)
	}
	st, _ := store.Open(home)
	if ok, _ := st.Exists("keep/img", "v1"); ok {
		t.Error("v1 未被删除")
	}
	if ok, _ := st.Exists("keep/img", "v2"); !ok {
		t.Error("v2 被误删")
	}
}

// TestRmiRejectsImageInUse 镜像仍被容器引用时默认拒绝，-f 才强制。
func TestRmiRejectsImageInUse(t *testing.T) {
	home := t.TempDir()
	buildOne(t, home, "inuse/img:v1")

	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// 造一个引用该镜像的容器配置（不真跑容器，rmi 只看 config.json）。
	id, err := store.NewContainerID()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &store.ContainerConfig{
		ConfigVersion: 1,
		ID:            id,
		Name:          "holder",
		ImageRef:      "inuse/img:v1",
		// rmi 只读 config.json 判断引用关系，不校验 rootfs 是否真实存在。
		Rootfs:  st.ContainerDir(id) + "/rootfs",
		Restart: store.RestartNo,
		// Cmd 是 ContainerConfig 的必填项；本测试不真跑容器，
		// 只需配置能通过校验以便 rmi 读到 ImageRef。
		Cmd: []string{"/bin/true"},
	}
	if err := st.CreateContainer(cfg); err != nil {
		t.Fatalf("创建容器配置: %v", err)
	}

	if _, err := runCLI(t, home, "rmi", "inuse/img:v1"); err == nil {
		t.Fatal("镜像被容器引用时必须拒绝删除")
	} else if !errors.Is(err, store.ErrImageInUse) {
		t.Errorf("err = %v, want ErrImageInUse", err)
	}
	// -f 强制删除应当成功。
	if _, err := runCLI(t, home, "rmi", "-f", "inuse/img:v1"); err != nil {
		t.Fatalf("rmi -f: %v", err)
	}
	if ok, _ := st.Exists("inuse/img", "v1"); ok {
		t.Error("-f 后镜像仍存在")
	}
}

// TestRmiRequiresArg 不给参数时报用法错误。
func TestRmiRequiresArg(t *testing.T) {
	home := t.TempDir()
	if _, err := runCLI(t, home, "rmi"); err == nil {
		t.Fatal("缺少参数必须报错")
	}
}

// TestRmiRegisteredInHelp 回归：rmi 必须出现在 --help 里
// （原问题是它根本不存在）。
func TestRmiRegisteredInHelp(t *testing.T) {
	var buf bytes.Buffer
	root := NewRootCommand(&buf, &buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--help: %v", err)
	}
	if !strings.Contains(buf.String(), "rmi") {
		t.Errorf("--help 未列出 rmi:\n%s", buf.String())
	}
}
