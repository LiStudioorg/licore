// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/licore/internal/build"
	"github.com/LiStudioorg/licore/internal/compose"
	"github.com/LiStudioorg/licore/internal/engine"
	"github.com/LiStudioorg/licore/internal/store"
)

// composeCmd 持有 compose 父命令的共享配置。
type composeCmd struct {
	out, errOut io.Writer
	file        string
	dataDir     string
}

// newComposeCommand 实现 `licore compose`。
func newComposeCommand(out io.Writer) *cobra.Command {
	cc := &composeCmd{out: out}
	cmd := &cobra.Command{
		Use:   "compose",
		Short: "LiCore compose 服务编排",
		Long:  "解析自研 compose 文件（默认 ./licore-compose.yaml）并对服务做 up/down/ps/logs/scale 编排。",
	}
	cmd.PersistentFlags().StringVarP(&cc.file, "file", "f", "licore-compose.yaml", "compose 文件路径")
	cmd.PersistentFlags().StringVarP(&cc.dataDir, "data-dir", "", "", "数据目录")
	cmd.AddCommand(cc.config(out))
	cmd.AddCommand(cc.up(out))
	cmd.AddCommand(cc.down(out))
	cmd.AddCommand(cc.ps(out))
	cmd.AddCommand(cc.logs(out))
	cmd.AddCommand(cc.scale(out))
	return cmd
}

// projectPrefix 是容器名命名规范 "<project>_<service>" 的前缀。
func (cc *composeCmd) projectPrefix(p *compose.Project) string { return p.Name + "_" }

// load 载入并解析项目。
func (cc *composeCmd) load() (*compose.Project, []*compose.ResolvedService, *store.Store, error) {
	p, err := compose.LoadFile(cc.file)
	if err != nil {
		return nil, nil, nil, err
	}
	services, err := p.Resolve()
	if err != nil {
		return nil, nil, nil, err
	}
	st, err := store.Open(cc.dataDir)
	if err != nil {
		return nil, nil, nil, err
	}
	return p, services, st, nil
}

func (cc *composeCmd) config(out io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "校验并输出解析后的项目",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, services, _, err := cc.load()
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "# project %s (%s)\n", p.Name, orDashC(p.Version))
			for _, s := range services {
				fmt.Fprintf(out, "%s\timage=%s\tsource=%s\treplicas=%d\n",
					s.Name, orDashC(s.Image), orDashC(srcOf(s)), s.Replicas)
			}
			return nil
		},
	}
}

func (cc *composeCmd) up(out io.Writer) *cobra.Command {
	var detach bool
	c := &cobra.Command{
		Use:   "up [SERVICE...]",
		Short: "创建并启动服务（缺省全部）",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, services, st, err := cc.load()
			if err != nil {
				return err
			}
			_ = st
			want := map[string]bool{}
			for _, a := range args {
				want[a] = true
			}
			// 先保证镜像可用（build/boxfile 服务就地构建并导入）。
			for _, s := range services {
				if len(want) > 0 && !want[s.Name] {
					continue
				}
				if s.Image == "" {
					if err := cc.buildService(out, cmd, st, p, s); err != nil {
						return err
					}
				}
			}
			// 依服务声明顺序启动（depends_on 的依赖先由用户保证已 up）。
			//
			// 副本数取 `replicas`（缺省 1）——此前恒启一个，声明的 replicas 被
			// 完全忽略。副本序号交给 serviceContainerName 生成名字，
			// 与 scale / stopService 使用同一套规则。
			for _, s := range services {
				if len(want) > 0 && !want[s.Name] {
					continue
				}
				reps := s.Replicas
				if reps <= 0 {
					reps = 1
				}
				for i := range reps {
					if err := cc.startService(cmd, st, p, s, i, detach); err != nil {
						return fmt.Errorf("compose up 服务 %s: %w", s.Name, err)
					}
				}
				if reps > 1 {
					fmt.Fprintf(out, "服务 %s 已启动（%d 个副本）\n", s.Name, reps)
				} else {
					fmt.Fprintf(out, "服务 %s 已启动\n", s.Name)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&detach, "detach", "d", false, "后台运行（缺省前台持有）")
	return c
}

func (cc *composeCmd) down(out io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "停止并移除项目的容器",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, _, st, err := cc.load()
			if err != nil {
				return err
			}
			return cc.forEachProjectContainer(st, p, func(c *store.ContainerConfig) error {
				_, err := engine.Stop(st, c.ID, time.Second*15)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "已停止 %s\n", c.Name)
				return nil
			})
		},
	}
}

