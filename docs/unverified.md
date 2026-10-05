# 未验证清单

本清单记录**代码已完成、但尚未在真机（真实 root 服务器 / 真实容器）验证**的项。

存在的意义：单元测试通过 ≠ 真机可用。本项目的开发沙箱是**无特权环境**
（`CapEff=0`、user namespace 被禁、无法创建容器），很多路径在那里根本
跑不起来。把"单测绿了"当成"功能验证过了"，是这类项目最容易犯、代价也最大
的错误。因此凡是有这种落差的项，都在这里显式登记。

**约定**：某一行被真机验证后，从这里**删除**，并把结果记进对应的验收报告
（如 `docs/test-report-*.md`），而不是留在这里打勾。

---

## 安全审计 v2 修复（v0.9.6）

对照 [docs/security-audit-v2.md](security-audit-v2.md) 的三条修复。

| 项 | 代码 | 沙箱内验证 | 真机验证 |
| --- | --- | --- | --- |
| H-1 `convert` tar 符号链接穿透 | ✅ | ✅ **已验 + 反向验证** | — （不需要真机）|
| M-1 Hub JWT 密钥可预测 | ✅ | ✅ **已验 + 反向验证** | ⏳ 待真机（伪造令牌端到端）|
| M-2 Hub JWT 无 exp 永久有效 | ✅ | ✅ **已验 + 反向验证** | — （不需要真机）|
| G112 Hub Slowloris 超时 | ✅ | ✅ 编译期 | — （不需要真机）|

**H-1 为什么不需要真机**：漏洞与修复都在纯文件系统层
（`archive/tar` + `Lstat`），不涉及 namespace/能力/挂载，沙箱内可完整复现。
反向验证已确认漏洞版本上 8/12 用例失败、修复版本 12/12 通过。

**M-1 的真机部分**：密钥强度与签名逻辑已在单元测试中完整覆盖（含伪造
令牌被拒），真机验证只剩"部署后实际调用 Hub API 伪造失败"这一端到端确认，
不是修复本身未验证。仍登记在此，以免被当成已端到端验证。

### 待真机验证的安全项（**本清单的核心**）

审计 v2 的 A 类（容器逃逸）与 B 类（网络隔离）**本次一条都未验证**——
沙箱 `CapEff=0`，无法创建容器。验证脚本已就绪：
[scripts/verify-security.sh](../scripts/verify-security.sh)

| 脚本节 | 覆盖 | 状态 |
| --- | --- | --- |
| T-1 | CapEff/NoNewPrivs/Seccomp、`/proc/sys` 只读、sysrq mask、**`/proc/mtrr`**、设备节点、seccomp 实拦、PID ns | ⏳ 未跑 |
| T-2 | `exec` 收口（位图须与容器 PID 1 一致）、`--cap-add` 被拒 | ⏳ 未跑 |
| T-3 | 容器→宿主阻断、出网、端口映射回包、LICORE-INPUT/nft | ⏳ 未跑 |
| T-4 | `exec` 的 cgroup 归置（v0.9.2 修复的回归）| ⏳ 未跑 |
| T-5 | 卷 `:ro` 真正只读 | ⏳ 未跑 |
| T-6 | **DESTRUCTIVE** sysrq 真实写入（默认跳过，须 `--unsafe`）| ⏳ 未跑 |

> 用法：`sudo bash scripts/verify-security.sh`，跑完把完整输出贴回审计会话。

---

## 安全修复（v0.9.0，真机已验证）

对照 GitHub 上的外部安全审计：容器此前无 capability 隔离、无 seccomp。
修复分三批：P0-1 no_new_privs、P0-2 capability 裁剪、P1 seccomp 黑名单。

| 项 | 代码 | 沙箱内验证 | 真机验证 |
| --- | --- | --- | --- |
| P0-1 `PR_SET_NO_NEW_PRIVS` | ✅ | ✅ 已验 | ✅ **已真机验证** |
| P0-2 capability 裁剪 | ✅ | ⚠️ 仅纯逻辑与 ABI | ✅ **已真机验证** |
| P1 seccomp 黑名单 | ✅ | ✅ 已验 | ✅ **已真机验证** |
| exec 收口（bind helper） | ✅ | ⚠️ 部分 | ✅ **已真机验证** |
| `/proc/sys` 只读 + sysrq mask | ✅ | ✅ 已验（单测） | ✅ **已真机验证**（2026-10-05） |
| exec 进程归入容器 cgroup | ✅ | ✅ 已验（单测） | ✅ **已真机验证**（2026-10-05） |

