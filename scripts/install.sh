#!/usr/bin/env bash
# Copyright (C) 2026 LiStudioorg
# SPDX-License-Identifier: AGPL-3.0-only
#
# LiCore 安装脚本：从 GitHub Releases（或镜像站）下载对应平台的归档并安装 licore。
#
# 用法：
#   curl -fsSL https://raw.githubusercontent.com/LiStudioorg/licore/main/scripts/install.sh | bash
#   curl -fsSL .../install.sh | bash -s -- --version v0.7.0
#   curl -fsSL .../install.sh | bash -s -- --dry-run
#   curl -fsSL .../install.sh | bash -s -- --mirror https://gitea.com/xiaoshuai/licore
#
# 国内网络访问 GitHub 受限时用 --mirror 走 Gitea 镜像站，见 usage()。
# 也可以用 gh-proxy 之类的加速前缀包裹整个 raw 链接（脚本本身即从镜像取），
# 再用 --mirror 让**资产下载**也走镜像——两者是独立的两跳，都要覆盖。
#
# 关于校验的**真实边界**（不夸大）：
#   SHA256SUMS 与 tar.gz 来自同一个 Release（同一 HTTPS 来源），因此校验能
#   发现**传输损坏**与**归档不完整**，但**不能**防篡改——能改 tar.gz 的一方
#   同样能改 SHA256SUMS。要真正防篡改需要签名（cosign / GPG）与独立的信任根，
#   当前未引入。本脚本不会把这层校验说成"安全验证"。
#
#   用 --mirror 时还多一层信任假设：你信任该镜像站**未篡改**资产。这与
#   "信任 GitHub 未篡改"是同一性质，但镜像站是第三方，信任面更大。
#
# 退出码：0 成功；1 一般错误；2 用法错误；3 平台不支持；4 校验失败；5 权限不足。

set -euo pipefail

REPO="LiStudioorg/licore"
BINARY="licore"
DEFAULT_PREFIX="/usr/local/bin"

# 下载基地址。默认走 GitHub Releases 的 HTTPS 端点；--mirror 可切到镜像站
# （见 parse_args 的说明）。LICORE_BASE_URL / LICORE_API_URL 仍是最后的
# 覆写口，便于本地起测试服务器验证下载/校验/安装全流程。
LICORE_BASE_URL="${LICORE_BASE_URL:-https://github.com/$REPO/releases/download}"
LICORE_API_URL="${LICORE_API_URL:-https://api.github.com/repos/$REPO/releases/latest}"

VERSION="latest"
PREFIX="$DEFAULT_PREFIX"
DRY_RUN=0
FORCE=0
MIRROR=""

# 已知镜像站的 owner 与主仓库不同：Gitea 上是 xiaoshuai/licore
# （AGENTS.md《Gitea 镜像站没有 Actions》一节；用 LiStudioorg 会 404）。
GITEA_OWNER="xiaoshuai"
GITEA_REPO="licore"

# ---------- 输出 ----------

# 只用 stderr 输出进度，避免污染 stdout（便于 `| bash -s --` 之外的场景）。
info()  { printf '%s\n' "$*" >&2; }
warn()  { printf '警告: %s\n' "$*" >&2; }
die()   { printf '错误: %s\n' "$*" >&2; exit "${2:-1}"; }

usage() {
  cat <<EOF
LiCore 安装脚本

用法:
  install.sh [选项]

选项:
  --version <TAG>   指定版本，如 v0.7.0（默认: latest，取最新 Release）
  --prefix <DIR>    安装目录（默认: $DEFAULT_PREFIX）
  --mirror <URL>    改用镜像站下载（默认: GitHub）
                    传镜像站的仓库根地址，脚本自行拼接 releases/download
                    与 API 路径。已知可用值：
                      https://gitea.com/$GITEA_OWNER/$GITEA_REPO
  --dry-run         只显示将要执行的操作，不下载、不写入
  --force           目标已存在时直接覆盖（默认会拒绝）
  -h, --help        显示本帮助

示例:
  # 安装最新版到 /usr/local/bin（需要 root 或 sudo）
  curl -fsSL https://raw.githubusercontent.com/$REPO/main/scripts/install.sh | sudo bash

  # 国内网络：脚本本体走加速前缀，资产走 Gitea 镜像
  curl -fsSL https://gh-proxy.org/https://raw.githubusercontent.com/$REPO/main/scripts/install.sh \\
    | sudo bash -s -- --mirror https://gitea.com/$GITEA_OWNER/$GITEA_REPO

  # 先看看会做什么
  curl -fsSL https://raw.githubusercontent.com/$REPO/main/scripts/install.sh | bash -s -- --dry-run

  # 装到用户目录（无需 root）
  curl -fsSL https://raw.githubusercontent.com/$REPO/main/scripts/install.sh | bash -s -- --prefix "\$HOME/.local/bin"
EOF
}

