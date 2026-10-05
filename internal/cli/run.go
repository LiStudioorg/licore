// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/licore/internal/engine"
	"github.com/LiStudioorg/licore/internal/network"
	"github.com/LiStudioorg/licore/internal/resource"
	"github.com/LiStudioorg/licore/internal/runtime"
	"github.com/LiStudioorg/licore/internal/store"
)

// newRunCommand 实现 `licore run`：镜像查找 → 层解包合并 rootfs → 写容器
// 状态 → 前台持有或后台 fork shim。编排逻辑在 internal/engine。
func newRunCommand(out io.Writer) *cobra.Command {
	var opts struct {
		detach       bool
		restart      string
		name         string
		hostname     string
		memoryMB     int
		memorySwapMB int
		memoryResMB  int
		cpus         float64
		pidsLimit    int
		cpuset       string
		blkioWeight  int
		storageMB    int
		networkBw    string
		gpu          int
		npu          int
		env          []string
		workdir      string
		user         string
		entrypoint   []string
		ports        []string
		volumes      []string
		network      string
		ip           string
		dataDir      string
		capAdd       []string
		capDrop      []string
	}
	cmd := &cobra.Command{
		Use:   "run [flags] <image> [command...]",
		Short: "创建并启动容器",
		Long: "创建并启动容器：镜像查找 → 层解包合并 rootfs → 写容器状态 → 启动。\n\n" +
			"默认前台运行，stdio 直连容器，Ctrl+C 停止容器；-d 后台运行并打印容器 ID。\n\n" +
			"注意：run 自身的 flag 必须写在镜像引用之前，镜像之后的内容一律作为容器命令\n" +
			"原样传入（与 Docker 一致）。例如 `licore run -e FOO=bar img sh -c 'echo $FOO'`。",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			restart := store.Restart(opts.restart)
			if !restart.Valid() {
				return fmt.Errorf("run: 非法 --restart 值 %q（可选：no|always|unless-stopped|on-failure）", opts.restart)
			}
			// 提前校验端口映射（约占位；引擎在启动前据此实化 NAT）。
			if _, err := parsePorts(opts.ports); err != nil {
				return err
			}
			if _, err := parseMounts(opts.volumes); err != nil {
				return err
			}
			// 提前校验能力名：拼错时立刻报错，而不是等容器启动到 exec 前才失败。
			if err := validateCapSpecs(opts.capDrop, opts.capAdd); err != nil {
				return err
			}
			lims, err := runLimits(opts.memoryMB, opts.memorySwapMB, opts.memoryResMB, opts.cpus, opts.pidsLimit, opts.cpuset, opts.blkioWeight, opts.storageMB, opts.networkBw, opts.gpu, opts.npu)
			if err != nil {
				return err
			}
			// 未设内存上限时明确告警（实现见 warnIfMemoryUnlimited）。
			warnIfMemoryUnlimited(opts.memoryMB, args[0])

			st, err := store.Open(opts.dataDir)
			if err != nil {
				return err
			}
			spec := &engine.RunSpec{
				ImageRef:   args[0],
				Cmd:        args[1:],
				Env:        opts.env,
				Name:       opts.name,
				Hostname:   opts.hostname,
				Workdir:    opts.workdir,
				User:       opts.user,
				Entrypoint: opts.entrypoint,
				Restart:    restart,
				Detach:     opts.detach,
				Ports:      opts.ports,
				Volumes:    opts.volumes,
				Network:    opts.network,
				IP:         opts.ip,
				MemoryMB:   opts.memoryMB,
				CPUs:       opts.cpus,
				PidsLimit:  opts.pidsLimit,
				Limits:     lims,
				CapDrop:    opts.capDrop,
				CapAdd:     opts.capAdd,
			}

			// 前台模式：Ctrl+C（SIGINT/SIGTERM）→ ctx 取消 → engine 转发容器。
			ctx := context.Background()
			if !opts.detach {
				stop := make(chan os.Signal, 1)
				signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
				defer signal.Stop(stop)
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				go func() {
					<-stop
					cancel()
				}()
			}

			res, err := engine.Run(ctx, st, spec)
			if res != nil && res.Container != nil {
				// 无论成败先公布容器 ID，便于 rm/logs 排查。
				if opts.detach {
					fmt.Fprintf(out, "%s\n", res.ShortID)
				}
			}
			if err != nil {
				return err
			}
			// 网络接入、卷挂载与资源限制均在 engine.Run 内部（启动前）完成：
			//   - 网络：分配 IP、登记端口并实化 NAT，veth 进容器 netns；
			//   - 卷：解析 -v，匿名/命名卷补齐源路径，运行时 bind 进容器 mount ns；
			//   - 资源：建 cgroup 写限制，运行时把 init PID 写入 cgroup.procs。
			if opts.detach {
				fmt.Fprintf(out, "容器 %s（%s）已在后台运行，shim 持有生命周期\n",
					res.Container.Name, res.Container.ID)
				return nil
			}
			if res.ExitCode != 0 {
				return &exitCodeError{code: res.ExitCode}
			}
			return nil
		},
	}
	f := cmd.Flags()
	// 容器命令里的 -c/-e 等必须原样透传（`licore run img sh -c 'exit 1'`），
	// 故第一个位置参数（镜像引用）之后不再解析 flag。
	//
	// 代价与 Docker 一致：所有 run 自己的 flag 必须写在镜像引用之前，
	// 写在之后的会被当作容器命令参数。help 里明确写出这一点。
	f.SetInterspersed(false)
	f.BoolVarP(&opts.detach, "detach", "d", false, "后台运行，打印容器 ID")
	f.StringVar(&opts.restart, "restart", "no", "重启策略：no|always|unless-stopped|on-failure")
	f.StringVar(&opts.name, "name", "", "容器名（默认自动生成）")
	f.StringVar(&opts.hostname, "hostname", "", "容器主机名（默认取容器名）")
	f.IntVar(&opts.memoryMB, "memory", 0, "内存上限（MiB，0=不限制）")
	f.IntVar(&opts.memorySwapMB, "memory-swap", 0, "内存+swap 总上限（MiB，-1=不限制 swap）")
	f.IntVar(&opts.memoryResMB, "memory-reservation", 0, "内存软限制（MiB，0=不限制）")
	f.Float64Var(&opts.cpus, "cpus", 0, "CPU 配额（核数，0=不限制）")
	f.StringVar(&opts.cpuset, "cpuset-cpus", "", "允许使用的 CPU 列表（如 0-3,7）")
	f.IntVar(&opts.pidsLimit, "pids-limit", 0, "进程数上限（0=不限制）")
	f.IntVar(&opts.blkioWeight, "blkio-weight", 0, "块设备相对权重 [10,1000]，0=不设置")
	f.IntVar(&opts.storageMB, "storage", 0, "可写层存储配额（MiB，0=不限制）")
	f.StringVar(&opts.networkBw, "network-bandwidth", "", "出向带宽上限（如 10mbps）")
	f.IntVar(&opts.gpu, "gpu", 0, "直通 GPU 设备数（0=不直通）")
	f.IntVar(&opts.npu, "npu", 0, "直通 NPU 设备数（0=不直通）")
	f.StringArrayVarP(&opts.env, "env", "e", nil, "环境变量 KEY=VALUE，可重复")
	f.StringVar(&opts.workdir, "workdir", "", "工作目录")
	f.StringVar(&opts.user, "user", "", "运行用户 uid:gid（阶段 3 生效）")
	f.StringSliceVar(&opts.entrypoint, "entrypoint", nil, "覆盖镜像 entrypoint")
	f.StringSliceVarP(&opts.ports, "publish", "p", nil, "端口映射 HOST:CONTAINER[:PROTO]")
	f.StringSliceVarP(&opts.volumes, "volume", "v", nil, "卷挂载 SRC:TARGET[:ro]；SRC 可为宿主路径或命名卷，省略=匿名卷")
	f.StringVar(&opts.network, "network", "licore0", "接入网络：licore0(bridge)|host|none|自定义")
	f.StringVar(&opts.ip, "ip", "", "指定容器 IP（默认自动分配）")
	f.StringVar(&opts.dataDir, "data-dir", "", "数据目录（默认 $LICORE_HOME 或 ~/.licore）")
	f.StringSliceVar(&opts.capDrop, "cap-drop", nil,
		"从默认能力集移除能力（可重复）；--cap-drop ALL 清空，只留 --cap-add 显式指定的")
	f.StringSliceVar(&opts.capAdd, "cap-add", nil,
		"在默认能力集之上追加能力（可重复），如 --cap-add SYS_ADMIN")
	return cmd
}

