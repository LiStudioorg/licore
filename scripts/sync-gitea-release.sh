#!/usr/bin/env bash
# Copyright (C) 2026 LiStudioorg
# SPDX-License-Identifier: AGPL-3.0-only
#
# sync-gitea-release.sh —— 把某个版本的 Release 从 GitHub 同步到 Gitea。
#
# 用法:
#   scripts/sync-gitea-release.sh v0.9.5
#   scripts/sync-gitea-release.sh v0.9.5 --dry-run
#
# 为什么需要它：Gitea 实例上没有跑 GitHub Actions（两边的 workflow 不互通），
# 因此 `git push gitea v0.9.5` 只推了 tag，**不会**自动建 Release、更不会上传
# 资产。每次发版后必须手动同步一次，否则 Gitea 上只有源码 tag、没有可下载的
# 二进制。本脚本把这套流程固化下来，避免漏做或做错。
#
# 它做什么：
#   1. 从 GitHub Release 拉取资产清单与 release notes；
#   2. 下载全部资产到临时目录；
#   3. 在 Gitea 建 Release（已存在则复用，不重复建）；
#   4. 逐个上传资产（已存在的跳过，支持中断后重跑）；
#   5. 校验两边的 SHA256SUMS 内容一致；
#   6. 清理临时目录。
#
# 配置（环境变量，或下面的默认值）：
#   GITEA_URL       Gitea 站点，默认 https://gitea.com
#   GITEA_REPO      owner/repo，默认 xiaoshuai/licore
#   GITEA_TOKEN     API 令牌（**必须有**；也可放 ~/.config/git/credentials）
#   GITHUB_REPO     GitHub 侧 owner/repo，默认 LiStudioorg/licore
#
# 退出码：0 成功；1 一般错误；2 用法错误；3 配置缺失；4 校验不一致。

set -euo pipefail

GITEA_URL="${GITEA_URL:-https://gitea.com}"
GITEA_REPO="${GITEA_REPO:-xiaoshuai/licore}"
GITHUB_REPO="${GITHUB_REPO:-LiStudioorg/licore}"

DRY_RUN=0
TAG=""

# ---------- 输出helper ----------
info()  { printf '  %s\n' "$*"; }
step()  { printf '\n==> %s\n' "$*"; }
warn()  { printf '警告: %s\n' "$*" >&2; }
die()   { printf '错误: %s\n' "$1" >&2; exit "${2:-1}"; }

usage() {
  cat <<'EOF'
把某个版本的 Release 从 GitHub 同步到 Gitea。

用法:
  scripts/sync-gitea-release.sh <TAG> [--dry-run]

参数:
  <TAG>        版本号，如 v0.9.5
  --dry-run    只显示将要执行的操作，不下载、不创建、不上传

环境变量:
  GITEA_URL     Gitea 站点（默认 https://gitea.com）
  GITEA_REPO    Gitea 仓库 owner/repo（默认 xiaoshuai/licore）
  GITEA_TOKEN   Gitea API 令牌（必需）
  GITHUB_REPO   GitHub 仓库 owner/repo（默认 LiStudioorg/licore）

示例:
  GITEA_TOKEN=xxx scripts/sync-gitea-release.sh v0.9.5
  scripts/sync-gitea-release.sh v0.9.5 --dry-run
EOF
}

# ---------- 参数解析 ----------
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) die "未知参数: $1（用 --help 查看用法）" 2 ;;
    *)
      [ -z "$TAG" ] || die "只能指定一个 TAG" 2
      TAG="$1"; shift ;;
  esac
done

[ -n "$TAG" ] || { usage >&2; exit 2; }

# 依赖检查。curl 与 python3 用来解析 JSON（不引入 jq，保持零额外依赖）。
for cmd in curl python3; do
  command -v "$cmd" >/dev/null 2>&1 || die "缺少依赖: $cmd" 3
done

