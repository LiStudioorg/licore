# LiCore 镜像格式规范（.licore）

版本：`licore/image-spec v1`（草案）
状态：阶段 1 定义中
适用范围：LiCore 引擎、`licore hub` 构建与分发工具链

> ⚠️ 本规范与 Docker / OCI Image Spec **没有任何兼容关系**，也不计划建立映射。
> 任何"把 OCI 镜像转成 .licore"或反向的设计提案一律不进入 LiCore 仓库。

## 1. 总览

一个 `.licore` 文件是一个**未压缩的 POSIX tar 归档**，文件名以 `.licore` 结尾，内部布局如下：

```text
myapp-1.0.licore (tar)
├── index.json          # 唯一入口：镜像清单（UTF-8 JSON，必须位于归档中）
├── layers/
│   ├── 000001.base.tar.gz
│   ├── 000002.app.tar.gz
│   └── 000003.conf.tar.gz
└── blobs/              # 小对象目录，条目名 = <digest-algo>-<digest-hex>
    └── sha256-9f2c…    # config 指向的 blob（必需；index.config.digest 即其摘要）
```

设计原则：

1. **单一文件即完整镜像**：拉取、拷贝、校验都针对一个 `.licore` 文件，没有多文件目录约定。
2. **外层 tar 不压缩**：层本身已经是 gzip，外层再压缩只会让流式解析和随机读取变复杂。
3. **index.json 是唯一元数据源**：任何字段缺失都视为镜像损坏，绝不"尽力猜测"。

## 2. 层（layers/）

- 每个层是一个独立的 `tar.gz`（gzip RFC 1952，method=deflate）。
- 层内是普通 POSIX ustar/pax tar；文件条目表示"覆盖"，删除用白out 约定（见 2.1）。
- 层文件名固定格式：`layers/NNNNNN.<name>.tar.gz`，`NNNNNN` 为 6 位十进制序号（从 `000001` 开始，零填充），与 `index.json` 中 `layers[].applyOrder` 一一对应。
- 解压后的层目录称为 diffid 目录，按 content 去重（同一 digest 的层在不同镜像中共享存储）。

### 2.1 删除标记（whiteout）

删除下层文件时，在层内同名位置写入：

- `.wh.<basename>` 的空 regular 文件 → 删除 `<basename>`
- 目录前缀 `.wh..wh..opq` 空文件置于某目录 → 该目录整体屏蔽（opaque dir）

这是自研约定，仅借用了"前缀 .wh."这一类命名习惯，语义由本文件定义，与 OCI 实现无代码或兼容关系。

## 3. index.json

UTF-8 JSON，无 BOM，允许任意空白，解析器必须容忍键序。Schema 如下（字段名区分大小写）：

```json
{
  "mediaType": "application/x.licore.manifest+json",
  "specVersion": "licore/image-spec/v1",
  "schemaVersion": 1,
  "architecture": "arm64",
  "os": "linux",
  "created": "2026-10-01T08:00:00Z",
  "name": "alice/myapp",
  "version": "1.0.0",
  "config": {
    "digest": "sha256:9f2c…",
    "sizeBytes": 512
  },
  "layers": [
    {
      "path": "layers/000001.base.tar.gz",
      "digest": "sha256:1a7f…",
      "sizeBytes": 10485760,
      "applyOrder": 1
    },
    {
      "path": "layers/000002.app.tar.gz",
      "digest": "sha256:bb03…",
      "sizeBytes": 2097152,
      "applyOrder": 2
    }
  ],
  "annotations": {
    "org.licore.build.tool": "licore-hub/0.3.0"
  }
}
```

### 3.1 字段定义

| 字段 | 类型 | 必填 | 约束 |
| --- | --- | --- | --- |
| `mediaType` | string | ✅ | 固定 `application/x.licore.manifest+json`，不匹配即拒绝 |
| `specVersion` | string | ✅ | 固定 `licore/image-spec/v1`；未来仅允许递增此值做不兼容升级 |
| `schemaVersion` | int | ✅ | 当前固定 `1` |
| `architecture` | string | ✅ | `amd64` / `arm64` / `arm` / `386` / `riscv64` / `loong64`（与 `GOARCH` 对齐） |
| `os` | string | ✅ | `linux` / `android` / `darwin`（与 `GOOS` 对齐；android 视为 linux 的发行形态，由镜像自述） |
| `created` | string | ✅ | RFC 3339 UTC，秒精度 |
| `name` | string | ✅ | 镜像仓库名 `^[a-z0-9][a-z0-9._/-]{0,254}$`，用于默认命名，不作校验锚 |
| `version` | string | ✅ | 语义化版本或 tag 名，同仓库 tag 规则 |
| `config` | object | ✅ | 容器运行配置小对象（见 3.3）；`digest`/`sizeBytes` 指向 `blobs/<algo>-<hex>`，该 blob 必需存在 |
| `layers` | array | ✅ | 1 ≤ len ≤ 127；`applyOrder` 必须恰为 1..n 且 path 唯一 |
| `layers[].path` | string | ✅ | 归档内相对路径，必须以 `layers/` 开头，禁止 `..`、绝对路径、反斜杠 |
| `layers[].digest` | string | ✅ | 见 3.2 |
| `layers[].sizeBytes` | int | ✅ | 归档内该条目的未压缩字节数，必须与 tar 头一致 |
| `layers[].applyOrder` | int | ✅ | 合并顺序，从 1 严格递增 |
| `annotations` | object | ❌ | `map[string]string`，仅 `org.licore.*` / 项目自有前缀 |

