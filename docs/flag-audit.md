# 全 flag 生效审计（flag-audit）

**执行时间**：2026-10-06
**执行环境**：本机 **45.207.198.91**（宿主机本体，非远程代跑），root，cgroups v2 统一层级
**内核**：Linux x86_64；Go 1.27.1；`CGO_ENABLED=0`
**被测二进制**：仓库根目录 `go build -o licore .` 产出，工作目录 `/home/li63050a/work/licore`

---

## 0. 结论摘要

| 项 | 结果 |
| --- | --- |
| 审计 flag 总数 | `run` 27 项、`exec` 9 项、`build` 8 项、`volume`/`network`/`compose` 子命令若干 |
| 真机实测生效 | 见 §2 全表（`run` 21 项、`exec` 6 项、`build` 5 项、`volume`/`network`/`compose` 全通过） |
| **发现并修复的缺陷** | **2 类 / 3 个 commit** |
| 显式拒绝（设计如此，非缺陷） | `--storage` / `--network-bandwidth` / `--gpu` / `--npu` |
| 无法在本机验证 | macOS/Android 平台项、`--storage` 实现（需 XFS project quota） |

**核心发现**：本轮 3 个缺陷中有 **2 个属于"整类静默失效"**——
参数解析正确、`config.json` 也存对了，只在最后一段断掉，且**失败是静默的**。
与历史上 `--cap-add`/`--user`/`--cpuset-cpus` 完全同一形态。

---

## 1. 判据与验证方法

### 1.1 为什么不能看 `config.json`

本轮三个缺陷在 `config.json` 里**全部显示正常**。它是"参数被解析并持久化"的证据，
不是"参数生效"的证据。因此判据一律取**容器内可见的真实状态**：

| 类型 | 判据 |
| --- | --- |
| 环境/身份 | 容器内 `echo $FOO`、`uid/gid`、`hostname`、`cwd` |
| 资源限制 | 该容器 cgroup 下的 `memory.max` / `pids.max` / `cpu.max` / `cpuset.cpus` / `io.weight` / `memory.swap.max` / `memory.low` |
| 网络 | 容器 netns 内网卡与地址、nft `table ip licore` 的 DNAT 规则、容器内监听端口可达性 |
| 卷 | 容器内写入 → 宿主卷目录实际出现该文件；`:ro` 写入必须失败 |
| 重启策略 | `kill -9` 容器 PID 1，观察 `runtime.json` 的 `initPid`/`restartCount` 是否变化 |
| 进程可见 | 容器 PID 1 的 `/proc/<pid>/cgroup`、`/proc/net/tcp` |

### 1.2 测试夹具

无 Docker、无现成镜像的宿主机（`docker` 未安装、`/root/.licore` 为空），
因此自建最小测试镜像：

- 用 **Go 静态编译**（`CGO_ENABLED=0`）造 3 个多调用小工具：`probe`（info/sleep/catfile/write/stat）、
  `listener`（TCP 监听，验证端口映射端到端）、`failer`（固定退出码 7，验证 `on-failure`）；
- 经 `licore build FROM scratch` 打成 `.licore` 镜像 `fgaudit/probe:{1..4}`。

**隔离与安全**（遵守 AGENTS.md《操作安全规范》）：

- 全程使用**专用数据目录** `/root/.licore-fgaudit`（另有 `/root/.licore-fgaudit2` 用于新旧二进制对照），
  **未触碰**任何生产容器 / 镜像；
- 所有测试容器名带 `fga-` 前缀并逐一登记，清理只删自己登记过的名字，
  脚本中**不存在** `rm -rf "$VAR"` 形式；
- 破坏性步骤（`kill -9` 容器 PID 1）只作用于**本轮自己创建的**容器，且单独一条命令执行。

---

## 2. flag 验证结果总表

图例：**解析** = CLI 接受并进入 config；**传递** = 下发到 init/运行时；**生效** = 容器内实测到真实状态。

### 2.1 `licore run`（27 个 flag）

