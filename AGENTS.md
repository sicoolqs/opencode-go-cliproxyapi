# AGENTS.md

本文件供 AI 编码代理（OpenCode 等）在本仓库工作时遵循。

## 构建与交叉编译原则

本工程是 CLIProxyAPI 的原生动态库插件（`import "C"` + `-buildmode=c-shared`），**任何平台产物都必须 CGO 编译**，且交叉编译目标必须与宿主运行环境的 libc 匹配。

### 基本规则

1. **CGO 必须显式开启**：`CGO_ENABLED=1`（Windows 环境 `go env` 默认为 0，cgo 源文件会被静默排除，导致 `undefined` 等诡异编译错误，连 `go test ./...` 也需要）。
2. **产物格式由目标平台决定**：Windows→`.dll`，Linux→`.so`，macOS→`.dylib`。
3. **构建命令**（本机构建）：
   ```powershell
   $env:CGO_ENABLED = "1"
   go build -buildvcs=false -trimpath -buildmode=c-shared -ldflags "-s -w" -o "dist\opencode-go-cliproxyapi.dll" .
   ```
   生成后删除 `-buildmode=c-shared` 附带的 `.h` 文件。

### Windows 上交叉编译 Linux/macOS 产物：必须用 zig cc

MinGW-w64 只能产出 Windows PE 格式（`.dll`），无法产出 Linux ELF / macOS Mach-O。本机以 zig 作为交叉 C 工具链（`D:\Work\zig\zig.exe`，无需加入 PATH，编译时显式引用）：

```powershell
# Linux x64（glibc 基线 2.17，兼容绝大多数发行版）
$env:CGO_ENABLED = "1"; $env:GOOS = "linux"; $env:GOARCH = "amd64"
$env:CC = "D:\Work\zig\zig.exe cc -target x86_64-linux-gnu.2.17"
go build -buildvcs=false -trimpath -buildmode=c-shared -ldflags "-s -w" -o "dist\opencode-go-cliproxyapi.so" .

# macOS（如需）
$env:CC = "D:\Work\zig\zig.exe cc -target x86_64-macos"    # Intel
$env:CC = "D:\Work\zig\zig.exe cc -target aarch64-macos"   # Apple Silicon
```

### ⚠️ 关键教训：glibc vs musl 必须与宿主镜像匹配

- **默认用 glibc 目标（`x86_64-linux-gnu.2.17`）**：Debian/Ubuntu/CentOS 等主流镜像都是 glibc，运行时只认带版本号的 `libc.so.6`。
- **禁止默认用 musl 目标**：musl 产物依赖无版本号的 `libc.so`，该文件在 glibc 系统上只存在于 `libc6-dev` 开发包，运行时 dlopen 直接报错：
  ```
  dlopen .../opencode-go-cliproxyapi.so: libc.so: cannot open shared object file: No such file or directory
  ```
  这是本项目实际踩过的坑（CPA 官方 Docker 镜像为 glibc）。
- 只有确认宿主是 Alpine（musl 系统）时才用 `-target x86_64-linux-musl`。拿不准时请用户先在容器内执行 `cat /etc/os-release` 确认。

### 产物验证（zig 0.17 的 objdump 是占位实现，不可用）

用字节级检查代替：

```powershell
$bytes = [System.IO.File]::ReadAllBytes("dist\opencode-go-cliproxyapi.so")
$text = [System.Text.Encoding]::ASCII.GetString($bytes)
# ELF 头：$bytes[4]=2 → ELF64；offset 16 (u16)=0x03 → ET_DYN 共享库；offset 18 (u16)=0x3E → x86-64
# libc 依赖：$text.Contains("libc.so.6`0") → glibc 产物正确
# musl 产物特征：精确匹配 "libc.so`0"（无版本号）
# 导出符号：检索 cliproxy_plugin_init / cliproxyPluginCall / cliproxyPluginFree / cliproxyPluginShutdown
```

### 代理环境注意

当前机器的 `HTTP_PROXY`/`HTTPS_PROXY` 环境变量已设置为网关代理（`https://…@ai-gateway.shieh.xin:57890`）。**环境变量已设置代理时，优先使用环境变量的代理，不要清除或覆盖它**：直连 github.com 会被阻断，旧的本地代理 `http://127.0.0.1:7890` 已不再运行。

