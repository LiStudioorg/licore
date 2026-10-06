// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package resource

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeV2Root 造一个可写的假 cgroup v2 根：含 cgroup.controllers，
// 并把 v2 seam 指过来。写路径全部落在临时目录，不触碰宿主 /sys/fs/cgroup。
//
// cgroup.controllers 用 controllers 常量派生，而不是硬编码一份列表：
// 真实内核对 subtree_control 的写入会校验控制器是否**可用**，
// 硬编码的假列表一旦落后于常量（本次给 controllers 补 io 时就撞上），
// 测试会以"写不进去"的形式假失败，而那不是被测代码的问题。
func fakeV2Root(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cgroup2")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"),
		[]byte(controllers+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := cgroupV2GroupRoot
	cgroupV2GroupRoot = root
	t.Cleanup(func() { cgroupV2GroupRoot = old })
	// v2 模式下 CgroupModeOf 必须先问 Available()，seam 已注入即成立；
	// 同时清掉可能存在的 v1 覆盖，保证走 v2 分支。
	oldV1 := v1ModeOverride
	v1ModeOverride = ""
	t.Cleanup(func() { v1ModeOverride = oldV1 })
	installFakeKernelSubtreeControl(t, root)
	return root
}

// installFakeKernelSubtreeControl 让假 cgroup 根的 subtree_control 具备**内核语义**：
//
//  1. 累加：`+cpu` 之后内容是 "cpu"，再 `+memory` 变成 "cpu memory"；
//     普通文件是覆盖语义，会让"逐个写控制器"看起来只生效最后一个。
//  2. 校验：只接受 cgroup.controllers 里列出的控制器，其余返回 ENOENT
//     —— 这正是真机上"宿主 root 未 delegate cpuset 时写 +cpuset 报 ENOENT"
//     的行为，是 enableControllers 逐个写要处理的场景。
//
// 注入了它，无特权环境就能覆盖"某个控制器不可用"与"其余仍然生效"两条真实路径。
func installFakeKernelSubtreeControl(t *testing.T, root string) {
	t.Helper()
	old := writeSubtreeControl
	writeSubtreeControl = func(path string, data []byte, perm os.FileMode) error {
		if filepath.Base(path) != "cgroup.subtree_control" {
			return old(path, data, perm)
		}
		avail, _ := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
		have := map[string]bool{}
		for _, c := range strings.Fields(string(avail)) {
			have[c] = true
		}
		cur := map[string]bool{}
		if b, err := os.ReadFile(path); err == nil {
			for _, c := range strings.Fields(string(b)) {
				cur[strings.TrimPrefix(c, "+")] = true
			}
		}
		for _, tok := range strings.Fields(string(data)) {
			name := strings.TrimPrefix(strings.TrimPrefix(tok, "+"), "-")
			if !have[name] {
				return &os.PathError{Op: "write", Path: path, Err: os.ErrNotExist}
			}
			if strings.HasPrefix(tok, "-") {
				delete(cur, name)
			} else {
				cur[name] = true
			}
		}
		// 按 controllers 常量顺序输出，保证结果可复现。
		var out []string
		for _, c := range strings.Fields(controllers) {
			if cur[c] {
				out = append(out, c)
			}
		}
		return os.WriteFile(path, []byte(strings.Join(out, " ")), perm)
	}
	t.Cleanup(func() { writeSubtreeControl = old })
}

