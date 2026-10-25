# 参与贡献 OpenCode Go CLIProxyAPI 插件

感谢你的参与。本指南介绍如何搭建开发环境、构建插件以及提交 Pull Request。

---

## 开发环境搭建

### 前置条件
- Go 1.26+（C-shared 构建需要开启 CGO）
- Git

### 构建与测试命令

**本地开发务必使用 `debug` 构建标签。不要以 Release 模式构建。**

运行测试套件：
```powershell
go test -tags debug ./...
```

运行静态检查：
```powershell
go vet ./...
```

构建本地测试用的原生共享库：
```powershell
go build -tags debug -buildmode=c-shared -o plugins/windows/amd64/opencode-go-cliproxyapi.dll .
```

---

## Pull Request 规范

1. **聚焦范围：** 每个 PR 只做一件事（单个修复或单个特性）。不要把不相关的重构或特性打包在一起。
2. **Debug 模式验证：** 所有改动都用 `-tags debug` 验证。Release 打包由 CI 单独处理。
3. **测试覆盖：** 所有新增或变更的行为路径都必须有单元测试。协议层改动必须同时覆盖流式与非流式测试。
4. **修复必须带活测：** 任何 Bug 修复都必须在 `tests/live_smoke_test.go` 中补充活测（`go test -tags live ./tests/...`），覆盖上报的问题。
5. **离线可执行：** 单元测试必须可在本地离线运行，使用 mock host client。不要在单元测试中引入对线上外部 API 的依赖。
6. **文档：** 改动合并/追加到 `RELEASE_NOTES.md`，如涉及用户可见的配置变更请同步更新 `README.md`。
7. **代码风格：** 所有代码用标准 `gofmt` 格式化，并遵循惯用的 Go 规范。

---

## 工作流程

1. Fork 本仓库，并从 `main` 切出特性分支。
2. 按上述代码风格与测试规范实现改动。
3. 确保 `go test -tags debug ./...` 与 `go vet ./...` 干净通过。
4. 向 `main` 发起 Pull Request，写清楚改动内容及关联的 issue。
