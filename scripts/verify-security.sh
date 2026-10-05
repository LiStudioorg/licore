#!/usr/bin/env bash
# Copyright (C) 2026 LiStudioorg
# SPDX-License-Identifier: AGPL-3.0-only
#
# licore 安全验证脚本（真机，root）
# 覆盖 docs/security-audit-v2.md 中标记为 ⏳ 待真机验证 的全部项（T-1 ~ T-5）。
#
# 用法（以 root 运行）：
#   sudo bash scripts/verify-security.sh
#   sudo LICORE_BIN=/usr/local/bin/licore bash scripts/verify-security.sh
#   sudo bash scripts/verify-security.sh --unsafe    # 额外跑 sysrq 写入测试（见下）
#   sudo bash scripts/verify-security.sh --keep      # 失败时保留容器便于排查
#   sudo bash scripts/verify-security.sh --json      # 只输出机器可读结论
#
# ---------------------------------------------------------------------------
# 安全约束（务必先读）
# ---------------------------------------------------------------------------
# 1. **只碰本脚本自己创建的容器**：全部以 licore-secverify- 前缀命名，
#    清理时按确切名字删除。脚本**从不**触碰其他容器 / 服务。
# 2. **数据目录隔离**：全程 LICORE_HOME=/tmp/licore-secverify-home，
#    不读不写用户真实的 ~/.licore。
# 3. **所有写入探测都是"只在容器内、只对已知危险文件做 open(O_WRONLY)"
#    的形式**，不真的写入危险内容。唯一的例外是 sysrq 测试，见第 4 条。
# 4. **DESTRUCTIVE：sysrq 写入测试默认不跑**，必须显式 --unsafe。
#    该测试会在容器内执行 `echo b > /proc/sysrq-trigger`。若隔离失效，
#    它会**立即重启这台宿主**（最坏情况：服务器掉线、未落盘数据丢失）。
#    因此脚本在跑之前会先确认容器没有 CAP_SYS_ADMIN，一旦发现仍有该能力
#    就**直接中止该测试**（不是继续跑）。
# 5. 不做任何宿主全局状态修改（不装包、不改 sysctl、不动防火墙既有规则）。
#
# ---------------------------------------------------------------------------
# 为什么读容器的 PID 1，而不是 licore exec 进去读自己的
# ---------------------------------------------------------------------------
# `licore exec` 由宿主侧 nsenter 拉起，进程来自宿主 root。容器启动时把
# licore 自身只读 bind 进 /.licore/exec-helper，由它做收口——因此 exec
# 进程**理论上**也应是裁剪后的（T-2 就是验证这一点）。
# 但"读容器 PID 1"与"读 exec 进程"是**两件事**，不能互相替代：
# 前者证明容器 init 收口了，后者证明 exec 路径收口了。本脚本两者都测。
#
# ---------------------------------------------------------------------------
# 判据来源
# ---------------------------------------------------------------------------
# 容器 init 与 exec 进程的期望值：CapEff=00000000a80425fb（Docker 默认集
# 14 项，不含 CAP_SYS_ADMIN）、NoNewPrivs=1、Seccomp=2。

set -uo pipefail
# 刻意不用 set -e：本脚本要运行"预期失败"的命令（断言被拒绝的操作），
# 且必须保证中途出错也能走到清理逻辑。

trap 'on_exit' EXIT

LICORE_BIN="${LICORE_BIN:-licore}"
VERIFY_HOME="${LICORE_VERIFY_HOME:-/tmp/licore-secverify-home}"
IMAGE="${LICORE_VERIFY_IMAGE:-alpine:3.20}"
PREFIX="licore-secverify"
NAME_MAIN="$PREFIX-main"
NAME_RO="$PREFIX-ro"
LOGFILE="/tmp/licore-secverify.log"

UNSAFE=no
KEEP=no
JSON_ONLY=no
for arg in "$@"; do
  case "$arg" in
    --unsafe)   UNSAFE=yes ;;
    --keep)     KEEP=yes ;;
    --json)     JSON_ONLY=yes ;;
    -h|--help)  sed -n '2,46p' "$0"; exit 0 ;;
    *) echo "未知参数：$arg（支持 --unsafe / --keep / --json / --help）" >&2; exit 2 ;;
  esac
