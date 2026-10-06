// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package resource

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// errNotCgroupV2 表示 /sys/fs/cgroup 不是 v2 统一层级。
var errNotCgroupV2 = errors.New("cgroups v2 不可用")

// Available 报告 cgroups v2 是否可用（内置 cgroup.controllers 存在即视为 v2）。
func Available() bool {
	data, err := os.ReadFile(filepath.Join(cgroupV2GroupRoot, "cgroup.controllers"))
	return err == nil && len(data) > 0
}

// controllers 需要开给 licore 子组的控制器，写进 <licore>/cgroup.subtree_control。
// 注意：只有此处 enable 后，licore/<id>/cpu.max|memory.max|pids.max 等才可写；
// 若不 enable，cgroup v2 子组写这些限制会 EPERM（此前 --memory/--cpus 静默落空）。
//
// **cpuset 必须在这里**：漏掉它会重现同一类静默失效——子组的
// cpuset.cpus 不可写，`--cpuset-cpus` 于是完全没效果（真机实测：
// 传 --cpuset-cpus 0 后 cpuset.cpus 仍为空，而父组显示 0-3）。
// v1 的实现（cgroup_v1_linux.go）本来就按需 enable cpuset，补齐 v2 是为了
// 两条路径语义一致。
//
// **io 必须在这里**：`--blkio-weight` 写的是子组的 io.weight。漏掉 io 时
// 子组根本没有 io.weight 文件，写入报 EACCES/ENOENT，`--blkio-weight`
// 于是**静默失效**（真机实测：传 --blkio-weight 500 后容器内看不到 io.weight，
// 只留一条 "创建容器 cgroup 失败" 的 WARN，容器照常启动）。
const controllers = "cpu memory pids cpuset io"

// enableControllers 在 licore 父组的 cgroup.subtree_control 里启用容器限制所需
// 的控制器（cpu/memory/pids/cpuset/io）。已在更外层启用时追加挂到本组；幂等、容错。
//
// 必须先建出 licore 父组：cgroup.subtree_control 只存在于已创建的子组里，
// 若父组不存在（例如首次运行，或上一次 `licore rm` / 验证脚本的清理把
// /sys/fs/cgroup/licore 删掉之后），写该文件会 ENOENT 并让整个资源限制
// 静默失效。此前依赖 Setup 里 MkdirAll(c.Path) 的副作用顺带建父组，但那是
// 在 enableControllers **之后**才执行的，属于顺序依赖的隐患。
//
// **逐个写、而不是一次写全部**：cgroup v2 的 subtree_control 写入是**原子**的，
// 只要有一个控制器不可用，整条写入就失败（内核返回 ENOENT/EINVAL），
// **其余本来可用的控制器也一并作废**。真机实测（2026-10-06）：
// 宿主 root 的 cgroup.subtree_control 尚未 enable cpuset 时，
// 一次写 "+cpu +memory +pids +cpuset" 直接 ENOENT，
// 结果 --memory/--cpus/--pids-limit **全部**静默失效；而单独写
// "+cpu"/"+memory"/"+pids" 各自成功。逐个写把"一个不可用"的爆炸半径
// 限制在它自己身上，其余控制器仍按预期生效。
//
// 返回**实际可用**的控制器集合（含此前已启用的）。单个控制器不可用不在此处
// 报错——是否致命取决于调用方请求了哪些限制（见 setupV2）：
// 宿主没有 io 控制器时，"不设 --blkio-weight 的容器"应当照常运行。
func enableControllers() (map[string]bool, error) {
	if !Available() {
		return nil, fmt.Errorf("cgroups v2 不可用: %w", ErrUnsupported)
	}
	group := filepath.Join(cgroupV2GroupRoot, LiCoreGroup)
	if err := os.MkdirAll(group, 0o755); err != nil {
		return nil, fmt.Errorf("创建 cgroup 父组 %s: %w", group, err)
	}
	path := filepath.Join(group, "cgroup.subtree_control")
	enabled := map[string]bool{}
	for _, c := range strings.Fields(readOr("", path)) {
		enabled[strings.TrimPrefix(c, "+")] = true
	}
	// 逐个启用缺失的控制器：内核写入是"全有或全无"，混在一起时一个不可用
	// 会拖垮其余（见上）。失败项**不终止**，只留在 enabled 之外，
	// 由调用方按"是否请求了对应限制"决定是否致命。
	for _, c := range strings.Fields(controllers) {
		if enabled[c] {
			continue
		}
		if err := writeSubtreeControl(path, []byte("+"+c), 0o644); err != nil {
			continue
		}
		enabled[c] = true
	}
	return enabled, nil
}

