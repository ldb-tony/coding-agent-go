# MCP 外部工具连接

> 对应教学章节 s14，参考实现：[mcp.go](../mcp.go) / [mcp_servers.go](../mcp_servers.go) / [mcp_permission.go](../mcp_permission.go)
>
> 上层入口工具：`connect_mcp`（让 Agent 在运行时 bring-in 外部服务提供的工具）

---

## 1. 架构概述

MCP 能力把"工具定义在代码里"改成了"工具在运行时动态发现"。五个组件串成一条明确的链路：

```
MCPServer (外部服务抽象)
   │  ListTools()              CallTool(name, args)
   ▼
MCPClient                    ← mcp.go 里定义，持有一个 server 实例 + 发现的工具列表
   │
   ▼
mcpRegistry                  ← 会话级注册表，管理所有已连接的 MCPClient
   │  assembleToolDefs()      callTool(safeName, argsJSON)
   ▼                          ▼
assembleToolPool(st)         runTool fallback (mcp__ 前缀)
   │                          │
   ▼                          ▼
每轮 Send 的 tools[]          mcpRegistry.clients[server].callTool → server.CallTool
```

关键设计点：

- **server 是接口**，不是具体实现。docsServer / deployServer 是教学用的进程内 stand-in；真实 MCP（stdio/SSE）只需实现 `MCPServer` 两个方法即可接入。
- **registry 是会话级**（`sessionState.mcp`），REPL 每轮新建 agentLoop，MCP 连接不跨轮持久化——每次新会话都要重新 `connect_mcp`。这符合 s14 的设计，如需跨会话持久化可后续扩展。
- **每轮工具池重算**：`assembleToolPool(st)` 在每轮 LLM 调用前组装——基础工具 + 已发现的 MCP 工具。Agent loop 和子代理都走这个入口，connect 之后下一轮生效。
- **runTool 有三级分发**：先查静态 `toolHandlers` map（基础工具），再查 `mcp__` 前缀走 registry fallback（动态发现的工具），最后返回 "unknown tool"。

## 2. REPL 使用示例

启动：

```bash
go run .
```

### 场景 A：连接 docs 服务并搜索

```text
> 连接文档服务，搜索 "mcp" 相关主题，再告诉我当前文档 API 版本
[HOOK] UserPromptSubmit: working in ...
[HOOK] connect_mcp({"name":"docs"})
Connected MCP server "docs". New tools available:
  - mcp__docs__search  (Search the documentation knowledge base by keyword...)
  - mcp__docs__get_version  (Return the current documentation API version...)

You can now invoke these tools directly.
[HOOK] mcp__docs__search({"query":"mcp"})
Found 5 results for "mcp":

1. [MCP] Tool name prefixing
   Tools from external servers are exposed as mcp__{server}__{tool}...
2. [MCP] connect_mcp tool
   Use connect_mcp(name="docs") to bring external tools into...
...

[HOOK] mcp__docs__get_version({})
Documentation API: v2.3.1
Last updated: 2026-10-15
...
```

**时间线**：
- Turn 1：模型只看到基础工具 + `connect_mcp`，发出 `connect_mcp(name="docs")`
- Turn 2：工具池已包含 `mcp__docs__search` / `mcp__docs__get_version`，模型可以直接调用

### 场景 B：deploy 服务的确认流程

deploy server 的 `trigger` 在 host 策略里配置为 `confirm`：

```text
> 连接 deploy 服务，触发 web-service 的部署
[HOOK] connect_mcp({"name":"deploy"})
Connected MCP server "deploy"...

[HOOK] mcp__deploy__trigger({"service":"web-service"})
⚠  MCP external tool — host policy requires confirmation
   Tool: mcp__deploy__trigger({"service":"web-service"})
   Allow? [y/N] y
Deployment triggered for web-service
Image:    latest
Strategy: rolling
Expected: ~90s
```

答 `n` 则拦截，tool_result 里会写明 "Permission denied"。

### 场景 C：未配置 server 名 → 错误回到模型

```text
> 连接 "metrics" 服务
Unknown MCP server "metrics". Available: docs, deploy
```

错误作为 tool_result 回喂模型，可以在下一轮自我纠正（换个 server 名重试），不会炸掉 loop。

## 3. 实现自定义 MCPServer

三步就能让一个新服务接入：实现接口 → 在 `mockMCPFactories` 注册 → 加 host-side 策略。

### 3.1 实现 MCPServer 接口

```go
// mcp.go 里的接口定义
type MCPServer interface {
    ListTools() ([]ToolDef, error)
    CallTool(name string, args map[string]any) (string, error)
}
```

`ToolDef` 结构：

```go
type ToolDef struct {
    Name        string         // server 内部的原始名, 会被安全化为 mcp__{server}__{tool} 的 tool 部分
    Description string         // 给模型看的用途说明
    InputSchema map[string]any // JSON Schema (type=object, properties, required)
}
```

一个最小示例（可以放进项目任何文件，比如 `custom_server_example_test.go` 做测试）：