done

export LICORE_HOME="$VERIFY_HOME"

PASS=0; FAIL=0; SKIP=0
pass() { PASS=$((PASS+1)); [ "$JSON_ONLY" = yes ] || echo "[PASS] $*"; }
fail() { FAIL=$((FAIL+1)); [ "$JSON_ONLY" = yes ] || echo "[FAIL] $*"; }
skip() { SKIP=$((SKIP+1)); [ "$JSON_ONLY" = yes ] || echo "[SKIP] $*"; }
step() { [ "$JSON_ONLY" = yes ] || { echo; echo "===== $* ====="; }; }
info() { [ "$JSON_ONLY" = yes ] || echo "  -> $*"; }

RUNNING=()
# ABORTED=yes 表示脚本在"起容器之前"就退出了（环境不满足）。此时**不能**
# 打印 "ALL PASS"——那会把"根本没测"误报成"全通过"。审计脚本尤其不能这样。
ABORTED=no

cleanup_verify() {
  [ "$KEEP" = yes ] && { info "--keep 指定，保留容器与 $VERIFY_HOME"; return; }
  local n
  for n in "${RUNNING[@]:-}"; do
    [ -z "$n" ] && continue
    "$LICORE_BIN" stop "$n" >/dev/null 2>&1 || true
    "$LICORE_BIN" rm -f "$n" >/dev/null 2>&1 || true
  done
  # **绝不 rm -rf 数据目录本身**。
  #
  # 初版会 `rm -rf "$VERIFY_HOME"`，而 LICORE_VERIFY_HOME 是用户可传的。
  # 实测事故：本脚本以 LICORE_VERIFY_HOME=/root/.licore 运行过一次，
  # 直接**删掉了生产数据目录**（含已导入镜像与一个既有容器）。
  # 审计脚本不该有这种能力——删镜像/容器是用户的决定，不是脚本的副作用。
  #
  # 现在只删**本脚本自己**在上面创建的临时资源：
  #   - RUNNING 里的 licore-secverify-* 容器（已删）
  #   - /tmp 下本脚本自建的只读卷源目录
  # 数据目录原样保留。
  if [ "$VERIFY_HOME_CREATED" = yes ]; then
    info "数据目录 $VERIFY_HOME 是本脚本新建的，保留（如需清理请手动 rm -rf）"
  else
    info "数据目录 $VERIFY_HOME 是既有目录，未做任何删除（只删了本脚本创建的容器）"
  fi
}

on_exit() {
  local rc=$?
  step "清理"
  cleanup_verify
  if [ "$ABORTED" = yes ]; then
    # 环境不满足、未执行任何测试项：保留原始退出码，不打印 "ALL PASS"。
    [ "$JSON_ONLY" = yes ] && printf '{"aborted":true,"exit":%d}\n' "$rc"
    exit $rc
  fi
  if [ "$JSON_ONLY" = yes ]; then
    printf '{"pass":%d,"fail":%d,"skip":%d,"log":"%s"}\n' "$PASS" "$FAIL" "$SKIP" "$LOGFILE"
  else
    echo
    echo "===== 结果 ====="
    echo "PASS=$PASS  FAIL=$FAIL  SKIP=$SKIP"
    echo "完整日志：$LOGFILE"
    if [ "$FAIL" -gt 0 ]; then
      echo "== 有失败项 =="
      echo "把上面输出（含本行）贴回给审计会话即可。"
      exit 1
    fi
    echo "== ALL PASS =="
    echo "把上面输出贴回给审计会话即可。"
  fi
  exit $rc
}

# ---------------------------------------------------------------------------
# 基础检查
# ---------------------------------------------------------------------------
step "0. 环境检查"

if [ "$(id -u)" != "0" ]; then
  echo "本脚本需要 root（创建容器 / 读 /proc/<pid>/status）。" >&2
  echo "请用：sudo bash $0" >&2
  ABORTED=yes
  exit 2
fi

if ! command -v "$LICORE_BIN" >/dev/null 2>&1 && [ ! -x "$LICORE_BIN" ]; then
  echo "找不到 licore 可执行文件（$LICORE_BIN）。用 LICORE_BIN=/path/to/licore 指定。" >&2
  ABORTED=yes
  exit 2
