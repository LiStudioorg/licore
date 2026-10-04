// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mockDocker 是一个可编程的 Docker 实现，用来在没有任何 docker 的机器上
// 测试转换流程，并能构造各种畸形/边界输出。
type mockDocker struct {
	// calls 记录收到的所有调用（"args 拼接"形式）。
	calls [][]string
	// responses 按子命令（args[0]）给出 stdout 与错误。
	responses map[string]mockResponse
	// exportTar 是 export 子命令要写出的 tar 内容（已构造好的字节）。
	exportTar []byte
	// onRun 可选：允许测试按调用动态返回，优先于 responses。
	onRun func(args []string) ([]byte, error)
}

type mockResponse struct {
	out []byte
	err error
}

func (m *mockDocker) Run(_ context.Context, args ...string) ([]byte, error) {
	m.calls = append(m.calls, args)
	if m.onRun != nil {
		return m.onRun(args)
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("空调用")
	}
	if r, ok := m.responses[args[0]]; ok {
		return r.out, r.err
	}
	return []byte(""), nil
}

func (m *mockDocker) RunToFile(_ context.Context, path string, args ...string) error {
	m.calls = append(m.calls, args)
	if len(args) == 0 {
		return fmt.Errorf("空调用")
	}
	if r, ok := m.responses[args[0]]; ok && r.err != nil {
		return r.err
	}
	if m.exportTar != nil {
		return os.WriteFile(path, m.exportTar, 0o644)
	}
	return os.WriteFile(path, []byte(""), 0o644)
}

// calledWith 报告 mock 是否收到过以 prefix 开头的调用。
func (m *mockDocker) calledWith(prefix ...string) bool {
	for _, c := range m.calls {
		if len(c) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if c[i] != p {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// -------- inspect 解析 --------

func TestParseInspect(t *testing.T) {
	data := []byte(`[{
	  "Os": "linux",
	  "Config": {
	    "User": "1000:1000",
	    "Env": ["PATH=/usr/bin", "GOPATH=/go"],
	    "Entrypoint": ["/docker-entrypoint.sh"],
	    "Cmd": ["nginx", "-g", "daemon off;"],
	    "WorkingDir": "/app",
	    "ExposedPorts": {"80/tcp": {}, "443/tcp": {}},
	    "Volumes": {"/data": {}},
	    "Labels": {"maintainer": "someone", "org.opencontainers.image.title": "x"}
	  }
	}]`)
	ins, err := parseInspect(data)
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	if ins.Os != "linux" {
		t.Errorf("os = %q", ins.Os)
	}
	if got := ins.Config.WorkingDir; got != "/app" {
		t.Errorf("workdir = %q", got)
	}
	if len(ins.Config.Entrypoint) != 1 || ins.Config.Entrypoint[0] != "/docker-entrypoint.sh" {
		t.Errorf("entrypoint = %v", ins.Config.Entrypoint)
	}
	if len(ins.Config.ExposedPorts) != 2 {
		t.Errorf("exposedPorts = %v", ins.Config.ExposedPorts)
	}
}

func TestParseInspectRejectsEmptyAndMalformed(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"空数组", `[]`},
		{"非法 JSON", `{not json`},
		{"空输入", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseInspect([]byte(tc.in)); err == nil {
				t.Fatal("期望报错")
			}
		})
	}
}

// -------- Boxfile 生成 --------

// mustParseGenerated 断言生成的 Boxfile 能被 LiCore 解析器接受，
// 这是生成逻辑最重要的正确性约束。
func mustParseGenerated(t *testing.T, text string) {
	t.Helper()
	if _, err := parseBoxfileText(t, text); err != nil {
		t.Fatalf("生成的 Boxfile 无法解析: %v\n---\n%s", err, text)
	}
}

