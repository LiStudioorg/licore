# LiCore — AI 协作者指南

本文件面向参与 LiCore 开发的 AI 代理与人类协作者，描述项目定位、核心约定与代码规矩。开始动手前请先通读本文件。

## 项目是什么

LiCore 是一个用 Go 编写的**轻量级容器引擎**，使用场景类似 Docker，但**不兼容 Docker / OCI，完全自研生态**：自研镜像格式、自研分发方式、自研运行时与网络。目标平台为 Linux 服务器、Android（有 Root / 无 Root）、macOS；支持多 CPU 架构；引擎常驻内存目标 **10–20 MiB**。

## 项目历史

| 版本区间 | 名称 | 说明 |
| --- | --- | --- |
| v0.1.0 ~ v0.6.1 | **Boxli** | 项目早期名称。模块路径 `github.com/LiStudioorg/boxli`，二进制 `boxli`，镜像后缀 `.boxli`，数据目录 `~/.boxli`，环境变量 `BOXLI_*`。 |
| v0.7.0 起 | **LiCore** | 现用名称。模块路径 `github.com/LiStudioorg/licore`，二进制 `licore`，镜像后缀 `.licore`，数据目录 `~/.licore`，环境变量 `LICORE_*`。 |

- **改名原因**：品牌统一。
- **v0.7.0 是 LiCore 的首个版本**，也是 Android（有 Root）支持完整交付的版本。
- **无数据迁移**：改名时项目尚未正式发布、无用户，因此**不提供** `~/.boxli` →
  `~/.licore` 的自动迁移，也**不保留**旧路径 / 旧环境变量的兼容层。改名靠的是一次性
  全仓库替换，不是运行时兼容分支。
- **历史 tag（v0.1.0 ~ v0.6.1）保留**，仍可拉取；旧 commit 的 author 与内容一律
  不改写。GitHub 旧仓库地址自动重定向到新地址。
- **文档中残留的旧名**：本文件的阶段记录（下文）与 `docs/` 下的历史验收/审计报告
  写于改名前后不同时期，其中的名称以当时为准。除"指向本机工作目录的绝对路径"
  外，仓库内不应再出现旧名，CI/评审可用下列命令核查：

  ```bash
  grep -rn 'boxli\|Boxli\|BOXLI' --include='*.go' --include='*.md' --include='*.sh' .
  # 期望仅剩形如 /home/.../work/boxli 的本机路径（若有），其余应为 0
  ```

## 发行注意事项

### tag 一旦推送，不要靠移动它来补充内容

**Go module proxy 对已发布 tag 的缓存是不可变的**：某个版本号一经 `proxy.golang.org`
抓取，其 `.info` / `.mod` / `.zip` 三个产物就被固定下来，之后**移动 tag 不会刷新
已缓存的内容**，重新下载拿到的仍是旧内容；pkg.go.dev 读的正是这份缓存，因此展示的
也是旧内容。这是 Go 代理的设计特性（保证依赖可复现），不是缺陷，也无法通过重试解决。

**因此**：tag 推送之后**不要**再补 commit 然后移动 tag 来"让内容进去"——那不会生效，
只会造成 git 里的 tag 指向与 proxy 缓存内容不一致。

**正确做法**：把所有改动（**包括 docs**）都 commit 并推送完，验证通过，**最后**再打
tag 并推送。tag 应当是一次性的发布动作，而不是可以事后修订的指针。

> 教训来源：v0.7.0 发布时先打了 tag、之后才补上 README 的改名通知与 AGENTS.md 的
> 项目历史节，再移动 tag。git 里的 v0.7.0 指向含通知的 `7875f4f`，但 proxy 缓存
> 的仍是移动前的 `d256eec`，导致 pkg.go.dev 展示的 README 缺少改名通知。
> 结论：不动 v0.7.0（保持缓存与 tag 语义清晰），把流程固化为下方检查清单。

### 唯一例外

若确实需要在 tag 之后补充内容，**只能通过发布新版本号**（如 v0.7.1）实现——
新版本号会走全新缓存，不受旧缓存影响。是否值得为此发补丁版，按"内容重要性 vs
发布节奏稳定性"权衡。

完整的打 tag 前检查清单见 [docs/release-checklist.md](docs/release-checklist.md)。

## 核心约定

- **语言**：Go，**纯 Go，全仓库零 CGO**（`CGO_ENABLED=0` 即可完整构建）。进入容器挂载命名空间这一步改由系统的 `nsenter` 承担（见《exec 与 nsenter》），因此**没有任何 cgo 例外**，也不再有 `-tags nocgo_exec` 构建变体。
- **模块路径**：`github.com/LiStudioorg/licore`。
- **可执行文件**：`licore`；`main.go` 位于项目根目录，便于在根目录直接 `go build`。
- **镜像后缀**：`.licore`。
- **镜像格式**：分层 gzip tar + 自研 `index.json` 清单，与 Docker / OCI 镜像**互不兼容**。
- **开源协议**：AGPL-3.0-only（见 `LICENSE`）。每个 `.go` 文件头部必须带版权声明：

  ```go
  // Copyright (C) 2026 LiStudioorg
  // SPDX-License-Identifier: AGPL-3.0-only
  ```

- **零外部容器组件**：不依赖 Docker、containerd、runc 及任何 OCI/runc/cgroups 库。

## 目录结构

