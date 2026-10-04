# 从 Docker 镜像转换（licore convert）

把 Docker 镜像转换成 LiCore 的 `.licore` 镜像，用于让 Docker Hub 上现成的镜像
在 LiCore 里可用。

## 原理：导出 rootfs + 重建运行配置

转换走的是**导出文件系统 + 重建元数据**的路线，而不是解析 Docker 的镜像格式：

1. `docker pull` / `create` / `export` → 拿到完整 rootfs tar
2. 解压 tar 到临时 rootfs 目录
3. `docker inspect` → 提取 `Entrypoint` / `Cmd` / `Env` / `Labels` / `User` /
   `WorkingDir` / `ExposedPorts` / `Volumes`
4. 生成 Boxfile → 调用 `build.Build()` → 产出标准 `.licore`

因此产物是「**完整的 rootfs（单层）+ 重建的运行配置**」，不含 Docker 的存储
驱动层链（overlay2 层链、容器元数据等）——那些对运行没有意义。

**转换是单向的**：LiCore 不会、也无法把 `.licore` 转回 Docker 镜像。

只通过 `os/exec` 调用 docker CLI，不引入第三方依赖，也不链接 Docker SDK。

## 前置条件

- 本机有可用的 `docker`（`licore convert` 需要调用它）
- 当前用户能访问 docker socket（否则见下方《错误处理》）

`licore convert` 不影响其他正在运行的容器：它只执行
`docker pull / create / export / rm`，操作对象是自己创建的临时容器
（名字形如 `licore-convert-<pid>`）。

## 单个镜像

```bash
# 转成文件（推荐先这样，产物可留存、可分发）
licore convert alpine:3.20 -o /tmp/alpine.licore

# 转换后直接导入本地 store
licore convert nginx:1.27-alpine --import

# 交叉架构（默认取宿主 GOARCH）
licore convert --arch arm64 alpine:3.20 -o /tmp/alpine-arm64.licore

# 覆盖产物镜像引用
licore convert alpine:3.20 --tag myorg/base:3.20 -o /tmp/base.licore
```

导入后即可正常运行：

```bash
licore run alpine:3.20 /bin/sh -c 'cat /etc/alpine-release'
```

### 常用参数

| 参数 | 说明 |
| --- | --- |
| `-o, --output PATH` | 输出 `.licore` 文件路径 |
| `--import` | 转换后直接导入本地 store（不产出文件） |
| `--arch ARCH` | 目标架构，默认宿主 `GOARCH` |
| `--tag NAME:VERSION` | 覆盖产物引用，默认由 Docker 镜像名派生 |
| `--keep-docker-image` | 转换后**不**删除 docker 侧拉取的镜像 |
| `--no-cleanup` | 保留临时容器与导出 tar（排查用） |
| `--data-dir DIR` | 数据目录（默认 `$LICORE_HOME` 或 `~/.licore`） |

> 默认会在转换成功后 `docker rmi` 掉本次拉取的镜像。如果你原本就想保留它，
> 加 `--keep-docker-image`；转换**失败**时不会删，便于排查。

## 批量转换

```bash
licore convert --from-file images.txt --output-dir ./dist/
licore convert --from-file images.txt --output-dir ./dist/ --jobs 4
licore convert --from-file images.txt --output-dir ./dist/ --arch arm64
```

清单文件格式：

```
# 以 # 开头的是注释，空行忽略
alpine:3.20
nginx:1.27-alpine

library/redis:7
```

逐条规则：

- 每行一个镜像引用；前后空白会被去掉；
- `#` 开头（含缩进）为注释；空行忽略；
- **重复项按首次出现去重**（同一镜像转两遍纯属浪费）；
- 产物名固定为 `<仓库名>-<架构>.licore`，落到 `--output-dir`。

### 并发

`--jobs N`（默认 1）控制并发转换数。docker 自身的部分操作是串行的，
调大不总是更快；并发时的进度输出会加锁，不会交错。

### 产物重名会被提前拦下

产物名是 `<仓库名>-<架构>`，**不含 tag**。因此：

```
nginx:1.26
nginx:1.27
```

两者都会映射到 `dist/nginx-amd64.licore`。若不检查，后转换的会**静默覆盖**
前一个——你以为拿到两个产物，实际只有一个。

LiCore 在**开始转换之前**就会报错并指出冲突的两个镜像与目标路径。遇到这种情况，
拆成两次运行，或用不同的 `--output-dir`。

### 单个失败不中断

批量模式下一个镜像失败不会终止整轮，失败项记入 `<output-dir>/failed.txt`：

```
<image>\t<error>
```

制表符分隔，第一列是镜像引用，第二列是错误（多行错误会先压成单行，保证
一行一条记录）。

**全部失败也会输出汇总**（成功 0 / 失败 N）并逐条列出原因，不会静默退出。
退出码：有失败项时为 1。

### 失败不会污染输出目录

产物先写到临时文件，**成功后才** rename 到最终路径。因此转换中途失败的镜像
不会在 `dist/` 里留下半成品 `.licore`。

汇总示例：

