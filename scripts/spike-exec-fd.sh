#!/usr/bin/env bash
# Copyright (C) 2026 LiStudioorg
# SPDX-License-Identifier: AGPL-3.0-only
#
# SC-1 spike：验证 licore exec 收口方案的可行性。
#
# 背景（为什么需要 spike，而不是直接写实现）
#   exec 的路径是：宿主 CLI → fork nsenter → setns 进容器 → **chroot 到容器 root**
#   → execve 目标命令。收口（no_new_privs / cap-drop / seccomp）必须插在
#   chroot 之后、execve 目标命令之前，因此需要有个「helper」在容器内被启动。
#
#   而 nsenter 的 -r/ 已经把 root 切到容器，所以 helper 的路径必须在**容器内可达**。
#   这是整个设计的死结，必须实测而非推断。
#
# 三条候选路径
#   (a) helper 经 /proc/self/fd/<N> 启动：CLI 打开自身二进制拿 fd，子进程继承，
#       nsenter 执行 "/proc/self/fd/<N>"。**依赖容器内有 procfs**（LiCore 会挂）。
#   (b) helper 在 run 时 bind 进容器（只读）。不依赖 procfs，代价是注入一个文件。
#   (c) 已排除：nsenter 不支持 execveat，无法直接用 fd 启动 helper。
#
# 顺带发现（已在本机验证，见脚本 Q4）：execveat(fd,"",argv,envp,AT_EMPTY_PATH)
# 可用，且**不依赖 procfs**。它可用于 helper 内部 execve 用户命令，
# 但**不能**用来启动 helper 本身。
#
# 用法：
#   sudo bash scripts/spike-exec-fd.sh              # 完整
#   bash scripts/spike-exec-fd.sh --no-container    # 只做宿主侧
#
# 安全：只在自己的临时目录操作，不碰任何既有容器。

set -uo pipefail

NO_CONTAINER=no
[ "${1:-}" = "--no-container" ] && NO_CONTAINER=yes

SPIKE_DIR="$(mktemp -d /tmp/licore-spike-XXXXXX)"
PASS=0; FAIL=0; SKIP=0
pass() { PASS=$((PASS+1)); echo "[PASS] $*"; }
fail() { FAIL=$((FAIL+1)); echo "[FAIL] $*"; }
skip() { SKIP=$((SKIP+1)); echo "[SKIP] $*"; }
step() { echo; echo "===== $* ====="; }
info() { echo "  -> $*"; }

cleanup() {
  [ -n "${SPIKE_MNT:-}" ] && umount "$SPIKE_MNT/proc" 2>/dev/null
  chmod -R u+w "$SPIKE_DIR" 2>/dev/null
  rm -rf "$SPIKE_DIR"
}
trap cleanup EXIT

Q1_RESULT="not-run"   # (a) 的决定性结论
Q4_RESULT="not-run"   # execveat 可用性

echo "spike 目录：$SPIKE_DIR"
echo "uid=$(id -u)"

# ---------------------------------------------------------------------------
step "0) 环境探测"
# ---------------------------------------------------------------------------
command -v nsenter >/dev/null 2>&1 && pass "util-linux nsenter：$(command -v nsenter)" || skip "无 util-linux nsenter"
if command -v busybox >/dev/null 2>&1 && busybox nsenter --help >/dev/null 2>&1; then
  pass "busybox nsenter 可用"
else
  skip "无 busybox nsenter"
fi
info "/proc/self/fd 可读：$(ls /proc/self/fd >/dev/null 2>&1 && echo yes || echo no)"

# ---------------------------------------------------------------------------
step "Q4: execveat(fd,'',argv,envp,AT_EMPTY_PATH) 是否可用（不依赖 procfs）"
#
# 用 python 直接发 syscall（不经 Go，避免参数编组干扰）。确认 322 号在 x86_64
# 上就是 execveat；若成功会打印标记字符串。
# ---------------------------------------------------------------------------
if command -v python3 >/dev/null 2>&1; then
  Q4_OUT="$(python3 - <<'PY' 2>&1 | tail -1
import ctypes, os
libc = ctypes.CDLL(None, use_errno=True)
f = os.open('/bin/echo', os.O_RDONLY)
argv = (ctypes.c_char_p * 2)(b'echo', b'EXECVEAT-OK')
envp = (ctypes.c_char_p * 1)(b'PATH=/bin:/usr/bin')
empty = ctypes.c_char_p(b'')
libc.syscall.restype = ctypes.c_long
libc.syscall(ctypes.c_long(322), ctypes.c_long(f),
             ctypes.cast(empty, ctypes.c_void_p),
             ctypes.cast(argv, ctypes.c_void_p),
             ctypes.cast(envp, ctypes.c_void_p),
             ctypes.c_long(0x1000))
print('EXECVEAT-FAILED errno=%d' % ctypes.get_errno())
PY
)"
  if echo "$Q4_OUT" | grep -q "EXECVEAT-OK"; then
    Q4_RESULT="pass"
    pass "Q4 成立：execveat 可用且不依赖 procfs"
  else
    Q4_RESULT="fail"
    fail "Q4 不成立：$Q4_OUT"
  fi
else
  skip "无 python3，Q4 跳过"
fi

