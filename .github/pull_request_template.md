提交前请先阅读 [CONTRIBUTING.md](../CONTRIBUTING.md)，重点是 Pull Request 规范。

## 摘要

<!-- 改了什么、为什么改？ -->

## 变更内容

<!-- 列出主要影响的行为或组件。保持每个 PR 只聚焦一个修复或特性。 -->

## 验证

- [ ] `go test -tags debug ./...`
- [ ] `go vet ./...`
- [ ] Bug 修复：已在 `tests/live_smoke_test.go` 补充活测（`go test -tags live ./tests/...`）。
- [ ] 新增或变更的行为路径已有单元测试。
- [ ] 协议层改动包含流式与非流式测试。
- [ ] `RELEASE_NOTES.md` 已更新。
- [ ] 如涉及用户可见配置变更，`README.md` 已更新。

## 兼容性与已知限制

<!-- 如有兼容性要求或已知限制，请在此说明。 -->