### 3.2 digest

- 格式：`sha256:<64位小写hex>`。当前只允许 `sha256`，新增算法须先修订本规范。
- 摘要对象：`layers[].digest` = 对应 tar.gz **原始字节**的 SHA-256；`config.digest` = config 对象序列化后字节的 SHA-256。
- 校验时机：`licore pull` 落地时全量校验；`licore run` 启动前只重算 config 与层头 4 KiB 抽样（信任 storage 层记录的校验态）。

### 3.3 config 对象

`config` 指向一个小 JSON 对象（≤ 64 KiB），存放容器运行配置，字段与 `licore run` 的参数一一对应，配置文件族沿用 YAML 风格的键名：

```json
{
  "entrypoint": ["/usr/bin/myapp"],
  "cmd": ["--config", "/etc/myapp/config.yaml"],
  "env": {"PATH": "/usr/local/bin:/usr/bin:/bin", "LANG": "C.UTF-8"},
  "workingDir": "/app",
  "user": "1000:1000",
  "expose": ["8080/tcp"],
  "volumes": ["/var/lib/myapp"],
  "labels": {"org.licore.maintainer": "alice"}
}
```

阶段 1 先支持 `entrypoint` / `cmd` / `env` / `workingDir`；其余字段解析但不生效，遇到未知键必须报错（防配置漂移静默生效）。

## 4. 校验与拒绝规则（规范性）

解析器（`internal/image`）必须拒绝以下任一情况，错误信息用 `fmt.Errorf("...: %w", err)` 包装：

1. 归档中存在绝对路径、`..` 段、符号链接逃逸出根目录的条目 → `ErrUnsafePath`
2. `index.json` 缺失、非 JSON、`mediaType`/`specVersion` 不匹配 → `ErrBadManifest`
3. `layers[].path` 指向归档中不存在的条目，或 `sizeBytes` 与 tar 头不符 → `ErrLayerMissing` / `ErrSizeMismatch`
4. 层内容 digest 不匹配 → `ErrDigestMismatch`
5. `applyOrder` 不是 1..n 严格递增 → `ErrBadApplyOrder`
6. `architecture` / `os` 与运行时不匹配且未显式 `--allow-arch-mismatch` → `ErrArchMismatch`（Android 上 `os=linux` 镜像视为兼容）
7. 层内出现 hardlink 指向 `..` 目标、设备文件、ACL/xattr 扩展位含未知命名空间 → `ErrUnsafeLayer`（设备节点与 setuid 位由 storage 层在解包时统一剥离）
8. `blobs/<algo>-<hex>` 缺失、大小与 `config.sizeBytes` 不符、内容摘要与 `config.digest` 不符、或不是合法 config JSON → `ErrConfigMissing` / `ErrSizeMismatch` / `ErrDigestMismatch` / `ErrBadManifest`

## 5. 存储布局（本地）

```text
~/.licore/
├── config.yaml
├── images/
│   └── <name>/<version>/
│       ├── source.licore        # 原始 .licore 文件
│       ├── index.json          # 解析出的清单（source of truth 的展开副本）
│       └── state.json          # 校验状态：全部层 OK/损坏、拉取时间
└── layers/
    └── sha256/<hex>/           # 按层 digest 内容寻址的解压结果
        ├── fs/                 # 合并前的单层文件系统视图
        └── meta.json
```

去重规则：层以 digest 寻址，跨镜像共享；删除镜像只减引用计数，引用清零后由 `licore images prune` 清理（阶段 2）。

## 6. 与工具链的关系

- `licore pull <file>.licore`：本地文件 → 校验 → 落地 `~/.licore/images`（阶段 1 已支持）
- `licore hub pack`（未实现）：目录 → 分层 → 生成 `index.json` → 输出 `.licore`
- 分发（阶段 2+）：`hub.licore.dev` 自研极简 registry，tag → `.licore` 文件 + `index.json` 摘要接口，不提供任何 Docker Distribution API 兼容层。

## 7. 版本演进策略

- 不兼容变更：只允许通过 `specVersion` 升级（如 `licore/image-spec/v2`），旧引擎必须明确拒绝。
- 兼容变更：新增可选 `annotations` 键、新增 `os`/`architecture` 枚举值，旧引擎按未知值告警跳过。
