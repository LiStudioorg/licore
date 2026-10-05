// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/LiStudioorg/licore/internal/build"
	"github.com/LiStudioorg/licore/internal/image"
)

// Options 描述一次 Docker → LiCore 转换。
type Options struct {
	// Image 是 Docker 镜像引用，形如 alpine:3.20 或 nginx:1.27-alpine。
	Image string
	// Arch 是目标架构（linux/<arch>），留空取宿主 GOARCH。
	Arch string
	// OutPath 是产出的 .licore 路径。与 ImportImage 二选一（或都为空由调用方定）。
	OutPath string
	// ImportImage 为 true 时只导入本地 store，不写输出文件。
	ImportImage bool
	// Name / Version 覆盖产物的镜像引用；留空由 Docker 镜像名派生。
	Name    string
	Version string
	// DataDir 是 LiCore 数据目录（导入时需要）。
	DataDir string
	// WorkDir 是临时工作目录的父目录，留空取系统临时目录。
	WorkDir string
	// NoCleanup 保留临时容器与导出 tar（调试用）。
	NoCleanup bool
	// KeepDockerImage 转换后不删除 docker 侧拉取的镜像。
	KeepDockerImage bool
	// Docker 是 docker 交互实现，留空用 CLIDocker。
	Docker Docker
	// Stderr 用于输出 docker 的进度信息，留空丢弃。
	Stderr io.Writer
}

// Result 是一次转换的产出摘要。
type Result struct {
	// Ref 是产物的镜像引用 name:version。
	Ref string
	// Arch 是产物架构。
	Arch string
	// Path 是产出的 .licore 路径，本函数返回后**仍然存在**：
	//   - 指定了 OutPath 时就是 OutPath；
	//   - ImportImage 模式（且未指定 OutPath）时是 work 之外的临时文件，
	//     供调用方导入 store，**清理由调用方负责**；
	//   - 两者都未指定时是 work 内的临时文件，随 work 一起清理，
	//     此时不应使用该字段。
	Path string
	// Bytes 是产出文件字节数。
	Bytes int64
	// Layers 是产物层数。
	Layers int
	// EnvSkipped / LabelsSkipped 记录因 LiCore 语法无法表达而丢弃的条目数。
	EnvSkipped    int
	LabelsSkipped int
}