```
批量转换 3 个镜像（架构 amd64，并发 2）
→ [alpine:3.20] 开始转换
→ [nginx:1.27] 开始转换
✓ [alpine:3.20] 完成 → dist/alpine-amd64.licore（3.4 MiB）
✓ [nginx:1.27] 完成 → dist/nginx-amd64.licore（52.1 MiB）

批量转换完成：成功 2 个 / 失败 0 个 / 总计 55.5 MiB
产物目录：dist
```

## 错误处理

`licore convert` 会区分几种失败并给出**可执行的下一步**：

| 情况 | 提示 |
| --- | --- |
| 没装 docker | 给出安装文档链接 |
| daemon 未运行 | `请启动 docker（sudo systemctl start docker）后重试` |
| socket 权限不足 | `请用 sudo 运行，或把用户加入 docker 组（sudo usermod -aG docker $USER 后重新登录）` |
| 镜像不存在 / 拉取失败 | 检查镜像名与网络 |
| Windows 容器镜像 | 明确拒绝（LiCore 不支持，见下） |
| 磁盘空间不足 | 估算所需空间并给出清理建议 |
| 导出 rootfs 为空 | 报错而不是产出一个空镜像 |
| Boxfile 无法表达元数据 | 见下方 `${}` 与元数据限制 |

## 限制（重要）

### Windows 容器镜像不支持

`docker inspect` 的 `Os` 为 `windows` 时直接拒绝。LiCore 的后端是
namespace + cgroup，无法运行 Windows 容器。拒绝发生在 `docker export`
**之前**，不会白导出一遍大文件。

### `ENV` / `CMD` / `ENTRYPOINT` 里的 `${...}` 会导致转换失败

LiCore 的 Boxfile 解析器对 `${` **无条件做变量展开**，且**没有任何转义语法**
（`\${X}` 与 `$${X}` 实测同样报"未声明的变量"）。因此：

- `ENV` 值含 `${`：该条**被跳过并计数**，转换结束时告警。绝不静默改写成别的值
  ——悄悄改掉用户的环境变量比丢掉它更危险。
- `CMD` / `ENTRYPOINT` 含 `${`：**整体转换失败**并明确指出是哪一条。理由是
  丢掉 `ENTRYPOINT` 会产出一个跑不起来的镜像，比转换失败更糟。

Docker 镜像里这类值真实存在（`JAVA_OPTS=-Xmx${MEMORY}`、URL query、base64 等）。
遇到时可用 `--tag` 手工构造 Boxfile 后 `licore build`。

### 部分元数据会被丢弃

LiCore 的语法比 Docker 严格，下列情况会被跳过（并计数告警）：

| 情况 | 原因 |
| --- | --- |
| `ENV` 值为空 | LiCore 的 `ENV` 不接受空值 |
| `ENV` 值含 `=` | 解析器把第二个 `=` 视为错误 |
| `LABEL` 键含空格 / 值为空 / 值含 `=` | 同上，语法限制 |
| `WORKDIR` 是相对路径 | LiCore 要求绝对路径，丢弃而不是猜 |

### `HEALTHCHECK` 被**静默丢弃**

**这是最容易踩的一点**：Dockerfile 里的 `HEALTHCHECK` 指令**不会**被转换过来，
而且**不会有任何提示**。

原因：转换走的是 `docker inspect` 路线，而 `docker inspect` 的输出里
**不包含 `HEALTHCHECK`**（它在镜像构建历史里，不在 inspect 的 `Config` 结构）。
因此不是"我们选择丢弃"，而是"这条信息在数据源里就拿不到"。

**影响**：转换后的镜像没有健康检查；依赖健康检查做编排/重启判断的场景需要自己
补（LiCore 侧可用 `--restart on-failure` + 应用自检替代）。

### 磁盘预检在首次拉取时只做基线检查

转换会同时占用三份空间（docker 镜像 + 导出 tar + 解压后的 rootfs），
因此有磁盘预检，但它的强度取决于镜像是否已在本地缓存：

- **镜像未缓存（首次 pull）**：只检查"剩余空间 > 512 MiB"这一条基线。
  此时无法预知镜像多大，**大镜像（数 GB）可能在解压到一半时磁盘满**。
- **镜像已缓存**：用 `docker image inspect` 拿到真实大小，按"三份"估算并预检，
  不足时提前报错。

拉取大镜像前建议自己确认一下可用空间。

### 产物是单层镜像

转换会把导出的完整 rootfs 打成**一个层**（`layers=1`），而不是保留 Docker 的
原始分层。原因是 `docker export` 给出的本来就是合并后的文件系统，层信息已经
不存在了。

因此转换产物不享受 LiCore 的层复用（基础镜像与派生镜像之间共享层）——
那是 `licore build` 的 `FROM` 继承才有的能力。

## 未验证

`licore convert` 的单镜像与批量流程用**注入的 fake docker** 完整跑通过
（含失败路径、并发、重名检查、失败不污染输出目录），但**尚未在装了真实
docker 的机器上运行过**。首次使用建议先转一个小镜像（如 `alpine`）确认。