// controllerFor 返回某个限制项依赖的 cgroup 控制器名。
// 用于把"请求了限制"精确映射到"需要哪个控制器"，从而只在必要时报错。
type limitNeed struct {
	// name 是给用户看的限制名（CLI flag 语义）。
	name string
	// controllers 是该限制写入所需的一个或多个控制器。
	controllers []string
	// requested 报告用户是否请求了该限制。
	requested func(*Limits) bool
}

// v2LimitNeeds 列出 v2 各限制项与其依赖的控制器。
// 顺序即构造错误信息的顺序，保持稳定以便测试与排障。
var v2LimitNeeds = []limitNeed{
	{"--cpus", []string{"cpu"}, func(l *Limits) bool { return l.CPUs > 0 }},
	{"--cpuset-cpus", []string{"cpuset"}, func(l *Limits) bool { return l.CPUSet != "" }},
	{"--memory", []string{"memory"}, func(l *Limits) bool { return l.Memory > 0 }},
	{"--memory-swap", []string{"memory"}, func(l *Limits) bool { return l.MemorySwap > 0 }},
	{"--memory-reservation", []string{"memory"}, func(l *Limits) bool { return l.MemoryReservation > 0 }},
	{"--pids-limit", []string{"pids"}, func(l *Limits) bool { return l.PidsLimit > 0 }},
	{"--blkio-weight", []string{"io"}, func(l *Limits) bool { return l.BlkioWeight > 0 }},
}

// missingControllersFor 返回"用户已请求、但宿主无法启用"的限制项描述。
// 为空表示请求的限制都能生效。
func missingControllersFor(l *Limits, enabled map[string]bool) []string {
	if l == nil {
		return nil
	}
	var missing []string
	for _, n := range v2LimitNeeds {
		if !n.requested(l) {
			continue
		}
		for _, c := range n.controllers {
			if !enabled[c] {
				missing = append(missing, fmt.Sprintf("%s（需要 %s 控制器）", n.name, c))
				break
			}
		}
	}
	return missing
}

func readOr(def, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return def
	}
	return strings.TrimSpace(string(b))
}

// Setup 创建容器专属 cgroup 并写入全部限制。l 为空或 Empty() 时仍创建组
// （便于 stats 统一采集），只是不写限制。
//
// 形态自适应：优先 cgroups v2 统一层级；宿主只有 v1（大量 Android 设备）
// 时自动走 v1 分支。两条路径的签名与语义完全一致，调用方无需关心。
func Setup(containerID string, l *Limits) (*Cgroup, error) {
	switch CgroupModeOf() {
	case ModeV2:
		return setupV2(containerID, l)
	case ModeV1:
		root := v1Root()
		slog.Debug("使用 cgroups v1 写入资源限制",
			slog.String("container", containerID), slog.String("root", root))
		return setupV1(containerID, l, root)
	default:
		return nil, fmt.Errorf("未探测到可用的 cgroup 挂载（v1/v2 均不可用）: %w", ErrUnsupported)
	}
}

