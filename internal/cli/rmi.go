// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/licore/internal/store"
)

// newRmiCommand 实现 `licore rmi <image>...`：删除本地镜像。
//
//	licore rmi alpine:3.20.3-arm64
//	licore rmi -f alpine:test
//
// 语义与 `licore rm`（删容器）保持一致的家族风格：默认拒绝删除仍被容器
// 引用的镜像，-f 才强制；不存在的镜像报明确错误而不是静默成功。
func newRmiCommand(out io.Writer) *cobra.Command {
	var (
		dataDir string
		force   bool
	)
	cmd := &cobra.Command{
		Use:   "rmi [flags] <镜像[:版本]>...",
		Short: "删除本地镜像",
		Long: "删除本地 store 中的镜像（<数据目录>/images/<name>/<version>/）。\n" +
			"镜像仍被某个容器引用时默认拒绝删除，用 -f/--force 强制。\n" +
			"只删该引用自己的目录；层缓存跨镜像共享，不随镜像删除回收。",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(dataDir)
			if err != nil {
				return err
			}
			// 预先收集容器引用，避免逐个镜像重复扫描容器目录。
			inUse, err := imagesInUse(st)
			if err != nil {
				return err
			}
			var failed []error
			for _, arg := range args {
				name, version, err := resolveImageRef(st, arg)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "licore rmi: %s: %v\n", arg, err)
					failed = append(failed, err)
					continue
				}
				ref := name + ":" + version
				if err := st.RemoveImage(name, version, force, inUse[ref]); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "licore rmi: %v\n", err)
					failed = append(failed, err)
					continue
				}
				fmt.Fprintf(out, "已删除镜像 %s:%s\n", name, version)
			}
			switch len(failed) {
			case 0:
				return nil
			case 1:
				// 单个失败直接透传原错误，保住 ErrImageNotFound / ErrImageInUse
				// 这些哨兵，调用方（脚本、上层 Go 代码）才能用 errors.Is 判定。
				return failed[0]
			default:
				// 批量删除时逐个报告：部分失败要让退出码非零，但不回滚已成功的。
				return fmt.Errorf("删除 %d 个镜像失败: %w", len(failed), errors.Join(failed...))
			}
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "数据目录（默认 $LICORE_HOME 或 ~/.licore）")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "强制删除（即使仍被容器引用）")
	return cmd
}

// resolveImageRef 把用户给的引用解析成 (name, version)。
// 只写 name 时（如 `licore rmi alpine`）：恰好一个 tag 就用它，多个 tag 则报错
// 要求写全，避免"删了哪个"这件事靠猜。
func resolveImageRef(st *store.Store, ref string) (string, string, error) {
	name, version := splitImageRef(ref)
	if version != "" {
		ok, err := st.Exists(name, version)
		if err != nil {
			return "", "", err
		}
		if !ok {
			return "", "", fmt.Errorf("镜像 %s: %w", ref, store.ErrImageNotFound)
		}
		return name, version, nil
	}
	infos, err := st.ListImages()
	if err != nil {
		return "", "", err
	}
	var versions []string
	for _, info := range infos {
		// ImageInfo 内嵌 State，引用以 "name:version" 形式存放在 Ref 里。
		n, v := splitImageRef(info.Ref)
		if n == name && v != "" {
			versions = append(versions, v)
		}
	}
	switch len(versions) {
	case 0:
		return "", "", fmt.Errorf("镜像 %s: %w", ref, store.ErrImageNotFound)
	case 1:
		return name, versions[0], nil
	default:
		return "", "", fmt.Errorf("镜像 %s 有 %d 个版本，请指定要删除的版本: %v: %w",
			name, len(versions), versions, store.ErrImageNotFound)
	}
}

// splitImageRef 把 ref 拆成 name/version。没有 ":" 时 version 为空。
// 与 build 的 splitBuildTag 同构，但语义是"读取已存在引用"而非"生成新引用"，
// 因此不填充默认 tag。
func splitImageRef(ref string) (string, string) {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == ':' {
			return ref[:i], ref[i+1:]
		}
	}
	return ref, ""
}

// imagesInUse 返回 "name:version" → 是否仍被容器引用的集合。
// 容器目录不可读时返回空集合：容器状态损坏不应让镜像完全无法删除，
// 强制安全性由 -f 的显式意图承担。
func imagesInUse(st *store.Store) (map[string]bool, error) {
	cfgs, err := st.ListContainers()
	if err != nil {
		return map[string]bool{}, nil
	}
	inUse := make(map[string]bool, len(cfgs))
	for _, c := range cfgs {
		if c.ImageRef != "" {
			inUse[c.ImageRef] = true
		}
	}
	return inUse, nil
}
