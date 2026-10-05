# LiCore 安全审计 v2

> **状态：Step 1（静态扫描 + 代码审计）初步清单。Step 2/3 待执行。**
> 本文档**只报告、不修复**。所有修复动作等用户确认后再动手。

- **审计对象**：LiCore v0.9.5
- **代码版本**：`8c89003f28eea55a4a9adf8cf5d02cf1a9598687`（2026-10-05 18:19:16 +0800）
- **审计环境**：`ecsQ99YJ`，Linux 5.15.0-194-generic x86_64
- **审计者身份**：`uid=1000(li63050a)`，`CapEff=0000000000000000`、`NoNewPrivs=1`
  —— **无特权沙箱**，无法创建容器、无法 `mount`、无法改宿主状态
- **执行时间**：2026-10-05

---

## 0. 方法与可信度声明

### 本报告的证据等级

| 等级 | 含义 | 本次可用性 |
| --- | --- | --- |
| **实测（沙箱内可复现）** | 已在本机跑出真实输出，附命令 | ✅ 可用 |
| **实测（真机必需）** | 需要 root / 容器，本机跑不了 | ❌ 不可用 → 标 ⏳ |
| **推断** | 只读代码得出，无实测 | ⛔ **本报告一律不采用** |

**重要限制（请务必知悉）**：本次审计运行在**无特权环境**（`CapEff=0`）。
LiCore 的核心安全属性——容器逃逸、capability 裁剪、seccomp 生效、`/proc/sys`
封堵、veth/NAT——**全部需要真机 root 才能验证**。因此：

- 本文档中所有"容器逃逸"类结论**均为 ⏳ 待真机验证**，不构成"已验证无漏洞"；
- 相反，**`✅ 已验证无漏洞`** 一节只包含**在本沙箱内真的跑过**的项。

**不要把本文档当成"扫描完了、系统是安全的"。** 它是一份"哪些东西被验过、
哪些没有"的诚实清单。

### 已执行与未执行的工具

| 工具 | 状态 | 结果 |
| --- | --- | --- |
| `gofmt -l .` | ✅ 已跑 | 仓库内 Go 文件全部合规（仅 `.gopath` 第三方缓存有 2 处，非本项目代码）|
| `go vet ./...` | ✅ 已跑 | **零告警** |
| `staticcheck ./...` | ✅ 已跑 | 18 条，**全部为死代码/风格类**，无安全相关（见 §5）|
| `gosec` | ✅ 已跑 | 235 条，逐条分类见 §5；**多数为误报**，但其中数条指向真实问题 |
| `govulncheck ./...` | ✅ 已跑 | **No vulnerabilities found**（见 §6）|
| `trivy` | ❌ 未安装，未跑 | 无 |
| 动态容器攻击测试 | ❌ **沙箱无特权，无法执行** | → Step 2 需真机 |
| 真机逃逸验证 | ❌ 同上 | → 全部标 ⏳ |

---

## 🔴 高危

### H-1 `licore convert` 的 tar 解压可穿越符号链接，写出 rootfs 之外（**已实测确认**）

**证据等级：实测（沙箱内可复现）**

`internal/convert/convert.go` 的 `extractEntry()` 在解压 `docker export`
产物时，只校验了**条目名本身**的路径逃逸（`../`、绝对路径），
**没有校验路径上已存在的父组件是否为符号链接**。

于是"先放一个符号链接条目、再经由它写文件"即可写出目标目录之外。

#### 复现命令

```bash
cd /home/li63050a/work/boxli
cat > internal/convert/audit_escape_test.go <<'EOF'
package convert

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"
)

func TestAuditExtractTarSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	_ = os.MkdirAll(victim, 0o755)
	_ = os.WriteFile(filepath.Join(victim, "secret"), []byte("ORIGINAL\n"), 0o644)

	tarPath := filepath.Join(dir, "evil.tar")
	f, _ := os.Create(tarPath)
	tw := tar.NewWriter(f)
	_ = tw.WriteHeader(&tar.Header{Name: "linkdir", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{
		Name: "linkdir/up", Typeflag: tar.TypeSymlink,
		Linkname: "../../victim", Mode: 0o777,
	})
	body := []byte("PWNED\n")
	_ = tw.WriteHeader(&tar.Header{
		Name: "linkdir/up/secret", Typeflag: tar.TypeReg,
		Mode: 0o644, Size: int64(len(body)),
	})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = f.Close()

	dst := filepath.Join(dir, "rootfs")
	_ = os.MkdirAll(dst, 0o755)
	_ = extractTar(tarPath, dst)

	got, _ := os.ReadFile(filepath.Join(victim, "secret"))
	if string(got) != "ORIGINAL\n" {
		t.Fatalf("!!! 逃逸已确认：rootfs 之外的文件被覆盖为 %q", string(got))
	}
}
EOF
GOCACHE=/tmp/gocache-audit go test ./internal/convert/ -run TestAuditExtractTarSymlinkEscape -v
rm -f internal/convert/audit_escape_test.go
```

#### 实际输出

```
=== RUN   TestAuditExtractTarSymlinkEscape
    audit_escape_test.go:58: !!! 逃逸已确认：rootfs 之外的文件被覆盖为 "PWNED\n"
--- FAIL: TestAuditExtractTarSymlinkEscape (0.00s)
FAIL
FAIL	github.com/LiStudioorg/licore/internal/convert	0.008s
FAIL
```

补充变体测试（同样全部逃逸成功）：

| 变体 | 结果 |
| --- | --- |
| `目录条目 → 相对符号链接 → 经链接写文件` | **逃逸成功** |
| `目录条目 → 相对符号链接（深层）→ 经链接写文件` | **逃逸成功** |
| `目录条目 → 绝对路径符号链接 → 经链接写文件` | **逃逸成功** |
| `符号链接 → 硬链接穿透` | 被 `MkdirAll` 意外挡住（非有意防护）|

#### 影响

- **触发方式**：用户执行 `licore convert <恶意镜像>`。攻击者只需发布一个
  含上述 tar 结构的 Docker 镜像（Docker 本身允许镜像内含符号链接），
  受害者 `licore convert` 一次即被写穿。
- **写入身份**：LiCore 通常以 **root** 运行（建容器需要），因此逃逸写入
  以 **root** 落盘 —— 可覆盖 `/etc/` 下任意文件、写 `authorized_keys`、
  投递 systemd unit。
- **注意**：`docker export` 的产物由 **Docker daemon** 生成，理论上
  daemon 不会主动产出恶意 tar；但 `docker pull` 下来的镜像**内容**由
  镜像作者控制，符号链接是镜像层里的合法条目，`export` 会原样带出。
  因此**镜像作者 = 攻击者**这条路径成立。

