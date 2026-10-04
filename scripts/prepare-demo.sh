#!/usr/bin/env bash
# 为 scripts/demo.tape 准备（或清理）一次性的演示环境。
#
#   scripts/prepare-demo.sh              构建演示镜像与临时数据目录
#   scripts/prepare-demo.sh --cleanup    删除全部演示状态（容器、临时 HOME、二进制）
#
# 演示状态全部放在 /tmp/licore-demo 下：不写入默认数据目录 ~/.licore，
# 不触碰任何既有容器或网络。licore 的 exec / veth / NAT 需要 root，
# 因此脚本通过 sudo 准备 root 属主的容器数据目录。
set -euo pipefail

D=/tmp/licore-demo
SRC=$(cd "$(dirname "$0")/.." && pwd)   # 仓库根目录

if [ "${1:-}" = "--cleanup" ]; then
    export LICORE_HOME=$D/home
    BIN=$D/bin/licore
    if [ -x "$BIN" ]; then
        # 尽力收尾：停掉并删除可能残留的演示容器（规则随 rm 一并清除）。
        sudo -n env LICORE_HOME=$D/home "$BIN" stop demo >/dev/null 2>&1 || true
        sudo -n env LICORE_HOME=$D/home "$BIN" rm -f demo >/dev/null 2>&1 || true
    fi
    # 录制用的临时 PATH 入口（符号链接，指向演示二进制）；系统安装不受影响。
    if [ "$(readlink -f /usr/local/bin/licore 2>/dev/null)" = "$D/bin/licore" ]; then
        sudo -n rm -f /usr/local/bin/licore
    fi
    sudo -n rm -rf "$D"
    echo "已清理 $D（默认数据目录与既有容器未受影响）"
    exit 0
fi

# —— 1. 构建演示二进制（注入版本号与正式发布一致）——
mkdir -p "$D/bin" "$D/ctx/bin" "$D/ctx/www" "$D/home/boot" "$D/build-home"
( cd "$SRC" && CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(git -C "$SRC" describe --tags --abbrev=0 2>/dev/null || echo dev)" -o "$D/bin/licore" . )
ln -sf "$D/bin/licore" /usr/local/bin/licore 2>/dev/null \
    || sudo -n ln -sf "$D/bin/licore" /usr/local/bin/licore   # Require licore 能在录制 PATH 里命中演示二进制

# —— 2. 演示镜像：scratch + busybox + 静态页面 + applet 软链 ——
cp /bin/busybox "$D/ctx/bin/" 2>/dev/null || cp "$(command -v busybox)" "$D/ctx/bin/"
for a in hostname cat ls; do ln -sf busybox "$D/ctx/bin/$a"; done
printf '<!doctype html>\n<html><body><h1>hello from LiCore</h1></body></html>\n' > "$D/ctx/www/index.html"
cat > "$D/ctx/Boxfile" <<'BOX'
FROM scratch
COPY bin /bin
COPY www /www
CMD ["/bin/busybox", "httpd", "-f", "-p", "8080", "-h", "/www"]
BOX
export LICORE_HOME=$D/build-home
"$D/bin/licore" build -t demo:v1 --context "$D/ctx" >/dev/null
"$D/bin/licore" save demo:v1 -o "$D/demo.licore" >/dev/null

# —— 3. 演示数据目录：boot 标记预置，避免录屏被首次使用引导打断 ——
touch "$D/home/boot/marker"
chown -R "$(id -u):$(id -g)" "$D/home" 2>/dev/null || sudo -n chown -R "$(id -u):$(id -g)" "$D/home"

# —— 4. 录制以 root 运行 vhs（licore exec 需要 root），
#      /usr/local/bin/licore 这根临时链接让录制 shell 直接找到演示二进制；
#      首次使用引导已由 boot 标记跳过。
echo "演示环境就绪：$D"
echo "录制： sudo -E env LICORE_HOME=$D/home VHS_NO_SANDBOX=1 vhs \"$SRC/scripts/demo.tape\""