func (cc *composeCmd) ps(out io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "ps",
		Short: "列出该项目下运行的容器",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, _, st, err := cc.load()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tIMAGE")
			return cc.forEachProjectContainer(st, p, func(c *store.ContainerConfig) error {
				fmt.Fprintf(tw, "%s\t%s\n", c.Name, c.ImageRef)
				return nil
			})
		},
	}
}

func (cc *composeCmd) logs(out io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "logs SERVICE",
		Short: "显示服务容器的日志",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, _, st, err := cc.load()
			if err != nil {
				return err
			}
			prefix := cc.projectPrefix(p) + args[0]
			containers, err := st.ListContainers()
			if err != nil {
				return err
			}
			for _, c := range containers {
				if !strings.HasPrefix(c.Name, prefix) {
					continue
				}
				logPath := filepath.Join(st.ContainerDir(c.ID), "container.log")
				data, rerr := os.ReadFile(logPath)
				if rerr != nil {
					continue
				}
				fmt.Fprintln(out, string(data))
			}
			return nil
		},
	}
}

// forEachProjectContainer 对匹配项目前缀的容器执行 fn。

// scale 设置服务副本数：目标多于当前则增启，少于当前则停止多余的。
func (cc *composeCmd) scale(out io.Writer) *cobra.Command {
	c := &cobra.Command{
		Use:   "scale SERVICE=N [SERVICE=N...]",
		Short: "设置服务副本数",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, services, st, err := cc.load()
			if err != nil {
				return err
			}
			byName := map[string]*compose.ResolvedService{}
			for _, s := range services {
				byName[s.Name] = s
			}
			for _, a := range args {
				svcName, nStr, ok := strings.Cut(a, "=")
				if !ok {
					return fmt.Errorf("compose scale: %q 应为 SERVICE=N", a)
				}
				n, err := strconv.Atoi(nStr)
				if err != nil || n < 0 {
					return fmt.Errorf("compose scale: %q 副本数非法", a)
				}
				svc, ok := byName[svcName]
				if !ok {
					return fmt.Errorf("compose scale: 未知服务 %q", svcName)
				}
				cur := cc.countService(st, p, svcName)
				switch {
				case n > cur:
					diff := n - cur
					for i := 0; i < diff; i++ {
						// 新副本的序号是 cur+i：首副本无序号，其后 _1/_2…
						// 序号传错会让所有副本撞同名，scale 直接失败。
						if err := cc.startService(cmd, st, p, svc, cur+i, true); err != nil {
							return fmt.Errorf("scale %s 增启: %w", svcName, err)
						}
						fmt.Fprintf(out, "服务 %s 副本 %d → %d\n", svcName, cur+i, cur+i+1)
					}
				case n < cur:
					for i := n; i < cur; i++ {
						if err := cc.stopService(st, p, svcName, i); err != nil {
							return fmt.Errorf("scale %s 缩容: %w", svcName, err)
						}
					}
					fmt.Fprintf(out, "服务 %s 副本缩至 %d\n", svcName, n)
				default:
					fmt.Fprintf(out, "服务 %s 副本数已为 %d\n", svcName, n)
				}
			}
			return nil
		},
	}
	return c
}

