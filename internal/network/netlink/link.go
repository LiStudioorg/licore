// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package netlink

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
)

// ErrLinkNotFound 表示按名字找不到链路。
var ErrLinkNotFound = errors.New("licore/network/netlink: 链路不存在")

// 常用链路类型 kind 值。
const (
	KindBridge = "bridge"
	KindVeth   = "veth"
)

// Link 是内核里的一个网络接口（ifindex + 名字）。
type Link struct {
	IfIndex int
	Name    string
	MTU     int
	Flags   uint32
}

// ifInfoMsg 生成 ifinfomsg 固定头（16 字节）。
func ifInfoMsg() []byte {
	b := make([]byte, 16)
	b[0] = syscall.AF_UNSPEC
	return b
}

// ifInfoMsgIdx 生成 ifinfomsg 固定头并带上 ifindex。
func ifInfoMsgIdx(ifindex uint32) []byte {
	b := ifInfoMsg()
	binary.LittleEndian.PutUint32(b[4:8], ifindex)
	return b
}

// NewLink 在当前命名空间创建 kind 类型的链路。optsExtra 是额外属性。
func NewLink(name, kind string, optsExtra ...Attr) error {
	r := newReq(RTM_NEWLINK, NLM_F_CREATE|NLM_F_EXCL, ifInfoMsg())
	r.addAttrString(IFLA_IFNAME, name)
	if kind != "" {
		r.addAttr(IFLA_LINKINFO, nestedAttrs([]Attr{
			{Type: IFLA_INFO_KIND, Data: cstr(kind)},
		}))
	}
	for _, a := range optsExtra {
		r.addAttr(a.Type, a.Data)
	}
	_, err := r.do()
	return err
}

// NewVeth 创建一对 veth：name + peer。
func NewVeth(name, peer string) error {
	r := buildVethReq(name, peer)
	_, err := r.do()
	return err
}

// buildVethReq 构造 veth 对创建的 rtnetlink 请求。
//
// 内核 veth_newlink 用 nla_parse_nested_deprecated 解析 VETH_INFO_PEER，
// 其值 = struct ifinfomsg（16 字节零）+ 一个 IFLA_IFNAME 属性存 peer 名。
// 旧实现把 peer 名拼成裸字符串（ifinfomsg + 缓冲），内核无法解析该属性、
// 丢弃请求并不回 ACK，导致 Recvfrom 收到 EAGAIN（"resource temporarily
// unavailable"）。
func buildVethReq(name, peer string) *req {
	// VETH_INFO_PEER 值：ifinfomsg + 内层 IFLA_IFNAME 属性（peer 名）。
	peerBody := nestedAttrs([]Attr{{Type: IFLA_IFNAME, Data: cstr(peer)}})
	peerBuf := append(ifInfoMsg(), peerBody...)
	r := newReq(RTM_NEWLINK, NLM_F_CREATE|NLM_F_EXCL, ifInfoMsg())
	r.addAttrString(IFLA_IFNAME, name)
	r.addAttr(IFLA_LINKINFO, nestedAttrs([]Attr{
		{Type: IFLA_INFO_KIND, Data: cstr(KindVeth)},
		{Type: IFLA_INFO_DATA, Data: nestedAttrs([]Attr{
			{Type: VETH_INFO_PEER, Data: peerBuf},
		})},
	}))
	return r
}

// DelLink 按名字删除链路。
func DelLink(name string) error {
	r := newReq(RTM_DELLINK, 0, ifInfoMsg())
	r.addAttrString(IFLA_IFNAME, name)
	_, err := r.do()
	return err
}

// LinkByName 按名字查找链路（当前命名空间）。
func LinkByName(name string) (*Link, error) {
	r := newReq(RTM_GETLINK, 0, ifInfoMsg())
	r.addAttrString(IFLA_IFNAME, name)
	msgs, err := r.do()
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if m.Type != RTM_NEWLINK || len(m.Data) < 16 {
			continue
		}
		attrs, err := parseAttrs(m.Data[16:])
		if err != nil {
			continue
		}
		if ifName := trimAttrString(attrBytes(attrs, IFLA_IFNAME)); ifName != name {
			continue
		}
		return &Link{
			IfIndex: int(binary.LittleEndian.Uint32(m.Data[4:8])),
			Name:    name,
			MTU:     int(binary.LittleEndian.Uint32(attrBytes(attrs, IFLA_MTU))),
			Flags:   binary.LittleEndian.Uint32(m.Data[8:12]),
		}, nil
	}
	return nil, fmt.Errorf("链路 %s: %w", name, ErrLinkNotFound)
}