```
licore/
├── main.go              # CLI 入口（根目录，直接 go build 即可编译）
├── hub/                 # 分发服务端：blob 存储 + JWT 鉴权 HTTP API + 客户端 Client
├── internal/
│   ├── runtime/         # 容器运行时：创建 / 启动 / 停止 / 回收，按平台后端分文件
│   ├── image/           # .licore 镜像的拉取、解析、校验（分层 gzip tar + index.json）
│   ├── network/         # 自研容器网络：容器间通信与 NAT 出口
│   ├── storage/         # 镜像与容器层存储：解压、层合并、读写层、卷
│   │   └── volume/      # 卷：驱动、命名/匿名卷、配额（volume/tmpfs/snapshot）
│   ├── store/           # 数据目录（~/.licore）：pull 落地、state.json、boot 标记
│   ├── resource/        # 资源限制与采集：CPU / 内存 / PID / 加速器直通
│   ├── engine/          # `licore run` 编排层：镜像查找 → 解包合并 → 状态落盘 → 启停
│   ├── shim/            # 每容器生命周期持有者（shim 进程 + restart 策略）
│   ├── boot/            # `licore boot` 一次性扫描拉起自启容器
│   ├── service/         # 系统服务（systemd / launchd / Magisk / Termux）管理
│   ├── build/           # 自研镜像构建：boxfile 解析 → 层生成
│   ├── compose/         # compose 编排解析与执行
│   ├── dev/             # 开发工具：文件监听、热重载
│   ├── doctor/          # 环境自检：内核/namespace/cgroup/systemd/存储
│   ├── scaffold/        # 项目脚手架：boxfile/compose 模板与 lint
│   └── execns/          # 纯 Go：探测并调用系统 nsenter，进入容器 mnt/uts/ipc/net/pid 命名空间
├── pkg/
│   └── sdk/             # 对外 Go SDK，供第三方以库方式驱动 LiCore
├── docs/
│   └── image-spec.md    # .licore 镜像格式规范（单一事实来源，改格式先改这里）
├── go.mod
├── LICENSE              # AGPL-3.0
├── AGENTS.md
└── README.md
```

- `internal/` 下的包不对外暴露；只有 `pkg/sdk` 是公开 API，改动须保持向后兼容。
- 新增顶层目录前先在这里登记，避免结构漂移。

## 平台后端：build tags 分文件

运行时按平台后端拆分，同一接口、多套实现，用 Go build tags 分文件，**禁止**在公共代码里散落 `runtime.GOOS` 判断：

| 文件后缀 | build tag | 适用平台 |
| --- | --- | --- |
| `*_linux.go` | `//go:build linux`（后端标记 `native_linux`） | Linux 服务器、有 Root 的 Android：原生 namespace/cgroups 路线 |
| `*_darwin.go` | `//go:build darwin`（后端标记 `vm_darwin`） | macOS：轻量虚拟机路线 |

约定：每个后端实现同一组内部接口，公共层只依赖接口；新平台 = 新 tag + 新文件，不改公共代码。

## Android 支持策略

- **Android 有 Root：官方原生支持**。走 `native_linux` 后端，使用
  namespace + cgroup，功能与 Linux 服务器一致，完整可用。
- **Android 无 Root：官方不支持**。LiCore 不做任何 proot 适配、不检测 proot、
  不集成 proot；用户可在 proot / Termux 等用户态 Linux 环境里自行运行 licore，
  但官方不保证可用性、不提供技术支持。
- **原因**：无 Root 的 Android 缺少容器所需的内核隔离能力（namespace /
  cgroup / setns 等），任何用户态方案（包括 proot）都无法提供真正的隔离。
- **代码约束**：不引入、不检测 proot / Termux；不影响其他平台行为。

## 开机自启动机制

LiCore **不采用全局常驻守护进程**。开机自启 = 一个**全局一次性系统服务** + **容器自身的 restart 策略**：

- 系统里只生成**一个** LiCore 服务文件。
- 开机时系统调用一次 `licore boot`。
- `licore boot` 扫描容器状态文件，拉起设置了自启的容器，**执行完即退出，不常驻**。
- 每个容器的生命周期由一个轻量 **shim 进程**持有（类似 Podman 的 conmon），引擎本体不常驻。

### 容器自启动标志（restart 策略）

创建容器时指定：

```bash
licore run -d --restart always         alice/myapp:v1
licore run -d --restart unless-stopped alice/myapp:v1
licore run -d --restart no             alice/myapp:v1
```

| 策略 | 行为 |
| --- | --- |
| `no`（默认） | 开机不自动启动 |
| `always` | 容器退出就重启；开机自动启动 |
| `unless-stopped` | 类似 always，但用户手动 `stop` 后开机不再拉起 |
| `on-failure` | 非零退出码才被 shim 重启；**开机不自动启动** |

### 一键配置系统服务（licore boot enable/disable/status）

用户只需一条命令，无需手写服务文件。`licore boot enable` 自动检测平台并完成配置：

| 平台 | 服务文件 | 注册方式 |
| --- | --- | --- |
| Linux | `/etc/systemd/system/licore.service` | `systemctl daemon-reload && systemctl enable licore` |
| macOS | `~/Library/LaunchAgents/dev.licore.boot.plist` | `launchctl load` |
| Android（Root） | `/data/adb/service.d/licore.sh`（赋执行权限） | Magisk service.d |
| Android（无 Root） | 不支持（见《Android 支持策略》） | 官方不提供自启支持 |

- `licore boot enable`：写入服务文件并注册，完成后输出服务文件路径与状态。
- `licore boot disable`：自动移除对应平台的系统服务文件并取消注册。
- `licore boot status`：显示开机自启是否启用、服务类型、服务文件路径；列出所有设置了 `restart=always` / `unless-stopped` 的容器及其状态。
- `licore boot`：由系统服务在开机时调用的一次性命令。扫描所有容器状态文件 → 启动 `restart=always` 或 `unless-stopped` 的容器 → 跳过标记 `stopped-by-user` 的 `unless-stopped` 容器 → 每个容器 fork 一个轻量 shim → 退出。