```go
// weatherServer 是一个模拟天气查询服务
type weatherServer struct{}

func newWeatherServer() MCPServer { return &weatherServer{} }

func (s *weatherServer) ListTools() ([]ToolDef, error) {
    return []ToolDef{
        {
            Name:        "forecast",
            Description: "Get weather forecast for a city. Returns temperature, condition, and chance of rain.",
            InputSchema: map[string]any{
                "type": "object",
                "properties": map[string]any{
                    "city": map[string]string{"type": "string", "description": "City name, e.g. 'Beijing'"},
                    "days": map[string]any{"type": "integer", "description": "Forecast days (default 1)"},
                },
                "required": []string{"city"},
            },
        },
    }, nil
}

func (s *weatherServer) CallTool(name string, args map[string]any) (string, error) {
    if name != "forecast" {
        return "", fmt.Errorf("unknown tool %q", name)
    }
    city, _ := args["city"].(string)
    days := 1
    if d, ok := args["days"].(float64); ok && int(d) > 0 {
        days = int(d)
    }
    return fmt.Sprintf("%s: %d°C, sunny, 10%% rain (mock, days=%d)", city, 25+len(city)%10, days), nil
}
```

### 3.2 注册工厂

在 [mcp.go](../mcp.go) 末尾的 `mockMCPFactories` map 里加一行：

```go
var mockMCPFactories = map[string]func() MCPServer{
    "docs":     newDocsServer,
    "deploy":   newDeployServer,
    "weather":  newWeatherServer,   // ← 新增
}
```

### 3.3 加 host-side 策略

在 [mcp_permission.go](../mcp_permission.go) 的 `mcpHostPolicy` 里配置：

```go
var mcpHostPolicy = map[string]mcpPermissionLevel{
    "docs/search":      mcpPermAllow,
    "docs/get_version": mcpPermAllow,
    "deploy/status":    mcpPermAllow,
    "deploy/trigger":   mcpPermConfirm,
    "weather/forecast": mcpPermAllow,   // ← 天气查询是只读, 放行
}
```

未配置的 `(server, tool)` 对默认走 `confirm`——最安全的兜底。

### 3.4 真实 MCP 接入

`mockMCPFactories` 里的 value 是 `func() MCPServer`。换成真实 MCP 传输层时，只需实现 MCPServer 的两个方法桥接到协议：

- `ListTools()` → 向 MCP server 发 `tools/list` 请求，把返回的工具列表转换成 `[]ToolDef`
- `CallTool(name, args)` → 发 `tools/call` 请求，把 JSON-RPC result 转成 `string, error`

stdio / SSE transport 的桥接逻辑放在独立包里，工厂函数在每次 `connect_mcp` 时新建一个 client 实例（进程内独立的 stdio 管道或 HTTP 客户端）。

## 4. 命名规范化与碰撞检测

MCP server 可以暴露任意名字的工具（`get.version`、`one/two`、`with-dash`）。模型工具名只允许 `[A-Za-z0-9_]`。

**规范化规则**（`safeToolName`）：

```go
var mcpNameSanitizer = regexp.MustCompile(`[^A-Za-z0-9_]`)

func safeToolName(raw string) string {
    return mcpNameSanitizer.ReplaceAllString(raw, "_")
}
```

示例：

| server 内部名 | safeToolName 结果 | 完整工具名 |
|--------------|-------------------|-----------|
| `search` | `search` | `mcp__docs__search` |
| `get.version` | `get_version` | `mcp__docs__get_version` |
| `one/two` | `one_two` | `mcp__serverX__one_two` |
| `with-dash` | `with_dash` | `mcp__serverX__with_dash` |

**碰撞检测**发生在 `mcpRegistry.connect` 里：注册前把新 server 的所有工具规范化后，与 `allFullNames()`（已有 client 全部工具的 fullName 集合）比对，撞名直接返回错误：

```
MCP tool name collision: "mcp__serverA__get_version" from server "serverB" (normalized from "get/version")
```

同时 `connect` 自己内部检测重复连接：同一个 server 名连两次直接 `"already connected"`。

**静态工具前缀保护**：`tools_test.go` 的 `TestAllToolsRegistered` 断言所有静态工具名**不得**占用 `mcp__` 前缀——否则会遮蔽 MCP fallback 分发（toolHandlers 先查）。

## 5. 权限策略详解

宿主侧三档策略：

| 级别 | 行为 | 典型用途 |
|------|------|---------|
| `allow` | 直接放行，不触发 stdin | docs/search、deploy/status 等确定只读 |
| `confirm` | 打印告警并询问 `Allow? [y/N]`（默认 N） | deploy/trigger 等破坏性操作 |
| `deny` | 硬拒，零打扰 | 明确禁止的工具 |

**默认行为**：`(server, tool)` 不在策略表里时，一律 `confirm`。这个兜底比 `allow` 更安全——新接入的未知工具宁可多问一次，也别静默放行破坏性操作。