// trimAttrString 去掉内核字符串属性末尾的 NUL/对齐填充（IFLA_IFNAME 等是
// NUL 结尾，attrBytes 原样返回含填充字节）；不足 3 字节（如 u32 属性）原样返回。
func trimAttrString(b []byte) string {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0 {
			return string(b[:i+1])
		}
	}
	return string(b)
}

// LinkUp / LinkDown 切换接口上下线。
func LinkUp(name string) error {
	r := newReq(RTM_NEWLINK, 0, ifInfoMsgIdx(mustIndex(name)))
	r.addAttrString(IFLA_IFNAME, name)
	r.buf = setIFFBuf(r.buf, syscall.IFF_UP, syscall.IFF_UP)
	_, err := r.do()
	return err
}

func LinkDown(name string) error {
	r := newReq(RTM_NEWLINK, 0, ifInfoMsgIdx(mustIndex(name)))
	r.addAttrString(IFLA_IFNAME, name)
	r.buf = setIFFBuf(r.buf, 0, syscall.IFF_UP)
	_, err := r.do()
	return err
}

func mustIndex(name string) uint32 {
	l, err := LinkByName(name)
	if err != nil {
		return 0
	}
	return uint32(l.IfIndex)
}

// setIFFBuf 修正 ifinfomsg 头里的 flags 与 change 字段。
// buf 是一条完整 nlmsg：偏移 0..16 是 nlmsghdr，16..32 是 struct ifinfomsg，
// 其 ifi_flags/ifi_change 位于 ifinfomsg 的 offset 8/12，即 buf 的 24/28。
// （此前在这里写 buf[8:12]/[12:16]，实为把 nlmsghdr 的 seq/pid 覆盖成
// 0x1，导致内核丢包、命令 EAGAIN 超时。）
func setIFFBuf(buf []byte, flags, change uint32) []byte {
	const nlmsgHdrLen = 16
	const ifiFlagsOff = 8 // struct ifinfomsg 内 ifi_flags 偏移
	if len(buf) >= nlmsgHdrLen+ifiFlagsOff+8 {
		binary.LittleEndian.PutUint32(buf[nlmsgHdrLen+ifiFlagsOff:], flags)
		binary.LittleEndian.PutUint32(buf[nlmsgHdrLen+ifiFlagsOff+4:], change)
	}
	return buf
}

// SetLinkMaster 把链路 name 挂到网桥 master 下。
// RTM_NEWLINK 的主体必须是待被挂接的链路 name（其 ifinfomsg.ifindex +
// IFLA_IFNAME），IFLA_MASTER 才指向网桥。此前误把 master 的 ifindex 放进
// ifinfomsg（得到"把网桥挂到网桥自己"）→ 内核回 EBUSY（device or resource
// busy）。
func SetLinkMaster(name, master string) error {
	slave, err := LinkByName(name) // 待挂接的链路
	if err != nil {
		return err
	}
	m, err := LinkByName(master) // 网桥
	if err != nil {
		return err
	}
	r := buildSetLinkMasterReq(uint32(slave.IfIndex), name, uint32(m.IfIndex))
	_, err = r.do()
	return err
}

// buildSetLinkMasterReq 构造把 slaveName 挂到 masterIfindex 网桥的 RTM_NEWLINK
// 请求；主题是 slave（ifinfomsg.ifindex + IFLA_IFNAME），IFLA_MASTER 指向网桥。
func buildSetLinkMasterReq(slaveIfindex uint32, slaveName string, masterIfindex uint32) *req {
	r := newReq(RTM_NEWLINK, 0, ifInfoMsgIdx(slaveIfindex))
	r.addAttrString(IFLA_IFNAME, slaveName)
	r.addAttr(IFLA_MASTER, u32(masterIfindex))
	return r
}

