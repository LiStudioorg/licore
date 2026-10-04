# 未验证清单

本清单记录**代码已完成、但尚未在真机（真实 root 服务器 / 真实容器）验证**的项。

存在的意义：单元测试通过 ≠ 真机可用。本项目的开发沙箱是**无特权环境**
（`CapEff=0`、user namespace 被禁、无法创建容器），很多路径在那里根本
跑不起来。把"单测绿了"当成"功能验证过了"，是这类项目最容易犯、代价也最大
的错误。因此凡是有这种落差的项，都在这里显式登记。

**约定**：某一行被真机验证后，从这里**删除**，并把结果记进对应的验收报告
（如 `docs/test-report-*.md`），而不是留在这里打勾。

---

## 安全修复（v0.7.7 开发中）

对照 GitHub 上的外部安全审计：容器此前无 capability 隔离、无 seccomp。
修复分三批：P0-1 no_new_privs、P0-2 capability 裁剪、P1 seccomp 黑名单。

| 项 | 代码 | 沙箱内验证 | 真机验证 |
| --- | --- | --- | --- |
| P0-1 `PR_SET_NO_NEW_PRIVS` | ✅ | ✅ 已验（标志置上 + 跨 execve 继承）| ❌ 未验 |
| P0-2 capability 裁剪 | ✅ | ⚠️ 仅纯逻辑与 ABI 已验 | ❌ **未验** |
| P1 seccomp 黑名单 | ✅ | ✅ 已验（真实安装 + 真实拦截）| ❌ 未验 |

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
- 已知覆盖缺口（代码注释里也写了）：`bpf`、`userfaultfd`、`kcmp`、
  `process_vm_readv/writev`、`open_by_handle_at`、`name_to_handle_at`、
  `kexec_file_load`、`finit_module`、`clock_adjtime` 未列入黑名单——
  标准库不导出这些 `SYS_*` 常量，手写各架构号的风险高于收益。

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

## 已知缺口（不是"未验证"，是**已确认未修**）

### `licore exec` 不继承容器的权限裁剪

`licore exec` 由宿主侧的 `nsenter` 拉起（`internal/execns`），继承的是
**宿主 root 的完整能力**，与容器 init 被裁剪成什么样无关。

后果：`licore exec <容器> /bin/sh` 进去拿到的仍是全能力，因此：

- 不要用 `licore exec ... grep CapEff` 来判断权限修复是否生效（会读到宿主位图，误判为"没修好"）；
- 通过 `exec` 进入容器的进程不受 capability 裁剪与 seccomp 过滤器的约束。

**这不在本次修复范围内**，属于独立缺口。修它需要让 `exec` 路径也走一遍
能力裁剪与 seccomp 安装（nsenter 之后、目标命令之前）。
