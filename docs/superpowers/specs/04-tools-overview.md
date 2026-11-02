# 04 - 工具层概述

## 1. 职责

工具层是 Agent 与外部世界交互的桥梁，负责：

1. 定义所有工具的名称、参数 schema、描述
2. 将工具注册到统一注册表（`toolRegistry`）
3. 组装为 eino `tool.InvokableTool` 接口供 Agent 调用
4. 提供并发调度元数据（`concurrencySafe`、`resourceSet`）
5. 支持静态工具（19 个内置）和动态工具（MCP 发现）

## 2. 文件位置

```
pkg/tool/
├── registry.go          # 工具注册表 + AssembleToolPool
├── bash.go              # (在 tools/basic/bash.go) bash 工具 eino 适配
├── read.go              # (在 tools/basic/read.go) read_file eino 适配
├── write.go             # (在 tools/basic/write.go) write_file eino 适配
├── edit.go              # (在 tools/basic/edit.go) edit_file eino 适配
├── glob.go              # (在 tools/basic/glob.go) glob eino 适配
├── grep.go              # (在 tools/basic/grep.go) grep eino 适配
├── task.go              # (在 tools/agent/task.go) 任务系统 7 个工具 eino 适配
├── mcp/
│   ├── wrapper.go       # MCP 工具包装器 + connect_mcp
│   └── registry.go      # (逻辑在 mcp.go) MCP 注册表
└── session/
    └── session.go       # todo_write, load_skill, compact
```

实际 handler 逻辑位于项目根目录的 `.go` 文件中（`tools.go`、`bash.go`、`taskstore.go`、`mcp.go` 等），eino 适配层位于 `pkg/tool/` 子包中。

## 3. 依赖

- `github.com/cloudwego/eino/components/tool` — eino 工具接口定义
- `github.com/cloudwego/eino/components/tool/utils` — `utils.InferTool` 泛型工具构建器
- `github.com/cloudwego/eino/schema` — eino 消息/工具 schema
- `github.com/eino-contrib/jsonschema` — JSON Schema 解析（MCP 工具参数）
- `encoding/json` — JSON 序列化/反序列化

## 4. 数据结构

### 4.1 toolSpec — 工具内部元数据

```go
type toolSpec struct {
    handler         toolHandler                        // 处理器函数
    concurrencySafe bool                               // 是否可并发执行（默认 false）
    resources       func(args json.RawMessage) resourceSet // 返回涉及资源集合
}
```

**字段说明**:
- `handler`: 接收 `*sessionState` 和 `json.RawMessage`，返回 `string` 结果
- `concurrencySafe`: 只读工具（read_file, glob, grep）设为 `true`；写操作和全局操作设为 `false`
- `resources`: 静态分析工具涉及的文件路径，用于路径感知调度

### 4.2 resourceSet — 资源集合

```go
type resourceSet struct {
    reads  []string // 读资源路径列表（已规范化为绝对路径）
    writes []string // 写资源路径列表（已规范化为绝对路径）
    global bool     // 是否为全局资源（无法静态分析的工具）
}
```

**语义**:
- `global = true`: bash、task、MCP 等无法静态分析资源路径的工具
- `reads` 与 `writes` 互斥：一个工具调用要么只读，要么只写
- 路径安全：通过 `safePath()` 规范化；路径越界时 fallback 为 `global = true`

### 4.3 toolRegistry — 工具注册表

```go
var toolRegistry = map[string]*toolSpec{}
```

**注册时机**: `init()` 函数中完成所有内置工具注册。`task` 工具延迟到 `init()` 中赋值以避免包级初始化循环。

### 4.4 Handlers — eino 适配层处理器集合

