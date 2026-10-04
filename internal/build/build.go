// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

// Package build 实现 `licore build` 的两半：
//
//   - Boxfile 解析：把面向用户的构建描述（FROM / COPY / ENV / WORKDIR /
//     ENTRYPOINT / CMD / EXPOSE / VOLUME / LABEL / USER / ARG）严格解析成
//     结构化的 *Boxfile（见 boxfile.go）；
//   - .licore 镜像构造：把解析结果施加到临时 rootfs 上，产出规范
//     docs/image-spec.md 定义的 .licore 文件——外层未压缩 tar，内含
//     index.json、基础镜像原样搬过来的层，以及一个追加的新层。
//
// 本包只做"Boxfile → .licore 文件"的纯函数式转换：不读写 ~/.licore 数据目录，
// 不启动容器，落地存储由调用方（internal/cli / internal/store）负责。
// 产物在返回前会用 image.OpenFile 自检，任何摘要或大小不符都在构建期暴露。
package build

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/LiStudioorg/licore/internal/image"
)

// 产物构造相关的固定取值。
const (
	// newLayerName 是追加层的固定文件名主体（序号由 applyOrder 决定）。
	newLayerName = "layer"
	// defFileMode 是新建目录与回填文件的默认权限。
	defDirMode  = 0o755
	defFileMode = 0o644
	// maxBuildLayerBytes 是追加层解压前的体积上限，防御解压炸弹。
	// 显式标注 int64：4 GiB 超出 32 位平台（linux/386、linux/arm）的 int 范围，
	// 无类型常量会在这些平台编译失败。比较对象 fi.Size() 本就是 int64。
	maxBuildLayerBytes int64 = 1 << 32
)

// Options 是一次构建的全部输入。
type Options struct {
	// ContextDir 是构建上下文根目录，COPY 的源路径相对它解析。可留空，
	// 但此时 Boxfile 不允许含 COPY 指令。
	ContextDir string
	// Boxfile 是已解析的构建描述，必填。
	Boxfile *Boxfile
	// BaseImage 是基础镜像 .licore 文件的本地路径，留空表示 scratch 构建。
	BaseImage string
	// OutPath 是产出的 .licore 路径；父目录必须已存在（本函数不做 MkdirAll，
	// 免得把拼错的路径变成一个空目录）。
	OutPath string
	// TempDir 是临时 rootfs 的父目录（存放 MkdirTemp 出来的构建工作区）。
	// 留空则取 OutPath 的父目录。
	TempDir string
	// Args 是 ARG 覆盖项，仅用于 config 的 annotations 记录；Boxfile 的
	// ${NAME} 展开已在解析期用 ARG 默认值完成（见 boxfile.go）。
	Args map[string]string
	// Architecture / OS 覆盖产物的平台字段，留空取 runtime.GOARCH / runtime.GOOS。
	Architecture string
	OS           string
	// Name / Version 覆盖写入 index.json 的镜像引用（对应 `licore build -t name:version`）。
	// 为空时退化为基础镜像引用或 OutPath 文件名派生（见 imageRefOf）。
	Name    string
	Version string
	// Labels 是附加到 config.labels 的标签，与 Boxfile 的 LABEL 合并，
	// 同名时以本字段为准。
	Labels map[string]string
}

// Result 是一次构建的产出摘要。
type Result struct {
	// Path 是产出的 .licore 文件路径。
	Path string
	// Bytes 是产出文件的字节数。
	Bytes int64
	// LayerDigest 是追加层的 sha256:<64 hex> 摘要（tar.gz 原始字节）。
	LayerDigest string
	// LayerCount 是产物总层数（基础镜像层数 + 1）。
	LayerCount int
	// Name / Version 是写入 index.json 的镜像引用。
	Name string
	// Version 是写入 index.json 的版本号。
	Version string
	// Skipped 是被跳过的非常规文件数（socket / 设备节点 / FIFO）。
	Skipped int
}

