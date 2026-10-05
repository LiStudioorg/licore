# LiCore 演示 GIF —— 从镜像导入到容器下线，全程单二进制。
#
#   准备环境：  scripts/prepare-demo.sh
#   重新录制：  sudo -E env LICORE_HOME=/tmp/licore-demo/home VHS_NO_SANDBOX=1 \
#                   vhs scripts/demo.tape              （需 root，见下）
#   录完清理：  scripts/prepare-demo.sh --cleanup
#
# 可复现性与隔离：
#   - 不依赖外部镜像仓库：demo.licore 由 prepare-demo.sh 用 `licore build`
#     （FROM scratch + busybox + applet 软链）现场构建，pull 走本地文件导入。
#   - 数据目录是临时的 LICORE_HOME=/tmp/licore-demo/home：不写 ~/.licore，
#     不触碰机器上其它容器与网络资源；录完 --cleanup 全部清除。
#   - 以 root 运行 vhs：licore exec 要 setns 进容器命名空间（需 root），
#     这样画面里只有 licore 命令本身，没有提权噪音。
#   - 首次使用引导（询问开机自启）由 prepare-demo.sh 预置 boot 标记跳过。

Output docs/demo.gif

Set Theme      "Dracula"
Set FontFamily "DejaVu Sans Mono"
Set FontSize   22
Set Width      1200
Set Height     820
Set Framerate  30
Set TypingSpeed 30ms
Set LoopOffset 100%

Require licore

# —— 演示目录（demo.licore 在这里；由 prepare-demo.sh 生成）——
Type "cd /tmp/licore-demo"
Enter
Sleep 1s

# 注：这里刻意**不**演示 `licore --version`。
# 原因：GIF 会固化版本号，每次发版就与画面不符（如刚发布的 v0.9.2/0.9.3），
# 演示资源"看一次就过期"。改为直接从 pull 开始——这些命令本身不随版本变化，
# 录一次可长期复用。

# —— 导入 .licore 镜像（本地文件，离线；全部层摘要校验）——
Type "licore pull ./demo.licore"
Enter
Sleep 2.2s

# —— 后台启动：-d 交给 shim 持有，-p 发布端口，--memory 设内存上限 ——
#
# `--memory 256` 不是随便加的，两个作用：
#   1. **消掉未设限额时的 WARN**——不设 --memory 会打一条 slog.Warn
#      （"容器可耗尽宿主内存并触发宿主 OOM killer"），在演示里占 4 行、
#      把画面节奏打乱；
#   2. **演示生产推荐用法**——README《默认无内存限额》一节要求生产环境
#      显式指定 --memory，演示里就该这么写，顺带把这条最佳实践展示出来。
Type "licore run -d --name demo -p 18080:8080 --memory 256 demo:v1"
Enter
Sleep 2.5s

# —— 真实状态：Up ——
Type "licore ps"
Enter
Sleep 2.2s

# —— 宿主直接访问发布端口（DNAT 端口映射生效）——
Type "curl -s http://localhost:18080/"
Enter
Sleep 2.5s

# —— exec 进入容器命名空间执行命令 ——
Type "licore exec demo -- /bin/hostname"
Enter
Sleep 2.2s

# —— 停止并删除：容器与 NAT 规则随 rm 清除，引擎本体全程不常驻 ——
Type "licore stop demo && licore rm demo"
Enter
Sleep 3s