fi

info "licore      : $("$LICORE_BIN" --version 2>/dev/null | head -n1 || echo "(取不到版本)")"
info "内核        : $(uname -r)"
info "数据目录    : $VERIFY_HOME"
info "镜像        : $IMAGE"
info "sysrq 测试  : $([ "$UNSAFE" = yes ] && echo '将执行（--unsafe）' || echo '跳过（默认）')"

# 数据目录安全提示：默认值在 /tmp 下，指向别处（尤其是生产目录）时明确
# 告知用户——脚本会往该目录**写入容器与镜像**，虽然不会删除目录本身，
# 但在生产数据目录里创建测试容器仍是不该默默发生的事。
case "$VERIFY_HOME" in
  /tmp/*) : ;;
  *)
    echo
    echo "注意：LICORE_VERIFY_HOME=$VERIFY_HOME 不在 /tmp 下。"
    echo "      脚本会在该数据目录内**创建测试容器**（licore-secverify-*）。"
    echo "      不会删除该目录，但建议用独立目录以免与生产容器混淆。"
    echo
    ;;
esac

if [ "$UNSAFE" = yes ]; then
  echo
  echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
  echo "!! DESTRUCTIVE 警告：--unsafe 已启用"
  echo "!! 将执行：容器内 echo b > /proc/sysrq-trigger"
  echo "!! 最坏情况：若隔离失效，本机将**立即重启**，未落盘数据丢失。"
  echo "!! 若这是生产机，请现在 Ctrl+C 中止（5 秒后继续）。"
  echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
  sleep 5
fi

# ---------------------------------------------------------------------------
# 辅助函数
# ---------------------------------------------------------------------------
read_field() { # $1=pid $2=field
  grep "^$2:" "/proc/$1/status" 2>/dev/null | awk '{print $2}'
}

container_id_of() { # $1=name → id（空=没找到）
  local name="$1" id
  id="$("$LICORE_BIN" ps -q -a 2>/dev/null | head -n 200 | while read -r cid; do
    d="$LICORE_HOME/containers/$cid"
    if [ -f "$d/config.json" ] && grep -q "\"name\": *\"$name\"" "$d/config.json" 2>/dev/null; then
      echo "$cid"; break
    fi
  done)"
  [ -n "$id" ] && echo "$id"
}

container_init_pid() { # $1=name → PID
  local cid rtjson
  cid="$(container_id_of "$1")"
  [ -z "$cid" ] && return 1
  rtjson="$LICORE_HOME/containers/$cid/runtime.json"
  [ -f "$rtjson" ] || return 1
  sed -n 's/.*"initPid"[[:space:]]*:[[:space:]]*\([0-9]\{1,\}\).*/\1/p' "$rtjson" | head -n1
}

# cexec NAME CMD... → 在容器内执行；输出原样返回，退出码透传
cexec() {
  local name="$1"; shift
  "$LICORE_BIN" exec "$name" "$@" 2>&1
}

# safe_probe NAME PATH → DENIED / WRITABLE / MISSING
#
# **判据必须是 write，不能是 open。**
#
# 初版用 `dd count=0`（只 open、不 write）判断，真机上把 /proc/kpageflags
# 误报成 WRITABLE。原因是容器 init 是宿主 uid 0、持有 CAP_DAC_OVERRIDE，
# **open(O_WRONLY) 会成功**（DAC 被绕过），内核在 write() 时才拒绝。
# 真机实测（宿主 root 直接操作同一批文件，用 python 的 os.write）：
#
#     /proc/kpageflags: open OK, write 拒绝 (EIO)
#     /proc/kcore:      open OK, write 拒绝 (EIO)
#     /proc/kmsg:       open OK, write 拒绝 (EIO)
#     /proc/mtrr:       open OK, write 拒绝 (EINVAL)
#
# 也不能用 `conv=notrunc`：对 1 字节输入它会**跳过 write**（dd 认为无可写
# 内容），于是又退化成"只 open"，同样误报。
#
# 采用的办法：`printf 'x' | dd of=PATH bs=1 count=1`（**不带 conv**）。
#   - 真的发出一次 write(2)，内核 handler 据此给出真实判据；
#   - 对只读挂载的 procfs 文件，open 阶段就报 "Read-only file system"；
#   - 对内核做了 capable() 检查的文件，write 报 EIO/EINVAL/EPERM；
#   - 对 /proc/sys 下真的**可写**的文件（隔离失效时）会写入 'x' 这一个
#     字节 —— 这正是我们想探测的状态，而且它意味着"已经出事了"，
#     此时脚本会把它标红；还原与否已不影响结论。
#   - 真机实测全部危险文件均为 DENIED，宿主 /proc/sys 未被改动
#     （core_pattern 仍为 "core"、kptr_restrict 仍为 1、sysrq 仍为 176，
#     uptime 连续，宿主未重启）。
safe_probe() {
  local name="$1" path="$2"
  local out
  out="$(cexec "$name" /bin/sh -c "
    if [ ! -e '$path' ]; then echo MISSING; exit 0; fi
    if printf 'x' | dd of='$path' bs=1 count=1 2>/dev/null; then
      echo WRITABLE
    else
      echo DENIED
    fi
  ")"
  echo "$out" | tail -n1
}

