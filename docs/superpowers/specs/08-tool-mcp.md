# 08 - MCP 工具规格

## 1. 职责

MCP (Model Context Protocol) 工具层负责连接外部 MCP 服务器，动态发现并执行远程工具：

1. **连接管理** — 通过 `connect_mcp` 工具连接外部 MCP 服务器
2. **工具发现** — 调用 `ListTools()` 获取服务器暴露的全部工具定义
3. **命名规范化** — 将外部工具名映射为 `mcp__{server}__{tool}` 格式，避免碰撞
4. **调用路由** — 将模型的工具调用请求路由到对应的 MCP 服务器执行
5. **输出截断** — 对 MCP 服务器返回结果施加字符预算限制
6. **权限策略** — host-side 独立决定每个外部工具的放行策略
7. **指令注入** — 将服务器提供的使用说明注入 system prompt

**设计理念**:
- MCP server 提供的提示（readOnlyHint / destructiveHint）不可作为授权依据
- 宿主 (harness) 侧独立决定权限策略
- 未配置的外部工具一律 require confirmation（最坏情况下用户被 prompt）

## 2. 文件位置

```
pkg/mcp/
├── wrapper.go         # mcpToolWrapper 实现 eino InvokableTool 接口
├── connect.go         # connect_mcp 工具 eino 适配
└── types.go           # ToolDef, MCPCaller 接口定义

mcp.go                 # MCPServer 接口, MCPClient, mcpRegistry, connectMCPHandler
mcp_servers.go         # 进程内 mock MCP server (docsServer, deployServer)
mcp_permission.go      # host-side 权限策略表
```

## 3. 依赖

- `encoding/json` — JSON 序列化/反序列化
- `regexp` — 工具名规范化
- `sort` — server 名排序（指令段输出稳定）
- `strings` — 字符串处理
- `fmt` — 格式化
- `context` — 上下文传递
- `github.com/cloudwego/eino/components/tool` — eino 工具接口
- `github.com/cloudwego/eino/components/tool/utils` — `utils.InferTool` 泛型构建器
- `github.com/cloudwego/eino/schema` — eino 消息/工具 schema
- `github.com/eino-contrib/jsonschema` — JSON Schema 解析（MCP 工具参数）

## 4. 数据结构

### 4.1 MCPServer — 服务端接口

```go
type MCPServer interface {
    // ListTools 返回服务器暴露的全部工具定义
    ListTools() ([]ToolDef, error)
    
    // CallTool 执行指定工具调用
    // name: server 内部的原始工具名（非 mcp__ 前缀化名称）
    // args: 模型提供的参数 map
    // 返回: 工具执行结果文本 或 错误
    CallTool(name string, args map[string]any) (string, error)
    
    // Instructions 返回 server 级使用说明
    // 等价于 MCP 协议的 instructions 字段
    // 返回空字符串表示该 server 没有额外说明
    Instructions() string
}
```

**设计说明**: 真实 MCP 传输层 (stdio/SSE) 通过实现此接口接入。教学版使用进程内 mock server。

### 4.2 ToolDef — 工具定义

```go
type ToolDef struct {
    Name        string         `json:"name"`        // server 内部的原始工具名
    Description string         `json:"description"` // 给模型看的用途说明
    InputSchema map[string]any `json:"inputSchema"` // JSON Schema (type=object)
}
```

InputSchema 格式示例:
```json
{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Search keyword or phrase"},
    "max_hits": {"type": "integer", "description": "Maximum results to return (default 5)"}
  },
  "required": ["query"]
}
```

### 4.3 MCPClient — 客户端

```go
type MCPClient struct {
    server     MCPServer          // 持有的 MCPServer 实例
    serverName string             // 注册时的 server 名（规范化后）
    tools      []ToolDef          // 发现的工具定义列表
    origNames  map[string]string  // safeName → 原始 server-side 名（CallTool 时回查）
}
```

字段说明:
- `server`: MCPServer 接口实例，用于实际的工具调用
- `serverName`: 规范化后的服务器名，用于构建 `mcp__{server}__{tool}` 前缀
- `tools`: ListTools() 返回的全部工具定义，缓存后不再变化
- `origNames`: 映射表，key 是规范化后的工具名，value 是 server 内部原始名。CallTool 时需要用原始名调用 server

### 4.4 mcpRegistry — 会话级注册表

```go
type mcpRegistry struct {
    clients map[string]*MCPClient  // key = server 名（规范化后）
}
```

mcpRegistry 管理所有已连接的 MCPClient，生命周期与 sessionState 绑定。

### 4.5 MCPCaller — eino 适配层调用接口

```go
// 位于 tools/mcp/wrapper.go
type MCPCaller interface {
    CallTool(ctx context.Context, name string, argsJSON json.RawMessage) (string, error)
}
```