**权限检查位置**：`permission.go` 的 `checkPermission` 函数顶部插入 MCP 分支（在 bash 三闸门之前），`PreToolUse` hook 调用它。confirm 级别直接复用 `askUser`（stdin 询问）。

### 测试里如何 stub stdin

`permission_test.go` 和 `mcp_test.go` 都有 `stubReadApproval` helper：

```go
func TestCheckPermissionMCPConfirm(t *testing.T) {
    called := stubReadApproval(t, "y\n")
    if !checkPermission("mcp__deploy__trigger", json.RawMessage(`{"service":"web-api"}`)) {
        t.Fatal("confirm + y should pass")
    }
    if !*called {
        t.Fatal("confirm level must ask the user")
    }
}
```

**永远不要让 loop 级测试走到 confirm 路径而不 stub readApproval**——go test 会真的阻塞在 stdin 上。

## 6. 输出截断

MCP server 返回的结果不受信任，可能非常大（文档搜索 50 条命中、日志拉取几万字符）。

`MCPClient.callTool` 里有与 read_file 同级的输出预算：

```go
// mcp.go
const maxMCPOutputRunes = 25000

if runes := []rune(result); len(runes) > maxMCPOutputRunes {
    result = string(runes[:maxMCPOutputRunes]) +
        fmt.Sprintf("\n... (MCP output truncated at %d chars, total %d)", maxMCPOutputRunes, len(runes))
}
```

截断返回给模型时会附带截断位置和总长，让模型知道内容被截了、有依据去请求更小的范围（比静默截断好）。

## 7. 错误路径

所有 MCP 边界错误都包装成 `tool_result` 回到模型，loop 不会中断：

| 场景 | tool_result 内容 | 原因 |
|------|-----------------|------|
| 未调用 `connect_mcp` 就调 `mcp__*` | `"MCP error: no MCP servers connected..."` | runTool fallback 检查 st.mcp == nil |
| 调用不存在的 server | `"MCP error: server "ghost" not connected"` | parseMCPName 成功但 registry 查不到 |
| 调用 server 不存在的工具 | `"MCP error: unknown tool "nope" on server "docs""` | MCPClient.origNames 查不到 |
| 缺必填参数 | `"MCP error: query is required"` (来自 server) | server.CallTool 内部校验 |
| server 返回错误 | `"MCP error: *errors.errorString: <msg>"` | CallTool 返回 error |
| 策略拒绝 | `"Permission denied"` | PreToolUse hook 拦截 |
| 名字无法解析 | `"Error: malformed tool name ..."` | runTool fallback 报错 |

模型可以用下一轮自我纠正——换个参数、先 connect、改调用对象都行。

## 8. 与子代理（task 工具）的交互

s14 改造了 `subTools` 的实现：**每轮重算**（与父 loop 的 `assembleToolPool(st)` 对齐），子代理共享父的 `sessionState`（包括 `mcp` 注册表）。

这意味着：
- 父会话里调过 `connect_mcp("docs")` 后，子代理也能直接调 `mcp__docs__search`
- 子代理自己调 `connect_mcp("weather")` 后，后续轮次也能看到 `mcp__weather__forecast`
- 但 `task` 工具本身不在子代理的工具池里（只允许一层委派），防止无限嵌套

## 9. 当前限制与后续扩展

| 限制 | 说明 | 扩展方向 |
|------|------|---------|
| 进程内 mock server | 不做真实网络通信 | 实现 stdio / SSE 传输层，桥接 MCPServer 接口 |
| 会话级注册表 | REPL 每轮新建 agentLoop，连接不跨会话 | 把 mcpRegistry 提升到 main 或 global session |
| 无并发保护 | 当前单 goroutine 访问安全 | MCP 调用后台化时加 sync.RWMutex |
| host 策略硬编码 | mcp_permission.go 里直接写表 | 改为从 YAML/JSON 配置文件加载 |
| 工具池无缓存 | 每轮 assembleToolPool 都重建切片 | 工具列表未变时缓存，用 channel signal connect 事件触发重建 |
| 无 unconnect_mcp | 只能连不能断 | 加一个 unconnect_mcp 工具清理 registry |

## 10. 运行测试

```bash
# 只跑 MCP 相关
go test -run "TestSafeToolName|TestParseMCPName|TestMCPRegistry|TestAssembleToolPool|TestRunToolMCP|TestConnectMCP|TestMCPPermission|TestCheckPermissionMCP" -v

# loop 级端到端 (包含 connect 后下一轮工具池变化验证)
go test -run TestAgentLoopMCPConnectThenCall -v

# 全量
go test ./...
```

MCP 新增的全部测试在 [mcp_test.go](../mcp_test.go)，覆盖：命名规范化、注册表连接/碰撞检测、工具池合并、runTool 分发（含错误路径）、connect_mcp handler（重复连接/未知 server）、host 策略三档（含确认路径的 stdin stub）、以及 loop 级端到端（用 `captureToolsClient` 记录每轮 Send 的工具列表）。
