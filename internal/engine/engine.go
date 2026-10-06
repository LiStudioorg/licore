// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

// Package engine 串联一次 `licore run` 的完整生命周期：镜像查找 → 层解包 →
// rootfs 合并 → 容器状态落盘 → 启动（前台持有 / 后台 fork shim）。
// CLI 层只做参数绑定，编排在 engine，可脱离 cobra 测试。
// 资源限制（--memory/--cpus/--pids-limit）与端口/卷为阶段 3 项：解析并
// 记录进容器配置，运行时暂不生效（调用方负责 slog.Warn）。
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/LiStudioorg/licore/internal/image"
	"github.com/LiStudioorg/licore/internal/resource"
	"github.com/LiStudioorg/licore/internal/runtime"
	"github.com/LiStudioorg/licore/internal/shim"
	"github.com/LiStudioorg/licore/internal/storage"
	"github.com/LiStudioorg/licore/internal/store"
)

// ErrImageNotFound 表示引用的镜像尚未导入本地数据目录。
var ErrImageNotFound = errors.New("licore/engine: 镜像未找到")

// RunSpec 是一次 run 的完整请求。
type RunSpec struct {
	// ImageRef 是镜像引用 name:version。
	ImageRef string
	// Cmd 是容器 1 号进程 argv；为空时取镜像 config.entrypoint+cmd。
	Cmd []string
	// Env 是追加环境变量 KEY=VALUE。
	Env []string
	// Name 为空时自动生成（与既有容器去重）。
	Name string
	// Hostname 为空时取容器名。
	Hostname string
	// Workdir / User 透传容器配置。
	Workdir string
	User    string
	// Entrypoint 覆盖镜像 entrypoint（非空时）。
	Entrypoint []string
	// Restart 是重启策略。
	Restart store.Restart
	// Detach 后台：fork shim，返回即容器 ID；nil=前台持有容器进程。
	Detach bool
	// Ports / Volumes 是 -p / -v 原始参数，运行时分别接入网络 NAT 与卷挂载。
	Ports   []string
	Volumes []string
	// Network 是接入的网络名（licore0/自定义/host/none）；IP 为期望地址（可空）。
	Network string
	IP      string
	// MemoryMB / CPUs / PidsLimit 是资源参数的便捷字段（等价字段已并入 Limits）。
	MemoryMB  int
	CPUs      float64
	PidsLimit int
	// Limits 是完整的资源限制（CLI 由 runLimits 装配）；空表示无显式限制。
	Limits *resource.Limits
	// CapDrop / CapAdd 是能力裁剪规格（--cap-drop / --cap-add）。
	// 空表示用运行时内置的 Docker 默认集。
	CapDrop []string
	CapAdd  []string
}

// RunResult 是一次 run 的结果。
type RunResult struct {
	// Container 是落盘的容器配置。
	Container *store.ContainerConfig
	// ShortID 是 12 位 ID 的前 12 位（当前即全 ID，留增长空间）。
	ShortID string
	// ExitCode 仅前台模式有效（容器退出码）。
	ExitCode int
	// Foreground 标记本次为前台运行。
	Foreground bool
}

