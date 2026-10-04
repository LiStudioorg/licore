// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// errString 是最小的 error 实现，用于构造失败的 batchResult。
type errString string

func (e errString) Error() string { return string(e) }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}

// testCmd 返回一个零值 cobra 命令：它的 Context() 为 nil，
// runBatchConvert 会退化为 context.Background()，正好够参数校验类用例使用。
func testCmd() *cobra.Command { return &cobra.Command{} }

// -------- 清单解析 --------

// TestParseImageListIgnoresCommentsAndBlanks 验证 # 注释、空行、前后空白被忽略。
func TestParseImageListIgnoresCommentsAndBlanks(t *testing.T) {
	in := `
# 这是注释
alpine:3.20

   nginx:1.27-alpine
	# 缩进的注释也要忽略
  #  还有前面带空格的注释
redis:7
`
	items, err := parseImageList(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseImageList: %v", err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.image)
	}
	want := "alpine:3.20,nginx:1.27-alpine,redis:7"
	if strings.Join(got, ",") != want {
		t.Errorf("解析结果 = %v，期望 %s", got, want)
	}
}

// TestParseImageListTrimsWhitespace 验证前后空白被去掉（否则 docker 会收到带空格的名字）。
func TestParseImageListTrimsWhitespace(t *testing.T) {
	items, err := parseImageList(strings.NewReader("   alpine:3.20   \t\n\t nginx:1.27 \t\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("项数 = %d，期望 2", len(items))
	}
	if items[0].image != "alpine:3.20" || items[1].image != "nginx:1.27" {
		t.Errorf("空白未去干净: %q, %q", items[0].image, items[1].image)
	}
}

// TestParseImageListDeduplicates 验证去重（同一镜像转两遍纯属浪费）。
func TestParseImageListDeduplicates(t *testing.T) {
	items, err := parseImageList(strings.NewReader("alpine:3.20\nnginx:1.27\nalpine:3.20\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("项数 = %d，期望 2（应按首次出现去重）", len(items))
	}
	if items[0].image != "alpine:3.20" || items[1].image != "nginx:1.27" {
		t.Errorf("去重后应保留首次出现顺序: %v", items)
	}
}

// TestParseImageListRecordsLineNumbers 验证记录行号，便于报错定位。
func TestParseImageListRecordsLineNumbers(t *testing.T) {
	items, err := parseImageList(strings.NewReader("# 1\nalpine:3.20\n\nnginx:1.27\n"))
	if err != nil {
		t.Fatal(err)
	}
	if items[0].line != 2 {
		t.Errorf("alpine 行号 = %d，期望 2", items[0].line)
	}
	if items[1].line != 4 {
		t.Errorf("nginx 行号 = %d，期望 4", items[1].line)
	}
}

// TestParseImageListEmpty 验证只有注释/空行时返回空清单（由调用方报错）。
func TestParseImageListEmpty(t *testing.T) {
	for _, in := range []string{"", "\n\n", "# only comments\n#more\n", "   \n\t\n"} {
		items, err := parseImageList(strings.NewReader(in))
		if err != nil {
			t.Errorf("输入 %q 不应报错: %v", in, err)
		}
		if len(items) != 0 {
			t.Errorf("输入 %q 应得到空清单，得到 %v", in, items)
		}
	}
}

// TestParseImageListHandlesLongLines 验证超长行（带 registry 的长引用）不被截断。
func TestParseImageListHandlesLongLines(t *testing.T) {
	long := "registry.example.com/" + strings.Repeat("a", 500) + ":v1"
	items, err := parseImageList(strings.NewReader(long + "\n"))
	if err != nil {
		t.Fatalf("超长行应被接受: %v", err)
	}
	if len(items) != 1 || items[0].image != long {
		t.Errorf("超长行被截断或丢失")
	}
}

// -------- 输出文件名 --------

// TestBatchOutputPath 验证产物命名规则。
//
// 约定：<repo 最后一段>-<arch>.licore，**不含 tag**。
func TestBatchOutputPath(t *testing.T) {
	cases := []struct {
		image, arch, want string
	}{
		{"alpine:3.20", "amd64", "alpine-amd64.licore"},
		{"library/nginx:1.27", "amd64", "nginx-amd64.licore"},
		{"ghcr.io/org/app:v1.2.3", "arm64", "app-arm64.licore"},
		{"registry:5000/x:1", "amd64", "x-amd64.licore"},
		{"alpine", "amd64", "alpine-amd64.licore"},
		{"alpine@sha256:abc123", "amd64", "alpine-amd64.licore"},
	}
	for _, tc := range cases {
		t.Run(tc.image+"/"+tc.arch, func(t *testing.T) {
			got := batchOutputPath("/dist", tc.image, tc.arch)
			if want := "/dist/" + tc.want; got != want {
				t.Errorf("batchOutputPath = %q，期望 %q", got, want)
			}
		})
	}
}

// TestBatchOutputPathSanitizes 验证文件名里的非法字符被替换、不产生目录层级。
func TestBatchOutputPathSanitizes(t *testing.T) {
	got := batchOutputPath("/dist", "my repo/app:v1", "amd64")
	if strings.Contains(got, "/dist/my repo") || strings.Contains(got[len("/dist/"):], "/") {
		t.Errorf("非法字符未清理或产生了目录层级: %q", got)
	}
	if !strings.HasSuffix(got, ".licore") {
		t.Errorf("应以 .licore 结尾: %q", got)
	}
}

// TestCheckOutputCollisions 验证重名前置检查。
//
// 这是**防静默数据丢失**的用例：产物名不含 tag，nginx:1.26 与 nginx:1.27
// 会映射到同一个文件；若不检查，后转的会覆盖先转的，用户以为拿到两个产物。
func TestCheckOutputCollisions(t *testing.T) {
	// 无冲突：不同仓库名。
	ok := []batchItem{{image: "alpine:3.20"}, {image: "nginx:1.27"}}
	if err := checkOutputCollisions(ok, "/dist", "amd64"); err != nil {
		t.Errorf("不同仓库名不应判为重名: %v", err)
	}

	// 冲突：同仓库不同 tag。
	bad := []batchItem{{image: "nginx:1.26"}, {image: "nginx:1.27"}}
	err := checkOutputCollisions(bad, "/dist", "amd64")
	if err == nil {
		t.Fatal("同仓库不同 tag 会产出同一文件，必须报错")
	}
	for _, want := range []string{"nginx:1.26", "nginx:1.27", "nginx-amd64.licore"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q 便于定位: %v", want, err)
		}
	}

	// 同名但不同平台前缀（library/nginx 与 nginx）也会冲突——两者都取最后一段。
	alsoBad := []batchItem{{image: "library/nginx:1.27"}, {image: "nginx:1.27"}}
	if err := checkOutputCollisions(alsoBad, "/dist", "amd64"); err == nil {
		t.Error("library/nginx 与 nginx 取同一段仓库名，应判为重名")
	}
}

// -------- 汇总与失败清单 --------

// TestReportBatchAllSucceeded 验证全成功时的汇总与 failed.txt 清理。
func TestReportBatchAllSucceeded(t *testing.T) {
	dir := t.TempDir()
	// 预置一个上一轮遗留的 failed.txt，成功运行后必须被清掉（否则误导用户）。
	writeFile(t, dir+"/failed.txt", "stale\told error\n")

	var out strings.Builder
	items := []batchItem{{image: "alpine:3.20"}, {image: "nginx:1.27"}}
	results := []batchResult{
		{item: items[0], path: dir + "/alpine-amd64.licore", size: 1024},
		{item: items[1], path: dir + "/nginx-amd64.licore", size: 2048},
	}
	err := reportBatch(&out, batchOptions{outputDir: dir}, items, results)
	if err != nil {
		t.Fatalf("全成功不应返回错误: %v", err)
	}
	got := out.String()
	for _, want := range []string{"成功 2 个", "失败 0 个", "3.0 KiB"} {
		if !strings.Contains(got, want) {
			t.Errorf("汇总缺少 %q:\n%s", want, got)
		}
	}
	if fileExists(dir + "/failed.txt") {
		t.Error("成功运行后应清除上一轮遗留的 failed.txt")
	}
}

// TestReportBatchAllFailedStillSummarizes 验证**全部失败也要输出汇总**。
//
// 这是补充要求 5：全败时用户更需要知道发生了什么，不能静默退出。
func TestReportBatchAllFailedStillSummarizes(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	items := []batchItem{{image: "bad:1"}, {image: "worse:2"}}
	results := []batchResult{
		{item: items[0], err: errString("拉取失败")},
		{item: items[1], err: errString("镜像不存在")},
	}
	err := reportBatch(&out, batchOptions{outputDir: dir}, items, results)
	if err == nil {
		t.Fatal("有失败项时应返回错误（供退出码使用）")
	}
	got := out.String()
	if !strings.Contains(got, "成功 0 个") || !strings.Contains(got, "失败 2 个") {
		t.Errorf("全败也必须输出汇总:\n%s", got)
	}
	if !strings.Contains(got, "bad:1") || !strings.Contains(got, "worse:2") {
		t.Errorf("汇总应逐条列出失败镜像:\n%s", got)
	}
}

// TestReportBatchWritesFailedFile 验证 failed.txt 的格式为 "<image>\t<error>"。
func TestReportBatchWritesFailedFile(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	items := []batchItem{{image: "alpine:3.20"}, {image: "bad:1"}}
	results := []batchResult{
		{item: items[0], size: 100},
		{item: items[1], err: errString("manifest unknown")},
	}
	_ = reportBatch(&out, batchOptions{outputDir: dir}, items, results)

	content := readFile(t, dir+"/failed.txt")
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) != 1 {
		t.Fatalf("failed.txt 应只有 1 行（只有 1 个失败），得到 %d 行:\n%s", len(lines), content)
	}
	parts := strings.SplitN(lines[0], "\t", 2)
	if len(parts) != 2 {
		t.Fatalf("failed.txt 行应为 <image>\\t<error> 形式，得到 %q", lines[0])
	}
	if parts[0] != "bad:1" {
		t.Errorf("第一列应为镜像名，得到 %q", parts[0])
	}
	if !strings.Contains(parts[1], "manifest unknown") {
		t.Errorf("第二列应为错误，得到 %q", parts[1])
	}
}