func TestGenerateBoxfileBasic(t *testing.T) {
	ins := &inspectResult{}
	ins.Config.Entrypoint = []string{"/docker-entrypoint.sh"}
	ins.Config.Cmd = []string{"nginx", "-g", "daemon off;"}
	ins.Config.Env = []string{"PATH=/usr/local/bin:/usr/bin", "NGINX_VERSION=1.27.0"}
	ins.Config.WorkingDir = "/app"
	ins.Config.User = "1000:1000"
	ins.Config.ExposedPorts = map[string]struct{}{"80/tcp": {}}
	ins.Config.Volumes = map[string]struct{}{"/data": {}}
	ins.Config.Labels = map[string]string{"maintainer": "team"}

	text := generateBoxfile(ins, genOptions{
		TopDirs:  []string{"bin", "etc", "usr"},
		TopFiles: []string{"init.sh"},
	})
	mustParseGenerated(t, text)

	for _, want := range []string{
		"FROM scratch",
		"COPY bin /bin",
		"COPY etc /etc",
		"COPY usr /usr",
		"COPY init.sh /init.sh",
		"ENV PATH=/usr/local/bin:/usr/bin",
		"ENV NGINX_VERSION=1.27.0",
		"WORKDIR /app",
		"USER 1000:1000",
		"EXPOSE 80/tcp",
		"VOLUME /data",
		"LABEL maintainer=team",
		`ENTRYPOINT ["/docker-entrypoint.sh"]`,
		`CMD ["nginx","-g","daemon off;"]`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("生成内容缺少 %q\n---\n%s", want, text)
		}
	}
}

// TestGenerateBoxfileSkipsDollarBraceEnv 关键回归：ENV 值里的 ${...} 必须被跳过。
//
// LiCore 的解析器对 "${" 无条件做变量展开且没有任何转义语法
// （实测 `\${X}` 与 `$${X}` 都会报"未声明的变量"），所以这类值无法无损
// 表达。跳过并计数由调用方警告——绝不静默改写成别的值。
func TestGenerateBoxfileSkipsDollarBraceEnv(t *testing.T) {
	ins := &inspectResult{}
	ins.Config.Env = []string{
		"JAVA_OPTS=-Xmx${MEMORY}",
		"PLAIN=hello",
		"BARE=$HOME", // 裸 $ 是合法的，必须保留
	}

	text := generateBoxfile(ins, genOptions{TopDirs: []string{"bin"}})
	mustParseGenerated(t, text)

	if strings.Contains(text, "${MEMORY}") {
		t.Errorf("含 ${} 的 ENV 应被跳过而不是写进去:\n%s", text)
	}
	for _, want := range []string{"ENV PLAIN=hello", "ENV BARE=$HOME"} {
		if !strings.Contains(text, want) {
			t.Errorf("应保留 %q\n---\n%s", want, text)
		}
	}
	if n, _ := countSkipped(ins); n != 1 {
		t.Errorf("envSkipped = %d, want 1", n)
	}
}

// TestUnexpressibleCommandRejectsDollarBrace CMD/ENTRYPOINT 含 ${} 时必须
// 明确失败：丢掉 ENTRYPOINT 会产出跑不起来的镜像，比转换失败更糟。
func TestUnexpressibleCommandRejectsDollarBrace(t *testing.T) {
	ins := &inspectResult{}
	ins.Config.Cmd = []string{"/bin/sh", "-c", "echo ${HOME}"}
	op, arg, bad := unexpressibleCommand(ins)
	if !bad {
		t.Fatal("含 ${} 的 CMD 应被识别为无法表达")
	}
	if op != "CMD" || arg != "echo ${HOME}" {
		t.Errorf("op=%q arg=%q", op, arg)
	}

	// 裸 $ 不受影响。
	ok := &inspectResult{}
	ok.Config.Cmd = []string{"/bin/sh", "-c", "echo $HOME"}
	if _, _, bad := unexpressibleCommand(ok); bad {
		t.Error("裸 $ 不应被判为无法表达")
	}
	if text := generateBoxfile(ok, genOptions{}); !strings.Contains(text, "echo $HOME") {
		t.Errorf("裸 $ 应原样保留:\n%s", text)
	}
	mustParseGenerated(t, generateBoxfile(ok, genOptions{}))
}