MCPClient 实现此接口，供 eino 工具包装器使用。

### 4.6 mcpToolWrapper — eino InvokableTool 适配

```go
type mcpToolWrapper struct {
    name   string      // 完整工具名: mcp__{server}__{tool}
    desc   string      // 工具描述
    client MCPCaller   // MCP 调用器
    tool   ToolDef     // 原始工具定义
}
```

实现 `tool.InvokableTool` 接口:
- `Info(ctx)` → 返回 `*schema.ToolInfo`，包含名称、描述和 JSON Schema 参数定义
- `InvokableRun(ctx, argsJSON, opts...)` → 调用 `client.CallTool()` 执行工具

### 4.7 MCPToolProvider — 工具提供者接口

```go
type MCPToolProvider interface {
    GetMCPServerName() string
    GetMCPTools() []mcp.ToolDef
    GetMCPCaller() mcp.MCPCaller
}
```

用于在 `AssembleToolPool()` 中收集 MCP 动态工具。`mcpProvider` 是其在主包中的实现:

```go
type mcpProvider struct {
    client *MCPClient
}
```

### 4.8 mcpPermissionLevel — 权限策略级别

```go
type mcpPermissionLevel string

const (
    mcpPermAllow   mcpPermissionLevel = "allow"    // 放行，无需确认
    mcpPermConfirm mcpPermissionLevel = "confirm"   // 询问用户（默认）
    mcpPermDeny    mcpPermissionLevel = "deny"      // 硬拒绝
)
```

### 4.9 connect_mcp 工具参数

```go
type ConnectMCPArgs struct {
    Server string `json:"server" jsonschema:"required" jsonschema_description:"MCP server name"`
}
```

## 5. 关键常量

```go
const (
    maxMCPOutputRunes = 25000  // MCP 工具单次返回的字符预算（对齐 maxReadOutputRunes）
)
```

### 5.1 工具命名规范化正则

```go
var mcpNameSanitizer = regexp.MustCompile(`[^A-Za-z0-9_]`)
```

将工具名中所有非 `[A-Za-z0-9_]` 字符替换为 `_`。

### 5.2 host-side 权限策略表

```go
var mcpHostPolicy = map[string]mcpPermissionLevel{
    // docs server —— 全是只读操作
    "docs/search":      mcpPermAllow,
    "docs/get_version": mcpPermAllow,
    
    // deploy server —— status 只读, trigger 破坏性
    "deploy/status":  mcpPermAllow,
    "deploy/trigger": mcpPermConfirm,
}
```

未命中的工具一律返回 `mcpPermConfirm`（兜底策略）。

### 5.3 mock server 工厂注册表

```go
var mockMCPFactories = map[string]func() MCPServer{
    "docs":   newDocsServer,
    "deploy": newDeployServer,
}
```

教学版使用进程内工厂函数代表真实服务。生产版从配置文件/命令行读取 (name → stdio command / SSE URL)。

## 6. 核心函数

### 6.1 newMCPClient()

```
函数签名: func newMCPClient(name string, srv MCPServer) (*MCPClient, error)

流程:
1. 调用 srv.ListTools() 发现全部工具
   - 失败 → 返回 nil, fmt.Errorf("list tools: %w", err)

2. 构建 MCPClient:
   c := &MCPClient{
     server:     srv,
     serverName: name,
     tools:      defs,
     origNames:  make(map[string]string),
   }

3. 返回 c, nil

注意: 此时只完成工具发现，不做碰撞检测。碰撞检测在 mcpRegistry.connect() 中执行。
```

### 6.2 safeToolName()

```
函数签名: func safeToolName(raw string) string

流程:
1. 使用正则 mcpNameSanitizer 替换所有非 [A-Za-z0-9_] 字符为 _
2. 返回替换后的字符串

示例:
- "search" → "search"
- "get-version" → "get_version"
- "send.email" → "send_email"
```

### 6.3 fullName()

```
函数签名: func fullName(server, tool string) string

流程:
1. 拼接: "mcp__" + server + "__" + tool
2. 返回完整工具名

示例:
- fullName("docs", "search") → "mcp__docs__search"
- fullName("deploy", "trigger") → "mcp__deploy__trigger"
```

### 6.4 MCPClient.callTool()