### 首次使用引导

用户首次执行任意 `licore` 命令时，若检测到未启用开机自启，提示：

```text
检测到 LiCore 尚未启用开机自启
是否启用？启用后开机会自动拉起设置了 restart=always 的容器
[y/N]:
```

用户确认后自动执行 `licore boot enable`。默认（直接回车）视为拒绝。

### 停止容器时的状态记录

`licore stop myapp`：停止容器 → 标记 `stopped-by-user`。下次开机时 `unless-stopped` 的容器不再被拉起；`always` 的容器仍会被拉起（与 Docker 行为一致）。

### systemd 服务文件内容（Linux）

```ini
[Unit]
Description=LiCore container engine
After=network.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/licore boot
ExecStop=/usr/local/bin/licore shutdown

[Install]
WantedBy=multi-user.target
```

### CLI 命令汇总（boot 相关）

```text
licore boot enable    启用开机自启
licore boot disable   关闭开机自启
licore boot status    查看开机自启状态与自启容器列表
licore boot           由系统服务在开机时调用，一次性拉起自启容器
licore shutdown       由系统服务停止时调用，优雅停止自启容器
```

## 代码规矩

- **错误处理**：错误必须包装上下文后再向上返回：`fmt.Errorf("load index: %w", err)`；只在 `main.go` / CLI 出口层打印，中间层只 `return`。忽略错误必须显式 `_ =`。
- **日志**：统一使用标准库 `log/slog`，结构化字段（`slog.String("container", id)` 等）；禁止 `fmt.Println` 打日志、禁止引入第三方日志库。
- **CLI**：使用 [cobra](https://github.com/spf13/cobra) 组织命令树（`pull` / `run` / `ps` / `exec` / `images` / `boot [enable|disable|status]` / `shutdown`；`run` 支持 `--restart no|always|unless-stopped|on-failure`）。命令注册代码全部在 `internal/cli`，未实现命令统一返回"尚未实现"。隐藏命令（`init` / `dev-run`）仅内部与开发用途，不在帮助中展示。
- **配置**：一律 YAML（`~/.licore/config.yaml` 及镜像 `index.json` 旁挂配置），字段用 `yaml` tag 显式命名；不要混用 TOML/JSON 配置文件（`index.json` 属于镜像格式，不算配置文件）。
- **依赖**：阶段 0 `go.mod` 保持零第三方依赖；新增第三方库必须在 PR 里单独说明理由，容器/镜像/oci 相关的库一律不批。
- **命名与注释**：导出标识符必须有文档注释；文件头保留 AGPL 版权声明两行。

## 禁止事项

1. **禁止**引入任何第三方容器组件 / 容器库（Docker、containerd、runc、buildkit、OCI 相关库、cgroups 库等）——容器生态完全自研。
2. **禁止**做任何形式的 Docker / OCI 兼容（不做镜像格式转换、不实现Distribution API），LiCore 只认 `.licore`。
3. **Android 无 Root 官方不支持**（见《Android 支持策略》）：不得引入/检测 proot 或 Termux、不得引导用户提权，也不得尝试任何用户态隔离方案冒充真隔离。
4. **禁止** CGO——**无任何例外**。全仓库必须 `CGO_ENABLED=0` 可构建，产物为静态二进制。
   需要与内核/命名空间交互而纯 Go 做不到的，一律通过调用**系统外部程序**解决
   （当前唯一一例是 `internal/execns` 调 `nsenter`，见《exec 与 nsenter》）。
   新增任何 cgo 代码必须先改本节并获得批准。
5. **禁止**在运行时引入常驻守护进程设计（引擎以单二进制按需执行为目标，服务化另立 RFC）。
6. **禁止**未经文档约定就新增顶层目录或改变 `pkg/sdk` 公开 API。

## exec 与 nsenter

`licore exec` 需要进入容器的 mount / uts / ipc / net / pid 命名空间，而纯 Go
**无法**可靠调用 `setns(CLONE_NEWNS)`（Go runtime 是多线程的，setns 要求调用线程
不与其它线程共享 `CLONE_FS`，见 Go issue #9091）。

LiCore 的解法：把「进入命名空间」这一步交给系统的 **`nsenter`**
（`internal/execns` 只负责探测与构造命令行）。

### 为什么是外部程序而不是 cgo

v0.7.x 及以前用自研 cgo 组件 fork 单线程子进程来完成 setns。v0.7.5 起改为
调用 `nsenter`，原因：

- **二进制保持纯 Go**：`CGO_ENABLED=0` 即可完整构建，静态链接、无 glibc 依赖，
  不再需要 C 工具链或 Android NDK；
- **构建矩阵简化**：不再有「纯 Go 版没有 exec」的分别，同平台只出一个包；
- **nsenter 已处理 PID namespace 的关键语义**：`setns(CLONE_NEWPID)` 不会把调用者
  本身移入新 PID namespace，只有之后 fork 出的子进程才是其成员。util-linux 与
  busybox 的 nsenter **默认都会在 setns 之后 fork**，正好满足这一点（自研 cgo 版
  为此写了两段 fork）。

### 探测顺序与命令构造（实现约束）

- 探测：`nsenter` → `busybox nsenter`（Magisk / 精简 Android 常见）→ 都没有则
  返回 `ErrNoNsenter` 并附带安装指引。
- **必须用短选项** `-t/-m/-u/-i/-n/-p`：busybox 的 nsenter **不支持任何长选项**
  （`--target` 会直接报错），长选项虽在 util-linux 上更可读却不可移植。
- **工作目录必须写成紧贴形式 `-w<dir>`**：`-w` 的参数在 util-linux 与 busybox 上
  都是「可选」的，写成 `-w` `/app` 会把 `/app` 当成要执行的命令。
- `-S/-G`（uid/gid）两个实现写法一致，可直接使用；其余用户态处理（伪终端分配、
  stdio 透传）仍由 Go 侧 `internal/runtime.Exec` 负责。
- 目标命令前必须加 `--`，否则以 `-` 开头的命令会被 nsenter 当成自己的选项。

### 运行期语义（对用户可见）

`exec` 是否可用取决于**运行环境有没有 nsenter**，与编译方式无关：同一个二进制在
装了 util-linux 的机器上可用，反之返回明确错误。macOS / Windows 上不可用（需在
VM / WSL2 内运行，由 VM 里的 nsenter 提供）。

## 常用命令

```bash
go build -o licore .        # 在根目录编译，产出 ./licore
go vet ./...               # 静态检查
gofmt -l .                 # 格式化检查（输出应为空）
go test ./...              # 运行测试
go run .                   # 快速跑一下 CLI
# 交叉编译示例：
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -o licore-android-arm64 .
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o licore-darwin-arm64 .
```

## 镜像格式：唯一规范在 docs/image-spec.md

`.licore` = 外层未压缩 tar（内含 `index.json` + `layers/NNNNNN.<name>.tar.gz` + 可选 `blobs/`）。

- 规范文档：[docs/image-spec.md](docs/image-spec.md)，它是镜像格式的**单一事实来源**。
- 任何格式改动必须**先改规范、再改代码**，且只允许通过 `specVersion` 做不兼容升级。
- 解析器实现位于 `internal/image`，必须实现规范第 4 节全部拒绝规则，禁止"尽力猜测"式宽容解析。
- digest 仅允许 `sha256:`；`index.json` 是唯一元数据源。

## 依赖白名单

`go.mod` 中的第三方依赖需要逐条批准，白名单如下：

| 依赖 | 用途 | 批准范围 |
| --- | --- | --- |
| `github.com/spf13/cobra` | CLI 命令树 | 只允许 `internal/cli` 包引用（main.go 仅调 `cli.Execute`） |

除此之外的第三方依赖一律不批；容器 / 镜像 / OCI / cgroups 相关库永久禁止（见"禁止事项"）。日志、配置、压缩、归档一律用标准库（`log/slog`、`archive/tar`、`compress/gzip`、`encoding/json`、`crypto/sha256`）。

## 当前阶段：v0.6.0（真机验收 + 审计已完成）

阶段 0 已完成：目录骨架、`go.mod`、文档、占位包，并已发布 `v0.1.0` 被 pkg.go.dev 收录。
阶段 1 / 阶段 2 已完成：镜像格式、运行时、boot/shim 体系、`licore run/stop/ps/rm` 端到端（见下）。
阶段 3 已完成：网络/卷/资源/CLI/Hub 五个并行模块合并入 main（v0.3.0）。
阶段 4 已完成：把网络/卷/资源参数真正作用到容器上（v0.4.0）。

**v0.6.0 已完成**：在真实 root 服务器上做全功能验收，修复发现的缺陷，并补齐静态与安全审计。

- **cgroup 限额真正生效**：cgroups v2 下 `cgroup.subtree_control` 未开启 `cpu memory pids`
  时子组限额文件不可写、写入被静默忽略；现由 `internal/resource` 在 `Setup` 前显式开启控制器。
- **`licore exec` 真正进入全部命名空间**：纯 Go 无法 `setns(CLONE_NEWNS)`（Go issue #9091），
  改由 `internal/execns` 完成（当时是 cgo 组件，v0.7.5 起改为调用系统 `nsenter`，见下）。
- **卷 `:ro` 真正只读**：bind 挂载后补 `MS_REMOUNT|MS_BIND|MS_RDONLY`，否则 `:ro` 形同虚设。
- **同名容器并发创建原子化**：名字唯一性从"扫描后创建"（TOCTOU）改为 `O_EXCL` 锁文件；
  `rm` 改走 `RemoveContainer`，避免绕过锁释放导致名字永久泄漏。
- **netlink 组包缺陷修复**：`IFF_UP` 写入 nlmsghdr 的 seq/pid 字段、`SetLinkMaster` 目标写反、
  `addroute` 的 `rtm_type=RTN_UNSPEC`、`addaddr` 前缀写进 `ifa_flags`——四处均导致真实 EAGAIN/EBUSY/EINVAL/ENETUNREACH。
- **审计与验收产物**：[docs/test-report-v0.6.0.md](docs/test-report-v0.6.0.md)（A–J 真机功能验收，
  PASS=10 / SKIP=1 / FAIL=0）、[docs/audit-v0.6.0.md](docs/audit-v0.6.0.md)（静态 + 安全 + 覆盖率）。
  可复现脚本：[docs/verify-root.sh](docs/verify-root.sh) + [docs/verify-root.md](docs/verify-root.md)。

**阶段 5 已完成（v0.5.0）**：消除"半成品"——
- **`licore build` 真正接线**：从"只输出构建计划"改为真正调用 `build.Build()` 构造
  `.licore` 镜像并自动 `licore pull` 导入本地 store；支持 `-t/--tag`、`-f/--file`、
  构建上下文、`FROM scratch`。
- **`compose up / scale`**：从占位改为经 `engine.Run` 真实创建容器 / 扩缩副本
  （boxfile/build 服务就地构建并导入）。
- **未实现的资源能力**（`--storage`、`--gpu/--npu`、`--network-bandwidth`）在
  CLI 层显式拒绝，`resource.write*` 返回 `ErrUnsupported`，不再"降级 warn 后假装成功"；
  `-p` 在 host/none 网络显式报错。
- **Android 支持策略定稿**：有 Root 官方原生支持；无 Root 官方不支持（见上文）。
- 特权路径（容器 B–F）由 root 真机按 [docs/e2e-v0.5.0.md](docs/e2e-v0.5.0.md) 验收，
  审计见 [docs/audit-v0.5.0.md](docs/audit-v0.5.0.md)。

**阶段 4 已完成（v0.4.0）**：把阶段 3 合并的参数真正作用到容器上——
- **网络接入**：`licore run` 启动时创建 veth pair，宿主端进网桥、容器端进容器
  netns；`-p` NAT 规则绑定到容器 IP；容器内 DNS 指向网桥网关（resolv.conf
  + hosts）。装配前移到启动前（engine 先分配 IP + 实化 NAT，runtime fork 后
  装配 veth）。
- **卷接入**：`-v` 在容器 mount namespace 里于 pivot_root 之前 bind 进
  rootfs 目标路径；`:ro` 只读标记生效；匿名卷自动创建（anon_<id>）。
- **资源接入**：容器 init fork 后其 PID 写入对应 cgroups v2 组
  `cgroup.procs`，CPU/内存/pids/io 限制在容器生命周期内生效；`licore rm`
  清理 cgroup。
- **`licore exec`**：setns 进入运行中容器的命名空间执行命令，支持 `-i`
  `-t`（PTY）`-e` `-w` `-u`；需 root（CAP_SYS_ADMIN）。
- **`licore hub serve`**：自建分发服务前台启动（--port/--data-dir/--storage
  local|s3），JWT 鉴权，Ctrl+C 优雅关闭；login/push/pull/search 端到端可跑。

> 特权路径（veth/nft、cgroup 写入、setns、exec）需 root；非 root 沙箱以
> runbook 记录验收步骤（docs/e2e.md）、审计结果见 docs/audit-v0.4.0.md。

**阶段 2 已收官**：`licore run` / `licore stop` / `licore ps` / `licore rm` 整合完成，冻结接口确立（见下节）。

**阶段 1 已完成**：`.licore` 镜像格式定义（docs/image-spec.md）、cobra CLI 骨架、`internal/image` 清单解析器、`internal/store` 落地存储、`licore pull` 本地 `.licore` 文件支持（`licore run` / `ps` / `exec` / `boot` / `shutdown` 为骨架占位，明确返回未实现）。

**阶段 2 进行中**：Linux 原生运行时 spike 已完成——`internal/runtime`（native_linux）实现纯 Go 的 namespace + pivot_root 容器（rootless 自动 user namespace），`licore init`（隐藏命令）为容器 1 号进程入口，`licore dev-run`（隐藏命令）为开发/基准入口；实测每容器 ≈ 2.3 MiB，报告见 [docs/runtime-benchmark.md](docs/runtime-benchmark.md)。`internal/storage` 层解包器已完成——`UnpackFile` 内容寻址解包（layers/sha256/<hex>/fs），`MergeLayers` 按序合并（whiteout/opaque 删除语义、符号链接逃逸防护、设备节点与 setuid 剥离、并发安全）；测试覆盖路径逃逸、重复条目、损坏 gzip、opaque 符号链接防护等场景。`licore images` 已完成——`store.ListImages` 扫描 state.json（损坏条目跳过并告警），输出 REPOSITORY/TAG/ARCH/LAYERS/SIZE/CREATED 按导入时间倒序，支持 `-q` 与 `--format` Go 模板，空 store 友好提示且退出码 0。boot/shim 体系已完成——`<root>/containers/<id>/` 状态目录（config.json + runtime.json + stopped-by-user 标记）为 run/boot/shim 共用地基；`internal/shim` 为每容器生命周期持有者（Reexec 重执行 + setsid 脱终端 + container.log，restart 策略循环与退避重启）；`internal/boot.StartAll` 实现 `licore boot` 一次性扫描拉起（策略矩阵 + 停止标记 + 幂等防重）；`internal/service` 管理 systemd unit（enable/disable/status，无 systemd 或无权限时明确提示并给出 sudo 手动命令）；`licore shutdown` 经 SIGTERM shim 优雅停机；首次使用引导接入真实 boot enable。剩余：`licore run` / `licore stop` / `licore ps` / `licore rm` 整合已完成（阶段 2 收官）。`internal/engine` 为一次 run 的编排层（镜像查找 → 每层解包 → rootfs 合并 → 容器状态落盘 → 前台持有或后台 fork shim），CLI 只做参数绑定；`licore run` 默认前台 stdio 直连、Ctrl+C 经 StopCh 转发容器、退出码透传 shell，`-d` 后台 fork shim 并打印容器 ID；`--name` 缺省自动生成 `adjective_animal` 式名字并去重；`-p`/`-v`/`--memory`/`--cpus`/`--pids-limit` 已解析并记入容器配置、运行时忽略并 `slog.Warn`（阶段 3 落地）。`licore stop` 先写 `stopped-by-user` 标记再 SIGTERM shim，超时强杀并补写终态，已停止容器幂等；默认宽限为 `shim.GraceHold+5s`，小于该值会与 shim 写终态竞态导致退出码丢失。`licore ps` 默认仅列运行中容器，`-a` 含已停止，`-q` 只出 ID，状态列区分 Up/Exited/Created 并标注 `user-stopped`。`licore rm` 删除已停止容器整目录（含该容器独占的 rootfs，共享层缓存保留），运行中拒绝并提示先 stop，`-f` 先停再删。另修复 `boot.PidAlive` 真实缺陷：僵尸进程对 `signal 0` 仍探活成功，僵死 shim 会被 boot 误判为“已在运行”而永不重启，现读 `/proc/<pid>/stat` 判僵尸态。剩余（阶段 3）：`licore exec`、资源限制（memory/cpus/pids）实际生效、`-p` 端口映射与 `-v` 卷挂载、`internal/network` 与 `internal/resource`。

### 阶段 3 并行模块（v0.3.0 已合并收官）

5 个模块分支已按序合并入 main，每个分支只碰自己那一列的路径，冻结接口零改动：

| 分支 | 合并 commit | 落地内容 |
| --- | --- | --- |
| `feat/network` | `5571467` | `internal/network`（bridge / veth / 端口 NAT / DNS / netlink 高层封装）+ `licore network` 命令树；`licore run` 接入 `--network/--ip` 与 `-p` 端口映射 |
| `feat/volume` | `df140b8` | `internal/storage/volume`（驱动 / 命名/匿名卷 / 配额 / tmpfs / snapshot）+ `licore volume` 命令；`licore run` 的 `-v` 卷落盘 |
| `feat/resource` | `92817c7` | `internal/resource`（cgroup / CPU / 内存 / PID / 加速器直通）+ `licore resource`、`licore stats`、`licore update`；`licore run` 接入 `--memory*`/`--cpus`/`--pids-limit`/`--cpuset`/`--blkio`/`--storage`/`--network-bandwidth`/`--gpu`/`--npu` |
| `feat/cli` | `750ec04` | `internal/cli` 新命令（tag/commit/save/load/export/import/compose/dev/build/doctor/lint/scaffold/completion）+ `internal/build`、`internal/compose`、`internal/dev`、`internal/doctor`、`internal/scaffold` |
| `feat/hub` | `eb88154` | `hub/`（blob 存储 + JWT 鉴权 HTTP API + 客户端 Client）；`licore login/pull/push/search` 已接入 `hub.Client`（`0c265c9`） |

Hub 分发命令说明：`licore login` 向 Hub 换取令牌并缓存到 `<数据目录>/hub/auth.json`
（绑定 Hub 地址）；`licore pull NAME:VERSION`、`licore push NAME:VERSION file.licore`、
`licore search QUERY` 复用该令牌。Hub 地址按 `--hub` > `$LICORE_HUB` > `http://127.0.0.1:3727`
顺序解析。`licore pull ./x.licore` 仍保留本地文件导入语义。

## 冻结接口（阶段 2 收官）

以下接口自 `licore run` 端到端跑通（阶段 2 收官）起**冻结**：签名、语义与哨兵错误均视为稳定契约。
多模块并行开发期间，**修改任一冻结接口必须先提 issue 讨论**，说明动机、兼容性影响与迁移方案，
达成一致后再动代码；禁止在业务分支里顺手改签名。只读使用不受限制。

新增接口（不改动既有签名）不需要 issue，但仍应在本节登记，保持本节为接口的单一索引。

### 一、Runtime（`internal/runtime`）

```go
// 启动
func Start(cfg *Config, onChildStart func(pid int)) (*StartResult, error)
func StartWith(cfg *Config, onChildStart func(pid int), opts *StartOptions) (*StartResult, error)

// 容器 1 号进程入口与分流
func RunInit() error
func IsInitProcess() bool

// 类型
type Config struct {
    Rootfs   string   // 容器新根（宿主机路径，必须已存在）
    Hostname string   // 容器 UTS 名
    Cmd      []string // 1 号进程 argv，必填
    Env      []string // KEY=VALUE
    Rootless bool     // 强制 user namespace；false 时按 euid 自动判定
}
func (c *Config) Validate() error

type StartOptions struct {
    Stdin, Stdout, Stderr *os.File     // nil → os.Stdin/os.Stdout
    StopCh                <-chan struct{} // 可读即向 init 转发 SIGTERM
    Grace                 time.Duration    // SIGTERM→SIGKILL 宽限，默认 10s
}
type StartResult struct {
    ChildPID int // 容器 init 在宿主上的 PID
    ExitCode int // 信号死亡时 = 128+signum
}

// 哨兵
ErrNotInit, ErrBadConfig, ErrNotRoot, ErrUnsupported
```

- **没有 `Stop` 函数**：停止容器由 `StartOptions.StopCh` 驱动（收到可读即 SIGTERM，`Grace` 后 SIGKILL）；
  面向用户的停止编排在 `internal/engine.Stop`（写 `stopped-by-user` 标记后 SIGTERM shim）。
- 平台后端以 build tag 分文件实现同一组签名（`*_linux.go` / `*_android.go` / `*_darwin.go`）；
  非 Linux 后端必须提供同名 stub 以保证全仓库可交叉编译。
- `Start` 与 `StartWith` 的分工：`Start` 是 `StartWith(cfg, onChildStart, nil)` 的简写，两者都必须保留。

**阶段 4 新增（v0.4.0，不做既有签名改动）**：

```go
// 在运行中容器的命名空间执行命令（setns 进入 mnt/uts/ipc/net/pid）
func Exec(o *ExecOptions) (int, error) // 返回退出码（信号死亡 128+signum）；需 root

type ExecOptions struct {
    TargetPID int          // 容器 init 宿主 PID（runtime.json initPid）
    Cmd       []string
    Env       []string
    Workdir   string
    User      string       // uid[:gid]
    Stdin, Stdout, Stderr  *os.File
    TTY       bool         // -t：伪终端
}
```

- `Exec` 由 `licore exec` 调用；非 Linux 后端提供同签名 stub（返回 ErrUnsupported）。
- 网络/卷/资源装配通过内部 `LICORE_NET_*` / `LICORE_MOUNT_*` / `LICORE_CGROUP_ID` 环境变量
  从父进程（engine/shim）传给容器 init，`envWithoutLiCore` 统一剥离，不经用户命令行。
- `runtime` 新增跨平台辅助：`NetEnv`、`ResolveNetEnv`、`MountEnv`、`CgroupEnv`（供 engine/shim）。

### 二、Store（`internal/store`）

```go
// 数据目录
func Open(root string) (*Store, error) // root 为空 → $LICORE_HOME → ~/.licore

// 容器状态目录（config.json + runtime.json + stopped-by-user + rootfs）
func NewContainerID() (string, error)
func (s *Store) CreateContainer(cfg *ContainerConfig) error
func (s *Store) LoadContainer(id string) (*ContainerConfig, error)
func (s *Store) FindContainer(idOrName string) (*ContainerConfig, error) // ID 前缀或名字；歧义报错
func (s *Store) ListContainers() ([]*ContainerConfig, error)            // 创建时间倒序
func (s *Store) ContainerNames() (map[string]bool, error)
func (s *Store) ContainerDir(id string) string
func (s *Store) ContainersRoot() string
func (s *Store) WriteRuntimeState(id string, st *RuntimeState) error
func (s *Store) ReadRuntimeState(id string) (*RuntimeState, bool, error)
func (s *Store) MarkStoppedByUser(id string) error
func (s *Store) ClearStoppedByUser(id string) error
func (s *Store) IsStoppedByUser(id string) bool
func (s *Store) BootEligible(cfg *ContainerConfig) bool

// 镜像落地
func (s *Store) Put(srcPath string, force, allowArchMismatch bool) (*image.Loaded, error)
func (s *Store) Exists(name, version string) (bool, error)
func (s *Store) ReadState(name, version string) (*State, error)
func (s *Store) ListImages() ([]ImageInfo, error)
func (s *Store) ImageDir(name, version string) string
func (s *Store) ImagesRoot() string
func (s *Store) RemoveImage(name, version string, force, inUse bool) error // 新增（v0.7.4）

// boot 标记
func (s *Store) BootMarker() string
func (s *Store) EnsureBootDir() error

type Restart string // RestartNo | RestartAlways | RestartUnlessStoped | RestartOnFailure
func (r Restart) Valid() bool
func (r Restart) BootEligible() bool

// 哨兵
ErrExists, ErrImageNotFound, ErrImageInUse, ErrContainerExists, ErrContainerNotFound, ErrBadContainerConfig
```

- `ContainerConfig` 的 JSON 字段为 `run`/`boot`/`shim`/`ps` 共用契约；**新增字段必须 omitempty**，
  且旧版本读新配置不得失败（向后兼容是硬要求）。
- `Put` 的 `allowArchMismatch` 对应规范第 4 节规则 6 的 `--allow-arch-mismatch`：为 `false` 时
  镜像平台须与宿主匹配；为 `true` 时跳过该校验，供 `licore build --arch <其他架构>` 交叉构建
  与显式导入异构镜像使用（层摘要仍会完整校验）。**v0.7.4 由 `Put(srcPath, force)` 变更为
  `Put(srcPath, force, allowArchMismatch)`**：原签名无逃生口，导致交叉构建的产物在自动导入
  阶段被 `CheckPlatform` 拒绝，用户只能手工改 `index.json` 再重打包。
- `licore pull` 与 `licore load`/`import` 走 `allowArchMismatch=false`（默认严格）；
  仅 `licore build --arch` 显式指定架构时传 `true`。
- 文件写入一律"临时文件 + rename"原子替换；容器目录内的临时文件必须与目标同目录（跨设备 rename 报 EXDEV）。
- `RuntimeState` 的写方只有 shim（前台模式下是持有容器的 CLI 进程）；其他模块只读。

### 三、Image（`internal/image`）

```go
// 清单解析（严格解析，未知字段一律拒绝，禁"尽力猜测"）
func ParseManifest(data []byte) (*Manifest, error) // index.json
func ParseConfig(data []byte) (*Config, error)     // config blob
func (m *Manifest) Validate() error
func (m *Manifest) Ref() string // name:version

// 归档打开与校验
func OpenFile(path string) (*Loaded, error)
func (l *Loaded) VerifyLayers() error
func (l *Loaded) CheckPlatform() error
func (l *Loaded) Entry(name string) (EntryInfo, bool)
func (l *Loaded) ExtractFile(name, dst string) error

// 路径安全（规范第 4 节）
func SafeArchivePath(name string) error

// 常量
IndexName, BlobsDir, MediaTypeManifest

// 哨兵
ErrBadManifest, ErrUnsafePath, ErrLayerMissing, ErrConfigMissing,
ErrSizeMismatch, ErrDigestMismatch, ErrBadApplyOrder, ErrArchMismatch,
ErrUnsafeLayer, ErrIndexTooLarge
```

- **没有 `ParseIndex`**：`index.json` 的解析入口是 `ParseManifest`（早期草案名，已废弃）。
- 任何格式改动必须**先改 `docs/image-spec.md`、再改代码**，且只允许通过 `specVersion` 做不兼容升级。
- `OpenFile` 只做清单类/结构类/config blob 校验；层全量摘要由 `VerifyLayers` 重算（流式，内存 O(1)）。
  `licore run` 走的是"信任 pull 期已校验"，不重复 `VerifyLayers`。

### 四、Storage（`internal/storage`）

```go
// 内容寻址层存储：<storeRoot>/layers/sha256/<hex>/fs
func UnpackFile(layerPath, wantDigest, storeRoot string) (*UnpackResult, error)
func MergeLayers(storeRoot string, orderedDigests []string, targetDir string) error
func LayerUnpacked(storeRoot, hexDigest string) bool
func LayerFSDir(storeRoot, hexDigest string) string
func LayersRoot(storeRoot string) string

type UnpackResult struct {
    DigestHex string
    FSDir     string
}

// 哨兵
ErrCorruptLayer, ErrDuplicateEntry, ErrBadDigest, ErrLayerMissingLocal
```

- 摘要参数形态固定：`UnpackFile`/`MergeLayers` 接受 64 位十六进制（`sha256:` 前缀可带可不带），
  `LayerUnpacked`/`LayerFSDir` 只接受裸十六进制。
- `UnpackFile` 是**差异视图**：whiteout 文件原样保留，删除语义由 `MergeLayers` 应用；两者职责不可混淆。
- `MergeLayers` 的 `targetDir` 可以不存在（内部 MkdirAll）；合并是"叠加拷贝"，同一容器重复合并不幂等，
  调用方须保证目标是全新目录。
- 层缓存**跨容器共享、永不随容器删除而回收**（引用计数是阶段 3 项）；`licore rm` 只删容器目录。

### 五、Shim（`internal/shim`）

```go
// 生命周期
func Reexec(storeRoot, id string) (*os.Process, error) // setsid 脱终端，日志追加 container.log
func Run(ctx context.Context, o *Options) error        // 主循环：启动 → 写状态 → 按策略重启或退出
func RunFromEnv(ctx context.Context) error             // main 分流入口
func IsShimProcess() bool
func LogPath(storeRoot, id string) string

type Options struct {
    Store        *store.Store
    Cfg          *store.ContainerConfig
    Stdin, Stdout, Stderr *os.File // 前台模式由持有容器的进程提供
    OnStart      func(pid int)     // init 起来后的回调，用于尽早落状态
}

// 常量
EnvMarker    = "LICORE_SHIM"
EnvStoreRoot = "LICORE_STORE_ROOT"
EnvContainer = "LICORE_CONTAINER"
GraceHold    = 10 * time.Second // 导出：stop 的宽限必须大于它

// 哨兵
ErrShimNotRequested
```

- `GraceHold` 是**跨模块契约**：`engine.Stop` 的默认超时派生为 `GraceHold+5s`。任何调小它的改动
  都会重新引入"stop 抢在 shim 写终态前强杀导致退出码丢失"的竞态，必须同步评估调用方。
- shim 是 `runtime.json` 的唯一写方（含前台模式下由 CLI 进程充当 shim 的场景）。
- 重启退避序列固定为 1s/2s/4s/8s/30s（封顶 30s）；改动需同步 `licore boot` 的幂等判定窗口评估。

## 并行开发约定

阶段 3 起多模块并行推进，约定如下。

### 分支与模块边界

每个模块一个独立分支，只允许改自己模块的目录与其 `_test.go`：

| 分支 | 允许修改 | 职责边界 |
| --- | --- | --- |
| `feat/network` | `internal/network/`（+ `*_test.go`） | bridge、veth、端口映射、DNS |
| `feat/volume` | `internal/storage/volume/`（+ `*_test.go`） | 卷、驱动、配额 |
| `feat/resource` | `internal/resource/`（+ `*_test.go`） | cgroup、GPU、IO |
| `feat/cli` | `internal/cli/`、`internal/engine/`（+ `*_test.go`） | 新命令、参数、输出 |
| `feat/hub` | `hub/`（+ `*_test.go`） | 仓库服务端 |

### 硬性禁止

1. **禁止修改 AGENTS.md、go.mod、main.go**，除非该模块明确需要且已在 issue 中说明理由。
   `go.mod` 新增第三方依赖一律需要单独批准（见"依赖白名单"）。
2. **禁止修改其他模块的接口签名**（上节"冻结接口"列出的全部符号）。需要新能力时，
   在**自己的模块内**定义窄接口并依赖它，不要反向改动对方包。
3. **禁止跨模块目录写入**：一个 PR 只碰自己那一列的路径。跨模块改动必须拆成多个 PR，
   由对应模块分支分别提交。
4. 新增顶层目录前先在"目录结构"一节登记（`internal/network` 与 `internal/resource` 已登记，
   `internal/storage/volume`、`hub/` 需在各自首个 PR 中补登记）。

### 跨模块协作方式

- 需要用别人的能力时，**依赖已冻结的具体函数**（当前内部包之间就是这样直连的），
  或者在自己模块里声明小接口由调用方注入。冻结接口已经足够支撑阶段 3，预期不需要新的跨模块缝。
- 共享的磁盘布局（`<root>/containers/<id>/`、`<root>/layers/sha256/<hex>/`、`<root>/images/`）
  是事实契约：新模块只允许**新增**子路径，不得改变既有文件名与语义。
- 每个模块的 PR 必须自带：`gofmt -l .` 为空、`go vet ./...` 通过、`go test ./...` 全绿、
  三平台交叉编译通过（linux/android/darwin amd64+arm64）。
- 发现冻结接口有缺陷时：**先提 issue，再改 AGENTS.md，最后改代码**；不要在自己的分支里
  悄悄放宽或绕过它。