```go
type Handlers struct {
    // Basic tools (6)
    BashHandler  func(args json.RawMessage) string
    ReadHandler  func(args json.RawMessage) string
    WriteHandler func(args json.RawMessage) string
    EditHandler  func(args json.RawMessage) string
    GlobHandler  func(args json.RawMessage) string
    GrepHandler  func(args json.RawMessage) string

    // Session tools (3)
    TodoWriteHandler func(args json.RawMessage) string
    LoadSkillHandler func(args json.RawMessage) string
    CompactHandler   func(args json.RawMessage) string

    // Agent tools (7)
    TaskHandler        func(args json.RawMessage) string
    CreateTaskHandler  func(args json.RawMessage) string
    UpdateTaskHandler  func(args json.RawMessage) string
    ClaimTaskHandler   func(args json.RawMessage) string
    CompleteTaskHandler func(args json.RawMessage) string
    GetTaskHandler     func(args json.RawMessage) string
    ListTasksHandler   func(args json.RawMessage) string

    // MCP tools (1)
    ConnectMCPHandler func(args json.RawMessage) string
}
```

### 4.5 MCPToolProvider — MCP 工具提供者接口

```go
type MCPToolProvider interface {
    GetMCPServerName() string
    GetMCPTools() []mcp.ToolDef
    GetMCPCaller() mcp.MCPCaller
}
```

## 5. 核心函数

### 5.1 AssembleToolPool()

```
函数签名: func AssembleToolPool(handlers Handlers, mcpProviders ...MCPToolProvider) []tool.InvokableTool

流程:
1. 创建空工具列表 tools := []tool.InvokableTool{}

2. 按固定顺序添加内置工具（顺序与测试期望对齐）:
   a. bash           → basic.NewBashTool(handlers.BashHandler)
   b. read_file      → basic.NewReadFileTool(handlers.ReadHandler)
   c. write_file     → basic.NewWriteFileTool(handlers.WriteHandler)
   d. edit_file      → basic.NewEditFileTool(handlers.EditHandler)
   e. glob           → basic.NewGlobTool(handlers.GlobHandler)
   f. grep           → basic.NewGrepTool(handlers.GrepHandler)
   g. todo_write     → session.NewTodoWriteTool(handlers.TodoWriteHandler)
   h. load_skill     → session.NewLoadSkillTool(handlers.LoadSkillHandler)
   i. task           → agent.NewTaskTool(handlers.TaskHandler)
   j. compact        → session.NewCompactTool(handlers.CompactHandler)
   k. create_task    → agent.NewCreateTaskTool(handlers.CreateTaskHandler)
   l. update_task    → agent.NewUpdateTaskTool(handlers.UpdateTaskHandler)
   m. claim_task     → agent.NewClaimTaskTool(handlers.ClaimTaskHandler)
   n. complete_task  → agent.NewCompleteTaskTool(handlers.CompleteTaskHandler)
   o. get_task       → agent.NewGetTaskTool(handlers.GetTaskHandler)
   p. list_tasks     → agent.NewListTasksTool(handlers.ListTasksHandler)
   q. connect_mcp    → mcp.NewConnectMCPTool(handlers.ConnectMCPHandler)

3. 遍历 mcpProviders，对每个 provider:
   a. 检查 provider.GetMCPCaller() != nil
   b. 调用 mcp.WrapMCPTools(serverName, caller, toolDefs)
   c. 将返回的 []tool.InvokableTool 追加到 tools

4. 返回 tools

错误处理:
- 每个 New*Tool 返回 (tool.InvokableTool, error)
- 创建失败时静默跳过（err != nil 则不 append）
```

### 5.2 eino 工具适配模式

每个内置工具遵循统一的适配模式：

```
函数签名: func New*Tool(handler func(args json.RawMessage) string) (tool.InvokableTool, error)

流程:
1. 调用 utils.InferTool[ArgsType, ResultType](name, description, runFunc)
2. runFunc 内部:
   a. 将强类型参数转为 map[string]interface{}
   b. json.Marshal 为 JSON bytes
   c. 调用 handler(jsonBytes) 获取 string 结果
   d. 包装为 ResultType 返回
```