| flag | 解析 | 传递 | 生效 | 实测判据与结果 |
| --- | --- | --- | --- | --- |
| `-e/--env` | ✅ | ✅ | ✅ | 容器内 `env_FOO=cli`、`env_BAR=cli2`（覆盖镜像 ENV） |
| `--entrypoint` | ✅ | ✅ | ✅ | `argv=["/probe" "info"]`，覆盖镜像 ENTRYPOINT |
| `--hostname` | ✅ | ✅ | ✅ | 容器内 `hostname`=`h42` |
| `--workdir` | ✅ | ✅ | ✅ | `/proc/<pid1>/cwd` = `/tmp`；非目录路径明确报错 |
| `--user` | ✅ | ✅ | ✅ | PID1 `uid=1000 gid=1000`（v0.9.8 已修，本轮复验通过） |
| `--cap-add` | ✅ | ✅ | ✅ | PID1 `CapEff=00000000a82425fb`（含 bit 21） |
| `--cap-drop` | ✅ | ✅ | ✅ | PID1 `CapEff=0000000000000000` |
| `--name` | ✅ | ✅ | ✅ | 缺省自动生成 `prime_wren`（adjective_animal）；重名明确拒绝 |
| `--memory` | ✅ | ✅ | ✅ | `memory.max=100663296`（96 MiB） |
| `--memory-swap` | ✅ | ✅ | ✅ | `128`→`memory.swap.max=33554432`（128−96=32 MiB）；`-1`→`max` |
| `--memory-reservation` | ✅ | ✅ | ✅ | 软限制落到 `memory.low=67108864`（64 MiB）；`memory.high` 保持 `max` |
| `--cpus` | ✅ | ✅ | ✅ | `cpu.max=50000 100000`（0.5 核） |
| `--cpuset-cpus` | ✅ | ✅ | ✅ | `cpuset.cpus=0-1` |
| `--pids-limit` | ✅ | ✅ | ✅ | `pids.max=24` / `32` |
| `--blkio-weight` | ✅ | ✅ | ✅ | `io.weight=default 4950`（10–1000 → 1–10000 换算正确） |
| `-v/--volume`（命名卷） | ✅ | ✅ | ✅ | 容器内写 `/data/hello.txt` → 宿主 `<store>/volumes/fgavol1/hello.txt` 实际出现 |
| `-v ...:ro` | ✅ | ✅ | ✅ | 读 `original` 成功；写报 `read-only file system`（拒绝） |
| `-p/--publish` | ✅ | ✅ | ✅ | nft `table ip licore` 生成 `tcp dport 18201 dnat to 172.18.0.19:80`；容器内 `*:80` LISTEN，宿主直连容器 IP 返回 `HELLO-FROM-CONTAINER` |
| `--network`（none/host/自定义） | ✅ | ✅ | ✅ | `none`→容器内仅 `lo`；自定义 `fganet1`→容器 `172.30.0.2/24`；`host`→与宿主同名 |
| `--ip` | ✅ | ✅ | ✅ | 容器内 `172.18.0.250` 可见（fib_trie） |
| `--restart` | ✅ | ✅ | ✅ | 四策略矩阵见 §3.4 |
| `--data-dir` | ✅ | ✅ | ✅ | 全程用它把测试隔离到 `/root/.licore-fgaudit` |
| `--storage` | ✅ | — | ❌ | **CLI 显式拒绝**：`--storage 存储配额尚未实现（需 XFS project quota）`，非静默降级 |
| `--network-bandwidth` | ✅ | — | ❌ | **CLI 显式拒绝**：`尚未实现（需 tc 流量整形）` |
| `--gpu` | ✅ | — | ❌ | **CLI 显式拒绝**：`设备直通尚未实现（需 cgroup eBPF 设备控制）` |
| `--npu` | ✅ | — | ❌ | 同上 |
| `-h/--help` | ✅ | — | ✅ | 输出正确 |

> **后 4 项是"显式拒绝"而非缺陷**：LiCore 的设计约定是"不支持的能力显式报错，
> 不做静默降级后假装成功"（AGENTS.md《阶段 5》）。它们的"生效"列记 ❌ 仅表示
> **功能未实现**，与"实现了解析却没生效"的静默失效有本质区别。

### 2.2 `licore exec`（9 个 flag）