// Run 执行完整流程。foreground 模式的 ctx SIGTERM 语义见 runtime.StartWith。
func Run(ctx context.Context, st *store.Store, spec *RunSpec) (*RunResult, error) {
	name, version, err := splitImageRef(spec.ImageRef)
	if err != nil {
		return nil, err
	}

	// 1. 镜像必须已导入。
	ok, err := st.Exists(name, version)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("镜像 %s:%s 未导入，请先 licore pull %s_%s.licore: %w", name, version, name, version, ErrImageNotFound)
	}
	loaded, err := image.OpenFile(filepath.Join(st.ImageDir(name, version), "source.licore"))
	if err != nil {
		return nil, fmt.Errorf("重新打开镜像文件失败: %w", err)
	}
	if err := loaded.CheckPlatform(); err != nil {
		return nil, err
	}

	// 2. 决定容器命令：显式 cmd 覆盖 > entrypoint+cmd。
	cmd := spec.Cmd
	if len(spec.Entrypoint) > 0 {
		cmd = append(append([]string{}, spec.Entrypoint...), cmd...)
	}
	if len(cmd) == 0 {
		cmd = append(append([]string{}, loaded.Config.Entrypoint...), loaded.Config.Cmd...)
	}
	if len(cmd) == 0 {
		return nil, fmt.Errorf("镜像 %s 未提供 entrypoint/cmd，且命令行为空: %w", spec.ImageRef, runtime.ErrBadConfig)
	}

	// 3. 容器名与 ID。
	id, err := store.NewContainerID()
	if err != nil {
		return nil, err
	}
	cname := spec.Name
	if cname == "" {
		cname = autoName(st, id)
	}

	// 4. 环境变量：镜像 env（map，按 key 排序保证确定性）打底，run -e 覆盖同名。
	base := make([]string, 0, len(loaded.Config.Env))
	for k, v := range loaded.Config.Env {
		base = append(base, k+"="+v)
	}
	slices.Sort(base)
	env := mergeEnv(base, spec.Env)
	workdir := spec.Workdir
	if workdir == "" {
		workdir = loaded.Config.WorkingDir
	}
	user := spec.User
	if user == "" {
		user = loaded.Config.User
	}

	// 端口映射解析（网络侧生效；错误直接返回，避免装了一半）。
	ports, err := parsePorts(spec.Ports)
	if err != nil {
		return nil, err
	}

	cfg := &store.ContainerConfig{
		ConfigVersion: 1,
		ID:            id,
		Name:          cname,
		ImageRef:      name + ":" + version,
		Rootfs:        filepath.Join(st.ContainerDir(id), "rootfs"),
		Restart:       spec.Restart,
		Hostname:      orDefault(spec.Hostname, cname),
		Cmd:           cmd,
		Env:           env,
		WorkingDir:    workdir,
		User:          user,
		Network:       spec.Network,
		CapDrop:       spec.CapDrop,
		CapAdd:        spec.CapAdd,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := st.CreateContainer(cfg); err != nil {
		return nil, err
	}
	// 容器已登记：接入网络（alloc IP）、登记端口并实化 NAT。失败时回滚断开，
	// 避免残留半接入的端点；veth 进容器 netns 由运行时装配，见 runForeground/shim。
	if err := wireNetworkBeforeStart(st, cfg, spec.Network, spec.IP, ports); err != nil {
		disconnectContainer(st, cfg)
		_ = st.RemoveContainer(cfg.ID)
		return nil, err
	}
	// 卷接入：解析 -v 并落盘已解析挂载（匿名/命名卷补齐源路径）；运行时在
	// pivot_root 前把源 bind 进容器 mount namespace。
	volMounts, err := parseVolumes(spec.Volumes)
	if err != nil {
		disconnectContainer(st, cfg)
		_ = st.RemoveContainer(cfg.ID)
		return nil, err
	}
	if err := wireVolumesBeforeStart(st, cfg, volMounts); err != nil {
		disconnectContainer(st, cfg)
		_ = st.RemoveContainer(cfg.ID)
		return nil, err
	}
	// 资源限制：创建容器专属 cgroup 并写入限制（哪怕空限制也建组，便于 stats）。
	// init PID 写入由 runtime 完成。
	//
	// **用户显式请求了限制时，失败必须中止启动**：此前一律降级为 WARN，
	// 结果是"参数解析正确、config.json 也存对了，但限制根本没生效"——
	// 这正是本仓库反复出现的那类静默失效（--cap-add / --user / --cpuset-cpus
	// / --blkio-weight 都是同一形态）。运行一个"以为自己有内存上限、
	// 实际没有"的容器，比直接启动失败危险得多：用户会据此以为 OOM 已被隔离。
	// 只有**空限制**（用户没提任何资源参数，建组只为 stats）才允许降级告警。
	if _, err := resource.Setup(cfg.ID, spec.Limits); err != nil {
		if spec.Limits != nil && !spec.Limits.Empty() {
			disconnectContainer(st, cfg)
			_ = st.RemoveContainer(cfg.ID)
			return nil, fmt.Errorf("应用资源限制失败（参数已解析但无法生效，拒绝以无限制方式启动）: %w", err)
		}
		slog.Warn("创建容器 cgroup 失败（无限制请求，仅 stats 受影响）", "container", cfg.ID, "err", err)
	}

	res := &RunResult{Container: cfg, ShortID: id, Foreground: !spec.Detach, ExitCode: -1}
	if err := prepareAndStartFn(ctx, st, cfg, res); err != nil {
		// 启动失败：状态目录保留（有 runtime.json 线索），rootfs 半成品清理避免误导。
		// 同时撤回已建的网络端点（防残留 veth/NAT）与 cgroup（无活进程）。
		_ = os.RemoveAll(cfg.Rootfs)
		disconnectContainer(st, cfg)
		if cerr := resource.Remove(cfg.ID); cerr != nil {
			slog.Debug("清理失败容器 cgroup 出错", "container", cfg.ID, "err", cerr)
		}
		return res, err
	}
	return res, nil
}

