// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/licore/internal/convert"
	"github.com/LiStudioorg/licore/internal/image"
)

// batchOptions 是批量转换的参数。
type batchOptions struct {
	fromFile  string
	outputDir string
	arch      string
	jobs      int
	noCleanup bool
	keepImage bool
	dataDir   string
}

// batchItem 是清单里的一行（已去重、去空白）。
type batchItem struct {
	image string // docker 镜像引用
	line  int    // 在清单文件中的行号（1 起），报错时便于定位
}

// batchResult 是单个镜像的转换结果。
type batchResult struct {
	item batchItem
	path string // 产出的 .licore 路径（成功时）
	ref  string
	size int64
	err  error
}

// parseImageList 解析镜像清单：忽略空行与 # 注释，去掉前后空白并去重
// （保留首次出现的顺序，重复项只转一次——同一镜像转多遍纯属浪费）。
func parseImageList(r io.Reader) ([]batchItem, error) {
	var items []batchItem
	seen := make(map[string]bool)
	sc := bufio.NewScanner(r)
	// 单行可能很长（带 registry 的长引用），放宽默认 64 KiB 上限。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if seen[text] {
			continue
		}
		seen[text] = true
		items = append(items, batchItem{image: text, line: line})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取镜像清单失败: %w", err)
	}
	return items, nil
}

// runBatchConvert 执行批量转换。
//
// 语义：
//   - 单个失败不中断，失败的镜像记入 <output-dir>/failed.txt（"<image>\t<error>"）；
//   - 失败不污染 dist/：产物先写临时文件，成功才 rename 到最终路径；
//   - --jobs > 1 时并发转换，打印加锁避免日志交错；
//   - 全部失败也照常输出汇总与退出（不静默）。
func runBatchConvert(cmd *cobra.Command, out io.Writer, o batchOptions) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if o.outputDir == "" {
		return fmt.Errorf("convert: 批量模式必须用 --output-dir 指定输出目录")
	}
	// 与单镜像分支保持一致的架构校验：否则错误要一路走到 docker 才暴露。
	if o.arch != "" && !image.ValidArch(o.arch) {
		return fmt.Errorf("convert: --arch %q 不受支持，可选值：%s",
			o.arch, strings.Join(image.SupportedArches(), " / "))
	}
	if o.jobs < 1 {
		return fmt.Errorf("convert: --jobs 必须 >= 1，得到 %d", o.jobs)
	}

	f, err := os.Open(o.fromFile)
	if err != nil {
		return fmt.Errorf("convert: 打开镜像清单 %s 失败: %w", o.fromFile, err)
	}
	defer func() { _ = f.Close() }()

	items, err := parseImageList(f)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("convert: 镜像清单 %s 里没有任何镜像（空行与 # 注释会被忽略）", o.fromFile)
	}
	if err := os.MkdirAll(o.outputDir, 0o755); err != nil {
		return fmt.Errorf("convert: 创建输出目录 %s 失败: %w", o.outputDir, err)
	}

	arch := o.arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	// 前置重名检查：产物名是 <repo>-<arch>、不含 tag，因此 nginx:1.26 与
	// nginx:1.27 会映射到同一个文件。在动手转换**之前**报错，避免转了一半
	// 才发现产物互相覆盖（那是静默数据丢失）。
	if err := checkOutputCollisions(items, o.outputDir, arch); err != nil {
		return err
	}
	fmt.Fprintf(out, "批量转换 %d 个镜像（架构 %s，并发 %d）\n", len(items), arch, o.jobs)

	// 打印加锁：并发时多个 goroutine 同时写 out 会让进度行交错。
	var mu sync.Mutex
	printf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(out, format, args...)
	}

	results := make([]batchResult, len(items))
	index := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < o.jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range index {
				results[i] = convertOne(ctx, o, items[i], arch, printf)
			}
		}()
	}
	for i := range items {
		index <- i
	}
	close(index)
	wg.Wait()

	return reportBatch(out, o, items, results)
}