// SetLinkNetnsPid 把链路移动到 pid 的网络命名空间。
func SetLinkNetnsPid(name string, pid int) error {
	r := newReq(RTM_NEWLINK, 0, ifInfoMsg())
	r.addAttrString(IFLA_IFNAME, name)
	r.addAttr(IFLA_NET_NS_PID, u32(uint32(pid)))
	_, err := r.do()
	return err
}

// AddAddr 为接口绑定一个 IP。ip 如 "172.18.0.2"，prefix 如 16。
func AddAddr(ifname, ip string, prefix int) error {
	idx := mustIndex(ifname)
	r := newReq(RTM_NEWADDR, NLM_F_CREATE|NLM_F_EXCL, buildAddrMsg(idx, prefix))
	r.addAttr(IFA_LOCAL, netIP4(ip))
	r.addAttr(IFA_ADDRESS, netIP4(ip))
	_, err := r.do()
	return err
}

// IfAddr 是接口上的一个 IPv4 地址。
type IfAddr struct {
	IP     string // 如 "172.22.0.1"
	Prefix int    // 如 16
}

// String 返回 CIDR 形式，便于比较与报错信息展示。
func (a IfAddr) String() string { return fmt.Sprintf("%s/%d", a.IP, a.Prefix) }

// ListAddrs 列出接口上的全部 IPv4 地址（RTM_GETADDR + NLM_F_DUMP 后按 index 过滤）。
//
// 用于"复用已存在的网桥前先核对网段"：接口存在只说明名字撞了，
// 不说明它的地址就是本网络定义的那个（见 driverBootstrap 与
// ErrBridgeSubnetMismatch）。只收集 AF_INET，IPv6 不参与判断。
//
// 必须带 NLM_F_DUMP：不带时内核回 EOPNOTSUPP（真机实测报
// "rtnetlink get-addr: operation not supported"，在 lo / licore0 /
// docker0 上均如此）。
func ListAddrs(ifname string) ([]IfAddr, error) {
	idx := mustIndex(ifname)
	// ifaddrmsg 只填 family 与 index，前缀长度留给内核按每条地址回填。
	msg := make([]byte, 8)
	msg[0] = syscall.AF_INET
	binary.LittleEndian.PutUint32(msg[4:8], idx)

	r := newReq(RTM_GETADDR, NLM_F_DUMP, msg)
	msgs, err := r.do()
	if err != nil {
		return nil, fmt.Errorf("列出 %s 的地址: %w", ifname, err)
	}

	var out []IfAddr
	for _, m := range msgs {
		if m.Type != RTM_NEWADDR || len(m.Data) < 8 {
			continue
		}
		// struct ifaddrmsg：family[0] prefixlen[1] flags[2] scope[3] index[4:8]
		if m.Data[0] != syscall.AF_INET {
			continue
		}
		if binary.LittleEndian.Uint32(m.Data[4:8]) != idx {
			continue
		}
		attrs, err := parseAttrs(m.Data[8:])
		if err != nil {
			continue
		}
		// IFA_ADDRESS 是接口地址；点对点链路上 IFA_LOCAL 才是本端地址，
		// 两者都有时优先 IFA_LOCAL。
		b := attrBytes(attrs, IFA_LOCAL)
		if len(b) < 4 {
			b = attrBytes(attrs, IFA_ADDRESS)
		}
		if len(b) < 4 {
			continue
		}
		out = append(out, IfAddr{
			IP:     net.IP(b[:4]).String(),
			Prefix: int(m.Data[1]),
		})
	}
	return out, nil
}

// DelAddr 移除接口上的 IP。
func DelAddr(ifname, ip string, prefix int) error {
	idx := mustIndex(ifname)
	r := newReq(RTM_DELADDR, 0, buildAddrMsg(idx, prefix))
	r.addAttr(IFA_LOCAL, netIP4(ip))
	r.addAttr(IFA_ADDRESS, netIP4(ip))
	_, err := r.do()
	return err
}

// buildAddrMsg 构造 struct ifaddrmsg（8 字节）：family[0] prefixlen[1]
// flags[2] scope[3] index[4:8]。此前把 prefix 写进 b[2](ifa_flags)，导致
// 地址以 /0 添加、网关不在连本网段，设默认路由时报 network is unreachable。
func buildAddrMsg(ifindex uint32, prefix int) []byte {
	b := make([]byte, 8)
	b[0] = syscall.AF_INET
	b[1] = byte(prefix) // ifa_prefixlen
	b[3] = byte(RT_SCOPE_UNIVERSE)
	binary.LittleEndian.PutUint32(b[4:8], ifindex)
	return b
}