// TestOneLineCollapsesMultilineErrors 验证多行错误被压成单行，
// 否则 failed.txt 的"一行一条记录"就会被破坏。
func TestOneLineCollapsesMultilineErrors(t *testing.T) {
	got := oneLine("line1\nline2\r\nline3\twith\ttabs")
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("换行未被压平: %q", got)
	}
	if !strings.HasPrefix(got, "line1 line2 line3") {
		t.Errorf("内容被破坏: %q", got)
	}
}

// -------- 参数校验 --------

// TestBatchRejectsBadJobs 验证 --jobs < 1 被拒绝。
func TestBatchRejectsBadJobs(t *testing.T) {
	dir := t.TempDir()
	list := dir + "/images.txt"
	writeFile(t, list, "alpine:3.20\n")

	for _, j := range []int{0, -1} {
		err := runBatchConvert(testCmd(), &strings.Builder{}, batchOptions{
			fromFile: list, outputDir: dir, jobs: j,
		})
		if err == nil {
			t.Errorf("--jobs=%d 应被拒绝", j)
			continue
		}
		if !strings.Contains(err.Error(), "jobs") {
			t.Errorf("--jobs=%d 的错误应提到 jobs: %v", j, err)
		}
	}
}

// TestBatchRejectsMissingOutputDir 验证批量模式必须指定 --output-dir。
func TestBatchRejectsMissingOutputDir(t *testing.T) {
	err := runBatchConvert(testCmd(), &strings.Builder{}, batchOptions{fromFile: "x.txt", jobs: 1})
	if err == nil || !strings.Contains(err.Error(), "output-dir") {
		t.Errorf("缺少 --output-dir 应报错并提示该参数: %v", err)
	}
}

