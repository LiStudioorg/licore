# 已知限制

本文档记录 LiCore **当前已知但尚未修复**的限制。每条都写清：现象、原因、
影响面、以及修复方向——便于用户判断是否会影响自己的用法，也便于后续接手。

> 与 [docs/unverified.md](unverified.md) 的分工：那里记录"**代码完成但未在
> 真机验证**"的能力；这里记录"**已确认存在、暂不修复**"的限制。

---

## L-1 宿主经 `127.0.0.1:<发布端口>` 访问容器不通

**状态**：已知，未修复（v0.9.5 起记录；2026-10-05 真机复现确认）

### 复现步骤（真机实测，2026-10-05）

在 `45.207.198.91`（Ubuntu，内核 5.15.0-194，root）上：

```bash
# 1. 起一个监听端口的容器，发布到宿主 18099
licore run -d --name pmap-test -p 18099:8099 alpine:3.20.3-amd64 \
  sh -c 'while true; do { echo -e "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"; } | nc -l -p 8099; done'

# 2. 经宿主**非回环** IP 访问 → 正常
curl -s -o /dev/null -w 'eth0 -> %{http_code}\n' http://45.207.198.91:18099/
#   eth0 -> 200                      ✓

# 3. 经宿主**回环**访问 → 卡住直到超时
curl -s -o /dev/null -w '127.0.0.1 -> %{http_code}\n' http://127.0.0.1:18099/
#   （无输出，curl 超时退出）          ✗
```

同一次会话内还确认了容器侧网络本身是好的（排除"网络整体不通"）：

```
容器内 ip route : default via 172.22.0.1 dev vpeXXXX   ✓
ping 网关       : 0% packet loss                        ✓
ping 8.8.8.8    : 0% packet loss                        ✓
nslookup        : example.com 解析成功                   ✓
```

**即：只有"宿主本机 → 回环地址 → 发布端口"这一条路径不通**，
其余全部正常。

### 原因

端口映射的 DNAT 只改写**目的地址**，源地址保持原样：

```
宿主 curl 127.0.0.1:18080
  → nat OUTPUT 的 DNAT：daddr 127.0.0.1:18080 → 172.22.0.3:80
    但 **src 仍是 127.0.0.1**
  → 报文出 licore0 到容器，容器收到 src=127.0.0.1 dst=172.22.0.3:80
  → 容器回包：src=172.22.0.3 dst=127.0.0.1
  → 容器查自己的路由表：127.0.0.0/8 是**本地**地址
  → 回包被投递到**容器自己的 lo**，永远回不到宿主
```

对比经 eth0 IP 访问时源地址是宿主真实 IP，容器按默认路由把回包发给网桥
网关，正常返回。

**为什么现有的回环伪装规则没生效**：`natLoopbackMasqArgs` 生成的规则是

```
-s 127.0.0.0/8 -d 172.22.0.0/16 -j MASQUERADE
```

真机实测该规则**命中计数为 0**——条件与实际报文不匹配（报文在 POSTROUTING
阶段的地址形态与规则预期不一致）。也就是说这条规则从加进来起就没真正工作过。

### 影响面

**仅限"宿主本机经回环地址访问发布端口"**。以下均不受影响：

- 外部（其它机器）经宿主公网 / 内网 IP 访问端口映射
- 宿主本机经宿主**非回环** IP 访问端口映射（实测 200）
- 容器内自访问
- 容器访问外网（实测通，含 DNS）

CI / 健康检查脚本若用 `curl localhost:<port>` 探测容器，会失败——改用宿主
实际 IP 即可。

### 修复方向

参考 Docker 的做法，两步：

1. **`net.ipv4.conf.<bridge>.route_localnet=1`**：允许在网桥上路由
   127.0.0.0/8 的地址。默认 0 时内核会丢弃"源或目的是回环地址却出现在
   非回环接口上"的报文。Docker 在创建网桥时就会设这一项
   （见 `libnetwork/drivers/bridge`，每个网关接口都开 `route_localnet`）。
2. **SNAT**：在报文**离开网桥之前**把源地址 127.0.0.1 改写成网桥地址，
   使容器的回包有真实可路由的目的地址。Docker 用的是
   `-j MASQUERADE`（在 `POSTROUTING` 阶段，配合 `route_localnet`）。

本项目的 `natLoopbackMasqArgs` 已有类似意图，但**条件与实际报文不匹配**
（命中计数为 0 已证实）。修复时要按真实报文重写规则条件，而不是简单保留
现有那条。

