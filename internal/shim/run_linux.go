// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package shim

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/LiStudioorg/licore/internal/runtime"
	"github.com/LiStudioorg/licore/internal/store"
)

// Options 是一次 shim 生命周期的装配参数。
type Options struct {
	// Store 与容器配置。shim 对状态目录只写 runtime.json 与清理停止标记。
	Store *store.Store
	Cfg   *store.ContainerConfig
	// Stdin/Stdout/Stderr 透传给容器 init；nil 时取 os.Stdin/os.Stdout。
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
	// OnStart 每次容器 init 启动后回调（参数为宿主视角 PID），可为 nil。
	OnStart func(pid int)
}

// startWithFn 是 runtime.StartWith 的注入点（测试用 fake 替换，避免真跑容器）。
var startWithFn = runtime.StartWith

// Run 是 shim 主循环：启动容器 → 写状态 → 按 restart 策略决定重启或退出。
//
// 停止语义（两条路径，与 doc.go 一致）：
//   - 用户 stop：`licore stop` 先写 stopped-by-user 标记再向容器 init 发信号；
//     init 退出后 shim 观察到标记，退出循环并保留标记（always 容器由下次
//     `licore start`/`boot` 清除标记重新拉起）。
//   - 直接 SIGTERM shim（systemd ExecStop 等）：shim 经 StartOptions.StopCh
//     让 runtime 向 init 转发 SIGTERM（宽限后 SIGKILL），不重启。
//
// 正常结束恒返回 nil；容器退出码只写在 runtime.json。
func Run(ctx context.Context, o *Options) error {
	cfg, st := o.Cfg, o.Store

	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// shim 拉起即代表用户意图运行：清停止标记，unless-stopped 下次 boot 才合法拉起。
	if err := st.ClearStoppedByUser(cfg.ID); err != nil {
		return err
	}

	out, errOut := o.Stdout, o.Stderr
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = out
	}
	in := o.Stdin
	if in == nil {
		in = os.Stdin
	}

	state := store.RuntimeState{ExitCode: -1, ShimPID: os.Getpid(), Status: store.StatusStarting}
	for {
		state.Running = false
		state.Status = store.StatusStarting
		state.StartedAt = nowRFC3339()
		state.FinishedAt = ""
		if err := st.WriteRuntimeState(cfg.ID, &state); err != nil {
			return err
		}

		env := append([]string{}, cfg.Env...)
		env = append(env, runtime.ResolveNetEnv(st.Root, cfg.Network, cfg.ID, cfg.Hostname)...)
		env = append(env, runtime.MountEnv(cfg.Mounts)...)
		env = append(env, runtime.CgroupEnv(cfg.ID)...)
		// 能力裁剪规格必须在这里下发。
		//
		// **曾经漏了这一行**，而 `licore run -d` 正是走这条 shim 路径 ——
		// 后果是从 v0.7.x 起，`-d` 创建的容器上 `--cap-add` / `--cap-drop`
		// **静默失效**：config.json 里存着正确的 capDrop/capAdd，但 init 的
		// 环境里根本没有 LICORE_CAPS_*，capsFromEnv 读到空值后就退回内置
		// 默认集——恰好等于"没传参数"的结果，因此不报错、不告警，
		// 只有读容器 PID 1 的 CapEff 才能看出。
		//
		// engine.go 的前台路径一直有这一行（`--cap-*` 在前台模式下是好的），
		// 两条路径各写各的才漏掉。**新增任何启动路径时，这份 env 必须与本
		// 行保持一致**；TestStartEnvMatchesEngine 会守住这个不变量。
		env = append(env, runtime.CapsEnv(cfg.CapDrop, cfg.CapAdd)...)
		// --workdir / --user 同样经环境变量下发（runtime.Config 是冻结接口，
		// 加不了字段）；init 在收口之后、execve 之前应用。
		env = append(env, runtime.WorkdirEnv(cfg.WorkingDir)...)
		env = append(env, runtime.UserEnv(cfg.User)...)
		res, err := startWithFn(&runtime.Config{
			Rootfs:   cfg.Rootfs,
			Hostname: cfg.Hostname,
			Cmd:      cfg.Cmd,
			Env:      env,
		}, func(pid int) {
			state.InitPID = pid
			// 容器已 fork 且 init 存活：置为 running，持续时间被 ps/stop 可见。
			running := state
			running.Running = true
			running.Status = store.StatusRunning
			_ = st.WriteRuntimeState(cfg.ID, &running)
			if o.OnStart != nil {
				o.OnStart(pid)
			}
		}, &runtime.StartOptions{
			Stdin:  in,
			Stdout: out,
			Stderr: errOut,
			StopCh: sigCtx.Done(), // shim 被停 → runtime 向 init 转发 SIGTERM/SIGKILL
			Grace:  GraceHold,
		})
		state.Running = false
		state.Status = store.StatusExited
		if err != nil {
			state.FinishedAt = nowRFC3339()
			_ = st.WriteRuntimeState(cfg.ID, &state)
			return fmt.Errorf("shim: 启动容器 %s 失败: %w", cfg.ID, err)
		}
		state.ExitCode = res.ExitCode
		state.FinishedAt = nowRFC3339()
		slog.Debug("容器 init 已退出", "id", cfg.ID, "code", res.ExitCode, "initPid", res.ChildPID)
		if err := st.WriteRuntimeState(cfg.ID, &state); err != nil {
			return err
		}

		if st.IsStoppedByUser(cfg.ID) {
			slog.Info("用户停止容器，shim 退出（标记保留）", "id", cfg.ID, "restart", cfg.Restart)
			return nil
		}
		if sigCtx.Err() != nil {
			// 直接 SIGTERM shim（systemd ExecStop 等）：彻底退出，不重启。
			slog.Info("shim 收到停止信号，退出", "id", cfg.ID)
			return nil
		}
		if !shouldRestart(cfg.Restart, res.ExitCode) {
			slog.Debug("按策略不再重启", "id", cfg.ID, "restart", cfg.Restart, "code", res.ExitCode)
			return nil
		}
		state.RestartCount++
		backoff := restartBackoff(state.RestartCount)
		if err := st.WriteRuntimeState(cfg.ID, &state); err != nil {
			return err
		}
		slog.Info("按策略重启容器", "id", cfg.ID, "attempt", state.RestartCount, "backoff", backoff.String())
		select {
		case <-time.After(backoff):
		case <-sigCtx.Done():
			slog.Info("重启等待期收到停止信号，shim 退出", "id", cfg.ID)
			return nil
		}
	}
}

