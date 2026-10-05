// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/licore/hub"
)

// newHubCommand 实现 `licore hub` 命令树（当前仅 serve 子命令）。
func newHubCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hub",
		Short: "LiCore Hub 分发服务",
	}
	cmd.AddCommand(newHubServeCommand(out))
	return cmd
}

// newHubServeCommand 实现 `licore hub serve`：启动自研镜像分发 HTTP 服务。
// 常驻前台运行，Ctrl+C（SIGINT/SIGTERM）优雅关闭。
func newHubServeCommand(out io.Writer) *cobra.Command {
	var (
		port     int
		dataDir  string
		storage  string
		authUser string
		authPass string
		bind     string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "启动 LiCore Hub HTTP 服务",
		Long: "启动 LiCore Hub HTTP 服务（blob 存储 + tag 索引 + JWT 鉴权）。\n" +
			"默认绑定 127.0.0.1:3727。storage 支持 local（默认）/ s3（预留占位）。\n" +
			"可用 --username/--password 注册第一个登录用户；未指定时登录接口不可用。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// 仓库根 = 数据目录（tags/manifests 直接落于其下，blobs 于 hub/blobs）。
			root := hubDataRoot(dataDir)
			if err := os.MkdirAll(root, 0o755); err != nil {
				return fmt.Errorf("创建 hub 数据目录失败: %w", err)
			}
			// blob 驱动：local 落盘；s3 为占位（未实现时明确报错）。
			var bs hub.BlobStore
			switch storage {
			case "", "local":
				lb, err := hub.NewLocalBlobStore(filepath.Join(root, "hub", "blobs"))
				if err != nil {
					return err
				}
				bs = lb
			case "s3":
				return fmt.Errorf("hub serve: --storage s3 尚未实现（当前仅 local）")
			default:
				return fmt.Errorf("hub serve: 非法 --storage %q（可选 local|s3）", storage)
			}
			reg, err := hub.NewRegistry(root, bs)
			if err != nil {
				return err
			}
			srv, err := hub.NewServer(reg)
			if err != nil {
				return err
			}
			if authUser != "" {
				if authPass == "" {
					return fmt.Errorf("hub serve: --username 已给出但缺 --password")
				}
				srv.SetUser(authUser, authPass)
				fmt.Fprintf(out, "已注册登录用户 %s\n", authUser)
			}
			if authUser == "" && authPass != "" {
				return fmt.Errorf("hub serve: --password 已给出但缺 --username")
			}

			addr := net.JoinHostPort(bind, fmt.Sprintf("%d", port))
			handler := srv.Handler()
			// 超时必须显式设置：零值意味着**永不超时**，慢速连接（Slowloris）
			// 可以一直占着 goroutine 与 fd 不放，把服务拖垮。
			// ReadHeaderTimeout 是这里最关键的一条——它限制"发完请求头"的时间，
			// 正是 Slowloris 利用的窗口（docs/security-audit-v2.md G110/G112）。
			httpSrv := &http.Server{
				Addr:              addr,
				Handler:           handler,
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       60 * time.Second,
				WriteTimeout:      5 * time.Minute, // blob 上传/下载可能很大
				IdleTimeout:       2 * time.Minute,
			}

			// 前台服务：信号触发优雅关闭。
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = httpSrv.Shutdown(shCtx)
			}()

			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("监听 %s 失败: %w", addr, err)
			}
			fmt.Fprintf(out, "LiCore Hub 已就绪，监听 %s（数据目录 %s）\n", addr, root)
			err = httpSrv.Serve(ln)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			fmt.Fprintf(out, "LiCore Hub 已关闭\n")
			return nil
		},
	}
	cmd.Flags().IntVarP(&port, "port", "p", 3727, "监听端口")
	cmd.Flags().StringVar(&bind, "bind", "127.0.0.1", "绑定地址")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "数据目录（默认 $LICORE_HOME 或 ~/.licore，仓库置于 <root>/hub）")
	cmd.Flags().StringVar(&storage, "storage", "local", "blob 存储驱动：local|s3")
	cmd.Flags().StringVar(&authUser, "username", "", "注册登录用户名")
	cmd.Flags().StringVar(&authPass, "password", "", "注册登录用户密码")
	return cmd
}