# ---------- 令牌解析 ----------
# 优先环境变量；否则尝试从 git credential store 里取（与 git push 用同一份，
# 避免用户在多处维护令牌）。
resolve_token() {
  if [ -n "${GITEA_TOKEN:-}" ]; then
    printf '%s' "$GITEA_TOKEN"
    return 0
  fi
  local credfile="${HOME}/.config/git/credentials"
  if [ -f "$credfile" ]; then
    # 形如 https://user:token@gitea.com —— 取 token 部分。
    python3 - "$credfile" "$GITEA_URL" <<'PY'
import sys, urllib.parse
path, url = sys.argv[1], sys.argv[2]
host = urllib.parse.urlparse(url).hostname
for line in open(path, encoding='utf-8', errors='replace'):
    line = line.strip()
    if not line.startswith('http'):
        continue
    parsed = urllib.parse.urlparse(line)
    if parsed.hostname == host and parsed.password:
        print(parsed.password, end='')
        break
PY
  fi
}

TOKEN="$(resolve_token || true)"
if [ -z "$TOKEN" ]; then
  if [ "$DRY_RUN" -eq 1 ]; then
    warn "未找到 Gitea 令牌；--dry-run 可继续，但真实运行会失败"
  else
    die "未找到 Gitea 令牌。请设置 GITEA_TOKEN，或写入 ~/.config/git/credentials" 3
  fi
fi

GITHUB_API="https://api.github.com/repos/${GITHUB_REPO}/releases/tags/${TAG}"
GITEA_API="${GITEA_URL}/api/v1/repos/${GITEA_REPO}"

step "同步 ${TAG}：GitHub → Gitea"
info "GitHub : ${GITHUB_REPO}"
info "Gitea  : ${GITEA_REPO}"
info "模式   : $([ "$DRY_RUN" -eq 1 ] && echo 'dry-run（不做实际改动）' || echo '实际同步')"

# ---------- 1. 取 GitHub release 元数据 ----------
step "1/6 读取 GitHub Release 元数据"
META="$(curl -fsSL "$GITHUB_API" 2>/dev/null)" \
  || die "GitHub 上找不到 ${TAG} 的 Release。请确认 tag 已推送且 workflow 已完成。" 1

INFO_JSON="$(printf '%s' "$META" | python3 -c '
import sys, json
d = json.load(sys.stdin)
name = d.get("name") or d["tag_name"]
body = d.get("body") or ""
assets = [(a["name"], a["browser_download_url"], a["size"]) for a in d["assets"]]
print(json.dumps({"name": name, "body": body, "assets": assets}))
')"

ASSET_COUNT="$(printf '%s' "$INFO_JSON" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["assets"]))')"
info "Release 名: $(printf '%s' "$INFO_JSON" | python3 -c 'import sys,json; print(json.load(sys.stdin)["name"])')"
info "资产数    : ${ASSET_COUNT}"
[ "$ASSET_COUNT" -gt 0 ] || die "GitHub Release 没有任何资产，拒绝同步。" 1

# ---------- 2. 准备临时目录并下载 ----------
WORK="$(mktemp -d)"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

step "2/6 从 GitHub 下载资产"
if [ "$DRY_RUN" -eq 0 ]; then
  printf '%s' "$INFO_JSON" | python3 -c '
import sys, json
for name, url, _ in json.load(sys.stdin)["assets"]:
    print(name, url)
' | while read -r name url; do
    printf '  %-36s ' "$name"
    curl -fsSL -o "$WORK/$name" "$url" && echo "OK" || die "下载 $name 失败" 1
  done
  info "已下载到 $WORK"
else
  printf '%s' "$INFO_JSON" | python3 -c '
import sys, json
for name, _, size in json.load(sys.stdin)["assets"]:
    print(f"  {name:<36} {size}")
'
fi

# ---------- 3. 建 Release（已存在则复用） ----------
step "3/6 在 Gitea 建 Release"
if [ "$DRY_RUN" -eq 1 ]; then
  info "将创建（或复用）Release: ${TAG}"
  RID=""
else
  # 先查是否已存在——重跑时不该重复创建。
  RID="$(curl -fsS -H "Authorization: token ${TOKEN}" \
      "${GITEA_API}/releases/tags/${TAG}" 2>/dev/null \
      | python3 -c 'import sys,json