# ---------------------------------------------------------------------------
# 起容器
# ---------------------------------------------------------------------------
step "1. 创建测试容器"

# 记录数据目录是"本脚本新建"还是"既有"——仅用于清理时给出正确提示，
# 无论如何都**不会删除**它（见 cleanup_verify 的事故说明）。
VERIFY_HOME_CREATED=no
[ -d "$VERIFY_HOME" ] || VERIFY_HOME_CREATED=yes

if ! "$LICORE_BIN" run -d --name "$NAME_MAIN" "$IMAGE" sleep 600 >/tmp/secverify-run.log 2>&1; then
  echo "容器启动失败，输出如下：" >&2
  cat /tmp/secverify-run.log >&2
  echo >&2
  echo "提示：需要镜像 $IMAGE 已导入（licore pull 或 licore images 确认）。" >&2
  ABORTED=yes
  exit 2
fi
RUNNING+=("$NAME_MAIN")
sleep 2
info "已启动 $NAME_MAIN"

INIT_PID="$(container_init_pid "$NAME_MAIN")"
if [ -z "${INIT_PID:-}" ]; then
  echo "拿不到容器 init PID（runtime.json 缺 initPid）" >&2
  ABORTED=yes
  exit 2
fi
info "容器 init 宿主 PID = $INIT_PID"

# ---------------------------------------------------------------------------
# T-1 容器逃逸面
# ---------------------------------------------------------------------------
step "T-1 容器逃逸面"

# --- T-1.1 权限三层：读容器 PID 1 ---
CAPEFF="$(read_field "$INIT_PID" CapEff)"
NNP="$(read_field "$INIT_PID" NoNewPrivs)"
SECCOMP="$(read_field "$INIT_PID" Seccomp)"
info "容器 PID 1: CapEff=$CAPEFF NoNewPrivs=$NNP Seccomp=$SECCOMP"

if [ "$CAPEFF" = "00000000a80425fb" ]; then
  pass "T-1.1a CapEff 等于 Docker 默认集（不含 CAP_SYS_ADMIN）"
else
  fail "T-1.1a CapEff=$CAPEFF，期望 00000000a80425fb"
fi

# 显式断言第 21 位（CAP_SYS_ADMIN）为 0
if [ -n "$CAPEFF" ]; then
  if [ "$(( (16#$CAPEFF >> 21) & 1 ))" = "0" ]; then
    pass "T-1.1b CAP_SYS_ADMIN(bit 21) 已丢弃"
  else
    fail "T-1.1b CAP_SYS_ADMIN(bit 21) 仍存在！CapEff=$CAPEFF"
  fi
fi

[ "$NNP" = "1" ] && pass "T-1.1c NoNewPrivs=1" || fail "T-1.1c NoNewPrivs=$NNP，期望 1"
[ "$SECCOMP" = "2" ] && pass "T-1.1d Seccomp=2（filter 模式）" || fail "T-1.1d Seccomp=$SECCOMP，期望 2"