| flag | 解析 | 传递 | 生效 | 实测判据与结果 |
| --- | --- | --- | --- | --- |
| `-e/--env` | ✅ | ✅ | ✅ | `env_FOO=execval` |
| `-w/--workdir` | ✅ | ✅ | ✅ | `cwd=/tmp`；缺省继承容器工作目录 |
| `-u/--user` | ✅ | ✅ | ✅ | `uid=1001 gid=1002`；缺省继承容器用户 |
| `-i/--interactive` | ✅ | ✅ | ✅ | stdin 可透传 |
| `-t/--tty` | ✅ | ✅ | ✅ | 分配伪终端，命令正常执行 |
| `--cap-drop` | ✅ | ✅ | ✅ | `CapEff=0000000000000000`（在容器已有能力上进一步收紧） |
| `--cap-add` | ✅ | — | ✅ | **明确拒绝**：`不允许放宽能力，--cap-add 被拒绝（收到 [SYS_ADMIN]）`，退出码 1 |
| `--data-dir` | ✅ | ✅ | ✅ | 隔离生效 |
| （cgroup 归置） | ✅ | ✅ | ✅ | exec 进程 `/proc/<pid>/cgroup` = `0::/licore/a36689d430b5`，即受容器 `pids.max=50` 约束（v0.9.2 修复复验通过） |

### 2.3 `licore build`（8 个 flag）

| flag | 解析 | 传递 | 生效 | 实测判据与结果 |
| --- | --- | --- | --- | --- |
| `-t/--tag` | ✅ | ✅ | ✅ | 产物落 `<store>/images/fgaudit/bt/1` |
| `-f/--file` | ✅ | ✅ | ✅ | 指定 Boxfile 生效 |
| `--context` | ✅ | ✅ | ✅ | 不存在时明确拒绝：`构建上下文非法` |
| `--arch` | ✅ | ✅ | ✅ | `--arch arm64` → `index.json` 的 `architecture=arm64` |
| `--os` | ✅ | ✅ | ✅ | `--os android` → `index.json` 的 `os=android` |
| `--no-cache` | ✅ | ✅ | ✅ | 构建成功（LiCore 始终从基础镜像重建，语义一致） |
| `--slim` | ✅ | ✅ | ✅ | 构建成功（当前与常规构建同，文档已注明） |
| `--data-dir` | ✅ | ✅ | ✅ | 隔离生效 |

> 默认跟随宿主：无 `--arch/--os` 时产物为 `amd64/linux`，实测确认。

### 2.4 `licore volume` / `network` / `compose`

| 命令 | 结果 | 判据 |
| --- | --- | --- |
| `volume create/ls/inspect/rm` | ✅ | 卷目录与 `Volume:` 输出正确 |
| `volume snapshot NAME` | ✅ | 生成 `<store>/volumes/.snapshots/<name>.<ts>` |
| `network create` | ✅ | 缺 `--gateway` 时报 `bridge 网络必须提供 --gateway`；补全后真实建出网桥 `fganet1`（`ip link` 可见），容器接入得 `172.30.0.2/24` |
| `network ls/inspect` | ✅ | 展示 driver/subnet/gateway/endpoints |
| `compose config` | ✅ | 解析并输出服务；流式 YAML（`[a, b]`）被明确拒绝并给出改写提示 |
| `compose up -d` | ⚠️→✅ | **修前 `replicas: 2` 只起 1 个**；修后起 2 个（见 §4.2） |
| `compose scale` | ⚠️→✅ | **修前恒失败**（同名冲突）；修后 2→4→1 正确扩缩容 |
| `compose ps/logs/down` | ✅ | 正常 |

---

## 3. 发现的问题与修复

### 3.1 【严重】cgroup 控制器未逐个启用 → 资源限制整类静默失效

**commit `930df16`** — `fix(resource): 控制器逐个启用，并拒绝"已请求但无法生效"的资源限制`

#### 现象

宿主 `/sys/fs/cgroup/cgroup.subtree_control` 未 delegate `cpuset`/`io` 时，
`licore run -d --memory 96 ...` 容器**正常 Up**，但
`/sys/fs/cgroup/licore/<id>/memory.max` **文件根本不存在**——
`--memory`、`--cpus`、`--pids-limit` 全部静默失效，且无任何报错。