> **2026-10-05 真机补测发现并修复 P0**：容器内可写穿宿主 `/proc/sysrq-trigger`、
> `/proc/sys/kernel/core_pattern`、`/proc/sys/kernel/modprobe`。根因是这三个文件
> 的内核 handler 走 `proc_dostring`、**不做 `capable()` 检查**，只依赖 DAC，
> 而容器 init 是真正的宿主 uid 0 —— capability 裁剪与 seccomp 对它们均无效。
> 已改为 `/proc/sys` 整体只读重挂 + 单独 mask `/proc/sysrq-trigger`，
> 真机复测三条全部 `denied`。详见 [docs/escape-audit.md](escape-audit.md) §2.A。

> **2026-10-05 修复 exec 绕过资源限额（v0.9.2）**：`licore exec` 的进程原先
> 落在调用者（CLI）的 cgroup（`user.slice/...session-N.scope`），而非容器的
> `/licore/<id>`，因此**完全绕过 `--memory` / `--pids-limit`**。实测容器限额
> 256 MiB，exec 进去的进程吃到 400 MiB 也不被拦。修复采用
> `clone3(CLONE_INTO_CGROUP)`（原子，无竞态）+ 写 `cgroup.procs` 回退；
> 真机复测 `cat /proc/self/cgroup` → `0::/licore/<CID>`，
> 且 exec 进程的内存被计入并压在上限内。详见 §2.I 的 I8。

> **2026-10-04 真机验证通过**（Linux 服务器，root）。容器 PID 1 与 exec 进程均为
> `CapEff=00000000a80425fb`（不含 CAP_SYS_ADMIN）、`NoNewPrivs=1`、`Seccomp=2`；
> 裸命令名启动、workdir、helper 拒绝与目录访问等行为均符合预期。
> 下面保留逐项说明，作为「当初为什么无法在沙箱验证」的记录。

### 逐项说明

**P0-1 `PR_SET_NO_NEW_PRIVS`**

- 沙箱内已验：`setNoNewPrivs()` 确实把 `/proc/self/status` 的 `NoNewPrivs`
  置为 1，且该标志跨 `execve` 继承（子进程重执行后仍为 1）。
- **未验**：真实容器的 init 进程是否走到了这一步。沙箱无法创建容器，
  因此 `executeContainerCmd` 的接线没有被端到端执行过。

**P0-2 capability 裁剪（风险最高的未验证项）**

- 沙箱内已验：默认集精确等于 Docker 默认集（14 项）且不含任何危险能力；
  `--cap-add` / `--cap-drop` / `drop ALL` 语义；`capHeader` / `capData`
  的结构偏移与内核 ABI 一致；`capget` 读回值与 `/proc/self/status` 一致。
- **未验**：真实调用 `prctl(PR_CAPBSET_DROP)` 与 `capset` 的效果。
  原因：`PR_CAPBSET_DROP` 要求有效集里有 `CAP_SETPCAP`，而沙箱 `CapEff=0`，
  调用只会返回 `EPERM`。相关用例因此**显式 skip**（不是静默通过）。
- 换句话说：**"容器真的丢了 CAP_SYS_ADMIN"这条目前只有逻辑与单测支撑，
  没有真机证据。**
- 风险上限：最坏情况是裁剪未生效，容器行为**不比修复前更差**——但这意味着
  审计指出的三条攻击路径（重启宿主、加载 eBPF、改宿主 `/proc/sys`）仍然成立。

**P1 seccomp 黑名单**

- 沙箱内已验（比 P0-2 强）：过滤器**真的装上并真的拦截**——
  子进程内 `Seccomp` 模式为 2，`unshare(0)` 从"成功"变为 `EPERM`；
  同时基础文件操作未被误伤（验证黑名单没有拦太宽）。
  安装过滤器只需 `no_new_privs`，不需要任何 capability，故这一项在沙箱内可验。
- **未验**：容器内端到端生效（同样受限于无法创建容器）。
- 已知覆盖缺口（代码注释里也写了）：`bpf`、`userfaultfd`、`open_by_handle_at`、
  `name_to_handle_at`、`kexec_file_load`、`finit_module`、`clock_adjtime`
  未列入黑名单；`clone` 只拦 `CLONE_NEWUSER`，未拦 `CLONE_NEWPID/NEWNS/NEWNET`
  等标志组合——这些调用都需 `CAP_SYS_ADMIN`（已丢）或 `CAP_BPF`/`CAP_SYS_MODULE`
  等同样已被丢弃的能力，属纵深防御第二层的不完整，而非可直接利用的缺口。
- **已覆盖（2026-10-05 安全专项补齐）**：`process_vm_readv`、`process_vm_writev`、
  `kcmp` 三者在 v0.9.0 时仍属缺口，现已加入黑名单。编号按架构分文件
  （`seccomp_vmproc_*_linux.go`）：arm64/riscv64 复用 `syscall.SYS_*`，
  x86 与 arm32 因标准库不导出而手写（x86 的 309/310/311 已用运行时实测校验）。
  补这三条的理由：`ptrace` 有无条件兜底，而这三条原先没有——用户一旦
  `--cap-add SYS_PTRACE`，它们会立刻失去唯一防护。
  回归测试：`internal/runtime/seccomp_vmproc_linux_test.go`。
  **仍未真机验证**（沙箱内无容器），因此本条从"缺口"变为"已实现但未真机验证"。