**设计决策**: handler 统一接收 `json.RawMessage` 而非强类型参数，因为实际业务逻辑在 `tools.go` 等文件中实现，eino 适配层仅做协议桥接。

### 5.3 runTool() — 工具调度入口

```
函数签名: func runTool(st *sessionState, name string, input json.RawMessage) string

流程:
1. 查找静态注册表:
   - spec, ok := toolRegistry[name]
   - 如果找到 → 调用 spec.handler(st, input) 返回结果

2. MCP 工具 fallback:
   - 如果 isMCPTool(name)（前缀 "mcp__"）:
     a. 检查 st.mcp != nil，否则返回错误
     b. parseMCPName(name) 提取 server 和 safeTool
     c. 查找 st.mcp.clients[server]
     d. 调用 client.callTool(safeTool, input)

3. 未知工具:
   - 返回 "Error: unknown tool %q"
```

## 6. 工具命名规范

### 6.1 内置工具命名

- 使用小写蛇形命名：`read_file`、`write_file`、`edit_file`、`todo_write`、`create_task`
- 名称直接反映功能：动词_名词 格式
- 全局唯一，无前缀

### 6.2 MCP 工具命名

- 格式：`mcp__{server}__{tool}`
- 示例：`mcp__docs__search`、`mcp__deploy__trigger`
- 规范化规则：原始工具名中 `[^A-Za-z0-9_]` 字符替换为 `_`
- 碰撞检测：不同 server 规范化后若 fullName 相同则拒绝注册

### 6.3 参数格式

所有工具参数均为 JSON 对象，通过 `json.RawMessage` 传递。参数命名使用蛇形命名（`old_text`、`run_in_background`、`output_mode`）。

## 7. eino tool.InvokableTool 接口

```go
type InvokableTool interface {
    // Info 返回工具的元信息（名称、描述、参数 JSON Schema）
    Info(ctx context.Context) (*schema.ToolInfo, error)
    
    // InvokableRun 执行工具调用
    // argsJSON 是模型生成的参数 JSON 字符串
    // 返回结果字符串和可选错误
    InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error)
}
```

**schema.ToolInfo 结构**:
```go
type ToolInfo struct {
    Name        string          // 工具名
    Desc        string          // 工具描述
    ParamsOneOf *ParamsOneOf    // 参数 JSON Schema
}
```

`utils.InferTool[Args, Result]` 通过 Go 泛型自动从结构体 tag（`jsonschema`、`jsonschema_description`）生成 JSON Schema，并处理参数反序列化和结果序列化。

## 8. 19 个工具完整列表

### 8.1 基础工具（6 个）

| # | 工具名 | 功能 | concurrencySafe | 资源类型 |
|---|--------|------|-----------------|----------|
| 1 | `bash` | 执行 shell 命令 | false | global |
| 2 | `read_file` | 读取文件内容 | true | reads: [absPath] |
| 3 | `write_file` | 写入/创建文件 | false | writes: [absPath] |
| 4 | `edit_file` | 精确子串替换 | false | writes: [absPath] |
| 5 | `glob` | 文件名模式匹配搜索 | true | reads: [searchRoot] |
| 6 | `grep` | 正则内容搜索 | true | reads: [searchRoot] |

### 8.2 会话管理工具（3 个）

| # | 工具名 | 功能 | concurrencySafe | 资源类型 |
|---|--------|------|-----------------|----------|
| 7 | `todo_write` | 会话内待办清单管理 | false | global |
| 8 | `load_skill` | 加载技能文件 | false | global |
| 9 | `compact` | 触发上下文压缩 | false | global |

### 8.3 子代理工具（1 个）

| # | 工具名 | 功能 | concurrencySafe | 资源类型 |
|---|--------|------|-----------------|----------|
| 10 | `task` | 委派子代理执行任务 | false | global |

### 8.4 任务系统工具（6 个）