// setupV2 是原来的 cgroup v2 路径，签名与行为保持不变。
func setupV2(containerID string, l *Limits) (*Cgroup, error) {
	c := NewCgroup(containerID)
	// cgroup v2：必须在父组 subtree_control 启用控制器，子组的限制文件才可写。
	enabled, err := enableControllers()
	if err != nil {
		return nil, err
	}
	// 用户**显式请求**的限制若因宿主不支持对应控制器而无法生效，必须报错。
	// 此前这种情形只在写限制文件时报一次上下文很弱的错（甚至完全静默），
	// 结果是容器带着"看起来设置成功、实际完全没有上限"的配置跑起来——
	// 对 --memory 而言这意味着用户以为 OOM 已被隔离。宁可不启动。
	//
	// 只对**已请求**的限制报错：宿主没有 io 控制器时，一个没传
	// --blkio-weight 的容器不应因此拒绝启动。
	if missing := missingControllersFor(l, enabled); len(missing) > 0 {
		return nil, fmt.Errorf("宿主 cgroup 不支持以下已请求的限制：%s: %w",
			strings.Join(missing, "、"), ErrUnsupported)
	}
	if err := os.MkdirAll(c.Path, 0o755); err != nil {
		return nil, fmt.Errorf("创建 cgroup %s: %w", c.Path, err)
	}
	if l != nil {
		if err := Apply(c, l); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Apply 把限制写入已存在的 cgroup（Setup/Update 共用）。
// 按当前宿主的 cgroup 形态分派到 v2 或 v1 写入路径。
func Apply(c *Cgroup, l *Limits) error {
	if CgroupModeOf() == ModeV1 {
		root := c.Root
		if root == "" || v1Root() != "" {
			root = v1Root()
		}
		return applyV1(c, l, root)
	}
	return applyV2(c, l)
}

// applyV2 是原来的 cgroup v2 写入路径。
func applyV2(c *Cgroup, l *Limits) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if q := l.CPUQuota(); q > 0 {
		if err := c.write("cpu.max", strconv.FormatInt(q, 10)+" "+strconv.Itoa(DefaultCPUPeriod)); err != nil {
			return err
		}
	}
	if w := l.CPUWeight(); w > 0 {
		if err := c.write("cpu.weight", strconv.FormatInt(w, 10)); err != nil {
			return err
		}
	}
	if l.CPUSet != "" {
		if err := c.write("cpuset.cpus", l.CPUSet); err != nil {
			return err
		}
	}
	if l.Memory > 0 {
		if err := c.write("memory.max", strconv.FormatInt(l.Memory, 10)); err != nil {
			return err
		}
	}
	if sw := l.SwapLimit(); sw != 0 && sw != -1 {
		if err := c.write("memory.swap.max", strconv.FormatInt(sw, 10)); err != nil {
			return err
		}
	} else if sw == -1 {
		if err := c.write("memory.swap.max", "max"); err != nil {
			return err
		}
	}
	if l.MemoryReservation > 0 {
		if err := c.write("memory.low", strconv.FormatInt(l.MemoryReservation, 10)); err != nil {
			return err
		}
	}
	if l.OOMKillDisable {
		// v2 里关 oom 击杀：写 oom.group=0 让击杀面向整个 group（等于禁用单进程杀掉）。
		_ = c.write("memory.oom.group", "0")
	}
	if l.PidsLimit > 0 {
		if err := c.write("pids.max", strconv.FormatInt(l.PidsLimit, 10)); err != nil {
			return err
		}
	}
	if l.BlkioWeight > 0 || len(l.BlkioWeightDevice) > 0 {
		if err := c.writeIOWeight(l); err != nil {
			return err
		}
	}
	if err := c.writeIONodeThrottle(l); err != nil {
		return err
	}
	if l.NetworkBandwidth > 0 {
		if err := c.writeNetworkBandwidth(l.NetworkBandwidth); err != nil {
			return err
		}
	}
	if l.Storage > 0 || len(l.GPU) > 0 || len(l.NPU) > 0 {
		if err := c.writeStorageAndDevices(l); err != nil {
			return err
		}
	}
	return nil
}

// AddPID 把容器 init（或任意容器进程）PID 写入其 cgroup.procs，使之后代
// 进程自动落入该组，让已设置的 CPU/内存/pids/io 限制对其生效。容器启动时
// 由 runtime 在 fork 出 init 后调用；cgroup 必须已由 Setup 创建。
// cgroup.procs 是内核虚拟文件，不支持 rename，直接整行写入追加 PID。
func AddPID(containerID string, pid int) error {
	if CgroupModeOf() == ModeV1 {
		return addPIDV1(containerID, v1Root(), pid)
	}
	c := NewCgroup(containerID)
	if _, err := os.Stat(c.Path); os.IsNotExist(err) {
		return fmt.Errorf("cgroup %s 不存在（先 Setup）: %w", c.Path, ErrUnsupported)
	}
	procs := filepath.Join(c.Path, "cgroup.procs")
	if err := os.WriteFile(procs, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("写入 cgroup.procs: %w", err)
	}
	return nil
}

// write 直接写 cgroup 控制文件。cgroup v2 的控制文件（memory.max、cpu.max、
// cgroup.procs 等）是内核虚拟文件，不支持临时文件 + rename，必须整行直写。
func (c *Cgroup) write(name, val string) error {
	if !Available() {
		return fmt.Errorf("cgroups v2 不可用: %w", ErrUnsupported)
	}
	path := filepath.Join(c.Path, name)
	if err := os.WriteFile(path, []byte(val), 0o644); err != nil {
		return fmt.Errorf("写入 %s: %w", name, err)
	}
	return nil
}

// writeIOWeight 写 io.weight（把 v1 blkio 换算成 v2）。
func (c *Cgroup) writeIOWeight(l *Limits) error {
	if l.BlkioWeight > 0 {
		if err := c.write("io.weight", strconv.FormatInt(BlkioToIOWeight(l.BlkioWeight), 10)); err != nil {
			return err
		}
	}
	for _, wd := range l.BlkioWeightDevice {
		val := fmt.Sprintf("%s %d", wd.Device, BlkioToIOWeight(wd.Weight))
		if err := c.write("io.weight", val); err != nil {
			return err
		}
	}
	return nil
}

// writeIONodeThrottle 写 io.max 的设备限速（device rbps/wbps/riops/wiops）。
func (c *Cgroup) writeIONodeThrottle(l *Limits) error {
	var lines []string
	for _, td := range l.DeviceReadBps {
		lines = append(lines, fmt.Sprintf("%s rbps=%d", td.Device, td.Rate))
	}
	for _, td := range l.DeviceWriteBps {
		lines = append(lines, fmt.Sprintf("%s wbps=%d", td.Device, td.Rate))
	}
	for _, td := range l.DeviceReadIOps {
		lines = append(lines, fmt.Sprintf("%s riops=%d", td.Device, td.Rate))
	}
	for _, td := range l.DeviceWriteIOps {
		lines = append(lines, fmt.Sprintf("%s wiops=%d", td.Device, td.Rate))
	}
	for _, entry := range lines {
		dev := strings.Fields(entry)[0]
		if err := c.write("io.max", entry); err != nil {
			return fmt.Errorf("写入 io.max %q: %w", dev, err)
		}
	}
	return nil
}

// writeNetworkBandwidth 限制容器出向带宽。需要 tc/HTB 在容器 veth 上做
// 面共享整棵树的编排，本环境不提供该基础设施，显式报错而不静默忽略。
func (c *Cgroup) writeNetworkBandwidth(bps int64) error {
	if bps <= 0 {
		return nil
	}
	return fmt.Errorf("--network-bandwidth 需要 tc 流量整形，当前未实现: %w", ErrUnsupported)
}

// writeStorageAndDevices 写存储配额与设备白名单。存储配额需 XFS project
// quota；GPU/NPU 需 cgroup eBPF 设备控制。两者当前未实现，请求时显式报错。
func (c *Cgroup) writeStorageAndDevices(l *Limits) error {
	if l.Storage > 0 {
		return fmt.Errorf("--storage 存储配额需 XFS project quota，当前未实现: %w", ErrUnsupported)
	}
	if len(l.GPU) > 0 || len(l.NPU) > 0 {
		return fmt.Errorf("--gpu/--npu 设备直通需 cgroup eBPF 设备控制，当前未实现: %w", ErrUnsupported)
	}
	return nil
}

// writeDevices 旧接口：GPU/NPU 直通能力已拆分到 writeStorageAndDevices，
// 此处不再接收（调用方不会传 requests）。
func (c *Cgroup) writeDevices(gpus, npus []DeviceRequest) error {
	_ = gpus
	_ = npus
	return fmt.Errorf("设备直通未实现: %w", ErrUnsupported)
}

// Remove 删除容器 cgroup（v1 下删除其在各控制器下的目录）。
func Remove(containerID string) error {
	if CgroupModeOf() == ModeV1 {
		return removeV1(containerID, v1Root())
	}
	c := NewCgroup(containerID)
	if _, err := os.Stat(c.Path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.Remove(c.Path); err != nil {
		return fmt.Errorf("删除 cgroup %s: %w", c.Path, err)
	}
	return nil
}

// Update 动态调整已存在 cgroup 的限制（licore update）。
func Update(containerID string, l *Limits) error {
	if CgroupModeOf() == ModeV1 {
		root := v1Root()
		if !v1StatsExist(containerID, root) {
			return fmt.Errorf("容器 %s 无 cgroup（可能未运行或被限制）: %w", containerID, ErrUnsupported)
		}
		return applyV1(newV1Cgroup(containerID, root), l, root)
	}
	c := NewCgroup(containerID)
	if _, err := os.Stat(c.Path); err != nil {
		return fmt.Errorf("容器 %s 无 cgroup（可能未运行或被限制）: %w", containerID, ErrUnsupported)
	}
	return Apply(c, l)
}

// read 读取 cgroup 控制文件内容（去除末尾换行）。
func (c *Cgroup) read(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(c.Path, name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// readInt 读取整数值，遇 "max" 或空返回 0。
func (c *Cgroup) readInt(name string) (int64, error) {
	s, err := c.read(name)
	if err != nil {
		return 0, err
	}
	if s == "" || s == "max" {
		return 0, nil
	}
	// cpu.max 形如 "50000 100000"。
	if i := strings.IndexByte(s, ' '); i >= 0 {
		s = s[:i]
	}
	return strconv.ParseInt(s, 10, 64)
}

// Collect 采集容器运行时的 cgroup 用量（供 licore stats）。
func Collect(c *Cgroup) (*Stats, error) {
	st := &Stats{ContainerID: c.ContainerID, Running: true}
	if cpu, err := c.readInt("cpu.usage_usec"); err == nil {
		st.CPUUsageNanos = cpu * 1000
	}
	if w, err := c.readInt("cpu.weight"); err == nil {
		st.CPUShares = WeightToShares(w)
	}
	if mu, err := c.readInt("memory.current"); err == nil {
		st.MemoryUsage = mu
	}
	if ml, err := c.readInt("memory.max"); err == nil {
		st.MemoryLimit = ml
	}
	if pn, err := c.readInt("pids.current"); err == nil {
		st.PidsCurrent = pn
	}
	if pl, err := c.readInt("pids.max"); err == nil {
		st.PidsLimit = pl
	}
	return st, nil
}

// StatsFor 按容器 ID 取 cgroup 并采集。
func StatsFor(containerID string) (*Stats, error) {
	if CgroupModeOf() == ModeV1 {
		root := v1Root()
		if !v1StatsExist(containerID, root) {
			return &Stats{ContainerID: containerID, Running: false}, nil
		}
		return collectV1(newV1Cgroup(containerID, root), root)
	}
	c := NewCgroup(containerID)
	if _, err := os.Stat(c.Path); errors.Is(err, os.ErrNotExist) {
		return &Stats{ContainerID: containerID, Running: false}, nil
	} else if err != nil {
		return nil, err
	}
	return Collect(c)
}
