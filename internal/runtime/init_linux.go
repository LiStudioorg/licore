// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/LiStudioorg/licore/internal/network"
)

// Linux 挂载常量（syscall 包未导出这些位）。
const (
	msRec     = 0x4000  // MS_REC
	msPrivate = 0x40000 // MS_PRIVATE
	msBind    = 0x1000  // MS_BIND

	oldRootPrefix = ".licore_old_root."
)

// prSetNoNewPrivs 是 prctl(2) 的 PR_SET_NO_NEW_PRIVS 选项号（linux/prctl.h）。
//
// 标准库 syscall 既没有 Prctl 包装、也没有这个常量，因此自己定义；
// 调用走 syscall.Syscall(syscall.SYS_PRCTL, ...)，SYS_PRCTL 由标准库按架构给出。
const prSetNoNewPrivs = 38

// setNoNewPrivs 设置 PR_SET_NO_NEW_PRIVS，阻止本进程及其后代通过 execve
// 获得新特权（setuid/setgid 二进制、file capabilities 提权）。
//
// 语义要点：
//   - 该标志**单向不可逆**（设上之后无法清除），且随 fork/exec 继承；
//   - 它**不丢弃 capability**——一个持有全部能力的 root 进程设了它之后
//     依然能做特权操作。它挡的是"通过 execve 提权"这一类，以及作为
//     安装 seccomp 过滤器（SECCOMP_MODE_FILTER）的前置条件；
//   - 因为不可逆，测试必须在子进程里跑，否则会污染整个测试进程。
func setNoNewPrivs() error {
	// prctl 有 5 个参数，但后两个为 0；syscall.Syscall 只传 3 个足够。
	_, _, errno := syscall.Syscall(syscall.SYS_PRCTL, uintptr(prSetNoNewPrivs), 1, 0)
	if errno != 0 {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", errno)
	}
	return nil
}

// 挂载常量补充（syscall 包未导出 MS_NOSUID / MS_NOEXEC / MS_NODEV）。
const (
	msNoSuid  = 0x2   // MS_NOSUID
	msNoDev   = 0x4   // MS_NODEV
	msNoExec  = 0x8   // MS_NOEXEC
	msNoAtime = 0x400 // MS_NOATIME
)

// minDevNodes 是最小可用 rootfs 需要的 /dev 设备节点。
//
// 采用"从宿主 bind 单个节点"而非整体 bind /dev：Android 的 /dev 下有
// binder / ashmem / kgsl 等平台专有节点，整体 bind 会把它们暴露给容器，
// 既无意义也可能带来越权风险。
//
// 清单比你直觉需要的更长，漏掉下面的项会在真实负载上出问题：
//   - full：/dev/full 写满返回 ENOSPC，部分测试与工具依赖；
//   - ptmx：没有它容器内无法开伪终端（exec -it 会失败）；
//   - 其余为常规字符设备。
var minDevNodes = []string{
	"null", "zero", "full", "random", "urandom", "tty", "ptmx",
}

// devSymlinks 是 /dev 下应存在的符号链接，指向 /proc/self/fd 对应项。
// 大量 shell 脚本与工具依赖 /dev/stdin、/dev/fd/N 这类路径；缺失会让
// 形如 `cmd < /dev/stdin`、`bash -c 'echo x > /dev/stderr'` 的用法失败。
var devSymlinks = map[string]string{
	"fd":     "/proc/self/fd",
	"stdin":  "/proc/self/fd/0",
	"stdout": "/proc/self/fd/1",
	"stderr": "/proc/self/fd/2",
}

// defaultShmSize 是 /dev/shm tmpfs 的默认大小（字节）。
// 共享内存是 PostgreSQL、Chromium 等负载的硬性依赖；内核默认的 tmpfs
// 大小是内存的一半，对容器来说过大且不可控，因此显式给一个保守默认值。
const defaultShmSize = 64 << 20

// envShmSize 允许调用方覆盖 /dev/shm 大小（字节，十进制字符串）。
const envShmSize = "LICORE_SHM_SIZE"

// devShmSize 返回本次容器 /dev/shm 的大小：环境变量优先，其次默认值。
// 非法或非正值一律回落默认值，不让坏输入阻断容器启动。
func devShmSize() int64 {
	v := strings.TrimSpace(os.Getenv(envShmSize))
	if v == "" {
		return defaultShmSize
	}
	if n, err := parseSizeSuffix(v); err == nil && n > 0 {
		return n
	}
	// 非法值不能静默用默认值：用户会以为限制生效了。
	slog.Warn("忽略非法的 "+envShmSize+"（期望字节数或带 K/M/G 后缀）",
		slog.String("value", v), slog.Int64("fallback", defaultShmSize))
	return defaultShmSize
}

