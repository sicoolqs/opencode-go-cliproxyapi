# OpenCode Go CLIProxyAPI 插件

一个原生动态 Go 插件（适用于 [CLIProxyAPI](https://help.router-for.me/plugin/development)），将 OpenCode Go 以单一服务商（`opencode-go`）的形式暴露出来。

该插件统一了 OpenCode Go 上游各端点的模型发现、协议转换与执行，同时复用 CLIProxyAPI 内建的认证、调度、密钥轮换与冷却管理。

## 问题背景

OpenCode Go 的模型分布在多种 API 协议上（OpenAI Chat Completions、Anthropic Messages、OpenAI Responses，以及 Google generateContent）。Console 网关允许文本模型跨 API 调用（由服务端翻译），但每个模型仍有一个目录声明的原生协议。

如果没有本插件，在 CLIProxyAPI 中使用 OpenCode Go 需要为每种协议分别配置 Provider，会带来以下问题：
- **配置与密钥重复**：同一批 API 密钥必须在多个 Provider 配置块中重复填写。
- **调度与轮换割裂**：密钥轮换、限流与冷却无法跨协议共享——一种协议上配额耗尽时无法与其他协议协同。
- **客户端协议负担重**：客户端必须事先知道每个模型需要走哪个上游协议和端点。
- **模型目录割裂**：模型被分散在互不相干的 Provider 命名空间下，没有统一的模型列表。

## 解决方案

本插件将 OpenCode Go 以单一服务商（`opencode-go`）暴露，并由共享密钥池支撑：
- **统一认证池**：只需配置一次密钥；CLIProxyAPI 会在所有协议间统一调度、轮换与冷却。
- **透明的协议转换与路由**：客户端只需请求模型（如 `opencode-go/glm-5.2`、`opencode-go/gpt-5.6-luna`），无需关心上游协议格式。
- **统一模型目录**：所有模型都会在 `/v1/models` 下以 `opencode-go` 服务商命名空间被发现和发布。

## 功能特性

- **单一服务商命名空间**：模型统一带 `opencode-go` 前缀暴露（如 `opencode-go/glm-5.2`、`opencode-go/qwen3.7-max`、`opencode-go/gpt-5.6-luna`）。
- **官方 Console 端点**：默认按协议使用 OpenCode Console Go 网关的独立端点（目录 / OpenAI / Anthropic / Google 各自完整 URL），并保留 `/zen/go/v1` 单基地址兼容布局。
- **按请求动态路由**：每次请求根据客户端协议、模型原生协议与可配置策略选择上游协议；同协议优先直通，跨协议复用现有适配器转换。
- **多协议转换**：在客户端格式与上游端点之间转换请求与流式响应：
  - OpenAI Chat Completions（`/v1/chat/completions`）
  - Anthropic Messages（`/v1/messages`）
  - OpenAI Responses（`/v1/responses`）
- **思考与推理支持**：按目录声明的 `capabilities.reasoning.effort` 发布可用档位，在受支持的客户端与上游格式之间映射推理强度。
- **动态目录发现**：拉取远端模型目录，解析名称、上下文/输出上限、输入模态与原生协议，支持本地兜底与自定义路由覆盖。
- **多密钥认证调度**：汇聚多个 API 密钥，复用 CLIProxyAPI 原生调度器，在所有协议间做轮换、重试与错误冷却。
- **凭证健康检查与自动隔离**：注册/重配置时校验每个 Key；401 失效的凭证自动禁用并记录指纹，已移除的旧凭证自动停用，轮换 Key 无需重启即可平滑切换。
- **原生配额**：滚动、周、月配额统一走 CLIProxyAPI 通用配额接口，并使用当前选中的凭证。不再保留独立的插件页面。

## 环境要求

- **CLIProxyAPI**：`v8.0.0+`
- **Go 工具链**：Go 1.26.7+（C-shared 构建模式需要开启 CGO）

> 兼容性验证基线：CLIProxyAPI `v8.0.56`、Claude Code `2.1.318`（Release 构建，Windows amd64）。插件 ABI 为 1，注册 schema 由 CPA SDK 自动上报。

## 构建

本地开发请带 `debug` 标签构建共享库。
Release 构建由 CI 负责。

### Windows (AMD64)
```powershell
go build -tags debug -buildmode=c-shared -o plugins/windows/amd64/opencode-go-cliproxyapi.dll .
```

### Linux (AMD64)
```bash
go build -tags debug -buildmode=c-shared -o plugins/linux/amd64/opencode-go-cliproxyapi.so .
```

### macOS (ARM64)
```bash
go build -tags debug -buildmode=c-shared -o plugins/darwin/arm64/opencode-go-cliproxyapi.dylib .
```

将编译产物放入 CLIProxyAPI 的插件目录（例如 `<cliproxyapi_root>/plugins/<os>/<arch>/`）。

## 上游端点

默认使用 OpenCode Console Go 网关（[官方文档](https://opencode.ai/v2/docs/console/go/)），Go 套餐基地址为 `https://opencode.ai/inference/go`。Console 为每种协议提供独立路径，插件按协议分别配置完整 URL，不再由单一 Base URL 拼接：

| 用途 | 默认端点 |
|---|---|
| 模型目录 | `https://opencode.ai/inference/go/v1/models` |
| OpenAI Chat Completions | `https://opencode.ai/inference/go/openai/v1/chat/completions` |
| OpenAI Responses | `https://opencode.ai/inference/go/openai/v1/responses` |
| Anthropic Messages | `https://opencode.ai/inference/go/anthropic/v1/messages` |
| Google generateContent | `https://opencode.ai/inference/go/google/v1beta`（按请求追加 `/models/<model>:generateContent`） |
| 配额用量 | `https://opencode.ai/zen/go/v1/usage`（保留已验证地址） |

说明：

* 每个端点都可用对应的 `*-url` 配置项覆盖；所有 URL 必须是合法 HTTPS（`allow-http: true` 时可为 HTTP），且不带查询参数、fragment、userinfo。
* **兼容旧部署**：显式设置 `base-url`（例如 `https://opencode.ai/zen/go/v1`）时回到单基地址布局：目录 `{base}/models`、chat `{base}/chat/completions`、messages `{base}/messages`、responses `{base}/responses`、usage `{base}/usage`。未设置 `base-url` 时使用上表默认值。
* 官方文档未记载 usage 接口。`/inference/go/v1/usage` 与 `/zen/go/v1/usage` 均存在（未鉴权返回 401），插件保留已验证的后者，并提供 `usage-url` 覆盖，不按 URL 命名规律迁移。
* Google 原生协议当前没有传输适配器：声明 `generate-content` 的模型会经文本协议转译（Console 会对所有文本模型做协议翻译），并在目录诊断中说明；不会静默丢弃。

## 配置

在 CLIProxyAPI 的 `config.yaml` 中通过 `plugins.configs.opencode-go-cliproxyapi` 配置本插件：

```yaml
plugins:
  configs:
    opencode-go-cliproxyapi:
      # 端点：默认使用 Console Go 网关，按协议分别配置（互不拼接）
      # catalog-url: "https://opencode.ai/inference/go/v1/models"
      # chat-completions-url: "https://opencode.ai/inference/go/openai/v1/chat/completions"
      # responses-url: "https://opencode.ai/inference/go/openai/v1/responses"
      # messages-url: "https://opencode.ai/inference/go/anthropic/v1/messages"
      # google-url: "https://opencode.ai/inference/go/google/v1beta"
      # usage-url: "https://opencode.ai/zen/go/v1/usage"

      # 兼容旧部署：显式设置 base-url 时回到单一基地址布局（/zen/go/v1）
      # base-url: "https://opencode.ai/zen/go/v1"

      # 面向客户端的模型 ID 前缀配置
      model-prefix:
        enabled: true           # true -> "opencode-go/<model>"，false -> 裸 "<model>"（默认：true）
        value: "opencode-go"    # 前缀名称（默认："opencode-go"）

      # OpenCode Go API 密钥（至少填一个）。支持 ${ENV_VAR} 环境变量展开。
      api-keys:
        - value: "sk-opencode-key-1"
          name: "Personal"      # 可选的账号备注
        - value: "sk-opencode-key-2"
        - value: "${OPENCODE_GO_API_KEY}"

      # 可选：外部密钥文件（与 api-keys 合并，按值去重），定期重读实现无缝轮换
      # api-keys-file: "/run/secrets/opencode-go-keys.yaml"
      # api-keys-file-poll: "1m"       # 默认 "1m"，最小 "10s"

      # 目录发现设置
      catalog:
        refresh-interval: "15m"          # 目录刷新间隔，最小 "1m"（默认："15m"）
        stale-while-unavailable: true    # 刷新失败时保留上一次可用的目录快照（默认：true）

      # 协议开关（默认全开）。定义为允许使用的上游协议集合
      protocols:
        chat-completions: true   # 允许 chat-completions 作为上游协议
        messages: true           # 允许 messages 作为上游协议
        responses: true          # 允许 responses 作为上游协议
        google: true             # 允许 Google 原生协议的模型（经文本协议转译）

      # 协议路由策略（默认 native，保持旧行为）
      routing:
        mode: "native"           # "native" | "prefer-client-protocol" | "anthropic"
        allow-translation: true  # 客户端协议不可用时是否允许回退到模型原生协议

      # 按模型的显式路由覆盖（优先级高于目录 api 字段与前缀路由）
      route-overrides:
        "custom-model":
          protocol: "messages"           # "chat-completions" | "messages" | "responses"
          # endpoint 可选：不配置时使用该协议的默认端点（messages-url）
          # 配置时必须以 / 开头，并拼接到 base-url（前导 /v1 会被去掉）
          # endpoint: "/anthropic/v1/messages"

      # 执行设置
      request-timeout: "5m"              # 上游请求超时（默认："5m"）
      max-response-bytes: 67108864       # 非流式响应体的最大字节数（默认：64 MiB）
      allow-http: false                  # 是否允许 http:// 方案，用于本地 mock/测试（默认：false）
```

### 配置项说明

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `api-keys` | `[]object` | *（必填，可与文件合并）* | API 密钥列表（`- value: "..."`，可选 `name: "Personal"`）。支持 `${ENV_VAR}` 展开。重复与空值会被拒绝。 |
| `api-keys-file` | `string` | *（不设置）* | 外部密钥文件路径（YAML，格式同 `api-keys`：`api-keys:` 列表或 `- value:` 裸列表）。与 `api-keys` 合并并按值去重，文件内容变化后自动应用，无需重启。 |
| `api-keys-file-poll` | `duration` | `1m` | 重读 `api-keys-file` 的间隔，最小 `10s`。 |
| `catalog-url` | `string` | `https://opencode.ai/inference/go/v1/models` | 模型目录完整 URL。 |
| `chat-completions-url` | `string` | `https://opencode.ai/inference/go/openai/v1/chat/completions` | Chat Completions 端点。 |
| `messages-url` | `string` | `https://opencode.ai/inference/go/anthropic/v1/messages` | Anthropic Messages 端点。 |
| `responses-url` | `string` | `https://opencode.ai/inference/go/openai/v1/responses` | OpenAI Responses 端点。 |
| `google-url` | `string` | `https://opencode.ai/inference/go/google/v1beta` | Google generateContent 基地址（模型路径按请求追加）。 |
| `usage-url` | `string` | `https://opencode.ai/zen/go/v1/usage` | 配额用量端点。官方文档未记载 usage 接口，故保留已验证地址，不按 URL 规律迁移。 |
| `base-url` | `string` | *（不设置）* | **兼容项**。显式设置时改用单一基地址布局：目录 `{base}/models`、chat `{base}/chat/completions`、messages `{base}/messages`、responses `{base}/responses`、usage `{base}/usage`。所有 URL 必须是合法 HTTPS（`allow-http: true` 时可为 HTTP），且不带查询参数、fragment、userinfo。 |
| `model-prefix.enabled` | `bool` | `true` | 为 `true` 时客户端模型名为 `<prefix>/<model>`；为 `false` 时使用裸模型 ID。 |
| `model-prefix.value` | `string` | `opencode-go` | 开启前缀时的服务商前缀字符串。 |
| `catalog.refresh-interval` | `duration` | `15m` | 目录轮询刷新间隔（如 `15m`、`1h`）。最小为 `1m`。 |
| `catalog.stale-while-unavailable` | `bool` | `true` | 为 `true` 时，更新失败则继续提供上一次有效的目录快照。 |
| `protocols.chat-completions` | `bool` | `true` | 允许 Chat Completions 作为上游协议。 |
| `protocols.messages` | `bool` | `true` | 允许 Messages 作为上游协议。 |
| `protocols.responses` | `bool` | `true` | 允许 Responses 作为上游协议。 |
| `protocols.google` | `bool` | `true` | 允许 Google 原生协议的模型（当前经文本协议转译）。 |
| `routing.mode` | `string` | `native` | 路由策略：`native`（按模型原生协议）、`prefer-client-protocol`（优先客户端协议，不可用时按 `allow-translation` 回退）、`anthropic`（统一走 Messages，要求 `protocols.messages` 开启）。 |
| `routing.allow-translation` | `bool` | `true` | `prefer-client-protocol` 下客户端协议不可用时，是否允许回退到模型原生协议（需要本地协议转换）。 |
| `route-overrides` | `map` | `{}` | 模型 ID 到 `{ protocol: "...", endpoint: "..." }` 的映射，覆盖目录 `api` 字段与前缀路由。合法协议：`chat-completions`、`messages`、`responses`。**`endpoint` 可选**：不配置时使用该协议的默认端点（`chat-completions-url` / `messages-url` / `responses-url`）；配置时按 `base-url` 拼接（沿用旧规则：前导 `/v1` 会被去掉），默认模式下 `base-url` 为 `https://opencode.ai/inference/go`，因此需写完整路径（如 `/openai/v1/responses`）。 |
| `request-timeout` | `duration` | `5m` | 上游 HTTP 请求超时。非流式请求为整体超时；流式请求为首字节超时上限（不超过 60s）与**空闲超时**（每收到数据即重置，长会话不会被总时长截断）。必须为正数。 |
| `max-response-bytes` | `int64` | `67108864`（64 MiB） | 非流式响应体的最大字节数。 |
| `allow-http` | `bool` | `false` | 为 `true` 时，各端点 URL 允许使用 `http://` 方案，便于本地测试。 |

### 模型目录字段

插件解析 OpenCode Console `/v1/models` 的实际结构，并兼容旧版字段：

| 用途 | Console 字段 | 旧字段（兼容） |
|---|---|---|
| 显示名 | `name` | `display_name` |
| 上下文上限 | `limit.context` | `context_length` |
| 输出上限 | `limit.output` | `max_output_tokens` |
| 输入模态 | `modalities.input` | `input_modes` / `input_modalities` |
| 输出模态 | `modalities.output` | `output_modes` / `output_modalities` |
| Thinking 档位 | `capabilities.reasoning.supported` + `capabilities.reasoning.effort` | `thinking.{levels,zero_allowed,dynamic_allowed}` |
| 原生协议 | `api.{chat-completions,messages,responses,generate-content}` | 无（回退到前缀表） |

模型原始 JSON 会保留在记录中供诊断；协议开关关闭、端点越权、名称冲突等异常都会进入 `Unsupported()` 诊断并附带原因，不会静默丢弃模型。

### 路由决策

优先级从高到低：

1. **显式覆盖** `route-overrides`（仍为最高优先级）。
2. **`routing.mode` 策略**。
3. **协议可用性检查**。
4. **原生协议回退**。

| `routing.mode` | 行为 | 适用场景 |
|---|---|---|
| `native`（默认） | 使用模型目录声明的原生协议 | 保持旧版行为，升级无感 |
| `prefer-client-protocol` | 优先使用 CPA 交给插件的客户端协议；不可用时按 `allow-translation` 回退原生协议 | 推荐。Claude Code 走 Messages、OpenCode 走 Chat Completions、Codex 走 Responses |
| `anthropic` | 对可服务的文本模型统一使用 Messages；要求 `protocols.messages` 开启 | Claude Code 专用部署 |

“协议可用”需同时满足：插件已实现该协议、对应 `protocols.*` 开关已开启、模型可表达该协议。Console 目录中的文本模型可跨 API 调用；旧目录模型与非文本输出模型仅限原生协议，未知能力不默认支持。

`protocols.*` 是**允许的上游协议集合**，不是模型过滤器：关闭某个协议不会隐藏“原生协议属于该类型、但可通过其他协议访问”的模型。`prefer-client-protocol` 下若目标协议被关闭，会回退到其他可用协议。

示例：

```text
Claude Code ──Messages──▶ CPA ──Messages──▶ OpenCode Go   （prefer-client-protocol，零本地转换）
Claude Code ──Messages──▶ CPA ──Chat──────▶ OpenCode Go   （native，按模型原生协议转换）
Codex       ──Responses─▶ CPA ──Responses─▶ OpenCode Go
```

### Thinking 档位

* 档位来源为目录的 `capabilities.reasoning.effort`（如 `glm-5.2` 为 `high/max`，`deepseek-v4.1-flash` 为 `low/high/max`），插件将其 1:1 发布给 CPA。
* 上下游协议一致时（如 Messages → Messages）Thinking 参数**原样透传**，不做本地改写。
* 跨协议时使用统一阈值表在“token 预算”和“离散档位”之间换算，并钳制到模型声明的范围内。
* 模型不支持的档位会被明确拒绝（例如返回 `level "medium" not supported, valid levels: high, max`），不会静默转换。
* CPA 支持在模型名后使用 `(level)` 语法（如 `opencode-go/glm-5.2(high)`），在调用插件前按已发布档位校验/钳制。

### Session ID

上游要求 Go 套餐请求携带稳定的 `x-opencode-session`。插件按以下优先级取值：

1. CPA 提供的会话元数据（`canonical_session_id`、`session_id`、`conversation_id`、`prompt_cache_key`）。
2. 客户端会话头：`X-Opencode-Session`、`X-Session-Affinity`、`X-Claude-Code-Session-Id`、`X-Conversation-Id`、`X-Session-Id`、`Session-Id`。
3. 以上都缺失时，使用会话首个用户轮次内容的 SHA-256（多轮请求保持稳定）。

取值会去除首尾空白并限制长度；不会伪造鉴权信息或绕过上游限制。

### 凭证健康与轮换

OpenCode Go 使用静态 API Key，没有刷新令牌接口，因此插件通过“校验 + 隔离 + 平滑轮换”保证可用性：

* **注册/重配置校验**：对每个配置 Key 调用 `GET {usage-url}`（无 token 消耗）。仅 **401** 判定为失效；5xx 与网络错误视为不确定，不会禁用凭证。
* **失效隔离**：401 的凭证写入 `disabled: true`，CPA 调度器不再选中；日志记录 `key_fingerprint`（sha256 前 8 位）与文件名，不记录明文。
* **最后凭证保护**：若所有配置 Key 都失效，保留启用状态，让客户端收到明确的 401，而不是 503 `no auth available`。
* **旧凭证停用**：配置中不再存在的 Key 对应凭证会被自动禁用（文件保留，可手工删除），避免过期 Key 继续参与调度。
* **定期体检**：插件内置 15 分钟周期体检（自驱动，不依赖宿主调度——CPA 的 auto-refresh 循环不会调度 API-key 类凭证）：周期性探测每个 Key，401 自动禁用、恢复后自动重新启用。宿主的 `auth.refresh` 仅在请求 401 触发或管理界面手动刷新时调用，插件会正确响应并回填凭证的 disabled 状态。
* **外部密钥文件**：`api-keys-file` 由插件定期重读（默认 1m），新增/移除 Key 会自动应用对应凭证，无需重启 CPA。
* **平滑轮换**：先在 `api-keys` 或密钥文件中同时保留新旧 Key 并重载/等待轮询，确认新 Key 生效后再移除旧 Key；在途流不受影响，新请求只用健康凭证。
* **运行时行为**：上游 401 返回 401 且标记不可重试（交由 CPA 切换凭证）；首字节前网络错误返回 502、超时返回 504；流式请求使用空闲超时，长会话不会被截断。错误码对齐 CPA v8.0.62+ 的 cooldown 语义：传输层瞬时失败（`transient_transport`）不会冷却健康凭证，流式中断（`stream_truncated`）不会换凭证重放，模型级 404（`model_not_found`）只冷却模型。
* **count_tokens**：Claude 源的 `/v1/messages/count_tokens` 会以选中凭证代理到上游 Anthropic count_tokens 端点；其他源格式仍返回明确的 unsupported。

### 冷却与可用性（个人部署建议）

插件本身不维护冷却状态；401/402/403/404/429/5xx 的冷却时长完全由 CPA 宿主按错误码/状态决定（例如 402/403 默认冷却模型 30 分钟、404 冷却 12 小时）。如果你的 CPA 只供个人使用、且对上游额度与可用性有把握，建议**关闭宿主冷却**，让上游错误直接返回给客户端，避免网络抖动或上游配额误报造成长时间 503：

```yaml
routing:
  cooldown:
    # 全局关闭 auth/model 冷却调度，避免失败后的黑洞窗口
    disable-cooling: true
  retry:
    # 流式请求不建议自动重放，避免重复生成
    request-retry: 0
```

代价：402/429/5xx 时每次都要真实等待上游响应，而不是快速失败；401 仍会触发插件的凭证禁用（15 分钟体检后恢复），最后凭证保护仍生效，不会出现 503 空池。

## 测试

```powershell
# 运行全部测试
go test -tags debug ./...

# 带覆盖率运行测试
go test -tags debug ./... -cover

# 运行 linter / vet 检查
go vet ./...
```

## 账号名称与配额

在每个 `api-keys` 条目的 `value` 之外，可选设置 `name`。
例如 `name: Personal` 或 `name: Work`。未命名时，单个密钥显示为 **OpenCode Go**；多个密钥依次显示为 **OpenCode Go 1**、**OpenCode Go 2**，依此类推。
默认编号按配置顺序生成。显式名称在密钥重排后保持稳定。
用量响应中不包含邮箱或账号身份信息，插件也不会从密钥推断邮箱。

新建凭证文件使用可读文件名。已存在的 auth ID 保持不变，
解析时会替换掉旧的自动生成标签。已有的自定义标签会被保留，
除非配置中指定了新的名称。已有文件名会自动保留；
如需把旧文件重命名为可读的 `.json` 文件名，请先停止 CLIProxyAPI，并保持
其 JSON 中的 `id` 与其他字段不变。操作前请先备份文件。

先用 `GET /v0/management/quota/providers` 查询支持情况，再调用
`POST /v0/management/quota/fetch` 并传入 `{"auth_index":"<选中序号>"}`。
这些接口需要 CLIProxyAPI 管理密钥。归一化后的响应包含
`subscription.plan`、`groups[].buckets[].window`、`remainingFraction` 与 `resetTime`。
缺失的时间窗口会被省略。非法读数会直接返回错误，不会编造零值。
不支持配额重置。旧的插件配额路由与资源页面已移除。
