#!/usr/bin/env bash
# Copyright (C) 2026 LiStudioorg
# SPDX-License-Identifier: AGPL-3.0-only
#
# licore 容器权限隔离真机验证脚本（对应 P0-1 no_new_privs + P0-2 capability 裁剪）。
#
# 用法（以 root 运行）：
#   sudo bash scripts/verify-capabilities.sh
#   sudo LICORE_BIN=/usr/local/bin/licore bash scripts/verify-capabilities.sh
#   sudo bash scripts/verify-capabilities.sh --unsafe     # 额外跑 sysrq 测试（见下）
#   sudo bash scripts/verify-capabilities.sh --keep       # 失败时保留容器便于排查
#
# ---------------------------------------------------------------------------
# 安全约束（重要，请先读）
# ---------------------------------------------------------------------------
# 1. **只碰本脚本自己创建的容器**：全部以 licore-verify- 前缀命名，清理时也只按
#    这些确切名字删除。脚本**从不**执行 docker stop/rm 作用于其他容器，
#    也从不修改 docker 配置。宿主机上正在跑的 new-api 等容器不受任何影响。
# 2. **数据目录隔离**：全程使用独立的 LICORE_HOME=/tmp/licore-verify-home，
#    不读也不写用户真实的 ~/.licore。
# 3. **sysrq 测试默认不跑**，必须显式 --unsafe。原因：该测试写入
#    /proc/sysrq-trigger，若权限隔离没生效，它会**真的重启这台服务器**。
#    脚本在执行前会先确认容器没有 CAP_SYS_ADMIN；一旦发现仍有该能力就
#    **直接中止该测试**（不是继续跑）。
# 4. Docker 对比项只使用 --rm 的一次性容器，且不会 pull 新镜像（只用本地已有的）。
#
# ---------------------------------------------------------------------------
# 为什么读容器的 PID 1，而不是 licore exec 进去读自己的
# ---------------------------------------------------------------------------
# `licore exec` 由宿主侧的 nsenter 拉起（internal/execns），继承的是**宿主
# root 的完整能力**，与容器 init 被裁剪成什么无关。因此 `licore exec <容器>
# grep CapEff /proc/self/status` 读到的是宿主的满能力位图，会让人误判为
# "修复没生效"。本脚本一律读 <数据目录>/containers/<id>/runtime.json 里的
# initPid，再读宿主机视角的 /proc/<initPid>/status。
# exec 的这条缺口是已知且独立的，不在本次修复范围内。

set -uo pipefail

# 刻意不用 set -e：本脚本要运行"预期失败"的命令（断言被拒绝的操作），
# 且必须保证中途出错也能走到清理逻辑，不能提前退出留下容器。
trap 'on_exit' EXIT

LICORE_BIN="${LICORE_BIN:-licore}"
VERIFY_HOME="${LICORE_VERIFY_HOME:-/tmp/licore-verify-home}"
IMAGE="${LICORE_VERIFY_IMAGE:-alpine:3.20}"
PREFIX="licore-verify"
NAME_DEFAULT="$PREFIX-default"
NAME_ADD="$PREFIX-add"
NAME_DROPALL="$PREFIX-dropall"
DOCKER_CMP="$PREFIX-docker-cmp"
LOGFILE="/tmp/licore-verify-capabilities.log"

UNSAFE=no
KEEP=no
for arg in "$@"; do
  case "$arg" in
    --unsafe) UNSAFE=yes ;;
    --keep)   KEEP=yes ;;
    -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "未知参数：$arg（支持 --unsafe / --keep / --help）" >&2; exit 2 ;;
  esac
done

# 隔离数据目录：所有 licore 命令都只认这里。
export LICORE_HOME="$VERIFY_HOME"

PASS=0; FAIL=0; SKIP=0
pass() { PASS=$((PASS+1)); echo "[PASS] $*"; }
fail() { FAIL=$((FAIL+1)); echo "[FAIL] $*"; }
skip() { SKIP=$((SKIP+1)); echo "[SKIP] $*"; }
step() { echo; echo "===== $* ====="; }
info() { echo "  -> $*"; }

RUNNING=()   # 记录本脚本创建的容器，仅这些会被清理