```
函数签名: func (c *MCPClient) callTool(safeTool string, argsJSON json.RawMessage) string

流程:
1. 回查原始工具名:
   orig, ok := c.origNames[safeTool]
   - !ok → 返回 "MCP error: unknown tool {safeTool} on server {serverName}"

2. 解析参数:
   args := map[string]any{}
   if len(argsJSON) > 0:
     json.Unmarshal(argsJSON, &args)
     - 失败 → 返回 "MCP error: invalid arguments: {err}"

3. 调用 server:
   result, err := c.server.CallTool(orig, args)
   - err != nil → 返回 "MCP error: {errorType}: {err}"
     注: 使用 %T 格式化错误类型，便于调试

4. 输出截断:
   runes := []rune(result)
   if len(runes) > maxMCPOutputRunes (25000):
     result = string(runes[:25000]) +
       "\n... (MCP output truncated at 25000 chars, total {len})"

5. 返回 result

设计决策: 与 read_file/bash 同级的输出预算，MCP server 返回不受信，
超长结果必须截断，避免单次调用吃掉整个上下文窗口。
```

### 6.5 MCPClient.CallTool() — eino 适配层入口

```
函数签名: func (c *MCPClient) CallTool(ctx context.Context, name string, argsJSON json.RawMessage) (string, error)

流程:
1. 调用 c.callTool(name, argsJSON) 获取结果字符串
2. 检查结果是否以 "MCP error:" 开头:
   - 是 → 返回 result, errors.New(result)
   - 否 → 返回 result, nil

设计说明: callTool 内部已处理所有错误并格式化为字符串，
此方法仅做 error 包装以满足 MCPCaller 接口。
```

### 6.6 newMCPRegistry()

```
函数签名: func newMCPRegistry() *mcpRegistry

流程:
1. 返回 &mcpRegistry{clients: map[string]*MCPClient{}}
```

### 6.7 mcpRegistry.connect()

```
函数签名: func (r *mcpRegistry) connect(serverName string, factory func() MCPServer) error

流程:
1. 检查重复连接:
   - r.clients[serverName] 已存在 → 返回 error("MCP server {name} already connected")

2. 实例化 server:
   srv := factory()

3. 创建客户端（含工具发现）:
   client, err := newMCPClient(serverName, srv)
   - 失败 → 返回 error

4. 碰撞检测:
   existing := r.allFullNames()  // 获取所有已注册工具的 fullName 集合
   for _, t := range client.tools:
     safe := safeToolName(t.Name)
     fn := fullName(serverName, safe)
     if existing[fn]:
       返回 error("MCP tool name collision: {fn} from server {serverName} (normalized from {t.Name})")
     client.origNames[safe] = t.Name  // 记录 safeName → 原始名映射
     existing[fn] = true

5. 注册客户端:
   r.clients[serverName] = client

6. 返回 nil

碰撞检测范围:
- 同一 server 内部工具规范化后不能撞名
- 新 server 的工具不能与已有 server 的工具撞名
```

### 6.8 mcpRegistry.allFullNames()

```
函数签名: func (r *mcpRegistry) allFullNames() map[string]bool

流程:
1. 遍历所有已注册的 client
2. 对每个 client 的每个 tool:
   fn := fullName(client.serverName, safeToolName(tool.Name))
   out[fn] = true
3. 返回 out
```

### 6.9 mcpRegistry.assembleToolDefs()

```
函数签名: func (r *mcpRegistry) assembleToolDefs() ([]anthropic.ToolUnionParam, map[string]*MCPClient)

流程:
1. 初始化:
   var defs []anthropic.ToolUnionParam
   dispatch := map[string]*MCPClient{}

2. 遍历所有 client:
   for _, c := range r.clients:
     for _, t := range c.tools:
       a. safe := safeToolName(t.Name)
       b. fn := fullName(c.serverName, safe)
       c. 处理 InputSchema:
          - schema == nil → 使用空 schema: {"type": "object", "properties": {}}
          - 提取 required 字段（兼容 []string 和 []any 两种类型）
       d. 构建 anthropic.ToolParam:
          tp := anthropic.ToolParam{
            Name:        fn,
            Description: anthropic.String(t.Description),
            InputSchema: anthropic.ToolInputSchemaParam{
              Type:       "object",
              Properties: schema["properties"],
              Required:   required,
            },
          }
       e. defs = append(defs, anthropic.ToolUnionParam{OfTool: &tp})
       f. dispatch[fn] = c  // 记录 fullName → client 映射，供 runTool 路由

3. 返回 defs, dispatch
```

### 6.10 mcpRegistry.instructionsSection()

```
函数签名: func (r *mcpRegistry) instructionsSection() string

流程:
1. 收集有非空 instructions 的 server 名:
   names := []string{}
   for name, c := range r.clients:
     if strings.TrimSpace(c.server.Instructions()) != "":
       names = append(names, name)

2. 无非空说明 → 返回 ""（systemPrompt 据此跳过整个 Block）

3. 按 server 名排序（保证输出稳定，利于测试与缓存）:
   sort.Strings(names)

4. 构建 Markdown 格式输出:
   # MCP Server Instructions
   
   The following MCP servers have provided instructions for how to use their tools and resources:
   
   ## {name1}
   {instructions1}
   
   ## {name2}
   {instructions2}

5. 返回构建的文本
```