// parseSizeSuffix 解析 "67108864" / "16m" / "512k" / "1g"（大小写不敏感）。
// 支持后缀是必要的：只收裸字节数时，写 "16m" 的用户会得到静默的默认值，
// 属于最糟糕的失败模式（配置看起来生效了，其实没有）。
func parseSizeSuffix(v string) (int64, error) {
	mult := int64(1)
	if last := v[len(v)-1]; last >= '0' && last <= '9' {
		// 纯数字。
	} else {
		switch last {
		case 'k', 'K':
			mult = 1 << 10
		case 'm', 'M':
			mult = 1 << 20
		case 'g', 'G':
			mult = 1 << 30
		default:
			return 0, strconv.ErrSyntax
		}
		v = v[:len(v)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

// syscallUnmount 卸载一个挂载点（MNT_DETACH：即使仍被占用也延迟卸载）。
// 供 /dev 装配的清理路径与测试使用。
func syscallUnmount(path string) error {
	return syscall.Unmount(path, syscall.MNT_DETACH)
}

// fileInfoSys 取 os.FileInfo 底层的 syscall.Stat_t，用于读取设备号。
// 判断"是否真的是独立挂载点"必须比对设备号，仅看目录存在会被普通目录骗过。
func fileInfoSys(fi os.FileInfo) (*syscall.Stat_t, bool) {
	if fi == nil {
		return nil, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return st, ok
}

// hostProcMountInfo 是宿主 /proc 自检所需的路径，做成变量以便测试注入。
var hostProcMountInfo = "/proc/self/mountinfo"

// parseHidePID 从 mountinfo 内容里解析宿主 /proc 的 hidepid 值。
//
// 返回 (值, 是否找到, 是否解析成功)。三条信息必须分开，因为决策不同：
//   - 找不到 hidepid 选项 → 未设置，调用方不传参数；
//   - 找到但值非法 → 视为解析失败，调用方不传参数（保守）；
//   - 找到且合法 → 用该值判断是否需要显式覆盖。
//
// mountinfo 的字段布局（见 proc(5)）：
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//	①  ②  ③     ④     ⑤     ⑥                    ⑦  ⑧      ⑨        ⑩
//
// 第 5 个字段（索引 4）是挂载点，第 6 个（索引 5）是 per-mount 选项，
// hidepid 属于 per-mount 选项，因此在索引 5 里查找。
//
// 为什么解析 /proc/self/mountinfo 而不是 /proc/mounts：mounts 只给出
// 文件系统级的挂载选项（superblock options），而 hidepid 是 per-mount
// 选项，只有 mountinfo 才完整呈现。
func parseHidePID(mountinfo string) (int, bool, bool) {
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		// 至少需要到 " - " 分隔符前的 6 个字段。
		if len(fields) < 6 {
			continue
		}
		if fields[4] != "/proc" {
			continue
		}
		for _, opt := range strings.Split(fields[5], ",") {
			val, ok := strings.CutPrefix(opt, "hidepid=")
			if !ok {
				continue
			}
			n, err := strconv.Atoi(val)
			if err != nil {
				return 0, true, false // 找到了但值非法
			}
			return n, true, true
		}
		// 找到 /proc 行但其中没有 hidepid → 未设置。
		return 0, false, true
	}
	// 没有 /proc 行（异常环境）→ 视为未设置，不阻断。
	return 0, false, true
}

// procMountOptions 决定容器 /proc 应使用的挂载参数。
//
// 决策表（与设计一致）：
//
//	宿主 hidepid 未设置   → ""（用内核默认）
//	宿主 hidepid = 0      → ""（已经是最宽松，无需覆盖）
//	宿主 hidepid = 1 或 2 → "hidepid=0"（否则容器内看不到自己的进程）
//	解析失败              → ""（保守，且记日志不阻断）
//
// 返回的字符串直接作为 mount(2) 的 data 参数（空串表示不传）。
func procMountOptions(mountinfo string) string {
	hidepid, found, ok := parseHidePID(mountinfo)
	if !ok {
		slog.Debug("解析宿主 /proc hidepid 失败，容器将使用默认挂载参数")
		return ""
	}
	if !found {
		return ""
	}
	if hidepid > 0 {
		return "hidepid=0"
	}
	return ""
}

// mountContainerProc 在 rootfs/proc 挂载绑定当前 PID namespace 的 procfs。
//
// 先按宿主 hidepid 情况决定参数；若带参数挂载失败（内核不支持 hidepid，
// 或受限环境拒绝），回退为不带参数再试一次。两次都失败才报错——这样
// 既能在需要时修正 hidepid，又不会因参数不被支持而阻断容器启动。
func mountContainerProc(rootfs string) error {
	target := filepath.Join(rootfs, "proc")
	opts := procMountOptions(readHostMountInfo())

	if opts != "" {
		if err := mountProcRaw(target, opts); err != nil {
			slog.Debug("带 hidepid 参数挂载 /proc 失败，回退为默认参数",
				slog.String("opts", opts), slog.Any("err", err))
		} else {
			return nil
		}
	}
	if err := mountProcRaw(target, ""); err != nil {
		return fmt.Errorf("挂载 /proc: %w", err)
	}
	return nil
}

// mountProcRaw 是 mountContainerProc 使用的挂载原语，做成变量以便测试注入
// 一个假的挂载函数来验证回退逻辑，而不必真的改宿主挂载表。
var mountProcRaw = func(target, opts string) error {
	return syscall.Mount("proc", target, "proc", 0, opts)
}

// mountProcSysReadOnly 把容器内的 /proc/sys 重挂为只读。
//
// **这是封堵两条 P0 宿主逃逸路径的关键**（真机实测，2026-10-05）：
//
//	/proc/sys/kernel/core_pattern  写管道 → 崩溃时以宿主 root 执行命令
//	/proc/sys/kernel/modprobe      写路径 → 劫持内核模块加载
//
// 注意 /proc/sysrq-trigger **不在**本函数的覆盖范围内——它在 /proc 根下，
// 由 maskProcRootFiles 单独处理（实测确认过这个区别）。
//
// 为什么 capability/seccomp 都挡不住：容器 init 是真正的宿主 uid 0，
// /proc/sys 下文件属 root 且模式 0600/0644，DAC 直接放行；而上述文件的
// 内核 handler 走 proc_dostring，**不做 capable() 检查**，只有 inode 权限。
// 对照实验：kptr_restrict / drop_caches 走 proc_dointvec_*（内部 capable()），
// 在同一容器里被正确拒绝——所以问题不是"能力没收干净"，而是这类文件本就
// 只靠 DAC，而容器恰好在 DAC 上等于宿主 root。
//
// 挂载层封堵的好处：open(O_WRONLY) 在 VFS 就被拒，与各内核接口是否做了
// 能力检查无关，也不需要维护"危险文件清单"（那种清单会随内核版本漂移）。
//
// 姿势与 InstallExecHelper / 卷 :ro 一致：先 MS_BIND 再
// MS_REMOUNT|MS_BIND|MS_RDONLY。**不能**单次
// mount(MS_BIND|MS_RDONLY)——内核会忽略该次的 MS_RDONLY，bind 仍是可写
// （v0.6.0 的教训）。
//
// 返回错误而非静默跳过：封堵失败意味着容器带着 P0 逃逸路径启动，
// 必须让启动失败，不能降级。
func mountProcSysReadOnly(rootfs string) error {
	target := filepath.Join(rootfs, "proc", "sys")
	if _, err := os.Stat(target); err != nil {
		// rootfs 的 /proc 是刚挂上的 procfs，/proc/sys 必然存在；
		// 走到这里说明 procfs 挂载异常，属真实故障，不能装作没事。
		return fmt.Errorf("封堵 /proc/sys：%s 不可达（procfs 挂载异常）: %w", target, err)
	}
	if err := mountRaw(target, target, "", uintptr(msBind), ""); err != nil {
		return fmt.Errorf("bind /proc/sys: %w", err)
	}
	if err := mountRaw(target, target, "",
		uintptr(msBind|syscall.MS_REMOUNT|syscall.MS_RDONLY), ""); err != nil {
		return fmt.Errorf("重挂 /proc/sys 为只读: %w", err)
	}
	slog.Debug("已把容器内 /proc/sys 重挂为只读（封堵 core_pattern / " +
		"modprobe 等宿主逃逸路径）")
	return nil
}

// procRootMaskedFiles 是挂在 /proc 根下、**不在 /proc/sys 内**、且能影响
// 宿主全局状态的 procfs 文件，需要逐个用空只读文件覆盖（mask）。
//
// 为什么不能只靠"把 /proc/sys 挂只读"：sysrq-trigger 的路径是
// /proc/sysrq-trigger —— 它在 /proc 根下，与 /proc/sys/ 目录毫无关系。
// 真机实测确认过：/proc/sys 重挂只读后，core_pattern 与 modprobe 均被拒，
// 而 sysrq-trigger 仍能写入（rc=0）。
//
// 清单只放**实测确认可写、且能破坏宿主**的项。/proc 根下其余写敏感文件
// （kmsg / kcore / kpageflags / mtrr 等）实测已被内核拒绝（写模式的文件
// 返回 EPERM），无需处理——盲目扩大清单会误伤合法用法。
var procRootMaskedFiles = []string{
	// 写 'b' 立即重启宿主；写 'c' 触发崩溃转储。这是最直接的一条。
	"sysrq-trigger",
}

// maskProcRootFiles 用空只读文件覆盖 /proc 根下的危险文件。
//
// 手法与 Docker 的 mask 一致：bind 挂一个"空且只读"的文件到目标路径上，
// 于是 open(O_WRONLY) 拿到只读文件、write 拿不到任何效果，且不依赖内核
// 是否为该文件做 capable() 检查。
//
// 空文件用 /dev/null 之外的选择更好：/dev/null 可写，覆盖后仍能写入（只是
// 丢弃）。因此这里在容器内自建一个 0400 的空文件作为源——写入会因只读
// 挂载与文件权限双重失败。
//
// 必须在 /proc 挂好之后、pivot_root 之前调用（与 /proc/sys 同一时机）。
func maskProcRootFiles(rootfs string) error {
	// 源文件：放在 rootfs 内，随新根进入容器，不依赖宿主任何路径。
	src := filepath.Join(rootfs, procMaskDir, "empty")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		return fmt.Errorf("创建 mask 源目录: %w", err)
	}
	if err := os.WriteFile(src, nil, 0o400); err != nil {
		return fmt.Errorf("创建 mask 源文件: %w", err)
	}

	for _, name := range procRootMaskedFiles {
		target := filepath.Join(rootfs, "proc", name)
		if _, err := os.Stat(target); err != nil {
			// 内核未提供该文件（如未启用 CONFIG_MAGIC_SYSRQ）——正常，跳过。
			slog.Debug("procfs 未提供该文件，跳过 mask",
				slog.String("file", name), slog.Any("err", err))
			continue
		}
		if err := mountRaw(src, target, "", uintptr(msBind), ""); err != nil {
			return fmt.Errorf("mask /proc/%s（bind）: %w", name, err)
		}
		// 与 /proc/sys 同一姿势：先 bind，再 remount 加只读。
		if err := mountRaw(target, target, "",
			uintptr(msBind|syscall.MS_REMOUNT|syscall.MS_RDONLY), ""); err != nil {
			return fmt.Errorf("mask /proc/%s（remount ro）: %w", name, err)
		}
		slog.Debug("已 mask 容器内危险 procfs 文件", slog.String("file", "/proc/"+name))
	}
	return nil
}

// procMaskDir 是容器内放置 mask 源文件的目录（在 rootfs 内，随新根进入容器）。
const procMaskDir = ".licore_mask"

// readHostMountInfo 读取宿主 mountinfo；失败返回空串（调用方按"未设置"处理）。
func readHostMountInfo() string {
	data, err := os.ReadFile(hostProcMountInfo)
	if err != nil {
		slog.Debug("读取宿主 mountinfo 失败，容器 /proc 使用默认参数",
			slog.String("path", hostProcMountInfo), slog.Any("err", err))
		return ""
	}
	return string(data)
}

// selinuxAttrExec 是进程 exec 过渡上下文的 procfs 入口。
//
// 语义：写入该文件的字符串是**本进程下一次 execve** 时切换到的新 SELinux
// 上下文。它作用于调用者自己的下一次 exec，而不是任意别的进程——因此必须
// 在容器 init 进程内、紧邻 execve 之前写，在父进程里写只会影响父进程自己。
var selinuxAttrExec = "/proc/self/attr/exec"

// readSELinuxExecContext 读取当前继承的 exec 上下文。
//
// 返回 (context, ok, err)：
//   - 系统未启用 SELinux 时内核返回 EINVAL/ENOENT，这是**正常情况**而非错误，
//     调用方据此跳过（ok=false, err=nil）；
//   - 读到空串也视为"无上下文"；
//   - 其它错误（如 EACCES）向上返回，由调用方决定是否降级。
//
// 用 /proc/self/attr/exec 而不是 /proc/self/attr/current：前者是 exec 过渡
// 目标（可写），后者是当前运行上下文（多数策略下不可写）。
func readSELinuxExecContext(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.EINVAL) ||
			errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENODATA) {
			return "", false, nil // 未启用 SELinux：正常跳过
		}
		return "", false, err
	}
	ctx := strings.TrimSpace(string(data))
	if ctx == "" {
		return "", false, nil
	}
	return ctx, true, nil
}