# ---------------------------------------------------------------------------
# 清理：只删本脚本创建的容器与临时目录。
# ---------------------------------------------------------------------------
cleanup_verify() {
  [ "$KEEP" = yes ] && { echo "  -> --keep 指定，保留容器与 $VERIFY_HOME"; return; }
  local n
  for n in "${RUNNING[@]:-}"; do
    [ -z "$n" ] && continue
    "$LICORE_BIN" stop "$n" >/dev/null 2>&1 || true
    "$LICORE_BIN" rm -f "$n" >/dev/null 2>&1 || true
  done
  # Docker 对比容器（只删这个确切名字，绝不碰其他容器）。
  if command -v docker >/dev/null 2>&1; then
    docker rm -f "$DOCKER_CMP" >/dev/null 2>&1 || true
  fi
  # **绝不 rm -rf 数据目录本身**（AGENTS.md《操作安全规范》规则 1/4）。
  #
  # 本行原为 `rm -rf "$VERIFY_HOME"`，而 VERIFY_HOME 来自用户可传的
  # LICORE_VERIFY_HOME —— 与 verify-security.sh 那次**删掉生产数据目录**
  # 的事故是同一个写法。该事故后按新规范对全仓库自查时发现本脚本仍然
  # 带着同一个洞，故一并修掉。
  #
  # 现在只删本脚本自己创建的容器（上面的 RUNNING 白名单）与 docker 对比
  # 容器；数据目录原样保留。
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
  echo
  echo "===== 结果 ====="
  echo "PASS=$PASS  FAIL=$FAIL  SKIP=$SKIP"
  echo "完整日志：$LOGFILE"
  if [ "$FAIL" -gt 0 ]; then
    echo "== 有失败项 =="
    exit 1
  fi
  echo "== ALL PASS =="
  exit $rc
}

# ---------------------------------------------------------------------------
# 能力位图辅助
# ---------------------------------------------------------------------------
# 下标即 capability 编号。
CAP_NAMES=(CHOWN DAC_OVERRIDE DAC_READ_SEARCH FOWNER FSETID KILL SETGID SETUID
  SETPCAP LINUX_IMMUTABLE NET_BIND_SERVICE NET_BROADCAST NET_ADMIN NET_RAW
  IPC_LOCK IPC_OWNER SYS_MODULE SYS_RAWIO SYS_CHROOT SYS_PTRACE SYS_PACCT SYS_ADMIN
  SYS_BOOT SYS_NICE SYS_RESOURCE SYS_TIME SYS_TTY_CONFIG MKNOD LEASE AUDIT_WRITE
  AUDIT_CONTROL SETFCAP MAC_OVERRIDE MAC_ADMIN SYSLOG WAKE_ALARM BLOCK_SUSPEND
  AUDIT_READ PERFMON BPF CHECKPOINT_RESTORE)

# Docker 默认集对应的位图：0xa80425fb。
DOCKER_DEFAULT_MASK="a80425fb"

