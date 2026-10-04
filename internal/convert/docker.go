// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

// Package convert 把 Docker 镜像转换为 LiCore 的 .licore 镜像。
//
// 转换走的是「导出 rootfs + 重建元数据」路线，而不是解析 Docker 的镜像格式：
//
//  1. docker pull / create / export       → 拿到完整 rootfs tar
//  2. 解压 tar 到临时 rootfs 目录
//  3. docker inspect                      → 提取 Entrypoint/Cmd/Env/Labels 等
//  4. 生成 Boxfile 并调 build.Build       → 产出标准 .licore
//
// 因此产物是「完整的 rootfs + 重建的运行配置」，不含 Docker 的存储驱动层
// （overlay2 层链、容器元数据等）——那些对运行无意义。这也意味着转换是
// 单向的：LiCore 不会、也无法把 .licore 转回 Docker 镜像。
//
// 本包只通过 os/exec 调用 docker CLI，不引入任何第三方依赖，也不链接
// Docker 的 SDK。
package convert

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// 哨兵错误：调用方（CLI / batch）据此区分失败原因，给出可执行的提示。
var (
	// ErrDockerMissing 表示 PATH 中找不到 docker 可执行文件。
	ErrDockerMissing = errors.New("licore/convert: 未找到 docker")
	// ErrDockerDaemon 表示 docker 存在但 daemon 不可达（未运行或无权限）。
	ErrDockerDaemon = errors.New("licore/convert: docker daemon 不可用")
	// ErrDockerPull 表示镜像拉取失败（多半是镜像名不存在）。
	ErrDockerPull = errors.New("licore/convert: 拉取镜像失败")
	// ErrNoRootfs 表示导出的归档里没有任何可用内容。
	ErrNoRootfs = errors.New("licore/convert: 导出的 rootfs 为空")
	// ErrDiskSpace 表示可用磁盘空间不足以完成转换。
	ErrDiskSpace = errors.New("licore/convert: 磁盘空间不足")
	// ErrWindowsImage 表示目标是 Windows 容器镜像，LiCore 不支持。
	ErrWindowsImage = errors.New("licore/convert: 不支持 Windows 容器镜像")
	// ErrUnsupportedMeta 表示元数据无法用 LiCore 的 Boxfile 语法表达。
	ErrUnsupportedMeta = errors.New("licore/convert: 元数据无法用 Boxfile 表达")
)

// Docker 是与 docker CLI 交互的最小接口。
//
// 抽成接口是为了让单元测试注入 mock 输出，而不是真跑 docker——
// 测试要能在没有 docker 的机器上跑，也必须能构造各种畸形 inspect 输出。
type Docker interface {
	// Run 执行一次 docker 调用，返回 stdout。stderr 非空且退出码非 0 时返回错误。
	Run(ctx context.Context, args ...string) ([]byte, error)
	// RunToFile 执行 docker 调用并把 stdout 写入文件（用于 docker export 的大流量）。
	RunToFile(ctx context.Context, path string, args ...string) error
}

// CLIDocker 是 Docker 接口的真实实现：直接 exec docker 可执行文件。
type CLIDocker struct {
	// Binary 是 docker 可执行文件路径，留空取 "docker"（走 PATH）。
	Binary string
}

// binary 返回实际要执行的 docker 路径。
func (d *CLIDocker) binary() string {
	if d.Binary != "" {
		return d.Binary
	}
	return "docker"
}

// Run 执行 docker 子命令并返回 stdout。
func (d *CLIDocker) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, d.binary(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("docker %s: %s: %w", strings.Join(args, " "), msg, err)
	}
	return stdout.Bytes(), nil
}

// RunToFile 执行 docker 子命令并把 stdout 直接落到文件。
// 镜像导出动辄几百 MB，走内存缓冲会白白吃掉同等内存。
func (d *CLIDocker) RunToFile(ctx context.Context, path string, args ...string) error {
	cmd := exec.CommandContext(ctx, d.binary(), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	f, err := createFile(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	cmd.Stdout = f
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("docker %s: %s: %w", strings.Join(args, " "), msg, err)
	}
	return f.Close()
}

// CheckDocker 确认 docker 可用且 daemon 可达。
//
// 区分三种失败，因为用户的下一步动作完全不同：
//   - 没装 docker          → 去装
//   - 装了但 daemon 连不上  → 启动 daemon，或者（最常见）当前用户没权限访问
//     /var/run/docker.sock，需要 sudo 或加入 docker 组
func CheckDocker(ctx context.Context, d Docker) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("%w：请先安装 docker（https://docs.docker.com/engine/install/）: %w",
			ErrDockerMissing, err)
	}
	out, err := d.Run(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		// daemon 不可达：把「未运行」和「无权限」分开说，二者的修法不同。
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "permission denied") {
			return fmt.Errorf("%w：当前用户无权访问 docker socket。"+
				"请用 sudo 运行，或把用户加入 docker 组（sudo usermod -aG docker $USER 后重新登录）: %w",
				ErrDockerDaemon, err)
		}
		return fmt.Errorf("%w：请启动 docker（sudo systemctl start docker）后重试: %w",
			ErrDockerDaemon, err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("%w：docker version 未返回服务端版本: %w", ErrDockerDaemon, err)
	}
	return nil
}