try:
    print(json.load(sys.stdin).get("id",""))
except Exception:
    print("")' || true)"

  if [ -n "$RID" ]; then
    info "Release 已存在（id=${RID}），复用并补齐缺失资产"
  else
    printf '%s' "$INFO_JSON" | python3 -c '
import sys, json
d = json.load(sys.stdin)
print(json.dumps({"tag_name": sys.argv[1], "name": d["name"],
                  "body": d["body"], "draft": False, "prerelease": False}))
' "$TAG" > "$WORK/release.json"

    RID="$(curl -fsS -X POST \
        -H "Authorization: token ${TOKEN}" \
        -H "Content-Type: application/json" \
        -d @"$WORK/release.json" \
        "${GITEA_API}/releases" \
      | python3 -c 'import sys,json; print(json.load(sys.stdin).get("id",""))')"
    [ -n "$RID" ] || die "创建 Release 失败（检查令牌权限）" 1
    info "已创建 Release id=${RID}"
  fi
fi

# ---------- 4. 上传资产（跳过已存在的） ----------
step "4/6 上传资产"
if [ "$DRY_RUN" -eq 1 ]; then
  info "将上传 ${ASSET_COUNT} 个资产"
else
  # 取已存在的资产名，避免重复上传（重跑安全）。
  EXISTING="$(curl -fsS -H "Authorization: token ${TOKEN}" \
      "${GITEA_API}/releases/${RID}" 2>/dev/null \
      | python3 -c 'import sys,json
try:
    print("\n".join(a["name"] for a in json.load(sys.stdin).get("assets",[])))
except Exception:
    pass' || true)"

  printf '%s' "$INFO_JSON" | python3 -c '
import sys, json
for name, _, _ in json.load(sys.stdin)["assets"]:
    print(name)
' | while read -r name; do
    if printf '%s\n' "$EXISTING" | grep -qxF "$name"; then
      printf '  %-36s 已存在，跳过\n' "$name"
      continue
    fi
    printf '  %-36s ' "$name"
    code="$(curl -sS -o "$WORK/upload.out" -w '%{http_code}' -X POST \
        -H "Authorization: token ${TOKEN}" \
        -F "attachment=@$WORK/$name" \
        "${GITEA_API}/releases/${RID}/assets?name=${name}")"
    case "$code" in
      201|200) echo "OK" ;;
      *) echo "FAIL(HTTP $code)"; cat "$WORK/upload.out" >&2; die "上传 $name 失败" 1 ;;
    esac
  done
fi

# ---------- 5. 校验 SHA256SUMS 一致 ----------
step "5/6 校验 SHA256SUMS 与 GitHub 一致"
if [ "$DRY_RUN" -eq 1 ]; then
  info "将对比两侧的 SHA256SUMS"
else
  GH_SUMS="$WORK/SHA256SUMS"
  GT_SUMS="$WORK/.gitea-SHA256SUMS"
  if [ ! -f "$GH_SUMS" ]; then
    warn "GitHub 资产里没有 SHA256SUMS，跳过一致性校验"
  else
    curl -fsSL -o "$GT_SUMS" \
      "${GITEA_URL}/${GITEA_REPO}/releases/download/${TAG}/SHA256SUMS" \
      || die "从 Gitea 下载 SHA256SUMS 失败" 1
    if diff -q "$GH_SUMS" "$GT_SUMS" >/dev/null; then
      info "✅ SHA256SUMS 与 GitHub 逐字节一致"
    else
      warn "两侧 SHA256SUMS 不一致："
      diff "$GH_SUMS" "$GT_SUMS" >&2 || true
      die "校验失败" 4
    fi
  fi
fi

# ---------- 6. 汇总 ----------
step "6/6 完成"
if [ "$DRY_RUN" -eq 0 ]; then
  info "Release 页面: ${GITEA_URL}/${GITEA_REPO}/releases/tag/${TAG}"
  info "资产数      : ${ASSET_COUNT}"
else
  info "（dry-run，未做任何改动）"
fi