// Build 把一份解析好的 Boxfile 构建成新的 .licore 文件。
//
// 流程：必要时用 image.OpenFile 打开基础镜像 → 把基础镜像的层按
// applyOrder 解进临时 rootfs → 逐条施加指令（COPY 落文件、ENV / WORKDIR /
// ENTRYPOINT / CMD / EXPOSE / VOLUME / LABEL / USER 落 config）→ 把 rootfs
// 打成一个 tar.gz 追加层 → 把基础镜像的层字节原样拷进新归档 → 写
// index.json 与 config blob → 用 image.OpenFile 自检产物。
//
// 安全：COPY 源路径经 filepath.EvalSymlinks 后必须仍在 ContextDir 内；
// 目标路径必须落在 rootfs 内且不经由符号链接；setuid / setgid / sticky 位
// 一律剥离；socket / 设备节点 / FIFO 跳过并记 slog.Warn。
func Build(ctx context.Context, opts *Options) (*Result, error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("构建已取消: %w", ctx.Err())
		default:
		}
		break
	}

	// 基础镜像的层按 applyOrder 序，scratch 构建时为空。
	base := &baseImage{}
	if opts.BaseImage != "" {
		loaded, err := image.OpenFile(opts.BaseImage)
		if err != nil {
			return nil, fmt.Errorf("打开基础镜像 %s: %w", opts.BaseImage, err)
		}
		base.loaded = loaded
		base.layers = append([]image.Layer(nil), loaded.Manifest.Layers...)
		sort.Slice(base.layers, func(i, j int) bool { return base.layers[i].ApplyOrder < base.layers[j].ApplyOrder })
	}
	if len(base.layers)+1 > image.MaxLayers {
		return nil, fmt.Errorf("基础镜像 %d 层 + 1 层追加超过上限 %d: %w",
			len(base.layers), image.MaxLayers, ErrBadInstruction)
	}

	name, version, err := imageRefOf(opts, base)
	if err != nil {
		return nil, err
	}

	workRoot, err := buildWorkRoot(opts)
	if err != nil {
		return nil, err
	}
	tmpDir, err := os.MkdirTemp(workRoot, ".licore-build-*")
	if err != nil {
		return nil, fmt.Errorf("创建构建临时目录: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	rootfs := filepath.Join(tmpDir, "rootfs")
	if err := os.Mkdir(rootfs, defDirMode); err != nil {
		return nil, fmt.Errorf("创建 rootfs 目录: %w", err)
	}

	// 1) 基础层铺进临时 rootfs（后层覆盖前层），与 storage.MergeLayers 语义一致。
	baseCount := len(base.layers)
	for i, la := range base.layers {
		layerTmp := filepath.Join(tmpDir, fmt.Sprintf("base-%06d.tar.gz", i+1))
		if err := base.loaded.ExtractFile(la.Path, layerTmp); err != nil {
			return nil, fmt.Errorf("取出基础层 %s: %w", la.Path, err)
		}
		if err := unpackLayer(layerTmp, rootfs); err != nil {
			return nil, fmt.Errorf("解包基础层 %s: %w", la.Path, err)
		}
		_ = os.Remove(layerTmp)
	}

	// 2) config 的初始状态 = 基础镜像 config，scratch 构建时为零值。
	cfg := configOf(opts, base)

	// 3) 逐条施加指令。
	res := &Result{Path: opts.OutPath, Name: name, Version: version}
	if err := applyInstructions(ctx, opts, rootfs, cfg, res); err != nil {
		return nil, err
	}

	// 4) rootfs → 追加层 tar.gz，摘要与大小边写边算。
	layerPath := filepath.Join(tmpDir, fmt.Sprintf("%06d.%s.tar.gz", baseCount+1, newLayerName))
	layerDigest, layerSize, err := writeLayer(rootfs, layerPath)
	if err != nil {
		return nil, err
	}

	// 5) 组装产物：基础层原样搬运 + 追加层 + index.json + config blob。
	cfgBytes, cfgDigest, err := marshalConfig(cfg)
	if err != nil {
		return nil, err
	}
	manifest := &image.Manifest{
		MediaType:     image.MediaTypeManifest,
		SpecVersion:   image.SpecVersionV1,
		SchemaVersion: image.SchemaVersionV1,
		Architecture:  archOf(opts),
		OS:            osOf(opts),
		Created:       time.Now().UTC().Format(time.RFC3339),
		Name:          name,
		Version:       version,
		Config:        image.ConfigRef{Digest: cfgDigest, SizeBytes: int64(len(cfgBytes))},
		Annotations:   buildAnnotations(opts),
	}
	for i, la := range base.layers {
		manifest.Layers = append(manifest.Layers, image.Layer{
			Path:       fmt.Sprintf("layers/%06d.%s", i+1, layerBaseName(la.Path)),
			Digest:     la.Digest,
			SizeBytes:  la.SizeBytes,
			ApplyOrder: i + 1,
		})
	}
	manifest.Layers = append(manifest.Layers, image.Layer{
		Path:       fmt.Sprintf("layers/%06d.%s.tar.gz", baseCount+1, newLayerName),
		Digest:     layerDigest,
		SizeBytes:  layerSize,
		ApplyOrder: baseCount + 1,
	})
	if err := manifest.Validate(); err != nil {
		return nil, fmt.Errorf("构造的 index.json 非法: %w", err)
	}
	indexBytes, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("序列化 index.json: %w", err)
	}
	if int64(len(indexBytes)) > image.MaxIndexBytes {
		return nil, fmt.Errorf("index.json %d 字节超过上限: %w", len(indexBytes), ErrBadInstruction)
	}

	out, err := os.OpenFile(opts.OutPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, defFileMode)
	if err != nil {
		return nil, fmt.Errorf("创建输出文件 %s: %w", opts.OutPath, err)
	}
	tw := tar.NewWriter(out)
	writeErr := func() error {
		if err := writeTarBytes(tw, image.IndexName, indexBytes); err != nil {
			return err
		}
		for i, la := range base.layers {
			newPath := manifest.Layers[i].Path
			srcTmp := filepath.Join(tmpDir, fmt.Sprintf("carry-%06d", i+1))
			if err := base.loaded.ExtractFile(la.Path, srcTmp); err != nil {
				return fmt.Errorf("取出基础层 %s: %w", la.Path, err)
			}
			if err := copyTarFile(tw, newPath, srcTmp); err != nil {
				return fmt.Errorf("搬运基础层 %s → %s: %w", la.Path, newPath, err)
			}
			_ = os.Remove(srcTmp)
		}
		if err := copyTarFile(tw, manifest.Layers[baseCount].Path, layerPath); err != nil {
			return fmt.Errorf("写入追加层: %w", err)
		}
		return writeTarBytes(tw, configBlobName(cfgDigest), cfgBytes)
	}()
	if cerr := tw.Close(); writeErr == nil {
		writeErr = cerr
	}
	if writeErr == nil {
		writeErr = out.Sync()
	}
	if cerr := out.Close(); writeErr == nil {
		writeErr = cerr
	}
	if writeErr != nil {
		_ = os.Remove(opts.OutPath)
		return nil, fmt.Errorf("写出镜像 %s: %w", opts.OutPath, writeErr)
	}

	fi, err := os.Stat(opts.OutPath)
	if err != nil {
		return nil, fmt.Errorf("统计产物 %s: %w", opts.OutPath, err)
	}
	res.Path = opts.OutPath
	res.Bytes = fi.Size()
	res.LayerDigest = layerDigest
	res.LayerCount = len(manifest.Layers)

	// 6) 自检：产物必须能被自家解析器打开（清单类 / 结构类 / config 校验）。
	verify, err := image.OpenFile(opts.OutPath)
	if err != nil {
		return nil, fmt.Errorf("自检产物 %s 失败: %w", opts.OutPath, err)
	}
	if verify.Manifest.Config.Digest != cfgDigest || verify.Manifest.Config.SizeBytes != int64(len(cfgBytes)) {
		return nil, fmt.Errorf("自检产物 %s: config 引用不一致: %w", opts.OutPath, image.ErrDigestMismatch)
	}
	res.Name, res.Version = verify.Manifest.Name, verify.Manifest.Version

	slog.Info("镜像构建完成",
		slog.String("ref", verify.Manifest.Ref()),
		slog.String("path", res.Path),
		slog.Int("layers", res.LayerCount),
		slog.Int64("bytes", res.Bytes),
		slog.String("layerDigest", res.LayerDigest),
		slog.Int("skipped", res.Skipped),
	)
	return res, nil
}

