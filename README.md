# LiCore

> 用 Go 编写的轻量级容器引擎：无守护进程，2.3 MiB/容器，覆盖 Linux / Android / macOS；自研 `.licore` 镜像格式，不兼容 OCI。

![LiCore 演示：导入镜像 → 后台运行 → 端口访问 → exec → 停止删除](docs/demo.gif)

> 演示录屏（非剪辑）：导入 `.licore` 镜像、`run -d` 后台启动、宿主经端口映射直接访问、
> `exec` 进入容器命名空间、停止并删除。录制脚本 [scripts/demo.tape](scripts/demo.tape)，
> 环境准备与清理 [scripts/prepare-demo.sh](scripts/prepare-demo.sh)。

![LiCore 架构：无守护进程，对照 Docker](docs/architecture.svg)

> 左侧是 LiCore：单个二进制按需执行，**没有常驻守护进程**，每个容器由一个轻量 shim 持有。
> 右侧是对照的 Docker：常驻 `dockerd` → containerd → runc。图源 [docs/architecture.svg](docs/architecture.svg)。

## 特性

- 🪶 **极轻**：运行时内存目标 10–20 MiB，单个静态二进制；除可选的 `internal/execns`（`exec` 进入容器挂载命名空间）外全部为纯 Go。
- 🧩 **自研镜像格式**：`.licore` = 分层 gzip tar + 自研 `index.json`，简单、可逐层审计。
- 📱 **平台**：Linux 服务器、Android（有 Root）、macOS；Android 无 Root 官方不支持（见《Android 支持策略》）。
- 🏗 **多架构**：amd64 / arm64 / 386 / riscv64 等同一条命令交叉编译。
- 🔌 **零外部依赖**：不需要 Docker、containerd 或任何 OCI 组件，装一个 `licore` 就能用。
- 🛡 **资源限制**：CPU / 内存 / PID 限额内置于引擎。
- 🚀 **开机自启**：一条 `licore boot enable` 完成系统服务配置，无全局守护进程，每个容器由轻量 shim 独立守护。

## 快速开始

> LiCore 已具备完整的镜像、运行时、网络、卷、资源限制、`exec`、Hub 分发能力，并配有 CI 与一行安装脚本。最新版本见 <https://github.com/LiStudioorg/licore/releases>。

```bash
licore build -t demo:v1 .                            # 根据 Boxfile 构建 .licore 并自动导入
licore run -p 8080:80 -v data:/data --memory 256 demo:v1   # 端口映射 + 卷挂载 + 内存限制（MiB）
licore ps                                            # 查看运行中的容器
licore exec -it demo /bin/sh                         # 进入运行中容器的命名空间执行命令
licore network ls                                    # 查看容器网络
licore volume ls                                     # 查看卷
licore resource info                                 # 查看资源能力（cgroups 等）
licore stats                                         # 实时查看容器资源用量
```

> 当前已落地：镜像构建（`build`）、`.licore` 镜像导入（`pull`）、容器运行（`run`）、
> 列出（`ps`）、命名空间执行（`exec`）、网络（`network`）、卷（`volume`）、资源限制
> （`--memory/--cpus/--pids-limit` 等）、镜像产物操作（`tag/commit/save/load/export/import`）、
> Hub 分发（`login/pull/push/search`）与服务端（`hub serve`）、compose 编排
> （`compose up/down/ps/logs/scale/config`）。可执行 `licore --help` 查看完整命令树。

> ⚠️ 网络 veth、cgroup 写入、`licore exec` 需要 **root**（CAP_NET_ADMIN / CAP_SYS_ADMIN）；
> 未实现的资源能力（`--storage`/`--gpu`/`--npu`/`--network-bandwidth`）会显式报错而非静默生效。

## 安装

### 方式一：一行命令（推荐）

自动检测系统与架构，下载对应归档并校验安装：

```bash
curl -fsSL https://raw.githubusercontent.com/LiStudioorg/licore/main/scripts/install.sh | sudo bash
```

