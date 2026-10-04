// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LiStudioorg/licore/internal/image"
	"github.com/LiStudioorg/licore/internal/store"
)

// -------- 纯函数：resolveOutputArg --------

func TestResolveOutputArg(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		flag    string
		want    string
		wantErr string
	}{
		{"位置参数", []string{"img:1", "out.licore"}, "", "out.licore", ""},
		{"仅 -o", []string{"img:1"}, "out.licore", "out.licore", ""},
		{"两者一致", []string{"img:1", "out.licore"}, "out.licore", "out.licore", ""},
		{"两者冲突", []string{"img:1", "a.licore"}, "b.licore", "", "冲突"},
		{"都没有", []string{"img:1"}, "", "", "必须指定输出文件"},
		// 清理后相同但字面不同：仍按字面比较，报冲突（用户自己也不确定）。
		{"清理后相同的冲突", []string{"img:1", "./out.licore"}, "out.licore", "", "冲突"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveOutputArg(tc.args, tc.flag, "save")
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("期望错误含 %q，却成功返回 %q", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v，期望含 %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外失败: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// -------- 端到端：save / export --------

// TestSavePositionalOutputArg 回归：`save IMAGE OUT` 两参数写法必须可用
// （原实现 Args=ExactArgs(1)，报 "accepts 1 arg(s), received 2"）。
func TestSavePositionalOutputArg(t *testing.T) {
	home := t.TempDir()
	buildOne(t, home, "savetest/img:v1")
	outFile := filepath.Join(t.TempDir(), "out.licore")

	stdout, err := runCLI(t, home, "save", "savetest/img:v1", outFile)
	if err != nil {
		t.Fatalf("save IMAGE OUT: %v", err)
	}
	if !strings.Contains(stdout, outFile) {
		t.Errorf("应回显输出路径，得到 %q", stdout)
	}
	fi, err := os.Stat(outFile)
	if err != nil {
		t.Fatalf("输出文件不存在: %v", err)
	}
	if fi.Size() == 0 {
		t.Error("输出文件为空")
	}
	// 落盘内容必须是能被解析器接受的合法镜像。
	loaded, err := image.OpenFile(outFile)
	if err != nil {
		t.Fatalf("产物不是合法 .licore: %v", err)
	}
	if loaded.Manifest.Ref() != "savetest/img:v1" {
		t.Errorf("ref = %q, want savetest/img:v1", loaded.Manifest.Ref())
	}
}

// TestSaveOutputFlagStillWorks -o/--output 写法不能被回归破坏。
func TestSaveOutputFlagStillWorks(t *testing.T) {
	home := t.TempDir()
	buildOne(t, home, "savetest/img:v1")
	outFile := filepath.Join(t.TempDir(), "flag.licore")

	if _, err := runCLI(t, home, "save", "savetest/img:v1", "-o", outFile); err != nil {
		t.Fatalf("save -o: %v", err)
	}
	if _, err := os.Stat(outFile); err != nil {
		t.Fatalf("输出文件不存在: %v", err)
	}
}

// TestSaveOutputConflictPositionalVsFlag 位置参数与 -o 不一致时必须报错，
// 而不是静默选一个写。
func TestSaveOutputConflictPositionalVsFlag(t *testing.T) {
	home := t.TempDir()
	buildOne(t, home, "savetest/img:v1")
	dir := t.TempDir()
	a := filepath.Join(dir, "a.licore")
	b := filepath.Join(dir, "b.licore")

	_, err := runCLI(t, home, "save", "savetest/img:v1", a, "-o", b)
	if err == nil {
		t.Fatal("路径冲突必须报错")
	}
	if !strings.Contains(err.Error(), "冲突") {
		t.Errorf("错误应说明冲突: %v", err)
	}
	// 冲突时不应写出任何文件。
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("冲突时不应写出 %s", p)
		}
	}
}

// TestSaveNoOutputArgIsUsageError 既不给位置参数也不给 -o 时报用法错误。
func TestSaveNoOutputArgIsUsageError(t *testing.T) {
	home := t.TempDir()
	buildOne(t, home, "savetest/img:v1")
	_, err := runCLI(t, home, "save", "savetest/img:v1")
	if err == nil {
		t.Fatal("缺输出路径必须报错")
	}
	if !strings.Contains(err.Error(), "必须指定输出文件") {
		t.Errorf("错误应说明缺少输出路径: %v", err)
	}
}

// TestSaveTooManyArgsRejected 三个以上参数仍然拒绝。
func TestSaveTooManyArgsRejected(t *testing.T) {
	home := t.TempDir()
	if _, err := runCLI(t, home, "save", "a:1", "b.licore", "c.licore"); err == nil {
		t.Fatal("超过两个位置参数必须拒绝")
	}
}

// TestSavePositionalRoundTrip 位置参数导出的文件能被 load 导入回来。
func TestSavePositionalRoundTrip(t *testing.T) {
	src := t.TempDir()
	buildOne(t, src, "roundtrip/img:v1")
	outFile := filepath.Join(t.TempDir(), "rt.licore")
	if _, err := runCLI(t, src, "save", "roundtrip/img:v1", outFile); err != nil {
		t.Fatalf("save: %v", err)
	}

	dst := t.TempDir()
	if _, err := runCLI(t, dst, "load", "-i", outFile); err != nil {
		t.Fatalf("load: %v", err)
	}
	st, err := store.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.Exists("roundtrip/img", "v1"); !ok {
		t.Error("load 后镜像未落地")
	}
}

// TestExportPositionalMatchesSave export 与 save 语法一致。
func TestExportPositionalMatchesSave(t *testing.T) {
	home := t.TempDir()
	buildOne(t, home, "exptest/img:v1")
	outFile := filepath.Join(t.TempDir(), "exp.licore")

	if _, err := runCLI(t, home, "export", "exptest/img:v1", outFile); err != nil {
		t.Fatalf("export IMAGE OUT: %v", err)
	}
	if _, err := os.Stat(outFile); err != nil {
		t.Fatalf("输出文件不存在: %v", err)
	}
	// 冲突检测在 export 上同样生效。
	if _, err := runCLI(t, home, "export", "exptest/img:v1",
		filepath.Join(t.TempDir(), "x.licore"), "-o", filepath.Join(t.TempDir(), "y.licore")); err == nil {
		t.Fatal("export 的路径冲突也必须报错")
	}
}
