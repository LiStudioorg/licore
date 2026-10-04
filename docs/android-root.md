# Android（有 Root）运行 LiCore

本文档说明 LiCore 在 **有 Root 的 Android 设备**上的支持范围、运行时适配细节、
已知限制与排查方法。

> 无 Root 的 Android **官方不支持**：缺少 namespace / cgroup / setns 等内核隔离
> 能力，任何用户态方案（含 proot）都无法提供真正的隔离。LiCore 不检测 proot、
> 不集成 Termux、不引导提权。详见 [AGENTS.md](../AGENTS.md) 的《Android 支持策略》。

---

## 1. 支持范围

Android 有 Root 走与 Linux 服务器相同的 `native_linux` 后端：namespace + cgroup，
功能与 Linux 服务器一致。差异由运行时**在探测到 Android 时自动适配**，用户无需
额外参数。

| 能力 | Android（有 Root） | 说明 |
| --- | --- | --- |
| namespace 隔离 | ✅ | `CLONE_NEWPID/NEWNS/NEWUTS/NEWIPC`，bridge 模式另加 `NEWNET`，见第 3 节矩阵 |
| cgroup 资源限制 | ✅ | 优先 cgroup v2，设备只有 v1（或 v1/v2 混合）时自动走 v1 |
| 网络（veth + NAT） | ✅ | 同 Linux |
| 卷 / `:ro` / 匿名卷 | ✅ | 同 Linux |
| `licore exec`（含 `-it`） | ✅ | 依赖设备的 `nsenter`；Android 10+ 由 Toybox 自带，见 3.2 |
| 开机自启 | ❌ 未实现 | 设计为 Magisk `service.d`，`internal/service` 的 Android 后端尚未落地（见第 8 节） |
| SELinux | ⚠️ 见第 2 节 | enforcing 设备上存在策略限制，无法完全消除 |

---

## 2. 真机安装步骤

前提：设备已 Root（Magisk / KernelSU），内核开启 `CONFIG_NAMESPACES`（出厂 Android 10+ 的 GKI 内核普遍满足；`uname -r` 确认内核版本，`licore doctor` 的 `kernel.namespaces` 项给出最终判定）。

**① 交叉编译二进制**（开发机执行）：

```bash
cd licore
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -o licore-android-arm64 .
# 无需 NDK、无需 C 工具链：产物是纯 Go（无 cgo / glibc 依赖），exec 能力完整。
# 注意 Android 产物本身是动态链接的（bionic 平台运行时要求），这与 cgo 无关。
# `licore exec` 依赖设备上的 nsenter（见 3.2）。
```

**② 推送到设备**：

```bash
adb push licore-android-arm64 /data/local/tmp/licore
adb shell su -c 'cp /data/local/tmp/licore /data/local/bin/licore; chmod 0755 /data/local/bin/licore'
```

> SELinux 提示：`/data/local/tmp` 与 `/data/local/bin` 的执行域受设备策略约束，
> 被拦时先按第 4.5 节查 `avc` 拒绝记录再调整安装位置/标签，LiCore 不会（也不能）
> 替你改策略。

**③ 环境自检**：

```bash
adb shell su -c '/data/local/bin/licore doctor'
# 关注 kernel.version / kernel.namespaces / cgroups.mount / cgroups.controllers 四项，
# 以及末尾的 android.env 专项（Android 版本/型号、root、cgroup 形态、SELinux、
# namespace 矩阵、userns），缺失能力会给出排查建议。逐项含义见 android-verify.md 第 2 节。
```

**④ 数据目录**：root 默认为 `/root/.licore`，Android 上建议显式指定（`/data/local/tmp` 带 `nosuid` 且可能被清理）：

```bash
adb shell su -c 'LICORE_HOME=/data/licore /data/local/bin/licore pull ./demo_v1.licore'
```

**⑤ 跑第一个容器**：

```bash
adb shell su -c 'LICORE_HOME=/data/licore /data/local/bin/licore run -d --name demo --memory 64m demo:v1'
adb shell su -c 'LICORE_HOME=/data/licore /data/local/bin/licore ps'
```

逐项验收（每条命令带期望输出）见 [android-verify.md](android-verify.md)。

> **开机自启**：当前版本 `licore boot enable` 只生成 Linux systemd unit，
> Magisk `service.d` 后端未实现（第 8 节）。临时替代——自行创建
> `/data/adb/service.d/licore.sh`（`chmod 0755`）：
>
> ```sh
> #!/system/bin/sh
> sleep 20          # 等 data 分区与网络就绪
> export LICORE_HOME=/data/licore
> /data/local/bin/licore boot
> ```

