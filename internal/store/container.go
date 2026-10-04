// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// stoppedByName 是 `licore stop` 留下的用户停止标记文件名（空文件，存在即有效）。
const stoppedByName = "stopped-by-user"

// Restart 是容器重启策略，取值与 `licore run --restart` 一致。
type Restart string

// 重启策略枚举（AGENTS.md《容器自启动标志》）。
const (
	RestartNo           Restart = "no"
	RestartAlways       Restart = "always"
	RestartUnlessStoped Restart = "unless-stopped"
	RestartOnFailure    Restart = "on-failure"
)

// Valid 报告策略取值是否合法。
func (r Restart) Valid() bool {
	switch r {
	case RestartNo, RestartAlways, RestartUnlessStoped, RestartOnFailure:
		return true
	}
	return false
}

// BootEligible 报告该策略下 `licore boot` 开机是否应拉起本容器
// （on-failure 只在 shim 内做失败重启，不参与开机自启）。
func (r Restart) BootEligible() bool {
	return r == RestartAlways || r == RestartUnlessStoped
}

// Mount 是一条容器卷挂载（已解析出宿主源路径）。
type Mount struct {
	// Source 是宿主源绝对路径（bind 挂载源；卷为卷数据目录）。
	Source string `json:"source"`
	// Target 是容器内挂载点（绝对路径）。
	Target string `json:"target"`
	// ReadOnly 是否以只读方式挂载（:ro）。
	ReadOnly bool `json:"readOnly,omitempty"`
}

// ContainerConfig 是容器目录里 config.json 的内容：run/boot/shim 三方共用的
// 单一事实来源。字段只增不改；不兼容变更须升 ConfigVersion。
type ContainerConfig struct {
	// ConfigVersion 是结构版本，当前固定 1。
	ConfigVersion int `json:"configVersion"`
	// ID 是容器唯一标识（12 位十六进制）。
	ID string `json:"id"`
	// Name 是用户可读名（数据目录内唯一）。
	Name string `json:"name"`
	// ImageRef 是来源镜像引用 name:version。
	ImageRef string `json:"imageRef"`
	// Rootfs 是合并后 rootfs 的绝对路径（<root>/containers/<id>/rootfs）。
	Rootfs string `json:"rootfs"`
	// Restart 是重启策略。
	Restart Restart `json:"restart"`
	// Hostname 是容器 UTS 名。
	Hostname string `json:"hostname,omitempty"`
	// Cmd 是容器 1 号进程 argv。
	Cmd []string `json:"cmd"`
	// Env 是注入容器的环境变量 KEY=VALUE。
	Env []string `json:"env,omitempty"`
	// WorkingDir 是容器工作目录（空则 /）。
	WorkingDir string `json:"workingDir,omitempty"`
	// User 是运行身份（阶段 2 暂仅记录，rootless 下等价 root）。
	User string `json:"user,omitempty"`
	// Network 是接入的网络名（licore0/自定义/host/none）；空表示未指定（默认 bridge licore0）。
	Network string `json:"network,omitempty"`
	// IP 是容器在桥接网络上的分配 IP；host/none 为空。
	IP string `json:"ip,omitempty"`
	// Mounts 是已解析的卷挂载列表（源路径就绪，运行时挂载）。
	Mounts []Mount `json:"mounts,omitempty"`
	// CapDrop 是从默认能力集里移除的能力（--cap-drop，可含 ALL）。
	// 新增字段一律 omitempty：旧版本读新配置不得失败。
	CapDrop []string `json:"capDrop,omitempty"`
	// CapAdd 是在默认能力集之上追加的能力（--cap-add）。
	CapAdd []string `json:"capAdd,omitempty"`
	// CreatedAt 是创建时间（UTC，RFC 3339）。
	CreatedAt string `json:"createdAt"`
}