历史教训仅作参考：早期曾因 Git Credential Manager 报 `ServicePointManager 不支持具有 https 方案的代理` 而需要临时覆盖代理；当前环境下直接沿用现有环境变量推送即可成功（凭证已缓存，GCM 不再触发该问题）。若未来推送再次报该错误，再考虑临时覆盖方案。

## 凭证生命周期约定

OpenCode Go 的 Key 是**静态 Key，没有刷新/续期接口**。因此凭证策略固定为「校验 + 隔离 + 平滑轮换」，**不要伪造 OAuth 续期或向上游发刷新请求**。

- **健康探测**：`GET {usage-url}`，不消耗 token。只有 **401 判定失效**；5xx、403、网络错误、超时一律视为「不确定」，不得禁用凭证。
- **失效隔离**：`auth.parse` / 重配置 / `auth.refresh` 探测到 401 → 对应凭证 `Disabled: true`，日志只输出 `key_fingerprint`（`keyHash` 前几位），**绝不打印 Key 明文**。
- **最后凭证保护**：全部 Key 都探测失败时不禁用最后一个，否则 CPA 会因空池返回 503。
- **凭证身份 = Key 哈希**（`authIDFor` = `opencode-go-key-<sha256>`）。Host 对 `Disabled` 是单向保留的（`preserveAuthDisabledState`），因此「换 Key」必须表现为「新凭证 ID」，不能靠复用旧 ID 复位禁用状态。
- **旧凭证回收**：`materializeAuthRecords` 会把配置中已移除 Key 对应的凭证置为 `disabled`（文件保留，供人工删除），避免过期 Key 继续参与调度。
- **周期体检**：**由插件自驱动**（`startHealthCheckLoop`，每 15 分钟复用 `materializeAuthRecords` 的探测+对账）。CPA 的 auto-refresh 循环**不会调度 API-key 类凭证**（`nextRefreshCheckAt` 在 `refresh_interval_seconds` 分支之前就把 `AuthKindAPIKey` 摘除），所以不要依赖宿主调度做周期检查；`auth.refresh` 处理器只为请求 401 触发的错误驱动刷新和手动刷新服务。`auth.refresh` 的响应必须回填 `StorageJSON` 里的 `disabled`（v8.0.61 起 Host 才有 `preserveAuthDisabledState`，旧宿主会直接采信响应中的 Disabled），只有 401 才能把 Disabled 翻成 true，任何路径都不得把它翻回 false。
- **去重/状态归属**：需要跨 `register` / `reconfigure` 存活的状态（诊断去重、凭证集合）放在插件 `Manager`，不要放进每次重建的 `catalog.Manager`。
- **密钥轮换**：`api-keys-file` 与内联 `api-keys` 合并、按值去重（内联名称优先）；文件按 `api-keys-file-poll`（默认 1m，最小 10s）重读，内容变化才应用，并重启目录刷新循环使新 Key 生效。轮换流程是「先加新 Key → 等生效 → 再删旧 Key」。

## 错误分类与超时语义

- 上游 **401 → 401 且不可重试**（交由 CPA 切换凭证）；不要重试同一个失效 Key。
- **首字节前**的网络错误 → 502，超时 → 504；已开始流式输出后的失败按流内错误处理，不再改写状态码。
- 配置项 `request-timeout` 对**流式请求不是总时长**：它是首字节上限（`min(request-timeout, 60s)`）+ 空闲超时（按 chunk 重置）。长会话不应被总时长截断，改动此逻辑前先看 `stream_timeout_test.go`。
- `executor.count_tokens` 只对 **claude 源**代理到上游 `{messages-url}/count_tokens`；其他源格式返回 `not_implemented`（501），保持凭证/模型可用性中立。

### 宿主可识别的错误码（v8.0.62+）

CPA 的 cooldown/重试流水线只认特定 `pluginabi.Error.Code`。插件在 `errclass` 里本地定义了这些常量（保持 v8.0.50 SDK pin），`ToEnvelopeError` 负责落到 envelope：

