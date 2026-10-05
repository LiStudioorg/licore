// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package image

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 规范常量，见 docs/image-spec.md。
const (
	MediaTypeManifest = "application/x.licore.manifest+json"
	SpecVersionV1     = "licore/image-spec/v1"
	SchemaVersionV1   = 1

	// MaxIndexBytes 是 index.json 的体积上限（1 MiB）。
	MaxIndexBytes = 1 << 20
	// MaxConfigBytes 是 config 对象的体积上限（64 KiB）。
	MaxConfigBytes = 64 << 10
	// MaxLayers 是单镜像的层数上限。
	MaxLayers = 127
)

var (
	digestRE   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	nameRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,254}$`)
	layerNameR = regexp.MustCompile(`^layers/[0-9]{6}\.[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.tar\.gz$`)
)

// supportedOS / supportedArch 是规范 3.1 允许的枚举值。
var (
	supportedOS  = map[string]bool{"linux": true, "android": true, "darwin": true}
	supportedArc = map[string]bool{"amd64": true, "arm64": true, "arm": true, "386": true, "riscv64": true, "loong64": true}
)

// SupportedArches 返回规范 3.1 允许的 architecture 枚举值（已排序）。
// 供 CLI 的 --arch 校验复用，避免命令行白名单与规范漂移——两边一旦不一致，
// 就会出现"构建成功但 pull 立刻拒绝"的坏包。
func SupportedArches() []string {
	out := make([]string, 0, len(supportedArc))
	for a := range supportedArc {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// ValidArch 报告 a 是否为规范允许的 architecture 值。
func ValidArch(a string) bool { return supportedArc[a] }

// SupportedOSes 返回规范 3.1 允许的 os 枚举值（已排序）。
func SupportedOSes() []string {
	out := make([]string, 0, len(supportedOS))
	for o := range supportedOS {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// ValidOS 报告 o 是否为规范允许的 os 值。
func ValidOS(o string) bool { return supportedOS[o] }

// Layer 描述 index.json 中声明的一个层。
type Layer struct {
	// Path 是层在外层归档内的相对路径，形如 layers/000001.base.tar.gz。
	Path string `json:"path"`
	// Digest 是层原始字节的摘要，形如 sha256:<64 hex>。
	Digest string `json:"digest"`
	// SizeBytes 是层在归档内的未压缩字节数。
	SizeBytes int64 `json:"sizeBytes"`
	// ApplyOrder 是层的合并顺序，必须从 1 严格递增。
	ApplyOrder int `json:"applyOrder"`
}

// ConfigRef 指向镜像的运行配置小对象。
type ConfigRef struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
}

// Config 是 config 小对象的内容（规范 3.3）。未知键一律拒绝。
type Config struct {
	Entrypoint []string          `json:"entrypoint"`
	Cmd        []string          `json:"cmd"`
	Env        map[string]string `json:"env"`
	WorkingDir string            `json:"workingDir"`
	User       string            `json:"user"`
	Expose     []string          `json:"expose"`
	Volumes    []string          `json:"volumes"`
	Labels     map[string]string `json:"labels"`
}

// Manifest 对应 .licore 归档内的 index.json（规范 3.1）。
type Manifest struct {
	MediaType     string            `json:"mediaType"`
	SpecVersion   string            `json:"specVersion"`
	SchemaVersion int               `json:"schemaVersion"`
	Architecture  string            `json:"architecture"`
	OS            string            `json:"os"`
	Created       string            `json:"created"`
	Name          string            `json:"name"`
	Version       string            `json:"version"`
	Config        ConfigRef         `json:"config"`
	Layers        []Layer           `json:"layers"`
	Annotations   map[string]string `json:"annotations"`
}

// ParseManifest 严格解析 index.json 并执行规范第 4 节的清单类校验。
// 未知字段一律拒绝：index.json 是唯一元数据源，禁止"尽力猜测"式宽容解析。
func ParseManifest(data []byte) (*Manifest, error) {
	if int64(len(data)) > MaxIndexBytes {
		return nil, fmt.Errorf("index.json %d 字节 > 上限 %d: %w", len(data), MaxIndexBytes, ErrIndexTooLarge)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("index.json 解析失败: %w（%w）", err, ErrBadManifest)
	}
	if dec.More() {
		return nil, fmt.Errorf("index.json 含多个 JSON 值: %w", ErrBadManifest)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate 按规范 3.1 / 第 4 节校验清单自身（不涉及归档内容）。
func (m *Manifest) Validate() error {
	switch {
	case m.MediaType != MediaTypeManifest:
		return fmt.Errorf("mediaType=%q，期望 %q: %w", m.MediaType, MediaTypeManifest, ErrBadManifest)
	case m.SpecVersion != SpecVersionV1:
		return fmt.Errorf("specVersion=%q，期望 %q: %w", m.SpecVersion, SpecVersionV1, ErrBadManifest)
	case m.SchemaVersion != SchemaVersionV1:
		return fmt.Errorf("schemaVersion=%d，期望 %d: %w", m.SchemaVersion, SchemaVersionV1, ErrBadManifest)
	}
	if !supportedArc[m.Architecture] {
		return fmt.Errorf("architecture=%q 不受支持: %w", m.Architecture, ErrBadManifest)
	}
	if !supportedOS[m.OS] {
		return fmt.Errorf("os=%q 不受支持: %w", m.OS, ErrBadManifest)
	}
	if !strings.HasSuffix(m.Created, "Z") {
		return fmt.Errorf("created=%q 必须为 RFC 3339 UTC: %w", m.Created, ErrBadManifest)
	}
	if _, err := time.Parse(time.RFC3339, m.Created); err != nil {
		return fmt.Errorf("created=%q 非法: %w（%w）", m.Created, err, ErrBadManifest)
	}
	if !nameRE.MatchString(m.Name) {
		return fmt.Errorf("name=%q 非法: %w", m.Name, ErrBadManifest)
	}
	if m.Version == "" || strings.TrimSpace(m.Version) != m.Version {
		return fmt.Errorf("version=%q 非法: %w", m.Version, ErrBadManifest)
	}
	if err := validateDigest(m.Config.Digest); err != nil {
		return fmt.Errorf("config.digest: %w", err)
	}
	if m.Config.SizeBytes <= 0 || m.Config.SizeBytes > MaxConfigBytes {
		return fmt.Errorf("config.sizeBytes=%d 需在 1..%d 之间: %w", m.Config.SizeBytes, MaxConfigBytes, ErrBadManifest)
	}
	if len(m.Layers) == 0 || len(m.Layers) > MaxLayers {
		return fmt.Errorf("层数 %d 需在 1..%d 之间: %w", len(m.Layers), MaxLayers, ErrBadManifest)
	}

	seenPath := make(map[string]bool, len(m.Layers))
	for i, l := range m.Layers {
		if err := SafeArchivePath(l.Path); err != nil {
			return fmt.Errorf("layers[%d].path: %w", i, err)
		}
		if !strings.HasPrefix(l.Path, "layers/") || !layerNameR.MatchString(l.Path) {
			return fmt.Errorf("layers[%d].path=%q 需匹配 layers/NNNNNN.<name>.tar.gz: %w", i, l.Path, ErrBadManifest)
		}
		if seenPath[l.Path] {
			return fmt.Errorf("layers[%d].path=%q 重复: %w", i, l.Path, ErrBadManifest)
		}
		seenPath[l.Path] = true
		if err := validateDigest(l.Digest); err != nil {
			return fmt.Errorf("layers[%d].digest: %w", i, err)
		}
		if l.SizeBytes <= 0 {
			return fmt.Errorf("layers[%d].sizeBytes=%d 必须为正: %w", i, l.SizeBytes, ErrBadManifest)
		}
		if l.ApplyOrder != i+1 {
			return fmt.Errorf("layers[%d].applyOrder=%d，期望 %d: %w", i, l.ApplyOrder, i+1, ErrBadApplyOrder)
		}
	}
	for k := range m.Annotations {
		if !hasAllowedAnnotationPrefix(k) {
			return fmt.Errorf("annotations 键 %q 前缀非法: %w", k, ErrBadManifest)
		}
	}
	return nil
}

// ParseConfig 严格解析 config 小对象，未知键报错以防配置漂移静默生效。
func ParseConfig(data []byte) (*Config, error) {
	if int64(len(data)) > MaxConfigBytes {
		return nil, fmt.Errorf("config %d 字节 > 上限 %d: %w", len(data), MaxConfigBytes, ErrIndexTooLarge)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config 解析失败: %w（%w）", err, ErrBadManifest)
	}
	return &c, nil
}

// Ref 返回 name:version 形式的镜像引用。
func (m *Manifest) Ref() string { return m.Name + ":" + m.Version }

func validateDigest(s string) error {
	if !digestRE.MatchString(s) {
		return fmt.Errorf("%q 需为 sha256:<64 位小写 hex>: %w", s, ErrBadManifest)
	}
	return nil
}

func hasAllowedAnnotationPrefix(k string) bool {
	return strings.HasPrefix(k, "org.licore.") || strings.HasPrefix(k, "dev.licore.")
}

// SafeExtractPath 校验「把归档条目 rel 解压到 rootDir 之下」是否安全，
// 并返回解析后的绝对目标路径。
//
// 这是**解压类代码唯一的父链校验入口**：先做 SafeArchivePath 的条目名
// 校验，再把 rel 的每一级**已存在**父组件 lstat 一遍，要求它们是真实目录。
//
// 为什么单靠 SafeArchivePath 不够（CVE 级别的经典漏洞形态）：
// SafeArchivePath 只看**条目名本身**是否含 "/../"，看不见**文件系统上已经
// 存在**的符号链接。于是下面这个归档能穿透出去：
//
//	条目 1: linkdir/        目录
//	条目 2: linkdir/up      符号链接 → ../../victim
//	条目 3: linkdir/up/x    普通文件
//
// 条目 3 的名字完全"干净"，但 /linkdir/up 是个指向外部的符号链接，
// 写入会经它落到 rootDir 之外。**必须逐级 lstat 父组件**才能拦住。
//
// 语义细节：
//   - finalMustDir=false（默认写文件场景）：**末级**允许是已存在的符号链接
//     或普通文件——先删后写、不跟随，本身就是安全的原子替换；
//   - finalMustDir=true（建目录 / whiteout 场景）：末级也必须是真实目录或
//     不存在，否则拒绝；
//   - 不存在的父组件视为合法（调用方后续 MkdirAll 创建）。
//
// 返回的 target 已确认落在 rootDir 之内（filepath.Rel 双重校验）。
func SafeExtractPath(rootDir, rel string, finalMustDir bool) (string, error) {
	if err := SafeArchivePath(rel); err != nil {
		return "", err
	}
	target := filepath.Join(rootDir, filepath.FromSlash(rel))
	// 双保险：Join 之后必须仍在 rootDir 之内。
	back, err := filepath.Rel(rootDir, target)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("目标 %q 越出 %q: %w", rel, rootDir, ErrUnsafePath)
	}
	if err := checkParentChain(rootDir, target, finalMustDir); err != nil {
		return "", err
	}
	return target, nil
}

// checkParentChain 逐级校验 target 相对 rootDir 的路径组件。
//
// 父组件：必须不存在（待创建）或是真实目录；符号链接与普通文件一律拒绝。
// 末级：finalMustDir 为真时同父组件标准，为假时允许任意已存在类型
// （覆盖替换是安全的，见 SafeExtractPath 说明）。
func checkParentChain(rootDir, target string, finalMustDir bool) error {
	back, err := filepath.Rel(rootDir, target)
	if err != nil || back == "." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return nil // 目标就是 rootDir 自身：无父链可查
	}
	comps := strings.Split(back, string(filepath.Separator))
	cur := rootDir
	for i, comp := range comps {
		cur = filepath.Join(cur, comp)
		final := i == len(comps)-1
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil // 后续层级尚不存在
		case err != nil:
			return fmt.Errorf("检查路径组件 %q: %w", cur, err)
		case fi.Mode()&fs.ModeSymlink != 0:
			if final && !finalMustDir {
				return nil // 覆盖符号链接本身：先删后写，不跟随
			}
			return fmt.Errorf("拒绝经由符号链接解压写出 %q: %w", cur, ErrUnsafePath)
		case !fi.IsDir():
			if final && !finalMustDir {
				return nil
			}
			return fmt.Errorf("路径组件 %q 不是目录: %w", cur, ErrUnsafeLayer)
		}
	}
	return nil
}

// SafeArchivePath 校验归档条目名：必须是干净的相对路径，
// 禁止绝对路径、反斜杠、"." / ".." 段与空段（规范第 4 节规则 1）。
func SafeArchivePath(name string) error {
	if name == "" {
		return fmt.Errorf("空路径: %w", ErrUnsafePath)
	}
	if strings.HasPrefix(name, "/") {
		return fmt.Errorf("绝对路径 %q: %w", name, ErrUnsafePath)
	}
	if strings.Contains(name, "\\") {
		return fmt.Errorf("路径 %q 含反斜杠: %w", name, ErrUnsafePath)
	}
	if strings.HasPrefix(name, "./") || name == "." {
		return fmt.Errorf("路径 %q 非规范化: %w", name, ErrUnsafePath)
	}
	for _, seg := range strings.Split(name, "/") {
		switch seg {
		case "":
			return fmt.Errorf("路径 %q 含空路径段: %w", name, ErrUnsafePath)
		case ".", "..":
			return fmt.Errorf("路径 %q 含 %q 段: %w", name, seg, ErrUnsafePath)
		}
	}
	return nil
}