装到用户目录（无需 root）：

```bash
curl -fsSL https://raw.githubusercontent.com/LiStudioorg/licore/main/scripts/install.sh \
  | bash -s -- --prefix "$HOME/.local/bin"
```

装之前先看它要做什么（不下载、不写入）：

```bash
curl -fsSL https://raw.githubusercontent.com/LiStudioorg/licore/main/scripts/install.sh | bash -s -- --dry-run
```

常用选项：

| 选项 | 说明 |
| --- | --- |
| `--version <TAG>` | 指定版本，如 `--version v0.7.0`（默认取最新 Release） |
| `--prefix <DIR>` | 安装目录（默认 `/usr/local/bin`） |
| `--dry-run` | 只显示将执行的操作 |
| `--force` | 目标已存在时覆盖（默认拒绝覆盖） |

脚本行为约定：平台不支持、校验失败、目标已存在等情况一律**明确报错并停止**，不静默降级；
不会改写你的 `.bashrc` / `.zshrc`，若安装目录不在 `PATH` 中只提示一句。
退出码：`2` 用法错误、`3` 平台不支持、`4` 校验失败、`5` 权限不足。

> **关于校验的边界（不夸大）**：脚本比对归档的 SHA256，能发现**传输损坏**与归档不完整；
> 但 `SHA256SUMS` 与归档来自**同一 Release、同一 HTTPS 来源**，因此**不能防篡改**——
> 能改归档的一方同样能改校验和。真正防篡改需要签名（cosign / GPG）与独立信任根，
> 当前版本未引入。

### 方式二：手动下载二进制