// RuntimeState 是容器目录里 runtime.json 的内容：shim 负责写，其他方只读。
// 与 config.json 分开，避免高频状态写触碰不可变配置。
type RuntimeState struct {
	// ShimPID 是 shim 进程（容器生命周期持有者）PID。
	ShimPID int `json:"shimPid,omitempty"`
	// InitPID 是最近一次容器 init 进程 PID（宿主视角）。
	InitPID int `json:"initPid,omitempty"`
	// Running 表示容器当前在跑（shim 维护）。
	Running bool `json:"running"`
	// Status 表示容器状态阶段：starting（装配中）→ running（init 已起）
	// → exited。Running 与之保持一致；starting 时 Running 为 false。
	Status string `json:"status,omitempty"`
	// ExitCode 是最近一次退出码（未退出为 -1）。
	ExitCode int `json:"exitCode"`
	// StartedAt / FinishedAt 是最近一次运行起止时间（UTC RFC 3339）。
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	// RestartCount 是 shim 按策略累计的重启次数。
	RestartCount int `json:"restartCount"`
}

// 容器状态阶段（RuntimeState.Status）。
const (
	StatusStarting = "starting" // 已 fork shim、装配尚未完成，未确认 init 存活
	StatusRunning  = "running"  // 容器 init 已启动
	StatusExited   = "exited"   // 已停止
)

// ErrorsContainersDir 辅助哨兵。
var (
	// ErrContainerExists 表示同名容器已存在。
	ErrContainerExists = errors.New("licore/store: 同名容器已存在")
	// ErrContainerNotFound 表示按 ID/名字找不到容器。
	ErrContainerNotFound = errors.New("licore/store: 容器不存在")
	// ErrBadContainerConfig 表示容器配置字段非法。
	ErrBadContainerConfig = errors.New("licore/store: 容器配置非法")
)

// ContainersRoot 返回容器状态根目录 <root>/containers。
func (s *Store) ContainersRoot() string { return filepath.Join(s.Root, "containers") }

// ContainerDir 返回单个容器状态目录。
func (s *Store) ContainerDir(id string) string {
	return filepath.Join(s.ContainersRoot(), id)
}