### 6.11 connectMCPHandler()

```
函数签名: func connectMCPHandler(st *sessionState, args json.RawMessage) string

流程:
1. 解析参数:
   var in struct { Name string `json:"name"` }
   json.Unmarshal(args, &in)
   - 失败 → "Error: invalid arguments: {err}"

2. 校验 name 非空:
   - in.Name == "" → "Error: name is required"

3. 初始化 MCP 注册表（首次连接时）:
   - st.mcp == nil → st.mcp = newMCPRegistry()

4. 检查重复连接:
   - st.mcp.hasClient(in.Name) → "MCP server {name} already connected"

5. 查找 server 工厂:
   factory, ok := mockMCPFactories[in.Name]
   - !ok → 列出可用 server 名:
     "Unknown MCP server {name}. Available: {comma-separated names}"

6. 连接 server:
   err := st.mcp.connect(in.Name, factory)
   - 失败 → "Error connecting MCP server: {err}"

7. 构建成功响应:
   client := st.mcp.clients[in.Name]
   列出新注册的工具:
   "Connected MCP server {name}. New tools available:
     - mcp__{name}__{tool1}  ({description1})
     - mcp__{name}__{tool2}  ({description2})
   You can now invoke these tools directly."

8. 返回响应文本
```

### 6.12 isMCPTool()

```
函数签名: func isMCPTool(name string) bool

流程:
1. 返回 strings.HasPrefix(name, "mcp__")
```

### 6.13 parseMCPName()

```
函数签名: func parseMCPName(name string) (server, safeTool string, ok bool)

流程:
1. 检查前缀:
   - !strings.HasPrefix(name, "mcp__") → return "", "", false

2. 去掉前缀:
   trimmed := strings.TrimPrefix(name, "mcp__")

3. 按 "__" 分割为两段:
   parts := strings.SplitN(trimmed, "__", 2)
   - len(parts) != 2 → return "", "", false
   - parts[0] == "" → return "", "", false（server 名为空）
   - parts[1] == "" → return "", "", false（tool 名为空）

4. 返回 parts[0], parts[1], true

示例:
- "mcp__docs__search" → ("docs", "search", true)
- "mcp__deploy__trigger" → ("deploy", "trigger", true)
- "mcp____search" → ("", "search", false)（server 名为空）
- "bash" → ("", "", false)（非 MCP 工具）
```

### 6.14 mcpPermission()

```
函数签名: func mcpPermission(server, safeTool string) mcpPermissionLevel

流程:
1. 构建查找 key: server + "/" + safeTool
2. 查 mcpHostPolicy 表:
   - 命中 → 返回对应策略级别
   - 未命中 → 返回 mcpPermConfirm（兜底: 未配置的外部工具一律确认）

示例:
- mcpPermission("docs", "search") → mcpPermAllow
- mcpPermission("deploy", "trigger") → mcpPermConfirm
- mcpPermission("unknown", "tool") → mcpPermConfirm（兜底）
```

### 6.15 WrapMCPTools() — eino 适配层

```
函数签名: func WrapMCPTools(serverName string, client MCPCaller, tools []ToolDef) []tool.InvokableTool

流程:
1. 遍历 tools:
   for _, t := range tools:
     a. safe := safeToolName(t.Name)
     b. fn := fullName(serverName, safe)
     c. 构建 mcpToolWrapper:
        wrapper := &mcpToolWrapper{
          name:   fn,
          desc:   t.Description,
          client: client,
          tool:   t,
        }
     d. 追加到结果列表

2. 返回 []tool.InvokableTool
```

### 6.16 mcpToolWrapper.Info()

```
函数签名: func (w *mcpToolWrapper) Info(ctx context.Context) (*schema.ToolInfo, error)

流程:
1. 处理 InputSchema:
   - w.tool.InputSchema != nil:
     a. json.Marshal(w.tool.InputSchema) → schemaJSON
     b. json.Unmarshal(schemaJSON, &js) → jsonschema.Schema
     c. schema.NewParamsOneOfByJSONSchema(&js) → paramsOneOf
   - w.tool.InputSchema == nil:
     paramsOneOf = nil

2. 返回 &schema.ToolInfo{
     Name:        w.name,
     Desc:        w.desc,
     ParamsOneOf: paramsOneOf,
   }, nil
```

### 6.17 mcpToolWrapper.InvokableRun()

