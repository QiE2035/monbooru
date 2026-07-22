# AGENTS.md

本文件面向在本仓库工作的 AI 编码 Agent。它汇总项目结构、构建/测试命令、目录约定和编码风格,作为高效协作的入口。面向人类使用者的信息请阅读 [README.md](README.md);面向用户的功能说明请阅读 [DOC.md](DOC.md);面向用户的版本变更请阅读 [CHANGELOG.md](CHANGELOG.md)。

> **当前活跃分支**:`i18n-new`(已完成全界面多语言、热切换与硬编码文本清理,仍在迭代翻译键命名)。合并前请确认 `internal/i18n/` 与 `[i18n] language` 配置在主干上已经发布。

## 1. 项目概述

Monbooru 是一个**自托管、离线、轻量的图库(booru)管理程序**。它用单文件二进制对外提供 Web UI(基于 `net/http` 与 `htmx`),将本地媒体(图片、视频、动图、CBZ/ZIP)组织为带标签、相册、关系、SD/ComfyUI 元数据的私人图库。默认完全离线;在线抓取能力由可选的 [monloader](https://github.com/leqwin/monloader) 配套应用承担。

- **语言/工具链**: Go `1.25.10`,`CGO_ENABLED=1` 才能启用 `tagger` 构建标签。
- **数据存储**: 每个 gallery 一个 SQLite 文件(`modernc.org/sqlite`,纯 Go 实现,无需 CGO)。
- **前端**: 服务端渲染 + htmx 局部刷新;无 npm/Node 构建,静态资源总大小约 300 KB,通过 `//go:embed` 直接打包进二进制。
- **国际化**: 基于 `github.com/nicksnyder/go-i18n/v2`(`golang.org/x/text/language` 解析 BCP-47 标签),翻译文件 `internal/i18n/locales/*.toml` 通过 `//go:embed` 嵌入,模板中以 `{{ T "key" }}` 渲染;Settings → Language 支持运行时热切换,无需重启。
- **AI 推理**: 默认关闭。通过 `tagger` 构建标签编译出 ONNX Runtime worker,常驻独立子进程(`monbooru tagger-worker`),父进程通过 Unix domain socket 与之通信。
- **核心能力**: 标签/分类、相册/收藏、Stable Diffusion 与 ComfyUI 元数据解析、perceptual hash(pHash)与 BK-tree 查重、可撤销的批量操作、文件监听(macOS/Linux 平台,Windows 用轮询回退)、多 gallery 实例、scoped Bearer Token 的 REST API、OpenAPI 规范。

## 2. 仓库结构

```
monbooru/
├── cmd/monbooru/         # 主二进制入口;包含 main.go、worker.go(tagger 子进程)、worker_stub.go(无 tagger 时的占位)、healthcheck.go(Docker 健康检查)
├── internal/
│   ├── api/              # REST API(OpenAPI、scoped bearer token)
│   ├── config/           # monbooru.toml 解析与校验
│   ├── db/               # SQLite 连接池、schema.sql
│   ├── gallery/          # 图库核心:摄取、缩略图、归档(manga/CBZ)、监视器、删除、移动
│   ├── i18n/             # 国际化:go-i18n Bundle + Localizer;locales/ 嵌入 .toml
│   ├── jobs/             # 通用后台任务状态
│   ├── logx/             # 简易结构化日志(slog 封装)
│   ├── metadata/         # PNG/JPG/WebP/ComfyUI/A1111 元数据解析
│   ├── models/           # 跨包共享的数据结构
│   ├── relations/        # pHash 查重、BK-tree、find-pairs 任务
│   ├── search/           # 自研搜索解析器与 SQL 生成器
│   ├── searchkw/         # 关键字辅助索引
│   ├── tagger/           # ONNX 自动打标(catalog/profile/dispatch/inproc/ipc)
│   ├── tags/             # 标签、分类、别名、蕴含、合并
│   └── web/              # HTTP 处理器、路由、模板、认证、CSRF、会话;compatibility 子包提供 Hydrus/Blombooru 导入
├── web/
│   ├── embed.go          # //go:embed static templates → 打进二进制
│   ├── static/           # main.css、main.js、htmx.min.js、logo、favicon
│   └── templates/        # Go html/template;partials/ 是共享片段
├── docker/               # Dockerfile、Dockerfile.cuda、docker-compose.yml、monbooru.container
├── .github/workflows/    # release.yml:打 tag 时构建 CPU/CUDA 镜像并发布 GitHub Release
├── Makefile              # 顶层构建/测试/lint/覆盖度入口
├── go.mod / go.sum       # 依赖(BurntSushi/toml、fsnotify、nicksnyder/go-i18n、onnxruntime_go、x/crypto、x/image、x/text、modernc.org/sqlite)
├── .golangci.yml         # 仅启用 gofmt
├── VERSION.md            # 当前版本号(Makefile 注入 -ldflags)
├── REPOSITORY.md         # 仓库 URL(同上)
├── DOC.md                # 文档站点 URL(同上)
├── README.md
├── CHANGELOG.md          # 按 ## [vx.y.z] - 日期 排序,顶部 ## [unreleased] 段记录未发布变更
└── LICENSE
```

`internal/i18n/locales/` 当前内置 `en.toml`、`zh-CN.toml`、`zh-TW.toml`。新增语言只需在该目录加一个 `xx-YY.toml`,Settings 页面会自动列出并热切换(若该文件随二进制一起编译进来)。

## 3. 环境与依赖

- 基础要求:**Go 1.25.10+**、`git`。
- 启用 `tagger` 标签:**CGO 必须开启**(`CGO_ENABLED=1`),并能链接 ONNX Runtime 共享库 `libonnxruntime.so`(Linux 容器已内置,Windows/macOS 需自行安装)。
- 可选外部工具:**ffmpeg/ffprobe**(视频缩略与动图预览);Dockerfile 会从 BtbN 静态构建下载。
- Windows 上部分测试在容器外可能因为路径/权限差异失败(见 §7)。

> **重要**: 修改 `go.mod` 时不要手动编辑 `go.sum`;`go mod tidy` 必须成功。提交前 `go mod tidy` 并检查无意外新增 indirect 依赖。

## 4. 常用命令

下列命令都在仓库根目录执行。Makefile 已封装常见组合;直接调 `go` 也行。

| 用途 | 命令 |
| --- | --- |
| 构建主二进制(无 tagger) | `make build` 或 `go build ./cmd/monbooru` |
| 构建含 ONNX 推理 | `make build-tagger` 或 `CGO_ENABLED=1 go build -tags tagger ./cmd/monbooru` |
| 运行 | `./monbooru -config ./monbooru.toml` |
| 生成密码哈希 | `./monbooru -hash-password 'yourPassword'` |
| 健康检查(供 Docker HEALTHCHECK) | `./monbooru healthcheck -config ./monbooru.toml` |
| 全部测试(含 race) | `make test` 或 `go test -race ./...` |
| 仅打标器相关测试 | `make test-tagger` 或 `go test -tags tagger ./...` |
| 仅 i18n 包测试 | `go test ./internal/i18n/...` |
| 静态检查 | `make lint` |
| 覆盖率(排除 cmd 与 tagger) | `make coverage` |
| 格式化 | `gofmt -w .` |
| 整理依赖 | `go mod tidy` |

构建产物文件名(Windows)为 `monbooru.exe`,类 Unix 平台为 `monbooru`。`make build` 会把 `VERSION.md`、`REPOSITORY.md`、`DOC.md` 中的字符串通过 `-ldflags -X` 注入到 `internal/web` 的导出变量上(Version/RepoURL/DocURL),务必保证这三个文件存在且无前后空白。

## 5. 配置文件 `monbooru.toml`

`internal/config/config.go` 是唯一可信源。常见字段(节选):

```toml
default_gallery = "main"

[server]
bind_address = "127.0.0.1:8080"   # 也可被环境变量 MONBOORU_SERVER_BIND_ADDRESS 覆盖
base_url     = "http://localhost:8080"
name         = "Monbooru"          # 出现在 <title> 与顶栏;空时回退 "Monbooru"
logo         = ""                  # 可选:指向 favicon/顶栏 logo 的绝对路径
custom_css   = ""                  # 可选:在 main.css 之后加载,便于 :root 覆盖
monloader_url = ""                 # 浏览器访问 monloader 的地址

[monloader]
api_url    = ""                    # server 端访问 monloader 的 URL,留空则从配对来源自动识别
api_token  = ""                    # 配对后由 monloader 颁发

[paths]
data_path  = "./data"              # 派生:每 gallery 的 *.db 与 thumbs 目录
model_path = "./models"            # ONNX 模型与标签 json 目录(只读)

[gallery]
watch_enabled       = true
max_file_size_mb    = 2048
default_upload_folder = ""

[[galleries]]
name         = "main"
gallery_path = "./gallery"

[tagger]
use_cuda                 = false
parallel                 = 1
idle_release_after_minutes = 15

[auth]
enable_password          = false
password_hash            = ""        # ./monbooru -hash-password 生成
session_lifetime_days    = 30

[i18n]
language = "en"                     # BCP-47,如 en / en-US / zh-CN / zh-TW / ja-JP;留空启动失败

[log]
level = "info"                     # debug / info / warn / error
```

环境变量优先级高于 TOML,常用项:`MONBOORU_SERVER_BIND_ADDRESS`、`MONBOORU_I18N_LANGUAGE`、`MONLOADER_SERVER_BASE_URL`、`TZ`(影响时间戳展示,存储仍是 UTC)、`MONBOORU_TAGGER_WORKER_LOG`、`MONBOORU_TAGGER_BACKEND`。

**i18n 节校验**:`[i18n] language` 不能为空,且必须能在 `internal/i18n/locales/` 中找到对应的 `<lang>.toml`(在 `MustInit` 时还会再用 `golang.org/x/text/language.Parse` 二次校验)。Settings → Language 的下拉内容直接来自 `i18n.AvailableLocales()`,写回 TOML 后调用 `i18n.SetLanguage` 热切换,不需要重启。

## 6. 代码风格与约定

- **格式化**:`gofmt` 必须零差异提交;`.golangci.yml` 目前只启用 `gofmt`。
- **导入**:标准库 → 第三方 → 本仓库;组之间空行,Go 1.25 风格(可使用 `slices`、`maps`、`cmp`)。
- **命名**:
  - 包名短小全小写,无下划线、无连字符;目录名即包名。
  - 导出符号需有 godoc 注释,**首字母小写、句号结尾**(golangci-lint 默认 revive 规则一致)。`internal/*` 中的私有函数也鼓励注释解释"为什么"而不是"做什么"。
  - 接收器名短、类型首字母小写、保持一致(`s *Server`、`cfg *Config`)。
- **错误处理**:
  - 优先 `fmt.Errorf("...: %w", err)` 包装;在包边界用 `errors.Is`/`errors.As` 判断。
  - 永远不要 `log.Fatal` 在库代码内——只允许在 `cmd/monbooru` 顶层 `main` 使用。
  - 不吞 `error`;需要时显式 `_ = err` 并在旁注释理由。
- **并发**:共享状态使用 `sync.Mutex`/`RWMutex`;goroutine 派发前想清楚生命周期,长任务接受 `context.Context`。`internal/i18n` 的 `Localizer` 即典型例子:`sync.RWMutex` 保护指针,读路径加 RLock、写路径加 Lock,模板渲染不会见到撕裂值。
- **SQL**:`?` 参数化占位符,不要字符串拼接;所有 SQL 集中在 `internal/db` 与各包的 `queries.go` 中。
- **日志**:使用 `internal/logx`(`slog` 封装);不要在库内直接 `log.Println`。
- **前端**:`web/templates` 用 `html/template`,所有动态值经 `template.JSEscape` 或自动转义;**不要**引入 JS 框架或构建步骤,扩展用 htmx。
- **i18n 用法**:
  - **模板渲染**:模板里通过 `{{ T "key" }}` 取翻译;占位符用 `{{ T "key" (dict "name" .User) }}`(go-i18n 要求 `TemplateData` 是 map 或 struct,裸标量会被丢弃并打 debug 日志)。
  - **Go 代码**:处理器/服务中的提示、错误信息使用 `internal/web` 包内的 `localize("key")` 或 `localize("key", map[string]any{"count": n})`;它封装了 `i18n.Localizer().MustLocalize`,未命中键会 panic,因此新增键必须同步入 `en.toml`。
  - **缺失键行为**:模板函数 `T` 在主语言 + English 回退都未命中时返回 key 原文,方便发现漏译;`localize` 使用 `MustLocalize`,缺 key 会直接 panic,所以 Go 代码路径更严格。
  - **同步所有语言文件**:新增键时**同步**改 `internal/i18n/locales/en.toml` 和其它要支持的语言文件;英文是回退根,所有键至少要进 `en.toml`。
  - **TOML 键名避坑**:翻译键名同时是 TOML 键,避免使用 TOML 保留字(如 `hash`、`id`)。若业务关键词与保留字冲突,在代码层做映射(参考 `internal/web/handlers_suggest.go` 的 `translateKeyName`: `hash` → `sha256_hash`、`id` → `image_id`),而不是在 toml 里硬写保留字。
  - **长文本/带链接文本拆分**:一句完整提示中若需插入 `<a>` 等 HTML 链接或可变组件,可拆成前缀、中段、后缀多个 key(如 `pairing_hint_prefix` / `pairing_hint_link` / `pairing_hint_suffix`),在模板/代码中拼接,避免把 HTML 写进翻译值。
  - **HTML `<html lang="...">`**:由 `layout.html` 根据当前 localizer 自动渲染;手动改 layout 时别覆盖。
- **嵌入资源**:`web/embed.go` 列出要打包的目录;`internal/i18n/locales.go` 用 `//go:embed locales/*.toml`。新增静态文件或翻译文件后需确认 `//go:embed` 通配仍覆盖到。
- **构建标签**:
  - `//go:build tagger` 用于 ONNX 推理相关代码。
  - `//go:build !tagger` 提供 stub,保证默认构建依然成功。
  - `internal/web/stats_linux.go` / `stats_other.go`、`internal/tagger/trim_linux.go` / `trim_other.go` 是按 GOOS 拆分的范例,新增平台相关代码时遵循同样模式。

## 7. 测试约定

- 标准库 `testing`,多数场景使用表驱动(table-driven)子测试;断言用 `t.Errorf`/`t.Fatalf`,避免外部断言库。
- `go test -race ./...` 必须在本地通过再提交。**任何共享状态的并发路径都要求 race 通过**。
- 测试文件命名:`xxx_test.go`,与被测文件同包同目录;白盒测试用包内测试,跨包边界用 `_test` 后缀包。
- 涉及 FS/SQL/网络的测试使用 `t.TempDir()`、`internal/db` 的内存或临时库,不要污染真实图库目录。
- **i18n 测试**:`internal/i18n` 有 `bundle_test.go`(覆盖 `MustInit` / `SetLanguage` 成功、失败、并发)、`dump_test.go`(列出所有键给开发期自检)、`testhelper_test.go`(共享测试夹具)。新增翻译键后,跑 `go test ./internal/i18n/...` 确认无 key 漂移。
- **Windows 路径已知问题**:`internal/web` 下的 `TestBooruLogo_PathTrustedRootGate`、`TestSessionExpiry`、以及 `TestUploadPost_*Subfolder`/`TestUploadPost_Traversal` 涉及 OS 路径/chown 语义,在原生 Windows 上偶尔失败。这些是平台差异而非代码缺陷,建议在 Linux 容器或 WSL 中复跑,合并前不强制要求本机通过(详见 §14)。
- 覆盖率:`make coverage` 排除 `cmd/` 与 `internal/tagger`(后者依赖外部 ONNX 模型);重要功能点(搜索解析、查询构造、tag 合并、监视器、i18n 热切换)需有覆盖。

## 8. 调试与排错

- **日志**:`log.level = "debug"` 输出更详细过程;tagger 子进程日志级别由 `MONBOORU_TAGGER_WORKER_LOG` 独立控制。
- **健康检查**:`curl http://127.0.0.1:8080/health`(由 `monbooru healthcheck` 子命令封装,供 Docker `HEALTHCHECK` 调用)。
- **pprof**:`internal/web` 暴露标准 `/debug/pprof`(开发模式);线上不要长期开启。
- **文件监视**:macOS/Linux 使用 `fsnotify`;Windows 走轮询回退。`watch_enabled = false` 可临时关掉。
- **DB 锁定**:SQLite 单写者;批量操作并发过高会出现 `database is locked`,降低 `tagger.parallel` 或拆分批次。
- **tagger 调不通**:确认 `CGO_ENABLED=1`、模型/标签 json 路径正确、`libonnxruntime.so` 可被加载;默认 backend 通过 Unix socket 派生子进程,日志会写 `MONBOORU_TAGGER_WORKER_LOG`。
- **i18n**:
  - 启动失败 `i18n: language "xx" is not available` → `[i18n] language` 在 `internal/i18n/locales/` 没有同名 `.toml`;要么改配置,要么补文件后重新编译。
  - 启动失败 `i18n: invalid language "xx"` → BCP-47 解析失败(常见:写成 `zh_CN` 用下划线、漏了大小写)。Use `-` 分隔,如 `zh-CN`。
  - 页面出现裸 key(如 `{{ T "..." }}` 直接渲染)→ 该 key 在主语言和 `en.toml` 都未定义;补翻译后无需重启,`SetLanguage` 下次切换会顺带覆盖。
  - 请求 panic `message "..." not found` → Go 代码通过 `localize()` 调用 `MustLocalize`,该 key 在 `en.toml` 里不存在;补 key 后重新编译(因为 toml 是 embed 的)。
  - 热切换没生效 → 检查 Settings POST 是否带了 CSRF token;`SetLanguage` 写入的是进程级 Localizer,新请求立即可见。
  - `MONBOORU_I18N_LANGUAGE` 环境变量会覆盖 TOML,但**不会**触发 `SetLanguage`——它只在 `MustInit` 时生效,所以热切换仍要走 Settings 页面。

## 9. 架构要点(给 Agent 的速查)

- **入口**:`cmd/monbooru/main.go` → `config.Load` → `internal/i18n.MustInit` → `internal/web.NewServer` 构造 HTTP 处理器;`srv.StartWatchers` 启动 fs 监听;信号优雅退出 10s 内。
- **多 gallery**:`Config.Galleries` 数组,每个派生 `DBPath` 与 `ThumbnailsPath` 写到 `data_path`;切换 gallery 由 URL 中的 `g=` 参数与 cookie 共同决定。
- **i18n**:`internal/i18n` 持有 `*i18n.Bundle`(进程级、只读,`MustInit` 时一次性加载)与 `*i18n.Localizer`(可热替换)。`web/templatefuncs.go` 注册 `T` 模板函数,每次渲染从 `i18n.Localizer()` 取当前 localizer。`Settings → Language` 通过 `i18n.SetLanguage` 替换 Localizer 指针,所有后续渲染即用新语言。
- **搜索**:`internal/search` 自研 DSL → AST → SQL;`executor.go` 负责执行与缓存,`cache.go` 负责结果集缓存键。
- **打标**:`internal/tagger` 通过 IPC 派发到 `tagger-worker` 子进程,父进程只持有 backend 接口;模型配置在 `dispatch_default/*.json`,阈值与每类别 topK 持久化在 TOML。
- **REST API**:`internal/api` 基于 `net/http` 包装,token 通过 `Authorization: Bearer <secret>`;scope 在 `internal/config` 定义(`read`/`write`/`delete`)。
- **OpenAPI**:`internal/api/openapi.go` 维护 schema;改动端点后请同步该文件。
- **会话/CSRF**:`internal/web/auth.go`、`csrf.go`;启用密码后强制使用 secure cookie;非安全环境下勿将 `bind_address` 暴露到公网。

## 10. 安全与合规

- **仅限内网/局域网**:`README.md` 与启动横幅已声明;不要在默认配置上对公网开放。
- **路径穿越**:`internal/web` 对用户提供的路径(自定义 CSS/Logo、上传子目录等)做 trusted-root 校验;新增类似配置项时务必复用同一套校验。
- **认证**:`password_hash` 使用 `bcrypt`;token 仅存 SHA-256,明文只在创建时返回一次。
- **CSRF**:所有非 GET 处理器需 CSRF token;表单模板已统一注入 `csrfToken`,Settings → Language 的 POST 也走同一通道。
- **SQL 注入**:禁止字符串拼接拼 SQL;占位符 `?` 走 `database/sql`。
- **HTML/JS 注入**:`html/template` 自动转义;若在 JS 上下文中插入字符串,使用 `template.JS`/`template.JSStr` 并明确来源可信。
- **翻译资源**:翻译文件经 `//go:embed` 打进二进制,运行时不再读盘,不存在外部输入导致的注入路径;但 `T` 的占位符(`TemplateData`)会传给 go-i18n 的模板引擎,**不要把用户原始输入直接当占位符**,需要走 `html/template` 自动转义。
- **ONNX 模型**:只接受操作员在 `[paths] model_path` 下放置的模型;不要从网络下载到运行时目录。

## 11. 提交与 PR 规范

- **分支**:从 `main` 拉功能分支;`main` 保持可发布。当前活跃特性分支 `i18n-new` 合并前需先 rebase 主干。
- **提交信息**:推荐 `类型(范围): 简述`,类型沿用 Conventional Commits(`feat`/`fix`/`refactor`/`docs`/`test`/`build`/`ci`/`chore`)。中文/英文均可,但**一句话写"为什么",而不是"做了什么"**。
- **本地预检**(提交前必跑):
  ```bash
  go mod tidy
  gofmt -l .                  # 应无输出
  go build ./...              # 默认构建
  CGO_ENABLED=1 go build -tags tagger ./cmd/monbooru
  go test -race ./...
  go test ./internal/i18n/... # 翻译键一致性
  ```
  如果改动触碰 `internal/tagger`,追加 `go test -tags tagger ./...`。
- **PR 标题**:`[monbooru] <一句话描述>`,关联 issue(如 `Closes #54`)。
- **PR 描述**:说明动机、影响面、截图(UI 变更)、回滚方案;破坏性变更必须在描述中独立小节标出 BREAKING。
- **CHANGELOG**:每次用户可见的功能/修复/移除都在 `CHANGELOG.md` 顶部新增 `## [unreleased]` 段落下的 `Added`/`Changed`/`Fixed`/`Removed` 分类(发布时由维护者把 `[unreleased]` 改为 `## [vx.y.z] - YYYY-MM-DD`)。Release workflow 会自动提取最新已发布条目作为 GitHub Release 正文。
- **i18n PR**:每次新增/修改用户可见字符串时,必须同步所有要支持的语言文件(`internal/i18n/locales/*.toml`);漏译至少要保证 `en.toml` 有 key,运行时自动回退。模板里只改 key 名称时记得跑 `go test ./internal/i18n/...` 检查键名一致性。
- **版本号**:`VERSION.md` 由维护者在发布时手工改;Agent 不要自行 bump。

## 12. Docker 与发布

- `docker/Dockerfile` 多阶段构建:`golang:1.25-trixie`(build) → `debian:trixie-slim`(assets:ffmpeg + onnxruntime) → `gcr.io/distroless/cc-debian13:nonroot`(runtime),UID/GID 1000。
- `docker/Dockerfile.cuda` 启用 CUDA 镜像(用 NVIDIA Container Toolkit 部署)。
- `docker-compose.yml` 演示了 monbooru + 可选 monloader 的挂载与端口(`127.0.0.1:8080` / `127.0.0.1:8081`)。
- `HEALTHCHECK` 由 `monbooru healthcheck` 子命令提供,30s 间隔。
- GitHub Actions(`.github/workflows/release.yml`):推送 `v*` tag → 矩阵构建 CPU+CUDA 镜像并 push 到 GHCR → 从 CHANGELOG 提取最新条目创建 GitHub Release。
- 容器内 `monbooru` 的运行参数来自 `/config/monbooru.toml`;修改配置后需重启容器。

## 13. 修改指引(常见任务)

| 想做的事 | 涉及位置 |
| --- | --- |
| 新增/调整页面 | `web/templates/` + `internal/web/handlers_*.go` + `internal/web/router.go` |
| 新增设置项 | `internal/config/config.go` + `web/templates/settings.html` + 对应 handler |
| 新增/修改用户可见字符串(翻译) | `web/templates/*.html`(用 `{{ T "key" }}`)+ `internal/i18n/locales/*.toml` 同步;新增语言加 `internal/i18n/locales/<lang>.toml` |
| 新增 REST 端点 | `internal/api/*.go` + `internal/api/openapi.go` |
| 改搜索语法 | `internal/search/parser.go` + `internal/search/wherebuilder.go` + 测试 |
| 改 SQL schema | `internal/db/schema.sql` + 增量迁移脚本(若有)+ `internal/db/placeholders.go` |
| 调整 ONNX 模型/标签 | `internal/tagger/catalog_default.json` 或 `internal/tagger/dispatch_default/*.json` |
| 增加新的自动打标 | `internal/tagger/backend.go` 增加实现,默认 inproc/IPC 二选一 |
| 改导入兼容层 | `internal/web/compatibility/` 子包,每个 booru 独立文件 |
| 调整 i18n 行为(回退、错误处理) | `internal/i18n/bundle.go`(`MustInit` / `SetLanguage` / `AvailableLocales`) + 测试 `bundle_test.go` |
| 增加新平台专用代码 | 用 `*_linux.go` / `*_other.go` 后缀拆分,例:`stats_linux.go` |

## 14. 已知限制

- **Windows 平台差异**:少量 `internal/web` 测试(见 §7)在 Windows 原生环境可能因为路径分隔符、chown 行为、socket 行为差异失败;推荐在 Linux 容器/CI 中复跑。
- **ONNX 依赖**:`tagger` 构建需要本地有可用的 `onnxruntime` 共享库;macOS/Windows 自行安装。
- **SQLite 写并发**:单写者模型,高并发批量写入需在调用方分批。
- **i18n Bundle 静态性**:Bundle(已加载的翻译文件)在 `MustInit` 之后不再变。新增/修改 `locales/*.toml` 必须重新编译二进制;Settings 页面下拉只能列出编译时已嵌入的语言。
- **公网暴露风险**:本项目未做抗 CSRF 之外的抗滥用设计(无速率限制、no fail2ban),请保持内网使用。

## 15. Agent 自检清单(PR 前)

- [ ] `gofmt -l .` 无输出
- [ ] `go build ./...` 通过
- [ ] `CGO_ENABLED=1 go build -tags tagger ./cmd/monbooru` 通过(如改动触及打标路径)
- [ ] `go test -race ./...` 通过(Windows 已知失败测试见 §7)
- [ ] `go test ./internal/i18n/...` 通过(任何翻译或模板键变动都跑)
- [ ] `go mod tidy` 无新增未声明依赖
- [ ] 用户可见字符串已加到 `internal/i18n/locales/*.toml`(至少 `en.toml`)
- [ ] 公开 API/配置变更同步更新 `CHANGELOG.md`(顶部 `[unreleased]`)与 `internal/api/openapi.go`
- [ ] 大改动在 PR 描述中标出影响面与回滚方案

---

按上述约定提交的内容,人工评审可以快速理解其影响面并放心合入。遇到歧义优先参照 `internal/` 中已存在的同类实现,而非外部惯例。
