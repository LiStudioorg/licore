// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LiStudioorg/licore/internal/image"
)

// 本文件是 docs/security-audit-v2.md H-1 的回归测试。
//
// 漏洞形态：extractEntry 只校验**条目名**（../、绝对路径），看不见
// **文件系统上已存在**的符号链接。归档只要"先放符号链接、再经由它写文件"
// 就能穿透到目标目录之外，以 root 写穿宿主。
//
// 这些用例必须覆盖全部三种穿透路径：相对符号链接、绝对符号链接、硬链接。

// entry 描述一个待写入 tar 的条目。
type entry struct {
	name     string
	typeflag byte
	linkname string
	content  string
}

// writeEntries 按给定顺序写 tar（顺序本身是攻击的一部分，不能用 map）。
func writeEntries(t *testing.T, path string, entries []entry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o644,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
		}
		if e.typeflag == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if e.typeflag == tar.TypeSymlink || e.typeflag == tar.TypeLink {
			hdr.Size = 0
		} else {
			hdr.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// setupVictim 造一个"rootfs 之外的受害文件"，返回 (base, victimPath)。
func setupVictim(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	victimDir := filepath.Join(base, "victim")
	if err := os.MkdirAll(victimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(victimDir, "secret")
	if err := os.WriteFile(victim, []byte("ORIGINAL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return base, victim
}

// assertNotEscaped 断言受害文件未被改写。
func assertNotEscaped(t *testing.T, victim string) {
	t.Helper()
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("读受害文件失败: %v", err)
	}
	if string(got) != "ORIGINAL\n" {
		t.Fatalf("!!! 逃逸：rootfs 之外的文件被覆盖为 %q", string(got))
	}
}

// TestExtractTarRejectsSymlinkTraversalRelative 相对符号链接穿透。
func TestExtractTarRejectsSymlinkTraversalRelative(t *testing.T) {
	base, victim := setupVictim(t)
	tarPath := filepath.Join(base, "evil.tar")
	writeEntries(t, tarPath, []entry{
		{name: "linkdir", typeflag: tar.TypeDir},
		{name: "linkdir/up", typeflag: tar.TypeSymlink, linkname: "../../victim"},
		{name: "linkdir/up/secret", typeflag: tar.TypeReg, content: "PWNED\n"},
	})

	dst := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	err := extractTar(tarPath, dst)
	if err == nil {
		t.Fatal("相对符号链接穿透必须被拒绝，实际未报错")
	}
	if !errors.Is(err, image.ErrUnsafePath) {
		t.Fatalf("应返回 ErrUnsafePath，实得: %v", err)
	}
	if !strings.Contains(err.Error(), "符号链接") {
		t.Errorf("错误信息应说明是符号链接问题，实得: %v", err)
	}
	assertNotEscaped(t, victim)
}

// TestExtractTarRejectsSymlinkTraversalAbsolute 绝对路径符号链接穿透。
func TestExtractTarRejectsSymlinkTraversalAbsolute(t *testing.T) {
	base, victim := setupVictim(t)
	victimDir := filepath.Dir(victim)

	tarPath := filepath.Join(base, "evil.tar")
	writeEntries(t, tarPath, []entry{
		{name: "x", typeflag: tar.TypeDir},
		{name: "x/y", typeflag: tar.TypeSymlink, linkname: victimDir},
		{name: "x/y/secret", typeflag: tar.TypeReg, content: "PWNED\n"},
	})

	dst := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	err := extractTar(tarPath, dst)
	if err == nil {
		t.Fatal("绝对符号链接穿透必须被拒绝，实际未报错")
	}
	if !errors.Is(err, image.ErrUnsafePath) {
		t.Fatalf("应返回 ErrUnsafePath，实得: %v", err)
	}
	assertNotEscaped(t, victim)
}

// TestExtractTarRejectsDeepSymlinkTraversal 深层：符号链接在中间层级。
func TestExtractTarRejectsDeepSymlinkTraversal(t *testing.T) {
	base, victim := setupVictim(t)
	tarPath := filepath.Join(base, "evil.tar")
	writeEntries(t, tarPath, []entry{
		{name: "a", typeflag: tar.TypeDir},
		{name: "a/b", typeflag: tar.TypeDir},
		{name: "a/b/up", typeflag: tar.TypeSymlink, linkname: "../../../victim"},
		{name: "a/b/up/secret", typeflag: tar.TypeReg, content: "PWNED\n"},
	})

	dst := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(tarPath, dst); err == nil {
		t.Fatal("深层符号链接穿透必须被拒绝")
	}
	assertNotEscaped(t, victim)
}

// TestExtractTarRejectsSymlinkEscapingMkdirAll MkdirAll 兜不住的情形：
// 父目录已由前面的条目真实建好，符号链接是后来替换进去的。
//
// 这条刻意绕开"File exists"那条意外路径——攻击者可以先用目录条目占位、
// 再删掉换成符号链接（tar 里同名条目后者覆盖前者），此时 MkdirAll 不再报错。
func TestExtractTarRejectsSymlinkEscapingMkdirAll(t *testing.T) {
	base, victim := setupVictim(t)
	tarPath := filepath.Join(base, "evil.tar")
	writeEntries(t, tarPath, []entry{
		// 先建真实目录 d
		{name: "d", typeflag: tar.TypeDir},
		// 再用符号链接条目覆盖它（tar 语义：后者胜）
		{name: "d", typeflag: tar.TypeSymlink, linkname: "../victim"},
		// 经由它写文件：此时父链上 d 已是符号链接
		{name: "d/secret", typeflag: tar.TypeReg, content: "PWNED\n"},
	})

	dst := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(tarPath, dst); err == nil {
		t.Fatal("被替换为符号链接的父目录必须被拒绝")
	}
	assertNotEscaped(t, victim)
}

// TestExtractTarRejectsHardlinkSourceTraversal 硬链接**源**路径穿越：
// linkname 指向 rootfs 之外，会把宿主敏感文件硬链接进 rootfs 并打包外泄。
func TestExtractTarRejectsHardlinkSourceTraversal(t *testing.T) {
	base, victim := setupVictim(t)
	tarPath := filepath.Join(base, "evil.tar")
	writeEntries(t, tarPath, []entry{
		// 硬链接源写绝对路径 —— 绝对路径必须先被 SafeArchivePath 拒绝
		{name: "leak", typeflag: tar.TypeLink, linkname: victim},
	})

	dst := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	err := extractTar(tarPath, dst)
	if err == nil {
		t.Fatal("硬链接源为绝对路径必须被拒绝")
	}
	if !errors.Is(err, image.ErrUnsafePath) {
		t.Fatalf("应返回 ErrUnsafePath，实得: %v", err)
	}
}

// TestExtractTarRejectsHardlinkSourceRelativeTraversal 硬链接源的相对上跳。
func TestExtractTarRejectsHardlinkSourceRelativeTraversal(t *testing.T) {
	base, victim := setupVictim(t)
	tarPath := filepath.Join(base, "evil.tar")
	writeEntries(t, tarPath, []entry{
		{name: "leak", typeflag: tar.TypeLink, linkname: "../../victim/secret"},
	})

	dst := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(tarPath, dst); err == nil {
		t.Fatal("硬链接源含 ../ 必须被拒绝")
	}
	assertNotEscaped(t, victim)
}

// TestExtractTarRejectsHardlinkThroughSymlinkedParent 硬链接**落点**经符号链接。
func TestExtractTarRejectsHardlinkThroughSymlinkedParent(t *testing.T) {
	base, victim := setupVictim(t)
	tarPath := filepath.Join(base, "evil.tar")
	writeEntries(t, tarPath, []entry{
		{name: "l", typeflag: tar.TypeSymlink, linkname: "../victim"},
		{name: "l/leak", typeflag: tar.TypeLink, linkname: "l/leak"},
	})

	dst := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(tarPath, dst); err == nil {
		t.Fatal("硬链接落点经符号链接必须被拒绝")
	}
	assertNotEscaped(t, victim)
}

// TestExtractTarRejectsFileThroughSymlinkedParent 普通文件经符号链接父目录。
func TestExtractTarRejectsFileThroughSymlinkedParent(t *testing.T) {
	base, victim := setupVictim(t)
	tarPath := filepath.Join(base, "evil.tar")
	writeEntries(t, tarPath, []entry{
		{name: "p", typeflag: tar.TypeSymlink, linkname: "../victim"},
		{name: "p/secret", typeflag: tar.TypeReg, content: "PWNED\n"},
	})

	dst := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(tarPath, dst); err == nil {
		t.Fatal("文件经符号链接父目录必须被拒绝")
	}
	assertNotEscaped(t, victim)
}

// TestExtractTarRejectsDirThroughSymlinkedParent 目录条目经符号链接父目录
// （finalMustDir=true 路径）。
func TestExtractTarRejectsDirThroughSymlinkedParent(t *testing.T) {
	base, victim := setupVictim(t)
	tarPath := filepath.Join(base, "evil.tar")
	writeEntries(t, tarPath, []entry{
		{name: "p", typeflag: tar.TypeSymlink, linkname: "../victim"},
		{name: "p/sub", typeflag: tar.TypeDir},
	})

	dst := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(tarPath, dst); err == nil {
		t.Fatal("目录条目经符号链接父目录必须被拒绝")
	}
	assertNotEscaped(t, victim)
}

// TestExtractTarStillAcceptsLegitArchives 反向约束：合法归档必须照常解压，
// 证明修复没有把功能一起挡掉（只堵穿透，不误伤正常镜像）。
func TestExtractTarStillAcceptsLegitArchives(t *testing.T) {
	base := t.TempDir()
	tarPath := filepath.Join(base, "good.tar")
	writeEntries(t, tarPath, []entry{
		{name: "bin", typeflag: tar.TypeDir},
		{name: "bin/app", typeflag: tar.TypeReg, content: "#!/bin/sh\n"},
		{name: "etc", typeflag: tar.TypeDir},
		{name: "etc/conf", typeflag: tar.TypeReg, content: "k=v\n"},
		// 相对符号链接指向 rootfs **内部**：完全合法，必须放行
		{name: "bin/link", typeflag: tar.TypeSymlink, linkname: "app"},
		{name: "bin/../bin/app2", typeflag: tar.TypeReg, content: "x\n"},
	})

	dst := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(tarPath, dst); err != nil {
		t.Fatalf("合法归档不应被拒绝: %v", err)
	}
	// 校验内容确实落地
	b, err := os.ReadFile(filepath.Join(dst, "bin", "app"))
	if err != nil {
		t.Fatalf("合法文件未落地: %v", err)
	}
	if string(b) != "#!/bin/sh\n" {
		t.Fatalf("文件内容不符: %q", string(b))
	}
	// 内部符号链接应被保留为链接
	fi, err := os.Lstat(filepath.Join(dst, "bin", "link"))
	if err != nil {
		t.Fatalf("符号链接未创建: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("bin/link 应为符号链接")
	}
}

// TestExtractTarAcceptsOverwritingExistingSymlink 末级是已存在符号链接时，
// 覆盖替换是安全的（先删后写、不跟随），必须放行。
func TestExtractTarAcceptsOverwritingExistingSymlink(t *testing.T) {
	base, victim := setupVictim(t)
	dst := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	// 预先在 rootfs 内放一个指向受害文件的符号链接
	if err := os.Symlink(victim, filepath.Join(dst, "sneaky")); err != nil {
		t.Fatal(err)
	}

	tarPath := filepath.Join(base, "ok.tar")
	writeEntries(t, tarPath, []entry{
		{name: "sneaky", typeflag: tar.TypeReg, content: "REPLACED\n"},
	})
	if err := extractTar(tarPath, dst); err != nil {
		t.Fatalf("覆盖末级符号链接本身应当允许: %v", err)
	}
	// 关键：受害文件不能被改动（因为是把链接本身换掉，不是跟随写入）
	assertNotEscaped(t, victim)
	// 且 rootfs 内该路径变成了普通文件
	fi, err := os.Lstat(filepath.Join(dst, "sneaky"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("末级符号链接应被替换为普通文件")
	}
}