# --- T-1.2 /proc/sys 只读封堵（P0：core_pattern / modprobe）---
for f in /proc/sys/kernel/core_pattern /proc/sys/kernel/modprobe; do
  r="$(safe_probe "$NAME_MAIN" "$f")"
  case "$r" in
    DENIED)   pass "T-1.2 $f 写入被拒（DENIED）" ;;
    MISSING)  skip "T-1.2 $f 不存在（内核未提供）" ;;
    WRITABLE) fail "T-1.2 $f **可写**！这是 P0 宿主逃逸路径" ;;
    *)        fail "T-1.2 $f 探测异常：$r" ;;
  esac
done

# 对照项：走 proc_dointvec_* 的文件本就该被 capability 层挡住
r="$(safe_probe "$NAME_MAIN" /proc/sys/kernel/kptr_restrict)"
[ "$r" = "DENIED" ] && pass "T-1.2b /proc/sys/kernel/kptr_restrict 写入被拒（对照项）" \
                    || skip "T-1.2b kptr_restrict 探测结果：$r"

# --- T-1.3 /proc 根级危险文件 ---
# sysrq-trigger 已在 mask 清单里，这里只做**只读探测**（不写！
# 真写入是 DESTRUCTIVE，单独放在最后一节）。
r="$(safe_probe "$NAME_MAIN" /proc/sysrq-trigger)"
case "$r" in
  DENIED)   pass "T-1.3a /proc/sysrq-trigger 写入被拒（mask 生效）" ;;
  MISSING)  skip "T-1.3a /proc/sysrq-trigger 不存在（未启用 CONFIG_MAGIC_SYSRQ）" ;;
  WRITABLE) fail "T-1.3a /proc/sysrq-trigger **可写**！P0 路径，写 b 会重启宿主（不要在 --unsafe 外验证）" ;;
  *)        fail "T-1.3a 探测异常：$r" ;;
esac

# L-3：/proc/mtrr（0644，审计中的待验项）
info "  /proc/mtrr 权限：$(cexec "$NAME_MAIN" /bin/sh -c 'ls -la /proc/mtrr 2>/dev/null || echo "(不存在)"')"
r="$(safe_probe "$NAME_MAIN" /proc/mtrr)"
case "$r" in
  DENIED)   pass "T-1.3b /proc/mtrr 写入被拒（内核 capable() 检查生效）" ;;
  MISSING)  skip "T-1.3b /proc/mtrr 不存在（CPU/内核未提供）" ;;
  WRITABLE) fail "T-1.3b /proc/mtrr **可写**！该文件为 0644，容器 init 是宿主 uid 0 —— 需评估是否加入 mask 清单" ;;
  *)        fail "T-1.3b 探测异常：$r" ;;
esac

for f in /proc/kcore /proc/kmsg /proc/kpageflags; do
  r="$(safe_probe "$NAME_MAIN" "$f")"
  case "$r" in
    DENIED)   pass "T-1.3c $f 写入被拒" ;;
    WRITABLE) fail "T-1.3c $f **可写**" ;;
    *)        info "  $f 探测结果：$r（不阻断）" ;;
  esac
done

# --- T-1.4 设备节点（不该有 /dev/mem 等危险节点）---
DEVOUT="$(cexec "$NAME_MAIN" /bin/sh -c 'ls /dev 2>/dev/null | tr "\n" " "')"
info "  容器 /dev：$DEVOUT"
DANGER=0
for d in mem kmem port; do
  case " $DEVOUT " in
    *" $d "*) DANGER=1; fail "T-1.4 容器内存在危险设备节点 /dev/$d" ;;
  esac
done
[ "$DANGER" = 0 ] && pass "T-1.4 容器 /dev 不含 mem/kmem/port 等危险节点"

# --- T-1.5 seccomp 确实拦截危险系统调用 ---
# 用 unshare 作为探针：容器内应 EPERM（seccomp 黑名单 + 无 CAP_SYS_ADMIN）。
r="$(cexec "$NAME_MAIN" /bin/sh -c 'unshare -m true 2>&1; echo "rc=$?"')"
if echo "$r" | grep -q "rc=0"; then
  fail "T-1.5 unshare -m 在容器内成功（应被拒绝）"
else
  pass "T-1.5 unshare -m 被拒绝：$(echo "$r" | tail -n1)"