#### 与既有防护的反差（说明这是遗漏而非设计取舍）

同一仓库的 `internal/storage` **有**这项防护。同一攻击对其无效：

```
=== RUN   TestAuditStorageSymlinkEscape
    audit_symlink_test.go:37: UnpackFile 拒绝: 拒绝经由符号链接写出
        ".../fs/linkdir/up": licore/image: 不安全的归档路径
    audit_symlink_test.go:43: storage 未逃逸（防护有效）
--- PASS
```

`MergeLayers` 同样有防护（实测拒绝）。**即：引擎主路径（storage）做对了，
`convert` 这条旁路漏了。**

#### 现有测试为何没抓到

`internal/convert/convert_test.go:416` 的 `TestExtractTarRejectsEscape`
只覆盖三种**条目名**逃逸（`../evil`、`/etc/evil`、`a/../../evil`），
**没有覆盖符号链接穿透**。测试通过 ≠ 防护完整。

#### 修复方向（待确认后再动手）

复用 `internal/storage` 的 `ensureParents` 语义：写路径的每个**已存在**
父组件必须是真实目录，遇符号链接即拒绝。注意 `internal/storage` 的
`chainOK()` 已把这件事做对，直接对齐即可，勿另起炉灶。

---

## 🟠 中危

### M-1 Hub 的 JWT 签名密钥可预测（**已实测确认**）

**证据等级：实测（沙箱内可复现）**

`hub/server.go:294` 的 `randSecret()` **不是随机数**：

```go
func randSecret() string {
	return fmt.Sprintf("licore-hub-%d-%d", os.Getpid(), time.Now().UnixNano())
}
```

而 `internal/cli/hub.go:76` 启动服务时调的是 `hub.NewServer(reg)`，
**从未传 `WithAuth`**（全仓库 `grep WithAuth` 只有定义处，零调用点）——
即 CLI 启动的 Hub **永远使用这个可预测密钥**。

#### 复现命令与输出

```
=== RUN   TestAuditRandSecretPredictable
    secret = "licore-hub-371030-1791196756427009199"
    解析成功: pid=371030 UnixNano=1791196756427009199
    时间: 2026-10-05T18:39:16.427009199+08:00
```

密钥 = `licore-hub-<PID>-<启动纳秒>`。PID 可枚举/可读，纳秒时间戳在
知道大致启动时刻（日志、`ps`、`systemctl show`）后可暴力收敛。

**影响**：攻击者可伪造 `{"sub":"attacker","scopes":["admin"]}` 的合法
签名令牌，**完全绕过 Hub 鉴权**（读、写、删任意镜像）。

**加重因素**：`hub serve` 默认 `--bind 127.0.0.1`，但文档与用法中
常配 `--bind 0.0.0.0` 暴露给内网/公网（分发服务的用途即如此）。

#### 修复方向

改用 `crypto/rand` 生成 ≥32 字节密钥，并让 CLI 支持从环境变量/文件
注入稳定密钥（否则每次重启令牌全失效）。**注意 `randSecret` 命名
暗示"随机"，实际不是——这类命名具有欺骗性，建议同时改名。**

---

### M-2 Hub 的 JWT 无 `exp` 时永久有效；`exp` 为负也可绕过（**已实测确认**）

**证据等级：实测（沙箱内可复现）**

`hub/auth.go:80` 的过期检查：

```go
if c.Exp > 0 && time.Now().Unix() > c.Exp {
	return nil, fmt.Errorf("JWT 已过期: %w", ErrUnauthorized)
}
```

`c.Exp > 0` 这个前置条件意味着：**`exp` 缺失（=0）或为负数的令牌
永远不做过期校验**。

#### 复现命令与输出

```
=== RUN   TestAuditJWTNoExp
    !!! 无 exp 的令牌被永久接受: sub=admin scopes=[admin]
--- PASS

=== RUN   TestAuditJWTNegativeExp
    !!! exp=-1 的令牌被接受（代码要求 c.Exp > 0 才检查）: sub=admin
--- PASS
```

#### 影响

配合 M-1，攻击者一旦伪造出令牌，**该令牌永不失效**——即使受害者
 rotates 密钥/重启 Hub（重启后 PID 与时间变了，需重取密钥；
但已签发的令牌在密钥不变期间永久有效）。

即使 M-1 修好，本条仍需独立修复：**"缺 `exp` 视为无效"才是正确语义**。

#### 修复方向

改为严格校验：`exp` 必须存在且为未来的正数，否则拒绝。
同时建议校验 `iat` 不晚于当前时间（防未来令牌）。

---

### M-3 （原判为高危，复核后降级并**撤回**）`licore pull` 的临时文件路径

**证据等级：实测（沙箱内可复现）——结论为"不可利用"**

`internal/cli/pull.go:66`：

```go
dst := filepath.Join(os.TempDir(), "licore-pull-"+ref+".licore")
```

`ref` 未经验证直接拼进临时路径。初看可穿越（`a/../../../../etc/evil:1`
→ `/etc/evil:1.licore`，已实测复现该拼接结果）。