// baseImage 是基础镜像的只读视图：Loaded 用来取层字节，layers 已按 applyOrder 排好。
type baseImage struct {
	loaded *image.Loaded
	layers []image.Layer
}

// validateOptions 校验必填项与路径形状，全部失败都在动手前暴露。
func validateOptions(opts *Options) error {
	switch {
	case opts == nil:
		return fmt.Errorf("构建参数为空: %w", ErrBadBoxfile)
	case opts.Boxfile == nil:
		return fmt.Errorf("缺少已解析的 Boxfile: %w", ErrBadBoxfile)
	case opts.OutPath == "":
		return fmt.Errorf("输出路径为空: %w", ErrBadInstruction)
	case strings.HasSuffix(opts.OutPath, string(filepath.Separator)):
		return fmt.Errorf("输出路径 %q 是目录: %w", opts.OutPath, ErrBadInstruction)
	}
	if fi, err := os.Stat(opts.OutPath); err == nil && fi.IsDir() {
		return fmt.Errorf("输出路径 %q 是已存在的目录: %w", opts.OutPath, ErrBadInstruction)
	}
	if opts.BaseImage != "" {
		fi, err := os.Stat(opts.BaseImage)
		if err != nil {
			return fmt.Errorf("基础镜像 %s: %w", opts.BaseImage, err)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("基础镜像 %s 不是普通文件: %w", opts.BaseImage, ErrBadInstruction)
		}
	}
	for _, la := range opts.Boxfile.Instructions {
		if la.Op == OpCopy {
			if opts.ContextDir == "" {
				return fmt.Errorf("第 %d 行 COPY: 缺少构建上下文: %w", la.Line, ErrNoContext)
			}
			return CheckContext(opts.ContextDir)
		}
	}
	return nil
}

// buildWorkRoot 返回临时构建工作区的父目录：优先 TempDir，其次输出目录的父目录。
func buildWorkRoot(opts *Options) (string, error) {
	dir := opts.TempDir
	if dir == "" {
		dir = filepath.Dir(opts.OutPath)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("解析临时目录 %s: %w", dir, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("临时目录 %s: %w", abs, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("临时目录 %s 不是目录: %w", abs, ErrBadInstruction)
	}
	return abs, nil
}

// imageRefOf 决定产物的 name:version：沿用基础镜像的引用，scratch 构建取
// 输出文件名去掉 .licore 后缀，再依次回退为 "scratch" / "latest"。
func imageRefOf(opts *Options, base *baseImage) (string, string, error) {
	name, version := opts.Name, opts.Version
	if name == "" {
		if base.loaded != nil {
			name, version = base.loaded.Manifest.Name, opts.Version
			if version == "" {
				version = base.loaded.Manifest.Version
			}
		} else {
			stem := strings.TrimSuffix(filepath.Base(opts.OutPath), ".licore")
			if stem == "" || stem == "." || stem == string(filepath.Separator) {
				stem = "scratch"
			}
			name, version = stem, opts.Version
			if version == "" {
				version = "latest"
			}
		}
	} else if version == "" {
		version = "latest"
	}
	if err := checkArtifactName(name); err != nil {
		return "", "", err
	}
	if version == "" || strings.TrimSpace(version) != version {
		return "", "", fmt.Errorf("版本号 %q 非法: %w", version, ErrBadInstruction)
	}
	return name, version, nil
}

// checkArtifactName 按规范 3.1 的 name 规则（^[a-z0-9][a-z0-9._/-]{0,254}$）校验文件名派生的镜像名。
func checkArtifactName(name string) error {
	if name == "" || len(name) > 255 {
		return fmt.Errorf("镜像名 %q 长度非法: %w", name, ErrBadInstruction)
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '.' || c == '_' || c == '-' || c == '/') && i > 0:
		default:
			return fmt.Errorf("镜像名 %q 含非法字符 %q（需匹配 ^[a-z0-9][a-z0-9._/-]{0,254}$）: %w",
				name, c, ErrBadInstruction)
		}
	}
	return nil
}

// archOf / osOf 决定产物的平台字段，留空时跟随宿主。
func archOf(opts *Options) string {
	if opts.Architecture != "" {
		return opts.Architecture
	}
	return runtime.GOARCH
}