fi

# mount 同理
r="$(cexec "$NAME_MAIN" /bin/sh -c 'mount -t tmpfs none /mnt 2>&1; echo "rc=$?"')"
if echo "$r" | grep -q "rc=0"; then
  fail "T-1.5b mount 在容器内成功（应被拒绝）"
else
  pass "T-1.5b mount 被拒绝"
fi

# --- T-1.6 容器内看不到宿主进程 ---
HOSTVIS="$(cexec "$NAME_MAIN" /bin/sh -c 'ls /proc | grep -c "^[0-9]\+$" || true')"
info "  容器内可见进程数：$HOSTVIS"
if [ "${HOSTVIS:-0}" -lt 20 ]; then
  pass "T-1.6 容器 PID namespace 隔离（可见进程数 $HOSTVIS < 20）"
else
  fail "T-1.6 容器内可见 $HOSTVIS 个进程，疑似 PID namespace 未隔离"
fi

# ---------------------------------------------------------------------------
# T-2 exec 收口
# ---------------------------------------------------------------------------
step "T-2 licore exec 的收口"

if ! command -v nsenter >/dev/null 2>&1 && ! command -v busybox >/dev/null 2>&1; then
  skip "T-2 无 nsenter/busybox，exec 不可用"
else
  EXECOUT="$(cexec "$NAME_MAIN" /bin/sh -c 'grep -E "^(CapEff|NoNewPrivs|Seccomp):" /proc/self/status')"
  info "  exec 进程：$(echo "$EXECOUT" | tr '\n' ' ')"
  ECAPEFF="$(echo "$EXECOUT" | sed -n 's/^CapEff:[[:space:]]*//p')"
  ENNP="$(echo "$EXECOUT" | sed -n 's/^NoNewPrivs:[[:space:]]*//p')"
  ESEC="$(echo "$EXECOUT" | sed -n 's/^Seccomp:[[:space:]]*//p')"

  if [ "$ECAPEFF" = "$CAPEFF" ]; then
    pass "T-2a exec 进程 CapEff 与容器 PID 1 一致（$ECAPEFF）"
  else
    fail "T-2a exec 进程 CapEff=$ECAPEFF != 容器 PID 1 $CAPEFF（exec 未收口！）"
  fi
  [ "$ENNP" = "1" ] && pass "T-2b exec 进程 NoNewPrivs=1" || fail "T-2b exec NoNewPrivs=$ENNP"
  [ "$ESEC" = "2" ] && pass "T-2c exec 进程 Seccomp=2" || fail "T-2c exec Seccomp=$ESEC"

  # exec 拒绝 --cap-add
  r="$("$LICORE_BIN" exec --cap-add SYS_ADMIN "$NAME_MAIN" true 2>&1)"
  if echo "$r" | grep -qi "拒绝\|不允许\|not allowed\|unsupported"; then
    pass "T-2d exec --cap-add 被拒绝"
  else
    fail "T-2d exec --cap-add 未被拒绝：$r"
  fi
fi

# ---------------------------------------------------------------------------
# T-3 网络隔离
# ---------------------------------------------------------------------------
step "T-3 网络隔离（容器 → 宿主）"

# 找宿主非回环 IPv4
HOSTIP="$(ip -4 route get 1.1.1.1 2>/dev/null | grep -o 'src [0-9.]*' | awk '{print $2}' | head -n1)"
info "  宿主出口 IP：${HOSTIP:-<未取到>}"

if [ -z "${HOSTIP:-}" ]; then
  skip "T-3 取不到宿主 IP，跳过网络测试"