// convertOne 转换单个镜像，并把产物原子地落到输出目录。
func convertOne(ctx context.Context, o batchOptions, item batchItem, arch string, printf func(string, ...any)) batchResult {
	res := batchResult{item: item}
	printf("→ [%s] 开始转换\n", item.image)

	// 先转到临时文件，成功后再 rename 到最终路径：
	// 中途失败时 dist/ 里不会留下半成品 .licore。
	tmpdir, err := os.MkdirTemp("", "licore-batch-")
	if err != nil {
		res.err = fmt.Errorf("创建临时目录失败: %w", err)
		printf("✗ [%s] %v\n", item.image, res.err)
		return res
	}
	defer func() { _ = os.RemoveAll(tmpdir) }()

	tmpOut := filepath.Join(tmpdir, "out.licore")
	conv, err := convert.Convert(ctx, &convert.Options{
		Image:           item.image,
		Arch:            o.arch,
		OutPath:         tmpOut,
		DataDir:         o.dataDir,
		NoCleanup:       o.noCleanup,
		KeepDockerImage: o.keepImage,
	})
	if err != nil {
		res.err = err
		printf("✗ [%s] 失败：%v\n", item.image, err)
		return res
	}

	final := batchOutputPath(o.outputDir, item.image, arch)
	if err := moveFile(tmpOut, final); err != nil {
		res.err = fmt.Errorf("写出 %s 失败: %w", final, err)
		printf("✗ [%s] %v\n", item.image, res.err)
		return res
	}

	fi, statErr := os.Stat(final)
	if statErr != nil {
		res.err = fmt.Errorf("读取产物信息失败: %w", statErr)
		printf("✗ [%s] %v\n", item.image, res.err)
		return res
	}
	res.path = final
	res.ref = conv.Ref
	res.size = fi.Size()
	printf("✓ [%s] 完成 → %s（%s）\n", item.image, final, humanSize(res.size))
	return res
}

// reportBatch 打印汇总，写出 failed.txt，并返回错误（有失败时）。
func reportBatch(out io.Writer, o batchOptions, items []batchItem, results []batchResult) error {
	var (
		okCount, failCount int
		totalBytes         int64
		failed             []batchResult
	)
	for _, r := range results {
		if r.err != nil {
			failCount++
			failed = append(failed, r)
			continue
		}
		okCount++
		totalBytes += r.size
	}

	failedPath := filepath.Join(o.outputDir, "failed.txt")
	if len(failed) > 0 {
		var b strings.Builder
		for _, r := range failed {
			// 格式固定为 "<image>\t<error>"，便于脚本按制表符切分。
			fmt.Fprintf(&b, "%s\t%s\n", r.item.image, oneLine(r.err.Error()))
		}
		if err := os.WriteFile(failedPath, []byte(b.String()), 0o644); err != nil {
			return fmt.Errorf("convert: 写失败清单 %s 失败: %w", failedPath, err)
		}
	} else {
		// 上一轮遗留的 failed.txt 会误导用户，成功时清掉。
		_ = os.Remove(failedPath)
	}

	// 汇总必须无条件输出：全部失败时更需要告诉用户发生了什么。
	fmt.Fprintf(out, "\n批量转换完成：成功 %d 个 / 失败 %d 个 / 总计 %s\n",
		okCount, failCount, humanSize(totalBytes))
	if failCount > 0 {
		fmt.Fprintf(out, "失败清单：%s\n", failedPath)
		for _, r := range failed {
			fmt.Fprintf(out, "  - %s：%s\n", r.item.image, oneLine(r.err.Error()))
		}
		return fmt.Errorf("convert: %d 个镜像转换失败（成功 %d 个）", failCount, okCount)
	}
	fmt.Fprintf(out, "产物目录：%s\n", o.outputDir)
	return nil
}

// oneLine 把多行错误压成单行，保证 failed.txt 每行一条记录。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
}

// moveFile 把 src 原子地移到 dst（同文件系统内 rename）。
// 跨设备时回退为「拷贝到临时文件 + rename」，同样不留下半成品。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, in); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