// Convert 执行完整的 Docker → LiCore 转换。
func Convert(ctx context.Context, opts *Options) (*Result, error) {
	if opts == nil || strings.TrimSpace(opts.Image) == "" {
		return nil, fmt.Errorf("convert: 必须指定 docker 镜像名")
	}
	d := opts.Docker
	if d == nil {
		d = &CLIDocker{}
	}
	if err := CheckDocker(ctx, d); err != nil {
		return nil, err
	}

	arch := opts.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}

	// 临时工作区：容器名、导出 tar、解压后的 rootfs 都放这里，
	// 统一 defer 清理，任何一步失败都不留垃圾。
	work, err := os.MkdirTemp(opts.WorkDir, "licore-convert-")
	if err != nil {
		return nil, fmt.Errorf("convert: 创建临时目录失败: %w", err)
	}
	cleanup := func() {
		if opts.NoCleanup {
			slog.Info("convert: --no-cleanup 已指定，保留临时目录", "dir", work)
			return
		}
		_ = os.RemoveAll(work)
	}
	defer cleanup()

	// promote 把 work 内的产物复制到 work 之外的稳定目录并返回新路径。
	// ImportImage 模式下调用方要在 Convert 返回**之后**才去读这个文件来导入
	// store，而 work 会被上面的 defer 删掉，所以必须先挪出来。
	// 目标目录可能位于其它文件系统，因此用 copyFile 而不是 rename。
	promote := func(src string) (string, error) {
		dir, err := os.MkdirTemp(opts.WorkDir, "licore-convert-out-")
		if err != nil {
			return "", fmt.Errorf("convert: 创建产出目录失败: %w", err)
		}
		dst := filepath.Join(dir, filepath.Base(src))
		if err := copyFile(src, dst); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("convert: 保存产出失败: %w", err)
		}
		return dst, nil
	}

	platform := "linux/" + arch
	// 磁盘预检：pull + export + 解压会同时占用三份空间（镜像、tar、rootfs），
	// 空间不足时提前报错，而不是跑到一半留下半拉子产物。
	if err := checkDiskSpace(ctx, d, opts.Image, work, platform); err != nil {
		return nil, err
	}

	// 1. pull：显式指定平台，避免在异构机器上拿到错架构。
	slog.Info("convert: 拉取 docker 镜像", "image", opts.Image, "platform", platform)
	if _, err := d.Run(ctx, "pull", "--platform", platform, opts.Image); err != nil {
		return nil, fmt.Errorf("%w：无法拉取 %s，请检查镜像名与网络: %w", ErrDockerPull, opts.Image, err)
	}

	// Windows 容器镜像无法在 LiCore 运行，尽早拒绝而不是产出一个跑不起来的包。
	if ins, err := inspect(ctx, d, opts.Image); err == nil && strings.EqualFold(ins.Os, "windows") {
		return nil, fmt.Errorf("%w：%s 是 Windows 容器镜像（os=%s）", ErrWindowsImage, opts.Image, ins.Os)
	}

	// 2. create + export + rm：拿完整 rootfs。
	containerName := fmt.Sprintf("licore-convert-%d", os.Getpid())
	if _, err := d.Run(ctx, "create", "--name", containerName, "--platform", platform, opts.Image); err != nil {
		return nil, fmt.Errorf("convert: 创建临时容器失败: %w", err)
	}
	containerRemoved := false
	removeContainer := func() {
		if containerRemoved {
			return
		}
		containerRemoved = true
		if opts.NoCleanup {
			slog.Info("convert: --no-cleanup 已指定，保留临时容器", "container", containerName)
			return
		}
		// 用 context.WithoutCancel 之外的独立上下文：主 ctx 可能已取消，
		// 但清理必须尽力完成。此处直接传 Background。
		if _, err := d.Run(context.Background(), "rm", "-f", containerName); err != nil {
			slog.Warn("convert: 清理临时容器失败", "container", containerName, "err", err)
		}
	}
	defer removeContainer()

	tarPath := filepath.Join(work, "rootfs.tar")
	slog.Info("convert: 导出容器文件系统", "container", containerName)
	if err := d.RunToFile(ctx, tarPath, "export", containerName); err != nil {
		return nil, fmt.Errorf("convert: docker export 失败: %w", err)
	}
	removeContainer()

	// 3. 解压到临时 rootfs 目录。
	rootfsDir := filepath.Join(work, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		return nil, fmt.Errorf("convert: 创建 rootfs 目录失败: %w", err)
	}
	if err := extractTar(tarPath, rootfsDir); err != nil {
		return nil, err
	}

	// 4. inspect 提取运行配置。
	ins, err := inspect(ctx, d, opts.Image)
	if err != nil {
		return nil, err
	}

	// 5. 扫描 rootfs 顶层，生成 COPY 列表。
	dirs, files, err := topLevelEntries(rootfsDir)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 && len(files) == 0 {
		return nil, fmt.Errorf("%w：%s 导出后 rootfs 为空", ErrNoRootfs, opts.Image)
	}

	// ENTRYPOINT/CMD 含 "${" 时无法写进 Boxfile（解析器无条件展开且无转义），
	// 这种情况必须明确失败：丢掉 ENTRYPOINT 会产出跑不起来的镜像。
	if op, arg, bad := unexpressibleCommand(ins); bad {
		return nil, fmt.Errorf("%w：%s 含 %q，LiCore 的 Boxfile 会对 %s 做变量展开且无转义语法，"+
			"无法无损表达（可用 --tag 手工构造 Boxfile 后 licore build）",
			ErrUnsupportedMeta, op, arg, "${...}")
	}

	boxfileText := generateBoxfile(ins, genOptions{TopDirs: dirs, TopFiles: files})
	boxfilePath := filepath.Join(work, "Boxfile")
	if err := os.WriteFile(boxfilePath, []byte(boxfileText), 0o644); err != nil {
		return nil, fmt.Errorf("convert: 写入 Boxfile 失败: %w", err)
	}
	slog.Debug("convert: 生成的 Boxfile", "path", boxfilePath)

	bf, err := build.ParseBoxfileFile(boxfilePath)
	if err != nil {
		// 生成的 Boxfile 解析失败属于本工具的 bug，把内容一并带出来便于定位。
		return nil, fmt.Errorf("convert: 生成的 Boxfile 无法解析（这是 bug，请连同以下内容反馈）:\n%s\n%w",
			boxfileText, err)
	}

	// 6. 构建 .licore。
	name, version := opts.Name, opts.Version
	if name == "" {
		name, version = deriveRef(opts.Image, version)
	}
	if name == "" {
		return nil, fmt.Errorf("convert: 无法从 %q 派生镜像名，请用 --tag 显式指定", opts.Image)
	}

	outTmp, err := os.CreateTemp(work, "out-*.licore")
	if err != nil {
		return nil, fmt.Errorf("convert: 创建输出临时文件失败: %w", err)
	}
	outPath := outTmp.Name()
	_ = outTmp.Close()

	slog.Info("convert: 构建 .licore 镜像", "name", name, "version", version, "arch", arch)
	_, err = build.Build(ctx, &build.Options{
		Boxfile:      bf,
		ContextDir:   rootfsDir,
		OutPath:      outPath,
		Name:         name,
		Version:      version,
		Architecture: arch,
		OS:           "linux",
		TempDir:      work,
	})
	if err != nil {
		return nil, fmt.Errorf("convert: 构建失败: %w", err)
	}

	// 7. 输出：写文件 或 留给调用方导入。
	//
	// ImportImage 模式下调用方（CLI）需要在本函数返回之后继续读这个文件去
	// 导入 store，因此不能把它留在 work 里——defer 会把 work 删掉，调用方
	// 拿到的是一个悬空路径（历史 bug）。promote 把它挪到 work 之外的稳定
	// 目录，清理责任转移给调用方。
	finalPath := outPath
	switch {
	case opts.OutPath != "":
		if err := copyFile(outPath, opts.OutPath); err != nil {
			return nil, fmt.Errorf("convert: 写出 %s 失败: %w", opts.OutPath, err)
		}
		finalPath = opts.OutPath
	case opts.ImportImage:
		promoted, err := promote(outPath)
		if err != nil {
			return nil, err
		}
		finalPath = promoted
	}
	fi, err := os.Stat(finalPath)
	if err != nil {
		return nil, fmt.Errorf("convert: 读取产出信息失败: %w", err)
	}

	// 8. 不保留 docker 镜像时清理（docker rmi 只在成功转换后执行，
	// 失败时留着便于排查）。
	if !opts.KeepDockerImage {
		if _, err := d.Run(context.Background(), "rmi", opts.Image); err != nil {
			slog.Warn("convert: 清理 docker 镜像失败（不影响转换结果）", "image", opts.Image, "err", err)
		}
	}

	envSkipped, labelsSkipped := countSkipped(ins)
	if envSkipped > 0 || labelsSkipped > 0 {
		slog.Warn("convert: 部分元数据无法用 LiCore 语法表达，已跳过",
			"env", envSkipped, "labels", labelsSkipped)
	}

	return &Result{
		Ref:           name + ":" + version,
		Arch:          arch,
		Path:          finalPath,
		Bytes:         fi.Size(),
		Layers:        1,
		EnvSkipped:    envSkipped,
		LabelsSkipped: labelsSkipped,
	}, nil
}