// prepareAndStartFn 是启动注入点（测试替换为 fake，避开真实 namespace）。
var prepareAndStartFn = prepareAndStart

// prepareAndStart 做"解包+合并 rootfs → 启动"。失败由 Run 清理。
func prepareAndStart(ctx context.Context, st *store.Store, cfg *store.ContainerConfig, res *RunResult) error {
	// 解包合并 rootfs。
	if err := BuildRootfs(st, cfg.ImageRef, cfg.Rootfs); err != nil {
		return err
	}
	// 镜像默认 entrypoint 存在时 runtime 走 cmd 通道即可。
	if err := st.ClearStoppedByUser(cfg.ID); err != nil {
		return err
	}

	if res.Foreground {
		return runForeground(ctx, st, cfg, res)
	}
	p, err := shim.Reexec(st.Root, cfg.ID)
	if err != nil {
		return err
	}
	// 尽早记录 shim PID，boot 幂等判定窗口最小化；完整状态由 shim 覆写。
	// 此阶段装配尚未完成，标记为 starting（Running=false），ps 不显示 Up。
	_ = st.WriteRuntimeState(cfg.ID, &store.RuntimeState{
		ShimPID: p.Pid, Running: false, Status: store.StatusStarting, ExitCode: -1,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	})
	return nil
}