// applySELinuxExecContext 把 ctx 写入 path，令容器 init 的 execve 过渡到该
// 上下文。返回错误由调用方决定是否降级。
//
// 只做这一件事：不调用 setenforce、不改任何全局 SELinux 状态、不动策略。
// 容器进程的上下文应与引擎保持一致（继承），这样"引擎能做的事容器也能做"，
// 且不会因为放宽标签而扩大攻击面。
func applySELinuxExecContext(path, ctx string) error {
	if ctx == "" {
		return nil
	}
	if err := os.WriteFile(path, []byte(ctx), 0o644); err != nil {
		return fmt.Errorf("写入 %s: %w", path, err)
	}
	return nil
}

// inheritSELinuxContext 在容器 execve 之前继承引擎的 SELinux exec 上下文。
//
// 返回读取到的上下文（可能为空）。**任何失败都不阻断容器启动**：
// SELinux 未启用、策略拒绝写入、内核不支持都只降级为 slog.Warn/Debug。
// 理由与设计一致——上下文继承是"让容器行为与引擎一致"的优化，
// 而不是容器能否运行的前提；把它做成硬失败会让非 SELinux 设备全盘不可用。
//
// enforcing 下无法设置上下文时，容器内进程可能被策略拦截（例如无法读某类
// 文件），这种降级必须在日志里说清楚，避免用户面对"某操作莫名失败"而无从
// 排查。本函数不做任何"假装成功"的处理。
func inheritSELinuxContext() string {
	ctx, ok, err := readSELinuxExecContext(selinuxAttrExec)
	if err != nil {
		slog.Warn("读取 SELinux exec 上下文失败，容器将沿用内核默认标签",
			slog.String("path", selinuxAttrExec), slog.Any("err", err))
		return ""
	}
	if !ok {
		// 未启用 SELinux（绝大多数 Linux 服务器与部分 Android 设备）：
		// 这是预期路径，不需要噪音日志。
		slog.Debug("未检测到 SELinux exec 上下文，跳过上下文继承")
		return ""
	}
	if err := applySELinuxExecContext(selinuxAttrExec, ctx); err != nil {
		slog.Warn("设置 SELinux exec 上下文失败，容器将继续启动；"+
			"若系统处于 enforcing，容器内进程可能被策略拦截",
			slog.String("context", ctx), slog.Any("err", err))
		return ""
	}
	slog.Debug("已继承 SELinux exec 上下文", slog.String("context", ctx))
	return ctx
}

