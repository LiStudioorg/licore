// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package resource

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// withV1Roots 把 cgroupV1Roots 临时指向给定的测试根，并强制形态判定为 v1，
// 测试结束自动还原。这样可以在只有 cgroup v2 的开发机上完整测试 v1 路径。
func withV1Roots(t *testing.T, roots ...string) {
	t.Helper()
	oldRoots := cgroupV1Roots
	oldMode := v1ModeOverride
	cgroupV1Roots = roots
	v1ModeOverride = ModeV1
	t.Cleanup(func() {
		cgroupV1Roots = oldRoots
		v1ModeOverride = oldMode
	})
}

// fakeV1Root 在临时目录里造出一个 cgroup v1 挂载形态的假根：
// 每个给定控制器建一个目录并放入 cgroup.procs（挂载后内核必然提供）。
func fakeV1Root(t *testing.T, controllers ...string) string {
	t.Helper()
	root := t.TempDir()
	addV1Controller(t, root, controllers...)
	return root
}

// addV1Controller 往已有的假根里追加控制器目录。
func addV1Controller(t *testing.T, root string, controllers ...string) {
	t.Helper()
	for _, c := range controllers {
		dir := filepath.Join(root, c)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("建控制器目录 %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o644); err != nil {
			t.Fatalf("写 %s/cgroup.procs: %v", c, err)
		}
	}
}

// removeFakeCgroupDir 模拟真实 cgroupfs 的 rmdir 语义：内核虚拟文件不阻止
// rmdir，因此测试里先把假目录清空再删，等价于内核的行为。
func removeFakeCgroupDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 %s: %v", dir, err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			t.Fatalf("清理 %s/%s: %v", dir, e.Name(), err)
		}
	}
	if err := os.Remove(dir); err != nil {
		t.Fatalf("删除 %s: %v", dir, err)
	}
}

// readTestFile 读取文件内容（去空白），失败即 Fatal。
func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