// inspect 执行 docker inspect 并解析。
func inspect(ctx context.Context, d Docker, image string) (*inspectResult, error) {
	out, err := d.Run(ctx, "inspect", image)
	if err != nil {
		return nil, fmt.Errorf("convert: docker inspect %s 失败: %w", image, err)
	}
	ins, err := parseInspect(out)
	if err != nil {
		return nil, fmt.Errorf("convert: %s: %w", image, err)
	}
	return ins, nil
}

// countSkipped 统计最终被丢弃的 ENV 与 LABEL 条目数，用于提示用户。
func countSkipped(ins *inspectResult) (envSkipped, labelsSkipped int) {
	for _, kv := range ins.Config.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || !envValueExpressible(v) {
			envSkipped++
		}
	}
	for k, v := range ins.Config.Labels {
		if v == "" || strings.Contains(v, "=") || strings.Contains(v, "\n") || !validLabelKey(k) {
			labelsSkipped++
		}
	}
	return envSkipped, labelsSkipped
}

// deriveRef 从 docker 镜像引用派生 LiCore 的 name:version。
//
//	alpine:3.20          → alpine:3.20
//	library/nginx:1.27   → library/nginx:1.27
//	nginx                → nginx:latest
//	registry:5000/x:1    → registry:5000/x:1（保留 registry 端口）
//
// LiCore 的镜像引用只允许小写，Docker 侧同理（Docker 也会拒绝大写仓库名）。
func deriveRef(image, version string) (string, string) {
	image = strings.TrimSpace(image)
	if image == "" {
		return "", ""
	}
	// 先剥掉 digest 形式（name@sha256:...），LiCore 的引用不带 digest。
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	// 找最后一个 ':'，且它必须出现在最后一个 '/' 之后才是 tag
	// （否则那是 registry 的端口，如 registry:5000/nginx）。
	if i := strings.LastIndexByte(image, ':'); i >= 0 && !strings.Contains(image[i:], "/") {
		if version == "" {
			version = image[i+1:]
		}
		image = image[:i]
	}
	if version == "" {
		version = "latest"
	}
	return image, version
}