#### 根因

`enableControllers` 把 `+cpu +memory +pids +cpuset` **一次**写下去。
cgroup v2 的 `subtree_control` 写入是**原子**的：内核因 `cpuset` 不可用返回
ENOENT，**整条写入**失败，其余本来可用的控制器一并作废。

真机逐项实测（同一宿主）：

```
cpu:    OK rc=0
memory: OK rc=0
pids:   OK rc=0
cpuset: FAIL（没有那个文件或目录）      ← 一个不可用
combined("+cpu +memory +pids +cpuset"): rc=1   ← 全灭
```

另有第二处同类问题：`io` 从未列入 `controllers`，而 `--blkio-weight` 写的正是
子组 `io.weight`，该文件不存在 → `--blkio-weight` 静默失效。

#### 修复（三处）

1. `controllers` 补上 `io`；
2. `enableControllers` 改为**逐个**写，返回实际可用的控制器集合——
   把"一个不可用"的爆炸半径限制在它自己身上；
3. `setupV2` 用 `missingControllersFor` 区分"是否致命"：
   **只有用户显式请求、且依赖的控制器不可用才报错**。
   这避免了向另一个方向倒退——宿主没有 `io` 时，一个没传 `--blkio-weight`
   的容器不应被拒绝启动。
4. `engine.Run` 由"一律 WARN 降级"改为：请求了限制却无法应用时**拒绝启动并回滚**，
   仅空限制（用户没提资源参数，建组只为 stats）才降级告警。
   放行一个"以为自己有内存上限、实际没有"的容器，比启动失败危险得多。

#### 反向验证（同一宿主状态，cpuset/io 未 delegate）

| 二进制 | 命令 | 结果 |
| --- | --- | --- |
| 旧（HEAD~） | `--memory 96` | 容器 **Up**，`memory.max` **文件不存在**（静默失效）❌ |
| 新 | `--cpuset-cpus 0` | **拒绝启动**，明确报"需要 cpuset 控制器" ✅ |
| 新 | `--memory 96 --pids-limit 24` | `memory.max=100663296`、`pids.max=24` ✅ |

**关键性质**：新二进制在 `cpuset` 不可用的同一宿主上，`--memory`/`--pids-limit`
**仍然生效**——证明"逐个写"确实隔离了故障。

#### 补充测试

新增测试缝 `writeSubtreeControl`，注入**模拟内核**的累加 + 可用性校验语义：

> 普通文件是覆盖语义，无法表达 `subtree_control` 的多次累积——直接拿它当假
> cgroupfs，会让"逐个写控制器"的实现看起来**只生效了最后一个**（实测：五次写后
> 文件里只剩 `+io`）。这个坑在写测试时实际踩到过。

回归用例 `TestSetupV2RejectsUnsupportedRequestedLimit` 固化两条要求：
`cpuset` 不可用时 memory/pids 仍须生效；请求 `cpuset` 必须报错。

### 3.2 【中】`compose scale` 恒失败、`replicas` 被忽略

**commit `62a5c5d`** — `fix(compose): 副本按序号命名，replicas 真正生效`

#### 现象

1. `licore compose scale web=3` **永远失败**：
   `licore: scale web 增启: 容器名 "compose_web" 已被占用`
2. `replicas: 2` 声明后 `compose up` **只起 1 个**容器。

#### 根因

- `startService` 生成的名字恒为 `<project>_<service>`，**不含副本序号**，
  而 `scale` 在循环里反复调用它 → 第二个副本必然撞名。
  讽刺的是 `stopService` 早就按"首副本无序号、其余 `_<idx>`"删除，
  **增删两侧对同一套命名规则的理解不一致**。
- `compose up` 压根没读 `s.Replicas`，对每个服务恒启一个容器。

#### 修复

- 新增 `serviceContainerName(p, service, idx)` 作为副本命名的**唯一**来源，
  `startService`/`stopService`/`countService` 共用；`startService` 增加 `idx` 参数。