// TestBatchRejectsEmptyList 验证空清单给出可读提示。
func TestBatchRejectsEmptyList(t *testing.T) {
	dir := t.TempDir()
	list := dir + "/images.txt"
	writeFile(t, list, "# 只有注释\n\n")
	err := runBatchConvert(testCmd(), &strings.Builder{}, batchOptions{
		fromFile: list, outputDir: dir, jobs: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "没有任何镜像") {
		t.Errorf("空清单应报错并说明原因: %v", err)
	}
}

// TestBatchRejectsMissingListFile 验证清单文件不存在时报错。
func TestBatchRejectsMissingListFile(t *testing.T) {
	dir := t.TempDir()
	err := runBatchConvert(testCmd(), &strings.Builder{}, batchOptions{
		fromFile: dir + "/nope.txt", outputDir: dir, jobs: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "打开镜像清单") {
		t.Errorf("清单不存在应报错: %v", err)
	}
}

// TestBatchValidatesArch 验证批量分支也做 --arch 校验（与单镜像分支一致）。
//
// 之前批量分支在校验之前就 return 了，导致 --arch bogus 会一路走到 docker
// 才暴露；这条用例把这个缺口钉住。
func TestBatchValidatesArch(t *testing.T) {
	dir := t.TempDir()
	list := dir + "/images.txt"
	writeFile(t, list, "alpine:3.20\n")
	err := runBatchConvert(testCmd(), &strings.Builder{}, batchOptions{
		fromFile: list, outputDir: dir, arch: "not-an-arch", jobs: 1,
	})
	if err == nil {
		t.Fatal("非法 --arch 应被拒绝")
	}
	if !strings.Contains(err.Error(), "不受支持") {
		t.Errorf("错误应说明架构不受支持: %v", err)
	}
}
