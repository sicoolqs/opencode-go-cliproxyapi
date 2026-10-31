## 未发布

### 改进

- 升级 CLIProxyAPI SDK 至 `v8.0.42`，并将上游错误响应的 `Retry-After` 指引（秒数或 HTTP-date）透传为 `retry_after_seconds`，使 CPA 认证冷却与下游重试头遵循上游提示。ABI/Schema 无变化。
- 升级 CLIProxyAPI SDK 至 `v8.0.48`（ABI=1、Schema=9，单 provider 注册保持兼容，无需改动）。
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