func osOf(opts *Options) string {
	if opts.OS != "" {
		return opts.OS
	}
	return runtime.GOOS
}

// buildAnnotations 记录构建工具与 ARG 取值，键前缀符合规范 3.1 的注解约束。
func buildAnnotations(opts *Options) map[string]string {
	ann := map[string]string{"org.licore.build.tool": "licore/build"}
	if len(opts.Args) > 0 {
		keys := make([]string, 0, len(opts.Args))
		for k := range opts.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+opts.Args[k])
		}
		ann["org.licore.build.args"] = strings.Join(parts, ",")
	}
	return ann
}

// configOf 以基础镜像的 config 为底，叠加调用方标签，得到产物的运行配置。
func configOf(opts *Options, base *baseImage) *image.Config {
	cfg := &image.Config{}
	if base.loaded != nil && base.loaded.Config != nil {
		src := base.loaded.Config
		cfg.Entrypoint = append([]string(nil), src.Entrypoint...)
		cfg.Cmd = append([]string(nil), src.Cmd...)
		cfg.WorkingDir = src.WorkingDir
		cfg.User = src.User
		cfg.Expose = append([]string(nil), src.Expose...)
		cfg.Volumes = append([]string(nil), src.Volumes...)
		for k, v := range src.Env {
			if cfg.Env == nil {
				cfg.Env = map[string]string{}
			}
			cfg.Env[k] = v
		}
		for k, v := range src.Labels {
			if cfg.Labels == nil {
				cfg.Labels = map[string]string{}
			}
			cfg.Labels[k] = v
		}
	}
	for k, v := range opts.Labels {
		if cfg.Labels == nil {
			cfg.Labels = map[string]string{}
		}
		cfg.Labels[k] = v
	}
	return cfg
}

// applyInstructions 按顺序施加全部指令：COPY 落 rootfs，其余落 config。
func applyInstructions(ctx context.Context, opts *Options, rootfs string, cfg *image.Config, res *Result) error {
	// copiedInodes 跨本次构建的所有 COPY 指令共享：同一份上下文文件被多条
	// COPY 引用时也能识别为同一 inode，避免重复落内容（见 copyInto）。
	copiedInodes := make(map[inodeKey]string)
	for _, in := range opts.Boxfile.Instructions {
		select {
		case <-ctx.Done():
			return fmt.Errorf("构建已取消: %w", ctx.Err())
		default:
		}
		switch in.Op {
		case OpCopy:
			if err := applyCopy(opts.ContextDir, rootfs, in, res, copiedInodes); err != nil {
				return err
			}
		case OpEnv:
			if len(in.Args) != 2 {
				return fmt.Errorf("第 %d 行 ENV 需要 2 个参数，得到 %d 个: %w", in.Line, len(in.Args), ErrBadInstruction)
			}
			if cfg.Env == nil {
				cfg.Env = map[string]string{}
			}
			cfg.Env[in.Args[0]] = in.Args[1]
		case OpWorkdir:
			if len(in.Args) != 1 {
				return fmt.Errorf("第 %d 行 WORKDIR 需要 1 个参数，得到 %d 个: %w", in.Line, len(in.Args), ErrBadInstruction)
			}
			dst, err := resolveWorkdir(rootfs, in.Args[0])
			if err != nil {
				return err
			}
			if err := os.MkdirAll(dst, defDirMode); err != nil {
				return fmt.Errorf("第 %d 行 WORKDIR 创建 %s: %w", in.Line, in.Args[0], err)
			}
			cfg.WorkingDir = in.Args[0]
		case OpEntrypoint:
			cfg.Entrypoint = append([]string(nil), in.Args...)
		case OpCmd:
			cfg.Cmd = append([]string(nil), in.Args...)
		case OpExpose:
			if len(in.Args) != 1 {
				return fmt.Errorf("第 %d 行 EXPOSE 需要 1 个参数，得到 %d 个: %w", in.Line, len(in.Args), ErrBadInstruction)
			}
			cfg.Expose = appendUnique(cfg.Expose, in.Args[0])
		case OpVolume:
			if len(in.Args) != 1 {
				return fmt.Errorf("第 %d 行 VOLUME 需要 1 个参数，得到 %d 个: %w", in.Line, len(in.Args), ErrBadInstruction)
			}
			cfg.Volumes = appendUnique(cfg.Volumes, in.Args[0])
		case OpLabel:
			if len(in.Args) != 2 {
				return fmt.Errorf("第 %d 行 LABEL 需要 2 个参数，得到 %d 个: %w", in.Line, len(in.Args), ErrBadInstruction)
			}
			if cfg.Labels == nil {
				cfg.Labels = map[string]string{}
			}
			cfg.Labels[in.Args[0]] = in.Args[1]
		case OpUser:
			if len(in.Args) != 1 {
				return fmt.Errorf("第 %d 行 USER 需要 1 个参数，得到 %d 个: %w", in.Line, len(in.Args), ErrBadInstruction)
			}
			cfg.User = in.Args[0]
		case OpArg:
			// ARG 只参与解析期展开与注解记录，不产生 rootfs 变更。
		default:
			return fmt.Errorf("第 %d 行 %s: 不支持的指令: %w", in.Line, in.Op, ErrBadInstruction)
		}
	}
	return nil
}