func TestGenerateBoxfileSkipsUnrepresentable(t *testing.T) {
	ins := &inspectResult{}
	ins.Config.Env = []string{
		"GOOD=1",
		"EMPTY=",          // 值为空：LiCore 的 ENV 不接受
		"NOEQUALS",        // 没有 '='：Docker 侧也罕见，跳过
		"BAD=a=b",         // 值含 '='：LiCore 的 ENV 不接受
		"WITHSPACE=a b c", // 值含空格：合法，必须保留
	}
	ins.Config.Labels = map[string]string{
		"ok":          "v",
		"empty":       "",          // 空值：跳过
		"withequals":  "a=b",       // 值含 '='：跳过
		"bad key":     "v",         // 键含空格：跳过
		"good.label":  "value",     // 合法：保留
		"another.key": "another v", // 值含空格：合法
	}

	text := generateBoxfile(ins, genOptions{TopDirs: []string{"etc"}})
	mustParseGenerated(t, text)

	for _, want := range []string{"ENV GOOD=1", "ENV WITHSPACE=a b c", "LABEL ok=v", "LABEL good.label=value"} {
		if !strings.Contains(text, want) {
			t.Errorf("应保留 %q\n---\n%s", want, text)
		}
	}
	for _, bad := range []string{"EMPTY=", "BAD=a=b", "withequals", "bad key"} {
		if strings.Contains(text, bad) {
			t.Errorf("不应出现 %q\n---\n%s", bad, text)
		}
	}

	envSkipped, labelSkipped := countSkipped(ins)
	if envSkipped != 3 {
		t.Errorf("envSkipped = %d, want 3", envSkipped)
	}
	if labelSkipped != 3 {
		t.Errorf("labelSkipped = %d, want 3", labelSkipped)
	}
}

func TestGenerateBoxfileDropsRelativeWorkdir(t *testing.T) {
	ins := &inspectResult{}
	ins.Config.WorkingDir = "relative/path" // 非绝对路径：LiCore 不接受
	text := generateBoxfile(ins, genOptions{TopDirs: []string{"bin"}})
	if strings.Contains(text, "WORKDIR") {
		t.Errorf("相对 WORKDIR 应被丢弃:\n%s", text)
	}
	mustParseGenerated(t, text)
}

func TestGenerateBoxfileJSONArrayEscaping(t *testing.T) {
	ins := &inspectResult{}
	// 命令里含引号与反斜杠：必须走 JSON 编码而不是手工拼接。
	ins.Config.Cmd = []string{"/bin/sh", "-c", `echo "hi\there"`}
	text := generateBoxfile(ins, genOptions{})
	mustParseGenerated(t, text)
	if !strings.Contains(text, `CMD ["/bin/sh","-c","echo \"hi\\there\""]`) {
		t.Errorf("JSON 数组转义不正确:\n%s", text)
	}
}

func TestGenerateBoxfileNoEntries(t *testing.T) {
	ins := &inspectResult{}
	text := generateBoxfile(ins, genOptions{})
	if strings.TrimSpace(text) != "FROM scratch" {
		t.Errorf("空 inspect 只应生成 FROM scratch，得到:\n%s", text)
	}
	mustParseGenerated(t, text)
}

// -------- 镜像引用派生 --------

func TestDeriveRef(t *testing.T) {
	cases := []struct {
		in, wantName, wantVer string
	}{
		{"alpine:3.20", "alpine", "3.20"},
		{"alpine", "alpine", "latest"},
		{"library/nginx:1.27", "library/nginx", "1.27"},
		{"registry:5000/nginx:1.27", "registry:5000/nginx", "1.27"},
		{"ghcr.io/org/app:v1.2.3", "ghcr.io/org/app", "v1.2.3"},
		{"alpine@sha256:abc123", "alpine", "latest"},
		{"alpine:3.20@sha256:abc", "alpine", "3.20"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			name, ver := deriveRef(tc.in, "")
			if name != tc.wantName || ver != tc.wantVer {
				t.Errorf("deriveRef(%q) = %q:%q, want %q:%q", tc.in, name, ver, tc.wantName, tc.wantVer)
			}
		})
	}
}