### 建议的顺手验证（首次跑容器时，约 10 秒）

不必专门跑验证脚本。第一次用 `licore run` 起容器时，顺手读一下容器
**PID 1** 的权限状态即可：

```bash
licore run -d --name captest alpine:3.20 sleep 3600

CID=$(licore ps -q | head -1)
INITPID=$(sed -n 's/.*"initPid"[[:space:]]*:[[:space:]]*\([0-9]\{1,\}\).*/\1/p' \
  ~/.licore/containers/$CID/runtime.json)
grep -E '^(CapEff|NoNewPrivs|Seccomp):' /proc/$INITPID/status

licore stop captest && licore rm captest
```

期望输出：

```
CapEff:	00000000a80425fb     ← Docker 默认集；不含 CAP_SYS_ADMIN(bit 21)
NoNewPrivs:	1                    ← P0-1 生效
Seccomp:	2                    ← P1 生效（2 = filter 模式）
```

任何一项不符，就是上表对应项在真机上没生效。

**注意**：一定要读**容器 PID 1**，不要用 `licore exec` 进去读——
`exec` 走宿主侧 `nsenter`，继承的是宿主 root 的完整能力，读到的是宿主位图
（见下节）。

完整的自动化验证脚本：`scripts/verify-capabilities.sh`（以 root 运行，
默认跳过有风险的 sysrq 测试，需 `--unsafe` 显式开启）。
该脚本**至今未在真机运行过**。

---

## `licore exec` 的收口（已实现，未真机验证）

**背景**：`licore exec` 走宿主侧的 `nsenter`，其进程继承的是**宿主 root 的
完整能力**——容器 init 里的 no_new_privs / cap-drop / seccomp 对它完全无效。
修之前，任何能执行 `licore exec` 的人都能拿到宿主 root 的全部能力，
让三层隔离形同虚设。

**现状（已修）**：容器启动时把 licore 自身**只读 bind** 到容器内
`/.licore/exec-helper`；exec 时 nsenter 在容器内执行它，由它做完收口
（no_new_privs → cap-drop → seccomp）再 execve 用户命令。

为什么用 bind 而不是 `/proc/self/fd`：后者依赖容器内有 procfs 且
`/proc/self/fd` 可读，Android/Magisk 环境下 procfs 参数与可见性不确定；
bind 进来的路径在容器内**一定**可达，跨平台更稳。代价是每个容器 rootfs
多一个只读文件。

**能力语义（安全边界）**：

- exec 默认继承**容器创建时**记录的 `--cap-drop`，可再叠加本次 `--cap-drop`；
- exec **拒绝 `--cap-add`**——允许它等于给隔离开后门。需要更多能力时用
  `licore run` 起一个配置正确的新容器。

### 未验证的部分

| 项 | 沙箱内验证 | 真机验证 |
| --- | --- | --- |
| helper 参数解析 / 收口规格编解码 | ✅ 已验 | — |
| 只读 bind（helper 落进 rootfs 且不可写） | ⚠️ 需 root，显式 skip | ✅ **已真机验证**（`ls -la /.licore/` 可见只读 exec-helper）|
| exec 收口后 NoNewPrivs=1、Seccomp=2 | ✅ 已验（子进程真跑） | ✅ **已真机验证** |
| seccomp 确实拦住危险调用 | ✅ 已验（unshare 探针） | ⚠️ 未直接验证（结论由 CapEff 无 CAP_SYS_ADMIN 间接闭合）|
| capability 真实裁剪 | ❌ 需 CAP_SETPCAP | ✅ **已真机验证** |
| 容器内 helper 路径真的可达（端到端） | ❌ 无法起容器 | ✅ **已真机验证** |

**sysrq 写测试刻意未执行**：写 `/proc/sysrq-trigger` 需要 `CAP_SYS_ADMIN`，
而真机 `CapEff` 已确认不含该位，内核必然拒绝——逻辑已闭合，不做破坏性验证。

### 顺手验证（有 root 服务器时）

```bash
licore run -d --name captest alpine:3.20 sleep 3600
licore exec captest /bin/sh -c 'grep -E "^(CapEff|NoNewPrivs|Seccomp):" /proc/self/status'
licore stop captest && licore rm captest
```

修好之后，exec 进程读到的应当**也是**裁剪后的位图：
`CapEff=00000000a80425fb`（不含 `CAP_SYS_ADMIN`）、`NoNewPrivs: 1`、`Seccomp: 2`。

> 注意：这与修复前正好相反——修复前 `licore exec ... grep CapEff` 读到的是
> **宿主**的满能力位图，那正是"exec 未收口"的症状。