| # | 工具名 | 功能 | concurrencySafe | 资源类型 |
|---|--------|------|-----------------|----------|
| 11 | `create_task` | 创建持久化任务 | false | global |
| 12 | `update_task` | 更新任务依赖/属性 | false | global |
| 13 | `claim_task` | 认领任务 | false | global |
| 14 | `complete_task` | 完成任务 | false | global |
| 15 | `get_task` | 获取任务详情 | false | global |
| 16 | `list_tasks` | 列出所有任务 | false | global |

### 8.5 MCP 工具（1 + N 个）

| # | 工具名 | 功能 | concurrencySafe | 资源类型 |
|---|--------|------|-----------------|----------|
| 17 | `connect_mcp` | 连接 MCP 服务器 | false | global |
| 18+ | `mcp__{server}__{tool}` | MCP 发现的动态工具 | false | global |

**注**: MCP 工具数量取决于连接的 server 及其暴露的工具数。预定义 mock server 有 `docs`（2 个工具）和 `deploy`（2 个工具）。

## 9. 并发调度机制

### 9.1 调度流程

```
模型返回 N 个 tool_use blocks
    │
    ▼
executeToolBatch()
    │
    ├── 阶段 1: PreToolUse hook 串行检查（拦截的标记为 blocked）
    │
    ├── 阶段 2: greedyGroup() 贪心分组
    │   - 遍历未拦截的 tool_use
    │   - concurrencySafe=true 的工具可合并到同一组
    │   - concurrencySafe=false 的工具独占一组
    │   - 资源冲突（reads ∩ writes, writes ∩ writes, global）触发分组
    │
    ├── 阶段 3: 按组执行
    │   - 组间串行
    │   - 组内并发（信号量限制 maxToolConcurrency=8）
    │   - 结果按原始下标写入预分配数组
    │
    └── 阶段 4: PostToolUse hook 串行执行 + 终端打印
```

### 9.2 冲突判定规则

```
conflicts(a, b resourceSet) bool:
1. 任一方 global → 冲突
2. a.writes ∩ b.writes ≠ ∅ → 冲突（即使路径不同）
3. a.reads ∩ b.writes ≠ ∅ → 冲突（路径相同或读路径是写路径的子目录）
4. a.writes ∩ b.reads ≠ ∅ → 冲突（路径相同或写路径是读路径的子目录）
5. 其他情况 → 无冲突
```

## 10. 关键常量

```go
const maxToolConcurrency = 8  // 并发执行工具的上限
```

## 11. 测试要点

### 11.1 注册表测试
- 所有 17 个内置工具均已注册到 `toolRegistry`
- 每个工具的 `concurrencySafe` 值正确
- `resources()` 函数返回正确的资源集合

### 11.2 组装测试
- `AssembleToolPool()` 返回正确数量的工具
- 工具顺序与预期一致
- MCP provider 为空时不产生额外工具
- MCP provider 非空时工具名正确前缀化

### 11.3 调度测试
- 两个 read_file 调用可并发执行
- read_file + write_file 同路径时串行执行
- bash 调用与任何工具串行执行
- 超过 `maxToolConcurrency` 时信号量生效
- PreToolUse hook 拦截的工具不参与调度

### 11.4 错误处理测试
- 未知工具名返回错误消息
- MCP 工具在未连接 server 时返回错误
- handler panic 被 recover 捕获并转为错误结果
- ctx 取消时未启动的工具产出 "canceled" 结果

## 12. 注意事项

1. **handler 闭包捕获**: `assembleToolPool()` 创建 handler 闭包捕获 `sessionState`，确保每个 agentLoop 有独立的会话状态
2. **init() 注册**: `task` 工具必须在 `init()` 中赋值而非 map 字面量中，否则会产生包级初始化循环
3. **MCP 动态性**: MCP 工具在 `connect_mcp` 调用后才可用，每轮 Agent 调用前重新组装工具池
4. **路径安全**: `resources()` 函数中 `safePath()` 失败时 fallback 为 `global = true`，确保保守调度