// Reexec 以 shim 身份重执行当前二进制并脱离调用方终端（setsid 新会话）。
// stdout/stderr 追加写容器日志文件；调用方拿到 PID 后即可退出（run -d / boot）。
func Reexec(storeRoot, id string) (*os.Process, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("定位 licore 可执行文件: %w", err)
	}
	lf, err := os.OpenFile(LogPath(storeRoot, id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开容器日志 %s: %w", LogPath(storeRoot, id), err)
	}
	defer func() { _ = lf.Close() }()

	cmd := exec.Command(self) // 无参数：靠 env 分流 shim 路径
	cmd.Env = append(os.Environ(),
		EnvMarker+"="+markerValue,
		EnvStoreRoot+"="+storeRoot,
		EnvContainer+"="+id,
	)
	cmd.Stdin = nil
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("fork shim 失败: %w", err)
	}
	p := cmd.Process
	go func() { _, _ = p.Wait() }() // 回收本函数 fork 的子进程，避免僵尸
	return p, nil
}

// RunFromEnv 供 main 分流：从 LICORE_* 环境变量装配并进入 shim 主循环。
func RunFromEnv(ctx context.Context) error {
	if !IsShimProcess() {
		return ErrShimNotRequested
	}
	root := os.Getenv(EnvStoreRoot)
	id := os.Getenv(EnvContainer)
	if root == "" || id == "" {
		return fmt.Errorf("环境变量 %s/%s 缺失: %w", EnvStoreRoot, EnvContainer, store.ErrBadContainerConfig)
	}
	st, err := store.Open(root)
	if err != nil {
		return err
	}
	cfg, err := st.LoadContainer(id)
	if err != nil {
		return fmt.Errorf("shim: 加载容器配置失败: %w", err)
	}
	lf, err := os.OpenFile(LogPath(root, id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开容器日志失败: %w", err)
	}
	// shim 是脱离终端的后台进程：未显式指定 LICORE_LOG 时默认 Info 级，
	// 让重启/停止等生命周期事件进入容器日志。
	if os.Getenv("LICORE_LOG") == "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	}
	return Run(ctx, &Options{Store: st, Cfg: cfg, Stdout: lf, Stderr: lf})
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