// resolveWorkdir 校验 WORKDIR 的容器内绝对路径并映射回宿主 rootfs 路径。
func resolveWorkdir(rootfs, dir string) (string, error) {
	if dir == "" || !strings.HasPrefix(dir, "/") {
		return "", fmt.Errorf("WORKDIR %q 必须是容器内绝对路径: %w", dir, ErrBadInstruction)
	}
	clean := filepath.Clean(dir)
	if clean == "/" {
		return rootfs, nil
	}
	rel := filepath.FromSlash(strings.TrimPrefix(clean, "/"))
	return ResolveRootfsPath(rootfs, rel)
}

// appendUnique 追加不重复的字符串（保持出现顺序）。
func appendUnique(list []string, v string) []string {
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}

// applyCopy 执行一条 COPY：源来自构建上下文，目标落在 rootfs 内。
func applyCopy(contextDir, rootfs string, in Instruction, res *Result, copiedInodes map[inodeKey]string) error {
	if len(in.Args) != 2 {
		return fmt.Errorf("第 %d 行 COPY 需要 <src> <dst> 两个参数，得到 %d 个: %w", in.Line, len(in.Args), ErrBadInstruction)
	}
	src, err := resolveCopySource(contextDir, in.Args[0])
	if err != nil {
		return fmt.Errorf("第 %d 行 COPY: %w", in.Line, err)
	}
	dst, err := resolveCopyDest(rootfs, in.Args[1])
	if err != nil {
		return fmt.Errorf("第 %d 行 COPY: %w", in.Line, err)
	}

	// 用 Lstat 判定源类型：符号链接必须原样搬进 rootfs，而不是跟随它拷贝内容。
	fi, err := os.Lstat(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("第 %d 行 COPY: 源路径 %q: %w（%w）", in.Line, in.Args[0], err, ErrNoContext)
		}
		return fmt.Errorf("第 %d 行 COPY: 源路径 %q: %w", in.Line, in.Args[0], err)
	}

	// 源是目录、或目标是已存在目录（Lstat：符号链接目标不算目录）或写成了 "/x/" 时：拷贝进目录。
	intoDir := fi.IsDir() || strings.HasSuffix(in.Args[1], "/")
	if existing, err := os.Lstat(dst); err == nil && existing.IsDir() {
		intoDir = true
	}
	if intoDir {
		if err := os.MkdirAll(dst, defDirMode); err != nil {
			return fmt.Errorf("第 %d 行 COPY: 创建目标目录 %q: %w", in.Line, in.Args[1], err)
		}
		srcList := []string{src}
		if fi.IsDir() {
			ents, err := os.ReadDir(src)
			if err != nil {
				return fmt.Errorf("第 %d 行 COPY: 读取源目录 %q: %w", in.Line, in.Args[0], err)
			}
			srcList = srcList[:0]
			for _, e := range ents {
				srcList = append(srcList, filepath.Join(src, e.Name()))
			}
		}
		for _, s := range srcList {
			if err := copyInto(contextDir, rootfs, s, filepath.Join(dst, filepath.Base(s)), res, copiedInodes); err != nil {
				return fmt.Errorf("第 %d 行 COPY %q: %w", in.Line, in.Args[0], err)
			}
		}
		return nil
	}
	return copyInto(contextDir, rootfs, src, dst, res, copiedInodes)
}

// copyInto 把单个源条目写到 rootfs 内的目标路径，保留权限与符号链接语义。
func copyInto(contextDir, rootfs, src, dst string, res *Result, copiedInodes map[inodeKey]string) error {
	// 目标路径链必须是真实目录，禁止经由符号链接写出 rootfs 之外。
	if err := mkParents(rootfs, dst); err != nil {
		return err
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("读取源 %s: %w", src, err)
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		link, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("读取符号链接 %s: %w", src, err)
		}
		if err := removeAny(dst); err != nil {
			return err
		}
		if err := os.Symlink(link, dst); err != nil {
			return fmt.Errorf("创建符号链接 %s: %w", dst, err)
		}
	case fi.Mode().IsRegular():
		// 硬链接：同一 inode 在上下文里可能有多个名字（busybox 式镜像常见）。
		// 若每个名字都拷一份完整内容，rootfs 会按链接数成倍膨胀——实测
		// busybox 从 6.8 MB 涨到 280 MB。因此首次出现正常拷贝，其余建硬链接。
		if dev, ino, nlink, ok := inodeOf(fi); ok && nlink > 1 {
			key := inodeKey{dev: dev, ino: ino}
			if first, dup := copiedInodes[key]; dup {
				if err := removeAny(dst); err != nil {
					return err
				}
				if err := os.Link(first, dst); err == nil {
					return nil
				}
				// 跨设备等导致 link 失败时退化为独立拷贝（语义仍正确）。
			} else {
				copiedInodes[key] = dst
			}
		}
		if err := copyRegular(src, dst, fi.Mode().Perm()); err != nil {
			return err
		}
	case fi.IsDir():
		if err := mkParents(rootfs, filepath.Join(dst, "x")); err != nil {
			return err
		}
		if err := os.MkdirAll(dst, fi.Mode().Perm()&^0o7000); err != nil {
			return fmt.Errorf("创建目录 %s: %w", dst, err)
		}
		ents, err := os.ReadDir(src)
		if err != nil {
			return fmt.Errorf("读取目录 %s: %w", src, err)
		}
		for _, e := range ents {
			if err := copyInto(contextDir, rootfs, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), res, copiedInodes); err != nil {
				return err
			}
		}
	default:
		res.Skipped++
		slog.Warn("跳过非常规文件（socket / 设备节点 / FIFO）",
			slog.String("src", src),
			slog.String("mode", fi.Mode().String()),
		)
	}
	return nil
}