func TestDeriveRefExplicitVersionWins(t *testing.T) {
	name, ver := deriveRef("alpine:3.20", "custom")
	if name != "alpine" || ver != "custom" {
		t.Errorf("显式版本应覆盖，得到 %q:%q", name, ver)
	}
}

// -------- 顶层扫描 --------

func TestTopLevelEntries(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"bin", "etc", "usr"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"init.sh", ".dockerenv"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dirs, files, err := topLevelEntries(dir)
	if err != nil {
		t.Fatalf("topLevelEntries: %v", err)
	}
	if strings.Join(dirs, ",") != "bin,etc,usr" {
		t.Errorf("dirs = %v", dirs)
	}
	// .dockerenv 是 docker 运行时产物，不应进镜像。
	if strings.Join(files, ",") != "init.sh" {
		t.Errorf("files = %v（.dockerenv 应被跳过）", files)
	}
}

// -------- Docker 可用性检查 --------

func TestCheckDockerMissing(t *testing.T) {
	// 用一个必然不存在的 PATH 让 LookPath 失败。
	t.Setenv("PATH", "/nonexistent-path-for-test")
	err := CheckDocker(context.Background(), &mockDocker{})
	if err == nil {
		t.Fatal("docker 缺失应报错")
	}
	if !strings.Contains(err.Error(), "请先安装 docker") {
		t.Errorf("错误应提示安装 docker: %v", err)
	}
}

func TestCheckDockerPermissionDenied(t *testing.T) {
	m := &mockDocker{responses: map[string]mockResponse{
		"version": {err: fmt.Errorf("permission denied while trying to connect to the docker API")},
	}}
	err := CheckDocker(context.Background(), m)
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "无权访问 docker socket") {
		t.Errorf("应识别为权限问题: %v", err)
	}
	if !strings.Contains(err.Error(), "sudo") {
		t.Errorf("应给出可执行的修复提示: %v", err)
	}
}

func TestCheckDockerDaemonDown(t *testing.T) {
	m := &mockDocker{responses: map[string]mockResponse{
		"version": {err: fmt.Errorf("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")},
	}}
	err := CheckDocker(context.Background(), m)
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "systemctl start docker") {
		t.Errorf("应提示启动 daemon: %v", err)
	}
}

func TestCheckDockerOK(t *testing.T) {
	m := &mockDocker{responses: map[string]mockResponse{
		"version": {out: []byte("29.8.1\n")},
	}}
	if err := CheckDocker(context.Background(), m); err != nil {
		t.Fatalf("docker 可用时不应报错: %v", err)
	}
}

// -------- 归档解压与逃逸防护 --------

func TestExtractTarRejectsEscape(t *testing.T) {
	for _, tc := range []struct{ name, entry string }{
		{"上跳", "../evil"},
		{"绝对路径", "/etc/evil"},
		{"深层上跳", "a/../../evil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tarPath := filepath.Join(dir, "t.tar")
			if err := writeTar(tarPath, map[string]string{tc.entry: "x"}); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(dir, "out")
			if err := os.MkdirAll(dst, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := extractTar(tarPath, dst); err == nil {
				t.Fatalf("条目 %q 必须被拒绝", tc.entry)
			}
			// 目标目录外不得出现文件。
			if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
				t.Error("逃逸文件被写出了目标目录")
			}
		})
	}
}

// -------- 磁盘与格式化辅助 --------

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{3 * 1024 * 1024 * 1024, "3.0 GiB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFreeBytes(t *testing.T) {
	n, err := freeBytes(t.TempDir())
	if err != nil {
		t.Fatalf("freeBytes: %v", err)
	}
	if n <= 0 {
		t.Errorf("可用空间应为正数，得到 %d", n)
	}
}