**注意**：`route_localnet=1` 有安全含义（放宽回环地址的路由限制，
使非回环接口上出现 127/8 地址时不再被内核丢弃）。需要在实现时评估是否
**只对容器网桥**开启、以及是否需要在 `LICORE-INPUT` 侧补充相应限制。
不要全局开 `net.ipv4.conf.all.route_localnet`。

### 验证用例（修复后应通过）

```bash
licore run -d --name web -p 18099:8099 alpine:3.20.3-amd64 \
  sh -c 'while true; do { echo -e "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"; } | nc -l -p 8099; done'

# 回环：修复后应为 200（当前超时）
curl -sS -o /dev/null -w '127.0.0.1 -> %{http_code}\n' --max-time 5 http://127.0.0.1:18099/

# 非回环：任何时候都应为 200（回归护栏）
curl -sS -o /dev/null -w 'eth0 -> %{http_code}\n' --max-time 5 http://<宿主IP>:18099/

# 同时确认回环伪装规则的命中计数**非 0**（当前恒为 0）
sudo iptables -t nat -L LICORE-POST -v -n | grep 127
sudo nft list chain ip licore post_nat 2>/dev/null

licore stop web && licore rm web
```

### 优先级

**不急修**（用户已知悉并接受）。理由：workaround 明确且成本极低——用宿主
实际 IP 即可，脚本里改一处。但**必须文档化**，否则每个用
`curl localhost:<port>` 做健康检查的用户都会撞上，且表现为"连接超时"，
极易误判成容器没起来或引擎有严重问题。

---

## L-4 传 `--user` 时容器**边界集**里保留 `CAP_SETPCAP`/`CAP_SETUID`/`CAP_SETGID`

**状态**：已知，**有意保留**（v0.9.8 起记录；2026-10-06 真机实测确认不可兑现）

### 现象

只传 `--cap-drop` 时，容器 PID 1 的**边界集**精确等于最终集合：

| 参数 | CapEff | CapBnd |
| --- | --- | --- |
| `--cap-drop ALL` | `0000000000000000` | `0000000000000000` |

但一旦**同时**传 `--user`，边界集就会多留三个能力：

| 参数 | CapEff | CapBnd |
| --- | --- | --- |
| `--user 1000:1000` | `0000000000000000` | `00000000a80425fb` |
| `--cap-drop ALL --user 1000:1000` | `0000000000000000` | `00000000000001c0` |
| `--cap-drop SETGID,SETUID --user 1000:1000` | `0000000000000000` | `00000000a80425fb` |

`0x1c0` = bit6 `SETGID` | bit7 `SETUID` | bit8 `SETPCAP`。
`0xa80425fb` 是默认 14 项再加上这三个。

注意 **`CapEff` 始终是 `0`** —— 有效集没有任何能力。这与"边界集残留"
是两件事，也是最容易看错的地方。

### 原因

这是"降权 + 裁剪能力"同时要求下**唯一可行顺序**的必然产物。

内核有五条方向互斥的约束（`internal/runtime/capability_linux.go` 有完整推导）：

- `PR_CAPBSET_DROP` 需要 `CAP_SETPCAP` 在**有效集**里；
- 从**边界集**移除某能力时，内核同时把它从 permitted/effective 清掉；
- `capset` 收紧**非空**集合同样需要 `CAP_SETPCAP` 在有效集里；
- `setuid/setgid` 到非 0 需要 `CAP_SETUID`/`CAP_SETGID` 在有效集里；
- **降权成功会清空 permitted/effective** → 降权必须是最后一步能力操作。

由前三条可得：`CAP_SETPCAP` 一旦离开边界集，之后**任何** `capset` 与
`PR_CAPBSET_DROP` 都 EPERM。而移除 `CAP_SETPCAP` 自身这一步又必须在
"用它清完其它能力"之后 —— **死锁**。解法是：清边界集时**把 `CAP_SETPCAP`
一起留着**，只把它从**有效集**摘掉。

`SETUID`/`SETGID` 同理：降权需要它们有效，而降权是最后一步，
所以它们必须在边界集里活到最后。

**五个错序全部真机实测过，容器一律 `Exited(1)`**：

```
先降权再裁剪        -> prctl(PR_CAPBSET_DROP, 2): operation not permitted
先裁剪再降权        -> 设置 gid=1000 失败: operation not permitted
清边界集后 capset    -> capset: operation not permitted
capset 后清边界集    -> prctl(PR_CAPBSET_DROP, 0): operation not permitted
降权后再收口        -> 清边界集(最终): operation not permitted
```

### 影响面：已实测确认**不可兑现**