// validateCapSpecs 在启动前校验 --cap-add / --cap-drop 的名字。
//
// 提前失败的理由：能力名写错如果拖到容器 init 的 exec 前才报错，用户看到的
// 是一个已经建好 rootfs、联网、占了名字的容器却启动失败，排查成本高得多。
func validateCapSpecs(drop, add []string) error {
	for _, spec := range [][]string{drop, add} {
		for _, s := range spec {
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("run: --cap-drop/--cap-add 里有空的能力名")
			}
			if _, err := runtime.ParseCapability(s); err != nil {
				// ALL 是集合关键字，parse 会拒绝；这里放行。
				if strings.EqualFold(strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "CAP_"), "ALL") {
					continue
				}
				return fmt.Errorf("run: %w", err)
			}
		}
	}
	return nil
}

// parsePorts 把 -p 的字符串解析为 PortMapping 列表。
// 支持形式："8080:80"、"8080:80/udp"、"80"（省略宿主端口=随机）、
// "127.0.0.1:8080:80"（宿主 IP+端口）。
func parsePorts(ports []string) ([]*network.PortMapping, error) {
	var out []*network.PortMapping
	for _, raw := range ports {
		p, err := parsePort(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// VolumeMount 是一次 -v 的解析结果。
type VolumeMount struct {
	// Source 是宿主源（绝对路径）或命名卷名；空表示匿名卷。
	Source string
	// Target 是容器内挂载点。
	Target string
	// ReadOnly 是否只读。
	ReadOnly bool
	// Anonymous 是否匿名卷（Source 为空）。
	Anonymous bool
}

// parseMounts 解析 -v 参数列表。
// 支持 "TARGET"（匿名卷）、"SRC:TARGET"（bind/命名卷）、"SRC:TARGET:ro"。
func parseMounts(vols []string) ([]*VolumeMount, error) {
	var out []*VolumeMount
	for _, raw := range vols {
		parts := splitVol(raw)
		m := &VolumeMount{}
		switch len(parts) {
		case 1: // 匿名卷
			m.Target = parts[0]
			m.Anonymous = true
		case 2: // SRC:TARGET
			m.Source = parts[0]
			m.Target = parts[1]
		case 3: // SRC:TARGET:ro
			m.Source = parts[0]
			m.Target = parts[1]
			if parts[2] != "ro" {
				return nil, fmt.Errorf("run: 非法 -v 选项 %q（仅支持 ro）", parts[2])
			}
			m.ReadOnly = true
		default:
			return nil, fmt.Errorf("run: 非法 -v %q", raw)
		}
		if m.Target == "" {
			return nil, fmt.Errorf("run: -v %q 缺少容器路径", raw)
		}
		out = append(out, m)
	}
	return out, nil
}

func parsePort(raw string) (*network.PortMapping, error) {
	proto := network.ProtoTCP
	if i := indexByte(raw, '/'); i >= 0 {
		switch raw[i+1:] {
		case "tcp":
			proto = network.ProtoTCP
		case "udp":
			proto = network.ProtoUDP
		default:
			return nil, fmt.Errorf("run: 非法协议 %q", raw[i+1:])
		}
		raw = raw[:i]
	}
	// 拆分 IP:host:container。
	parts := splitColon(raw)
	p := &network.PortMapping{Proto: proto}
	switch len(parts) {
	case 1: // 只有容器端口
		p.ContainerPort = atoi(parts[0])
		p.HostPort = 0 // 随机
	case 2: // host:container
		p.HostPort = atoi(parts[0])
		p.ContainerPort = atoi(parts[1])
	case 3: // ip:host:container
		p.HostIP = parts[0]
		p.HostPort = atoi(parts[1])
		p.ContainerPort = atoi(parts[2])
	default:
		return nil, fmt.Errorf("run: 非法端口映射 %q", raw)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	return p, nil
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func splitColon(s string) []string {
	var out []string
	var cur []byte
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			out = append(out, string(cur))
			cur = cur[:0]
		} else {
			cur = append(cur, s[i])
		}
	}
	out = append(out, string(cur))
	return out
}

func splitVol(s string) []string {
	return splitColonV(s)
}

func splitColonV(s string) []string {
	var out []string
	var cur []byte
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			out = append(out, string(cur))
			cur = cur[:0]
		} else {
			cur = append(cur, s[i])
		}
	}
	out = append(out, string(cur))
	return out
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// runLimits 把 run 的 CLI 资源参数翻译成 resource.Limits。
// warnIfMemoryUnlimited 在容器未设置内存上限时打出结构化告警。
//
// 为什么必须告警（而不是静默沿用默认值）：没有 cgroup 内存限额的容器可以把
// 内存吃到触发**宿主 OOM killer**，从而杀死宿主上的其它进程——这是真实的
// "容器影响宿主机" 路径，不是理论风险。
//
// 为什么**不**直接改默认限额：默认不限与 Docker 语义一致，擅自加默认上限会
// 破坏合法的大内存负载（数据库、构建、机器学习）。因此选择"保持语义 + 明确
// 告警"，把决定权交回用户（决策记录见 docs/escape-audit.md 的 I7）。
//
// memoryMB <= 0 一律视为未限制：0 是 flag 约定的"不限制"，负值同属未设置，
// 不能因为没走 ==0 分支就静默放过。
func warnIfMemoryUnlimited(memoryMB int, image string) {
	if memoryMB > 0 {
		return
	}
	slog.Warn("未设置内存上限，容器可耗尽宿主内存并触发宿主 OOM killer；"+
		"生产环境请显式指定 --memory",
		slog.String("image", image),
		slog.String("suggestion", "--memory 256m"))
}

// runLimits 把 CLI 的限额参数转成 resource.Limits。
//
// 未实现的资源能力（存储配额、GPU/NPU 直通、网络带宽）在此显式拒绝，
// 不允许"参数接受但运行时假装生效"。
func runLimits(memoryMB, memorySwapMB, memoryResMB int, cpus float64, pidsLimit int,
	cpuset string, blkioWeight, storageMB int, networkBw string, gpu, npu int,
) (*resource.Limits, error) {
	if storageMB > 0 {
		return nil, fmt.Errorf("run: --storage 存储配额尚未实现（需 XFS project quota）")
	}
	if networkBw != "" {
		return nil, fmt.Errorf("run: --network-bandwidth 尚未实现（需 tc 流量整形）")
	}
	if gpu > 0 {
		return nil, fmt.Errorf("run: --gpu 设备直通尚未实现（需 cgroup eBPF 设备控制）")
	}
	if npu > 0 {
		return nil, fmt.Errorf("run: --npu 设备直通尚未实现（需 cgroup eBPF 设备控制）")
	}
	l := &resource.Limits{}
	mb := func(v int) int64 { return int64(v) * 1024 * 1024 }
	if memoryMB > 0 {
		l.Memory = mb(memoryMB)
	}
	if memoryResMB > 0 {
		l.MemoryReservation = mb(memoryResMB)
	}
	l.MemorySwap = mb(memorySwapMB)
	if memorySwapMB == -1 {
		l.MemorySwap = -1
	}
	if cpus > 0 {
		l.CPUs = cpus
	}
	l.CPUSet = cpuset
	if pidsLimit > 0 {
		l.PidsLimit = int64(pidsLimit)
	}
	if blkioWeight > 0 {
		l.BlkioWeight = int64(blkioWeight)
	}
	return l, nil
}