// copyRegular 拷贝普通文件，覆盖符号链接而非跟随，并剥离危险权限位。
func copyRegular(src, dst string, perm fs.FileMode) error {
	if fi, err := os.Lstat(dst); err == nil && !fi.IsDir() {
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("移除已存在目标 %s: %w", dst, err)
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开源文件 %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("创建目标文件 %s: %w", dst, err)
	}
	_, err = io.Copy(out, in)
	if cerr := out.Chmod(perm &^ 0o7000); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("拷贝 %s → %s: %w", src, dst, err)
	}
	return nil
}

// mkParents 保证 target 的父目录链在 rootfs 内且全为真实目录（符号链接一律拒绝）。
func mkParents(rootfs, target string) error {
	rel, err := filepath.Rel(rootfs, filepath.Dir(target))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("目标 %q 逃逸出 rootfs: %w", target, ErrNoContext)
	}
	cur := rootfs
	if rel == "." {
		return nil
	}
	for _, comp := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, comp)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(cur, defDirMode); err != nil && !os.IsExist(err) {
				return fmt.Errorf("创建中间目录 %s: %w", cur, err)
			}
		case err != nil:
			return fmt.Errorf("检查中间目录 %s: %w", cur, err)
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("拒绝经由符号链接写出 %s: %w", cur, image.ErrUnsafePath)
		case !fi.IsDir():
			return fmt.Errorf("中间组件 %s 不是目录: %w", cur, ErrNoContext)
		}
	}
	return nil
}

// removeAny 删除路径（文件、符号链接或整个目录子树），不存在为 no-op。
func removeAny(p string) error {
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := os.RemoveAll(p); err != nil {
		return fmt.Errorf("移除 %s: %w", p, err)
	}
	return nil
}

// resolveCopySource 解析 COPY 源路径：先按语法拒绝绝对路径与 "." / ".." 段，
// 再用 EvalSymlinks 实体化其父目录，确认它仍在 ContextDir 内。
//
// 只实体化父目录、不实体化最后一段：末段本身若是符号链接，应当原样搬进
// rootfs（这是构建立即语义），而不是被解析成链接目标。
func resolveCopySource(contextDir, src string) (string, error) {
	full, err := ResolveContextPath(contextDir, src)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(contextDir)
	if err != nil {
		return "", fmt.Errorf("解析构建上下文 %s: %w（%w）", contextDir, err, ErrNoContext)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("解析构建上下文 %s 的真实路径: %w（%w）", contextDir, err, ErrNoContext)
	}
	resolvedParent, err := evalExistingPrefix(filepath.Dir(full))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedParent)
	if err != nil {
		return "", fmt.Errorf("源路径 %q 相对构建上下文: %w（%w）", src, err, ErrNoContext)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("源路径 %q 经符号链接逃逸出构建上下文 %s: %w", src, contextDir, ErrNoContext)
	}
	return filepath.Join(resolvedParent, filepath.Base(full)), nil
}

// evalExistingPrefix 对路径的最深已存在前缀做 EvalSymlinks，再拼回不存在的尾部，
// 从而对"源还不存在"与"源经符号链接跳出去"两种情况给出同一套判定。
func evalExistingPrefix(p string) (string, error) {
	cur := filepath.Clean(p)
	var tail []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("检查路径 %s: %w", cur, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
	resolved, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", fmt.Errorf("解析路径 %s 的真实路径: %w（%w）", cur, err, ErrNoContext)
	}
	for i := len(tail) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, tail[i])
	}
	return resolved, nil
}

// resolveCopyDest 解析 COPY 目标：允许容器内绝对路径与上下文相对路径两种写法，
// 结果必须落在 rootfs 内。
func resolveCopyDest(rootfs, dst string) (string, error) {
	if dst == "" {
		return "", fmt.Errorf("目标路径为空: %w", ErrNoContext)
	}
	if strings.Contains(dst, "\\") {
		return "", fmt.Errorf("目标路径 %q 含反斜杠: %w", dst, ErrNoContext)
	}
	trimmed := strings.Trim(dst, "/")
	if trimmed == "" {
		return rootfs, nil
	}
	return ResolveRootfsPath(rootfs, filepath.ToSlash(trimmed))
}