// executeContainerCmd 是容器 init 的最后一步：收紧权限（no_new_privs +
// capability 裁剪）后设置 SELinux exec 上下文，再 execve 用户命令。
// 抽成独立函数以便单元测试覆盖上下文处理路径。
//
// **顺序在这里是安全属性的一部分**，不能随意调整：
//  1. 权限收紧必须在所有特权准备（mount / pivot_root / 建网卡 / SELinux
//     写 attr/exec 之前的网络配置）**之后**——那些操作需要完整能力，
//     提前丢能力会让容器直接启动失败；
//  2. 又必须在 execve **之前**——否则用户命令会带着完整能力跑起来。
//
// 因此这里是唯一正确的收口位置：init 干完所有需要特权的事，在交出控制权
// 的最后一刻把权限降下来。
func executeContainerCmd(cmdline, env []string) error {
	// 1. no_new_privs：阻止 execve 获得新特权，同时是安装 seccomp 过滤器的前置条件。
	if err := setNoNewPrivs(); err != nil {
		return err
	}
	// 2. 能力裁剪：先清边界集再 capset。**这一步才是真正挡住危险操作的**
	//    （写 /proc/sysrq-trigger、加载 eBPF 都需要 CAP_SYS_ADMIN）；
	//    no_new_privs 本身不丢能力，两者是互补关系。
	kept, err := applyCapabilitiesFromEnv()
	if err != nil {
		return fmt.Errorf("裁剪容器能力失败: %w", err)
	}
	slog.Debug("容器能力已裁剪", slog.Any("kept", kept))
	// 3. seccomp 黑名单：在能力裁剪之后再装。安装过滤器需要 no_new_privs
	//    （第 1 步已设）或 CAP_SYS_ADMIN，因此即便此时 CAP_SYS_ADMIN 已丢，
	//    本调用依然成立。它是纵深防御的第二层——万一某个能力被放回来，
	//    危险调用仍会被 EPERM 挡住。
	if err := installSeccompFilter(); err != nil {
		return fmt.Errorf("安装 seccomp 过滤器失败: %w", err)
	}
	// 4. 解析命令路径。
	//
	// execve 是内核系统调用，**从不做 PATH 查找**——直接执行裸命令名
	// （如 `licore run img sleep 3600`）会得到 ENOENT，即使容器里存在
	// /bin/sleep。Docker 靠 runc 在 exec 前做 LookPath，这里必须做同样的事。
	//
	// 必须在收口之后：此时的 PATH 来自容器 env（已剥掉 LICORE_*），
	// 且查找是在容器 mount namespace 视图内进行的。
	execPath, err := resolveContainerCmd(cmdline, env)
	if err != nil {
		return err
	}
	// 5. SELinux 的 attr/exec 只对本进程的**下一次** execve 生效，必须紧邻 execve。
	inheritSELinuxContext()
	// 6. 切换到用户指定的工作目录与身份（--workdir / --user）。
	//
	// **必须排在能力裁剪之后、execve 之前**：
	//  - 之后：setgid/setuid 需要能力，提前做会与 capset 的语义打架（且
	//    --cap-drop ALL 之后连 setuid 都做不了，那就无法支持
	//    "drop ALL + 非 root 用户"的合法组合）；
	//  - 之前：晚了就来不及——execve 一旦发生，进程已经是用户命令了。
	//
	// 与 exec 路径（RunExecSetup）保持同样的语义：那里也支持 --user/--workdir。
	if err := applyWorkdirAndUser(); err != nil {
		return err
	}
	// 注意：execve 用解析后的路径，但 argv[0] 保持原样——
	// 程序通过 argv[0] 看到的仍是用户写的命令名（与 shell 行为一致）。
	if err := syscall.Exec(execPath, cmdline, env); err != nil {
		return fmt.Errorf("exec %s: %w", execPath, err)
	}
	return nil // 不可达
}