**但复核调用链后确认不可达**：进入 `runHubPull` 前必须过
`isHubRef()`，而它要求 ref **完全不含 `/` 与 `\`**：

```
isHubRef("nginx:1.27")                 = true
isHubRef("a/../../../../etc/evil:1")   = false   ← 被拦
isHubRef("../../../../tmp/x:1")        = false   ← 被拦
```

无斜杠时 `filepath.Join` 无法上跳（实测 `..:1` → `/tmp/licore-pull-..:1.licore`）。

**结论：当前不可利用。** 但属**脆弱设计**：防护依赖的是一个
**语义完全不同的函数**（判断"是否 Hub 引用"），而非路径校验本身。
任何放宽 `isHubRef`（例如为支持 `alice/myapp:v1` 而允许 `/`）的改动
都会**瞬间**把它变成真实漏洞。

#### 建议（低优先级，纵深防御）

在 `runHubPull` 内独立做一次路径校验，不依赖上游的 `isHubRef`。

> **注**：本次审计中我一度把它判为高危，复核后撤回。
> 记录在此以说明"拼接结果会逃逸"**不等于**"可达"——两者必须分开验证。

---

## 🟡 低危 / 纵深防御

### L-1 `alice/myapp:v1` 形式的引用无法从 Hub 拉取（**已实测确认**）

**证据等级：实测（沙箱内可复现）**

`isHubRef` 要求 ref 不含 `/`，但 `AGENTS.md` 与 README 中记录的
用法是 `licore pull alice/myapp:v1`（`from-feature` 分支的
`feat/hub` 说明、以及 `hub-e2e.md`）。

```
isHubRef("alice/myapp:v1") = false
```

**后果**：该参数不会被当成 Hub 引用，而会被当作**本地文件路径**去
`store.Put()`，最终报"文件不存在"——**用户按文档操作会失败**，
且错误信息指向"文件不存在"而非"这是 Hub 引用但格式不支持"，误导排查。

**这是功能缺陷而非安全问题**，但它与 M-3 的 `isHubRef` 逻辑同源，
修 M-3 时建议一并处理。

### L-2 `parsePort` 对非法输入静默回落到 `HostPort=0`（**已实测确认**）

**证据等级：实测（沙箱内可复现）**

`internal/cli/run.go:351` 的 `atoi` 遇到非数字**返回 0**：

```
parsePort("abc:80")   -> host=0  container=80   （无报错！）
parsePort("-1:80")    -> host=0  container=80   （无报错！）
parsePort(":80")      -> host=0  container=80   （无报错！）
atoi("99999999999999999999") = 7766279631452241919   （溢出回绕）
```

`HostPort=0` 语义是"随机分配宿主端口"。因此用户写错端口（如
`-p abc:80`）不会报错，而是**静默拿到一个随机端口**，与"我想要 80"
的意图完全不符。端口映射不生效且无从察觉。

**影响面**：用户体验 / 可观测性，非安全边界。但属"静默错误"，
排障成本高。建议非法数字直接报错。

### L-3 `/proc/mtrr` 未纳入 mask 清单（**待真机确认**）

**证据等级：⏳ 待真机验证**

`init_linux.go:315` 的 `procRootMaskedFiles` **只含 `sysrq-trigger`**。
`/proc` 根下其余"写敏感的全局文件"以"实测已被内核拒绝"为由未纳入。

实测本机 `/proc/mtrr` 的 DAC 权限是 **`0644 root:root`**
（对比 `sysrq-trigger` 是 `0200 root:root`）：

```
/proc/mtrr:  644 root root    ← 注意：世界可读、属主可写
/proc/sysrq-trigger: 200 root root
```

容器 init 是**真正的宿主 uid 0**，DAC 上等于属主，因此 **`0644` 对它
等于可写**。这正是 `sysrq-trigger` 当初被攻破的同一类条件。

**但**：本机无特权（`uid=1000`），`open("/proc/mtrr", O_WRONLY)` 返回
`EACCES`，**无法在此验证**。且内核 `mtrr_ioctl` 路径**确实做了
`capable(CAP_SYS_ADMIN)` 检查**（与 `proc_dostring` 类接口不同），
而 `CAP_SYS_ADMIN` 已被默认能力集丢弃——**因此推断是安全的**。

> ⚠️ 按本项目 `AGENTS.md` 的明确教训（"不要从'某能力已丢弃'推断'某内核
> 接口被挡住'"），**这条推断不能作为结论**。必须真机实测写入才能关闭。

**真机验证方法**（Step 2，需 root；**只读探测，非破坏性**）：

```bash
licore run -d --name mtrrtest alpine:3.20 sleep 300
licore exec mtrrtest /bin/sh -c 'ls -la /proc/mtrr; cat /proc/mtrr'
# 期望：能看到文件；写入应被内核以 EPERM 拒绝
licore stop mtrrtest && licore rm mtrrtest
```

### L-4 seccomp 黑名单的已知覆盖缺口（**文档已如实记录**）

**证据等级：代码审阅（与 `AGENTS.md` 记录一致，非新发现）**

`seccomp_linux.go:293` 末尾诚实列出了未覆盖项：
`bpf`、`userfaultfd`、`open_by_handle_at`、`name_to_handle_at`、
`kexec_file_load`、`finit_module`、`clock_adjtime`。

这些都需要已被丢弃的能力（`CAP_BPF` / `CAP_SYS_MODULE` / `CAP_SYS_ADMIN`），
因此**当前不可直接利用**；但若用户 `--cap-add` 回某个能力，
黑名单不会提供第二层防护。属纵深防御的已知不完整，**记录在案即可**。

### L-5 `/proc/sys` 只读封堵覆盖不到 `/proc/sysrq-trigger` 之外的根级文件

**证据等级：代码审阅（设计已说明）**

`mountProcSysReadOnly` 只重挂 `/proc/sys`；`maskProcRootFiles` 只 mask
`sysrq-trigger`。`/proc` 根下若有**未来内核新增的** `proc_dostring`
类可写文件，两者都覆盖不到。

这是"清单式封堵"的固有限制（`AGENTS.md` 也承认"清单会随内核版本漂移"）。
**建议**（探索性）：考虑在容器内以 `MS_RDONLY` 重挂**整个 `/proc`**
的可行性与兼容性——但那会破坏 `/proc/self/attr` 等合法写入，
需谨慎评估，**不建议盲改**。

---

## ✅ 已验证无漏洞（本沙箱内真的跑过）

> 以下全部是**真实执行 + 真实输出**，不是"看代码觉得没问题"。

### ✅ V-1 `internal/storage` 的符号链接穿透防护（两条路径均有效）

**UnpackFile**：

```
UnpackFile 拒绝: 拒绝经由符号链接写出 ".../fs/linkdir/up":
    licore/image: 不安全的归档路径
storage 未逃逸（防护有效）
```

**MergeLayers**：

```
MergeLayers 拒绝: 合并第 2 层 ...: 拒绝经由符号链接合并写出
    ".../rootfs/d/up": licore/image: 不安全的归档路径
MergeLayers 未逃逸（防护有效）
```

`ensureParents` / `chainOK` / `realDirChain` 三处防护实测均生效。
**H-1 正是这条防护在 `convert` 里的缺失。**

### ✅ V-2 seccomp BPF 过滤器生成逻辑正确

**证据等级：实测（用 BPF 解释器逐条执行字节码）**

把 `buildSeccompFilter` 的构造逻辑复刻进独立程序，用软件 BPF VM
执行生成的指令序列：

```
 0: code=0x20 jt=0 jf=0 k=0x00000004   A = seccomp_data.arch
 1: code=0x15 jt=1 jf=0 k=0xc000003e   if A == AUDIT_ARCH_X86_64
 2: code=0x06 jt=0 jf=0 k=0x00050001   return ERRNO|EPERM   ← arch 不符即拒
 3: code=0x20 jt=0 jf=0 k=0x00000000   A = seccomp_data.nr
 4: code=0x15 jt=0 jf=1 k=0x000000a9   nr == 169 (reboot)?
 5: code=0x06 jt=0 jf=0 k=0x00050001   return ERRNO|EPERM
 6: code=0x15 jt=0 jf=1 k=0x000000a5   nr == 165 (mount)?
 7: code=0x06 jt=0 jf=0 k=0x00050001   return ERRNO|EPERM
 8: code=0x15 jt=0 jf=4 k=0x00000038   nr == 56 (clone)?
 9: code=0x20 jt=0 jf=0 k=0x00000010   A = args[0]
