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
   go build -buildvcs=false -trimpath -buildmode=c-shared -ldflags "-s -w" -o "dist\model-quota.dll" .
   ```
   生成后删除 `-buildmode=c-shared` 附带的 `.h` 文件。

### Windows 上交叉编译 Linux/macOS 产物：必须用 zig cc

MinGW-w64 只能产出 Windows PE 格式（`.dll`），无法产出 Linux ELF / macOS Mach-O。本机以 zig 作为交叉 C 工具链（`D:\Work\zig\zig.exe`，无需加入 PATH，编译时显式引用）：

```powershell
# Linux x64（glibc 基线 2.17，兼容绝大多数发行版）
$env:CGO_ENABLED = "1"; $env:GOOS = "linux"; $env:GOARCH = "amd64"
$env:CC = "D:\Work\zig\zig.exe cc -target x86_64-linux-gnu.2.17"
go build -buildvcs=false -trimpath -buildmode=c-shared -ldflags "-s -w" -o "dist\model-quota.so" .

# macOS（如需）
$env:CC = "D:\Work\zig\zig.exe cc -target x86_64-macos"    # Intel
$env:CC = "D:\Work\zig\zig.exe cc -target aarch64-macos"   # Apple Silicon
```

### ⚠️ 关键教训：glibc vs musl 必须与宿主镜像匹配

- **默认用 glibc 目标（`x86_64-linux-gnu.2.17`）**：Debian/Ubuntu/CentOS 等主流镜像都是 glibc，运行时只认带版本号的 `libc.so.6`。
- **禁止默认用 musl 目标**：musl 产物依赖无版本号的 `libc.so`，该文件在 glibc 系统上只存在于 `libc6-dev` 开发包，运行时 dlopen 直接报错：
  ```
  dlopen .../model-quota.so: libc.so: cannot open shared object file: No such file or directory
  ```
  这是本项目实际踩过的坑（CPA 官方 Docker 镜像为 glibc）。
- 只有确认宿主是 Alpine（musl 系统）时才用 `-target x86_64-linux-musl`。拿不准时请用户先在容器内执行 `cat /etc/os-release` 确认。

### 产物验证（zig 0.17 的 objdump 是占位实现，不可用）

用字节级检查代替：

```powershell
$bytes = [System.IO.File]::ReadAllBytes("dist\model-quota.so")
$text = [System.Text.Encoding]::ASCII.GetString($bytes)
# ELF 头：$bytes[4]=2 → ELF64；offset 16 (u16)=0x03 → ET_DYN 共享库；offset 18 (u16)=0x3E → x86-64
# libc 依赖：$text.Contains("libc.so.6`0") → glibc 产物正确
# musl 产物特征：精确匹配 "libc.so`0"（无版本号）
# 导出符号：检索 cliproxy_plugin_init / cliproxyPluginCall / cliproxyPluginFree / cliproxyPluginShutdown
```

### 代理环境注意

本机全局环境变量 `HTTP_PROXY`/`HTTPS_PROXY` 使用了 `https://` 方案，会导致 Git Credential Manager 报
`ServicePointManager 不支持具有 https 方案的代理` 而中断推送。需要 git 网络操作时，临时覆盖：

```powershell
$env:HTTP_PROXY = "http://127.0.0.1:7890"; $env:HTTPS_PROXY = "http://127.0.0.1:7890"
git push origin main
```

## CPA 版本适配与升级检查清单

本插件基于 CLIProxyAPI v8 插件体系（审计基线：`router-for-me/CLIProxyAPI@main` commit `2422c6b`，v8.0.32）。适配新版 CPA 时按本清单重跑合规审计。

### 关于 `/v0/management` 的「废弃」注记（不要误读）

CPA 仓库 `AGENTS.md` 声称 `/v0/management` 已废弃——那**仅针对内核自身管理端点的维护策略**（v8 新功能只进 `/v8/management`，v0 冻结只修编译错误），不是移除。官方文档 `docs/management-api-v8.md` 明确 "The existing `/v0/management` API remains supported"。

而**插件 Management API** 虽挂在同一前缀下，但属于 `internal/pluginhost` 子系统，是活跃维护的：

- `pluginhost/management.go` 的 `managementBasePath = "/v0/management"` 仍是插件路由注册的现行常量
- v8 的插件商店、插件配置（`/plugins/:id/config`）、插件额度（`/plugins/:id/quota`）端点全部挂在 `/v0/management` 组下
- 官方示例 `examples/plugin/management-api` 即此模式

**结论：插件路由继续挂 `/v0/management` 是正确且安全的。** 若未来 CPA 官方迁移插件路由前缀，本插件只需改两处：`main.go` 的 `matchPath` 前缀列表 + `resources/index.html` 里面板 `fetch` 的请求路径字符串。

### 升级新版 CPA 时的审计项

1. `sdk/pluginabi/types.go`：核对 `ABIVersion`（当前 1）与 `SchemaVersion`（当前 6）是否变化；若变化需同步 `main.go` 的 `cliproxy_plugin_init` 与 `registrationPayload`
2. `internal/pluginhost/loader_*.go`：核对 C ABI 结构体与 `cliproxy_plugin_init` 符号名是否变化
3. `internal/pluginhost/rpc_schema.go`：核对方法名（`plugin.register` / `management.register` / `management.handle` / `host.http.*`）与 JSON 字段名（rpc 外壳小写 tag、pluginapi 结构体大写无 tag）是否变化
4. `internal/api/server_management.go`：确认插件路由的挂载与鉴权方式是否迁移
5. 关注新增能力是否值得采用：如 `quota_provider`（注意：其凭据模型绑定 CPA auth 体系，本插件的配置式 API Key 模型不匹配，宿主不会为其发起 `quota.fetch`，暂不采用——详见 2026-10 合规审计记录）

### 已知行为差异（勿重复排查）

- `host.http.do` 的请求体**没有 timeout 字段**，`timeout_ms` 会被宿主静默忽略。真实超时依赖 `host.http.operation_open` + 请求携带 `operation_id` + 超时后 `host.http.cancel`（已在 `hostbridge.go` 实现，旧版宿主自动降级为不可取消请求）
- `plugin.quiesce` 已显式处理（返回空成功信封）；即使不处理，宿主对含 "unknown method" 的错误信封也仅记 Debug 日志（`host.go` 的 `quiesceUnsupported`）