# ---------- 镜像站 ----------

# apply_mirror BASE 把下载基地址切到镜像站。
#
# **`--mirror` 的取值是 Gitea 仓库根地址**，形如：
#
#     https://gitea.com/xiaoshuai/licore
#
# 从它派生两个端点（Gitea 与 GitHub 的路径结构一致，只有域名不同）：
#
#     Release 资产: <base>/releases/download/<tag>/<asset>.tar.gz
#     SHA256SUMS  : <base>/releases/download/<tag>/SHA256SUMS   （同一 BASE/<tag>）
#     latest API  : <base>/api/v1/repos/<owner>/<repo>/releases/latest
#
# 注意 owner 用镜像站自己的：Gitea 上是 xiaoshuai/licore，而**不是**主仓库的
# LiStudioorg/licore（用错会 404）。若用户传的 base 里已含 owner/repo，
# 直接用它；否则用默认的 Gitea owner/repo 拼接。
apply_mirror() {
  base="${1%/}"
  [ -n "$base" ] || die "--mirror 不能为空" 2
  case "$base" in
    http://*|https://*) ;;
    *) die "--mirror 必须是 http(s):// 开头的地址（当前: $base）" 2 ;;
  esac

  # 判断 base 是否已含 owner/repo：路径部分去掉首尾斜杠后若还有两段以上，
  # 就认为用户已指定（形如 https://gitea.com/xiaoshuai/licore）；否则补默认值。
  origin="${base#*://}"        # 保留 host 部分用于拼 API
  origin="${origin%%/*}"       # host[:port]
  scheme="${base%%://*}"

  path="${base#*://}"          # https://host/a/b -> host/a/b
  path="${path#*/}"            # -> a/b
  path="${path%%\?*}"          # 去掉可能的查询串
  path="${path%/}"
  case "$path" in
    */*)
      owner="${path%%/*}"
      rest="${path#*/}"
      repo="${rest%%/*}"
      ;;
    *)
      owner="$GITEA_OWNER"
      repo="$GITEA_REPO"
      base="$base/$owner/$repo"
      ;;
  esac

  # Release 资产走仓库路径；但 **Gitea 的 API 挂在 host 根下**，
  # 不是仓库路径之下。真机实测：
  #   https://gitea.com/xiaoshuai/licore/api/v1/...   → 404
  #   https://gitea.com/api/v1/repos/xiaoshuai/licore/releases/latest → 200
  LICORE_BASE_URL="$base/releases/download"
  LICORE_API_URL="$scheme://$origin/api/v1/repos/$owner/$repo/releases/latest"
  info "  镜像站    : $base"
}