// TestV1AvailableControllers 覆盖控制器探测：只有带 cgroup.procs 的目录
// 才算已挂载，空目录必须被忽略。
func TestV1AvailableControllers(t *testing.T) {
	root := t.TempDir()
	addV1Controller(t, root, "memory", "pids")
	// 造一个只有目录、没有 cgroup.procs 的假控制器，应被忽略。
	if err := os.MkdirAll(filepath.Join(root, "cpu"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := v1AvailableControllers(root)
	sort.Strings(got)
	want := []string{"memory", "pids"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestV1RootFallback 覆盖根挂载点回退：首选根无控制器时用备用根。
func TestV1RootFallback(t *testing.T) {
	primary := t.TempDir() // 空目录，模拟无控制器
	alt := fakeV1Root(t, "memory")
	withV1Roots(t, primary, alt)

	if got := v1Root(); got != alt {
		t.Fatalf("v1Root = %q, want %q", got, alt)
	}
}

// TestV1RootNone 覆盖两个根都不可用：返回空串。
func TestV1RootNone(t *testing.T) {
	withV1Roots(t, t.TempDir(), t.TempDir())
	if got := v1Root(); got != "" {
		t.Fatalf("v1Root = %q, want empty", got)
	}
}

// TestV1PrimaryControllerPreference 覆盖主控制器选择优先级：
// memory > cpu > cpuacct > 其它（按 name 顺序）。
func TestV1PrimaryControllerPreference(t *testing.T) {
	cases := []struct {
		name        string
		controllers []string
		want        string
	}{
		{"memory-preferred", []string{"blkio", "cpu", "memory"}, "memory"},
		{"cpu-when-no-memory", []string{"blkio", "pids", "cpu"}, "cpu"},
		{"cpuacct-when-no-memory-cpu", []string{"cpuacct", "pids"}, "cpuacct"},
		{"fallback-first-available", []string{"pids", "blkio"}, "pids"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := fakeV1Root(t, tc.controllers...)
			if got := v1PrimaryController(root); got != tc.want {
				t.Fatalf("v1PrimaryController = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNewV1CgroupPath 覆盖 v1 句柄的路径派生。
func TestNewV1CgroupPath(t *testing.T) {
	root := fakeV1Root(t, "memory", "cpu")
	c := newV1Cgroup("abc123", root)
	want := filepath.Join(root, "memory", LiCoreGroup, "abc123")
	if c.Path != want {
		t.Fatalf("Path = %q, want %q", c.Path, want)
	}
	if c.Root != root || c.ContainerID != "abc123" {
		t.Fatalf("句柄字段错误: %+v", c)
	}
}

// TestSetupV1CreatesAllControllerDirs 覆盖 v1 Setup：
// 必须在**每个**可用控制器下都建出容器目录。
func TestSetupV1CreatesAllControllerDirs(t *testing.T) {
	root := fakeV1Root(t, "memory", "cpu", "pids")
	withV1Roots(t, root)

	if _, err := Setup("c1", &Limits{Memory: 64 << 20, PidsLimit: 32}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	for _, ctrl := range []string{"memory", "cpu", "pids"} {
		dir := cgroupV1Path(root, ctrl, "c1")
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("控制器 %s 下未建出容器目录 %s: %v", ctrl, dir, err)
		}
	}
	// 限制必须真的落到对应控制器文件上。
	base := cgroupV1Path(root, "memory", "c1")
	if got := readTestFile(t, filepath.Join(base, "memory.limit_in_bytes")); got != strconv.Itoa(64<<20) {
		t.Fatalf("memory.limit_in_bytes = %q", got)
	}
	pidsPath := filepath.Join(cgroupV1Path(root, "pids", "c1"), "pids.max")
	if got := readTestFile(t, pidsPath); got != "32" {
		t.Fatalf("pids.max = %q", got)
	}
}

// TestApplyV1MemoryAndSwap 覆盖内存与 swap 写入。
func TestApplyV1MemoryAndSwap(t *testing.T) {
	root := fakeV1Root(t, "memory")
	withV1Roots(t, root)

	if _, err := Setup("c2", &Limits{Memory: 32 << 20, MemorySwap: 96 << 20, MemoryReservation: 16 << 20}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	base := cgroupV1Path(root, "memory", "c2")
	if got := readTestFile(t, filepath.Join(base, "memory.limit_in_bytes")); got != strconv.Itoa(32<<20) {
		t.Fatalf("limit = %q", got)
	}
	// v1 的 memsw 是"内存 + swap"的总和：32MiB + 64MiB = 96MiB。
	// 这里是最容易搞错的地方——SwapLimit() 返回的是 swap 单独值。
	if got := readTestFile(t, filepath.Join(base, "memory.memsw.limit_in_bytes")); got != strconv.Itoa(96<<20) {
		t.Fatalf("memsw = %q, want %d (内存+swap)", got, 96<<20)
	}
	if got := readTestFile(t, filepath.Join(base, "memory.soft_limit_in_bytes")); got != strconv.Itoa(16<<20) {
		t.Fatalf("soft_limit = %q", got)
	}
}

// TestApplyV1CPU 覆盖 CPU 配额与份额：v1 用 cfs_quota_us/cfs_period_us 与 cpu.shares。
func TestApplyV1CPU(t *testing.T) {
	root := fakeV1Root(t, "cpu")
	withV1Roots(t, root)

	if _, err := Setup("c3", &Limits{CPUs: 0.5, CPUShares: 512}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	base := cgroupV1Path(root, "cpu", "c3")
	// 0.5 CPU = 50000us 配额 / 100000us 周期。
	if got := readTestFile(t, filepath.Join(base, "cpu.cfs_quota_us")); got != "50000" {
		t.Fatalf("cfs_quota_us = %q, want 50000", got)
	}
	if got := readTestFile(t, filepath.Join(base, "cpu.cfs_period_us")); got != strconv.Itoa(DefaultCPUPeriod) {
		t.Fatalf("cfs_period_us = %q", got)
	}
	// v1 的 cpu.shares 直接用 --cpu-shares 的值（不经 weight 换算）。
	if got := readTestFile(t, filepath.Join(base, "cpu.shares")); got != "512" {
		t.Fatalf("cpu.shares = %q, want 512", got)
	}
}

// TestApplyV1CpusetInitialized 覆盖 cpuset 需要同时初始化 cpus 与 mems，
// 否则内核会拒绝写入（EINVAL）。
func TestApplyV1CpusetInitialized(t *testing.T) {
	root := fakeV1Root(t, "cpuset")
	withV1Roots(t, root)

	if _, err := Setup("c4", &Limits{CPUSet: "0-1"}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	base := cgroupV1Path(root, "cpuset", "c4")
	if got := readTestFile(t, filepath.Join(base, "cpuset.cpus")); got != "0-1" {
		t.Fatalf("cpuset.cpus = %q", got)
	}
	if got := readTestFile(t, filepath.Join(base, "cpuset.mems")); got != "0-1" {
		t.Fatalf("cpuset.mems 必须一并初始化, got %q", got)
	}
}

// TestApplyV1MissingControllerDegrades 覆盖关键取舍：请求了某个控制器但
// 设备没挂载它时，必须优雅降级（不阻断），而不是整体失败。
func TestApplyV1MissingControllerDegrades(t *testing.T) {
	root := fakeV1Root(t, "memory") // 故意不挂 pids / cpu
	withV1Roots(t, root)

	c, err := Setup("c5", &Limits{Memory: 32 << 20, PidsLimit: 16, CPUs: 1})
	if err != nil {
		t.Fatalf("缺少控制器时不应失败: %v", err)
	}
	// memory 落盘成功。
	if got := readTestFile(t, filepath.Join(c.Path, "memory.limit_in_bytes")); got != strconv.Itoa(32<<20) {
		t.Fatalf("memory.limit_in_bytes = %q", got)
	}
	// pids 未挂载：不应创建目录，也不应报错。
	if _, err := os.Stat(cgroupV1Path(root, "pids", "c5")); !os.IsNotExist(err) {
		t.Fatalf("未挂载的 pids 不应被创建: %v", err)
	}
}

// TestSetupV1NoControllers 覆盖一个控制器都没有时的明确错误。
func TestSetupV1NoControllers(t *testing.T) {
	empty := t.TempDir()
	withV1Roots(t, empty)

	_, err := setupV1("c6", nil, empty)
	if err == nil {
		t.Fatal("无控制器时应报错")
	}
	if !strings.Contains(err.Error(), "无可用 cgroup v1 控制器") {
		t.Fatalf("错误信息不清晰: %v", err)
	}
}

// TestAddPIDV1 覆盖 PID 写入：应写进全部已存在的控制器目录。
func TestAddPIDV1(t *testing.T) {
	root := fakeV1Root(t, "memory", "pids")
	withV1Roots(t, root)

	if _, err := Setup("c7", nil); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := AddPID("c7", 4242); err != nil {
		t.Fatalf("AddPID: %v", err)
	}
	for _, ctrl := range []string{"memory", "pids"} {
		path := filepath.Join(cgroupV1Path(root, ctrl, "c7"), "cgroup.procs")
		if got := readTestFile(t, path); got != "4242" {
			t.Fatalf("%s/cgroup.procs = %q, want 4242", ctrl, got)
		}
	}
}

// TestAddPIDV1ContainerMissing 覆盖容器目录不存在：应报错而非静默成功。
func TestAddPIDV1ContainerMissing(t *testing.T) {
	root := fakeV1Root(t, "memory")
	withV1Roots(t, root)

	err := AddPID("nonexistent", 1)
	if err == nil {
		t.Fatal("容器目录不存在时应报错")
	}
}

// TestRemoveV1 覆盖删除：所有控制器下的目录都要清掉，且幂等。
func TestRemoveV1(t *testing.T) {
	root := fakeV1Root(t, "memory", "cpu", "pids")
	withV1Roots(t, root)

	if _, err := Setup("c8", &Limits{Memory: 1 << 20}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	// 测试环境是普通文件系统，rmdir 不会像真实 cgroupfs 那样忽略内部文件；
	// 先把各控制器目录清空，再调用被测的 Remove。
	for _, ctrl := range []string{"memory", "cpu", "pids"} {
		removeFakeCgroupDir(t, cgroupV1Path(root, ctrl, "c8"))
	}
	if err := Remove("c8"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, ctrl := range []string{"memory", "cpu", "pids"} {
		if _, err := os.Stat(cgroupV1Path(root, ctrl, "c8")); !os.IsNotExist(err) {
			t.Fatalf("控制器 %s 下的目录未删除", ctrl)
		}
	}
	// 幂等：再删一次不应报错（目录已不存在）。
	if err := Remove("c8"); err != nil {
		t.Fatalf("重复 Remove 应幂等: %v", err)
	}
}

// TestCollectV1 覆盖用量采集的字段映射（与 v2 Collect 对齐）。
func TestCollectV1(t *testing.T) {
	root := fakeV1Root(t, "memory", "cpu", "cpuacct", "pids")
	withV1Roots(t, root)

	if _, err := Setup("c9", nil); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	// 造出内核会提供的用量文件。
	write := func(ctrl, file, val string) {
		t.Helper()
		p := filepath.Join(cgroupV1Path(root, ctrl, "c9"), file)
		if err := os.WriteFile(p, []byte(val), 0o644); err != nil {
			t.Fatalf("写 %s: %v", p, err)
		}
	}
	write("cpuacct", "cpuacct.usage", "1500000") // 纳秒
	write("cpu", "cpu.shares", "512")
	write("memory", "memory.usage_in_bytes", "1048576")
	write("memory", "memory.limit_in_bytes", "2097152")
	write("pids", "pids.current", "7")
	write("pids", "pids.max", "32")

	st, err := StatsFor("c9")
	if err != nil {
		t.Fatalf("StatsFor: %v", err)
	}
	if !st.Running {
		t.Fatal("Running 应为 true")
	}
	if st.CPUUsageNanos != 1500000 {
		t.Fatalf("CPUUsageNanos = %d, want 1500000", st.CPUUsageNanos)
	}
	if st.CPUShares != 512 {
		t.Fatalf("CPUShares = %d", st.CPUShares)
	}
	if st.MemoryUsage != 1048576 || st.MemoryLimit != 2097152 {
		t.Fatalf("内存字段错误: usage=%d limit=%d", st.MemoryUsage, st.MemoryLimit)
	}
	if st.PidsCurrent != 7 || st.PidsLimit != 32 {
		t.Fatalf("pids 字段错误: current=%d limit=%d", st.PidsCurrent, st.PidsLimit)
	}
}

// TestStatsForV1NotRunning 覆盖容器 cgroup 不存在时返回 Running=false。
func TestStatsForV1NotRunning(t *testing.T) {
	root := fakeV1Root(t, "memory")
	withV1Roots(t, root)

	st, err := StatsFor("ghost")
	if err != nil {
		t.Fatalf("StatsFor: %v", err)
	}
	if st.Running {
		t.Fatal("不存在的容器 Running 应为 false")
	}
}

// TestUpdateV1 覆盖动态调整：容器存在则写入，不存在则明确报错。
func TestUpdateV1(t *testing.T) {
	root := fakeV1Root(t, "memory")
	withV1Roots(t, root)

	if _, err := Setup("c10", &Limits{Memory: 16 << 20}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := Update("c10", &Limits{Memory: 128 << 20}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := readTestFile(t, filepath.Join(cgroupV1Path(root, "memory", "c10"), "memory.limit_in_bytes"))
	if got != strconv.Itoa(128<<20) {
		t.Fatalf("更新后 limit = %q, want %d", got, 128<<20)
	}
	if err := Update("ghost", &Limits{Memory: 1 << 20}); err == nil {
		t.Fatal("更新不存在的容器应报错")
	}
}

// TestV1ControllersFor 覆盖"限制需要哪些控制器"的推导。
func TestV1ControllersFor(t *testing.T) {
	cases := []struct {
		name string
		l    *Limits
		want []string
	}{
		{"nil", nil, nil},
		{"memory-only", &Limits{Memory: 1}, []string{"memory"}},
		{"cpu-quota", &Limits{CPUs: 1}, []string{"cpu"}},
		{"cpuset", &Limits{CPUSet: "0"}, []string{"cpuset"}},
		{"pids", &Limits{PidsLimit: 1}, []string{"pids"}},
		{"blkio-weight", &Limits{BlkioWeight: 100}, []string{"blkio"}},
		{"all", &Limits{Memory: 1, CPUs: 1, CPUSet: "0", PidsLimit: 1, BlkioWeight: 1},
			[]string{"blkio", "cpu", "cpuset", "memory", "pids"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := v1ControllersFor(tc.l)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestDetectCgroupMountsAndMode 在真实宿主上跑一次探测：结果必须自洽，
// 且不依赖宿主具体形态（本机是 v2，Android 真机可能是 v1）。
func TestDetectCgroupMountsAndMode(t *testing.T) {
	mounts := DetectCgroupMounts()
	mode := CgroupModeOf()
	t.Logf("真实宿主: mode=%q mounts=%v", mode, mounts)

	switch mode {
	case ModeV2:
		if len(mounts) == 0 || mounts[0] != CgroupV2Mount {
			t.Fatalf("v2 模式下第一个挂载点应为 %s: %v", CgroupV2Mount, mounts)
		}
	case ModeV1:
		if len(mounts) == 0 {
			t.Fatal("v1 模式下必须有挂载点")
		}
	case "":
		if len(mounts) != 0 {
			t.Fatalf("无 cgroup 时不应有挂载点: %v", mounts)
		}
	default:
		t.Fatalf("未知 cgroup 模式 %q", mode)
	}
}

// TestV1ThenV2Preference 覆盖形态优先级：v2 可用时必须选 v2，
// 保证支持 v2 的设备（含桌面 Linux、新 Android）行为不退化。
// 本用例不注入 v1ModeOverride，走真实的形态判定。
func TestV1ThenV2Preference(t *testing.T) {
	root := fakeV1Root(t, "memory")
	oldRoots := cgroupV1Roots
	cgroupV1Roots = []string{root}
	t.Cleanup(func() { cgroupV1Roots = oldRoots })

	mode := CgroupModeOf()
	if Available() {
		// 本机有 v2：即便注入了可用 v1 根，也必须优先 v2。
		if mode != ModeV2 {
			t.Fatalf("v2 可用时应选 v2, got %q", mode)
		}
	} else if mode != ModeV1 {
		t.Fatalf("无 v2 且 v1 可用时应选 v1, got %q", mode)
	}
}

// withV2GroupRoot 把 v2 父组根指向临时目录（测试缝，生产恒为 CgroupV2Mount）。
func withV2GroupRoot(t *testing.T, root string) {
	t.Helper()
	old := cgroupV2GroupRoot
	cgroupV2GroupRoot = root
	t.Cleanup(func() { cgroupV2GroupRoot = old })
}

// TestEnableControllersCreatesParentGroup 是回归测试：licore 父组不存在时
// enableControllers 必须先把它建出来，否则写 cgroup.subtree_control 会 ENOENT，
// 导致所有资源限制静默失效。
//
// 该缺陷真实发生过：验证脚本的清理会删掉 /sys/fs/cgroup/licore，此后再 run
// 带 --memory/--cpus 的容器就写不进限制（memory.max/cpu.max 全部 missing），
// 而此前一直"看起来正常"只是因为 licore 目录在多次运行之间幸存。
func TestEnableControllersCreatesParentGroup(t *testing.T) {
	root := t.TempDir()
	withV2GroupRoot(t, root)
	installFakeKernelSubtreeControl(t, root)

	// 造出 v2 的可用标记，并把控制器列表写进去。
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory pids\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	group := filepath.Join(root, LiCoreGroup)
	if _, err := os.Stat(group); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("前置条件：%s 不应存在", group)
	}

	if _, err := enableControllers(); err != nil {
		t.Fatalf("enableControllers 应自动创建父组: %v", err)
	}
	fi, err := os.Stat(group)
	if err != nil || !fi.IsDir() {
		t.Fatalf("父组 %s 未被创建: %v", group, err)
	}
	// 真实 cgroupfs 会把写入的 "+cpu" 规范化为文件里的 "cpu"，因此按
	// **空格切分后的 token** 比对，而不是找字面量 "+cpu"。
	got := readTestFile(t, filepath.Join(group, "cgroup.subtree_control"))
	have := map[string]bool{}
	for _, f := range strings.Fields(got) {
		have[strings.TrimPrefix(f, "+")] = true
	}
	for _, c := range []string{"cpu", "memory", "pids"} {
		if !have[c] {
			t.Errorf("subtree_control 缺少 %s: %q", c, got)
		}
	}
}

// TestEnableControllersIdempotent 覆盖重复调用：已有父组与已启用的控制器
// 不应重复追加，也不应报错。
func TestEnableControllersIdempotent(t *testing.T) {
	root := t.TempDir()
	withV2GroupRoot(t, root)
	installFakeKernelSubtreeControl(t, root)
	// 用 controllers 常量派生可用控制器列表，而不是硬编码 ——
	// 否则常量一变（如补上 cpuset）这条幂等用例就会假失败。
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte(controllers+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := enableControllers(); err != nil {
			t.Fatalf("第 %d 次 enableControllers: %v", i+1, err)
		}
	}
	got := readTestFile(t, filepath.Join(root, LiCoreGroup, "cgroup.subtree_control"))
	// 已启用后不应再写（内容保持首次写入的结果）。
	//
	// 必须按**空格切分后的完整 token** 计数，不能对整串做子串计数：
	// 子串计数会把 cpuset 误算成 cpu 的第二次出现（给 controllers 补 cpuset
	// 时就被这条绊过）。真实 cgroupfs 的 token 是裸控制器名（无 "+"），
	// 所以按去前缀后的名字计数，两种写法都能覆盖。
	counts := map[string]int{}
	for _, f := range strings.Fields(got) {
		counts[strings.TrimPrefix(f, "+")]++
	}
	for _, c := range strings.Fields(controllers) {
		if counts[c] != 1 {
			t.Errorf("%s 应恰好出现一次，实得 %d 次: %q", c, counts[c], got)
		}
	}
}