// unpackLayer 把层 tar.gz 解进目标 rootfs（差异视图叠加：文件覆盖，目录合并；
// whiteout 是合并层语义，不属于构建期职责）。
func unpackLayer(layerPath, targetDir string) error {
	f, err := os.Open(layerPath)
	if err != nil {
		return fmt.Errorf("打开层文件: %w", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip 头非法: %w", err)
	}
	defer func() { _ = gz.Close() }()
	if err := extractInto(gz, targetDir); err != nil {
		return err
	}
	return gz.Close()
}

// extractInto 把 tar 流解进 targetDir，逐条目做路径与权限约束。
func extractInto(r io.Reader, targetDir string) error {
	tr := tar.NewReader(r)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar 流损坏: %w", err)
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if err := image.SafeArchivePath(name); err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("层内条目 %q 重复: %w", name, ErrBadInstruction)
		}
		seen[name] = true

		target, err := ResolveRootfsPath(targetDir, name)
		if err != nil {
			return err
		}
		mode := fs.FileMode(hdr.Mode).Perm() &^ 0o7000
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := mkParents(targetDir, filepath.Join(target, "x")); err != nil {
				return err
			}
			if err := os.MkdirAll(target, mode); err != nil {
				return fmt.Errorf("创建目录 %q: %w", name, err)
			}
			_ = os.Chmod(target, mode)
		case tar.TypeReg:
			if hdr.Size < 0 {
				return fmt.Errorf("条目 %q 大小为负: %w", name, ErrBadInstruction)
			}
			if err := mkParents(targetDir, target); err != nil {
				return err
			}
			if err := writeReg(target, mode, tr, hdr.Size); err != nil {
				return fmt.Errorf("写入 %q: %w", name, err)
			}
		case tar.TypeSymlink:
			if err := mkParents(targetDir, target); err != nil {
				return err
			}
			if err := removeAny(target); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("创建符号链接 %q: %w", name, err)
			}
		case tar.TypeLink:
			if err := image.SafeArchivePath(hdr.Linkname); err != nil {
				return fmt.Errorf("硬链接目标: %w", err)
			}
			if !seen[hdr.Linkname] {
				// 指向本层之外的硬链接：按普通文件落一份拷贝，语义等价且更安全。
				srcAbs, err := ResolveRootfsPath(targetDir, hdr.Linkname)
				if err != nil {
					return err
				}
				fi, err := os.Lstat(srcAbs)
				if err != nil || !fi.Mode().IsRegular() {
					return fmt.Errorf("硬链接 %q 的目标 %q 不存在或不是普通文件: %w", name, hdr.Linkname, ErrBadInstruction)
				}
				if err := mkParents(targetDir, target); err != nil {
					return err
				}
				if err := copyRegular(srcAbs, target, fi.Mode().Perm()); err != nil {
					return err
				}
				break
			}
			if err := mkParents(targetDir, target); err != nil {
				return err
			}
			if err := removeAny(target); err != nil {
				return err
			}
			linkSrc, err := ResolveRootfsPath(targetDir, hdr.Linkname)
			if err != nil {
				return err
			}
			if err := os.Link(linkSrc, target); err != nil {
				return fmt.Errorf("创建硬链接 %q: %w", name, err)
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			slog.Warn("基础层含设备节点 / FIFO，已跳过", slog.String("entry", name), slog.String("type", string(hdr.Typeflag)))
		default:
			slog.Debug("忽略未知 tar 类型", slog.String("entry", name), slog.String("type", string(hdr.Typeflag)))
		}
	}
}

// writeReg 以剥离危险位后的权限写入普通文件内容。
func writeReg(target string, mode fs.FileMode, r io.Reader, size int64) error {
	if fi, err := os.Lstat(target); err == nil && fi.IsDir() {
		return fmt.Errorf("目标 %q 与已存在目录冲突: %w", target, ErrBadInstruction)
	}
	if err := removeAny(target); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("创建文件 %s: %w", target, err)
	}
	n, err := io.Copy(out, io.LimitReader(r, size))
	if err == nil && n != size {
		err = fmt.Errorf("条目截断：期望 %d 字节实得 %d", size, n)
	}
	if cerr := out.Chmod(mode); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// writeLayer 把 rootfs 打成 tar.gz 追加层，边写边算 tar.gz 原始字节的摘要与大小。
func writeLayer(rootfs, outPath string) (string, int64, error) {
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("创建层文件 %s: %w", outPath, err)
	}
	hasher := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(out, hasher))
	tw := tar.NewWriter(gz)

	werr := tarTree(tw, rootfs)
	if err := tw.Close(); werr == nil {
		werr = err
	}
	if err := gz.Close(); werr == nil {
		werr = err
	}
	if err := out.Close(); werr == nil {
		werr = err
	}
	if werr != nil {
		_ = os.Remove(outPath)
		return "", 0, fmt.Errorf("写出层 %s: %w", outPath, werr)
	}
	fi, err := os.Stat(outPath)
	if err != nil {
		return "", 0, fmt.Errorf("统计层 %s: %w", outPath, err)
	}
	if fi.Size() > maxBuildLayerBytes {
		return "", 0, fmt.Errorf("层 %s 达 %d 字节，超过上限 %d: %w", outPath, fi.Size(), maxBuildLayerBytes, ErrBadInstruction)
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), fi.Size(), nil
}

// tarTree 按确定顺序把 rootfs 打包：目录在前，同目录内按文件名字典序。
func tarTree(tw *tar.Writer, rootfs string) error {
	// seenInodes 记录 (dev, ino) → 首次出现的归档路径，用于把硬链接写成
	// tar.TypeLink 而不是重复的文件内容（见 writeTarEntry）。
	seenInodes := make(map[inodeKey]string)
	return filepath.WalkDir(rootfs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == rootfs {
			return nil
		}
		rel, err := filepath.Rel(rootfs, p)
		if err != nil {
			return fmt.Errorf("相对路径 %s: %w", p, err)
		}
		name := filepath.ToSlash(rel)
		return writeTarEntry(tw, name, p, seenInodes)
	})
}

// inodeKey 唯一标识一个 inode（含设备号，跨设备不会误判）。
type inodeKey struct {
	dev uint64
	ino uint64
}

// hardlinkKey 判断 name 是否为「已经写过的 inode 的又一个硬链接名字」。
//
// 返回 (首次出现的归档路径, true) 表示它是重复链接，调用方应写 tar.TypeLink；
// 否则返回 ("", false)，并把本次路径登记为该 inode 的首次出现。
//
// nlink == 1 是绝大多数文件的常态，直接短路，避免为每个文件查表。
// 平台不支持 inode 查询时（见 inode_other.go）恒返回 false，退化为各写一份内容。
func hardlinkKey(fi os.FileInfo, seen map[inodeKey]string, name string) (string, bool) {
	dev, ino, nlink, ok := inodeOf(fi)
	if !ok || nlink <= 1 {
		return "", false
	}
	key := inodeKey{dev: dev, ino: ino}
	if first, dup := seen[key]; dup {
		return first, true
	}
	seen[key] = name
	return "", false
}