// BuildRootfs 解包镜像全部层（内容寻址，跨容器复用）并按 applyOrder 合并到 targetDir。
func BuildRootfs(st *store.Store, imageRef, targetDir string) error {
	name, version, err := splitImageRef(imageRef)
	if err != nil {
		return err
	}
	src := filepath.Join(st.ImageDir(name, version), "source.licore")
	loaded, err := image.OpenFile(src)
	if err != nil {
		return err
	}
	layers := append([]image.Layer{}, loaded.Manifest.Layers...)
	sort.SliceStable(layers, func(i, j int) bool { return layers[i].ApplyOrder < layers[j].ApplyOrder })

	tmp, err := os.MkdirTemp(st.Root, ".layer-tmp-*") // 与最终目录同 fs，rename 安全
	if err != nil {
		return fmt.Errorf("创建层临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	digests := make([]string, 0, len(layers))
	for _, l := range layers {
		hexsum := strings.TrimPrefix(l.Digest, "sha256:")
		if storage.LayerUnpacked(st.Root, hexsum) {
			digests = append(digests, hexsum)
			continue
		}
		tmpFile := filepath.Join(tmp, hexsum+".tar.gz")
		if err := loaded.ExtractFile(l.Path, tmpFile); err != nil {
			return err
		}
		got, err := storage.UnpackFile(tmpFile, hexsum, st.Root)
		if err != nil {
			return fmt.Errorf("解包层 %s: %w", l.Path, err)
		}
		digests = append(digests, got.DigestHex)
		_ = os.Remove(tmpFile)
	}
	return storage.MergeLayers(st.Root, digests, targetDir)
}

// runForeground 前台运行：CLI 进程持有容器（stdio 直连），Ctrl+C 经
// StopCh 转发容器。返回容器退出码写入 res。
func runForeground(ctx context.Context, st *store.Store, cfg *store.ContainerConfig, res *RunResult) error {
	stopCh := make(chan struct{})
	watched := make(chan struct{})
	defer close(watched)
	go func() {
		select {
		case <-ctx.Done():
			close(stopCh)
		case <-watched:
		}
	}()

	state := store.RuntimeState{ExitCode: -1, Running: true, StartedAt: time.Now().UTC().Format(time.RFC3339)}
	env := append([]string{}, cfg.Env...)
	env = append(env, netEnvFor(st, cfg, cfg.Hostname)...)
	env = append(env, runtime.MountEnv(cfg.Mounts)...)
	env = append(env, runtime.CgroupEnv(cfg.ID)...)
	// 能力裁剪规格同样经环境变量下发：init 在 execve 前读取并应用。
	// 空值表示走运行时内置的 Docker 默认集。
	env = append(env, runtime.CapsEnv(cfg.CapDrop, cfg.CapAdd)...)
	// --workdir / --user 同理：runtime.Config 是冻结接口，加不了字段，
	// 经环境变量下发给 init。
	env = append(env, runtime.WorkdirEnv(cfg.WorkingDir)...)
	env = append(env, runtime.UserEnv(cfg.User)...)
	r, err := runtime.StartWith(&runtime.Config{
		Rootfs:   cfg.Rootfs,
		Hostname: cfg.Hostname,
		Cmd:      cfg.Cmd,
		Env:      env,
	}, func(pid int) {
		state.InitPID = pid
		s := state
		_ = st.WriteRuntimeState(cfg.ID, &s)
	}, &runtime.StartOptions{StopCh: stopCh})
	state.Running = false
	state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		_ = st.WriteRuntimeState(cfg.ID, &state)
		return fmt.Errorf("前台运行容器失败: %w", err)
	}
	state.ExitCode = r.ExitCode
	_ = st.WriteRuntimeState(cfg.ID, &state)
	res.ExitCode = r.ExitCode
	return nil
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// mergeEnv 以 KEY 维度合并两组 KEY=VALUE（后者覆盖前者，保持顺序稳定）。
func mergeEnv(base, override []string) []string {
	idx := map[string]int{}
	var out []string
	add := func(kv string, replace bool) {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			return
		}
		if i, seen := idx[k]; seen {
			if replace {
				out[i] = kv
			}
			return
		}
		idx[k] = len(out)
		out = append(out, kv)
	}
	for _, kv := range base {
		add(kv, false)
	}
	for _, kv := range override {
		add(kv, true)
	}
	return out
}

// splitImageRef 拆解 name:version；name 允许含 /（规范 §3.1），version 不允许。
func splitImageRef(ref string) (name, version string, err error) {
	i := strings.LastIndex(ref, ":")
	if i <= 0 || i == len(ref)-1 {
		return "", "", fmt.Errorf("镜像引用 %q 应为 name:version 形式（先 licore pull 导入）", ref)
	}
	return ref[:i], ref[i+1:], nil
}

var _ = io.Discard // 预留输出注入点（engine 层不打印）

// autoName 生成 "adjective_animal" 式随机名，与既有容器去重。
func autoName(st *store.Store, id string) string {
	names, _ := st.ContainerNames()
	words := []string{"bold", "calm", "eager", "fleet", "keen", "lucid", "nimble", "prime", "quint", "swift"}
	animals := []string{"fox", "owl", "puma", "wren", "lynx", "moth", "newt", "crab", "dory", "ermine"}
	// 用十六进制 ID 的字节做确定性映射：同 ID 同名，重跑可复现。
	hi, _ := hexByte(id, 0)
	lo, _ := hexByte(id, 1)
	for n := 0; n < 256; n++ {
		cand := fmt.Sprintf("%s_%s", words[(int(hi)+n)%len(words)], animals[(int(lo)+n*3)%len(animals)])
		if !names[cand] {
			return cand
		}
	}
	// 极端撞名场景：直接拼 ID 片段。
	return "licore_" + id
}

func hexByte(s string, i int) (byte, error) {
	if len(s) < 2*(i+1) {
		return 0, fmt.Errorf("ID %q 过短", s)
	}
	var b byte
	for _, c := range s[2*i : 2*i+2] {
		b <<= 4
		switch {
		case c >= '0' && c <= '9':
			b |= byte(c - '0')
		case c >= 'a' && c <= 'f':
			b |= byte(c-'a') + 10
		default:
			return 0, fmt.Errorf("ID %q 含非法字符", s)
		}
	}
	return b, nil
}