# ---------- 参数解析 ----------

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --version)
        [ $# -ge 2 ] || die "--version 需要一个参数" 2
        VERSION="$2"; shift 2 ;;
      --version=*)
        VERSION="${1#*=}"; shift ;;
      --prefix)
        [ $# -ge 2 ] || die "--prefix 需要一个参数" 2
        PREFIX="$2"; shift 2 ;;
      --prefix=*)
        PREFIX="${1#*=}"; shift ;;
      --dry-run)
        DRY_RUN=1; shift ;;
      --force)
        FORCE=1; shift ;;
      --mirror)
        [ $# -ge 2 ] || die "--mirror 需要一个参数（如 https://gitea.com/$GITEA_OWNER/$GITEA_REPO）" 2
        MIRROR="$2"; shift 2 ;;
      --mirror=*)
        MIRROR="${1#*=}"; shift ;;
      -h|--help)
        usage; exit 0 ;;
      *)
        die "未知参数: $1（用 --help 查看用法）" 2 ;;
    esac
  done

  [ -n "$VERSION" ] || die "--version 不能为空" 2
  [ -n "$PREFIX" ]  || die "--prefix 不能为空" 2
  case "$PREFIX" in
    /*) ;;
    *) die "--prefix 必须是绝对路径（当前: $PREFIX）" 2 ;;
  esac

  # 镜像站在参数校验通过后再应用，保证 --mirror 的非法值能被准确报出。
  #
  # 优先级：显式 --mirror > LICORE_BASE_URL 环境变量 > 内置 GitHub 默认值。
  # 环境变量在脚本顶部已作为默认值读入；这里若给了 --mirror 就覆写它，
  # 这样"环境变量"与"命令行"各自都能独立工作，不会互相打架。
  #
  # 注意末尾的 `|| true`：`[ -n "$MIRROR" ] && apply_mirror` 在
  # MIRROR 为空（未指定 --mirror，即最常见的默认路径）时整体返回 1，
  # 在 `set -e` 下会让脚本**静默退出 1**——真机实测踩到过
  # （`--dry-run` 无任何输出、退出码 1）。必须显式吞掉这个"条件为假"。
  if [ -n "$MIRROR" ]; then
    apply_mirror "$MIRROR"
  fi
}

# ---------- 依赖 ----------

# 下载器：优先 curl，回落到 wget。两者都没有则明确失败，不做隐式降级。
pick_downloader() {
  if command -v curl >/dev/null 2>&1; then
    DOWNLOADER="curl"
  elif command -v wget >/dev/null 2>&1; then
    DOWNLOADER="wget"
  else
    die "需要 curl 或 wget 之一，但都没有找到" 1
  fi
}

# 下载到文件；全部走 HTTPS（由 BASE_URL 固定为 https://）。
download() {
  url="$1"; dest="$2"
  case "$DOWNLOADER" in
    curl) curl -fsSL --retry 3 --connect-timeout 15 -o "$dest" "$url" ;;
    wget) wget -q -T 15 -t 3 -O "$dest" "$url" ;;
  esac
}

# 校验和工具：优先 sha256sum（GNU），macOS 上用 shasum -a 256。
pick_hasher() {
  if command -v sha256sum >/dev/null 2>&1; then
    HASHER="sha256sum"
  elif command -v shasum >/dev/null 2>&1; then
    HASHER="shasum"
  else
    die "需要 sha256sum 或 shasum 之一来校验归档" 1
  fi
}

# 计算单个文件的 sha256（只输出十六进制摘要）。
hash_file() {
  if [ "$HASHER" = "sha256sum" ]; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# ---------- 平台探测 ----------

# 判定是否为 Android。需在 Linux 内核上额外区分 Android 与普通发行版：
# Android 上 uname -s 同样返回 Linux，因此用 /system/bin/getprop 作为判据。
is_android() {
  [ -x /system/bin/getprop ] || return 1
  sdk="$(/system/bin/getprop ro.build.version.sdk 2>/dev/null || true)"
  [ -n "$sdk" ]
}

# 归一化 CPU 架构到资产名中的取值。
detect_arch() {
  raw="$(uname -m)"
  case "$raw" in
    x86_64|amd64)          echo "amd64" ;;
    aarch64|arm64)         echo "arm64" ;;
    armv7l|armv7|armv6l|armv6|armhf|arm) echo "arm" ;;
    i386|i486|i586|i686|x86) echo "386" ;;
    riscv64)               echo "riscv64" ;;
    *)
      die "不支持的 CPU 架构: $raw（可支持的: x86_64/aarch64/armv7l/i686/riscv64）" 3 ;;
  esac
}

# 归一化操作系统到资产名中的取值。
detect_os() {
  raw="$(uname -s)"
  case "$raw" in
    Linux)
      if is_android; then echo "android"; else echo "linux"; fi ;;
    Darwin) echo "darwin" ;;
    *)
      die "不支持的操作系统: $raw（仅支持 Linux、Android、macOS）" 3 ;;
  esac
}

# 组装资产名；不支持组合时明确报错，不猜测、不降级。
asset_name() {
  os="$1"; arch="$2"
  base="licore-$os-$arch"
  case "$os/$arch" in
    linux/amd64|linux/arm64) ;;
    linux/arm|linux/386|linux/riscv64) ;;
    android/arm64) ;;
    darwin/amd64|darwin/arm64) ;;
    *)
      die "平台 $os/$arch 没有官方构建产物。可用的组合见 README 的平台支持矩阵。" 3 ;;
  esac
  printf '%s' "$base"
}

# ---------- 版本解析 ----------

# 取 latest 的 tag 名。走 GitHub API 并解析 JSON；不引入 jq 依赖。
resolve_version() {
  if [ "$VERSION" != "latest" ]; then
    printf '%s' "$VERSION"
    return
  fi
  api="$LICORE_API_URL"
  tmp="$(mktemp)"
  if ! download "$api" "$tmp"; then
    rm -f "$tmp"
    die "无法查询最新版本（$api）。网络受限时可用 --version 指定版本。" 1
  fi
  tag="$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$tmp" | head -1)"
  rm -f "$tmp"
  [ -n "$tag" ] || die "无法从 GitHub API 响应中解析出 tag_name" 1
  printf '%s' "$tag"
}

# ---------- 权限检查 ----------

# 目标目录不可写时明确报错并提示 sudo，**不**自动改到别的路径。
check_writable() {
  dir="$1"
  if [ -d "$dir" ]; then
    [ -w "$dir" ] || die "$dir 不可写。请用 sudo 运行，或用 --prefix 指定你有写权限的目录（如 \$HOME/.local/bin）。" 5
  else
    # 目录不存在：需检查最近的已存在祖先是否可写。
    probe="$dir"
    while [ ! -d "$probe" ]; do
      parent="$(dirname "$probe")"
      [ "$parent" != "$probe" ] || break
      probe="$parent"
    done
    [ -w "$probe" ] || die "无法创建 $dir（$probe 不可写）。请用 sudo 运行，或换用 --prefix。" 5
  fi
}

# ---------- 主流程 ----------

parse_args "$@"
pick_downloader
pick_hasher

OS="$(detect_os)"
ARCH="$(detect_arch)"
ASSET="$(asset_name "$OS" "$ARCH")"

ON_ANDROID=0
[ "$OS" = "android" ] && ON_ANDROID=1

# Android 上 exec 需要 cgo 版。默认取 cgo 包；取不到时明确说明，不静默换包。
if [ "$ON_ANDROID" -eq 1 ]; then
  ASSET="$ASSET-cgo"
fi

info "LiCore 安装脚本"
info "  平台      : $OS/$ARCH"
info "  资产      : $ASSET.tar.gz"
info "  版本      : $VERSION"
info "  安装到    : $PREFIX/$BINARY"
if [ "$ON_ANDROID" -eq 1 ]; then
  info "  说明      : Android 使用 cgo 版（licore exec 可用）"
fi
info ""

if [ "$DRY_RUN" -eq 1 ]; then
  # dry-run 只描述动作。仍需解析版本号，这样用户能看到实际会装哪个版本。
  TAG="$(resolve_version)"
  BASE="$LICORE_BASE_URL/$TAG"
  info "--- dry-run：以下操作不会真正执行 ---"
  info "1. 下载 $BASE/$ASSET.tar.gz"
  info "2. 下载 $BASE/SHA256SUMS"
  info "3. 用 $HASHER 校验 $ASSET.tar.gz 的 sha256（能发现传输损坏；不能防篡改）"
  info "4. 解压出 $BINARY"
  if [ -e "$PREFIX/$BINARY" ]; then
    info "5. 安装到 $PREFIX/$BINARY（该文件已存在$([ "$FORCE" -eq 1 ] && echo "，--force 已指定，将覆盖" || echo "，未加 --force 时会拒绝")）"
  else
    info "5. 安装到 $PREFIX/$BINARY"
  fi
  info "6. 校验可执行：$PREFIX/$BINARY version"
  info ""
  info "dry-run 结束，未做任何改动。"
  exit 0
fi

check_writable "$PREFIX"

TAG="$(resolve_version)"
BASE="$LICORE_BASE_URL/$TAG"

# 已存在时默认拒绝，避免悄悄覆盖用户已有的 licore。
TARGET="$PREFIX/$BINARY"
if [ -e "$TARGET" ] && [ "$FORCE" -ne 1 ]; then
  die "$TARGET 已存在。如需覆盖请加 --force，或先用 --prefix 换个目录。" 1
fi

WORK="$(mktemp -d)"
# 无论成功失败都清理临时目录。
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

info "下载 $ASSET.tar.gz ..."
download "$BASE/$ASSET.tar.gz" "$WORK/$ASSET.tar.gz" \
  || die "下载失败：$BASE/$ASSET.tar.gz。请确认版本 $TAG 存在且包含该平台资产。" 1

info "下载 SHA256SUMS ..."
download "$BASE/SHA256SUMS" "$WORK/SHA256SUMS" \
  || die "下载失败：$BASE/SHA256SUMS" 1

info "校验完整性 ..."
# 从 SHA256SUMS 中取出本资产的期望摘要。格式为 "<hex>  <name>"。
#
# **必须容忍文件名带 "./" 前缀**：`sha256sum *` 在部分环境（含本项目 Release
# workflow 用的 busybox sha256sum）输出 `./name`，而 `sha256sum name` 输出
# `name`——两种都是合法格式。只匹配不带前缀会让**所有用户在安装时被拒**。
# 真机实测踩到过：Release 的 SHA256SUMS 里是 `./licore-linux-amd64.tar.gz`，
# 而这里按不带前缀比较，报 "SHA256SUMS 中没有该资产的记录，拒绝安装"。
expected="$(awk -v want="$ASSET.tar.gz" '
  $2 == want       { print $1; exit }
  $2 == "./" want  { print $1; exit }
' "$WORK/SHA256SUMS")"
[ -n "$expected" ] || die "SHA256SUMS 中没有 $ASSET.tar.gz 的记录，拒绝安装。" 4

actual="$(hash_file "$WORK/$ASSET.tar.gz")"
if [ "$expected" != "$actual" ]; then
  warn "期望: $expected"
  warn "实际: $actual"
  die "校验失败，归档可能已损坏或被改动，拒绝安装。" 4
fi
info "  校验通过（注意：仅能发现传输损坏，不能防篡改）"

info "解压 ..."
mkdir -p "$WORK/extract"
tar -xzf "$WORK/$ASSET.tar.gz" -C "$WORK/extract" || die "解压失败，归档可能损坏。" 1
[ -f "$WORK/extract/$BINARY" ] || die "归档中缺少 $BINARY，拒绝安装。" 4
[ -x "$WORK/extract/$BINARY" ] || chmod +x "$WORK/extract/$BINARY"

info "安装到 $PREFIX ..."
mkdir -p "$PREFIX" 2>/dev/null || true
# 先装到临时文件再原子替换，避免覆盖过程中被中断留下半个二进制。
install -m 0755 "$WORK/extract/$BINARY" "$PREFIX/.$BINARY.new" \
  || die "写入 $PREFIX 失败（权限不足？）。请用 sudo，或换 --prefix。" 5
mv -f "$PREFIX/.$BINARY.new" "$TARGET" \
  || die "替换 $TARGET 失败。" 5

info ""
info "LiCore $TAG 已安装到 $TARGET"

# 尽力做一次可执行性验证；失败只提示，不回滚（回滚会掩盖真实原因）。
if command -v "$BINARY" >/dev/null 2>&1; then
  info "验证: $("$BINARY" version 2>/dev/null || echo '（执行 version 失败，可能是权限或平台问题）')"
else
  case ":$PATH:" in
    *":$PREFIX:"*) info "验证: $("$TARGET" version 2>/dev/null || echo '（执行 version 失败）')" ;;
    *)
      info ""
      info "注意: $PREFIX 不在 PATH 中，请把它加入 PATH，或用绝对路径调用 $TARGET"
      ;;
  esac
fi