| Code | 插件使用点 | 宿主行为 |
|---|---|---|
| `transient_transport` | 首字节前的 EOF/reset/dial/TLS/DNS 失败（502） | 跳过凭证冷却，仍可参与请求重试 |
| `stream_truncated` | 首字节后上游流中断（`error_detail`） | 跳过冷却，且**不换凭证重放**（生成已提交） |
| `request_scoped` | 翻译失败、参数/协议不支持、上游 4xx | 跳过冷却并停止凭证轮换 |
| `connection_lifecycle` | 下游客户端断开/背压导致 emit 失败 | 跳过冷却，但保留凭证轮换 |
| `model_not_found` | 上游 404 | 只冷却该模型，不把凭证冷却 12h |
| `not_implemented` | 非 claude 源的 `count_tokens` | 凭证/模型可用性中立 |
| `credential_quota` | 429/402 且报文含 quota | 整个凭证额度耗尽，进入配额退避 |
| `transient_rate_limit` | 429 且报文为容量过载（overloaded / at capacity / service busy） | 只冷却该模型至 Retry-After，不标记额度耗尽 |

**背景**：未识别的 code 会退回按 HTTP 状态处理——502 冷却 1 分钟、400 冷却 1 分钟、404 冷却 12 小时（`transientErrorCooldown=1m`）。因此新增错误路径时必须显式给出上表 code，否则一次瞬时的上游 EOF 就会把健康凭证冷却掉。流式失败通过 `host.stream.close` 的 `error_detail` 字段携带结构化错误（旧宿主忽略该字段，退回纯 message）。

**流式失败分类的取舍**：`stream_truncated` 同时用于上游读失败、空闲超时和响应超限——这三类都已经提交了生成，重放会重复生成/计费，因此宁可跳过冷却也不换凭证重试。代价是「持续卡顿的上游不会被自动冷却」（健康体检只探测 `/usage`，不覆盖模型级卡顿）；若未来需要冷却，必须显式改用 `force_cooldown`，不要改回状态码路径。

## 冷却策略

插件**不维护冷却状态**；冷却完全由 CPA 宿主按错误码/状态决定（v8.0.66）：

| 上游 | 插件 code | 宿主冷却（模型级） |
|---|---|---|
| 401 | `auth_failure` | 30m + 凭证拒绝（插件体检另会禁用该 Key） |
| 402 | `billing` / `quota_exhaustion` | 30m（忽略 Retry-After） |
| 403 | `auth_failure` / `quota_exhaustion` | 30m |
| 404 | `model_not_found` | 12h（仅该模型） |
| 429 overload | `transient_rate_limit` | 至 Retry-After（短） |
| 429 quota | `credential_quota` | 凭证级配额退避 |
| 5xx | `upstream_server_failure` | 1m |
| 首字节前网络错误 | `transient_transport` | 无 |
| 流中断 | `stream_truncated` | 无 |
| 请求级/翻译 | `request_scoped` | 无 |

- 请求结果带 model key 时为模型级；无 model key 时才落到凭证级。
- 插件无法设置宿主的 `disable-cooling`（那是 CPA provider/全局配置）；只能通过错误码影响冷却。
- **个人部署建议**：在 CPA 配置中设置 `routing.cooldown.disable-cooling: true`，关闭全部 auth/model 冷却，优先保证连通性；`request-retry` 保持 0，避免流式重放。代价是 402/429/5xx 每次都真实等待上游。
- 关闭冷却不会关闭插件的 401 凭证禁用与最后凭证保护；也不会关闭 panic 熔断（后者仅由 panic 触发，已全部 recover）。

## 协议路由约定

