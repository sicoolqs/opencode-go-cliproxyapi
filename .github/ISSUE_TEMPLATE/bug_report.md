---
name: Bug Report
about: 上报插件中可复现的问题或非预期错误
title: "[BUG] "
labels: bug
assignees: ''
---

### 问题描述
清晰简要地描述这个 Bug 是什么。

### 复现步骤
1. 入站端点：（例如 `/v1/chat/completions`、`/v1/messages`、`/v1/responses`）
2. 目标模型 ID：（例如 `opencode-go/gpt-5.6-luna`）
3. 脱敏后的请求 payload：
```json
// 在此粘贴请求 payload
```

### 预期行为
应该发生什么。

### 实际行为 / 错误堆栈
实际发生了什么，包括 HTTP 状态码与上游/插件错误输出：
```text
// 在此粘贴错误响应或日志堆栈
```

### 环境信息
- **CLIProxyAPI 版本：**（例如 v7.2.138）
- **插件版本 / Commit：**
- **操作系统：**（例如 Windows / Linux / macOS）
- **Go 版本：**（如从源码构建）