// writeTarEntry 把 one 条 rootfs 条目写进 tar：常规文件读磁盘，符号链接写目标，
// 目录写头，socket / 设备节点 / FIFO 跳过并计数告警。
func writeTarEntry(tw *tar.Writer, name, src string, seenInodes map[inodeKey]string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("读取 %s: %w", src, err)
	}
	switch {
	case fi.IsDir():
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return fmt.Errorf("构造目录头 %s: %w", name, err)
		}
		hdr.Name = name + "/"
		hdr.Mode = int64(fi.Mode().Perm() &^ 0o7000)
		return tw.WriteHeader(hdr)

	case fi.Mode().IsRegular():
		// 硬链接：nlink > 1 的文件在 rootfs 里是同一份 inode 的多个名字
		// （busybox 就是一个二进制 + 数百个 applet 名）。若每个名字都写一份
		// 完整内容，层体积会按链接数成倍膨胀——实测 busybox 从 6.8 MB 涨到
		// 280 MB（约 41 倍）。因此首次出现写常规文件，其余写成 hard link 条目。
		if first, dup := hardlinkKey(fi, seenInodes, name); dup {
			hdr, err := tar.FileInfoHeader(fi, "")
			if err != nil {
				return fmt.Errorf("构造硬链接头 %s: %w", name, err)
			}
			hdr.Name = name
			hdr.Typeflag = tar.TypeLink
			hdr.Linkname = first
			hdr.Size = 0
			hdr.Mode = int64(fi.Mode().Perm() &^ 0o7000)
			return tw.WriteHeader(hdr)
		}

		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return fmt.Errorf("构造文件头 %s: %w", name, err)
		}
		hdr.Name = name
		hdr.Size = fi.Size()
		hdr.Mode = int64(fi.Mode().Perm() &^ 0o7000)
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("写文件头 %s: %w", name, err)
		}
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("打开 %s: %w", src, err)
		}
		_, cerr := io.Copy(tw, f)
		if err := f.Close(); cerr == nil {
			cerr = err
		}
		if cerr != nil {
			return fmt.Errorf("写文件内容 %s: %w", name, cerr)
		}
		return nil

	case fi.Mode()&fs.ModeSymlink != 0:
		link, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("读取符号链接 %s: %w", src, err)
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return fmt.Errorf("构造符号链接头 %s: %w", name, err)
		}
		hdr.Name = name
		hdr.Linkname = link
		hdr.Mode = int64(fi.Mode().Perm() &^ 0o7000)
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("写符号链接头 %s: %w", name, err)
		}
		return nil

	default:
		slog.Warn("跳过非常规条目（socket / 设备节点 / FIFO）",
			slog.String("entry", name),
			slog.String("mode", fi.Mode().String()),
		)
		return nil
	}
}

// writeTarBytes 把一个内存字节串写进 tar 成为常规文件条目。
func writeTarBytes(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     int64(len(data)),
		Mode:     int64(defFileMode),
		ModTime:  time.Now(),
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("写条目头 %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("写条目内容 %s: %w", name, err)
	}
	return nil
}

// copyTarFile 把磁盘上的文件按指定归档条目名原样写进 tar（字节与大小都不变，
// 从而保住基础层在 index.json 里声明的 digest / sizeBytes）。
func copyTarFile(tw *tar.Writer, name, src string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("统计 %s: %w", src, err)
	}
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", src, err)
	}
	defer func() { _ = f.Close() }()
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     fi.Size(),
		Mode:     int64(defFileMode),
		ModTime:  fi.ModTime(),
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("写条目头 %s: %w", name, err)
	}
	n, err := io.Copy(tw, f)
	if err != nil {
		return fmt.Errorf("写条目内容 %s: %w", name, err)
	}
	if n != fi.Size() {
		return fmt.Errorf("条目 %s 截断：期望 %d 字节实得 %d: %w", name, fi.Size(), n, image.ErrSizeMismatch)
	}
	return nil
}

// configBlobName 返回 config blob 在归档内的条目名：blobs/sha256-<hex>。
func configBlobName(digest string) string {
	algo, sum, _ := strings.Cut(digest, ":")
	return image.BlobsDir + algo + "-" + sum
}

// marshalConfig 序列化 config 小对象，并返回其字节与摘要。
// 使用 json.Marshal（HTML 转义开启），保证 image.ParseConfig 严格解析回同一结构。
func marshalConfig(cfg *image.Config) ([]byte, string, error) {
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	if cfg.Labels == nil {
		cfg.Labels = map[string]string{}
	}
	if cfg.Entrypoint == nil {
		cfg.Entrypoint = []string{}
	}
	if cfg.Cmd == nil {
		cfg.Cmd = []string{}
	}
	if cfg.Expose == nil {
		cfg.Expose = []string{}
	}
	if cfg.Volumes == nil {
		cfg.Volumes = []string{}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("序列化 config: %w", err)
	}
	if _, err := image.ParseConfig(data); err != nil {
		return nil, "", fmt.Errorf("序列化的 config 无法回读: %w", err)
	}
	sum := sha256.Sum256(data)
	return data, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// layerBaseName 从归档层路径里取出文件名主体，如 layers/000001.base.tar.gz → base.tar.gz。
func layerBaseName(p string) string {
	base := filepath.Base(p)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		return base[i+1:]
	}
	return base
}
