## 未发布

### 改进

- 新增 `api-keys-file` / `api-keys-file-poll`：外部密钥文件与内联 `api-keys` 合并，定期重读并自动应用（新增启用、移除禁用），轮换无需重启。
- 凭证周期体检：插件内置 15 分钟自驱动健康检查（CPA 的 auto-refresh 循环不调度 API-key 类凭证，`auth.refresh` 仅由请求 401 或手动刷新触发），周期探测每个 Key：401 自动禁用、恢复后自动重新启用；`auth.refresh` 处理器回填存储的 `disabled` 状态，刷新响应不会把已禁用凭证洗白（兼容 v8.0.61 之前的宿主）。
- `executor.count_tokens` 实现：Claude 源请求代理到上游 Anthropic `count_tokens`，避免 Claude Code 持续 400。
- 凭证健康检查：注册/重配置时探测每个 Key（`GET {usage-url}`），401 的凭证自动禁用并记录指纹；最后一个可用 Key 失效时保留启用，避免 503 空池。
- 旧凭证隔离：配置中已移除的 Key 对应凭证自动 `disabled`，不再参与调度；文件保留可手工删除。
- 错误分类对齐：上游 401 改为不可重试；首字节前网络错误返回 502、超时返回 504，便于客户端与 CPA 正确收敛。
- 流式超时改为“首字节超时（≤60s）+ 空闲超时”，长会话不再被 5 分钟总时长截断。
- 新增单元测试覆盖凭证禁用、最后凭证保护、旧凭证隔离、不确定探测不改状态、空闲超时重置、首字节超时 504、count_tokens 代理、周期刷新与密钥文件热应用。

### 改进

- 新增按请求的协议路由策略：`routing.mode` 支持 `native`（默认，保持旧行为）、`prefer-client-protocol`（Claude Code → Messages、OpenCode → Chat Completions、Codex → Responses，优先同协议、减少转换）与 `anthropic`（统一 Messages）；`routing.allow-translation` 控制是否允许回退到模型原生协议。
- 路由决策统一由 `catalog.ResolveRoute` 产出，流式与非流式共用；`protocols.*` 重新定义为允许的上游协议集合，关闭某协议不再隐藏可通过其他协议服务的模型。
- 协议可用性按“已实现 + 已启用 + 模型可表达”判定：Console 文本模型可跨 API 调用，旧目录与非文本模型仅限原生协议，未知能力不默认支持；不支持时返回明确错误。
- 显式 `route-overrides` 仍为最高优先级；Google 原生模型无传输时回退到文本协议并输出诊断。
- 对齐 OpenCode Console Go 官方规范：默认端点改为按协议独立配置（目录 `/inference/go/v1/models`、chat `/inference/go/openai/v1/chat/completions`、responses `/inference/go/openai/v1/responses`、messages `/inference/go/anthropic/v1/messages`、google `/inference/go/google/v1beta`）；显式设置 `base-url` 时保留 `/zen/go/v1` 单基地址布局。
- 模型目录解析新版字段：`name`、`limit.context/output`、`modalities.input/output`、`capabilities.reasoning.effort`、`api`，并保留原始元数据；旧字段继续兼容。
- 路由改用目录 `api` 字段作为原生协议依据，前缀表仅作旧目录兜底；Google 原生模型经 chat-completions 转译并输出诊断；未知协议模型进入 `Unsupported()` 诊断而非静默丢弃。
- Session ID：优先透传 CPA 元数据（`canonical_session_id`/`session_id`/`conversation_id`/`prompt_cache_key`）与官方 `x-opencode-session` 等客户端头，缺失时才按首轮用户内容派生，多轮保持稳定。
- 配额：新增 `usage-url` 配置项，默认保留已验证的 `/zen/go/v1/usage`，不按 URL 规律迁移。

### 改进

- 升级 CLIProxyAPI SDK 至 `v8.0.42`，并将上游错误响应的 `Retry-After` 指引（秒数或 HTTP-date）透传为 `retry_after_seconds`，使 CPA 认证冷却与下游重试头遵循上游提示。ABI/Schema 无变化。
- 升级 CLIProxyAPI SDK 至 `v8.0.48`（ABI=1、Schema=9，单 provider 注册保持兼容，无需改动）。
- 升级 CLIProxyAPI SDK 至 `v8.0.50`（ABI=1、Schema=10，新增能力位均不适用；例行跟进，无代码改动）。
- 支持会话中 Claude `tool_addition` 块（与 host translator 同语义）：内联 `tool_definition` 合并为上游工具，`tool_reference` 按顶层声明解析，无法解析的条目跳过不再整单 400。覆盖 claude→chat-completions / messages / responses 三条腿，块内容不向上传输。

## 本次变更

### Bug 修复

- 保留路由到 Chat Completions 的 Claude 图像工具结果：在工具回复后，把图片保留在关联的用户消息中，同时保留文本、工具引用与错误标记，并纳入会话推导。所选上游模型必须支持图像输入。

特别感谢 @c0mm4nd 在 #16 中的贡献。

- 修复收到 Claude 客户端 `tool_reference` 内容块（如 Claude Code 中的 ToolSearch、网页搜索、网页抓取）时的 `tool_result` 转换，避免在 DeepSeek 等 Chat Completions 与 Responses 模型上出现 500 错误与会话卡死。

特别感谢 @SpiGAndromeda 在 #14 中的贡献。

## 升级说明

- 用新版本插件二进制替换旧插件二进制。
- 替换插件后重启 CLIProxyAPI。

**完整变更记录**：https://github.com/massiveits/opencode-go-cliproxyapi/compare/v0.2.1...v0.2.2