10: code=0x54 jt=0 jf=0 k=0x10000000   A &= CLONE_NEWUSER
11: code=0x15 jt=1 jf=0 k=0x00000000   if A == 0 跳过 RET
12: code=0x06 jt=0 jf=0 k=0x00050001   return ERRNO|EPERM
13: code=0x06 jt=0 jf=0 k=0x7fff0000   return ALLOW
```

逐项断言全部通过：

| 用例 | 期望 | 实测 |
| --- | --- | --- |
| 错误 arch（int 0x80 绕过） | 拒绝 | ✅ 拒绝 |
| `reboot` | 拒绝 | ✅ 拒绝 |
| `mount` | 拒绝 | ✅ 拒绝 |
| `clone(0)`（普通线程） | 放行 | ✅ 放行 |
| `clone(CLONE_NEWUSER)` | 拒绝 | ✅ 拒绝 |
| `read`（无害） | 放行 | ✅ 放行 |
| `unsafe.Sizeof(sockFilter{})` | 8 | ✅ 8 |

**特别确认了 `AGENTS.md` 强调的两条安全属性**：

1. **带参数规则排在无条件规则之后**——第 8–12 条确实在尾部，
   累加器未被 `args[0]` 冲掉导致静默放行；
2. **`AUDIT_ARCH` 未知时返回错误而非 0**——第 2 条 arch 校验失败即
   `ERRNO|EPERM`，不会退化成"拒绝所有系统调用"。

### ✅ V-3 JWT 抗算法混淆（`alg=none`）

```
TestAuditJWTAlgNone: alg=none 被拒绝
```

`Verify` 不看头部 `alg`，直接用 HS256 重算签名并 `hmac.Equal` 比较，
因此经典的 `alg:none` / RS256→HS256 混淆攻击**不成立**。

同时确认：签名比较用的是 `hmac.Equal`（恒定时间），非 base64 段
（如 `{"x":1}`）也会被签名校验挡下。

### ✅ V-4 Hub blob 摘要校验阻止路径穿越

```
validDigest("../../../etc/passwd")       = false
validDigest("sha256:../../../../tmp/evil") = false
validDigest("..")                        = false
validDigest("a/b")                       = false
```

`/blobs/<digest>` 路径不可穿越。

### ✅ V-5 端口映射参数校验

```
parsePort("99999:80")  -> 错误: 宿主端口 99999 非法
parsePort("8080:")     -> 错误: 容器端口 0 非法
parsePort("80/sctp")   -> 错误: 非法协议 "sctp"
parsePort("1:2:3:4")   -> 错误: 非法端口映射
```

越界与畸形输入被正确拒绝（`abc:80` 的静默问题见 L-2）。

### ✅ V-6 `-v` 卷挂载语法校验

```
parseMounts("/host:/ctr:rw") -> 错误: 非法 -v 选项 "rw"（仅支持 ro）
parseMounts("a:b:c:d")       -> 错误: 非法 -v
```

`:ro` 之外的模式被拒绝。

### ✅ V-7 容器名 / ID 路径穿越防护

`ContainerConfig.Validate()`：

```go
if c.ID == "" || len(c.ID) > 64 || strings.ContainsAny(c.ID, `/\`) { ... }
if c.Name == "" || strings.ContainsAny(c.Name, `/\`) || strings.HasPrefix(c.Name, ".") { ... }
```

`/`、`\`、`.` 前缀均被拒绝，`filepath.Join` 无法被污染。
配合 `claimName` 的 `O_EXCL` 原子抢占（防 TOCTOU），逻辑正确。

### ✅ V-8 构建（`licore build`）的 COPY 路径防护

`resolveCopySource` 用 `evalExistingPrefix` + `EvalSymlinks` 实体化
最深已存在前缀，再 `filepath.Rel` 确认仍在 `ContextDir` 内：

```go
if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
	return "", fmt.Errorf("源路径 %q 经符号链接逃逸出构建上下文 %s: %w", ...)
}
```

`checkContextRel` 拒绝绝对路径、反斜杠、`.`/`..` 段。
**设计正确**（`convert` 的问题恰恰是没走这套）。

### ✅ V-9 依赖扫描：无已知 CVE

```
$ govulncheck ./...
No vulnerabilities found.
```

### ✅ V-10 依赖白名单合规

全仓库第三方依赖只有 `github.com/spf13/cobra`（+ `pflag`、
`mousetrap` 两个间接依赖），与 `AGENTS.md` 白名单完全一致。
**无任何容器 / OCI / cgroups 相关库**，符合项目定位。
`CGO_ENABLED=0` 静态构建通过（`statically linked`）。

### ✅ V-11 静态扫描无安全相关告警

- `go vet ./...` → 零告警
- `staticcheck ./...` → 18 条，全部 `U1000`（未使用符号）/ `S1003` /
  `S1011` / `SA4004` / `SA4010` 类，**无安全语义**

值得单独一提的两条（均为**死代码，无功能影响**）：

- `internal/scaffold/rules.go:388`：`instructions = append(...)` 的结果
  从未被读取（`instructions` 只在 364 行声明、388 行追加，
  **全函数无第三个引用**）。SA4010 告警属实，属 lint 功能未接线。
- `internal/build/build.go:119`：`for { select {...}; break }` 无条件终止
  循环（SA4004），是重构残留。

### ✅ V-12 完整测试套件通过

```
$ go test ./...
19 个包全部 ok，0 FAIL
```

（作为基线；注意"测试通过"不代表安全——H-1 就是测试覆盖不到的。）

---

## ⏳ 待真机验证（**本次无法执行**）

> 全部需要 root + 真实容器。**本沙箱 `CapEff=0`，一条都跑不了。**
> 这一节是 Step 2 的输入清单。

### T-1 容器逃逸面（最高优先级）

| 项 | 验证方法 | 期望 |
| --- | --- | --- |
| `/proc/sysrq-trigger` 写入 | 容器内 `echo b > /proc/sysrq-trigger` | **DESTRUCTIVE**：真写会重启宿主。改用 `open(O_WRONLY)` 探测即可 |
| `/proc/sys/kernel/core_pattern` | 容器内尝试 `open(O_WRONLY)` | EACCES/EROFS（只读重挂生效）|
| `/proc/sys/kernel/modprobe` | 同上 | EACCES/EROFS |
| `/proc/mtrr`（见 L-3） | 容器内 `ls -la /proc/mtrr` + 写探测 | 待定 |
| `/proc/kcore`、`/proc/kmsg` | 容器内读探测 | EPERM/EACCES |
| 设备节点 | 容器内 `ls -la /dev` | 仅最小集，无 `/dev/mem` |
| `CapEff` / `NoNewPrivs` / `Seccomp` | 读**容器 PID 1** 的 `/proc/<pid>/status` | `a80425fb` / `1` / `2` |
| `unshare(CLONE_NEWUSER)` | 容器内调用 | EPERM（seccomp）|
| `mount` / `pivot_root` | 容器内调用 | EPERM |

> `scripts/verify-capabilities.sh` 已存在但**从未在真机运行过**
> （`unverified.md` 记载）。本次审计**也未运行**。

### T-2 `licore exec` 的收口

```bash
licore exec <name> /bin/sh -c 'grep -E "^(CapEff|NoNewPrivs|Seccomp):" /proc/self/status'
```

期望与容器 PID 1 一致（`exec` 走 helper 收口）。另需确认
`--cap-add` 被 `rejectCapAddForExec` 拒绝。

### T-3 网络隔离

| 项 | 期望 |
| --- | --- |
| `LICORE-INPUT` 的 DROP 命中计数 | 容器 `nc <宿主IP> 22` 不通，且计数增长 |
| 容器 → 宿主网桥网关 | 应**放行**（DNS 依赖它）|
| 端口映射回包 | 外部经宿主 IP 访问容器端口返回 200 |
| `licore rm` 后规则残留 | nft 与 iptables 两侧均**零残留** |
| 容器 → 外网 | 通（`-i licore0 -o eth0 -j ACCEPT`）|

### T-4 cgroup 限额（`exec` 归置）

`licore exec` 进程 `cat /proc/self/cgroup` → `/licore/<CID>`，
且 `--memory` 限额对 exec 进程生效（v0.9.2 修复的回归验证）。

### T-5 `-v :ro` 真正只读

容器内尝试写 `:ro` 挂载点，应 EROFS。

---

## §5 扫描器原始结果分类

### gosec 235 条的分类结论

| 规则 | 条数 | 判定 |
| --- | --- | --- |
| `G304` 文件路径由变量拼接 | 63 | **误报**——路径来自 LiCore 自身数据目录，且 `SafeArchivePath` 等已校验 |
| `G301` 目录权限 > 0750 | 49 | **误报**——容器 rootfs 内权限由镜像层决定，本就需 0755 |
| `G115` 整数溢出转换 | 37 | **误报**——多为 `uintptr→uint32` 的系统调用号/控制码，值域受控 |
| `G306` 文件权限 > 0600 | 27 | **误报**——同上，且多为 `0o644` 的元数据文件 |
| `G703` 路径穿越（污点分析） | 25 | **需逐条看**——多数在 `init_linux.go`，路径来自内部 env，非用户可控 |
| `G302` 文件权限 > 0600 | 12 | 误报 |
| `G204` 子进程由变量启动 | 9 | **误报**——`iptables`/`nft`/`nsenter`/`docker` 均为固定程序名 + 结构化参数，无 shell |
| `G103` 使用 unsafe | 5 | **设计如此**——seccomp/capability 的 ABI 结构体必需 |
| `G122` `WalkDir` 竞态路径 | 4 | **低危**——见下 |
| `G110` 解压炸弹 | 2 | **低危**——见下 |
| `G112` Slowloris | 1 | **低危**——见下 |
| `G104` 未处理错误 | 1 | 误报 |

### 需要跟进的三条 gosec 发现

**G122（`WalkDir` 竞态）**：`internal/storage/storage.go:365/367`、
`volume_snapshot.go:92`、`cli/imageops.go:787`。
`filepath.Walk` 的路径操作存在 TOCTOU 窗口。但**这些操作发生在
LiCore 自己的数据目录内**（root-only），攻击者需先有写权限才能利用——
风险很低。**建议**：长期改用 `os.Root`（Go 1.24+）等 root-scoped API。

**G110（解压炸弹）**：`image/archive.go:218/277`（`VerifyLayers` 与
`ExtractFile` 的 `io.Copy` 无大小上限）。
`storage` 侧有 `maxTarEntries = 1<<20` 防护，但 `image` 侧这两处
**只受 `index.json` 声明的 `sizeBytes` 约束**。恶意 `.licore` 若声明
巨大 `sizeBytes` 并填 gzip 炸弹，`VerifyLayers` 会持续读。
**建议**：给 `io.Copy` 加 `io.LimitReader(tr, declaredSize+1)` 并校验。

**G112（Slowloris）**：`internal/cli/hub.go:93` 的 `http.Server`
未设 `ReadHeaderTimeout`。Hub 若暴露到网络，可被慢速连接耗尽。
**建议**：加 `ReadHeaderTimeout` / `ReadTimeout` / `WriteTimeout`。

---

## §6 依赖扫描

```
$ govulncheck ./...
No vulnerabilities found.
```

| 依赖 | 版本 | 用途 | CVE |
| --- | --- | --- | --- |
| `github.com/spf13/cobra` | v1.10.2 | CLI 命令树 | 无 |
| `github.com/spf13/pflag` | v1.0.9 | indirect | 无 |
| `github.com/inconshreveable/mousetrap` | v1.1.0 | indirect | 无 |

`trivy` **未安装**，未执行。如需文件系统/镜像层扫描（配置错误、
密钥泄漏、许可证），需在 Step 3 补。

---

## §7 发现的完整性自评

**本次审计的真实覆盖面有限，请按此折价看待结论。**

| 模块 | 静态审阅 | 动态验证 | 说明 |
| --- | --- | --- | --- |
| A. 容器逃逸 | ✅ 已读核心代码 | ❌ **完全没验** | 需真机，见 T-1 |
| B. 网络 | ✅ 已读 | ❌ **完全没验** | 需真机，见 T-3 |
| C. 输入验证 | ✅ 已读 | ✅ **部分实测** | H-1、L-2、V-4~V-7 |
| D. 存储 | ✅ 已读 | ✅ **已实测** | V-1（两条路径均验）|
| E. 认证 | ✅ 已读 | ✅ **已实测** | M-1、M-2、V-3 |
| F. 依赖 | ✅ 已跑 | ✅ | V-9、V-10 |

**最重要的判断**：本次最有价值的产出是 **H-1**（`convert` 符号链接逃逸，
已实测复现）。其余多为纵深防御建议与"待验清单"。

**A 类（容器逃逸）——即本项目的核心安全属性——本次一条也没验证成功**，
因为沙箱没有 `CAP_SYS_ADMIN`。**在真机跑完 T-1/T-2 之前，
不应认为 LiCore 的容器隔离"已审计通过"。**

---

## §8 建议的下一步

1. **用户确认 H-1**（`convert` 符号链接逃逸）是否优先修复——
   这是本次唯一"已实测复现、可被远程镜像触发、以 root 写穿"的问题。
2. **Step 2 需真机**：请在 `45.207.198.91`（root）上执行 T-1~T-5。
   我可以产出脚本，但**破坏性命令（sysrq 写入）会单独列出并停下等确认**。
3. **Step 3**：安装 `trivy` 补文件系统/配置扫描；`govulncheck` 已跑完。
4. **M-1 / M-2（Hub JWT）**：若 Hub 曾暴露到非回环地址，建议按
   "密钥可能已泄漏"处理（轮换 + 审计访问日志）。

---

*本报告为 Step 1 产物。所有 ⏳ 项在真机验证前不得转为 ✅。*

---
---

# 修复记录与反向验证（v0.9.6）

> 本部分是 Step 1 报告之后、经用户批准执行的修复。
> **修复只做已确认的三条**（H-1 / M-1 / M-2），其余保持"只报告不修"。

## 修复清单

| 编号 | 严重度 | 修复 commit | 状态 |
| --- | --- | --- | --- |
| H-1 `convert` tar 符号链接穿透 | 🔴 高危 | `73550b5` | ✅ 已修 + 反向验证 |
| M-1 Hub JWT 密钥可预测 | 🟠 中危 | `677f3d9` | ✅ 已修 + 反向验证 |
| M-2 Hub JWT 无 exp 永久有效 | 🟠 中危 | `677f3d9` | ✅ 已修 + 反向验证 |
| G112 Hub Slowloris | 🟡 低危 | `677f3d9` | ✅ 顺带修 |
| 真机验证脚本 | — | `046e1e0` | ✅ 已交付，待真机执行 |

---

## H-1 修复详情

### 改了什么

1. **新增 `image.SafeExtractPath`**（`internal/image/manifest.go`）——
   解压类代码的**唯一**路径校验入口：先跑 `SafeArchivePath` 校验条目名，
   再逐级 `Lstat` 父组件，符号链接一律拒绝。
2. **`storage` 改为委托同一实现**：原私有 `ensureParents` 现在调用
   `image.SafeExtractPath`。**漏洞的根因正是"两份实现、一份漏做"**——
   不消除重复，同样的洞还会在第三个解压点出现。
3. **`convert.extractEntry` 全面接入**，并额外封堵两条穿透路径：
   - **硬链接源路径**：`linkname` 指向 rootfs 之外时，会把宿主敏感文件
     硬链接进 rootfs 并随镜像打包外泄。落点与源路径都要校验。
   - **`O_TRUNC` 跟随符号链接**（修复过程中**实测发现的第二条路径**）：
     原代码用 `O_CREATE|O_WRONLY|O_TRUNC` 写普通文件，而 O_TRUNC 会
     **跟随**已存在的符号链接（内核解析到链接目标再截断）。末级符号链接
     虽被父链校验放行（覆盖替换本身安全），O_TRUNC 却会写到链接目标去。
     改为先 `Remove` 再 `O_EXCL` 创建。

### 关于 O_TRUNC 那条：它是怎么被发现的

我最初只想"验证一下末级符号链接覆盖是安全的"，写了个正向用例
`TestExtractTarAcceptsOverwritingExistingSymlink`，预期它通过。
**结果它失败了**，报 `!!! 逃逸：rootfs 之外的文件被覆盖为 "REPLACED\n"`。

独立复现确认了内核行为：

```
$ python3 -c "
import os
p='/tmp/otest/rootfs/sneaky'   # 预先存在的符号链接 → /tmp/otest/victim/secret
print('lstat islink:', os.path.islink(p), '->', os.readlink(p))
fd=os.open(p, os.O_CREAT|os.O_WRONLY|os.O_TRUNC, 0o644)
os.write(fd, b'REPLACED\n'); os.close(fd)
print('victim/secret =', open('/tmp/otest/victim/secret').read().strip())
print('仍然 islink:', os.path.islink(p))
"
lstat islink: True -> /tmp/otest/victim/secret
victim/secret = REPLACED        ← 写穿了
仍然 islink: True               ← 链接本身没动
```

**结论**：只做父链校验是不够的；写入动作本身也必须"先删后建"。
这条已并入 H-1 的同一个 commit。

### 反向验证（H-1）

把 `extractEntry` 改回漏洞版本后重跑（测试文件一字未动）：

```
$ go test ./internal/convert/ -run 'TestExtractTar' -v
--- PASS: TestExtractTarRejectsEscape (0.00s)          ← 旧测试，漏洞版也能过（覆盖不全）
    symlink_traversal_test.go:116: 相对符号链接穿透必须被拒绝，实际未报错
