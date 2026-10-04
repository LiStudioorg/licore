// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/licore/internal/build"
	"github.com/LiStudioorg/licore/internal/dev"
	"github.com/LiStudioorg/licore/internal/image"
	"github.com/LiStudioorg/licore/internal/store"
)

// newBuildCommand 实现 `licore build`：解析 Boxfile → 真正调用 build.Build()
// 构造 .licore 镜像 → 自动 pull 导入本地 store。
//
//	licore build -t demo:v1 .
//	licore build -f Boxfile -t demo:v1 --context ./src
//
// 构建上下文**必须显式给出**（位置参数或 --context 二选一），理由见
// resolveBuildContext 的注释：隐式默认 cwd 是"最坏失败模式"。
func newBuildCommand(out io.Writer) *cobra.Command {
	var (
		file       string
		tag        string
		contextDir string
		dataDir    string
		noCache    bool
		slim       bool
		arch       string
		osName     string
	)
	cmd := &cobra.Command{
		Use:   "build [--file Boxfile] [--tag NAME:VERSION] (--context DIR | <context>)",
		Short: "根据 Boxfile 构建 .licore 镜像并入本地 store",
		Long: "解析 Boxfile（FROM/COPY/ENV/WORKDIR/ENTRYPOINT/CMD/EXPOSE/VOLUME/" +
			"LABEL/USER/ARG），对基础镜像或 scratch 执行指令，构造一个 .licore " +
			"镜像并自动 licore pull 导入本地 store。\n" +
			"构建上下文（COPY 源目录）必须显式指定：末尾位置参数或 --context，" +
			"想用当前目录就传 \".\"。\n" +
			"--arch/--os 覆盖产物的平台字段，默认跟随宿主，交叉构建 arm64 等镜像时用得上。\n" +
			"注意：COPY 的源路径必须逐层列出（如 COPY etc /etc），**不支持 COPY . /**；" +
			"要复制整个上下文请按目录逐个写，详见 docs/image-spec.md。",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctxDir, err := resolveBuildContext(contextDir, args)
			if err != nil {
				return err
			}
			contextDir = ctxDir
			// 平台字段校验：留空跟随宿主（向后兼容），显式给出则必须落在
			// 镜像规范 3.1 的枚举内——否则构建会"成功"产出一个 pull 立刻
			// 拒绝的坏包，错误必须在这里就暴露。
			if arch != "" && !image.ValidArch(arch) {
				return fmt.Errorf("build: --arch %q 不受支持，可选值：%s: %w",
					arch, strings.Join(image.SupportedArches(), " / "), build.ErrBadInstruction)
			}
			if osName != "" && !image.ValidOS(osName) {
				return fmt.Errorf("build: --os %q 不受支持，可选值：%s: %w",
					osName, strings.Join(image.SupportedOSes(), " / "), build.ErrBadInstruction)
			}
			// Boxfile 路径：--file 优先，其次 <context>/Boxfile|boxfile。
			if file == "" {
				cand := []string{filepath.Join(contextDir, "Boxfile"), filepath.Join(contextDir, "boxfile")}
				found := ""
				for _, c := range cand {
					if _, err := os.Stat(c); err == nil {
						found = c
						break
					}
				}
				if found == "" {
					return fmt.Errorf("build: 构建上下文 %s 下未找到 Boxfile/boxfile，用 --file 指定: %w",
						contextDir, build.ErrNoContext)
				}
				file = found
			}
			bf, err := build.ParseBoxfileFile(file)
			if err != nil {
				return err
			}
			// 校验构建上下文可访问。
			if err := build.CheckContext(contextDir); err != nil {
				return fmt.Errorf("build: 构建上下文: %w", err)
			}

			st, err := store.Open(dataDir)
			if err != nil {
				return err
			}

			// 定位基础镜像：FROM scratch 或空 → scratch；否则须本地已导入。
			basePath := ""
			if bf.From != "" && !strings.EqualFold(bf.From, "scratch") {
				basePath, err = resolveBuildBase(st, bf.From)
				if err != nil {
					return err
				}
			}

			// 产出临时 .licore（Build 内部用 base/OutPath 派生 name:version，
			// 我们把 -t 传给它写出正确清单；导入由 ImportImage 决定落位 ref）。
			outTmp, err := os.CreateTemp("", "licore-build-*.licore")
			if err != nil {
				return fmt.Errorf("build: 创建输出临时文件: %w", err)
			}
			outPath := outTmp.Name()
			_ = outTmp.Close()
			defer os.Remove(outPath)

			if noCache {
				// 无缓存语义：构建始终从基础镜像重建，不加可复用层。
				slog.Debug("build: --no-cache 提示（LiCore 构建当前始终重打追加层）")
			}
			_ = slim

			name, version := splitBuildTag(tag)
			res, err := build.Build(cmd.Context(), &build.Options{
				Boxfile:      bf,
				ContextDir:   contextDir,
				BaseImage:    basePath,
				OutPath:      outPath,
				Name:         name,
				Version:      version,
				Architecture: arch,
				OS:           osName,
			})
			if err != nil {
				return err
			}

			// 自动导入本地 store（等价 licore pull）。
			dstRef := tag
			loaded, err := ImportImage(st, res.Path, dstRef, ImportOptions{
				Force:             true,       // 覆盖重构建同名
				AllowArchMismatch: arch != "", // --arch 交叉构建：产物架构与宿主不同是预期内的
			})
			if err != nil {
				return fmt.Errorf("build: 导入产物失败: %w", err)
			}
			fmt.Fprintf(out, "已构建并导入 %s:%s（%s，%d 层，%.1f KiB）\n",
				loaded.Manifest.Name, loaded.Manifest.Version, res.Path, res.LayerCount, float64(res.Bytes)/1024)
			fmt.Fprintf(out, "落地目录：%s\n", st.ImageDir(loaded.Manifest.Name, loaded.Manifest.Version))
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "Boxfile 路径（默认 <context>/Boxfile 或 ./Boxfile）")
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "镜像引用 NAME:VERSION（默认由 Boxfile FROM 或文件名派生）")
	cmd.Flags().StringVarP(&contextDir, "context", "", "", "构建上下文目录（COPY 源相对它解析；与末尾位置参数二选一）")
	cmd.Flags().StringVar(&arch, "arch", "", "产物 architecture（"+strings.Join(image.SupportedArches(), "/")+"；默认跟随宿主 GOARCH）")
	cmd.Flags().StringVar(&osName, "os", "", "产物 os（"+strings.Join(image.SupportedOSes(), "/")+"；默认跟随宿主 GOOS）")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "数据目录（默认 $LICORE_HOME 或 ~/.licore）")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "禁用构建缓存（LiCore 始终从基础镜像重建）")
	cmd.Flags().BoolVar(&slim, "slim", false, "构建精简镜像（当前与常规构建同）")
	return cmd
}