// buildService 就地构建一个 source 为 boxfile/build 的服务并导入本地 store，
// 把解析出的镜像引用写回 s.Image 供 startService 使用。
func (cc *composeCmd) buildService(out io.Writer, cmd *cobra.Command, st *store.Store, p *compose.Project, s *compose.ResolvedService) error {
	contextDir := s.Build
	boxfile := s.Boxfile
	if boxfile != "" {
		contextDir = filepath.Dir(boxfile)
	} else if contextDir != "" {
		if bf := filepath.Join(contextDir, "Boxfile"); fileExists(bf) {
			boxfile = bf
		} else if bf := filepath.Join(contextDir, "boxfile"); fileExists(bf) {
			boxfile = bf
		} else {
			return fmt.Errorf("compose up 服务 %s：构建上下文 %s 无 Boxfile", s.Name, contextDir)
		}
	}
	if boxfile == "" {
		return fmt.Errorf("compose up 服务 %s：未声明 image/boxfile/build", s.Name)
	}
	bf, err := build.ParseBoxfileFile(boxfile)
	if err != nil {
		return err
	}
	if err := build.CheckContext(contextDir); err != nil {
		return fmt.Errorf("compose up 服务 %s：构建上下文: %w", s.Name, err)
	}
	basePath := ""
	if bf.From != "" && !strings.EqualFold(bf.From, "scratch") {
		basePath, err = resolveBuildBase(st, bf.From)
		if err != nil {
			return err
		}
	}
	tag := composeServiceImageRef(p, s.Name)
	outTmp, err := os.CreateTemp("", "licore-compose-build-*.licore")
	if err != nil {
		return err
	}
	outPath := outTmp.Name()
	_ = outTmp.Close()
	defer os.Remove(outPath)
	name, version := splitBuildTag(tag)
	res, err := build.Build(cmd.Context(), &build.Options{
		Boxfile: bf, ContextDir: contextDir, BaseImage: basePath,
		OutPath: outPath, Name: name, Version: version,
	})
	if err != nil {
		return err
	}
	if _, err := ImportImage(st, res.Path, tag, ImportOptions{Force: true}); err != nil {
		return fmt.Errorf("compose up 导入服务 %s 产物: %w", s.Name, err)
	}
	s.Image = tag
	fmt.Fprintf(out, "已构建服务 %s → %s\n", s.Name, tag)
	return nil
}

// serviceContainerName 返回服务第 idx 个副本的容器名。
// 命名规则（与 stopService / countService 共用，必须一致）：首副本无序号
// （<project>_<service>），额外副本带序号（<project>_<service>_<idx>）。
func (cc *composeCmd) serviceContainerName(p *compose.Project, service string, idx int) string {
	base := cc.projectPrefix(p) + service
	if idx == 0 {
		return base
	}
	return fmt.Sprintf("%s_%d", base, idx)
}

// startService 用 engine.Run 创建并启动服务容器（容器名见 serviceContainerName）。
//
// idx 是副本序号：扩缩容时必须传入目标序号，否则每个副本都会落到同一个
// `<project>_<service>` 上，第二次创建就因 "容器名已被占用" 失败——
// `compose scale web=3` 于是永远只能有一个副本（真机实测）。
func (cc *composeCmd) startService(cmd *cobra.Command, st *store.Store, p *compose.Project, s *compose.ResolvedService, idx int, detach bool) error {
	imageRef := s.Image
	if imageRef == "" {
		return fmt.Errorf("服务 %s 无可用镜像（先 build）", s.Name)
	}
	n, v := splitComposeRef(imageRef)
	if ok, _ := st.Exists(n, v); !ok {
		return fmt.Errorf("镜像 %s 未导入，请先 licore pull", imageRef)
	}
	env := make([]string, 0, len(s.Environment))
	for _, k := range sortedKeysC(s.Environment) {
		env = append(env, k+"="+s.Environment[k])
	}
	spec := &engine.RunSpec{
		ImageRef:   imageRef,
		Name:       cc.serviceContainerName(p, s.Name, idx),
		Hostname:   s.Hostname,
		Cmd:        s.Command,
		Entrypoint: s.Entrypoint,
		Env:        env,
		Workdir:    s.WorkingDir,
		User:       s.User,
		Restart:    store.Restart(s.Restart),
		Detach:     detach,
		Ports:      s.Ports,
		Volumes:    s.Volumes,
	}
	_, err := engine.Run(cmd.Context(), st, spec)
	return err
}