---

## 3. 命名空间能力矩阵与 user namespace

### 3.1 矩阵

每个命名空间**是否需要 root**、**Android 上的可用性**、**缺失时 LiCore 的行为**：

| 命名空间 | 需要 CAP_SYS_ADMIN | Android 典型可用性 | 缺失时 LiCore 行为 |
| --- | --- | --- | --- |
| `pid` | 是（或 userns 内） | ✅ 内核标配 | **硬失败** `ErrNoNamespaces`：没有 pid ns 谈不上容器 |
| `mnt` | 是（或 userns 内） | ✅ 内核标配 | 同上，硬失败 |
| `uts` | 是（或 userns 内） | ✅ 内核标配 | 同上，硬失败 |
| `ipc` | 是（或 userns 内） | ✅ 内核标配 | 同上，硬失败 |
| `net` | 是 | ✅ 通常可用 | **软降级**：容器共享宿主网络栈，`Degraded` + Warn（端口映射/独立网络不可用，其余照常） |
| `user` | 内核开关（`max_user_namespaces>0`） | ⚠️ 多数 ROM 禁用/半残 | root 路径**根本不需要它**（见 3.3）；仅非 root 场景缺它才硬失败 `ErrNotRoot` |
| `cgroup` | 是（或 userns 内） | ⚠️ 部分内核缺 | 非必需项，当前实现不使用 cgroup ns |

探测方式是只读的（`/proc/self/ns` 目录项 + `/proc/sys/user/max_user_namespaces`），
任何情况下都不会为了"看看行不行"去真的创建命名空间。

### 3.2 `licore exec` 在 Android 上依赖 nsenter

二进制**始终是纯 Go**（`CGO_ENABLED=0`，无 cgo / glibc 依赖、无需 NDK；Android 产物
因 bionic 平台运行时是动态链接的，与 cgo 无关）。`exec` 需要进入容器的
**挂载**命名空间，即 `setns(CLONE_NEWNS)`；纯 Go 无法安全调用（线程会在 syscall 间迁移，
Go issue #9091），因此这一步交给系统的 `nsenter`：

| 功能 | Android 10+ | Android 9 及以下 |
| --- | --- | --- |
| 容器创建/运行/停止/资源限制 | ✅ | ✅ |
| `licore exec` 进入容器 | ✅ Toybox 自带 `nsenter` | ⚠️ 部分设备没有，需装 busybox |

`internal/execns` 的探测顺序：`nsenter` → `busybox nsenter`。都没有时返回
`ErrNoNsenter`，并给出安装指引（Magisk 或 busybox）；**其余功能完全不受影响**。

装 busybox 后即自动走 `busybox nsenter` 后备，无需额外配置：

```bash
# Magisk 环境通常已内置 busybox
which busybox || magisk --install-module busybox  # 或用 Magisk 模块商店安装
busybox nsenter --help | head -1                  # 确认可用
```

这不是 Android 特有的限制，Linux 服务器同理（只是 util-linux 标配了 nsenter）。

### 3.3 为什么 root 下**不加** user namespace 是正常路径

常见误解："root 都不加 userns，是不是降级了？"——不是。规划逻辑是：

| euid | userns 可用性 | 决策 | 语义 |
| --- | --- | --- | --- |
| 0 | 任意 | 必需 ns（+可选 net），**不加** `CLONE_NEWUSER` | **正常完整路径**，日志 Debug 级（Android 常见形态，不是警告） |
| ≠0 | 可用 | 追加 `CLONE_NEWUSER`（rootless） | 用 uid 映射换隔离 |
| ≠0 | 不可用 | **硬失败** `ErrNotRoot` | 无隔离可用，绝不假装 |

root 下加 `CLONE_NEWUSER` 反而是**负优化**：容器 root 会被映射成宿主普通 uid，
凭空失去挂载、改网络、写 cgroup 的能力——为了解决"没有 root"的问题而制造
"root 不够用"的问题。Android 设备上多数 ROM 干脆禁用了非特权 userns，这不影响
root 路线，LiCore 因此把它处理为 Debug 而不是警告（回归测试
`TestPlanNamespacesRootNoUserNSIsNotDegraded` 锁定该语义：root+无 userns 组合
**必须**既不报错也不标记 Degraded）。

