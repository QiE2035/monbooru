# AGENTS.md

本文件面向在本仓库工作的 AI 编码 Agent。它汇总项目结构、构建/测试命令、目录约定和编码风格，作为高效协作的入口。面向人类使用者的信息请阅读 [README.md](README.md)；面向用户的功能说明请阅读 [DOC.md](DOC.md)；面向用户的版本变更请阅读 [CHANGELOG.md](CHANGELOG.md)。

## 1. 项目概述

Monbooru 是一个**自托管、离线、轻量的图库(booru)管理程序**。它用单文件二进制对外提供 Web UI(基于 `net/http` 与 `htmx`)，将本地媒体(图片、视频、动图、CBZ/ZIP)组织为带标签、相册、关系、SD/ComfyUI 元数据的私人图库。默认完全离线，在线抓取能力由可选的 [monloader](https://github.com/leqwin/monloader) 配套应用承担。

- **语言/工具链**: Go `1.25.10`，`CGO_ENABLED=1` 才能启用 `tagger` 构建标签。
- **数据存储**: 每个 gallery 一个 SQLite 文件(`modernc.org/sqlite`，纯 Go 实现，无需 CGO)。
- **前端**: 服务端渲染 + htmx 局部刷新；无 npm/Node 构建，静态资源总大小约 300 KB，通过 `//go:embed` 直接打包进二进制。
- **AI 推理**: 默认关闭。通过 `tagger` 构建标签编译出 ONNX Runtime worker，常驻独立子进程(`monbooru tagger-worker`)，父进程通过 TCP loopback 与之通信。支持多执行提供者切换：CPU / CUDA / DirectML / TensorRT / OpenVINO / CoreML。
- **远程打标**: 支持将打标任务发送到另一台配对的 monbooru 实例(`remote_client`)，或在本地作为服务端接受远程打标请求(`remote_server`)。
- **核心能力**: 标签/分类(含内置 `species` 类别)、相册/收藏、Stable Diffusion 与 ComfyUI 元数据解析、基于 gojq 的 ComfyUI 工作流 `jq:` 搜索过滤、perceptual hash(pHash)与 BK-tree 查重、可撤销的批量操作、文件监视(macOS/Linux 平台，Windows 用轮询回退)、多 gallery 实例(支持跨 gallery 转移)、scoped Bearer Token 的 REST API、OpenAPI 规范、图片标注(annotations)、原始来源追踪与批量刷新、Public Tag Repository(PTR)贡献与查询、每日定时维护任务调度。

## 2. 仓库结构

```
monbooru/
├── cmd/monbooru/         # 主二进制入口;包含 main.go、worker.go(tagger 子进程)、worker_stub.go(无 tagger 时的占位)、healthcheck.go(Docker 健康检查)
├── internal/
│   ├── api/              # REST API(OpenAPI、scoped bearer token);包含 images_dto.go(DTO 定义)
│   ├── config/           # monbooru.toml 解析与校验;支持 relations/schedule/ui/tagger.remote_* 等配置节
│   ├── db/               # SQLite 连接池、schema.sql、placeholders.go(占位符工具)
│   ├── gallery/          # 图库核心:摄取、缩略图、归档(manga/CBZ)、监视器、删除、移动、annotations、sources、phash、transfer;hidden.go 处理点前缀隐藏文件/目录
│   ├── jobs/             # 通用后台任务状态
│   ├── logx/             # 简明结构化日志(slog 封装)
│   ├── metadata/         # PNG/JPG/WebP/ComfyUI/A1111/ComicInfo 元数据解析
│   ├── models/           # 跨包共享的数据结构
│   ├── relations/        # pHash 查重、BK-tree、find-pairs 任务、queue_prune.go(队列清理)、service.go(关系服务层)
│   ├── search/           # 自研搜索解析器与 SQL 生成器;fastcount.go 优化大库查询;comfyui_jq.go 基于 gojq 提供 jq: 过滤;similarityfilter.go 相似度过滤
│   ├── searchkw/         # 关键字辅助索引
│   ├── tagger/           # ONNX 自动打标(catalog/profile/dispatch/inproc/ipc/aggregation);dispatch_default/ 与 profile_default/ 含模型配置;remote_backend.go 远程 HTTP 推理客户端;store.go 打标结果落库与远程委托
│   ├── tags/             # 标签、分类、别名、蕴含、合并、image_tag_sources.go(来源追踪)、suggest.go(自动补全)、similarity.go(标签重叠相似度)
│   └── web/              # HTTP 处理器、路由、模板、认证、CSRF、会话、scheduler.go(定时任务)、pairing.go(monloader 配对);compatibility 子包提供 Hydrus/Blombooru 导入
├── web/
│   ├── embed.go          # //go:embed static templates → 打进二进制
│   ├── static/           # main.css、main.js、htmx.min.js、logo、favicon
│   └── templates/        # Go html/template;partials/ 是共享片段
├── docker/               # Dockerfile、Dockerfile.cuda、docker-compose.yml、monbooru.container
├── .github/workflows/    # release.yml:打 tag 时构建 CPU/CUDA 镜像并发布 GitHub Release
├── Makefile              # 顶层构建/测试/lint/覆盖率入口
├── go.mod / go.sum       # 依赖(BurntSushi/toml、fsnotify、gojq、onnxruntime_go、x/crypto、x/image、modernc.org/sqlite、goexif)
├── .golangci.yml         # 仅启用 gofmt
├── VERSION.md            # 当前版本号(Makefile 注入 -ldflags)
├── REPOSITORY.md         # 仓库 URL(同上)
├── DOC.md                # 文档站点 URL(同上)
├── README.md
├── CHANGELOG.md          # 按 ## [vx.y.z] - 日期 排序，发布时取最新条目
└── LICENSE
```

## 3. 环境与依赖

- 基础要求:**Go 1.25.10+**、`git`。
- 搜索增强:`github.com/itchyny/gojq` 用于解析 ComfyUI 工作流 JSON 的 `jq:` 搜索过滤器。
- 启用 `tagger` 标签:**CGO 必须开启**(`CGO_ENABLED=1`)，并能链接 ONNX Runtime 共享库。Linux/macOS 查找 `libonnxruntime.so`，Windows 查找 `onnxruntime.dll`；可通过环境变量 `ORT_LIB_PATH` 显式覆盖路径。
- 可选外部工具:**ffmpeg/ffprobe**(视频缩略与动图预览)；Dockerfile 会从 BtbN 静态构建下载。
- Windows 上部分测试在容器外可能因路径/权限差异失败(见 §7)。

> **重要**: 修改 `go.mod` 时不要手动编辑 `go.sum`；`go mod tidy` 必须成功。提交前 `go mod tidy` 并检查无意外新增 indirect 依赖。

## 4. 常用命令

下列命令都在仓库根目录执行。Makefile 已封装常见组合；直接调 `go` 也行。

| 用途 | 命令 |
| --- | --- |
| 构建主二进制(无 tagger) | `make build` 或 `go build ./cmd/monbooru` |
| 构建含 ONNX 推理 | `CGO_ENABLED=1 make build-tagger` 或 `CGO_ENABLED=1 go build -tags tagger ./cmd/monbooru` |
| Windows 构建 tagger 二进制 | `CGO_ENABLED=1 go build -tags tagger -o monbooru.exe ./cmd/monbooru` |
| 运行 | `./monbooru -config ./monbooru.toml` |
| 生成密码哈希 | `./monbooru -hash-password 'yourPassword'` |
| 健康检查(如 Docker HEALTHCHECK) | `./monbooru healthcheck -config ./monbooru.toml` |
| 全部测试(含 race) | `make test` 或 `go test -race ./...` |
| 仅打标器相关测试 | `make test-tagger` 或 `go test -tags tagger ./...` |
| 静态检查 | `make lint` |
| 覆盖率(排除 cmd 与 tagger) | `make coverage` |
| 格式化 | `gofmt -w .` |
| 整理依赖 | `go mod tidy` |

构建产物文件名 Windows 为 `monbooru.exe`，类 Unix 平台为 `monbooru`。`make build` 会把 `VERSION.md`、`REPOSITORY.md`、`DOC.md` 中的字符串通过 `-ldflags -X` 注入到 `internal/web` 的导出变量上(Version/RepoURL/DocURL)，务必保证这三个文件存在且无前后空白。

## 5. 配置文件 `monbooru.toml`

`internal/config/config.go` 是唯一可信来源。常见字段/节:

```toml
default_gallery = "default"

[server]
bind_address = "127.0.0.1:8080"   # 也可被环境变量 MONBOORU_SERVER_BIND_ADDRESS 覆盖
base_url     = "http://localhost:8080"
name         = "Monbooru"          # 出现在 <title> 与顶栏；空时回退 "Monbooru"
logo         = ""                  # 可选:指向 favicon/顶栏 logo 的绝对路径
custom_css   = ""                  # 可选:在 main.css 之后加载，便于 :root 覆盖
monloader_url = ""                 # 浏览器访问 monloader 的地址

[monloader]
api_url    = ""                    # server 端访问 monloader 的 URL，留空则从配对来源自动识别
api_token  = ""                    # 配对后由 monloader 颁发
paused     = false                 # 可选:暂停所有 monloader 调用但保留配对凭据

[paths]
data_path  = "./data"              # 派生:每个 gallery 的 *.db 与 thumbs 目录
model_path = "./models"            # ONNX 模型与标签 json 目录(只读)

[gallery]
watch_enabled       = true
max_file_size_mb    = 2048         # 默认 2GB，可在设置中提高到 5GB
default_upload_folder = ""

[[galleries]]
name         = "default"
gallery_path = "./gallery"

[tagger]
execution_provider       = "cpu"   # cpu / cuda / directml / tensorrt / openvino / coreml / coremlv2
parallel                 = 4
idle_release_after_minutes = 15

[tagger.aggregation]
min_hit_fraction = 0.05            # 多帧聚合时标签命中比例阈值

[[tagger.taggers]]
# 自动打标器实例配置

[tagger.remote_server]             # 作为服务端接受远程打标请求
allow_remote = false

[tagger.remote_client]             # 作为客户端把打标任务发给远端实例
url  = ""                          # 远端 monbooru 实例地址
token = ""                         # 远端颁发的打标 token

[auth]
enable_password          = false
password_hash            = ""        # ./monbooru -hash-password 生成
session_lifetime_days    = 7

[log]
level = "warn"                     # debug / info / warn / error

[relations]
default_distance         = 4       # find-pairs 与增量探测的默认汉明距离
default_session_order    = "smallest_distance_first"
incremental_on_ingest    = true    # 摄取时是否启用 BK-tree 增量探测
tag_pairs                = true    # 是否把标签相似度也加入关系对队列
tag_pair_threshold       = 0.85    # 标签相似度入队阈值

[schedule]
# 定时任务配置

[ui]
page_size     = 40
thumbnail_fit = "natural"          # natural(原始比例) | square(裁剪)
```

环境变量优先级高于 TOML。完整列表:
- `MONBOORU_SERVER_BIND_ADDRESS` - 监听地址
- `MONBOORU_SERVER_BASE_URL` - 基础 URL
- `MONBOORU_SERVER_MONLOADER_URL` - monloader 浏览器访问地址
- `MONBOORU_PATHS_DATA_PATH` - 数据目录(修改后会重新计算各 gallery 派生路径)
- `MONBOORU_PATHS_MODEL_PATH` - 模型目录
- `MONBOORU_GALLERY_WATCH_ENABLED` - 是否启用文件监视
- `MONBOORU_GALLERY_MAX_FILE_SIZE_MB` - 最大文件大小(MB)
- `MONBOORU_TAGGER_EXECUTION_PROVIDER` - 选择 ONNX Runtime Execution Provider(`cpu`/`cuda`/`directml`/`tensorrt`/`openvino`/`coreml`/`coremlv2`)
- `MONBOORU_TAGGER_WORKER_LOG` - tagger 子进程日志级别
- `MONBOORU_TAGGER_BACKEND` - tagger 后端类型
- `MONBOORU_AUTH_ENABLE_PASSWORD` - 是否启用密码认证
- `MONBOORU_AUTH_PASSWORD_HASH` - 密码哈希
- `MONBOORU_AUTH_SESSION_LIFETIME_DAYS` - 会话有效期(天)
- `MONBOORU_MONLOADER_API_URL` - monloader API 地址
- `MONBOORU_MONLOADER_API_TOKEN` - monloader API token
- `MONBOORU_LOG_LEVEL` - 日志级别
- `TZ` - 时区(影响时间戳展示；存储仍是 UTC)

## 6. 代码风格与约定

- **格式化**:`gofmt` 必须零差异提交。`.golangci.yml` 使用 v2 格式并将 `gofmt` 注册为 formatter。`make lint` 运行 `golangci-lint run`；如需自动格式化可运行 `golangci-lint fmt`。
- **导入**:标准库 → 第三方 → 本仓库，组之间空行；Go 1.25 风格(可使用 `slices`、`maps`、`cmp`)。
- **命名**:
  - 包名短小全小写、无下划线、无连字符；目录名即包名。
  - 导出符号需要有 godoc 注释，**首字母大写、句号结尾**(golangci-lint 默认 revive 规则一致)。`internal/*` 中的私有函数也鼓励注释解释「为什么」而不是「做什么」。
  - 接收者名短、类型首字母小写、保持一致(`s *Server`、`cfg *Config`)。
- **错误处理**:
  - 优先 `fmt.Errorf("...: %w", err)` 包装；在包边界用 `errors.Is`/`errors.As` 判断。
  - 永远不要 `log.Fatal` 在库代码内——只允许在 `cmd/monbooru` 顶层 `main` 使用。
  - 不吞 `error`；需要时显式 `_ = err` 并在旁注释理由。
- **并发**:共享状态使用 `sync.Mutex`/`RWMutex`；goroutine 派发前想清楚生命周期，长任务接受 `context.Context`。
- **SQL**:`?` 参数化占位符，不要字符串拼接；所有 SQL 集中在 `internal/db` 与各包的 `queries.go` 中。
- **日志**:使用 `internal/logx`(`slog` 封装)；不要在库内直接 `log.Println`。
- **前端**:`web/templates` 用 `html/template`，所有动态值经 `template.JSEscape` 或自动转义。**不要**引入 JS 框架或构建步骤；扩展用 htmx。
- **嵌入资源**:`web/embed.go` 列出要打包的目录；新增静态文件后需确认 `//go:embed` 通配仍覆盖到。
- **构建标签**:
  - `//go:build tagger` 用于 ONNX 推理相关代码。
  - `//go:build !tagger` 提供 stub，保证默认构建依然成功。
  - `internal/web/stats_linux.go` / `stats_other.go` 是按 GOOS 拆分的范例；新增平台相关代码时遵循同样模式。

## 7. 测试约定

> **当前状态**: 本整合分支(`integrate/v1.17`)暂不包含任何 `*_test.go` 测试文件，测试文件后续按需补充。下列约定作为补充测试时的规范。

- 标准库 `testing`，多数场景使用表驱动(table-driven)子测试；断言用 `t.Errorf`/`t.Fatalf`，避免外部断言库。
- `go test -race ./...` 必须在本地通过再提交。**任何共享状态的并发路径都要求 race 通过**。
- 测试文件命名:`xxx_test.go`，与被测文件同包同目录；白盒测试用包内测试，跨包边界用 `_test` 后缀包。
- 涉及 FS/SQL/网络的测试使用 `t.TempDir()`、`internal/db` 的内存或临时库，不要污染真实图库目录。
- **Windows 路径已知问题**:`internal/web` 下的 `TestBooruLogo_PathTrustedRootGate`、`TestSessionExpiry`、以及 `TestUploadPost_*Subfolder`/`TestUploadPost_Traversal` 涉及 OS 路径/chown 语义，在原生 Windows 上偶尔失败。这些是平台差异而非代码缺陷，建议在 Linux 容器或 WSL 中复跑；合并前不强制要求本机通过(详见 §14)。
- 覆盖率:`make coverage` 排除 `cmd/` 与 `internal/tagger`(后者依赖外部 ONNX 模型)；重要功能点(搜索解析、查询构造、tag 合并、监视器)需有覆盖。

## 8. 调试与排错

- **日志**:`log.level = "debug"` 输出更详细过程；tagger 子进程日志级别由 `MONBOORU_TAGGER_WORKER_LOG` 独立控制。
- **健康检查**:`curl http://127.0.0.1:8080/health`(用 `monbooru healthcheck` 子命令封装，供 Docker `HEALTHCHECK` 调用)。
- **pprof**:`internal/web` 暴露标准 `/debug/pprof`(开发模式)；线上不要长期开启。
- **文件监视**:macOS/Linux 使用 `fsnotify`；Windows 走轮询回退。`watch_enabled = false` 可临时关掉。
- **DB 锁定**:SQLite 单写者，批量操作并发过高会出现 `database is locked`，降低 `tagger.parallel` 或拆分批次。
- **tagger 调不通**:确认 `CGO_ENABLED=1`、模型/标签 json 路径正确、ONNX Runtime 库(`libonnxruntime.so` 或 `onnxruntime.dll`，或 `ORT_LIB_PATH` 覆盖的路径)可被加载；默认 backend 通过 TCP loopback 派生子进程，日志会写 `MONBOORU_TAGGER_WORKER_LOG`。
- **远程打标**:客户端需在 `[tagger.remote_client]` 配置 URL 与 token；服务端需开启 `[tagger.remote_server] allow_remote` 并给客户端颁发带 `tag` scope 的 token。配对流程见设置页的配对面板。
- **定时任务**:`[schedule]` 节配置每日维护窗口(默认 01:00)，可勾选同步 gallery、清理孤立数据、运行自动打标、查找关系对。`internal/web/scheduler.go` 驱动调度。

## 9. 架构要点(给 Agent 的速查)

- **入口**:`cmd/monbooru/main.go` → `internal/web.NewServer` 构造 HTTP 处理器 + `srv.StartWatchers` 启动 fs 监听；信号优雅退出 10s 内。
- **多 gallery**:`Config.Galleries` 数组，每个派生 `DBPath` 与 `ThumbnailsPath` 写到 `data_path`；切换 gallery 由 URL 中的 `g=` 参数与 cookie 共同决定。
- **搜索**:`internal/search` 自研 DSL → AST → SQL；`executor.go` 负责执行与缓存，`cache.go` 负责结果集缓存键；`fastcount.go` 优化大库的 source/metadata/wildcard 查询性能；`comfyui_jq.go` 基于 `gojq` 对 ComfyUI 工作流 JSON 提供 `jq:` 过滤器，默认 500ms 超时，多个 jq 表达式以 AND 组合；`similarityfilter.go` 实现 `similar:` 按共享标签排序。
- **打标**:`internal/tagger` 通过 TCP loopback IPC 派发到 `tagger-worker` 子进程；父进程只持有 backend 接口；模型配置在 `dispatch_default/*.json`，阈值与每类别 topK 持久化在 TOML；`aggregation.go` 负责多 tagger 结果聚合；`store.go` 的 `storeResults` 负责结果落库(usage_count 同步、rating 剪枝、蕴含 fanout)；`remote_backend.go` 实现远程 HTTP 推理(流式上传、跳过/失败记账)；`noop.go` 在无 tagger 构建下委托远程实例。
- **Public Tag Repository(PTR)**:`internal/web/handlers_ptr_contrib.go` 与 `handlers_ptr_lookup.go` 处理标签贡献与查询，需要 monloader v1.5.0+。
- **标签来源追踪**:`internal/gallery/sources.go` 与 `internal/tags/image_tag_sources.go` 记录每个标签的声明来源；`internal/web/handlers_image_tags.go` 在详情页按来源分组展示。
- **关系查重**:`internal/relations` 提供 pHash 查重、BK-tree、find-pairs 任务；`RelationsConfig` 控制默认距离、会话排序、摄取时增量探测；`queue_prune.go` 清理旧设置产生的失效队列。
- **REST API**:`internal/api` 基于 `net/http` 包装，token 通过 `Authorization: Bearer <secret>`；scope 在 `internal/config` 定义(`read`/`write`/`delete`/`tag`)。
- **OpenAPI**:`internal/api/openapi.go` 维护 schema；改动端点后请同步该文件。
- **会话/CSRF**:`internal/web/auth.go`、`csrf.go`；启用密码后强制使用 secure cookie；非安全环境下勿将 `bind_address` 暴露到公网。
- **Monloader 集成**:`internal/web/pairing.go` 处理配对；`monloader_status.go` 追踪连接状态；`fetch_status.go` 管理源刷新任务。
- **远程打标配对**:`internal/web/remote_tagger_pairing.go` 与 `tagger_remote_handler.go` 实现双向配对协议与 `POST /api/v1/tagger/remote-run` 处理器(Bearer token 认证，`tag` scope)。

## 10. 安全与合规

- **仅限内网/局域网**:`README.md` 与启动横幅已声明；不要在默认配置上对公网开放。
- **路径穿越**:`internal/web` 对用户提供的路径(自定义 CSS/Logo、上传子目录等)做 trusted-root 校验；新增类似配置项时务必复用同一套校验。
- **认证**:`password_hash` 使用 `bcrypt`；token 仅存 SHA-256，明文只在创建时返回一次。
- **CSRF**:所有非 GET 处理器需 CSRF token；表单模板已统一注入 `csrfToken`。
- **SQL 注入**:禁止字符串拼接拼 SQL；占位符 `?` 起于 `database/sql`。
- **HTML/JS 注入**:`html/template` 自动转义；若在 JS 上下文中插入字符串，使用 `template.JS`/`template.JSStr` 并明确来源可信。
- **ONNX 模型**:只接受操作员在 `[paths] model_path` 下放置的模型；不要从网络下载到运行时目录。
- **远程打标**:仅在 `allow_remote` 开启时接受远程请求，且必须校验 `tag` scope 的 Bearer token；打标请求的图片按 12 字节头部嗅探内容类型，损坏文件在读取前即被拒绝。

## 11. 提交与 PR 规范

- **分支**:从 `main` 拉功能分支；`main` 保持可发布。
- **提交信息**:推荐 `类型(范围): 简述`，类型沿用 Conventional Commits(`feat`/`fix`/`refactor`/`docs`/`test`/`build`/`ci`/`chore`)。中文英文均可，但**一句话说「为什么」，而不是「做了什么」**。
- **本地预检**(提交前必跑):
  ```bash
  go mod tidy
  gofmt -l .                  # 应无输出
  go build ./...              # 默认构建
  CGO_ENABLED=1 go build -tags tagger ./cmd/monbooru
  go test -race ./...
  ```
  如果改动触碰 `internal/tagger` 或 `internal/search` 的 jq 路径，追加 `go test -tags tagger ./...`。改动搜索相关代码后可额外跑 `go vet ./internal/search/...` 做静态检查。
- **PR 标题**:`[monbooru] <一句话描述>`，关联 issue(如 `Closes #54`)。
- **PR 描述**:说明动机、影响面、截图(UI 变更)、回滚方案；破坏性变更必须在描述中独立小节标出 `BREAKING`。
- **CHANGELOG**:每次用户可见的功能/修复/移除都在 `CHANGELOG.md` 顶部新增 `## [vx.y.z] - YYYY-MM-DD` 节，按 `Added`/`Changed`/`Fixed`/`Removed` 分类。Release workflow 会自动提取最新条目作为 GitHub Release 正文。
- **版本号**:`VERSION.md` 由维护者在发布时手工改；Agent 不要自行 bump。

## 12. Docker 与发布

- **镜像仓库**:`ghcr.io/monbooru/monbooru`(CPU)与 `ghcr.io/monbooru/monbooru:latest-cuda`(GPU)。
- `docker/Dockerfile` 多阶段构建:`golang:1.25-trixie`(build) → `debian:trixie-slim`(assets:ffmpeg n8.1 + onnxruntime 1.21.0) → `gcr.io/distroless/cc-debian13:nonroot`(runtime)，UID/GID 1000。
- `docker/Dockerfile.cuda` 启用 CUDA 镜像(用 NVIDIA Container Toolkit 部署)。
- `docker-compose.yml` 演示了 monbooru + 可选 monloader 的挂载与端口(`127.0.0.1:8080` / `127.0.0.1:8081`)；包含 monloader 的 PTR 同步卷配置示例。
- `HEALTHCHECK` 用 `monbooru healthcheck` 子命令提供，30s 间隔，10s 启动宽限期。
- GitHub Actions(`.github/workflows/release.yml`):推 `v*` tag → 矩阵构建 CPU+CUDA 镜像并 push 到 GHCR → 从 CHANGELOG 提取最新条目创建 GitHub Release。
- 容器内 `monbooru` 的运行参数来自 `/config/monbooru.toml`；修改配置后需重启容器。
- 环境变量:`MONBOORU_SERVER_BIND_ADDRESS=0.0.0.0:8080`、`MONBOORU_SERVER_BASE_URL`(浏览器访问地址)、可选 `TZ`。

## 13. 修改指引(常见任务)

| 想做的事 | 涉及位置 |
| --- | --- |
| 新增/调整页面 | `web/templates/` + `internal/web/handlers_*.go` + `internal/web/router.go` |
| 新增设置页 | `internal/config/config.go` + `web/templates/settings.html` + 对应 handler |
| 新增 REST 端点 | `internal/api/*.go` + `internal/api/openapi.go` + `internal/api/images_dto.go`(DTO) |
| 改搜索语法 / 通用 DSL | `internal/search/parser.go` + `internal/search/wherebuilder.go` |
| 改 ComfyUI 工作流 jq 搜索 | `internal/search/comfyui_jq.go` + `internal/search/executor.go`(SQL 预过滤) |
| 改 SQL schema | `internal/db/schema.sql` + 增量迁移脚本(若有)+ `internal/db/placeholders.go` |
| 调整 ONNX 模型/标签 | `internal/tagger/catalog_default.json` 或 `internal/tagger/dispatch_default/*.json` |
| 改自动打标流程 | `internal/tagger/`(backend/ipc/aggregation/store/remote_backend) |
| 改远程打标配对 | `internal/web/remote_tagger_pairing.go` + `tagger_remote_handler.go` + `internal/tagger/remote_backend.go` |
| 改文件监视/摄取 | `internal/gallery/watcher.go` + `ingest.go` + `sync.go`(含点前缀隐藏逻辑 `hidden.go`) |
| 改标签来源追踪 | `internal/gallery/sources.go` + `internal/tags/image_tag_sources.go` |

## 14. 平台差异备忘

- `internal/web/stats_linux.go` / `stats_other.go` 按 GOOS 拆分，是新增平台相关代码的范例。
- Windows 走轮询回退而非 `fsnotify`；图库 `folder_path` 在 DB 中始终以 `/` 分隔存储(跨平台可移植)，文件系统操作才用 `filepath` 语义。
- 原生 Windows 上部分 web 测试因路径/chown 语义失败是平台差异；合并前不强制要求本机通过，建议在 Linux 容器/WSL 中复跑。