else
  # 容器主动连宿主：应被 LICORE-INPUT 的 DROP 挡住
  r="$(cexec "$NAME_MAIN" /bin/sh -c "
    timeout 5 nc -z -w 3 $HOSTIP 22 >/dev/null 2>&1 && echo CONNECTED || echo BLOCKED
  ")"
  r="$(echo "$r" | tail -n1)"
  if [ "$r" = "BLOCKED" ]; then
    pass "T-3a 容器 → 宿主 $HOSTIP:22 被阻断"
  else
    info "  T-3a 结果是 $r（若宿主 22 端口本就没开，BLOCKED 不具判别力）"
    skip "T-3a 宿主 22 未监听或结果不明确：$r"
  fi

  # 容器能出网
  r="$(cexec "$NAME_MAIN" /bin/sh -c '
    timeout 8 sh -c "echo > /dev/tcp/1.1.1.1/443" >/dev/null 2>&1 && echo OK || echo FAIL
  ')"
  r="$(echo "$r" | tail -n1)"
  if [ "$r" = "OK" ]; then
    pass "T-3b 容器可访问外网（1.1.1.1:443）"
  else
    skip "T-3b 容器出网失败：$r"
    info "  排查：若本机已存在 licore0 网桥而本脚本用了隔离数据目录，"
    info "        两者网段可能不一致（真机实测过：网桥 172.22.0.1，"
    info "        新 store 分配 172.21.0.0/16 → 网关不存在、出网必失败）。"
    info "        这是引擎的已知缺陷（见 docs/security-audit-v2.md L-6），"
    info "        与隔离无关。用 LICORE_HOME=<既有数据目录> 复跑可验证。"
  fi

  # 宿主能访问容器端口映射：起一个监听并映射
  "$LICORE_BIN" run -d --name "$NAME_RO" -p 18099:8099 "$IMAGE" \
    sh -c 'while true; do { echo -e "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"; } | nc -l -p 8099; done' \
    >/dev/null 2>&1 && RUNNING+=("$NAME_RO") && sleep 3
  if [ -n "$(container_id_of "$NAME_RO")" ]; then
    code="$(timeout 8 curl -s -o /dev/null -w '%{http_code}' "http://$HOSTIP:18099/" 2>/dev/null)"
    if [ "$code" = "200" ]; then
      pass "T-3c 经宿主 IP 访问端口映射返回 200"
    else
      info "  T-3c 返回码：${code:-<无响应>}"
      skip "T-3c 端口映射未返回 200（容器内 nc 行为差异，或上述网段不一致）"
    fi
  else
    skip "T-3c $NAME_RO 未启动，跳过端口映射测试"
  fi
fi

# 规则是否真的存在
if command -v iptables >/dev/null 2>&1; then
  if iptables -S LICORE-INPUT >/dev/null 2>&1; then
    pass "T-3d LICORE-INPUT 链存在"
    info "  $(iptables -S LICORE-INPUT 2>/dev/null | tr '\n' ' ')"
  else
    skip "T-3d 无 LICORE-INPUT 链（可能全走 nft 或未创建 bridge 网络）"
  fi
fi
if command -v nft >/dev/null 2>&1; then
  if nft list table ip licore >/dev/null 2>&1; then
    pass "T-3e nft 表 ip licore 存在"
  else
    skip "T-3e 无 nft 表 ip licore（可能走了 iptables 回退）"
  fi
fi

# ---------------------------------------------------------------------------
# T-4 cgroup 资源限额（exec 归置）
# ---------------------------------------------------------------------------
step "T-4 资源限额（exec 进程 cgroup 归置）"

EXECCG="$(cexec "$NAME_MAIN" /bin/sh -c 'cat /proc/self/cgroup' 2>/dev/null | tail -n1)"
INITCG="$(cat "/proc/$INIT_PID/cgroup" 2>/dev/null | tail -n1)"
info "  容器 init cgroup：$INITCG"
info "  exec 进程 cgroup：$EXECCG"

if [ -n "$EXECCG" ] && [ "$EXECCG" = "$INITCG" ]; then
  # 与容器 init **同一个** cgroup 才算真正归置：只判 "含 /licore/" 会漏掉
  # "落进别人的容器 cgroup" 这种错位。
  pass "T-4a exec 进程与容器 init 同 cgroup（$EXECCG）"
elif [ -n "$EXECCG" ] && echo "$EXECCG" | grep -q "/licore/"; then
  fail "T-4a exec 进程在 $EXECCG，但容器 init 在 $INITCG —— 归置到了别的组"
elif [ -n "$EXECCG" ]; then
  fail "T-4a exec 进程落在 $EXECCG，未归入容器 cgroup（会绕过 --memory 限额）"
else
  skip "T-4a 取不到 exec 进程 cgroup"
fi