- **上游请求头（对齐 OpenCode 官方要求）**：Go 要求客户端用**自定义 User-Agent**（不能是 `Go-http-client/1.1`、`hertz`、`python-requests` 这类通用库/框架名）并发送稳定的 `x-opencode-session`。插件优先转发**有意义的客户端 UA**（如 `claude-cli/2.1.322`、`opencode/1.x`、`my-coding-agent/1.0`）；空值、超长（>200）、含控制字符或命中通用库/框架黑名单时回退为 `opencode-go-cliproxyapi/<version>`。session 优先取 CPA canonical / 客户端原生 session header（Claude Code、Codex 等），缺失时用对话首轮内容摘要，保证多轮稳定。不要恢复为默认 UA——文档明确把通用 UA 视为不合规客户端。
- **三类文本后端**：`chat-completions-url`（默认 `.../inference/go/openai/v1/chat/completions`）、`messages-url`（`.../anthropic/v1/messages`）、`responses-url`（`.../openai/v1/responses`）；`google-url` 仅作 generateContent 回退，`usage-url` 与 `catalog-url` 独立。
- **路由优先级**：`route-overrides`（模型级）> `routing.mode` > 能力校验 > 原生回退。`prefer-client-protocol` 在模型「可服务」客户端协议时直接用客户端协议并透传（`conversion=false`），否则在 `allow-translation` 下回退原生协议并由插件翻译。
- **Console 目录的能力假设**：`SpeaksProtocol` 对 Console 文本模型放行任意文本协议（`routing.go:86-89`），即假定网关跨 API 翻译；旧目录/非文本模型只认 `api` 声明与原生协议。这是已知的宽松点：模型即使只声明 `responses`，Chat Completions 客户端仍会被发到 chat-completions 端点。遇到「协议不匹配类」上游失败（404/405/EOF）时，先用 `route-overrides` 固定到原生协议验证。
- **`route-overrides` 语义**：`protocol` 必填（`chat-completions` / `messages` / `responses`）；`endpoint` 可选，为空时使用该协议的默认端点；配置时按 `base-url` 拼接且沿用旧规则（前导 `/v1` 被去掉）——Console base 下必须写完整路径（如 `/openai/v1/responses`，写成 `/v1/responses` 会得到 `.../inference/go/responses`）。

## 并发与故障隔离

- **单模型失败不应升级为插件级故障**：路由、转换、会话都是请求内状态，模型级错误用 `model_not_found` 等 code 控制冷却范围；不要引入跨模型的可变全局状态。
- **宿主 `fusePlugin` 只由 panic 触发**，作用域是整个插件（所有模型/凭证/管理接口），恢复需重载插件或重启。因此插件不得把 panic 抛回宿主：RPC 边界由 `HandleCall` recover，后台 goroutine 必须用 `recoverBackground` 包住（stream pump / 密钥文件轮询 / 周期体检 / 目录刷新）。
- Go 中任意 goroutine 的未捕获 panic 会终止整个进程（即 CPA 宿主），影响面是全部模型；**新增后台 goroutine 时必须带 recover**。

## 日志与诊断约定

- **原因必须写进 message**：CPA 默认日志格式不展示 `host.log` 的 fields，只把 fields 当结构化补充；若原因只在 fields 里，线上等同于丢失。
- 注册/重配置的诊断要**逐条分级**（info/warn/error）并做**变更抑制**：状态未变不重复输出。去重状态放 `Manager`，否则每次重配置会重复刷屏。
- 任何涉及凭证的日志只允许出现指纹，不要出现 Key、Authorization、x-api-key 原文。

## 测试注意事项

- `fakeCaller` 会自动持久化 `AuthSave` 并在 `AuthList` 返回（没有 Provider/Path）。凭证探测会给每个 Key 额外产生一次 `host.http.do`，断言调用次数时必须**按 URL 过滤**，否则计数会漂移。
- 测试插件内部逻辑时不要导入 `sdk/translator`：它会把 gin/logrus 等依赖拖进共享库；插件内同类常量请用本地定义。
- 修改流式/超时/凭证逻辑后至少跑：`go test -tags debug ./...` 与 `go test -race ./internal/plugin/`（`CGO_ENABLED=1`）。

## CPA 版本适配与升级检查清单

本插件基于 CLIProxyAPI v8 插件体系（审计基线：SDK `github.com/router-for-me/CLIProxyAPI/v8 v8.0.50`，`ABIVersion=1`、`SchemaVersion=10`；上一基线为 `main@2422c6b`/v8.0.32）。**2026-11-11 已对照最新 release v8.0.66 重跑审计：v8.0.63→v8.0.66 无插件面变更——`sdk/pluginabi/types.go` 完全一致（ABI 1 / Schema 10 / 65 个方法），`sdk/pluginapi`、`internal/pluginhost`、`server_management.go` 均未变；唯一改动的 `sdk/cliproxy/auth/conductor_cooldown.go` 只涉及 Claude cooldown signal 持久化与 quota 窗口保留，错误码识别逻辑（`transient_rate_limit` 等）未变。v8.0.66 实机冒烟通过（models/chat/messages/responses/muse 全 200）。** 此前的关键适配仍有效：v8.0.62 起宿主才通过 `rpcError.ErrorCode()` 传播插件错误码（v8.0.56 会退化为状态码冷却），**生产宿主必须 ≥ v8.0.62，建议 v8.0.66**。适配新版 CPA 时按本清单重跑合规审计。