"降级（Degraded）"在 LiCore 里只有一个触发条件：**隔离确实变弱了**（bridge
网络下 `net` 命名空间缺失）。它与"能力探测结果不同"是两回事。

---

## 4. SELinux


绝大多数 Android 设备默认 **enforcing**，这是与 Linux 服务器最主要的差异来源。

### 4.1 LiCore 做了什么

容器 init 进程在 `execve` 用户命令**之前**，继承引擎自身的 exec 过渡上下文：

1. 读引擎进程的 `/proc/self/attr/exec`（当前继承到的上下文）；
2. 写回**容器 init 自己**的同一路径，使 `execve` 后过渡到该上下文；
3. 失败时**只降级告警**，容器继续启动。

设计原则：

- **容器上下文与引擎保持一致**。这样"引擎能访问的东西容器也能访问"，不会因为
  放宽标签而扩大攻击面，也不需要在设备上预置任何策略。
- **绝不调用 `setenforce`**，绝不修改 `/sys/fs/selinux/*`，绝不动全局策略或
  SELinux 运行状态。LiCore 只读写自身进程的 `attr/exec`。
  > 这条约束由单元测试 `TestSELinuxNoGlobalStateChange` 静态守护：源码中一旦出现
  > `setenforce` / `/sys/fs/selinux` 等字样，测试立即失败。
- **不做"假装成功"**：写不进去就明确告警，不会静默吞掉。

### 4.2 为什么用 `attr/exec` 而不是 `attr/current`

`/proc/self/attr/exec` 是**下一次 `execve` 的过渡目标**（可写）；
`/proc/self/attr/current` 是**当前运行上下文**（多数策略下不可写）。
容器 init 需要的是"exec 用户命令时进入哪个上下文"，因此只能用前者。

另外，该属性**只作用于调用者自己的下一次 `execve`**，所以必须在容器 init 进程内、
紧邻 `execve` 写入——在父进程里写只会影响父进程自己。

### 4.3 三种状态下的行为

| 设备 SELinux 状态 | LiCore 行为 | 日志 |
| --- | --- | --- |
| **disabled**（`selinuxfs` 未挂载） | 直接跳过 | Debug 级，无噪音 |
| **permissive**（只记录不拦截） | 尝试继承上下文；失败仅告警 | 失败时 Warn |
| **enforcing**（强制拦截） | 尝试继承上下文；失败仅告警 | 失败时 Warn（见下） |
| 状态不可读（`unknown`） | 同 enforcing 处理 | — |

非 SELinux 宿主上 `/proc/self/attr/exec` 读取返回 **EINVAL**（内核只在 SELinux 为
活跃 LSM 时才提供该属性的读写；若宿主用 AppArmor，该文件存在但读写返回 EINVAL）。
这被当作**正常情况跳过**而不是错误——否则所有非 SELinux 设备都会打出误导性告警。
该分支有真实宿主上的测试覆盖（本项目开发机即 AppArmor，`attr/exec` 恒返回 EINVAL）。

> 语义依据：proc(5) 说明 `/proc/[pid]/attr/exec` 表示"**后续 execve(2) 时**要赋予
> 进程的属性"，且"SELinux 下该属性在 `execve(2)` 时被重置"。这正是"必须在
> execve 之前、在将要 exec 的那个进程里写"的原因。

### 4.4 enforcing 设备上的已知限制

LiCore **不修改策略、不放宽标签**，因此下面这些情况必然存在：

1. **`licore` 二进制自身的域受限**。若 `licore` 运行在受限域（如从 `/data/local/tmp`
   执行、被 Magisk 域约束），引擎本身可能无法挂载、建 cgroup 或 `setns`。
   表现为创建容器时 `EPERM`。

2. **容器内进程沿用引擎的标签**，可能被策略拦截。典型症状：
   - 读取某些宿主路径返回 `EACCES`／`Permission denied`；
   - `exec` 容器内可执行文件报 `Permission denied`（标签不允许 `execute`）；
   - 网络配置失败（标签无 `net_admin` 等）。

3. **把宿主目录 bind 进容器时标签不匹配**。宿主目录自带标签，容器内进程若
   无权访问该标签，即使挂载成功也读不到。设备文件同理。

### 4.5 排查指引

**第一步：确认 SELinux 状态与标签**

```bash
getenforce                     # Enforcing / Permissive / Disabled
cat /sys/fs/selinux/enforce    # 1=enforcing, 0=permissive
ls -Z /data/local/tmp/licore    # 看 licore 自身的标签
id -Z                          # 当前 shell 的上下文
```