# ---------------------------------------------------------------------------
step "Q1: nsenter -r/ 之后能否经 /proc/self/fd/N 执行 fd 持有的二进制"
#
# 这是方案 (a) 成立与否的关键。做法：造一个新 root，里面**挂 procfs**、
# 但**不含** payload；payload 只以继承的 fd 形式存在；chroot 进去后执行
# /proc/self/fd/9。
# ---------------------------------------------------------------------------
if [ "$(id -u)" -ne 0 ]; then
  Q1_RESULT="skip-noroot"
  skip "Q1 需要 root（mount procfs + chroot），当前 uid=$(id -u)"
  info "请以 root 在服务器上重跑本脚本"
else
  ROOT="$SPIKE_DIR/root"
  mkdir -p "$ROOT/proc"
  PAYLOAD_SRC=""
  for cand in /bin/busybox /usr/bin/busybox; do
    [ -x "$cand" ] && PAYLOAD_SRC="$cand" && break
  done
  if [ -z "$PAYLOAD_SRC" ]; then
    Q1_RESULT="skip-nopayload"
    skip "找不到 busybox 作静态 payload"
  else
    cp "$PAYLOAD_SRC" "$SPIKE_DIR/payload"; chmod +x "$SPIKE_DIR/payload"
    if mount -t proc proc "$ROOT/proc" 2>/dev/null; then
      SPIKE_MNT="$ROOT"; pass "procfs 已挂入新 root"
    else
      Q1_RESULT="skip-mount"
      skip "无法 mount procfs；fd 方案在该环境不可用，应考虑方案 (b)"
    fi
    if [ -n "${SPIKE_MNT:-}" ]; then
      RESULT="$(exec 9<"$SPIKE_DIR/payload"; chroot "$ROOT" /proc/self/fd/9 echo 2>&1 || true)"
      if echo "$RESULT" | grep -q "BusyBox\|Usage\|applet"; then
        Q1_RESULT="pass"; pass "Q1 成立：chroot 后可经 /proc/self/fd/N 执行 fd 持有的二进制"
      else
        Q1_RESULT="fail"; fail "Q1 不成立：$RESULT"
        info "应退化为方案 (b)：run 时 bind 只读 helper 进容器"
      fi
    fi
  fi
fi

# ---------------------------------------------------------------------------
step "Q2: 容器内 /proc/self/fd 可读（licore 实测）"
# ---------------------------------------------------------------------------
if [ "$NO_CONTAINER" = yes ]; then
  skip "指定了 --no-container"
elif ! command -v licore >/dev/null 2>&1; then
  skip "PATH 里没有 licore"
elif [ "$(id -u)" -ne 0 ]; then
  skip "需要 root 才能起容器"
else
  if licore run -d --name spike-probe alpine:3.20 sleep 30 >/dev/null 2>&1; then
    OUT="$(licore exec spike-probe /bin/sh -c 'ls -l /proc/self/fd/ | head -5' 2>&1 || true)"
    if echo "$OUT" | grep -qE "[0-9]+ ->"; then
      pass "Q2 成立：容器内 /proc/self/fd 可读"
      info "$(echo "$OUT" | head -2 | tr '\n' ' ')"
    else
      fail "Q2：容器内 /proc/self/fd 不可读：$OUT"
    fi
    licore stop spike-probe >/dev/null 2>&1; licore rm -f spike-probe >/dev/null 2>&1
  else
    skip "无法启动探测容器（需要 alpine:3.20 镜像与 root）"
  fi
fi

# ---------------------------------------------------------------------------
step "Q3: busybox nsenter 支持 -t/-r/-w（后备路径）"
# ---------------------------------------------------------------------------
if command -v busybox >/dev/null 2>&1; then
  HELP="$(busybox nsenter --help 2>&1 || true)"; ok_all=yes
  for opt in "-t" "-r" "-w"; do
    echo "$HELP" | grep -q -- "$opt" || { ok_all=no; info "缺 $opt"; }
  done
  if [ "$ok_all" = yes ]; then pass "Q3 成立：busybox nsenter 短选项齐全"; else fail "Q3：busybox nsenter 选项不全"; fi
  info "（busybox nsenter 不支持长选项，必须用短选项）"
else
  skip "无 busybox"
fi

# ---------------------------------------------------------------------------
step "结论"
# ---------------------------------------------------------------------------
echo "PASS=$PASS FAIL=$FAIL SKIP=$SKIP"
echo
# 结论必须基于决定性项（Q1）**确实执行过**，而不是"没有 FAIL"。
# 之前踩过的坑：Q1 被 skip 时仍打印"全部通过"，是典型的静默假阳性。
case "$Q1_RESULT" in
  pass) echo "== Q1 成立 → 采用方案 (a)：helper 经 /proc/self/fd/<N> 启动 ==" ;;
  fail) echo "== Q1 不成立 → 采用方案 (b)：run 时 bind 只读 helper 进容器 ==" ;;
  skip-mount) echo "== 结论未定：Q1 因无法 mount procfs 未执行 =="
              echo "   这本身是信号——fd 方案依赖目标 root 内有 procfs，"
              echo "   宿主若不允许挂载，应直接考虑方案 (b)。" ;;
  *) echo "== 结论未定：Q1 未执行（$Q1_RESULT） =="
     echo "   请以 root 在服务器上重跑，否则不要据此选方案。" ;;
esac
echo
case "$Q4_RESULT" in
  pass) echo "附：execveat 可用 → helper 内部 execve 用户命令可用它，进一步减少对 /proc 的依赖。" ;;
  fail) echo "附：execveat 不可用 → helper 内部仍走普通 execve（路径在容器内解析，正常可用）。" ;;
  *)    echo "附：execveat 未测。" ;;
esac
echo
echo "把以上完整输出回贴即可。"