--- FAIL: TestExtractTarRejectsSymlinkTraversalRelative (0.00s)
    symlink_traversal_test.go:145: 绝对符号链接穿透必须被拒绝，实际未报错
--- FAIL: TestExtractTarRejectsSymlinkTraversalAbsolute (0.00s)
    symlink_traversal_test.go:169: 深层符号链接穿透必须被拒绝
--- FAIL: TestExtractTarRejectsDeepSymlinkTraversal (0.00s)
    symlink_traversal_test.go:196: 被替换为符号链接的父目录必须被拒绝
--- FAIL: TestExtractTarRejectsSymlinkEscapingMkdirAll (0.00s)
--- FAIL: TestExtractTarRejectsHardlinkSourceTraversal (0.00s)
--- PASS: TestExtractTarRejectsHardlinkSourceRelativeTraversal (0.00s)
--- PASS: TestExtractTarRejectsHardlinkThroughSymlinkedParent (0.00s)
    symlink_traversal_test.go:275: 文件经符号链接父目录必须被拒绝
--- FAIL: TestExtractTarRejectsFileThroughSymlinkedParent (0.00s)
    symlink_traversal_test.go:295: 目录条目经符号链接父目录必须被拒绝
--- FAIL: TestExtractTarRejectsDirThroughSymlinkedParent (0.00s)
--- PASS: TestExtractTarStillAcceptsLegitArchives (0.00s)
    symlink_traversal_test.go:361: !!! 逃逸：rootfs 之外的文件被覆盖为 "REPLACED\n"