### 关于 `/v0/management` 的「废弃」注记（不要误读）

CPA 仓库 `AGENTS.md` 声称 `/v0/management` 已废弃——那**仅针对内核自身管理端点的维护策略**（v8 新功能只进 `/v8/management`，v0 冻结只修编译错误），不是移除。官方文档 `docs/management-api-v8.md` 明确 "The existing `/v0/management` API remains supported"。

而**插件 Management API** 虽挂在同一前缀下，但属于 `internal/pluginhost` 子系统，是活跃维护的：

- `pluginhost/management.go` 的 `managementBasePath = "/v0/management"` 仍是插件路由注册的现行常量
- v8 的插件商店、插件配置（`/plugins/:id/config`）、插件额度（`/plugins/:id/quota`）端点全部挂在 `/v0/management` 组下
- 官方示例 `examples/plugin/management-api` 即此模式

**结论：插件路由继续挂 `/v0/management` 是正确且安全的。** 若未来 CPA 官方迁移插件路由前缀，本插件只需改两处：`main.go` 的 `matchPath` 前缀列表 + `resources/index.html` 里面板 `fetch` 的请求路径字符串。

### 升级新版 CPA 时的审计项

1. `sdk/pluginabi/types.go`：核对 `ABIVersion`（当前 1）与 `SchemaVersion`（当前 10）是否变化；若变化需同步 `main.go` 的 `cliproxy_plugin_init` 与 `registrationPayload`
2. `internal/pluginhost/loader_*.go`：核对 C ABI 结构体与 `cliproxy_plugin_init` 符号名是否变化
3. `internal/pluginhost/rpc_schema.go`：核对方法名（`plugin.register` / `management.register` / `management.handle` / `host.http.*`）与 JSON 字段名（rpc 外壳小写 tag、pluginapi 结构体大写无 tag）是否变化
4. `internal/api/server_management.go`：确认插件路由的挂载与鉴权方式是否迁移
5. 关注新增能力是否值得采用：如 `quota_provider`（注意：其凭据模型绑定 CPA auth 体系，本插件的配置式 API Key 模型不匹配，宿主不会为其发起 `quota.fetch`，暂不采用——详见 2026-10 合规审计记录）

### 已知行为差异（勿重复排查）

- **API-key 类凭证不会被 auto-refresh 调度**（v8.0.50 与 v8.0.61 均如此）：`sdk/cliproxy/auth/auto_refresh_loop.go` 的 `nextRefreshCheckAt` 在 `authPreferredInterval`（`refresh_interval_seconds` 的消费点）之前就返回 false 摘除 `AuthKindAPIKey`，`handleDueAuth` 对 `!shouldSchedule` 直接 `remove` 不执行刷新。因此带 `api_key` 属性的凭证永远不会收到周期性 `auth.refresh`——周期性健康检查必须由插件自驱动。`auth.refresh` 仍会在请求 401 的错误驱动刷新与管理界面手动刷新时到达。
- `preserveAuthDisabledState`（refresh 响应的单向 Disabled 保留）是 **v8.0.61 新增**：v8.0.50 的 pluginhost `RefreshAuth` 会直接采信插件响应中的 `Disabled`，健康刷新响应会把已禁用凭证洗白。插件侧已通过回填 `StorageJSON.disabled` 规避，但升级审计时注意这一差异。

- `host.http.do` 的请求体**没有 timeout 字段**，`timeout_ms` 会被宿主静默忽略。真实超时依赖 `host.http.operation_open` + 请求携带 `operation_id` + 超时后 `host.http.cancel`（已在 `hostbridge.go` 实现，旧版宿主自动降级为不可取消请求）
- `plugin.quiesce` 已显式处理（返回空成功信封）。v8.0.61 起宿主为 quiesce 引入 needsRecovery 状态机（`callQuiesceWithOutcome`）：quiesce 失败/不支持的插件在热替换回滚时会被标记需要恢复；由于本插件正确响应成功信封，行为不受影响，但若未来移除该处理器需重新验证热替换流程。