// applyWorkdirAndUser 应用 --workdir 与 --user（经 LICORE_WORKDIR / LICORE_USER
// 下发）。两者都为空时是 no-op（保持历史行为：root 身份、根目录）。
//
// 顺序：先 chdir 再 setgid/setuid。反过来的话，chdir 就要以目标用户身份
// 执行，而用户可能对目标目录没有权限——Docker 也是先切目录再降权的。
//
// **失败必须报错，不许静默忽略**：静默忽略会让用户以为容器以 uid 1000 跑着，
// 实际是 root——这正是"以为收紧了其实没有"的那类安全问题。
func applyWorkdirAndUser() error {
	env := os.Environ()

	if wd := workdirFromEnv(env); wd != "" {
		if err := os.Chdir(wd); err != nil {
			return fmt.Errorf("切换工作目录到 %s 失败: %w", wd, err)
		}
	}

	spec := userFromEnv(env)
	if spec == "" {
		return nil
	}
	uid, gid, err := ParseUserSpec(spec)
	if err != nil {
		return fmt.Errorf("解析 --user %q 失败: %w", spec, err)
	}
	// 先 gid 后 uid：setuid 之后就不再持有 CAP_SETGID，改不动组了。
	if err := syscall.Setgroups([]int{gid}); err != nil {
		// 非 root 起点时允许失败（此时本就无法改组），但继续尝试 setgid。
		slog.Debug("设置附加组失败（忽略）", "gid", gid, "err", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("设置 gid=%d 失败: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("设置 uid=%d 失败: %w", uid, err)
	}
	slog.Debug("容器以指定用户启动", "uid", uid, "gid", gid)
	return nil
}

// setupContainerDev 在 rootfs/dev 下装配容器所需的设备环境。
//
// 顺序有讲究：
//  1. 先 bind 字符设备（需要 rootfs/dev 已存在）；
//  2. 再建 /dev/shm 并挂 tmpfs（独立挂载，避免容器写满宿主 /dev）；
//  3. 再建 /dev/pts 并挂 devpts（exec -it 依赖）；
//  4. 最后建符号链接（纯文件操作，无失败风险但放最后更清晰）。
//
// 任一步骤失败都返回错误而非静默跳过：设备环境不完整会让容器内的
// 表现为"命令莫名失败"，比启动时报错难排查得多。唯一例外是宿主缺少
// 某设备节点——那是宿主本身的问题，跳过即可。
func setupContainerDev(rootfs string) error {
	devDir := filepath.Join(rootfs, "dev")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		return fmt.Errorf("创建 /dev: %w", err)
	}
	if err := bindHostDevices(rootfs); err != nil {
		return err
	}
	if err := mountDevShm(rootfs); err != nil {
		return err
	}
	if err := mountDevPts(rootfs); err != nil {
		return err
	}
	return createDevSymlinks(rootfs)
}

// mountDevShm 在 rootfs/dev/shm 挂 tmpfs。
//
// 先尝试带 size 挂载；内核不支持 size= 时（极老内核或受限环境）退化为
// 不带参数的默认 tmpfs，而不是让容器启动失败——共享内存"有但大小不可控"
// 远好于"完全没有"。
// mountRaw 是通用挂载原语，做成变量以便测试注入（与 mountProcRaw 同一手法）。
//
// /dev 下的 tmpfs / devpts / bind 三条路径都各有"参数不被支持就回退"的分支，
// 这些分支在真实 root 测试里很难稳定触发（取决于宿主内核），因此用注入的方式
// 让"首次失败、回退成功"与"两次都失败"两条路径都能被普通单测覆盖。
var mountRaw = func(source, target, fstype string, flags uintptr, data string) error {
	return syscall.Mount(source, target, fstype, flags, data)
}

func mountDevShm(rootfs string) error {
	shm := filepath.Join(rootfs, "dev", "shm")
	if err := os.MkdirAll(shm, 0o1777); err != nil {
		return fmt.Errorf("创建 /dev/shm: %w", err)
	}
	size := devShmSize()
	flags := uintptr(msNoSuid | msNoDev | msNoExec)
	opts := fmt.Sprintf("size=%d", size)
	if err := mountRaw("tmpfs", shm, "tmpfs", flags, opts); err != nil {
		// 回退：不带 size 参数再试一次。
		if err2 := mountRaw("tmpfs", shm, "tmpfs", flags, ""); err2 != nil {
			return fmt.Errorf("挂载 /dev/shm: %w", err)
		}
		slog.Debug("挂载 /dev/shm 时 size 参数不被支持，已回退默认大小",
			slog.Int64("size", size))
	}
	return nil
}

// mountDevPts 在 rootfs/dev/pts 挂 devpts，供容器内分配伪终端。
// 同样带一次回退：部分内核/环境对 newinstance 支持不完整。
func mountDevPts(rootfs string) error {
	pts := filepath.Join(rootfs, "dev", "pts")
	if err := os.MkdirAll(pts, 0o755); err != nil {
		return fmt.Errorf("创建 /dev/pts: %w", err)
	}
	flags := uintptr(msNoSuid | msNoDev | msNoExec)
	if err := mountRaw("devpts", pts, "devpts", flags, "newinstance,ptmxmode=0666,mode=0620"); err != nil {
		if err2 := mountRaw("devpts", pts, "devpts", flags, "ptmxmode=0666,mode=0620"); err2 != nil {
			return fmt.Errorf("挂载 /dev/pts: %w", err)
		}
		slog.Debug("挂载 /dev/pts 时 newinstance 不被支持，已回退")
	}
	return nil
}

// createDevSymlinks 在 rootfs/dev 下建立 fd/stdin/stdout/stderr 符号链接。
// 已存在则覆盖（rootfs 由镜像层决定，可能预置了指向别处的同名链接）。
func createDevSymlinks(rootfs string) error {
	for name, target := range devSymlinks {
		link := filepath.Join(rootfs, "dev", name)
		if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// 目录等无法删除的形态也不必失败，交给 Symlink 报错更准确。
			slog.Debug("移除既有 /dev 链接失败", slog.String("path", link), slog.Any("err", err))
		}
		if err := os.Symlink(target, link); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("创建 /dev/%s → %s: %w", name, target, err)
		}
	}
	return nil
}