到 [Releases](https://github.com/LiStudioorg/licore/releases) 选择对应平台的归档：

```bash
# 以 linux/amd64 为例
curl -fsSLO https://github.com/LiStudioorg/licore/releases/latest/download/licore-linux-amd64.tar.gz
curl -fsSLO https://github.com/LiStudioorg/licore/releases/latest/download/SHA256SUMS

sha256sum -c SHA256SUMS --ignore-missing   # macOS 用 shasum -a 256 -c
tar -xzf licore-linux-amd64.tar.gz          # 内含 licore + README.md + LICENSE
sudo install -m 0755 licore /usr/local/bin/licore
```

归档命名规则：`licore-<os>-<arch>[-cgo].tar.gz`。带 **`-cgo`** 后缀的包支持
`licore exec`，不带的为纯 Go 构建（exec 不可用，其余功能完整）。

### 方式三：从源码编译

```bash
git clone https://github.com/LiStudioorg/licore.git
cd licore
make all              # linux/amd64 + linux/arm64 + android/arm64，产物在 dist/
# 或直接编译当前平台：
go build -o licore .
```

> ⚠️ `make install` 与 `make linux` 产出的是**纯 Go** 构建，**没有 `licore exec`**。
> 需要 exec 请显式启用 cgo：`CGO_ENABLED=1 go build -o licore .`（详见《构建矩阵》）。

### 平台支持矩阵

| 平台 | 安装方式 | 是否可用 | root | cgo | `licore exec` |
| --- | --- | --- | --- | --- | --- |
| Linux amd64 / arm64 | 一行命令 / 手动 / 源码 | ✅ 完整支持 | 需要（网络、cgroup、exec） | 可选 | 仅 `-cgo` 包 |
| Linux arm / 386 / riscv64 | 一行命令 / 手动 / 源码 | ✅ 完整支持 | 需要 | 仅提供纯 Go | ❌ 明确报错 |
| Android arm64（有 Root） | 一行命令 / 手动 / NDK 编译 | ✅ 官方原生支持 | 需要 | 一行命令默认取 `-cgo` 包 | ✅（cgo 包） |
| Android arm64（无 Root） | — | ❌ 官方不支持 | — | — | — |
| macOS amd64 / arm64 | 一行命令 / 手动 / 源码 | ⚠️ 通过轻量 VM | 不需要 | 仅提供纯 Go | ❌ |

- **Linux 非 root**：镜像、卷、`build`、`run`（host/none 网络）可用；网络 veth、cgroup 限制、
  `exec` 需要 root。
- **Android 无 Root 官方不支持**：无 Root 环境缺少容器所需的内核隔离能力
  （namespace / cgroup / setns）。LiCore 不做 proot 适配、不检测 proot、不集成 proot；
  你可以在 proot / Termux 等用户态 Linux 环境中自行运行，但官方不保证可用性。
- 更细的能力对比见《[平台能力矩阵](#平台能力矩阵v060-实测)》，Android 适配细节见
  [docs/android-root.md](docs/android-root.md)。

## Hub 分发（login / pull / push / search）

LiCore 自研分发服务（不兼容 Docker Distribution API）。先登录，再推送与拉取：

```bash
licore login                            # 交互式换令牌（--hub 指定服务端，缺省 $LICORE_HUB 或 http://127.0.0.1:3727）
licore push alice/myapp:v1 ./myapp.licore   # 上传本地 .licore 到 Hub
licore pull alice/myapp:v1              # 从 Hub 拉取并落地为本地镜像
licore search myapp                     # 在 Hub 上搜索镜像
```

`licore pull ./x.licore` 仍保留本地文件导入语义；Hub 地址按 `--hub` > `$LICORE_HUB` > `http://127.0.0.1:3727` 顺序解析。

## 构建镜像（build）

根据 Boxfile（`FROM` / `COPY` / `ENV` / `WORKDIR` / `ENTRYPOINT` / `CMD` /
`EXPOSE` / `VOLUME` / `LABEL` / `USER` / `ARG`）构造 `.licore` 镜像并自动导入本地：

```bash
licore build -t demo:v1 .                 # 用 ./Boxfile（或 ./boxfile）构建并导入 demo:v1
licore build -f path/to/Boxfile -t demo:v1 --context ./src
licore images                              # 看到 demo:v1
```

- **构建上下文必须显式给出**（末尾位置参数或 `--context`，当前目录就传 `.`）：
  上下文决定 `COPY` 的源目录，静默落到 cwd 会把错误的（甚至敏感的）文件打进
  镜像；两者同时给出且不一致会直接报错。

- `FROM scratch` 为空基础镜像；`FROM name:version` 需先在本地存在（或先 `licore pull`）。
- 未实现的指令（`RUN`、远程 `ADD`）与资源能力会显式报错，不假装成功。

## 容器网络

```bash
licore run --network licore0 myapp:v1         # 接入默认 bridge：licore0（自动分配 IP）
licore run --network host myapp:v1           # 宿主网络
licore run --network none myapp:v1           # 无网络
licore run -p 8080:80 myapp:v1               # 端口映射 HOST:CONTAINER[:PROTO]
licore network ls / create / inspect / rm     # 网络管理
licore network connect NETWORK CONTAINER   # 把容器接入某网络
```

## 卷

```bash
licore run -v /data myapp:v1                  # 匿名卷（自动命名）
licore run -v myvol:/data myapp:v1            # 命名卷（先 create）
licore run -v /host/path:/data:ro myapp:v1    # bind 挂载（只读）
licore volume create / ls / inspect / rm      # 卷管理（local / tmpfs / snapshot）
```

## 资源限制

```bash
licore run --memory 256 --memory-swap 512 myapp:v1    # 内存（MiB，含软限制 --memory-reservation）
licore run --cpus 2 --cpuset-cpus 0-3  myapp:v1        # CPU 配额与绑核
licore run --pids-limit 128 myapp:v1                   # PID 上限
licore run --gpu 1 --npu 1 myapp:v1                    # 加速器直通
licore run --storage 1024 --network-bandwidth 10mbps myapp:v1  # 存储配额与带宽（当前显式报错，见已知限制）
licore resource info      # 资源能力诊断（cgroups v2 / 配额 / 加速器）
licore stats  [容器ID]     # 实时资源用量
licore resource update 容器ID --memory 512   # 动态调整运行中容器限制
```

注：资源限制在无权限或非 Linux 平台下列表应用失败时降级为告警（`slog.Warn`），不阻断容器运行。

## 进入运行中容器（exec）

```bash
licore exec myapp /bin/echo hi               # 在容器命名空间执行命令
licore exec -it myapp /bin/sh                # 交互式 TTY
licore exec -e FOO=bar -w /data -u 1000 myapp /bin/env   # 环境变量 / 工作目录 / 用户
```

`exec` 通过 setns 进入容器的 mnt/uts/ipc/net/pid 命名空间后执行命令；
需要 root。交互模式可带 `-i`（保持 stdin）与 `-t`（伪终端）。

> **构建说明**：纯 Go 无法 `setns(CLONE_NEWNS)`（见 Go issue #9091），因此进入容器
> **挂载命名空间**这一步由可选的 cgo 组件 `internal/execns` 完成。这是全仓库唯一的 cgo
> 代码，且仅在 Linux + 启用 cgo 时编译：
>
> - `go build -o licore .`（默认，Linux + cgo 可用）→ `exec` 功能完整。
> - `CGO_ENABLED=0 go build -o licore .` 或 `-tags nocgo_exec` → 自动走纯 Go stub，
>   其余功能完全不受影响，只有 `licore exec` 会返回明确的"未启用 cgo 支持"错误。
> - `GOOS=android` 官方交叉编译为纯 Go，`exec` 同样返回明确错误；需要 Android 上
>   的 `exec` 请用 NDK 工具链做 cgo 交叉编译（见 [docs/android-root.md](docs/android-root.md) 第 2 节）。
> - 交叉编译到 linux/arm64 需要对应架构的 C 工具链；没有时可加 `-tags nocgo_exec`。

## 自建 Hub 服务（hub serve）

```bash
licore hub serve                                   # 启动分发服务（默认 127.0.0.1:3727，Ctrl+C 关闭）
licore hub serve --port 9000 --data-dir /data/hub  # 自定义端口与数据目录
licore hub serve --username alice --password secret # 注册登录用户
```

服务端数据布局：`<root>/tags`、`<root>/blobs/sha256/<hex>`、`<root>/manifests`；
客户端凭证存于 `<root>/hub/auth.json`。`licore login/push/pull/search` 指向本地
Hub 即可端到端分发镜像（验证步骤见 `docs/hub-e2e.md`）。

## 开机自启

LiCore 没有常驻守护进程。给容器标记重启策略，再开启系统级自启，重启机器后容器会自动拉起：

```bash
licore run -d --restart always myapp:v1   # always / unless-stopped / no（默认）/ on-failure
licore boot enable                        # 一键写入并注册系统服务（自动识别平台）
licore boot status                        # 查看自启状态与自启容器列表
licore boot disable                       # 移除系统服务
```

开机时系统调用一次 `licore boot`，拉起 `restart=always` / `unless-stopped` 的容器后立即退出；每个容器由各自的轻量 shim 进程持有生命周期。首次使用 `licore` 时会引导你开启。

## 支持平台

| 平台 | 支持级别 |
| --- | --- |
| Linux 服务器 | 完整支持 |
| Android 有 Root | 完整支持（native_linux 后端；差异自动适配，见 docs/android-root.md） |
| Android 无 Root | 官方不支持（用户可自行在 proot 等环境中运行，不保证可用性） |
| macOS | 通过轻量 VM |
| Windows（WSL2 / 虚拟机） | 通过 Linux 版运行 |

### 平台能力矩阵

> 下表是**真机逐项实测**的结果。之所以不标版本号：能力矩阵只在重新跑一遍真机验收时才更新，
> 而版本号每发一次就会变，写死会让读者误以为矩阵已经过时。**没在真机上重测过，就不改这里的 ✅/❌。**

| 能力 | Linux（root） | Linux（非 root） | Android（root） | macOS | `CGO_ENABLED=0` 构建 |
| --- | --- | --- | --- | --- | --- |
| 镜像 / 卷 / `build` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `run` / `stop` / `ps` / `rm` | ✅ | ✅ | ✅ | — | ✅ |
| 网络（bridge / veth / NAT / DNS） | ✅ | 仅 host / none | ✅ | — | ✅ |
| 资源限制（cgroup） | ✅ v2 | ⚠️ 视 cgroup 委派而定 | ✅ v2 优先，自动回退 v1 | — | ✅ |
| `exec` 进入命名空间 | ✅ | ❌（需 CAP_SYS_ADMIN） | ✅ 仅 cgo 构建¹ | ❌ | ❌ 返回明确错误 |
| 卷 `:ro` 只读 | ✅ | ✅ | ✅ | — | ✅ |
| 开机自启（boot enable） | ✅ systemd | ✅ systemd（用户级视环境） | ❌ Magisk 后端未实现² | — | ✅ |

¹ 官方 `GOOS=android` 交叉编译是纯 Go，`exec` 返回明确错误；用 NDK 做 cgo 交叉编译后完整可用。
² 临时替代：手动放置 `/data/adb/service.d/licore.sh`（见 [docs/android-root.md](docs/android-root.md) 第 2 节）。

### 已知限制

> 同样来自真机实测；**未修复之前不会从这里删掉**。

- **`-p` 端口映射依赖宿主的 FORWARD 链**：若宿主 `iptables` FORWARD 策略为 `DROP`
  且没有放行 licore 网桥的规则（部分云主机、启用 rootless-docker 的机器如此），
  `-p` 发布的端口从宿主外部不可达。这与 Docker 在同一台机器上的行为一致；此时
  licore 会打印告警，容器间通信与出网不受影响。可用 `licore network ls` 确认网桥状态。
- **`exec` 需要 cgo 构建**：见上文《进入运行中容器（exec）》的构建说明。
- **层缓存不回收**：`licore rm` 只删容器目录，共享的 `layers/sha256/<hex>/fs` 缓存
  跨容器复用且不会自动清理（引用计数尚未实现）。
- **未实现的资源能力显式报错**：`--storage`、`--gpu`、`--npu`、`--network-bandwidth`
  在 CLI 层直接拒绝，不会静默降级。
- **Android**：`licore boot enable` 尚不生成 Magisk `service.d` 脚本（手动放置可替代）；
  `licore doctor` 未输出 Android 专项（SELinux 状态等，探测函数已就绪未接线）；
  SELinux **enforcing 真机行为未实测**（无测试机），LiCore 从不调 `setenforce`、
  不改设备策略。详见 [docs/android-root.md](docs/android-root.md) 第 8 节。

### Android 支持策略

- **有 Root**：官方原生支持，走 `native_linux` 后端（namespace + cgroup），
  功能与 Linux 服务器一致。
- **无 Root**：官方不支持。LiCore 不做任何 proot 适配、不检测 proot、不集成
  proot；你可以在 proot / Termux 等用户态 Linux 环境里自行运行 licore，但官方
  不保证可用性、不提供技术支持。
- **为什么无 Root 不支持**：Android 无 Root 环境缺少容器所需的内核隔离能力
  （namespace / cgroup / setns 等）。任何用户态方案（包括 proot）都只能模拟根
  目录，无法提供真正的进程 / 挂载 / 网络 / 资源隔离——这与 LiCore "真隔离" 的
  容器模型冲突，因此官方不支持。

**安装（有 Root）**：开发机交叉编译 → `adb push` 到 `/data/local/bin/licore` →
`chmod 0755` → `su` 下运行并建议 `LICORE_HOME=/data/licore`：

```bash
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -o licore-android-arm64 .
# 需要 exec 功能时改用 NDK cgo 交叉编译（docs/android-root.md 第 2 节）
```

有 Root 设备上的适配细节（SELinux 处理与限制、cgroup v1/v2 回退与语义差异、
命名空间矩阵、root 下为何不用 user namespace、`/dev` 与 `/proc` 装配、排查指引）
见 [docs/android-root.md](docs/android-root.md)；逐项验收命令与期望输出见
[docs/android-verify.md](docs/android-verify.md)。

## 镜像格式：.licore

`.licore` 文件是一个自研容器镜像包：内部由若干**分层 gzip tar** 组成，附一份自研 **`index.json`** 描述层顺序、架构与元数据。

⚠️ **与 Docker 不兼容**：`.licore` 不能由 Docker 构建或运行，`docker` 镜像也不能被 LiCore 使用。LiCore 配套自己的镜像构建与分发工具链（`licore hub`），生态完全独立。

## 构建

### 快速开始（Makefile）

```bash
make all            # 默认：linux/amd64 + linux/arm64 + android/arm64
make linux          # 桌面 Linux（amd64 + arm64）
make android        # Android arm64；有 NDK 则含 cgo（exec 可用），无则自动降级
make android-nocgo  # Android arm64 纯 Go（exec 不可用，其余功能正常）
make test           # go test ./...
make vet            # go vet ./...
make fmt            # gofmt 检查（有未格式化文件则失败）
make clean          # 清理 dist/
make install        # 装到 /usr/local/bin/licore
make help           # 列出全部目标
```

产物统一落在 `dist/`，形如 `dist/licore-linux-amd64`。版本号可注入：
`make VERSION=0.7.0 all`。

> 需要 `licore exec` 的 Linux 服务器请自行用 cgo 构建（Makefile 目标是纯 Go，
> 不含 exec）：
>
> ```bash
> CGO_ENABLED=1 go build -ldflags '-X main.version=0.7.0' -o licore .
> ```

### 构建矩阵

`licore exec` 需要 cgo（见下方说明），因此**凡 `CGO_ENABLED=0` 的构建都没有 exec**，
包括默认的 `make linux`。这一列是实测结论，不是推断：

| 目标平台 | 命令 | cgo | 交叉编译前置依赖 | `licore exec` |
| --- | --- | --- | --- | --- |
| linux/amd64 | `make linux` | 否（纯 Go） | 无 | ❌ 明确报错 |
| linux/arm64 | `make linux` | 否（纯 Go） | 无 | ❌ 明确报错 |
| android/arm64 | `make android`（有 NDK） | **是** | Android NDK（`ANDROID_NDK_HOME`） | ✅ |
| android/arm64 | `make android`（无 NDK，自动降级） | 否 | 无 | ❌ 明确报错 |
| android/arm64 | `make android-nocgo` | 否（纯 Go） | 无 | ❌ 明确报错 |
| linux/amd64（本机） | `CGO_ENABLED=1 go build -o licore .` | 是 | 本机 C 工具链 | ✅ |
| darwin/arm64、linux/386、linux/riscv64 等 | 手动命令 | 否 | 无 | ❌（非 linux 恒为 stub） |

**为什么 exec 要 cgo**：`licore exec` 需要进入容器的挂载命名空间，必须
`setns(CLONE_NEWNS)`，而纯 Go 无法安全调用（Go issue #9091），因此由可选 cgo 组件
`internal/execns` 承担。纯 Go 构建下 exec 返回明确的 `ErrNoCgoExec`
（提示"exec 需要 cgo 构建（CGO_ENABLED=1）"）——**不是"部分可用"，是明确拒绝**，
其余功能完全正常。需要 exec 就选一个带 cgo 的构建。

### 交叉编译（不依赖 Makefile）

```bash
# 服务器与桌面：无需任何工具链
CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -tags nocgo_exec -o licore-linux-arm64 .
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -tags nocgo_exec -o licore-darwin-arm64 .

# Android 纯 Go（exec 不可用）
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -tags nocgo_exec -o licore-android-arm64 .

# Android 含 cgo（exec 可用）：需要 Android NDK
NDK=$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/linux-x86_64/bin
CGO_ENABLED=1 GOOS=android GOARCH=arm64 \
  CC=$NDK/aarch64-linux-android21-clang \
  go build -o licore-android-arm64 .
```

前提：Go **1.27+**（`go.mod` 的 `go` 指令）；Android 侧还需一台已 Root 的设备，
安装与验证步骤见 [docs/android-root.md](docs/android-root.md) 与
[docs/android-verify.md](docs/android-verify.md)。

### 开发命令

```bash
go build -o licore .   # 根目录编译
go vet ./...           # 静态检查
gofmt -l .             # 格式检查
go test ./...          # 测试
```

## 常见问题

### 1. 为什么不用 Docker？

因为目标是**在 Docker 已经够用的地方之外**找一个更轻的点。Docker 需要常驻 `dockerd`，
再经 containerd / runc 拉起容器；LiCore 是单个二进制，按需执行，**没有任何常驻守护进程**，
每个容器由一个轻量 shim 持有。实测每容器成本约 **2.3 MiB**（见《实测 / 未验证》）。

需要说明的是：**如果你要的是生态，Docker 是对的答案**。LiCore 不兼容 Docker / OCI 镜像，
拿不到 Docker Hub 的现成镜像。它的定位是"自己能完全掌控、代码量小到可以通读"的容器引擎。

### 2. 兼容 Docker 镜像吗？

**不兼容，而且是刻意不兼容。**

- `.licore` 是自研格式（分层 gzip tar + 自研 `index.json`），**不能**由 Docker 构建或运行。
- Docker / OCI 镜像也**不能**被 LiCore 使用；不实现 Distribution API，不做镜像格式转换。
- `licore pull` 的 Hub 也是自研服务端，与 Docker Registry 无关。

LiCore 只认 `.licore`。这不是"还没做"，是设计选择（见 [docs/image-spec.md](docs/image-spec.md)）。

### 3. Android 无 Root 能用吗？

**官方不支持。**

无 Root 的 Android 缺少容器所需的内核隔离能力（namespace / cgroup / setns 等）。任何
用户态方案（包括 proot）都只能模拟根目录，无法提供真正的进程 / 挂载 / 网络 / 资源隔离——
这与 LiCore "真隔离" 的模型冲突。

你可以自行在 proot / Termux 等用户态 Linux 环境里运行 `licore`，但官方不保证可用性、
不提供技术支持。LiCore **不做** proot 适配、**不检测** proot、**不集成** proot。
**有 Root 才是官方支持路径**（走 `native_linux` 后端，与 Linux 服务器功能一致）。

### 4. 现在能上生产吗？

**请按"可评估、勿托付"来对待。**

- ✅ 在 **Linux root 服务器**上做过 A–J 全功能验收（PASS=10 / SKIP=1 / FAIL=0，见
  [docs/test-report-v0.6.0.md](docs/test-report-v0.6.0.md)），日常 `run / ps / exec / 卷 / 资源限制`
  是能跑通的。
- ⚠️ **Android 真机、macOS、多主机网络、大规模并发都未验证**（见下节）。
- ⚠️ 项目**尚未发布 1.0**，接口与行为仍可能变化；层缓存不做引用计数回收；`exec` 需要 cgo 构建。
- ⚠️ 目前**没有真实用户群**，出问题时你基本得自己读源码。

**建议**：适合在个人服务器、实验环境、CI 里试；把重要业务压上去之前，请先自己按
[docs/verify-root.md](docs/verify-root.md) 在你的机器上验一遍。

### 5. 为什么是 AGPL？

为了让"改进回到社区"这件事对**服务化使用**同样成立：如果有人把 LiCore 改一改做成
托管服务对外提供，AGPL 要求其修改同样开源。对普通自用、内部部署而言，AGPL 与 MIT
一样没有额外义务。

如果你需要其它许可（例如闭源集成），欢迎开 issue 说明用途。

## 实测 / 未验证

诚实地划分边界——**下列"未验证"项不代表不能用，只代表我们没在真实环境里跑过**。

### 已实测

| 项目 | 结论 | 依据 |
| --- | --- | --- |
| Linux 服务器（root）全功能验收 | **A–J：PASS=10 / SKIP=1 / FAIL=0** | [docs/test-report-v0.6.0.md](docs/test-report-v0.6.0.md) |
| 每容器内存成本 | 100 容器并发实测系统增量 **227 MiB**，摊薄 **≈ 2.3 MiB/容器**，退出后完全回收 | [docs/runtime-benchmark.md](docs/runtime-benchmark.md) |
| 100 容器并发启动 | 共享同一 rootfs 并发启动 **0 失败** | 同上 |
| 多平台交叉编译 | CI 每次推送都跑：**8 个目标平台**（linux amd64/arm64/arm/386/riscv64、android arm64、darwin amd64/arm64）+ android/linux 两个 cgo 构建，全部通过 | GitHub Actions |
| 静态 / 安全 / 覆盖审计 | 见各版本审计报告 | [docs/audit-v0.6.0.md](docs/audit-v0.6.0.md) 等 |
| 发布流程本身 | v0.7.1 真实发布：**11 个归档 + SHA256SUMS** 全部上传，下载后 `sha256sum -c` 校验通过 | [Releases](https://github.com/LiStudioorg/licore/releases) |

> 说明：CI 共 11 个 job（8 个交叉编译 + 格式与测试 + 2 个 cgo）。**"8 个平台"指 GOOS/GOARCH
> 组合数**；加上 linux/amd64、linux/arm64、android/arm64 的 cgo 变体，发布时产出 **11 个归档**。

> **那一项 SKIP 是什么**：`-p` 端口映射的宿主外部可达性。验收主机为 rootless-docker +
> ufw FORWARD DROP 环境，Docker 自身的端口映射同样不通（作对照），因此改在标准 root 主机
> 验证——目前**尚未**在标准主机上补测。

### 未验证

| 项目 | 状态 | 说明 |
| --- | --- | --- |
| **Android 真机** | ❌ 未验证 | 适配层已实现、`doctor` 有 `android.env` 检查项，但**没有在真实设备上跑过**；SELinux enforcing 行为未实测 |
| **macOS** | ❌ 未验证 | `vm_darwin` 后端（轻量 VM 路线）**尚未实现**，当前仅保证可交叉编译 |
| **多主机网络** | ❌ 未验证 | 只做单机 bridge / veth / NAT；跨主机容器网络未实现也未测试 |
| **大规模并发** | ⚠️ 仅到 100 | 100 容器已实测；更高密度、长时间运行、压测下的稳定性未验证 |
| **非 root（rootless）完整功能** | ⚠️ 部分 | user namespace 可跑，但网络仅 host / none，cgroup 限额视委派而定 |
| **`licore boot enable`** | ⚠️ 部分 | systemd unit **生成与内容**已验证；未在真机实际 `enable` 并重启验证 |
| **Windows** | ⚙️ 通过 WSL2 | 在 WSL2（或虚拟机）里安装 Linux 版 LiCore，与原生 Linux 体验一致；LiCore 本身不提供 Windows 原生后端。 |
| **ARM / 386 / riscv64 真机运行** | ⚠️ 仅交叉编译 | 这些平台**能编译通过**，但未在对应硬件上运行验证 |

> 我们宁可在 README 里写"没验证过"，也不希望你踩到才发现。发现文档与实现不符请
> 直接开 issue——那属于 bug。

> 项目原名 Boxli，自 v0.7.0 起更名为 LiCore。历史 tag（v0.1.0 ~ v0.6.1）保留可用。

## 支持这个项目

LiCore 是个人项目，无商业支持。如果它帮到了你，可以请我喝杯咖啡：

![微信收款码](docs/wechat-pay.png)

是否支持完全自愿，不影响项目走向。

## 开源协议

AGPL-3.0-only，详见 [LICENSE](LICENSE)。
