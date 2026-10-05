# 已知限制

本文档记录 LiCore **当前已知但尚未修复**的限制。每条都写清：现象、原因、
影响面、以及修复方向——便于用户判断是否会影响自己的用法，也便于后续接手。

> 与 [docs/unverified.md](unverified.md) 的分工：那里记录"**代码完成但未在
> 真机验证**"的能力；这里记录"**已确认存在、暂不修复**"的限制。

---

## L-1 宿主经 `127.0.0.1:<发布端口>` 访问容器不通

**状态**：已知，未修复（v0.9.5 起记录）

### 现象

```bash
licore run -d --name web -p 18080:80 nginx:1.27-alpine
curl http://127.0.0.1:18080/        # ✗ 一直卡住，无响应
curl http://<宿主 eth0 IP>:18080/   # ✓ HTTP 200
```

容器内自访问正常（`curl http://localhost:80/` 返回页面），外部经宿主公网 IP
访问也正常。**只有宿主本机走回环地址这一条路径不通。**

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
- 宿主本机经宿主**非回环** IP 访问端口映射
- 容器内自访问
- 容器访问外网

CI / 健康检查脚本若用 `curl localhost:<port>` 探测容器，会失败——改用宿主
实际 IP 即可。

### 修复方向

参考 Docker 的做法，两步：

1. **`net.ipv4.conf.<bridge>.route_localnet=1`**：允许在网桥上路由
   127.0.0.0/8 的地址。默认 0 时内核会丢弃"源或目的是回环地址却出现在
   非回环接口上"的报文。
2. **SNAT**：在报文**离开网桥之前**把源地址 127.0.0.1 改写成网桥地址，
   使容器的回包有真实可路由的目的地址。

Docker 用的是 `-j MASQUERADE` 配合 `route_localnet`；本项目的
`natLoopbackMasqArgs` 已有类似意图，但条件需要按真实报文重写。

**注意**：`route_localnet=1` 有安全含义（放宽回环地址的路由限制），
需要在实现时评估是否只对容器网桥开启。

### 验证用例（修复后应通过）

```bash
licore run -d --name web -p 18080:80 nginx:1.27-alpine
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18080/   # 期望 200
licore stop web && licore rm web
```

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