// stopService 停止第 i 个副本（名字 <project>_<service>_<i>）。副本命名规则：
// 首副本无序号，扩缩容的额外副本带序号。
func (cc *composeCmd) stopService(st *store.Store, p *compose.Project, service string, idx int) error {
	base := cc.projectPrefix(p) + service
	names := []string{base}
	if idx > 0 {
		names = []string{fmt.Sprintf("%s_%d", base, idx)}
	}
	for _, name := range names {
		c, err := st.FindContainer(name)
		if err != nil {
			continue
		}
		if _, err := engine.Stop(st, c.ID, 15*time.Second); err != nil {
			return err
		}
		if _, err := engine.Remove(st, c.ID, true); err != nil {
			return err
		}
		break
	}
	return nil
}

// countService 统计某服务当前运行的容器副本数。
// countService 数出服务当前有多少个副本容器。
//
// 必须只认"首副本无序号、其余 _<数字>"这一种名字（与 serviceContainerName /
// stopService 同一规则）。用 `prefix+"_"` 做前缀匹配会把**兄弟服务算进来**：
// 服务 web 的前缀是 "<proj>_web_"，而服务 web_2 的容器叫 "<proj>_web_2"，
// 于是 web 的副本数会被 web_2 的容器虚增，scale 的增/减方向随之判错。
func (cc *composeCmd) countService(st *store.Store, p *compose.Project, service string) int {
	base := cc.projectPrefix(p) + service
	containers, _ := st.ListContainers()
	count := 0
	for _, c := range containers {
		if c.Name == base {
			count++
			continue
		}
		// 只接受 base_<数字>：下标必须整体是数字，"_web_2x" 这类不算同服务副本。
		if rest, ok := strings.CutPrefix(c.Name, base+"_"); ok {
			if _, err := strconv.Atoi(rest); err == nil {
				count++
			}
		}
	}
	return count
}

// forEachProjectContainer 对匹配项目前缀的容器执行 fn。
func (cc *composeCmd) forEachProjectContainer(st *store.Store, p *compose.Project, fn func(*store.ContainerConfig) error) error {
	prefix := cc.projectPrefix(p)
	containers, err := st.ListContainers()
	if err != nil {
		return err
	}
	for _, c := range containers {
		if strings.HasPrefix(c.Name, prefix) {
			if err := fn(c); err != nil {
				return err
			}
		}
	}
	return nil
}

func srcOf(s *compose.ResolvedService) string {
	switch {
	case s.Boxfile != "":
		return "boxfile:" + s.Boxfile
	case s.Build != "":
		return "build:" + s.Build
	default:
		return "unknown"
	}
}

func splitComposeRef(ref string) (name, version string) {
	i := strings.LastIndex(ref, ":")
	if i <= 0 || i == len(ref)-1 {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

// orDashC 是 compose 命令的 orDash 辅助。
func orDashC(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// composeServiceImageRef 为就地构建的服务派生镜像引用 <project>/<service>:latest
// （镜像名不含下划线/空格等非法字符，service 名已由 compose 校验）。
func composeServiceImageRef(p *compose.Project, service string) string {
	return p.Name + "/" + service + ":latest"
}

// fileExists 报告路径存在且为常规文件。
func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// sortedKeysC 按字典序返回 map 的键切片。
func sortedKeysC(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