// resolveBuildContext 决定构建上下文目录，规则（缺失必报错、冲突必拒绝）：
//
//   - 只给位置参数 → 用它；只给 --context → 用它；
//   - 两者都给了：Clean 后字符串相等 → 放行；不等 → 报错拒绝，绝不猜；
//   - 两者都没给 → 报错，**不默认 "."**。
//
// 为什么不默认 cwd：上下文目录决定 COPY 把哪些文件打进镜像。旧实现里
// --context 的 flag 默认值是 "."，而判空分支 `if contextDir == ""` 因此
// 永远不成立——位置参数被**静默忽略**，COPY 从 cwd 解析。这意味着"以为
// 在构建 A 目录、实际打了 cwd"：构建出错误镜像还是小事，cwd 里恰好有
// 私钥/配置而 Boxfile 写了 COPY . 时，敏感文件会被静默打进镜像分发出去，
// 且用户全程无任何提示。宁可让用户多敲一个 "."，也不做静默猜测。
func resolveBuildContext(flagCtx string, args []string) (string, error) {
	var pos string
	if len(args) == 1 {
		pos = args[0]
	}
	switch {
	case flagCtx != "" && pos != "":
		if filepath.Clean(flagCtx) == filepath.Clean(pos) {
			return flagCtx, nil
		}
		return "", fmt.Errorf("build: 位置参数上下文 %q 与 --context %q 冲突，二者只能选一个（或传相同路径）: %w",
			pos, flagCtx, build.ErrNoContext)
	case flagCtx != "":
		return flagCtx, nil
	case pos != "":
		return pos, nil
	default:
		return "", fmt.Errorf("build: 必须显式指定构建上下文（COPY 的源目录），当前目录就传 \".\"，例如：licore build -t NAME:VERSION <context>: %w",
			build.ErrNoContext)
	}
}

// resolveBuildBase 把 FROM 引用解析为本地已导入镜像的 source.licore 路径。
func resolveBuildBase(st *store.Store, ref string) (string, error) {
	name, version, ok := splitRefC(ref)
	if !ok {
		return "", fmt.Errorf("build: 基础镜像引用 %q 应为 NAME:VERSION（先 licore pull）", ref)
	}
	exists, err := st.Exists(name, version)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("build: 基础镜像 %s 未导入本地，请先 licore pull %s.licore", ref, name+"_"+version)
	}
	return filepath.Join(st.ImageDir(name, version), "source.licore"), nil
}

// splitBuildTag 拆分 "name:version"；空返回 ("","")。
func splitBuildTag(tag string) (string, string) {
	if tag == "" {
		return "", ""
	}
	i := strings.LastIndex(tag, ":")
	if i <= 0 || i == len(tag)-1 {
		return "", ""
	}
	return tag[:i], tag[i+1:]
}

// splitRefC 拆分 "name:version"。
func splitRefC(ref string) (string, string, bool) {
	i := strings.LastIndex(ref, ":")
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// newDevCommand 实现 `licore dev`：文件热重载。
// 监听路径，文件变化时去抖并输出批次（真实重建/重启由运行时编排）。
func newDevCommand(out io.Writer) *cobra.Command {
	var watch []string
	var ignore []string
	var once bool
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "开发模式：文件热重载监听",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(watch) == 0 {
				watch = []string{"."}
			}
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			spec := dev.WatchSpec{Roots: watch, Ignore: ignore, Debounce: 300 * time.Millisecond}
			fmt.Fprintf(out, "监听 %v（Ctrl+C 退出）\n", watch)
			for {
				changed, err := dev.WaitForChange(ctx, spec)
				if err != nil {
					return err
				}
				for _, p := range changed {
					fmt.Fprintf(out, "变更: %s\n", p)
				}
				if once {
					return nil
				}
			}
		},
	}
	cmd.Flags().StringArrayVar(&watch, "watch", nil, "要监听的目录，可重复（默认 .）")
	cmd.Flags().StringArrayVar(&ignore, "ignore", nil, "忽略模式，可重复")
	cmd.Flags().BoolVar(&once, "once", false, "检测到一次变更后退出")
	return cmd
}