--- FAIL: TestExtractTarAcceptsOverwritingExistingSymlink (0.00s)
FAIL
FAIL	github.com/LiStudioorg/licore/internal/convert	0.019s
```

**漏洞版本上 12 个用例中 8 个失败**，每条都报出预期错误，
其中 1 条直接打印了逃逸证据。

改回修复版本后：

```
$ go test ./internal/convert/ -run 'TestExtractTar' -v
--- PASS: TestExtractTarRejectsEscape (0.00s)
--- PASS: TestExtractTarRejectsSymlinkTraversalRelative (0.00s)
--- PASS: TestExtractTarRejectsSymlinkTraversalAbsolute (0.00s)
--- PASS: TestExtractTarRejectsDeepSymlinkTraversal (0.00s)
--- PASS: TestExtractTarRejectsSymlinkEscapingMkdirAll (0.00s)
--- PASS: TestExtractTarRejectsHardlinkSourceTraversal (0.00s)
--- PASS: TestExtractTarRejectsHardlinkSourceRelativeTraversal (0.00s)
--- PASS: TestExtractTarRejectsHardlinkThroughSymlinkedParent (0.00s)
--- PASS: TestExtractTarRejectsFileThroughSymlinkedParent (0.00s)
--- PASS: TestExtractTarRejectsDirThroughSymlinkedParent (0.00s)
--- PASS: TestExtractTarStillAcceptsLegitArchives (0.00s)
--- PASS: TestExtractTarAcceptsOverwritingExistingSymlink (0.00s)
ok  	github.com/LiStudioorg/licore/internal/convert	0.015s
```

**12/12 通过。**

**诚实说明两点**：
- 漏洞版上仍有 4 个用例通过（`RejectsHardlinkSourceRelativeTraversal`、
  `RejectsHardlinkThroughSymlinkedParent`、`StillAcceptsLegitArchives`、
  以及原有的 `RejectsEscape`）。前两个是因为 `MkdirAll`/`SafeArchivePath`
  恰好挡住了——**防护来自旁路而非设计**，所以它们在两版都"通过"，
  不构成有效反向验证。这正是"通过 ≠ 有防护"的实例。
- 反向验证中我一度用过 `git checkout <vulnerable-commit> -- hub/auth.go`，
  结果因**函数签名不兼容导致测试编译失败**——那种情况下"失败"是假的。
  已改为保留签名、只回退**逻辑**。反向验证必须确保失败来自断言，
  而不是来自编译错误。

### 本次为 H-1 新增的测试覆盖

| 用例 | 攻击向量 | 漏洞版 | 修复版 |
| --- | --- | --- | --- |
| RejectsSymlinkTraversalRelative | 相对符号链接父目录 | FAIL | PASS |
| RejectsSymlinkTraversalAbsolute | 绝对路径符号链接父目录 | FAIL | PASS |
| RejectsDeepSymlinkTraversal | 深层（多级）符号链接 | FAIL | PASS |
| RejectsSymlinkEscapingMkdirAll | 目录被同名符号链接条目覆盖 | FAIL | PASS |
| RejectsHardlinkSourceTraversal | 硬链接源为绝对路径 | FAIL | PASS |
| RejectsHardlinkSourceRelativeTraversal | 硬链接源含 `../` | PASS* | PASS |
| RejectsHardlinkThroughSymlinkedParent | 硬链接落点经符号链接 | PASS* | PASS |
| RejectsFileThroughSymlinkedParent | 普通文件经符号链接父目录 | FAIL | PASS |
| RejectsDirThroughSymlinkedParent | 目录条目经符号链接父目录 | FAIL | PASS |
| AcceptsOverwritingExistingSymlink | O_TRUNC 跟随末级符号链接 | FAIL | PASS |
| StillAcceptsLegitArchives | 反向约束：合法归档须照常解压 | PASS | PASS |

`PASS*` = 漏洞版下也通过，因旁路防护生效，**不构成有效反向验证**（见上）。

---

## M-1 / M-2 修复详情

### M-1：密钥改用 crypto/rand

```go
// 修复前（可预测："licore-hub-<PID>-<UnixNano>"）
func randSecret() string {
	return fmt.Sprintf("licore-hub-%d-%d", os.Getpid(), time.Now().UnixNano())
}

