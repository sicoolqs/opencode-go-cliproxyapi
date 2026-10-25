# OpenCode Go CLIProxyAPI 插件

一个原生动态 Go 插件（适用于 [CLIProxyAPI](https://help.router-for.me/plugin/development)），将 OpenCode Go 以单一服务商（`opencode-go`）的形式暴露出来。

该插件统一了 OpenCode Go 上游各端点的模型发现、协议转换与执行，同时复用 CLIProxyAPI 内建的认证、调度、密钥轮换与冷却管理。

## 问题背景

OpenCode Go 的模型分布在多种 API 协议上（OpenAI Chat Completions `/v1/chat/completions`、Anthropic Messages `/v1/messages`、OpenAI Responses `/v1/responses`）。

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
- **多协议转换**：在客户端格式与上游端点之间转换请求与流式响应：
  - OpenAI Chat Completions（`/v1/chat/completions`）
  - Anthropic Messages（`/v1/messages`）
  - OpenAI Responses（`/v1/responses`）
- **思考与推理支持**：在受支持的客户端与上游格式之间映射推理强度（reasoning effort）。
- **动态目录发现**：拉取远端模型目录，支持本地兜底与自定义路由覆盖。
- **多密钥认证调度**：汇聚多个 API 密钥，复用 CLIProxyAPI 原生调度器，在所有协议间做轮换、重试与错误冷却。
- **原生配额**：滚动、周、月配额统一走 CLIProxyAPI 通用配额接口，并使用当前选中的凭证。不再保留独立的插件页面。

## 环境要求

- **CLIProxyAPI**：`v8.0.0+`
- **Go 工具链**：Go 1.26.7+（C-shared 构建模式需要开启 CGO）

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

## 配置

在 CLIProxyAPI 的 `config.yaml` 中通过 `plugins.configs.opencode-go-cliproxyapi` 配置本插件：

```yaml
plugins:
  configs:
    opencode-go-cliproxyapi:
      # 上游基地址（默认："https://opencode.ai/zen/go/v1"）
      base-url: "https://opencode.ai/zen/go/v1"

      # 可选的目录接口覆盖（默认："{base-url}/models"）
      # catalog-url: "https://opencode.ai/zen/go/v1/models"

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

      # 目录发现设置
      catalog:
        refresh-interval: "15m"          # 目录刷新间隔，最小 "1m"（默认："15m"）
        stale-while-unavailable: true    # 刷新失败时保留上一次可用的目录快照（默认：true）

      # 协议开关（默认全开）
      protocols:
        chat-completions: true   # 启用路由到 /v1/chat/completions 的模型
        messages: true           # 启用路由到 /v1/messages 的模型
        responses: true          # 启用路由到 /v1/responses 的模型

      # 按模型的显式路由覆盖（优先级高于内置前缀路由）
      route-overrides:
        "custom-model":
          protocol: "messages"           # "chat-completions" | "messages" | "responses"
          endpoint: "/v1/messages"       # 必须以 / 开头

      # 执行设置
      request-timeout: "5m"              # 上游请求超时（默认："5m"）
      max-response-bytes: 67108864       # 非流式响应体的最大字节数（默认：64 MiB）
      allow-http: false                  # 是否允许 http:// 方案，用于本地 mock/测试（默认：false）
```

### 配置项说明

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `api-keys` | `[]object` | *（必填）* | API 密钥列表（`- value: "..."`，可选 `name: "Personal"`）。支持 `${ENV_VAR}` 展开。重复与空值会被拒绝。 |
| `base-url` | `string` | `https://opencode.ai/zen/go/v1` | 上游基地址。必须是合法 HTTPS（`allow-http: true` 时可为 HTTP），且不带查询参数、fragment、userinfo。 |
| `catalog-url` | `string` | `{base-url}/models` | 目录发现完整 URL。默认为 `{base-url}/models`。 |
| `model-prefix.enabled` | `bool` | `true` | 为 `true` 时客户端模型名为 `<prefix>/<model>`；为 `false` 时使用裸模型 ID。 |
| `model-prefix.value` | `string` | `opencode-go` | 开启前缀时的服务商前缀字符串。 |
| `catalog.refresh-interval` | `duration` | `15m` | 目录轮询刷新间隔（如 `15m`、`1h`）。最小为 `1m`。 |
| `catalog.stale-while-unavailable` | `bool` | `true` | 为 `true` 时，更新失败则继续提供上一次有效的目录快照。 |
| `protocols.chat-completions` | `bool` | `true` | Chat Completions 端点的协议开关。 |
| `protocols.messages` | `bool` | `true` | Messages 端点的协议开关。 |
| `protocols.responses` | `bool` | `true` | Responses 端点的协议开关。 |
| `route-overrides` | `map` | `{}` | 模型 ID 到 `{ protocol: "...", endpoint: "..." }` 的映射，覆盖内置的家族路由。合法协议：`chat-completions`、`messages`、`responses`。 |
| `request-timeout` | `duration` | `5m` | 上游 HTTP 请求超时。必须为正数。 |
| `max-response-bytes` | `int64` | `67108864`（64 MiB） | 非流式响应体的最大字节数。 |
| `allow-http` | `bool` | `false` | 为 `true` 时，`base-url` / `catalog-url` 允许使用 `http://` 方案，便于本地测试。 |

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