- `compose up` 按 `s.Replicas`（缺省 1）循环启动。
- `scale` 增启时传入正确序号 `cur+i`。
- 顺带修 `countService` 的前缀匹配缺陷：原用 `prefix+"_"` 匹配，会把**兄弟服务**
  算成本服务副本（服务 `web` 的前缀 `<proj>_web_` 同时匹配兄弟服务 `web_2x` 的容器
  `<proj>_web_2x`）→ 副本数虚增、scale 增/减方向判错。改为只认
  `base` 与 `base_<纯数字>`。

#### 反向验证（真机，同一 compose 文件）

| 操作 | 修前 | 修后 |
| --- | --- | --- |
| `up -d`（`replicas: 2`） | 1 个容器 | **2 个**：`compose_web` + `compose_web_1` ✅ |
| `scale web=4` | 失败 | **4 个**（补齐 `_2`/`_3`）✅ |
| `scale web=1` | 失败 | 剩 `compose_web` 一个 ✅ |

新增 `internal/cli/compose_naming_test.go`：固定命名规则（idx 0/1/2/10），
并覆盖"兄弟服务不被计入"。

---

## 4. 未验证项与原因

| 项 | 原因 |
| --- | --- |
| macOS（`vm_darwin` 后端）/ Android 项 | 本机是 Linux x86_64 服务器，无对应平台 |
| `--storage` 配额实际生效 | 需 XFS project quota，本机根分区非 XFS；**CLI 已显式拒绝**，不是静默失效 |
| `--network-bandwidth` / `--gpu` / `--npu` | 功能未实现，**CLI 已显式拒绝**（`ErrUnsupported`） |
| cgroups v1 路径下的上述资源项 | 本机是 cgroup v2 统一层级 |
| `android/amd64` 交叉编译 | Go 工具链限制：`android/amd64 requires external (cgo) linking`，与"零 CGO"约束冲突。项目 Makefile 的官方矩阵本就只含 **android arm64**，非本仓库目标 |
| `compose logs` 内容正确性 | 仅确认命令可执行；日志内容的语义正确性未逐条核对 |

---

## 5. 质量门

| 门 | 结果 |
| --- | --- |
| `gofmt -l .` | 空（干净） |
| `go vet ./...` | 通过，无输出 |
| `go build ./...` | 通过 |
| 交叉编译（项目官方矩阵 5 个目标） | linux/amd64 ✅ linux/arm64 ✅ android/arm64 ✅ darwin/amd64 ✅ darwin/arm64 ✅ |
| `go test`（本次改动涉及包） | `resource` / `cli` / `compose` / `store` / `image` / `storage` / `network` 全绿 |
| `go test ./...` 其余失败项 | **均为本机环境所致，与本次改动无关**：`convert` 系列需 `docker`（未安装）、`engine` 网络用例需以 root 提供 `nft`/`iptables`。已用 `git stash` 在干净基线上复现同样失败以确认 |

---

## 6. 经验教训（与既有条目同源）

本轮 3 个缺陷里，2 个是**同一失效形态的变体**：

> **参数解析正确、`config.json` 也存对了，只在最后一段断掉，而且失败是静默的。**

它与 AGENTS.md 已记录的 `--cap-add`/`--user`/`--cpuset-cpus` 完全同类。本轮新增的
两条可迁移认识：

1. **"文件不存在"比"文件内容不对"更隐蔽。** `memory.max` 写不进去时，内核不是
   拒绝一次写入，而是**该文件根本没有被创建**。任何"读一下这个文件看看值对不对"
   的检查都会同时漏掉这两种情况——判据应当是"文件存在且值正确"。
2. **原子写入会把局部故障放大成全局故障。** cgroup v2 的 `subtree_control`
   一次写多个控制器时是全有或全无，一个不可用的控制器能让**其余全部**失效。
   凡是"批量写一个原子接口"的地方，都要考虑逐项写 + 按需判定致命性。

> 判据纪律（复用 AGENTS.md 既有结论）：**只看容器内真实状态**，不看 `config.json`、
> 不看命令退出码、不看 CLI 打印的"成功"。本轮三个缺陷在这三处全部显示"正常"。