```
函数签名: func (w *mcpToolWrapper) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error)

流程:
1. 调用 w.client.CallTool(ctx, w.tool.Name, json.RawMessage(argsJSON))
2. 返回 result, err

注意: 这里传给 CallTool 的是 w.tool.Name（原始名），
由 MCPClient.CallTool 内部做 safeName → origName 的回查。
```

### 6.18 runTool() 中的 MCP 路由

```
函数签名: func runTool(st *sessionState, name string, input json.RawMessage) string

MCP 工具路由流程（在静态注册表查找失败后）:
1. 检查 isMCPTool(name):
   - false → 返回 "Error: unknown tool {name}"

2. 检查 st.mcp != nil:
   - nil → 返回 "MCP error: no MCP servers connected — call connect_mcp first"

3. 解析工具名:
   server, safeTool, ok := parseMCPName(name)
   - !ok → 返回 "MCP error: malformed tool name {name} (expected mcp__{server}__{tool})"

4. 查找 client:
   client, exists := st.mcp.clients[server]
   - !exists → 返回 "MCP error: server {server} not connected"

5. 调用工具:
   return client.callTool(safeTool, input)
```

## 7. 工具发现流程

### 7.1 完整发现流程

```
1. 模型调用 connect_mcp(name="docs")
       │
       ▼
2. connectMCPHandler() 查找 mockMCPFactories["docs"]
       │
       ▼
3. mcpRegistry.connect("docs", factory)
       │
       ├── factory() → 创建 docsServer 实例
       │
       ├── newMCPClient("docs", server)
       │     │
       │     └── server.ListTools() → []ToolDef{
       │           {Name: "search", Description: "...", InputSchema: {...}},
       │           {Name: "get_version", Description: "...", InputSchema: {...}},
       │         }
       │
       ├── 碰撞检测:
       │     safeToolName("search") → "search"
       │     fullName("docs", "search") → "mcp__docs__search"
       │     检查是否与已有工具名冲突
       │
       └── 注册: r.clients["docs"] = client
       │
       ▼
4. 返回成功消息:
   "Connected MCP server \"docs\". New tools available:
     - mcp__docs__search  (Search the documentation...)
     - mcp__docs__get_version  (Return the current API version...)
   You can now invoke these tools directly."
       │
       ▼
5. 下一轮 Agent Loop:
   assembleToolPool(st) → 遍历 st.mcp.clients → WrapMCPTools() → 
   新工具加入工具池 → 模型可见并调用
```

### 7.2 工具池组装中的 MCP 集成

```
函数签名: func assembleToolPool(st *sessionState) []tool.InvokableTool

MCP 集成部分:
1. 收集 MCP 工具提供者:
   var mcpProviders []tools.MCPToolProvider
   if st != nil && st.mcp != nil:
     for _, c := range st.mcp.clients:
       mcpProviders = append(mcpProviders, &mcpProvider{client: c})

2. 传入 AssembleToolPool:
   return tools.AssembleToolPool(handlers, mcpProviders...)

3. AssembleToolPool 内部:
   for _, provider := range mcpProviders:
     if provider.GetMCPCaller() != nil:
       mcpTools := mcp.WrapMCPTools(
         provider.GetMCPServerName(),
         provider.GetMCPCaller(),
         provider.GetMCPTools(),
       )
       tools = append(tools, mcpTools...)
```

## 8. 工具调用流程

### 8.1 完整调用链路

```
模型请求: tool_use { name: "mcp__docs__search", input: {query: "hooks"} }
       │
       ▼
Agent Loop: executeToolBatch()
       │
       ▼
runTool(st, "mcp__docs__search", inputJSON)
       │
       ├── 1. 查静态注册表 toolRegistry["mcp__docs__search"]
       │      → 未找到（MCP 动态工具不在静态表中）
       │
       ├── 2. isMCPTool("mcp__docs__search") → true
       │
       ├── 3. parseMCPName("mcp__docs__search")
       │      → server="docs", safeTool="search", ok=true
       │
       ├── 4. st.mcp.clients["docs"] → client
       │
       └── 5. client.callTool("search", inputJSON)
              │
              ├── orig = client.origNames["search"] → "search"
              │   （docsServer 的原始工具名恰好与 safe 名相同）
              │
              ├── args = json.Unmarshal(inputJSON) → map[string]any{"query": "hooks"}
              │
              ├── client.server.CallTool("search", args)
              │   → docsServer.CallTool("search", {"query": "hooks"})
              │   → 执行 mock 文档搜索
              │   → 返回结果文本
              │
              ├── 输出截断: len(runes) > 25000 → 截断
              │
              └── 返回结果字符串
```

### 8.2 eino 适配层调用链路

当通过 eino 框架调用时:

```
eino ChatModel: tool_use { name: "mcp__docs__search", input: {...} }
       │
       ▼
mcpToolWrapper.InvokableRun(ctx, argsJSON)
       │
       ▼
w.client.CallTool(ctx, w.tool.Name, json.RawMessage(argsJSON))
       │
       ▼
MCPClient.CallTool(ctx, "search", argsJSON)
       │
       ├── c.callTool("search", argsJSON) → result string
       │
       └── 检查 "MCP error:" 前缀
           - 有 → return result, error
           - 无 → return result, nil
```

## 9. MCP Server 生命周期管理

### 9.1 创建

```
时机: connect_mcp 工具被模型调用时
方式: mockMCPFactories[name]() 调用工厂函数创建 server 实例
```

### 9.2 发现

```
时机: newMCPClient() 内部
方式: srv.ListTools() 获取全部工具定义
缓存: 工具列表缓存在 MCPClient.tools 中，不再重复调用
```

### 9.3 注册

```
时机: mcpRegistry.connect() 内部
碰撞检测: 所有工具的 fullName 不得与已有工具冲突
映射记录: safeName → origName 映射存入 client.origNames
```

### 9.4 使用

```
时机: 每轮 Agent Loop
工具池刷新: assembleToolPool() 每轮重新组装，包含最新 MCP 工具
调用路由: runTool() 通过 mcp__ 前缀识别并路由到对应 client
```

### 9.5 销毁

```
时机: sessionState 生命周期结束
方式: mcpRegistry 随 sessionState 一起释放，无显式 Close()
```

**注**: 教学版 mock server 无需显式关闭。真实 MCP 传输层 (stdio/SSE) 实现 MCPServer 接口时需添加 Close() 方法并在适当时机调用。

## 10. 进程内 Mock MCP Server

### 10.1 docsServer — 文档搜索 + 版本查询

```go
type docsServer struct{}

func newDocsServer() MCPServer { return &docsServer{} }
```

**Instructions**:
```
This server exposes product documentation search and version lookup.
Prefer mcp__docs__search over grep when you need API behavior or documented conventions,
and use mcp__docs__get_version when the answer depends on the documented API version.
```

**工具列表**:

| 工具名 | 描述 | 参数 |
|--------|------|------|
| search | 按关键词搜索文档知识库 | query (string, required), max_hits (integer, optional, default 5) |
| get_version | 返回当前文档 API 版本和更新时间 | 无参数 |

**search 返回格式**:
```
Found {N} results for "{query}":

1. [{section}] {title}
   {snippet}

2. [{section}] {title}
   {snippet}
```

**get_version 返回**:
```
Documentation API: v2.3.1
Last updated: 2026-10-15
Docs endpoint: https://docs.example.com/api/v2
Supported SDKs: Go, Python, TypeScript
```

**mock 数据**: 内置 10 条文档条目，涵盖 Hooks、Permission、MCP、Architecture 等分类。搜索逻辑为简单的 `strings.Contains` 关键词匹配（不区分大小写）。

### 10.2 deployServer — 服务状态查询 + 部署触发

```go
type deployServer struct {
    triggered bool  // 有状态：trigger 后变为 true
}

func newDeployServer() MCPServer { return &deployServer{} }
```

**Instructions**:
```
This server inspects and changes shared deployment state.
Calling mcp__deploy__trigger restarts a running service, so confirm the target service
and image with the user before triggering, and use mcp__deploy__status to verify the outcome.
```

**工具列表**:

| 工具名 | 描述 | 参数 |
|--------|------|------|
| status | 查询服务当前部署状态 | service (string, optional, default "web-api") |
| trigger | 触发服务部署（破坏性操作） | service (string, optional), image (string, optional, default "latest") |

**status 返回**:
- triggered=false:
  ```
  Service: {service}
  Status:  RUNNING
  Version: 2.3.1
  Health:  OK
  Uptime:  14d 6h
  Instances: 3/3 healthy
  ```
- triggered=true:
  ```
  Service: {service}
  Status:  DEPLOYING (rolling restart in progress)
  Version: 2.4.0 (pending)
  Health:  transitioning
  Uptime:  0m
  ```

**trigger 返回**:
```
Deployment triggered for {service}
Image:    {image}
Strategy: rolling
Expected: ~90s
Call status again to poll progress.
```

**权限策略**: status → allow, trigger → confirm（破坏性操作需用户确认）

## 11. 工具命名规范

### 11.1 命名格式

```
mcp__{server}__{tool}
```

- `mcp__`: 固定前缀，标识此为 MCP 动态工具
- `{server}`: 服务器名（规范化后），如 `docs`、`deploy`
- `__`: 分隔符
- `{tool}`: 工具名（规范化后），如 `search`、`get_version`

### 11.2 规范化规则