**第二步：用 LiCore 自检**

```bash
licore doctor          # 含 Android 环境探测：SELinux 状态、cgroup 形态、
                      # namespace 可用性、user namespace 支持
```

**第三步：读容器日志里的降级说明**

若日志出现：

```text
WARN 设置 SELinux exec 上下文失败，容器将继续启动；若系统处于 enforcing，
     容器内进程可能被策略拦截 context=... err=...
```

说明上下文继承没成功，容器已用内核默认标签启动。此时容器内遇到的
`EACCES`／`Permission denied` 基本都可以归因到标签不匹配，而不是 LiCore 的缺陷。

**第四步：定位是哪一步被拦**

```bash
# 先看内核审计日志（最直接）
dmesg | grep -i avc | tail -20
logcat | grep -i avc | tail -20

# avc 记录会给出 scontext（谁）、tcontext（访问谁）、tclass 与被拒的权限
```

**第五步：可选缓解（由用户自行决定，LiCore 不代劳）**

- 把 `licore` 放到标签更宽松的位置执行；
- 由用户自行编写并加载针对性的策略模块（需要设备端 SELinux 工具链）；
- 仅在测试设备上临时 `setenforce 0` 验证"是否为 SELinux 导致"。
  > ⚠️ **LiCore 自身永远不会执行此操作**，也不会建议在生产设备上这样做。
  > 关闭 enforcing 会显著降低设备安全性。

### 4.6 本项目的验证边界

SELinux 的**上下文继承路径**有完整单元测试覆盖（读取、写入、跳过、降级、
安全约束），但：

- **enforcing 真机行为未经本项目实测**——所有开发/验证主机均为非 SELinux
  （`/sys/fs/selinux` 不存在，`attr/exec` 读取返回 EINVAL）。因此 enforcing 设备
  上的实际策略交互属于**已知未验证区域**，第 2.4 节的限制基于 SELinux 机制推导，
  而非实测结论。
- 验证脚本 [verify-root.sh](verify-root.sh) 在本机（非 SELinux）全部通过，
  确认**非 SELinux 路径零回归**。

---

## 5. cgroup 适配

Android 设备的 cgroup 形态比 Linux 服务器更分散：

| 形态 | 探测方式 | LiCore 行为 |
| --- | --- | --- |
| cgroup v2 | `/sys/fs/cgroup/cgroup.controllers` 存在 | 优先使用 |
| cgroup v1 | 各控制器分别挂载（`memory/`、`cpu/`、`pids/`…） | v2 不可用时自动回退 |
| v1/v2 混合 | 部分控制器在 v1、部分在 v2 | 按 v2 优先，v1 补齐 |
| 无 cgroup | 两者都无 | 容器可运行，但**无资源限制**（明确告警） |

**优先 v2**：保证支持 v2 的设备（含桌面 Linux 与新 Android）行为不回归。

**v1 的控制器子集可以是不完整的**：设备的 v1 常常只挂了部分控制器。此时
**缺失的控制器不是错误**——能设的限制照设，设不了的跳过并继续（与 v2 统一层级
的全有全无语义不同）。

**两个 v1 与 v2 的语义陷阱**（已在实现中显式处理）：

- `memory.memsw.limit_in_bytes`（v1）是**内存 + swap 的总和**，而 v2 的
  `memory.swap.max` 是 **swap 单独**的上限。搞反会把容器总内存限得比 `--memory`
  还小，导致容器被立刻 OOM。
- v1 的 `cpu.shares` 就是 `--cpu-shares` 的原始语义 `[2,262144]`，与 v2 的
  `cpu.weight` 量纲不同，**不能**做换算。
- CPU 用量的位置也不同：v1 在 **cpuacct** 控制器（`cpuacct.usage`，单位纳秒），
  v2 在 `cpu.stat` 的 `usage_usec`（微秒）。`licore stats` 已各自适配。

**组布局**（`licore rm` / 排查时按此找）：

```text
v2： /sys/fs/cgroup/licore/<容器ID>/            # 统一层级一个目录
v1： /sys/fs/cgroup/memory/licore/<容器ID>/     # 每个已挂载控制器一个
     /sys/fs/cgroup/cpu/licore/<容器ID>/  …
```

LiCore 会在 v2 的**父组** `/sys/fs/cgroup/licore` 上开启
`cgroup.subtree_control`（`cpu memory pids`）——不开启时子组的限额文件
根本不可写，这是 Android 定制内核上最容易踩的一处。

**自查设备形态**：