// bindHostDevices 把宿主 /dev 的最小设备集 bind 进 rootfs/dev。
func bindHostDevices(rootfs string) error {
	for _, name := range minDevNodes {
		src := filepath.Join("/dev", name)
		dst := filepath.Join(rootfs, "dev", name)
		if _, err := os.Stat(src); err != nil {
			continue // 宿主没有该设备则跳过
		}
		// 占位文件是 bind 的目标挂载点，必须先有父目录（rootfs/dev 通常
		// 已由调用方创建，但单测与异常 rootfs 里可能缺失）。
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("创建 /dev 目录: %w", err)
		}
		f, err := os.OpenFile(dst, os.O_CREATE, 0o666)
		if err != nil {
			return fmt.Errorf("创建设备占位 %s: %w", dst, err)
		}
		_ = f.Close()
		if err := mountRaw(src, dst, "", msBind, ""); err != nil {
			return fmt.Errorf("bind %s: %w", src, err)
		}
	}
	return nil
}

// RunInit 是容器 1 号进程入口：pivot_root 进新根、挂载最小 /dev 与 /proc，
// 最后 exec 用户命令。仅应由 `licore init` 重执行路径调用（见 IsInitProcess）。
func RunInit() error {
	if !IsInitProcess() {
		return ErrNotInit
	}
	// chdir/chroot/pivot_root 都是线程级 FS 语义，锁定线程避免 Go 调度干扰。
	runtime.LockOSThread()

	rootfs := os.Getenv(envRootfs)
	if rootfs == "" {
		return fmt.Errorf("环境变量 %s 缺失: %w", envRootfs, ErrBadConfig)
	}
	cmdline, err := childCmdline()
	if err != nil {
		return err
	}
	env := envWithoutLiCore()

	if hn := os.Getenv(envHostname); hn != "" {
		if err := syscall.Sethostname([]byte(hn)); err != nil {
			return fmt.Errorf("sethostname %q: %w", hn, err)
		}
	}

	// 每容器唯一旧根目录名：同一 rootfs 上并发多容器共享磁盘目录项，
	// 固定名会让先退出者 Rmdir 掉他人尚未完成 pivot 的旧根。
	oldRoot := oldRootPrefix + instanceID()

	// 1. 全树挂载事件设 private：切断宿主共享子树的传播，也是 bind/pivot 的前置条件。
	if err := syscall.Mount("", "/", "", uintptr(msRec|msPrivate), ""); err != nil {
		return fmt.Errorf("根挂载设 private: %w", err)
	}
	// 2. 预建目录与最小 /dev（此刻宿主路径仍可见）。
	for _, d := range []string{"proc", "dev", "tmp", oldRoot} {
		if err := os.MkdirAll(filepath.Join(rootfs, d), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	// 2.5 装配容器 /dev：字符设备 + shm(tmpfs) + pts(devpts) + 标准符号链接。
	if err := setupContainerDev(rootfs); err != nil {
		return err
	}
	// 3. rootfs 必须自身是挂载点；递归 bind 自身一次（连带把 dev bind 复制进新子树）。
	if err := syscall.Mount(rootfs, rootfs, "", uintptr(msRec|msBind), ""); err != nil {
		return fmt.Errorf("bind rootfs %s: %w", rootfs, err)
	}
	// 3.5 卷挂载：把宿主任一命名卷/匿名卷/bind 源 bind 或 tmpfs 进 rootfs
	//    目标路径。必须在 pivot_root 之前，保证挂载随新根一同进入容器。
	if err := mountRootfsVolumes(rootfs); err != nil {
		return err
	}
	// 3.6 exec helper：把 licore 自身只读 bind 进容器固定路径。
	//     与卷同一时机（pivot_root 之前），原因相同。
	//     `licore exec` 在容器内执行它来做收口——否则 exec 进来的进程是
	//     宿主 root 满能力，容器 init 的隔离对它完全无效。
	if err := InstallExecHelper(rootfs); err != nil {
		return err
	}
	// 4. 先挂 procfs（早于 pivot_root）。部分环境（如嵌套容器）在 pivot 并卸载旧根后
	//    拒绝再建 proc 超块；pivot 前挂载得到的是绑定本 PID namespace 的全新 procfs，
	//    随 rootfs 一起进入新根，语义完全等价。
	//    参数按宿主 hidepid 情况决定：宿主为 hidepid=1/2 时显式覆盖为 0，
	//    否则容器内 ps 看不到自己的进程（Android 有设备默认 hidepid=2）。
	if err := mountContainerProc(rootfs); err != nil {
		return err
	}
	// 4.5 把 /proc/sys 重挂为只读（必须在 /proc 挂好之后，pivot_root 之前）。
	//
	// 为什么必须做（真机实测确认的 P0 逃逸路径，2026-10-05）：
	// 容器 init 是**真正的宿主 uid 0**（root 下刻意不加 CLONE_NEWUSER，见
	// nsplan_linux.go 的决策表），而 /proc/sys 下的文件属 root、模式 0600/0644。
	// 于是 DAC 检查直接通过——**只要该文件自身没有额外的 capable() 检查**，
	// 容器内就能写穿宿主：
	//
	//	容器内：echo b > /proc/sysrq-trigger        → 重启宿主
	//	容器内：echo X > /proc/sys/kernel/core_pattern → 崩溃时以宿主 root 执行
	//	容器内：echo X > /proc/sys/kernel/modprobe     → 劫持模块加载路径
	//
	// capability 裁剪对这三个**无效**：实测 CapEff 已不含 CAP_SYS_ADMIN，
	// 而 kptr_restrict / drop_caches（走 proc_dointvec_* ，内部会 capable()）
	// 被正确拒绝，sysrq-trigger / core_pattern / modprobe（走 proc_dostring，
	// 只依赖 inode 权限）却能写成功。seccomp 也帮不上——这是 open+write，
	// 不是专用系统调用。
	//
	// 因此唯一可靠的封堵点是**挂载层**：把 /proc/sys 整体 remount 成只读。
	// 这样连 open(O_WRONLY) 都在 VFS 层被拒，与内核各文件是否有 capable()
	// 检查无关，不依赖逐文件清单（清单会随内核版本漂移）。
	//
	// 与 InstallExecHelper / 卷挂载同一姿势：先 MS_BIND，再
	// MS_REMOUNT|MS_BIND|MS_RDONLY。单次 mount(MS_BIND|MS_RDONLY) 的
	// MS_RDONLY 会被内核忽略，bind 仍是可写（v0.6.0 的教训）。
	if err := mountProcSysReadOnly(rootfs); err != nil {
		return err
	}
	// 4.6 mask /proc 根下的危险文件（sysrq-trigger **不在** /proc/sys 内，
	//     上一步覆盖不到它）。
	//
	//     实测发现（2026-10-05）：/proc/sys 挂只读后，core_pattern 与
	//     modprobe 已被拒绝，但 /proc/sysrq-trigger 仍能写入（rc=0）——
	//     因为它的路径在 /proc 根下。写 'b' 会立即重启宿主。
	if err := maskProcRootFiles(rootfs); err != nil {
		return err
	}
	// 5. pivot_root(".", oldRoot)。注意：不做 chroot——pivot_root 的内核约束是
	//    new_root 必须位于当前 root 之下且不等于当前 root，chroot 反而使其失败。
	if err := os.Chdir(rootfs); err != nil {
		return fmt.Errorf("chdir %s: %w", rootfs, err)
	}
	if err := syscall.PivotRoot(".", oldRoot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}
	// 6. 摘除旧根，宿主文件系统从容器视野消失。
	if err := syscall.Unmount("/"+oldRoot, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("卸载旧根: %w", err)
	}
	_ = syscall.Rmdir("/" + oldRoot)

	// 6.5 拉起容器回环。bridge 与 none 都会新建 netns，内核默认把其中的 lo
	//     置为 DOWN，于是容器内一切发往 127.0.0.1 的连接都只会超时。
	//     host 模式共享宿主 netns，跳过。（降级到共享宿主 netns 时本调用
	//     依然安全：宿主 lo 本就 UP，重复置位是幂等操作。）
	if mode, _, _, _, _, _, _, ok := parseNetEnv(os.Environ()); ok && *mode != network.ModeHost {
		if err := network.BringUpLoopback(); err != nil {
			return fmt.Errorf("容器网络初始化失败: %w", err)
		}
	}

	// 7. 容器侧网络装配：赋值 IP/路由，把 DNS 指向网桥网关。此刻已在
	//    容器 netns 与 rootfs 内；非 bridge 模式（host/none）跳过。
	if mode, cid, name, ip, gw, hostname, prefix, ok := parseNetEnv(os.Environ()); ok && *mode == network.ModeBridge {
		if err := network.ConfigurePeer(name, cid, ip, gw, prefix, hostname); err != nil {
			return fmt.Errorf("配置容器网络失败: %w", err)
		}
	}

	// 7.5 SELinux：execve 之前继承引擎的 exec 上下文。attr/exec 只对本进程
	//     的下一次 execve 生效，因此必须紧邻 execve 写入。失败只降级告警。
	//
	// 8. execve 用户命令（此后不再返回）。
	if err := executeContainerCmd(cmdline, env); err != nil {
		return err
	}
	return nil // 不可达
}

// instanceID 返回本次容器实例 ID（父进程注入）。缺失时退化为固定名，
// 单容器场景仍正确，仅并发共享同一 rootfs 时才有旧根重名风险。
func instanceID() string {
	if id := os.Getenv(envCID); id != "" {
		return id
	}
	return "0"
}

// mountRootfsVolumes 把 -v 解析出的挂载列表 bind 进 rootfs 目标路径。
// 在 pivot_root 之前调用：rootfs 已 bind 为挂载点，挂载随新根进入容器。
// 目标路径必须是容器内绝对路径，且经 Clean 后不得逃逸出 rootfs。
func mountRootfsVolumes(rootfs string) error {
	mounts, ok := parseMountEnv(os.Environ())
	if !ok {
		return nil
	}
	for _, m := range mounts {
		if err := safeContainerTarget(m.Target); err != nil {
			return err
		}
		dst := filepath.Join(rootfs, filepath.Clean(m.Target)[1:])
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("创建挂载点 %s: %w", m.Target, err)
		}
		flags := uintptr(msBind)
		if m.ReadOnly {
			flags |= syscall.MS_RDONLY
		}
		if err := syscall.Mount(m.Source, dst, "", flags, ""); err != nil {
			return fmt.Errorf("挂载卷 %s → %s: %w", m.Source, m.Target, err)
		}
		// bind 之后必须先 bind 再 remount 才能应用只读：单个
		// mount(MS_BIND|MS_RDONLY) 的 MS_RDONLY 会被内核忽略，bind 仍是可写。
		if m.ReadOnly {
			if err := syscall.Mount(dst, dst, "", uintptr(msBind|syscall.MS_REMOUNT|syscall.MS_RDONLY), ""); err != nil {
				return fmt.Errorf("卷 %s 设为只读失败: %w", m.Target, err)
			}
		}
	}
	return nil
}

// safeContainerTarget 校验容器内挂载目标是绝对路径且不含目录穿越。
func safeContainerTarget(target string) error {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("卷目标 %q 不是绝对路径: %w", target, ErrBadConfig)
	}
	clean := filepath.Clean(target)
	if clean != target || strings.Contains(target, "..") {
		return fmt.Errorf("卷目标 %q 非法（含 .. 或非规范路径）: %w", target, ErrBadConfig)
	}
	return nil
}