边界集只是"execve 时**最多**能获得哪些能力"的**上限**，它本身**不授予**
任何能力。要把它兑现成真实特权只有两条路，**两条都堵死**。

**路径一：经 execve 提权**（file capability / setuid-root 程序）。
容器已设 `no_new_privs=1`，内核在 execve 时**不赋予任何新特权**。

真机验证（`--cap-drop ALL --user 1000:1000` 容器内）：

```
CapInh: 00000000000000c0     <- 继承集里确实有 SETUID|SETGID
CapPrm: 0000000000000000     <- 但 permitted 为空
CapEff: 0000000000000000     <- effective 也为空
CapBnd: 00000000000001c0     <- 边界集残留
CapAmb: 0000000000000000
NoNewPrivs: 1
```

且容器内**没有任何可兑现它的东西**：

```bash
find / -xdev -type f -perm -4000      # 无 setuid 程序
/bin/busybox id                        # uid=1000 gid=1000 —— 没拿回 root
```

**路径二：直接调 `capset` 改自己的能力集**（不需要 execve —— 这是
`CAP_SETPCAP` 比 `SETUID`/`SETGID` 更值得单独说明的地方）。
但 `capset` 收紧非空集合需要 `CAP_SETPCAP` 在**有效集**里，
而收口第 3 步（`capset(keep ∪ {SETUID,SETGID})`）已把它从有效集摘掉，
此后 permitted 与 inheritable 均为空，子进程也继承不到。

两条路都不可达，故残留**不构成提权面**。

### 为什么不干脆清干净

因为**清不掉**：降权之后内核清空了 permitted/effective，`PR_CAPBSET_DROP`
必然 EPERM（约束 5）。想清就必须把降权放到清边界集之前，而那会让
`setuid` 因缺 `CAP_SETUID` 失败（约束 4）—— 这正是上面五条错序里的两种。

### 修复方向

**没有干净的修复方向**，除非改变隔离模型本身：

1. **引入 `CLONE_NEWUSER`**：容器 root 映射为非 root，`setuid` 语义改变，
   不再需要 `CAP_SETUID`。但这正是本项目**有意不做**的（root 下运行时不加
   `CLONE_NEWUSER`，加了会失去挂载能力）。属架构级取舍，不是缺陷修复。
2. **降权改由父进程在 fork 后、exec 前完成**：需要引擎侧多一层配合，
   且要保证降权发生在收口之后。成本高，收益仅是"边界集更干净"，
   而残留已证明不可兑现。

**结论：保持现状。** 这条记录的目的是让审计者看到 `CapBnd` 非零时
**不要直接判为漏洞** —— 需结合 `CapEff`/`CapPrm`/`NoNewPrivs` 一起看。

### 验证用例

```bash
# 1. 边界集残留确实存在（读容器 PID 1，不是 exec 进程）
licore run -d --name setpcap --cap-drop ALL --user 1000:1000 \
  alpine:3.20.3-amd64 /bin/busybox sleep 300
P=$(sudo python3 -c "import json;print(json.load(open('/root/.licore/containers/$(licore ps -q | head -1)/runtime.json'))['initPid'])")
sudo grep -E 'Cap|Uid|NoNewPrivs' /proc/$P/status
#   期望 CapBnd=00000000000001c0、CapEff=0000000000000000、NoNewPrivs=1

# 2. 残留不可兑现：容器内拿不回特权
licore exec setpcap /bin/busybox id                                  # 期望 uid=1000 gid=1000
licore exec setpcap /bin/sh -c 'find / -xdev -type f -perm -4000'    # 期望空

# 3. exec 进程同样不能兑现（exec 走独立的收口路径）
licore exec setpcap /bin/sh -c 'grep -E "Cap(Prm|Eff|Bnd)" /proc/self/status'

licore stop setpcap && licore rm setpcap
```

### 优先级

**不修**（有意保留）。若未来审计要求"边界集必须精确等于最终集合"，
需先推翻"root 下不加 `CLONE_NEWUSER`"的架构决定，属 RFC 级议题。

---

## L-2 容器 DNS 依赖宿主可用的上游

**状态**：设计如此，非缺陷（记录以便排查）

容器 `resolv.conf` 写的是**宿主侧探测到的上游 DNS**（见
`internal/network/dnsresolve_linux.go`）。探测顺序：systemd-resolved 的真实
上游 → 宿主 `/etc/resolv.conf` → 公共 DNS 兜底。