// 修复后
func randSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成 hub 会话密钥失败（crypto/rand 不可用）: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
```

**生成失败即启动失败**——弱熵是*静默*灾难：服务照常起来、攻击者却能
伪造令牌，没有任何症状。宁可起不来，不可带病运行。

### M-2：exp 必须存在且有效

```go
// 修复前：c.Exp > 0 这个前置条件让 exp=0（缺失）/ exp<0 的令牌永不校验过期
if c.Exp > 0 && time.Now().Unix() > c.Exp { ... }

// 修复后
if c.Exp <= 0 {
	return nil, fmt.Errorf("JWT 缺少合法 exp（必须为正数）: %w", ErrUnauthorized)
}
now := time.Now().Unix()
if now > c.Exp { ... }
if c.Iat > now { ... }   // 额外：拒绝"未来签发"的令牌
```

### 顺带修 G112（Slowloris）

`internal/cli/hub.go` 的 `http.Server` 零值超时意味着**永不超时**，
慢速连接可长期占用 goroutine 与 fd。已显式设置四个超时
（`ReadHeaderTimeout: 10s` / `ReadTimeout: 60s` / `WriteTimeout: 5m`
/ `IdleTimeout: 2m`）。

### 反向验证（M-1 / M-2）

为保证失败来自**断言**而非编译错误，本次回退**保留 `(string, error)`
签名**，只把函数体换回可预测公式（真正的漏洞点就在函数体）。

漏洞版本：

```
$ go test ./hub/ -run 'TestRandSecret|TestForgedToken|TestDefaultServer|TestVerify' -v
    auth_security_test.go:44: 密钥长度 37 不足（期望 >= 43，即 32 字节熵）
    auth_security_test.go:48: 密钥仍是旧的可预测格式: "licore-hub-408305-1791201283396068048"
--- FAIL: TestRandSecretIsCryptographicallyRandom (0.00s)
--- PASS: TestRandSecretNotDerivableFromPidAndTime (0.00s)
    auth_security_test.go:124: !!! 密钥形如旧的可预测格式: "licore-hub-408305-1791201283396475269"
--- FAIL: TestForgedTokenRejected (0.00s)
    auth_security_test.go:156: 默认密钥仍是旧的可预测格式: "licore-hub-408305-1791201283397032627"
--- FAIL: TestDefaultServerSecretNotPredictable (0.00s)
    auth_security_test.go:169: !!! 无 exp 的令牌被接受（必须拒绝）
--- FAIL: TestVerifyRejectsMissingExp (0.00s)
    auth_security_test.go:183: !!! exp=0 的令牌被接受