// childCmdline 从 LICORE_ARGC / LICORE_ARG0..N 读取用户命令。
func childCmdline() ([]string, error) {
	n, err := strconv.Atoi(os.Getenv(envChildCmdCountKey))
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("%s=%q 非法: %w", envChildCmdCountKey, os.Getenv(envChildCmdCountKey), ErrBadConfig)
	}
	argv := make([]string, 0, n)
	for i := range n {
		v, ok := os.LookupEnv(envChildCmdPrefix + strconv.Itoa(i))
		if !ok {
			return nil, fmt.Errorf("缺少 %s%d: %w", envChildCmdPrefix, i, ErrBadConfig)
		}
		argv = append(argv, v)
	}
	return argv, nil
}

// envWithoutLiCore 返回剥离 LICORE_* 内部变量后的容器环境。
func envWithoutLiCore() []string {
	return envWithoutLiCoreFrom(os.Environ())
}

// envWithoutLiCoreFrom 是 envWithoutLiCore 的纯函数实现，便于单元测试验证
// 内部变量不会泄漏进容器（LICORE_NET_* / LICORE_MOUNT_* / LICORE_CGROUP_ID
// 等是引擎与 init 之间的私有信道，不应出现在容器进程的环境里）。
func envWithoutLiCoreFrom(environ []string) []string {
	env := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "LICORE_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}