```
原始名 → safeToolName() → 规范化名

规则: [^A-Za-z0-9_] → _

示例:
- "search" → "search"（无需变化）
- "get-version" → "get_version"（连字符替换）
- "send.email" → "send_email"（点号替换）
- "my tool" → "my_tool"（空格替换）
```

### 11.3 碰撞检测

```
碰撞条件: 两个不同来源的工具规范化后 fullName 相同

检测范围:
1. 同一 server 内部: 不同原始工具名规范化后不能撞名
2. 跨 server: 新 server 的工具不能与已有 server 的工具撞名
3. 与内置工具: MCP 工具名以 mcp__ 开头，不会与内置工具（无前缀）碰撞

碰撞时行为: 拒绝注册整个 server，返回错误
```

### 11.4 解析

```
parseMCPName("mcp__docs__search")
  → server="docs", safeTool="search", ok=true

parseMCPName("mcp__my_server__get-version")
  → server="my_server", safeTool="get_version"（注意: 这里 safeTool 是规范化后的）
  
parseMCPName("bash")
  → ok=false（非 MCP 工具）
```

## 12. 与 eino 的集成

### 12.1 集成架构

```
┌─────────────────────────────────────────────────────┐
│                   eino ChatModel                     │
│  (发送 tool_use → 接收 tool_result)                  │
├─────────────────────────────────────────────────────┤
│              tool.InvokableTool 接口                  │
├────────────┬────────────────┬───────────────────────┤
│  内置工具   │  会话工具       │  MCP 动态工具          │
│  (basic/)  │  (session/)    │  (mcp/wrapper.go)     │
│            │                │                       │
│  bash      │  todo_write    │  mcpToolWrapper       │
│  read_file │  load_skill    │   ├─ Info()           │
│  write_file│  compact       │   └─ InvokableRun()   │
│  edit_file │                │       ↓               │
│  glob      │                │  MCPClient.CallTool() │
│  grep      │                │       ↓               │
│            │                │  MCPServer.CallTool() │
├────────────┴────────────────┴───────────────────────┤
│              AssembleToolPool()                      │
│  内置工具 + MCP 工具 → []tool.InvokableTool          │
└─────────────────────────────────────────────────────┘
```

### 12.2 工具池组装

每轮 Agent Loop 调用 `assembleToolPool(st)` 重新组装工具池:

```
1. 创建 Handlers 结构体（闭包捕获 sessionState）
2. 收集 MCP 工具提供者（从 st.mcp.clients）
3. 调用 tools.AssembleToolPool(handlers, mcpProviders...)
4. 返回完整工具列表
```

### 12.3 JSON Schema 桥接

MCP 工具的 InputSchema 使用标准 JSON Schema 格式，需要转换为 eino 的 `schema.ParamsOneOf`:

```
MCP InputSchema (map[string]any)
    │
    ▼ json.Marshal → JSON bytes
    │
    ▼ json.Unmarshal → jsonschema.Schema
    │
    ▼ schema.NewParamsOneOfByJSONSchema(&js)
    │
    ▼ *schema.ParamsOneOf
```

### 12.4 MCP 工具的 eino 注册

```go
// WrapMCPTools 将 MCP 工具列表转换为 eino 工具列表
func WrapMCPTools(serverName string, client MCPCaller, tools []ToolDef) []tool.InvokableTool
```

每个 MCP 工具被包装为 `mcpToolWrapper`，实现 `tool.InvokableTool` 接口:
- `Info()` 返回工具元信息（名称、描述、JSON Schema 参数定义）
- `InvokableRun()` 调用 `client.CallTool()` 执行工具

## 13. 错误处理

### 13.1 connect_mcp 错误

| 错误场景 | 返回格式 |
|---------|---------|
| name 参数为空 | "Error: name is required" |
| server 名未知 | "Unknown MCP server {name}. Available: {names}" |
| 重复连接 | "MCP server {name} already connected" |
| ListTools 失败 | "Error connecting MCP server: list tools: {err}" |
| 工具名碰撞 | "Error connecting MCP server: MCP tool name collision: {fullName}" |

### 13.2 callTool 错误

| 错误场景 | 返回格式 |
|---------|---------|
| 无 MCP 注册表 | "MCP error: no MCP servers connected — call connect_mcp first" |
| 工具名格式错误 | "MCP error: malformed tool name {name} (expected mcp__{server}__{tool})" |
| server 未连接 | "MCP error: server {server} not connected" |
| 工具名未知 | "MCP error: unknown tool {safeTool} on server {server}" |
| 参数解析失败 | "MCP error: invalid arguments: {err}" |
| server 执行失败 | "MCP error: {errorType}: {err}" |
| 输出超长 | 截断到 25000 字符 + "... (MCP output truncated at 25000 chars, total {N})" |