// AddRoute 添加一条 IPv4 路由。dst 为空表示默认路由。
func AddRoute(dst string, prefix int, via string, ifname string) error {
	if dst == "" {
		prefix = 0
	}
	r := newReq(RTM_NEWROUTE, NLM_F_CREATE|NLM_F_EXCL, buildRouteMsg(prefix))
	if dst != "" {
		r.addAttr(RTA_DST, netIP4(dst))
	}
	if via != "" {
		r.addAttr(RTA_GATEWAY, netIP4(via))
	}
	if ifname != "" {
		if l, err := LinkByName(ifname); err == nil {
			r.addAttr(RTA_OIF, u32(uint32(l.IfIndex)))
		}
	}
	_, err := r.do()
	return err
}

// DelRoute 删除一条 IPv4 路由。
func DelRoute(dst string, prefix int, via string, ifname string) error {
	if dst == "" {
		prefix = 0
	}
	r := newReq(RTM_DELROUTE, 0, buildRouteMsg(prefix))
	if dst != "" {
		r.addAttr(RTA_DST, netIP4(dst))
	}
	if via != "" {
		r.addAttr(RTA_GATEWAY, netIP4(via))
	}
	if ifname != "" {
		if l, err := LinkByName(ifname); err == nil {
			r.addAttr(RTA_OIF, u32(uint32(l.IfIndex)))
		}
	}
	_, err := r.do()
	return err
}

// buildRouteMsg 构造 struct rtmsg（12 字节）的完整字节。
//
// 布局：family[0] dst_len[1] src_len[2] tos[3] table[4] protocol[5]
// scope[6] type[7] flags[8:12]。此前用 putU32(b[4:8], RT_TABLE_MAIN) 把
// rtm_type(byte7) 盖成 0(RTN_UNSPEC)、并把 RTN_UNICAST 错写到 rtm_flags，
// 内核拒收 → add-route 报 invalid argument。
func buildRouteMsg(prefix int) []byte {
	b := make([]byte, 12)
	b[0] = syscall.AF_INET
	b[1] = byte(prefix) // rtm_dst_len，默认路由为 0
	b[4] = byte(RT_TABLE_MAIN)
	b[5] = byte(RTPROT_BOOT)
	b[6] = byte(RT_SCOPE_UNIVERSE)
	b[7] = byte(RTN_UNICAST)
	return b
}

// ---- 辅助 ----

// Attr 是用户侧表达的一个 rtnetlink 属性。
type Attr struct {
	Type uint16
	Data []byte
}

// nestedAttrs 把一组属性编码为嵌套属性体。
func nestedAttrs(attrs []Attr) []byte {
	var buf []byte
	for _, a := range attrs {
		n := attrAlign(4 + len(a.Data))
		out := make([]byte, n)
		putU16(out[0:2], uint16(4+len(a.Data)))
		putU16(out[2:4], a.Type)
		copy(out[4:], a.Data)
		buf = append(buf, out...)
	}
	return buf
}

// cstr 转为 NUL 结尾字符串字节。
func cstr(s string) []byte { return append([]byte(s), 0) }

// u32 编码 32 位小端。
func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

// netIP4 把 "172.18.0.2" 编码为 4 字节；格式错误返回 nil。
func netIP4(s string) []byte {
	out := [4]byte{}
	var cur byte
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '.' {
			if n >= 3 {
				return nil
			}
			out[n] = cur
			n++
			cur = 0
			continue
		}
		if c < '0' || c > '9' {
			return nil
		}
		cur = cur*10 + (c - '0')
	}
	if n != 3 {
		return nil
	}
	out[3] = cur
	return out[:]
}

// attrBytes 从 parseAttrs 结果里取属性原始字节。
func attrBytes(attrs map[uint16][]byte, t uint16) []byte {
	v, ok := attrs[t]
	if !ok {
		return nil
	}
	return v
}

// MustIndexOf 返回接口 ifindex。
func MustIndexOf(name string) (uint32, error) {
	l, err := LinkByName(name)
	if err != nil {
		return 0, err
	}
	return uint32(l.IfIndex), nil
}
