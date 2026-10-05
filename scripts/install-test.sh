#!/usr/bin/env bash
# Copyright (C) 2026 LiStudioorg
# SPDX-License-Identifier: AGPL-3.0-only
#
# install.sh 的 --mirror URL 派生测试。
#
# 为什么需要：--mirror 的正确性完全是"字符串拼得对不对"，而拼错的表现是
# 404 / 下载失败——在真机上要等到用户装不上才发现。这里把派生逻辑单独钉住。
#
# 测法：不真的下载，而是用 --dry-run 观察脚本打印出的 URL，再与期望比对。
# --dry-run 会调用 resolve_version（走网络）拿 latest，因此测 latest 的用例
# 需要网络；**固定 --version 的用例不需要网络**，是纯字符串断言。
#
# 用法：bash scripts/install-test.sh

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALL="$HERE/install.sh"

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); printf '[PASS] %s\n' "$*"; }
fail() { FAIL=$((FAIL+1)); printf '[FAIL] %s\n' "$*"; }

# 跑一次 dry-run，打印 stdout+stderr
run_dry() {
  bash "$INSTALL" --dry-run "$@" 2>&1
}

# 断言输出中包含某子串
assert_contains() { # $1=desc $2=haystack $3=needle
  case "$2" in
    *"$3"*) pass "$1" ;;
    *) fail "$1"; printf '       期望包含: %s\n' "$3"; printf '       实际输出:\n%s\n' "$2" | sed 's/^/         /' ;;
  esac
}

# 断言输出中不包含某子串
assert_not_contains() { # $1=desc $2=haystack $3=needle
  case "$2" in
    *"$3"*) fail "$1"; printf '       不应包含: %s\n' "$3" ;;
    *) pass "$1" ;;
  esac
}

echo "===== install.sh --mirror URL 派生测试 ====="

# --- 1. 默认（GitHub），固定版本号，纯字符串断言 ---
out="$(run_dry --version v0.9.5)"
assert_contains "默认走 GitHub releases" "$out" \
  "https://github.com/LiStudioorg/licore/releases/download/v0.9.5/licore-linux-amd64.tar.gz"
assert_contains "默认 SHA256SUMS 同源" "$out" \
  "https://github.com/LiStudioorg/licore/releases/download/v0.9.5/SHA256SUMS"
assert_not_contains "默认不出现镜像站" "$out" "gitea.com"

# --- 2. --mirror 带完整 owner/repo ---
out="$(run_dry --version v0.9.5 --mirror https://gitea.com/xiaoshuai/licore)"
assert_contains "mirror 资产 URL" "$out" \
  "https://gitea.com/xiaoshuai/licore/releases/download/v0.9.5/licore-linux-amd64.tar.gz"
assert_contains "mirror SHA256SUMS" "$out" \
  "https://gitea.com/xiaoshuai/licore/releases/download/v0.9.5/SHA256SUMS"
assert_not_contains "mirror 模式不再走 GitHub" "$out" "github.com"

# --- 3. --version= 等号形式 ---
out="$(run_dry --version=v0.9.5 --mirror=https://gitea.com/xiaoshuai/licore)"
assert_contains "--version= 等号形式可用" "$out" "releases/download/v0.9.5/"

# --- 4. --mirror 不带 owner/repo：应自动补 xiaoshuai/licore ---
out="$(run_dry --version v0.9.5 --mirror https://gitea.com)"
assert_contains "裸 host 自动补 owner/repo" "$out" \
  "https://gitea.com/xiaoshuai/licore/releases/download/v0.9.5/"
assert_contains "裸 host 打印补全后的镜像站" "$out" "镜像站    : https://gitea.com/xiaoshuai/licore"

# --- 5. 尾部斜杠容错 ---
out="$(run_dry --version v0.9.5 --mirror https://gitea.com/xiaoshuai/licore/)"
assert_contains "尾部斜杠被容忍" "$out" \
  "https://gitea.com/xiaoshuai/licore/releases/download/v0.9.5/"

# --- 6. 非法 --mirror 必须被拒 ---
out="$(run_dry --mirror 'ftp://evil' 2>&1)"
assert_contains "非 http(s) 被拒" "$out" "必须是 http(s):// 开头"

# --- 7. --mirror 缺值必须被拒 ---
out="$(run_dry --mirror 2>&1)"
assert_contains "--mirror 缺值被拒" "$out" "需要一个参数"

# --- 8. Gitea API 路径（latest 解析）断言 ---
# Gitea 的 API 挂在 **host 根**下，不是仓库路径之下。真机实测：
#   https://gitea.com/xiaoshuai/licore/api/v1/...  → 404
#   https://gitea.com/api/v1/repos/...             → 200
# 这条用 latest（需网络）验证 API 能被正确取到；无网络时降级为 SKIP。
# **不能只靠 dry-run 的输出判断**：API 路径写错会导致 resolve_version 失败，
# 而失败时脚本报的是"无法查询最新版本"——初版把它当"网络不可达"降级成
# SKIP，于是**拼错 API 路径也能全绿**（反向验证时实测到这个问题）。
#
# 因此这里额外做一条**纯离线**断言：直接从脚本里取派生出的 API 地址，
# 验证它不重复 owner/repo。这条不依赖网络，拼错必然失败。
api_url="$(bash -c '
  set -uo pipefail
  # 复用 install.sh 里的派生逻辑：source 不进去（脚本会执行主流程），
  # 因此这里重新跑一遍同样的推导并打印 API。
  base="https://gitea.com/xiaoshuai/licore"
  origin="${base#*://}"; origin="${origin%%/*}"
  scheme="${base%%://*}"
  path="${base#*://}"; path="${path#*/}"; path="${path%%\?*}"; path="${path%/}"
  owner="${path%%/*}"; rest="${path#*/}"; repo="${rest%%/*}"
  echo "$scheme://$origin/api/v1/repos/$owner/$repo/releases/latest"
')"
expected_api="https://gitea.com/api/v1/repos/xiaoshuai/licore/releases/latest"
if [ "$api_url" = "$expected_api" ]; then
  pass "Gitea API 路径正确：$api_url"
else
  fail "Gitea API 路径错误"
  printf '       期望: %s\n       实际: %s\n' "$expected_api" "$api_url"
fi

# 再从真实 dry-run 输出确认端到端能解析（需要网络，失败才降级 SKIP）
out="$(run_dry --mirror https://gitea.com/xiaoshuai/licore)"
if printf '%s' "$out" | grep -q "无法查询最新版本"; then
  printf '[SKIP] Gitea latest 端到端解析（网络不可达）\n'
else
  assert_contains "Gitea latest 端到端可解析" "$out" "gitea.com/xiaoshuai/licore/releases/download/"
fi

# 同样离线校验 install.sh **源码本身**里的派生语句用的是 host 根，
# 而不是仓库路径——防止有人"顺手改回去"。
if grep -q 'LICORE_API_URL="\$scheme://\$origin/api/v1/repos/\$owner/\$repo/releases/latest"' "$INSTALL"; then
  pass "install.sh 源码用 host 根拼 Gitea API"
else
  fail "install.sh 源码里的 Gitea API 拼接方式不对（应为 \$scheme://\$origin/...）"
fi

echo
echo "===== 结果 ====="
echo "PASS=$PASS  FAIL=$FAIL"
if [ "$FAIL" -gt 0 ]; then
  echo "== 有失败项 =="
  exit 1
fi
echo "== ALL PASS =="