// NewContainerID 生成 12 位十六进制的容器 ID。
func NewContainerID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成容器 ID 失败: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Validate 检查配置完备性；Rootfs 留待调用方在实际合并后校验存在性。
func (c *ContainerConfig) Validate() error {
	if c.ConfigVersion != 1 {
		return fmt.Errorf("configVersion=%d 不受支持: %w", c.ConfigVersion, ErrBadContainerConfig)
	}
	if c.ID == "" || len(c.ID) > 64 || strings.ContainsAny(c.ID, `/\`) {
		return fmt.Errorf("容器 ID %q 非法: %w", c.ID, ErrBadContainerConfig)
	}
	if c.Name == "" || strings.ContainsAny(c.Name, `/\`) || strings.HasPrefix(c.Name, ".") {
		return fmt.Errorf("容器名 %q 非法: %w", c.Name, ErrBadContainerConfig)
	}
	if c.ImageRef == "" || c.Rootfs == "" {
		return fmt.Errorf("imageRef/rootfs 不能为空: %w", ErrBadContainerConfig)
	}
	if !c.Restart.Valid() {
		return fmt.Errorf("restart=%q 非法: %w", c.Restart, ErrBadContainerConfig)
	}
	if len(c.Cmd) == 0 {
		return fmt.Errorf("cmd 不能为空: %w", ErrBadContainerConfig)
	}
	return nil
}

// CreateContainer 原子创建容器状态目录并写入 config.json。
// 同名容器已存在时返回 ErrContainerExists。
func (s *Store) CreateContainer(cfg *ContainerConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	root := s.ContainersRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("创建容器根目录失败: %w", err)
	}
	// 名字唯一性：用 O_EXCL 锁文件原子抢占，杜绝并发同名都通过的 TOCTOU。
	// claimName 失败即名字已被占用（他人持锁），直接拒绝；不做扫描兜底，
	// 避免并发下互相删锁竞争。旧容器（升级前，无锁文件）首次再建/列出时
	// claimName 会成功创建锁。
	claimed, err := s.claimName(cfg.Name)
	if err != nil {
		return err
	}
	if !claimed {
		// 名字已被占用（别处持锁或有 live 容器）。孤儿锁由 RemoveContainer 在
		// 容器删除时释放，正常不残留；此处分歧即拒绝，保证并发单胜者。
		return fmt.Errorf("容器名 %q 已被占用: %w", cfg.Name, ErrContainerExists)
	}
	// 锁已抢占：后续任何失败都释放，避免泄漏锁。
	release := true
	defer func() {
		if release {
			_ = s.releaseName(cfg.Name)
		}
	}()

	dir := s.ContainerDir(cfg.ID)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("容器目录 %s 已存在: %w", cfg.ID, ErrContainerExists)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查容器目录失败: %w", err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("创建容器目录失败: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 config.json 失败: %w", err)
	}
	// tmp 必须与目标同目录：跨目录 rename 在 overlayfs 等文件系统上可能 EXDEV。
	tmp := filepath.Join(dir, ".config.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写 config.json 失败: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "config.json")); err != nil {
		return fmt.Errorf("落位 config.json 失败: %w", err)
	}
	release = false // 成功：保留名字锁
	return nil
}

// namesRoot 存放名字唯一性锁文件 <root>/names/<name>。
func (s *Store) namesRoot() string { return filepath.Join(s.Root, "names") }

// claimName 用 O_CREATE|O_EXCL 原子抢占名字。true=抢占成功；false=已被占用。
func (s *Store) claimName(name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	dir := s.namesRoot()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("创建名字锁目录失败: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		_ = f.Close()
		return true, nil
	}
	if !os.IsExist(err) {
		return false, fmt.Errorf("抢占容器名 %q 失败: %w", name, err)
	}
	return false, nil
}

// releaseName 释放名字锁。
func (s *Store) releaseName(name string) error {
	if name == "" {
		return nil
	}
	return os.Remove(filepath.Join(s.namesRoot(), name))
}

// WriteContainerConfig 原子重写某容器已存在的 config.json（如启动前解析出
// 网络 IP 后回写）。配置字段只增不改；不存在或校验失败时返回错误。
func (s *Store) WriteContainerConfig(cfg *ContainerConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 config.json 失败: %w", err)
	}
	tmp := filepath.Join(s.ContainerDir(cfg.ID), ".config.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写 config.json 失败: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(s.ContainerDir(cfg.ID), "config.json")); err != nil {
		return fmt.Errorf("落位 config.json 失败: %w", err)
	}
	return nil
}

// RemoveContainer 删除容器状态目录（配置、运行状态、该容器独占 rootfs）。
// 供启动失败等清理路径调用；共享层缓存不受影响。
func (s *Store) RemoveContainer(id string) error {
	// 先取容器名以释放名字锁。
	if cfg, err := s.LoadContainer(id); err == nil {
		_ = s.releaseName(cfg.Name)
	}
	if err := os.RemoveAll(s.ContainerDir(id)); err != nil {
		return fmt.Errorf("删除容器目录失败: %w", err)
	}
	return nil
}

// LoadContainer 按容器 ID 读取配置。找不到返回 ErrContainerNotFound。
func (s *Store) LoadContainer(id string) (*ContainerConfig, error) {
	data, err := os.ReadFile(filepath.Join(s.ContainerDir(id), "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("容器 %s: %w", id, ErrContainerNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("读取容器配置失败: %w", err)
	}
	var cfg ContainerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 config.json 失败: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("容器 %s 配置损坏: %w", id, err)
	}
	return &cfg, nil
}

// FindContainer 按 ID 前缀或名字定位容器（供 run/stop/status 用）。
// 前缀命中多个时返回 ErrBadContainerConfig 提示歧义。
func (s *Store) FindContainer(idOrName string) (*ContainerConfig, error) {
	all, err := s.ListContainers()
	if err != nil {
		return nil, err
	}
	var matches []*ContainerConfig
	for _, c := range all {
		if c.ID == idOrName || c.Name == idOrName || strings.HasPrefix(c.ID, idOrName) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("容器 %q: %w", idOrName, ErrContainerNotFound)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("%q 匹配到 %d 个容器，请写全 ID 或名字: %w", idOrName, len(matches), ErrBadContainerConfig)
	}
}

// ListContainers 返回全部容器配置，按创建时间倒序。损坏的目录跳过并告警。
func (s *Store) ListContainers() ([]*ContainerConfig, error) {
	ents, err := os.ReadDir(s.ContainersRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // 尚无容器
	}
	if err != nil {
		return nil, fmt.Errorf("扫描容器目录失败: %w", err)
	}
	var out []*ContainerConfig
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		cfg, err := s.LoadContainer(e.Name())
		if err != nil {
			slog.Warn("跳过损坏的容器目录", "dir", e.Name(), "err", err)
			continue
		}
		out = append(out, cfg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// WriteRuntimeState 原子写容器运行状态（shim / run 调用）。
func (s *Store) WriteRuntimeState(id string, st *RuntimeState) error {
	dir := s.ContainerDir(id)
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("容器 %s: %w", id, ErrContainerNotFound)
		}
		return fmt.Errorf("检查容器目录失败: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 runtime.json 失败: %w", err)
	}
	tmp := filepath.Join(dir, ".runtime.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写 runtime.json 失败: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "runtime.json")); err != nil {
		return fmt.Errorf("落位 runtime.json 失败: %w", err)
	}
	return nil
}

// ReadRuntimeState 读取容器运行状态；从未运行过时返回零值 + false。
func (s *Store) ReadRuntimeState(id string) (*RuntimeState, bool, error) {
	data, err := os.ReadFile(filepath.Join(s.ContainerDir(id), "runtime.json"))
	if errors.Is(err, os.ErrNotExist) {
		return &RuntimeState{ExitCode: -1}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("读取 runtime.json 失败: %w", err)
	}
	var st RuntimeState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, false, fmt.Errorf("解析 runtime.json 失败: %w", err)
	}
	return &st, true, nil
}

// MarkStoppedByUser 写入 stopped-by-user 标记（licore stop 调用）。
func (s *Store) MarkStoppedByUser(id string) error {
	if _, err := os.Stat(filepath.Join(s.ContainerDir(id), "config.json")); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("容器 %s: %w", id, ErrContainerNotFound)
		}
		return fmt.Errorf("检查容器目录失败: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(s.ContainerDir(id), stoppedByName), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("写停止标记失败: %w", err)
	}
	return f.Close()
}

// ClearStoppedByUser 移除标记（容器重新启动时调用）。
func (s *Store) ClearStoppedByUser(id string) error {
	if err := os.Remove(filepath.Join(s.ContainerDir(id), stoppedByName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("清除停止标记失败: %w", err)
	}
	return nil
}

// IsStoppedByUser 报告容器是否带有用户停止标记。
func (s *Store) IsStoppedByUser(id string) bool {
	_, err := os.Stat(filepath.Join(s.ContainerDir(id), stoppedByName))
	return err == nil
}

// BootEligible 报告 boot 时该容器是否应被拉起：策略允许且未被用户停止
// （unless-stopped 语义；always 无视标记，与 Docker 行为一致）。
func (s *Store) BootEligible(cfg *ContainerConfig) bool {
	if !cfg.Restart.BootEligible() {
		return false
	}
	if cfg.Restart == RestartUnlessStoped && s.IsStoppedByUser(cfg.ID) {
		return false
	}
	return true
}

// ContainerNames 返回已占用容器名集合（engine 生成自动名时避让）。
func (s *Store) ContainerNames() (map[string]bool, error) { return s.containerNames() }

// containerNames 返回已占用容器名集合。
func (s *Store) containerNames() (map[string]bool, error) {
	list, err := s.ListContainers()
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(list))
	for _, c := range list {
		names[c.Name] = true
	}
	return names, nil
}

// nowUTC 是当前 UTC 时间的 RFC 3339 表示。
func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }
