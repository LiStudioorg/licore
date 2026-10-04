# 发布检查清单

打 tag 前请**逐项**走完本清单。核心纪律只有一条：

> **tag 是一次性的发布动作，不是可以事后修订的指针。**
> 所有改动（**包括文档**）先 commit 并推送完，验证通过，**最后**才打 tag。

原因见下文《为什么 tag 之后不能再补 commit》。

---

## 0. 前置确认

- [ ] 工作树干净：`git status --short` 输出为空
- [ ] 当前分支就是 `main`，且与远端一致：`git rev-parse HEAD origin/main` 两者相同
- [ ] 本次发布的**全部**改动都已 commit —— 包括 README、AGENTS.md、`docs/`、
      CHANGELOG 之类的文字改动。**不要**抱着"tag 完再补文档"的想法往下走
- [ ] 版本号已确定（如 `v0.7.1`），且**未**被占用：`git tag -l | grep v0.7.1` 应为空；
      若该版本曾被 proxy 抓取过，见文末《已经缓存过的版本号》

## 1. 代码验证

```bash
export GOCACHE=/tmp/boxli-gocache GOMODCACHE=/tmp/boxli-modcache GOPATH=/tmp/boxli-gopath

# 格式化：必须无输出
gofmt -l $(git ls-files '*.go')

# 静态检查
go vet ./...

# 全量测试
go test ./... -count=1
```

- [ ] `gofmt -l` 输出为空
- [ ] `go vet ./...` 无告警
- [ ] `go test ./... -count=1` 全绿（`-count=1` 是为了绕开测试缓存，必须带）

## 2. 交叉编译

发布的代码必须能在声明的全部目标平台编译通过。v0.7.5 起全仓库零 CGO，
因此**只有这一组纯 Go 目标**（不再有 cgo 变体、也不再用 `-tags nocgo_exec`）：

```bash
for t in "linux amd64" "linux arm64" "linux arm" "linux 386" "linux riscv64" \
         "android arm64" "darwin arm64" "darwin amd64"; do
  set -- $t
  CGO_ENABLED=0 GOOS=$1 GOARCH=$2 go build -o /tmp/rel-$1-$2 . \
    && echo "OK   $1/$2" || echo "FAIL $1/$2"
done
```

- [ ] 上面每个平台都输出 `OK`

> **关于静态链接**：`CGO_ENABLED=0` 保证「无 cgo / 无 glibc 依赖」，但**不等于
> 一律静态链接**——linux/* 是静态的，而 android/arm64 与 darwin/* 因平台运行时
> （bionic linker / Mach-O DYLDLINK）本就是动态链接。校验时**不要用 `ldd`**
> （对异构 ELF 会谎报），用 `file` 与 `go version -m` 判断，判据见
> [.github/workflows/release.yml](.github/workflows/release.yml) 的构建步骤。
> v0.7.5 首次 Release 就是被这个误解 + `ldd` 判据坑掉的。

> **为什么必须每个平台都编**：只在 linux 上验证会漏掉平台专属符号的缺失。
> v0.7.0 开发期间就发生过一次：`internal/cli/platform.go`（无 build tag）引用了
> 只在 `//go:build linux` 下存在的 `doctor.AndroidEnv`，linux 一切正常、
> `darwin/arm64` 直接编译失败。是交叉编译抓住的。

## 3. Makefile 目标自检

```bash
make clean && make all && make test && make vet && make fmt && make clean
```

- [ ] `make all` 产出 `dist/licore-linux-amd64`、`dist/licore-linux-arm64`、
      `dist/licore-android-arm64`
- [ ] `make test` / `make vet` / `make fmt` 全过
- [ ] `make android` 无需 NDK 即可成功（v0.7.5 起无 cgo 变体，不存在降级分支）
- [ ] `make clean` 后 `dist/` 已删除

## 4. 真机验证（特权路径，有 root 服务器时）

```bash
sudo -n env LICORE_BIN=/usr/local/bin/licore LICORE_HOME=<临时目录> \
  timeout 1500 bash docs/verify-root.sh
```

- [ ] 脚本输出以 `== ALL PASS ==` 结尾且 **`FAIL=0`**
      （判据是"零失败"，不是某个固定的 PASS 数——PASS 数随脚本新增用例而增长；
      历史结果见 `docs/test-report-v0.6.0.md` 与 `docs/verify-root.md`）
- [ ] 若无 root 环境，**必须在发布说明里明确写"特权路径本次未在真机验证"**，
      不得默认通过
- [ ] 清理：shim 残留、`/server` 孤儿、`/sys/fs/cgroup/licore/**`（内层先 rmdir）、
      nft 表 `ip licore` / `licore-fwd`、`licore0` 网卡

## 5. 改名/残留核查（改动涉及命名时）

```bash
# 全仓库扫旧名（当前旧名为 boxli，历史遗留）
grep -rn 'boxli\|Boxli\|BOXLI' --include='*.go' --include='*.md' --include='*.sh' .
```

- [ ] 输出中**只允许**两类内容：指向本机 checkout 的绝对路径、以及"有意解释改名"
      的说明性文字（如 README 改名通知）
- [ ] 逐条确认，不放过任何"看起来像漏网"的命中

## 6. 打 tag 并推送

```bash
git tag -a vX.Y.Z -m "LiCore vX.Y.Z

<发布说明：本版本新增/修复了什么，破坏性变更与迁移说明>"

git push origin main
git push origin vX.Y.Z
```

- [ ] 用 **annotated tag**（`-a`），不要轻量 tag —— 发布需要 tagger、日期与说明
- [ ] tagger 身份是本仓库配置的发布身份（`git config user.name` / `user.email`）
- [ ] `git ls-remote --tags origin | grep vX.Y.Z` 能看到 tag 与其指向的 commit
- [ ] tag 指向的 commit **就是** `main` 的 HEAD：`git rev-parse --short 'vX.Y.Z^{}'`
      与 `git rev-parse --short HEAD` 相同

**到此为止。tag 推送之后不要再动它。**

## 7. 发布后确认

```bash
# 触发 proxy 抓取
GOPROXY=proxy.golang.org go list -m github.com/LiStudioorg/licore@vX.Y.Z

# 校验 proxy 三件套
for f in info mod zip; do
  printf "%-5s " "$f"
  curl -s -o /dev/null -w "%{http_code}\n" \
    "https://proxy.golang.org/github.com/!li!studioorg/licore/@v/vX.Y.Z.$f"
done
```

- [ ] `go list -m` 解析出期望的版本号
- [ ] 三件套都是 `200`
- [ ] **抽查 zip 内容确实是本次发布的代码**（重点：若本次含文档改动，确认文档在里面）：
  ```bash
  curl -s "https://proxy.golang.org/github.com/!li!studioorg/licore/@v/vX.Y.Z.zip" -o /tmp/rel.zip
  unzip -p /tmp/rel.zip '*/README.md' | head -20
  ```
- [ ] `https://pkg.go.dev/github.com/LiStudioorg/licore` 页面出现（**异步**，可能滞后
      几分钟到数小时；未出现时记录状态即可，**不要反复重试**）

---

## 为什么 tag 之后不能再补 commit

**Go module proxy 对已发布 tag 的缓存是不可变的。** 某个版本号一经
`proxy.golang.org` 抓取，其 `.info` / `.mod` / `.zip` 三个产物就被固定下来：

- **移动 tag 不会刷新已缓存内容** —— 重新下载拿到的仍是旧内容
- pkg.go.dev 读的正是这份缓存，因此展示的也是**旧内容**
- 这是 Go 代理的设计特性（保证依赖可复现），**不是缺陷**，也无法通过重试解决

**因此**："先打 tag，之后再补 commit 然后移动 tag 让内容进去"——这个做法**不会生效**，
只会让 git 里的 tag 指向与 proxy 缓存内容**不一致**，产生一个"看起来发了、其实没发"
的文档缺口。

### 教训来源：v0.7.0

v0.7.0 的实际顺序是：

1. 打了 `v0.7.0` → 推送 → proxy 抓取并缓存 `d256eec`
2. **之后**才补上 README 的改名通知与 AGENTS.md 的项目历史节（commit `7875f4f`）
3. 再把 `v0.7.0` 移动到 `7875f4f` 并强推

结果：

| 位置 | 指向/内容 |
| --- | --- |
| git 里的 `v0.7.0` | `7875f4f`（**含**改名通知） |
| proxy 缓存的 `v0.7.0` | `d256eec`（**不含**改名通知） |
| pkg.go.dev 展示的 README | 来自 proxy 缓存，**缺**改名通知 |

实测确认：重新下载 `v0.7.0.zip`，其中 README 的改名通知计数为 `0`、AGENTS.md 的
项目历史节计数为 `0`；而本地 `7875f4f` 的 README 计数为 `1`。

**结论**：`v0.7.0` 保持不动（该 tag 的**功能代码**是正确的 v0.7.0，缺的只是文档
快照），不改 tag、不动已缓存内容，把流程固化为本清单。

## 已经缓存过的版本号

某个版本号一旦被 proxy 抓取过，**该版本号的内容就永久固定**。若发现某次发布的
内容有误且已缓存：

- **不要**移动 tag（无效，且造成不一致）
- **不要**删除后重建同名 tag（proxy 缓存仍在，且会让下游校验失败）
- **只能发新版本号**（如 `v0.7.1`）—— 新版本号走全新缓存，不受旧缓存影响

是否值得为此发补丁版，按"内容重要性 vs 发布节奏稳定性"权衡。若缺的只是文档快照、
GitHub 主页显示的 README 已是正确的，通常**不值得**为此发补丁版。