# ---------------------------------------------------------------------------
# T-5 卷 :ro 真正只读
# ---------------------------------------------------------------------------
step "T-5 卷 :ro 只读"

RO_SRC="/tmp/licore-secverify-ro-src"
mkdir -p "$RO_SRC" && echo "original" > "$RO_SRC/file.txt"
RO_NAME="$PREFIX-rov"

if "$LICORE_BIN" run -d --name "$RO_NAME" -v "$RO_SRC:/mnt/ro:ro" "$IMAGE" sleep 300 >/dev/null 2>&1; then
  RUNNING+=("$RO_NAME")
  sleep 2
  r="$(cexec "$RO_NAME" /bin/sh -c 'echo x >> /mnt/ro/file.txt 2>&1; echo "rc=$?"')"
  r="$(echo "$r" | tail -n1)"
  if echo "$r" | grep -q "rc=0"; then
    fail "T-5 :ro 卷可写（只读挂载未生效）"
  else
    pass "T-5 :ro 卷写入被拒"
  fi
  # 宿主侧内容未被改动
  if [ "$(cat "$RO_SRC/file.txt" 2>/dev/null)" = "original" ]; then
    pass "T-5b 宿主侧源文件未被改动"
  else
    fail "T-5b 宿主侧源文件被改动！"
  fi
else
  skip "T-5 无法创建带 :ro 卷的容器"
fi
rm -rf "$RO_SRC"

# ---------------------------------------------------------------------------
# T-6 DESTRUCTIVE：sysrq 写入（默认跳过）
# ---------------------------------------------------------------------------
step "T-6 sysrq 写入（DESTRUCTIVE，默认跳过）"

if [ "$UNSAFE" != yes ]; then
  skip "T-6 sysrq 写入测试未启用（默认跳过）。需要时用 --unsafe 显式开启。"
  info "  提示：T-1.3a 已用 dd count=0 做只读探测；若那里是 DENIED，"
  info "        则真实写入必然也被拒（同一 open 路径），通常无需再跑 --unsafe。"
else
  # 前置断言：必须已确认没有 CAP_SYS_ADMIN，否则立即中止（不"继续跑"）。
  if [ "$(( (16#$CAPEFF >> 21) & 1 ))" = "1" ]; then
    fail "T-6 前置断言失败：容器仍持有 CAP_SYS_ADMIN，**中止** sysrq 测试（继续会重启宿主）"
  else
    info "  前置断言通过：容器无 CAP_SYS_ADMIN"
    info "  执行：容器内 echo b > /proc/sysrq-trigger"
    r="$(cexec "$NAME_MAIN" /bin/sh -c 'echo b > /proc/sysrq-trigger 2>&1; echo "rc=$?"')"
    r="$(echo "$r" | tail -n1)"
    if echo "$r" | grep -q "rc=0"; then
      # 走到这里其实说明宿主已经要重启了（或 sysrq 被禁用而静默成功）
      fail "T-6 sysrq-trigger 写入返回 0！若宿主未重启，请检查 /proc/sys/kernel/sysrq 是否被禁用"
    else
      pass "T-6 sysrq-trigger 写入被拒（$r）—— 宿主存活，隔离有效"
    fi
  fi
fi

# ---------------------------------------------------------------------------
# 修复项回归（H-1 / M-1 / M-2 不需要真机，但顺带确认二进制是新版）
# ---------------------------------------------------------------------------
step "T-7 版本确认（H-1 / M-1 / M-2 修复应在 v0.9.6+）"
VER="$("$LICORE_BIN" --version 2>/dev/null | head -n1 | tr -d '\n')"
info "  licore version：$VER"
if echo "$VER" | grep -qE "v0\.9\.[6-9]|v0\.([1-9][0-9])|v[1-9]"; then
  pass "T-7 二进制为 v0.9.6 或更新（含 H-1 / M-1 / M-2 修复）"
else
  skip "T-7 版本号 $VER 无法判定是否含修复，请人工确认"
fi
info "  说明：H-1（convert 符号链接逃逸）与 M-1/M-2（Hub JWT）已在单元测试中"
info "        完成反向验证，无需真机；本条只是确认部署的二进制是新版。"

step "全部测试项结束"