### 13.3 Mock Server 错误

| 错误场景 | 返回格式 |
|---------|---------|
| docs search query 为空 | error("query is required") |
| 未知工具名 | error("unknown docs tool {name}" / "unknown deploy tool {name}") |

## 14. 并发安全

- mcpRegistry 的 clients map 在 connect 时写入，在 assembleToolPool/callTool 时读取
- 建议: connect 操作由 Agent Loop 串行化（模型不会在同一轮并发调用 connect_mcp）
- MCPClient.callTool() 是无状态的（只读 origNames map + 调用 server），可安全并发
- Mock server 中 deployServer.triggered 是有状态的，并发 trigger 可能导致竞态
- 真实 MCP 传输层应保证 CallTool 的线程安全性

## 15. 测试要点

### 15.1 单元测试

1. **safeToolName**:
   - 纯字母数字 → 不变
   - 含连字符 → 替换为 _
   - 含点号 → 替换为 _
   - 含空格 → 替换为 _
   - 混合特殊字符 → 全部替换

2. **fullName**:
   - 正常拼接: fullName("docs", "search") → "mcp__docs__search"
   - server 名含下划线: fullName("my_server", "tool") → "mcp__my_server__tool"

3. **parseMCPName**:
   - 正常解析: "mcp__docs__search" → ("docs", "search", true)
   - 非 MCP 工具: "bash" → ("", "", false)
   - 缺少 tool 名: "mcp__docs__" → ("", "", false)
   - 缺少分隔符: "mcp__docs" → ("", "", false)
   - 空 server 名: "mcp____search" → ("", "", false)

4. **isMCPTool**:
   - "mcp__docs__search" → true
   - "bash" → false
   - "mcp_" → false
   - "" → false

5. **newMCPClient**:
   - ListTools 成功 → client.tools 正确填充
   - ListTools 失败 → error

6. **MCPClient.callTool**:
   - 正常调用 → 返回结果
   - 未知 safeTool → "MCP error: unknown tool"
   - 参数解析失败 → "MCP error: invalid arguments"
   - server 执行失败 → "MCP error: {type}: {err}"
   - 输出超长 → 截断 + 提示

7. **mcpRegistry.connect**:
   - 正常连接 → 注册成功
   - 重复连接 → error
   - 碰撞检测 → error
   - ListTools 失败 → error

8. **mcpPermission**:
   - 已配置 allow → mcpPermAllow
   - 已配置 confirm → mcpPermConfirm
   - 未配置 → mcpPermConfirm（兜底）

### 15.2 Mock Server 测试

1. **docsServer**:
   - search 正常搜索 → 返回格式化结果
   - search 无结果 → "No documentation found for {query}"
   - search query 为空 → error
   - get_version → 返回固定版本信息
   - 未知工具 → error

2. **deployServer**:
   - status 初始 → RUNNING
   - trigger → 返回部署消息
   - status 触发后 → DEPLOYING
   - 自定义 service/image 参数

### 15.3 集成测试

1. **端到端发现流程**:
   - connect_mcp("docs") → 工具池新增 2 个工具
   - 模型调用 mcp__docs__search → 正确路由到 docsServer
   - 返回结果正确

2. **多 server 共存**:
   - connect_mcp("docs") + connect_mcp("deploy")
   - 两个 server 的工具同时可用
   - instructionsSection() 包含两个 server 的说明

3. **碰撞检测**:
   - 注册两个 server 暴露同名工具 → 第二个注册失败
   - 错误消息包含碰撞的 fullName

4. **输出截断**:
   - mock server 返回超长结果 → 截断到 25000 字符
   - 截断消息格式正确

5. **工具池刷新**:
   - connect_mcp 后下一轮 assembleToolPool 包含新工具
   - 新工具可被模型正常调用

6. **错误路由**:
   - 未连接 server 时调用 mcp__ 工具 → 错误消息
   - 格式错误的 mcp__ 工具名 → 错误消息

### 15.4 eino 适配层测试

1. **WrapMCPTools**:
   - 返回正确数量的 InvokableTool
   - 每个工具的 Info().Name 格式为 mcp__{server}__{tool}
   - 每个工具的 Info().Desc 与原始 Description 一致

2. **mcpToolWrapper.Info**:
   - InputSchema 正确转换为 ParamsOneOf
   - 无 InputSchema 时 ParamsOneOf 为 nil

3. **mcpToolWrapper.InvokableRun**:
   - 正常调用 → 返回结果
   - server 错误 → 返回 error

4. **mcpProvider 接口**:
   - GetMCPServerName() 返回正确 server 名
   - GetMCPTools() 返回正确工具列表
   - GetMCPCaller() 返回 MCPClient