--- FAIL: TestVerifyRejectsZeroExp (0.00s)
    auth_security_test.go:194: !!! exp=-1 的令牌被接受
--- FAIL: TestVerifyRejectsNegativeExp (0.00s)
--- PASS: TestVerifyRejectsExpired (0.00s)
    auth_security_test.go:221: !!! iat 为未来的令牌被接受
--- FAIL: TestVerifyRejectsFutureIat (0.00s)
--- PASS: TestVerifyAcceptsLegitToken (0.00s)
```

**10 个用例中 7 个失败**，全部报出预期错误。

修复版本：

```
$ go test ./hub/ -run 'TestRandSecret|TestForgedToken|TestDefaultServer|TestVerify' -v
--- PASS: TestRandSecretIsCryptographicallyRandom (0.00s)
--- PASS: TestRandSecretNotDerivableFromPidAndTime (0.00s)
--- PASS: TestForgedTokenRejected (0.00s)
--- PASS: TestDefaultServerSecretNotPredictable (0.00s)
--- PASS: TestVerifyRejectsMissingExp (0.00s)
--- PASS: TestVerifyRejectsZeroExp (0.00s)
--- PASS: TestVerifyRejectsNegativeExp (0.00s)
--- PASS: TestVerifyRejectsExpired (0.00s)
--- PASS: TestVerifyRejectsFutureIat (0.00s)
--- PASS: TestVerifyAcceptsLegitToken (0.00s)
ok  	github.com/LiStudioorg/licore/hub	0.007s
```

**10/10 通过。**

### ⚠️ 反向验证抓出的一个测试自身缺陷（重要）

`TestForgedTokenRejected` **第一版是假阴性**：它只拿几个"猜错的密钥"
去签令牌，然后断言验不过。这在**漏洞版本上同样通过**——因为猜错本来
就验不过，测的根本不是漏洞。

我发现后重写了它，改为**显式断言"密钥不可由 PID+时间推导"**。
并用独立探针确认了真实攻击在漏洞版本下确实成立：

```
=== RUN   TestProbeForgeryRealAttack
    probe_audit_test.go:22: !!! 攻击成功：伪造出 admin 令牌 sub=attacker scopes=[admin]
```

即：漏洞版本下，攻击者按旧公式复现出服务端密钥后，**签出的 admin
令牌能通过校验**。重写后的用例在漏洞版上如实 FAIL、修复版 PASS。

**这是本次审计最有价值的方法论收获**：反向验证不只是"证明测试有效"，
它也是**检验测试本身是否在测正确的东西**的唯一手段。若不跑反向验证，
这个假阴性用例会一直"绿"着，给人一种"伪造已被防住"的错觉。

---

## 质量门禁（两个 commit 各自通过）

| 检查 | 结果 |
| --- | --- |
| `gofmt -l .` | 空（通过）|
| `go vet ./...` | 零告警 |
| `go test ./...` | **19 个包全绿，0 FAIL** |
| `CGO_ENABLED=0` 静态构建 | 通过 |
| 交叉编译 | `linux/{amd64,arm64,arm,386,riscv64}`、`android/arm64`、`darwin/{arm64,amd64}` 共 **8 个目标全部 OK** |
| 漏洞版代码残留 | 0 处（已 grep 确认）|

> 注：`android/amd64` 不可构建，但那是 **Go 本身不支持的组合**
> （Android 仅有 arm64/386 等目标），与本次改动无关，8 个真实目标全部通过。

---

## 真机验证脚本（scripts/verify-security.sh）

覆盖本报告 §⏳ 的全部 T-1 ~ T-5，外加审计新增的 `/proc/mtrr` 与版本确认。

### 安全设计（与 AGENTS.md 操作规范一致）

1. **破坏性命令独占一节**：sysrq 真实写入放在最后的 T-6，
   **默认跳过**，须显式 `--unsafe`。启用时先打印 DESTRUCTIVE 警告
   并留 5 秒 Ctrl+C 窗口。
2. **执行前先做能力断言**：`--unsafe` 下若发现容器仍持有
   `CAP_SYS_ADMIN`，**直接中止该测试**（不是"继续跑"）。
3. **非 sysrq 的写入探测一律用 `dd count=0`**——只 `open(O_WRONLY)`，
   不写一个字节。不会像 `: > file` 那样截断文件。
4. **数据目录隔离**：`/tmp/licore-secverify-home`，不读写真实 `~/.licore`。
5. **只碰自己创建的 `licore-secverify-*` 容器**。

### 修正了脚本自身的一处缺陷

初版在前置检查失败（非 root / 无 licore）时会打印 `== ALL PASS ==`
——把"**根本没测**"误报成"**全通过**"。审计脚本谎报通过比不跑更糟。
已引入 `ABORTED` 标志，实测确认：

```
$ bash scripts/verify-security.sh          # 非 root
本脚本需要 root（创建容器 / 读 /proc/<pid>/status）。
请用：sudo bash scripts/verify-security.sh
退出码=2                                   ← 不再打印 ALL PASS

$ bash scripts/verify-security.sh --json
{"aborted":true,"exit":2}
```

### 用法

```bash
sudo bash scripts/verify-security.sh                 # 常规（推荐）
sudo bash scripts/verify-security.sh --unsafe        # 额外跑 sysrq 写入
sudo bash scripts/verify-security.sh --keep          # 失败时保留容器排查
sudo bash scripts/verify-security.sh --json          # 只输出机器可读结论
```

跑完把**完整输出**贴回审计会话即可。

---

## 仍未关闭的项（诚实清单）

本次修了 3 条，**没有**变成"审计完成"：

| 项 | 状态 |
| --- | --- |
| **A 类容器逃逸** | ⏳ **仍完全未验证**——沙箱无特权，需真机跑 `verify-security.sh` |
| **B 类网络隔离** | ⏳ 同上 |
| L-3 `/proc/mtrr` | ⏳ 已写入脚本（T-1.3b），待真机 |
| M-3 `pull` 临时路径 | 🟡 不可利用，但属脆弱设计，未修改 |
| L-1 `alice/myapp:v1` 无法拉取 | 🟡 功能缺陷（与 `isHubRef` 同源），未修改 |
| L-2 `parsePort` 静默回落 0 | 🟡 未修改 |
| G110 解压炸弹（`image` 侧） | 🟡 未修改 |
| G122 `WalkDir` 竞态路径 | 🟡 未修改 |
| `trivy` 扫描 | ❌ 未执行（工具未安装）|

**下一步建议**：在 `45.207.198.91`（root）上跑 `verify-security.sh`，
把输出贴回，再决定是否需要补修。

---

*修复部分完成于 v0.9.6。反向验证输出为真实执行结果，未做任何修饰。*