cap_bit() { # $1=hex mask $2=bit → 0/1
  local v=$((16#$1))
  echo $(( (v >> $2) & 1 ))
}

cap_list() { # $1=hex mask → 名称列表
  local v=$((16#$1)) out=""
  local i
  for i in "${!CAP_NAMES[@]}"; do
    if (( (v >> i) & 1 )); then out+="${CAP_NAMES[$i]} "; fi
  done
  echo "${out:-<空>}"
}

# read_field PID FIELD → /proc/<pid>/status 里该字段的值
read_field() {
  local pid="$1" field="$2"
  grep "^$field:" "/proc/$pid/status" 2>/dev/null | awk '{print $2}'
}

# container_init_pid NAME → 容器 init 在宿主上的 PID（空表示拿不到）
container_init_pid() {
  local name="$1" cid rtjson
  cid="$("$LICORE_BIN" ps -q -a 2>/dev/null | head -n 200 | while read -r id; do
    d="$LICORE_HOME/containers/$id"
    if [ -f "$d/config.json" ] && grep -q "\"name\": *\"$name\"" "$d/config.json" 2>/dev/null; then
      echo "$id"; break
    fi
  done)"
  [ -z "$cid" ] && return 1
  rtjson="$LICORE_HOME/containers/$cid/runtime.json"
  [ -f "$rtjson" ] || return 1
  # 用 sed 而不是 jq/python3：目标服务器上不保证有这些工具。
  sed -n 's/.*"initPid"[[:space:]]*:[[:space:]]*\([0-9]\{1,\}\).*/\1/p' "$rtjson" | head -n1
}

# container_caps NAME → "CAPEFF NNP"（失败返回非 0）
container_caps() {
  local name="$1" pid
  pid="$(container_init_pid "$name")" || return 1
  [ -z "$pid" ] && return 1
  [ -d "/proc/$pid" ] || return 1
  echo "$(read_field "$pid" CapEff) $(read_field "$pid" NoNewPrivs)"
}

# ---------------------------------------------------------------------------
step "前置检查"
: > "$LOGFILE"

if [ "$(id -u)" -ne 0 ]; then
  echo "必须以 root 运行（需要创建 namespace 与写 cgroup）。" >&2
  echo "用法：sudo bash $0" >&2
  exit 2
fi
info "root 检查通过"

if ! command -v "$LICORE_BIN" >/dev/null 2>&1; then
  if [ -x "$LICORE_BIN" ]; then
    info "使用指定路径的二进制：$LICORE_BIN"
  else
    echo "找不到 licore 可执行文件：$LICORE_BIN" >&2
    echo >&2
    echo "安装方式（任选其一）：" >&2
    echo "  A. 从发布包安装：" >&2
    echo "     curl -fL https://github.com/LiStudioorg/licore/releases/download/v0.7.6/licore-linux-amd64.tar.gz -o /tmp/licore.tar.gz" >&2
    echo "     tar -xzf /tmp/licore.tar.gz -C /tmp && sudo install -m 0755 /tmp/licore /usr/local/bin/licore" >&2
    echo "  B. 从源码构建（**推荐**，见下方警告）：" >&2
    echo "     CGO_ENABLED=0 go build -o /tmp/licore ." >&2
    echo "     sudo LICORE_BIN=/tmp/licore bash $0" >&2
    echo >&2
    echo "⚠️ 重要：capability 裁剪（P0-2）**尚未发布**，v0.7.6 及更早的发布包里**没有**这个修复。" >&2
    echo "   用发布包跑本脚本，第 2–4 项会失败——那是预期结果，不是脚本问题。" >&2
    echo "   要验证本次修复，必须用当前 main 构建的二进制。" >&2
    exit 2
  fi
fi
BIN_REAL="$(command -v "$LICORE_BIN" 2>/dev/null || echo "$LICORE_BIN")"
info "licore：$BIN_REAL"
VER="$("$LICORE_BIN" --version 2>&1 | head -n1 || true)"
info "版本：${VER:-<未知>}"

# 版本提示：修复未发布时明确告知，避免把预期失败当成回归。
if echo "$VER" | grep -qE 'v0\.7\.[0-6]|0\.0\.0'; then
  echo
  echo "⚠️  注意：当前二进制版本为 ${VER}。capability 裁剪尚未发布，"
  echo "    v0.7.6 及更早的发布包不含此修复，第 2–4 项预期会失败。"
  echo "    请用当前 main 构建：CGO_ENABLED=0 go build -o /tmp/licore ."
  echo
fi

if command -v docker >/dev/null 2>&1; then
  info "docker：$(docker --version 2>/dev/null | head -n1)（将用于默认集对比）"
  HAVE_DOCKER=yes
else
  info "docker：未安装（跳过对比项）"
  HAVE_DOCKER=no
fi

# 隔离目录：全新开始，避免上一轮残留影响判断。
#
# **只在目录不存在时创建，绝不先删**（AGENTS.md《操作安全规范》规则 4）。
# 原写法是 `rm -rf "$VERIFY_HOME"` 再 mkdir，等于"每次运行都先把用户指定的
# 数据目录清空"——若 LICORE_VERIFY_HOME 指向真实数据目录，跑一次就毁一次。
#
# 需要干净环境时，请自己传一个全新的路径（如
# `LICORE_VERIFY_HOME=/tmp/licore-cap-$(date +%s)`），而不是让脚本替你做删除。
VERIFY_HOME_CREATED=no
if [ ! -d "$VERIFY_HOME" ]; then
  mkdir -p "$VERIFY_HOME" && VERIFY_HOME_CREATED=yes
fi

# 数据目录不在 /tmp 下时明确提示：脚本会往其中写测试容器。
case "$VERIFY_HOME" in
  /tmp/*) : ;;
  *)
    echo
    echo "注意：LICORE_VERIFY_HOME=$VERIFY_HOME 不在 /tmp 下。"
    echo "      脚本会在该数据目录内**创建测试容器**（$NAME_MAIN 等）。"
    echo "      不会删除该目录或其中既有镜像/容器。"
    echo
    ;;
esac
info "隔离数据目录：$VERIFY_HOME"

# ---------------------------------------------------------------------------
step "准备镜像 $IMAGE"
if "$LICORE_BIN" images 2>/dev/null | awk 'NR>1 {print $1":"$2}' | grep -qx "$IMAGE"; then
  info "镜像已在本地 store"
else
  # 没有就尝试用 convert 从 docker 导入（需要 docker）。
  if [ "$HAVE_DOCKER" = yes ]; then
    info "本地无镜像，尝试 licore convert $IMAGE --import"
    if "$LICORE_BIN" convert "$IMAGE" --import >>"$LOGFILE" 2>&1; then
      info "convert 导入成功"
    fi
  fi
fi
if ! "$LICORE_BIN" images 2>/dev/null | awk 'NR>1 {print $1":"$2}' | grep -qx "$IMAGE"; then
  fail "镜像 $IMAGE 不可用。请先准备镜像，例如："
  echo "       licore convert $IMAGE --import     # 需要 docker"
  echo "       licore pull /path/to/xxx.licore    # 或手工导入"
  echo "     也可用 LICORE_VERIFY_IMAGE=<其他已导入镜像> 重跑。"
  exit 1
fi
pass "镜像 $IMAGE 可用"

# ---------------------------------------------------------------------------
step "1) 默认启动容器并读取 PID 1 的权限位图"
if "$LICORE_BIN" run -d --name "$NAME_DEFAULT" "$IMAGE" sleep 3600 >>"$LOGFILE" 2>&1; then
  RUNNING+=("$NAME_DEFAULT")
  pass "容器 $NAME_DEFAULT 已启动"
else
  fail "容器启动失败，见 $LOGFILE"
  exit 1
fi
sleep 1

CAPS="$(container_caps "$NAME_DEFAULT")" || CAPS=""
if [ -z "$CAPS" ]; then
  fail "无法读取容器 PID 1 的能力（runtime.json 的 initPid 或 /proc 不可用）"
else
  CAPEFF="${CAPS%% *}"; NNP="${CAPS##* }"
  info "CapEff      = $CAPEFF"
  info "NoNewPrivs  = $NNP"
  info "持有能力：$(cap_list "$CAPEFF")"
fi

# ---------------------------------------------------------------------------
step "2) 断言 NoNewPrivs=1 且不含 CAP_SYS_ADMIN"
if [ -z "$CAPS" ]; then
  fail "上一步未能取得能力位图，无法断言"
else
  if [ "$NNP" = "1" ]; then
    pass "NoNewPrivs = 1（execve 不能获得新特权）"
  else
    fail "NoNewPrivs = ${NNP:-<空>}，期望 1（P0-1 未生效）"
  fi

  # CAP_SYS_ADMIN = bit 21。它是写 /proc/sysrq-trigger、加载 eBPF 的关键能力。
  if [ "$(cap_bit "$CAPEFF" 21)" = "0" ]; then
    pass "CapEff 不含 CAP_SYS_ADMIN（bit 21）—— 攻击路径的关键能力已丢弃"
  else
    fail "CapEff 仍含 CAP_SYS_ADMIN（bit 21）—— 容器仍可写 /proc/sysrq-trigger 重启宿主"
  fi

  # 对照 Docker 默认集：应当是同一个位图。
  if [ "$CAPEFF" = "$DOCKER_DEFAULT_MASK" ]; then
    pass "CapEff = 0x$CAPEFF，与 Docker 默认集（0x$DOCKER_DEFAULT_MASK）完全一致"
  else
    info "CapEff = 0x$CAPEFF，Docker 默认集为 0x$DOCKER_DEFAULT_MASK（下面第 3 项会对比实际 Docker）"
  fi
fi

# ---------------------------------------------------------------------------
step "3) 与 Docker 默认集对比（可选）"
if [ "$HAVE_DOCKER" != yes ]; then
  skip "系统没有 docker，跳过对比"
elif ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  skip "docker 本地没有 $IMAGE（脚本刻意不 pull），跳过对比"
else
  # --rm 一次性容器，只读它自己的 /proc/self/status；不触碰任何其他容器。
  DCAPS="$(docker run --rm --name "$DOCKER_CMP" "$IMAGE" \
    sh -c 'grep -E "^(CapEff|NoNewPrivs):" /proc/self/status' 2>/dev/null || true)"
  if [ -z "$DCAPS" ]; then
    skip "docker 对比容器未能返回能力信息"
  else
    DCAPEFF="$(echo "$DCAPS" | awk '/^CapEff:/{print $2}')"
    DNNP="$(echo "$DCAPS" | awk '/^NoNewPrivs:/{print $2}')"
    info "docker  CapEff = $DCAPEFF   NoNewPrivs = $DNNP"
    info "licore CapEff = ${CAPEFF:-<空>}   NoNewPrivs = ${NNP:-<空>}"
    if [ "${CAPEFF:-}" = "$DCAPEFF" ]; then
      pass "licore 默认能力集与 Docker 一致（0x$DCAPEFF）"
    else
      fail "licore 默认能力集 0x${CAPEFF:-?} 与 Docker 0x$DCAPEFF 不一致"
      info "差异（licore 缺失）：$(cap_list "$DCAPEFF")"
    fi
  fi
fi

# ---------------------------------------------------------------------------
step "4) --cap-add / --cap-drop 行为"
# 4a. --cap-add SYS_ADMIN 应把它加回来。
if "$LICORE_BIN" run -d --name "$NAME_ADD" --cap-add SYS_ADMIN "$IMAGE" sleep 3600 >>"$LOGFILE" 2>&1; then
  RUNNING+=("$NAME_ADD")
  sleep 1
  ADDCAPS="$(container_caps "$NAME_ADD")" || ADDCAPS=""
  if [ -z "$ADDCAPS" ]; then
    fail "--cap-add 容器启动后读取不到能力位图"
  else
    ACAPEFF="${ADDCAPS%% *}"
    info "--cap-add SYS_ADMIN → CapEff = $ACAPEFF"
    if [ "$(cap_bit "$ACAPEFF" 21)" = "1" ]; then
      pass "--cap-add SYS_ADMIN 生效（bit 21 已置位）"
    else
      fail "--cap-add SYS_ADMIN 未生效：CapEff 仍无 CAP_SYS_ADMIN"
    fi
  fi
else
  fail "--cap-add 容器启动失败，见 $LOGFILE"
fi

# 4b. --cap-drop ALL 应得到全零。
if "$LICORE_BIN" run -d --name "$NAME_DROPALL" --cap-drop ALL "$IMAGE" sleep 3600 >>"$LOGFILE" 2>&1; then
  RUNNING+=("$NAME_DROPALL")
  sleep 1
  DROPCAPS="$(container_caps "$NAME_DROPALL")" || DROPCAPS=""
  if [ -z "$DROPCAPS" ]; then
    fail "--cap-drop ALL 容器启动后读取不到能力位图"
  else
    DCAPEFF2="${DROPCAPS%% *}"
    info "--cap-drop ALL → CapEff = $DCAPEFF2"
    if [ "$((16#$DCAPEFF2))" -eq 0 ]; then
      pass "--cap-drop ALL 生效（CapEff = 0，容器不持有任何能力）"
    else
      fail "--cap-drop ALL 未生效：CapEff = 0x$DCAPEFF2，期望 0"
      info "仍持有：$(cap_list "$DCAPEFF2")"
    fi
  fi
else
  fail "--cap-drop ALL 容器启动失败，见 $LOGFILE"
fi

# ---------------------------------------------------------------------------
step "5) sysrq 测试（默认跳过）"
if [ "$UNSAFE" != yes ]; then
  skip "未指定 --unsafe，跳过 /proc/sysrq-trigger 测试"
  info "该测试若在隔离未生效时会真的重启本机，因此默认关闭"
else
  # 安全联锁：先确认默认容器确实没有 CAP_SYS_ADMIN。
  # 没有这一层，一次回归就可能重启生产服务器。
  if [ -z "${CAPEFF:-}" ] || [ "$(cap_bit "$CAPEFF" 21)" = "1" ]; then
    fail "安全联锁触发：默认容器仍持有 CAP_SYS_ADMIN，**中止** sysrq 测试（避免重启本机）"
  else
    info "安全联锁通过：默认容器无 CAP_SYS_ADMIN，可安全执行"
    if [ ! -e "/proc/sysrq-trigger" ]; then
      skip "宿主没有 /proc/sysrq-trigger"
    else
      OUT="$("$LICORE_BIN" run "$NAME_DEFAULT-marker" "$IMAGE" \
        sh -c 'echo b > /proc/sysrq-trigger' 2>&1 || true)"
      if echo "$OUT" | grep -qiE 'denied|not permitted|read-only|permission'; then
        pass "写 /proc/sysrq-trigger 被拒绝（容器无法重启宿主）"
        info "输出：$(echo "$OUT" | tail -n1)"
      else
        # 能走到这里说明没报权限错——若真的写进去，机器已经重启了。
        fail "写 /proc/sysrq-trigger 未被拒绝：$(echo "$OUT" | tail -n1)"
      fi
    fi
  fi
fi

# ---------------------------------------------------------------------------
step "6) exec 缺口说明（本次未修）"
info "licore exec 走宿主侧 nsenter，继承宿主 root 的完整能力，"
info "与容器 init 的裁剪无关。因此不要用 \`licore exec <容器> grep CapEff\`"
info "来判断本修复——那样读到的是宿主的位图。"
info "该缺口已知且独立，不在本次修复范围内。"

echo
echo "（脚本结束，开始清理）"