// topLevelEntries 返回 rootfs 顶层的目录与常规文件（各自已排序）。
//
// 只取顶层：LiCore 的 COPY 会递归复制目录内容，逐个顶层条目即可覆盖整个 rootfs。
// 符号链接之类的特殊条目在 LiCore 侧按既有规则处理（构建期剥离），这里只
// 保证不漏掉需要显式 COPY 的常规条目。
func topLevelEntries(rootfs string) (dirs, files []string, err error) {
	entries, err := os.ReadDir(rootfs)
	if err != nil {
		return nil, nil, fmt.Errorf("convert: 读取 rootfs 失败: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		// 跳过容器运行时自己生成的挂载点残留，它们不该进镜像。
		if isRuntimeOnlyEntry(name) {
			continue
		}
		switch {
		case e.IsDir():
			dirs = append(dirs, name)
		case e.Type().IsRegular():
			files = append(files, name)
		default:
			// 符号链接、设备节点等：docker export 的归档里通常不出现设备节点；
			// 符号链接若指向 rootfs 内部，跟随其目标类型（当作目录处理更安全，
			// 因为 COPY 一个目录会连同其中的内容一起复制）。
			if e.Type()&os.ModeSymlink != 0 {
				if fi, serr := os.Stat(filepath.Join(rootfs, name)); serr == nil && fi.IsDir() {
					dirs = append(dirs, name)
					continue
				}
				files = append(files, name)
				continue
			}
			slog.Debug("convert: 跳过不支持的特殊条目", "name", name, "mode", e.Type().String())
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)
	return dirs, files, nil
}

// isRuntimeOnlyEntry 判断是否为 Docker 运行时生成、不应进镜像的条目。
func isRuntimeOnlyEntry(name string) bool {
	switch name {
	case ".dockerenv", ".dockerinit":
		return true
	}
	return false
}

// extractTar 把 docker export 產出的 tar 解到目标目录。
//
// 这里自己解压而不是复用 internal/storage：docker export 的 tar 是普通
// rootfs 归档，没有层语义也不需要 whiteout 处理；而且必须拦截路径逃逸
// （恶意镜像可以放 ../ 条目）。
func extractTar(tarPath, dst string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("convert: 打开导出归档失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("convert: 读取导出归档失败: %w", err)
		}
		if err := extractEntry(tr, hdr, dst); err != nil {
			return err
		}
	}
	return nil
}

// extractEntry 解出一个归档条目，并做路径逃逸防护。
//
// **两层防护，缺一不可**：
//  1. 条目名本身：绝对路径、".." 段、非规范化路径 —— image.SafeExtractPath
//     内部先跑 SafeArchivePath；
//  2. **父链**：目标路径上每一级已存在的父组件必须是真实目录。只做第 1 层
//     是不够的——归档可以"先放一个符号链接条目、再经由它写文件"，此时
//     条目名完全干净，但写入会穿透到目标目录之外（真实逃逸漏洞，
//     见 docs/security-audit-v2.md 的 H-1）。
//
// 因此逐条复用 image.SafeExtractPath，而不是各自手写一套路径判断。
func extractEntry(tr *tar.Reader, hdr *tar.Header, dst string) error {
	name := filepath.Clean(hdr.Name)
	if name == "." || name == "" {
		return nil
	}
	// 建目录与硬链接的末级也须是真实目录/不存在；写文件与建符号链接时
	// 末级允许被覆盖（先删后写，不跟随）。
	finalMustDir := hdr.Typeflag == tar.TypeDir
	target, err := image.SafeExtractPath(dst, filepath.ToSlash(name), finalMustDir)
	if err != nil {
		return fmt.Errorf("convert: 归档条目 %q: %w", hdr.Name, err)
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, fs(hdr.Mode)); err != nil {
			return fmt.Errorf("convert: 创建目录 %s 失败: %w", target, err)
		}
		return nil
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("convert: 创建父目录失败: %w", err)
		}
		// **必须先删掉已存在的目标**，不能用 O_CREATE|O_TRUNC 直接写：
		// O_TRUNC 会**跟随**已存在的符号链接（内核解析到链接目标再截断），
		// 于是"rootfs 内预置一个指向宿主文件的符号链接"就构成第二条穿透路径
		// ——它绕过了父链校验（末级符号链接是允许覆盖的），却依然写到了外面。
		// 删除后重建是"替换链接本身"，不跟随。实测确认过 O_TRUNC 的行为。
		if err := removeExisting(target); err != nil {
			return fmt.Errorf("convert: 移除已存在目标 %s 失败: %w", target, err)
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, fs(hdr.Mode))
		if err != nil {
			return fmt.Errorf("convert: 创建文件 %s 失败: %w", target, err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			return fmt.Errorf("convert: 写入 %s 失败: %w", target, err)
		}
		return out.Close()
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("convert: 创建父目录失败: %w", err)
		}
		_ = os.Remove(target)
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return fmt.Errorf("convert: 创建符号链接 %s 失败: %w", target, err)
		}
		return nil
	case tar.TypeLink:
		// 硬链接的**落点**父链已由 SafeExtractPath 校验；其**源**路径同样
		// 必须校验，否则 `linkname` 可以指向 rootfs 之外（如 ../../etc/passwd），
		// 把宿主敏感文件硬链接进 rootfs 并随镜像打包外泄。
		linkRel := filepath.ToSlash(filepath.Clean(hdr.Linkname))
		src, err := image.SafeExtractPath(dst, linkRel, false)
		if err != nil {
			return fmt.Errorf("convert: 归档条目 %q 硬链接源 %q: %w", hdr.Name, hdr.Linkname, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("convert: 创建父目录失败: %w", err)
		}
		_ = os.Remove(target)
		if err := os.Link(src, target); err != nil {
			// 硬链接跨目录失败时退化为拷贝，不让整个转换失败。
			return copyFile(src, target)
		}
		return nil
	default:
		// 设备节点、FIFO 等：docker export 通常不产出，跳过并记录。
		slog.Debug("convert: 跳过归档中的特殊条目", "name", hdr.Name, "type", string(hdr.Typeflag))
		return nil
	}
}