```bash
ls /sys/fs/cgroup/cgroup.controllers && echo v2 可用 || echo 无 v2
grep cgroup /proc/mounts                        # v1 时各控制器分别挂载
```

---

## 6. `/dev` 与 `/proc` 适配

### 6.1 `/dev`

容器拿到一个**最小可用**的 `/dev`，而不是整体 bind 宿主 `/dev`——后者会把
Android 的 `binder` / `ashmem` / `kgsl` 等平台专有节点暴露给容器，既无意义也
可能带来越权面。

| 内容 | 实现 |
| --- | --- |
| `null` `zero` `full` `random` `urandom` `tty` `ptmx` | 按需从宿主 bind 单个节点 |
| `/dev/shm` | 独立 tmpfs，默认 64 MiB；`LICORE_SHM_SIZE` 可调整（支持 `16m`/`512k`/`1g` 后缀，非法值告警后回退默认） |
| `/dev/pts` | devpts 实例（`exec -it` 依赖） |
| `/dev/fd` `stdin` `stdout` `stderr` | 指向 `/proc/self/fd` 的符号链接 |

宿主缺少某设备节点时跳过（宿主自身的问题，不该阻断容器）。

### 6.2 `/proc`

部分 Android 设备默认 `hidepid=2`，会让容器内 `ps` 看不到自己的进程。

LiCore 先解析**宿主** `/proc/self/mountinfo` 判断现状：

| 宿主 `hidepid` | 容器挂载参数 |
| --- | --- |
| 未设置 | 不传（用内核默认） |
| `0` | 不传（已最宽松） |
| `1` 或 `2` | 显式 `hidepid=0` |
| 状态不可解析 | 不传（保守），记 Debug 日志 |

若内核拒绝 `hidepid` 参数，回退为不带参数重试；两次都失败才报错——
**参数不被支持不应阻断容器启动**。

> 用 `mountinfo` 而非 `/proc/mounts`：`hidepid` 是 per-mount 选项，
> `mounts` 只反映 superblock 级选项，**读不到** hidepid。

---

## 7. 已知限制汇总

| 限制 | 状态 | 说明 |
| --- | --- | --- |
| 无 Root 的 Android | **不支持** | 官方策略，见开头 |
| enforcing 下的策略拦截 | **无法消除** | LiCore 不改策略；见第 4.4 节 |
| enforcing 真机实测 | **未验证** | 无 SELinux 测试机；见第 4.6 节 |
| `exec` 报「需要 nsenter」 | 环境缺 nsenter | Android 10+ 由 Toybox 自带；更早版本装 busybox；见 3.2 |
| 开机自启（Magisk） | **未实现** | 手动 service.d 脚本可替代；见第 2 节与第 8 节 |
| 无 cgroup 的设备 | 可运行但无限制 | 明确告警，不假装成功 |
| user namespace 不可用 | 对 root 路线无影响 | root 本就不加 userns（3.3），非 root 才受影响 |

---

## 8. 路线图（"设计已有、代码未落地"项——避免与第 1 节的 ✅ 混淆）

| 项 | 现状 | 计划 |
| --- | --- | --- |
| `licore boot enable` 的 Magisk 后端 | 未实现（`internal/service` 仅有 systemd 后端；AGENTS.md 已登记设计） | 生成 `/data/adb/service.d/licore.sh` |
| `licore version` 的 Android 平台标识 | 显示 `linux/arm64` 等原始 GOOS/GOARCH | 识别 Android 身份后追加 `android/arm64, root` |

以上均为**已识别、未接线**状态；第 1 节矩阵中的 ✅ 不包含它们。

已接线（曾在本节列出，保留对照）：`licore doctor` 已渲染 **`android.env`** 专项检查项
——Android 身份/版本/API/型号、root 状态、cgroup 形态、SELinux 状态、namespace 能力矩阵、
userns 可用性与降级提示；非 Android 平台折叠为一行 `[跳过]`，不影响既有输出。
等级判定与检索建议见 [android-verify.md](android-verify.md) 第 2 节。

---

## 9. 相关文档

- [AGENTS.md](../AGENTS.md) — Android 支持策略与项目约定
- [android-verify.md](android-verify.md) — Android 真机逐项验证手册（命令 + 期望输出 + 排查）
- [verify-root.sh](verify-root.sh) — Linux root 真机验证脚本（A–J，licore 全功能基线）
- [test-report-v0.6.0.md](test-report-v0.6.0.md) — 历史真机验收报告