**含义**：如果宿主用的 DNS 只在宿主本机可达（如企业内网 DNS），容器可能
解析不了——因为容器有独立 netns，宿主回环上的解析器**在容器里不可达**，
探测时会被过滤掉。

**排查方法**：

```bash
licore run -d --name t alpine:3.20.3-amd64 sleep 60
licore exec t /bin/sh -c 'cat /etc/resolv.conf'   # 看实际写了什么
```

若写入的是公共 DNS 兜底（1.1.1.1 / 8.8.8.8），说明宿主上游不可被容器使用。
此时可在宿主的 systemd-resolved 里配置一个**非回环**的上游。

**后续方向**：在网桥网关上跑一个轻量 DNS 转发（容器始终指向网关），
可同时获得缓存与集中日志能力。这需要引入第三方 DNS 库，**须先批准依赖**。

---

## L-3 nft 与 iptables 双后端并存

**状态**：设计如此（兼容性取舍）

容器网络的 NAT 与 FORWARD 规则**固定用 iptables** 实现（见
`internal/network/forward_linux.go` 说明），而 NAT 规则本身 nft 优先、
失败回退 iptables。

**原因**：部分发行版（实测 Ubuntu 22.04 标准版 nftables 1.0.2）的
`masquerade` 语句不可用，而 iptables 的 `-j MASQUERADE` 正常。为了在两类
环境上都能工作，保留了两条路径。

**含义**：宿主上会同时出现 LiCore 的 nft 表（若 nft 可用）与 iptables 链。
两者由代码保证互不冲突，但排查网络问题时要**同时看两边**：

```bash
sudo nft list table ip licore          # nft 侧（若在用）
sudo iptables -t nat -S | grep LICORE  # iptables NAT 侧
sudo iptables -S | grep LICORE         # iptables FORWARD/INPUT 侧
```

`licore rm` 会清理两侧，实测零残留。

---

## L-6 多数据目录共用同一宿主时的网桥网段冲突（已修复）

**状态**：已修复（v0.9.7，commit `0a97664`）

### 曾经的现象

在**不是创建网桥时所用的那个数据目录**里起容器，容器"起来了"、`ps` 显示
Up、退出码 0，但**完全没网**，且没有任何报错：

```
容器内 ip route : default via 172.21.0.1 dev vpeXXXX    ← 这个网关不存在
容器内 ping 8.8.8.8 : 100% packet loss
```

宿主网桥 `licore0` 实际的地址是 `172.22.0.1/16`。

### 原因（根因不在报错的那一行）

`Manager.ensurePreset` 在新数据目录里无条件调用 `pickFreeSubnet` 挑网段。
而 `pickFreeSubnet` → `subnetRoutedByOther` **显式跳过 licore0 自己的路由**
（`iface == PresetBridgeName → continue`，本意是"别被自己绊倒"），
因此它看不见既有网桥占着的 `172.22.0.0/16`，很自然地选出 `172.21.0.0/16`：

```
宿主网桥实际  : 172.22.0.1/16   （还在，继续服务老容器）
新数据目录定义: 172.21.0.1/16   （凭空分配，网桥上根本没这个地址）
```

随后 `driverBootstrap` 见同名网桥就直接复用（只看名字、不看地址），
容器于是拿到一个指向不存在下一跳的默认路由。

### 修复

1. **`ensurePreset` 优先采用既有网桥的真实网段**（网桥是事实来源），
   只有宿主上还没有网桥时才自行挑选。这是根因修复。
2. **`driverBootstrap` 复用前校验网段**，不匹配返回
   `ErrBridgeSubnetMismatch`，不静默复用。
3. **engine 把该错误致命化**：`run` 明确失败，不留下无网容器。

判据是"网桥上存在一个与本网络 Gateway + Subnet 完全吻合的地址"，
不是"只有它一个"——网桥挂多地址是合法用法。

### 触发场景与现状

多用户共用一台机器、换 `LICORE_HOME`、容器内跑 CI —— 只要"宿主已有
licore0 而引擎用了另一个数据目录"就会命中。

**现在**：新数据目录会自动采用既有网桥的网段，与既有容器共存无冲突
（真机验证：全新数据目录起容器，网关与外网均通）。
真的无法共存时（如网桥被人为改过）会明确报错并给出处置建议，
而不是给你一个静默无网的容器。

### 附带影响（值得记住）

被覆写的 `licore0.json` 会让**原本正常的数据目录也变得不可用**。
修复前这表现为"昨天还好好的，今天容器都没网了"——因为彼此的表现都是
静默无网，极难定位。修复后同一种情况会直接报错并指出是哪个网桥、
期望什么、实际什么。