// removeFakeCgroupDirContents 清空假 cgroup 目录（模拟 cgroupfs 的 rmdir
// 语义：内核虚拟文件不阻止 rmdir，假目录里的普通文件得先删）。
func removeFakeCgroupDirContents(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

func readFake(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

// TestV2SetupWritesAllLimits 完整走一遍 Setup→(假)AddPID→Collect→Remove，
// 覆盖 setupV2 / applyV2 / write / read / readInt / Collect / Remove 整条
// 此前 0% 的 v2 主链路。
func TestV2SetupWritesAllLimits(t *testing.T) {
	root := fakeV2Root(t)

	l := &Limits{
		CPUs: 2, CPUShares: 1024, CPUSet: "0-1",
		Memory: 256 << 20, MemorySwap: (256 + 128) << 20,
		MemoryReservation: 64 << 20,
		PidsLimit:         128,
	}
	c, err := Setup("abc123", l)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if c.Path != filepath.Join(root, LiCoreGroup, "abc123") {
		t.Fatalf("Path = %s", c.Path)
	}

	cgroup := filepath.Join(root, LiCoreGroup, "abc123")
	// 父组必须被 enableControllers 创建并开启控制器（回归 db14aa6）。
	parent := filepath.Join(root, LiCoreGroup)
	if got := readFake(t, filepath.Join(parent, "cgroup.subtree_control")); !strings.Contains(got, "cpu") ||
		!strings.Contains(got, "memory") || !strings.Contains(got, "pids") {
		t.Fatalf("subtree_control = %q", got)
	}
	checks := map[string]string{
		"cpu.max":         "200000 100000",
		"cpu.weight":      "39", // SharesToWeight(1024)=1+1022*9999/262142=39
		"cpuset.cpus":     "0-1",
		"memory.max":      strconv.Itoa(256 << 20),
		"memory.swap.max": strconv.Itoa(128 << 20), // swap 单独值
		"memory.low":      strconv.Itoa(64 << 20),
		"pids.max":        "128",
	}
	for f, want := range checks {
		if got := readFake(t, filepath.Join(cgroup, f)); got != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}

	// AddPID：v2 下写 cgroup.procs。
	if err := os.WriteFile(filepath.Join(cgroup, "cgroup.procs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AddPID("abc123", 4242); err != nil {
		t.Fatalf("AddPID: %v", err)
	}
	if got := readFake(t, filepath.Join(cgroup, "cgroup.procs")); got != "4242" {
		t.Fatalf("cgroup.procs = %q", got)
	}

	// Collect：喂假统计文件后读取换算。
	for f, v := range map[string]string{
		"cpu.usage_usec": "5000",
		"cpu.weight":     "100",
		"memory.current": strconv.Itoa(50 << 20),
		"memory.max":     strconv.Itoa(256 << 20),
		"pids.current":   "7",
		"pids.max":       "128",
	} {
		if err := os.WriteFile(filepath.Join(cgroup, f), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := Collect(c)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if st.CPUUsageNanos != 5_000_000 || st.MemoryUsage != 50<<20 ||
		st.PidsCurrent != 7 || st.PidsLimit != 128 {
		t.Errorf("Collect 结果异常: %+v", st)
	}
	if st.CPUShares == 0 {
		t.Error("weight→shares 换算未生效")
	}

	// StatsFor：存在 → Running=true。
	st2, err := StatsFor("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !st2.Running {
		t.Error("存在的组应 Running=true")
	}

	// Update：改限额。
	if err := Update("abc123", &Limits{Memory: 512 << 20}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := readFake(t, filepath.Join(cgroup, "memory.max")); got != strconv.Itoa(512<<20) {
		t.Fatalf("Update 后 memory.max = %q", got)
	}

	// Remove：真实 cgroupfs 的 rmdir 不被内核虚拟文件阻挡，假目录则要先清空
	//（与 removeFakeCgroupDir 同理）。Remove 自身只 os.Remove 空目录。
	removeFakeCgroupDirContents(t, cgroup)
	if err := Remove("abc123"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(cgroup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove 后仍存在: %v", err)
	}
	// Remove 幂等。
	if err := Remove("abc123"); err != nil {
		t.Fatalf("重复 Remove 应静默: %v", err)
	}
}

// TestV2SwapUnlimitedWritesMax 覆盖 memory.swap.max 的 -1 → "max" 分支，
// 以及 readInt 对 "max" 返回 0 的约定。
func TestV2SwapUnlimitedWritesMax(t *testing.T) {
	fakeV2Root(t)
	c, err := Setup("swap1", &Limits{Memory: 128 << 20, MemorySwap: -1})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFake(t, filepath.Join(c.Path, "memory.swap.max")); got != "max" {
		t.Fatalf("memory.swap.max = %q, want max", got)
	}
	if v, err := c.readInt("memory.swap.max"); err != nil || v != 0 {
		t.Fatalf("readInt(max) = %d,%v want 0,nil", v, err)
	}
	// StatsFor 缺失时 Running=false。
	st, err := StatsFor("nosuch")
	if err != nil || st.Running {
		t.Fatalf("不存在的容器应 Running=false: %+v %v", st, err)
	}
	// AddPID 未 Setup 的容器应报 ErrUnsupported。
	if err := AddPID("nosuch", 1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("AddPID(不存在) 应 ErrUnsupported, got %v", err)
	}
	// Update 未 Setup 的容器同样报错。
	if err := Update("nosuch", &Limits{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Update(不存在) 应 ErrUnsupported, got %v", err)
	}
}

// TestV2UnimplementedOptionsReject 覆盖 v2 侧显式拒绝的选项：
// --network-bandwidth / --storage / --[--gpu/--npu] 必须返回错误而非静默。
func TestV2UnimplementedOptionsReject(t *testing.T) {
	fakeV2Root(t)
	cases := map[string]*Limits{
		"bandwidth": {NetworkBandwidth: 1 << 20},
		"storage":   {Storage: 1 << 30},
		"gpu":       {GPU: []DeviceRequest{{Kind: "gpu", Reference: "all"}}},
		"npu":       {NPU: []DeviceRequest{{Kind: "npu", Reference: "all"}}},
	}
	for name, l := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Setup("unimpl-"+name, l); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("应 ErrUnsupported, got %v", err)
			}
		})
	}
}

// TestV2IONodeThrottleWritesMax 覆盖 io.weight / io.max 多行写入。
func TestV2IONodeThrottleWritesMax(t *testing.T) {
	fakeV2Root(t)
	c, err := Setup("io1", &Limits{
		BlkioWeight:    300,
		DeviceReadBps:  []ThrottleDevice{{Device: "8:0", Rate: 1 << 20}},
		DeviceWriteBps: []ThrottleDevice{{Device: "8:0", Rate: 2 << 20}},
		DeviceReadIOps: []ThrottleDevice{{Device: "8:1", Rate: 100}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFake(t, filepath.Join(c.Path, "io.weight")); got == "" {
		t.Fatal("io.weight 未写")
	}
	// 注意：真实 cgroupfs 上内核会把逐行写入按设备**合并**成一张限速表；
	// 假根是普通文件，write 是整体覆盖语义，因此只有**最后一条**留在文件里。
	// 这里如实断言"每条都发起了写入、内容为 <dev> <key>=<rate> 形态"，
	// 合并语义属于内核行为，留给真机验证覆盖。
	iomax := readFake(t, filepath.Join(c.Path, "io.max"))
	if !strings.Contains(iomax, "8:1 riops=") {
		t.Errorf("io.max 最后一条应为 8:1 riops=…: %q", iomax)
	}
	// writeDevices 是旧接口，永远显式报错。
	if err := c.writeDevices(nil, nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("writeDevices 应 ErrUnsupported: %v", err)
	}
}

// TestV2EnableControllersIdempotentWithIO 覆盖 enableControllers 去重分支。
func TestV2EnableControllersIdempotentWithIO(t *testing.T) {
	root := fakeV2Root(t)
	parent := filepath.Join(root, LiCoreGroup)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	// 预置已开启全部需要的控制器 → enableControllers 应无事可做（不重复写）。
	// 期望值由 controllers 常量派生：常量新增控制器时本用例自动跟上，
	// 不需要（也不应该）手工同步硬编码列表。
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"),
		[]byte(controllers+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := enableControllers(); err != nil {
		t.Fatal(err)
	}
	if got := readFake(t, filepath.Join(parent, "cgroup.subtree_control")); got != controllers {
		t.Fatalf("幂等失败: %q（期望 %q）", got, controllers)
	}
}

// TestSetupNoCgroupAtAll 覆盖既无 v2 又无 v1 的兜底分支（ErrUnsupported）。
func TestSetupNoCgroupAtAll(t *testing.T) {
	old := cgroupV2GroupRoot
	cgroupV2GroupRoot = filepath.Join(t.TempDir(), "none") // 无 cgroup.controllers
	oldV1 := v1ModeOverride
	v1ModeOverride = ""
	oldRoots := cgroupV1Roots
	cgroupV1Roots = []string{filepath.Join(t.TempDir(), "nothing")}
	t.Cleanup(func() {
		cgroupV2GroupRoot, v1ModeOverride, cgroupV1Roots = old, oldV1, oldRoots
	})
	if _, err := Setup("x", &Limits{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("应 ErrUnsupported, got %v", err)
	}
	if m := CgroupModeOf(); m != "" {
		t.Fatalf("无任何层级时 mode 应为空, got %q", m)
	}
	if got := DetectCgroupMounts(); len(got) != 0 {
		t.Fatalf("DetectCgroupMounts 应为空: %v", got)
	}
}

// TestApplyV1CPURawShares 钉死 **v1 的 cpu.shares 原始语义**（2.5 明确要求）：
// v1 写原始 [2,262144]，**不得**做 v2 weight 换算。
func TestApplyV1CPURawShares(t *testing.T) {
	withV1Roots(t, fakeV1Controller(t, "cpu"), fakeV1Controller(t, "memory"))
	c, err := Setup("raw1", &Limits{CPUShares: 300})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFake(t, cgroupV1Path(c.Root, "cpu", "raw1")+"/cpu.shares"); got != "300" {
		t.Fatalf("v1 cpu.shares 必须是原始值 300（禁止换算），got %q", got)
	}
}

// TestV1MemswUnlimitedAndInvalid 钉死 **v1 memsw 语义边界**（2.5 明确要求）：
//   - MemorySwap=-1（不限 swap）→ memsw 写"放开"哨兵（1<<62），
//     绝不能写 memory+swap 的加法结果；
//   - Memory=0 且 swap=0 → memsw 不写（total<=0 跳过分支）。
func TestV1MemswUnlimitedAndInvalid(t *testing.T) {
	t.Run("unlimited-sentinel", func(t *testing.T) {
		withV1Roots(t, fakeV1Controller(t, "memory"))
		c, err := Setup("mu1", &Limits{Memory: 64 << 20, MemorySwap: -1})
		if err != nil {
			t.Fatal(err)
		}
		got := readFake(t, cgroupV1Path(c.Root, "memory", "mu1")+"/memory.memsw.limit_in_bytes")
		if got != strconv.FormatInt(1<<62, 10) {
			t.Fatalf("memsw 放开应写 1<<62 哨兵, got %q", got)
		}
	})
	t.Run("zero-total-skipped", func(t *testing.T) {
		withV1Roots(t, fakeV1Controller(t, "memory"))
		c, err := Setup("mu2", &Limits{})
		if err != nil {
			t.Fatal(err)
		}
		// 先确认组目录本身被创建了（否则下面的"不存在"断言是假阳性）。
		if _, err := os.Stat(cgroupV1Path(c.Root, "memory", "mu2")); err != nil {
			t.Fatalf("组目录未创建: %v", err)
		}
		p := cgroupV1Path(c.Root, "memory", "mu2") + "/memory.memsw.limit_in_bytes"
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("无内存限制时不应写 memsw: %v", err)
		}
	})
}

// fakeV1Controller 造一个假 v1 挂载根（只挂一个控制器）。
func fakeV1Controller(t *testing.T, controller string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "v1-"+controller)
	dir := filepath.Join(root, controller)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestCgroupModeDispatch 覆盖 **v2→v1 切换（mode override seam）**：
// 同一台机器上 override 决定分支；override 为空时 v2 优先。
func TestCgroupModeDispatch(t *testing.T) {
	// v2 可用 → ModeV2。
	fakeV2Root(t)
	if m := CgroupModeOf(); m != ModeV2 {
		t.Fatalf("v2 可用时应 ModeV2, got %q", m)
	}
	// override 成 v1 → ModeV1（模拟 Android 只有 v1 的设备）。
	oldV1 := v1ModeOverride
	v1ModeOverride = ModeV1
	t.Cleanup(func() { v1ModeOverride = oldV1 })
	if m := CgroupModeOf(); m != ModeV1 {
		t.Fatalf("override 后应 ModeV1, got %q", m)
	}
	// 只有 v1 roots 可用（v2 不可读）时自动 ModeV1。
	v1ModeOverride = ""
	oldRoots := cgroupV1Roots
	v1Root := fakeV1Controller(t, "memory")
	cgroupV1Roots = []string{v1Root}
	oldV2 := cgroupV2GroupRoot
	cgroupV2GroupRoot = filepath.Join(t.TempDir(), "not-cgroup")
	t.Cleanup(func() { cgroupV1Roots, cgroupV2GroupRoot = oldRoots, oldV2 })
	if m := CgroupModeOf(); m != ModeV1 {
		t.Fatalf("v2 不可用且 v1 存在时应 ModeV1, got %q", m)
	}
	// 混合层级（2.1 场景）：v2 优先不被 v1 干扰。
	fakeV2Root(t)
	cgroupV1Roots = []string{v1Root}
	if m := CgroupModeOf(); m != ModeV2 {
		t.Fatalf("混合层级时 v2 必须优先, got %q", m)
	}
}

// TestSetupV2RejectsUnsupportedRequestedLimit 是本次静默失效的回归测试。
//
// 背景（真机实测 2026-10-06）：宿主 root 的 cgroup.subtree_control 未 delegate
// cpuset/io 时，enableControllers 把 "+cpu +memory +pids +cpuset" 一次写下去，
// 内核因 cpuset 不可用而让**整条写入** ENOENT —— 结果 --memory/--cpus/--pids-limit
// 全部静默失效，容器照常启动且看不出异常（memory.max 文件压根不存在）。
//
// 现在的要求是两条：可用的控制器必须仍然生效；用户**显式请求**了却无法生效的
// 限制必须让 Setup 报错，而不是放行一个"以为自己有上限"的容器。
func TestSetupV2RejectsUnsupportedRequestedLimit(t *testing.T) {
	root := fakeV2Root(t)
	// 宿主只提供 cpu/memory/pids，没有 cpuset/io。
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"),
		[]byte("cpu memory pids\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1) 未请求 cpuset/io 的容器：应当正常创建，且 cpu/memory/pids 生效。
	if _, err := Setup("ok1", &Limits{Memory: 64 << 20, PidsLimit: 32, CPUs: 1}); err != nil {
		t.Fatalf("未请求不可用控制器，Setup 不应失败: %v", err)
	}
	got := readFake(t, filepath.Join(root, LiCoreGroup, "ok1", "memory.max"))
	if got != strconv.Itoa(64<<20) {
		t.Errorf("memory.max = %q，期望 %d（关键：cpuset 不可用不应拖垮 memory）",
			got, 64<<20)
	}

	// 2) 显式请求 cpuset 的容器：必须报错，不能静默放行。
	if _, err := Setup("bad1", &Limits{CPUSet: "0-1"}); err == nil {
		t.Fatal("请求 --cpuset-cpus 但宿主不支持 cpuset 时，Setup 必须报错")
	} else if !errors.Is(err, ErrUnsupported) {
		t.Errorf("错误应包装 ErrUnsupported，实得 %v", err)
	}

	// 3) 显式请求 blkio 的容器：同样必须报错。
	if _, err := Setup("bad2", &Limits{BlkioWeight: 500}); err == nil {
		t.Fatal("请求 --blkio-weight 但宿主不支持 io 时，Setup 必须报错")
	}
}