// removeExisting 删除已存在的目标路径（文件 / 符号链接 / 空目录），
// 不存在时是 no-op。
//
// 用 Lstat 判定而不跟随符号链接：调用方的语义是"替换这个路径本身"，
// 而不是"写到它指向的地方"。这正是防 O_TRUNC 跟随链接的关键一步。
// 目录只在为空时才能被 Remove 删掉——非空目录说明归档本身自相矛盾
// （先建目录又当文件写），报错比静默递归删除安全。
func removeExisting(p string) error {
	_, err := os.Lstat(p)
	if errors.Is(err, iofs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// fs 把 tar 的 mode 转成 os.FileMode，并剥掉 setuid/setgid/sticky 位——
// 与 internal/storage 的既有安全策略一致。
func fs(mode int64) os.FileMode {
	m := os.FileMode(mode).Perm()
	return m
}

// checkDiskSpace 在拉取前做一次磁盘预检。
//
// 转换过程中同一份内容会以三种形态同时存在（docker 镜像、导出 tar、解压后的
// rootfs），因此需要大约「镜像大小 × 3」的空闲空间。镜像大小在 pull 之前
// 拿不到，所以用一个保守的下限 + 粗略估算：
//   - 先确认至少有 minFreeBytes 可用（拉小镜像也不该把磁盘写满）
//   - 若 docker 侧已缓存该镜像，用其实际大小估算并校验
func checkDiskSpace(ctx context.Context, d Docker, image, workDir, platform string) error {
	const minFreeBytes = 512 << 20 // 512 MiB 基线
	avail, err := freeBytes(workDir)
	if err != nil {
		// 拿不到磁盘信息不该阻断转换，只记录。
		slog.Debug("convert: 无法获取可用空间，跳过预检", "err", err)
		return nil
	}
	if avail < minFreeBytes {
		return fmt.Errorf("%w：%s 仅剩 %s 可用，转换至少需要 %s。"+
			"请清理空间后重试（docker system prune 或删除临时文件）",
			ErrDiskSpace, workDir, humanBytes(avail), humanBytes(minFreeBytes))
	}
	// 若镜像已在本地缓存，用真实大小做二次校验。
	out, err := d.Run(ctx, "image", "inspect", "--format", "{{.Size}}", image)
	if err != nil {
		return nil // 未缓存，无法估算，基线检查已够
	}
	size, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if perr != nil || size <= 0 {
		return nil
	}
	need := size * 3
	if avail < need {
		return fmt.Errorf("%w：镜像 %s 约 %s，转换（镜像+tar+rootfs 三份）约需 %s，"+
			"当前仅剩 %s。请清理空间后重试",
			ErrDiskSpace, image, humanBytes(size), humanBytes(need), humanBytes(avail))
	}
	return nil
}

// freeBytes 返回路径所在文件系统的可用字节数。
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// humanBytes 把字节数格式化为人类可读形式。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
