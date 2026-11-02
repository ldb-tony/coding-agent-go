# 09 - Agent 核心循环规格

## 1. 职责

Agent 核心循环是整个 Coding Agent 的中枢，基于 eino 框架的 `adk.ChatModelAgent` 实现 REACT 模式编排：

1. **编排 LLM 调用** — 通过 eino ChatModelAgent 驱动多轮对话
2. **工具执行调度** — 路径感知并发调度，组内并发、组间串行
3. **Hook 触发** — 在生命周期关键节点触发自定义逻辑
4. **上下文管理** — messages 数组维护、压缩、截断
5. **轮数控制** — maxTurns 限制，防止无限循环
6. **后台任务注入** — 收集已完成后台任务结果并注入对话
7. **记忆系统集成** — 召回相关记忆、抽取新知识
8. **子代理委派** — 嵌套 agent loop，独立 messages 上下文

**设计理念**:
- 使用 eino `adk.ChatModelAgent` 作为核心编排器，而非手写循环
- 通过 `callbacks.Handler` 将 Hook 系统映射到 eino 生命周期
- 保留现有业务逻辑（权限、记忆、任务图），通过 eino 接口集成

## 2. 文件位置

```
pkg/agent/
├── agent.go         # ChatModelAgent 封装、主循环编排
├── config.go        # AgentConfig 配置
└── callbacks.go     # callbacks.Handler 实现、Hook 映射
```

## 3. 依赖

- `github.com/cloudwego/eino/adk` — ChatModelAgent 编排器
- `github.com/cloudwego/eino/components/model` — ChatModel 接口
- `github.com/cloudwego/eino/components/tool` — InvokableTool 接口
- `github.com/cloudwego/eino/schema` — Message 类型
- `github.com/cloudwego/eino/callbacks` — Handler 接口
- `coding-agent-go/pkg/tool` — 工具注册表
- `coding-agent-go/pkg/permission` — 权限管线
- `coding-agent-go/pkg/memory` — 跨会话记忆
- `coding-agent-go/pkg/taskstore` — 持久化任务图
- `coding-agent-go/pkg/background` — 后台任务管理
- `coding-agent-go/internal/logger` — 日志系统

## 4. 数据结构

### 4.1 AgentConfig

```go
type AgentConfig struct {
    Model       string            // 模型 ID，如 "claude-sonnet-4-5"
    BaseURL     string            // 可选，API base URL
    MaxTokens   int               // 可选，每次生成的最大 token 数（默认 8000）
    SystemPrompt string           // 可选，自定义 system prompt
    MaxTurns    int               // 可选，最大轮数限制（默认无限制）
    Tools       []tool.InvokableTool // 可选，注入的工具列表（测试用）
    Callbacks   callbacks.Handler    // 可选，注入的回调处理器
}
```

### 4.2 Agent

```go
type Agent struct {
    config    AgentConfig              // 配置
    client    model.ChatModel          // eino ChatModel 实例
    tools     []tool.InvokableTool     // 工具池
    callbacks callbacks.Handler        // 回调处理器
    session   *sessionState            // 会话状态
}
```

### 4.3 sessionState

会话级状态，挂在 Agent 上而不是全局变量。每个 Agent 调用创建一个实例。

```go
type sessionState struct {
    reads            map[string]readState // absPath -> 最近一次成功读取的状态
    readsMu          sync.Mutex           // 保护 reads map 的并发访问
    todo             *todoManager         // 会话级待办列表
    tasks            *taskStore           // .tasks/ 持久化任务图（跨会话）
    skills           *skillLoader         // 会话启动时扫描出的技能注册表
    memory           *memoryStore         // 跨会话记忆存储（.memory/）
    recalledMemories string               // 会话开始时召回的相关记忆全文（进 system prompt）
    shellCwd         string               // bash cwd 持久化（跨调用保持工作目录）
    mcp              *mcpRegistry         // 已连接 MCP 服务注册表
    background       *backgroundManager   // 后台任务管理器
}
```

### 4.4 readState

```go
type readState struct {
    mtime    time.Time // 文件修改时间
    start    int       // 返回区间起始行（11-based，含）；空文件为 0
    end      int       // 返回区间结束行（1-based，含）；空文件为 0
    fullView bool      // 本次读取是否覆盖整个文件
}
```

### 4.5 toolSpec

工具的内部元数据，包含处理器、并发安全性和资源函数。

```go
type toolSpec struct {
    handler         toolHandler                              // 工具处理函数
    concurrencySafe bool                                     // 是否可并发执行
    resources       func(args json.RawMessage) resourceSet   // 返回工具调用涉及的资源集合
}

type toolHandler func(st *sessionState, args json.RawMessage) string
```

### 4.6 resourceSet

表示工具调用涉及的资源集合，用于路径感知调度。

```go
type resourceSet struct {
    reads  []string // 读资源路径列表（已规范化为绝对路径）
    writes []string // 写资源路径列表（已规范化为绝对路径）
    global bool     // 是否为全局资源（bash/task/MCP 等无法静态分析的工具）
}
```

### 4.7 toolUseWithIndex

包装 tool_use block 与其原始下标，用于分组后仍能按原顺序回填结果。

```go
type toolUseWithIndex struct {
    index int              // 在原始 blocks 数组中的下标
    call  *schema.ToolCall // eino schema.ToolCall
}
```

### 4.8 toolGroup

一组可以并发执行的工具调用。

```go
type toolGroup struct {
    tools      []toolUseWithIndex // 组内工具列表
    hasUnsafe  bool               // 组内是否包含不安全工具
    unsafeSpec *toolSpec          // 组内唯一的 unsafe 工具（如果有）
}
```

## 5. 关键常量

```go
// 子代理
const subagentMaxTurns  = 30   // 子代理循环轮数上限
const subagentMaxTokens = 8000 // 子代理每次生成的最大 token 数

// 工具并发
const maxToolConcurrency = 8 // 并发执行工具的上限

// LLM 调用
const defaultMaxTokens = 8000 // 默认每次生成的最大 token 数

// reactive compact
const maxReactiveRetries = 1 // prompt_too_long 最多重试 1 次

// todo 提醒
const todoReminderThreshold = 3 // 连续 3 轮未使用 todo_write → 注入 reminder

// 文件工具
const maxReadOutputRunes = 25000    // read_file 输出字符预算
const maxGrepFileSize    = 5 * 1024 * 1024  // grep 跳过文件大小

// bash 工具
const defaultBashTimeout = 120 * time.Second  // 默认超时
const maxBashOutputRunes = 25000              // 输出截断

// 上下文压缩
const toolResultBudgetChars = 200000   // 一轮 tool_result 合计上限
const largeResultCharLimit = 30000     // 单条大结果落盘门槛
const largeResultPreviewChars = 2000   // 落盘后预览长度
const defaultContextWindow = 200000    // 上下文窗口
const autoCompactThreshold = 167000    // 自动压缩阈值 (200k - 20k - 13k)

// 记忆系统
const maxRecallMemories = 5            // 单次召回记忆数
const maxMemoryBodyRunes = 4000        // 单条记忆正文上限
const consolidateThreshold = 10        // 合并触发阈值

// 任务系统
const maxTodoItems = 20                // 待办清单上限
```

## 6. 核心函数

### 6.1 NewAgent()

```
函数签名: func NewAgent(ctx context.Context, cfg AgentConfig) (*Agent, error)

流程:
1. 创建 ChatModel
   - 如果 cfg.BaseURL != "":
     * 使用 eino-ext/claude 创建 ChatModel，设置 BaseURL
   - 否则:
     * 使用 eino-ext/claude 创建 ChatModel，使用默认配置
   - 失败 → 返回 nil, error

2. 创建 sessionState
   - st := newSessionState(cfg)

3. 组装工具池
   - 如果 cfg.Tools != nil（测试注入）→ 使用 cfg.Tools
   - 否则 → tools = assembleToolPool(st)

4. 创建 callbacks.Handler
   - 如果 cfg.Callbacks != nil（测试注入）→ 使用 cfg.Callbacks
   - 否则 → cbs = newAgentCallbacks(st)

5. 创建 ChatModelAgent
   - agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
       Model:     client,
       Tools:     tools,
       Callbacks: cbs,
   })
   - 失败 → 返回 nil, error

6. 返回 &Agent{config: cfg, client: client, tools: tools, callbacks: cbs, session: st}
```

### 6.2 Invoke()

```
函数签名: func (a *Agent) Invoke(ctx context.Context, messages []*schema.Message) ([]*schema.Message, error)

流程:
1. 触发 UserPromptSubmit hook
   - triggerHooks(EventUserPromptSubmit, &hookContext{Query: lastUserMessage(messages)})

2. 收集后台任务结果并注入
   - notes := a.session.background.collect()
   - 如果 len(notes) > 0:
     * 格式化为 <task_notification> 注入 messages

3. 召回记忆
   - a.session.recallMemories(extractQuery(messages))
   - 相关记忆全文由 systemPrompt 拼进 system

4. 初始化循环变量
   - turn = 0
   - reactiveRetries = 0
   - totalIn = 0, totalOut = 0（会话级 token 累计）

5. 主循环 for ; ; turn++:
   a. 检查轮数限制
      - 如果 a.config.MaxTurns > 0 && turn >= a.config.MaxTurns:
        * 返回 messages, error("max turns exceeded")

   b. 准备上下文（压缩流水线）
      - messages = a.compactor.prepare(ctx, a.session, messages)
      - 依次执行: toolResultBudget → microCompactIdle → shouldAutoCompact → autoCompact

   c. 调用 agent.Invoke(ctx, messages)
      - resp, err := a.agent.Invoke(ctx, messages)
      - 失败 → 检查是否 prompt_too_long（步骤 d）
      - 成功 → 继续（步骤 e）

   d. prompt_too_long 处理
      - 如果错误消息包含 "prompt_too_long" 或 "too many tokens":
        * 如果 reactiveRetries < maxReactiveRetries (1):
          - messages = a.compactor.reactiveCompact(ctx, messages)
          - reactiveRetries++
          - continue（重试本轮）
      - 否则 → 返回 messages, err

   e. 成功响应后重置 reactiveRetries = 0

   f. 统计 token 用量
      - totalIn += resp.Usage.PromptTokens
      - totalOut += resp.Usage.CompletionTokens
      - 打印: [tokens] in=... out=... | Σ in=... out=...

   g. 追加响应到 messages
      - messages = append(messages, resp)

   h. 检查 tool_use
      - 如果 resp.ToolCalls 为空 → 退出循环（步骤 i）
      - 否则 → 执行工具批次（步骤 j）

   i. 无工具调用 → 退出循环:
      - 触发 Stop hook: triggerHooks(EventStop, &hookContext{Messages: messages})
      - 如果 Stop hook 返回非 nil → 注入消息到对话，continue 继续循环
      - 否则:
        * a.session.extractMemories(messages) — 抽取记忆
        * 如果写入了新记忆 → a.session.consolidateMemories() — 合并记忆
        * 返回 messages, nil

   j. 执行工具批次
      - results = executeToolBatch(ctx, a.session, resp.ToolCalls, preHook, postHook)
      - preHook: 调用 triggerHooks(EventPreToolUse, hc)，返回非 nil 则拦截
      - postHook: 调用 triggerHooks(EventPostToolUse, hc)

   k. todo 提醒:
      - 如果本轮未使用 todo_write → roundsSinceTodo++
      - 如果 roundsSinceTodo >= 3:
        * 追加 reminder 到 results: "<reminder>Consider using todo_write to update your task plan.</reminder>"
        * roundsSinceTodo = 0（重置计数器）

   l. 追加 tool_results 到 messages
      - messages = append(messages, schema.ToolResultMessage(results...))
```

### 6.3 newSessionState()

```
函数签名: func newSessionState(cfg AgentConfig) *sessionState

流程:
1. 创建 skillLoader
   - skills := newSkillLoader(filepath.Join(workdir, "skills"))
   - skills.scan() — 扫描技能目录

2. 创建 taskStore
   - tasks: newTaskStore(filepath.Join(workdir, tasksDirName))

3. 创建 memoryStore
   - memory: newMemoryStore(filepath.Join(workdir, memoryDirName))

4. 创建 backgroundManager
   - background: newBackgroundManager()

5. 初始化 reads map 和 todoManager
   - reads: map[string]readState{}
   - todo: newTodoManager()

6. 返回 &sessionState{...}
```

### 6.4 assembleToolPool()

```
函数签名: func assembleToolPool(st *sessionState) []tool.InvokableTool

流程:
1. 创建 handlers 闭包集（捕获 sessionState）
   - BashHandler: func(args) → bashHandler(st, args)
   - ReadHandler: func(args) → readHandler(st, args)
   - WriteHandler: func(args) → writeHandler(st, args)
   - EditHandler: func(args) → editHandler(st, args)
   - GlobHandler: func(args) → globHandler(st, args)
   - GrepHandler: func(args) → grepHandler(st, args)
   - TodoWriteHandler: func(args) → todoWriteHandler(st, args)
   - LoadSkillHandler: func(args) → loadSkillHandler(st, args)
   - CompactHandler: func(args) → compactHandler(st, args)
   - TaskHandler: func(args) → taskHandler(st, args)
   - CreateTaskHandler: func(args) → createTaskHandler(st, args)
   - UpdateTaskHandler: func(args) → updateTaskHandler(st, args)
   - ClaimTaskHandler: func(args) → claimTaskHandler(st, args)
   - CompleteTaskHandler: func(args) → completeTaskHandler(st, args)
   - GetTaskHandler: func(args) → getTaskHandler(st, args)
   - ListTasksHandler: func(args) → listTaskHandler(st, args)
   - ConnectMCPHandler: func(args) → connectMCPHandler(st, args)

2. 收集 MCP 工具提供者
   - 如果 st != nil && st.mcp != nil:
     遍历 st.mcp.clients，创建 mcpProvider

3. 调用 tools.AssembleToolPool(handlers, mcpProviders...)
   - 返回所有 eino 兼容的工具列表

工具总数: 17 个基础工具 + MCP 动态工具
```

### 6.5 executeToolBatch()

```
函数签名: func executeToolBatch(
    ctx context.Context,
    st *sessionState,
    toolCalls []*schema.ToolCall,
    triggerPreHook func(*hookContext) *string,
    triggerPostHook func(*hookContext),
) []*schema.Message

流程:
1. 转换 toolCalls 为 toolUseWithIndex
   - 遍历 toolCalls，保留原始下标
   - toolUses = []toolUseWithIndex{{index: i, call: tc}}

2. 预分配结果数组
   - results = make([]*schema.Message, len(toolUses))
   - outputs = make([]string, len(toolUses))
   - blocked = make([]bool, len(toolUses))

3. 阶段 1: 串行 PreToolUse hook
   - 遍历 toolUses:
     * 构建 hookContext{ToolName: tc.Function.Name, Input: tc.Function.Arguments}
     * 调用 triggerPreHook(hc)
     * 如果返回非 nil（被拦截）:
       - blocked[i] = true
       - results[i] = schema.ToolResultMessage(tc.ID, *blockedMsg, true)
       - outputs[i] = *blockedMsg

4. 阶段 2: 贪心分组 (greedyGroup)
   - 遍历未被拦截的 toolUses:
     * 获取 toolSpec (getToolSpec)
     * 获取 resourceSet (spec.resources)
     * 按资源冲突关系分组（详见 10.1 节）

5. 阶段 3: 按组执行
   - 创建信号量 sem = make(chan struct{}, maxToolConcurrency)
   - 对每个 group:
     * executeGroup(ctx, st, group, sem, ...)
       - 组内: 每个 tool_use 启动 goroutine
         * 获取信号量 (select sem / ctx.Done)
         * 如果 ctx 已取消 → results[pos] = "Error: canceled"
         * 否则 → executeToolWithRecover(ctx, st, name, input)
           defer recover: panic → "Error: tool xxx panicked: ..."
           如果 shouldRunBackground(name, input) → startBackgroundTask(input)
           否则 → runTool(st, name, input)
         * 结果按原始下标写入 results[pos] 和 outputs[pos]
       - wg.Wait() — 等待组内全部完成

6. 阶段 4: 串行 PostToolUse hook
   - 遍历 toolUses:
     * 如果 blocked[i] → 只打印输出
     * 否则:
       - printToolOutput(outputs[i])
       - triggerPostHook(&hookContext{ToolName, Input, Output})

7. 返回 results
```

### 6.6 runTool()

```
函数签名: func runTool(st *sessionState, name string, input json.RawMessage) string

流程:
1. 查找 toolRegistry[name]
   - 存在 → 调用 spec.handler(st, input)，返回结果

2. MCP 外部工具 fallback
   - isMCPTool(name) 检查是否为 MCP 工具
   - st.mcp == nil → 返回 "MCP error: no MCP servers connected"
   - parseMCPName(name) 解析 server 和 tool 名
   - 解析失败 → 返回 "MCP error: malformed tool name"
   - server 未连接 → 返回 "MCP error: server not connected"
   - 调用 client.callTool(safeTool, input)

3. 未知工具
   - 返回 "Error: unknown tool name"
```

## 7. 与 eino 的集成

### 7.1 ChatModelAgent 的创建和配置

```go
agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
    Model:     chatModel,      // eino-ext/claude 创建的 ChatModel
    Tools:     tools,          // []tool.InvokableTool
    Callbacks: callbacks,      // callbacks.Handler
    // Middlewares 通过 adk.WithMiddlewares() 添加
})
```

**关键配置**:
- Model: 使用 `eino-ext/components/model/claude` 适配器
- Tools: 通过 `assembleToolPool()` 组装所有工具
- Callbacks: 通过 `newAgentCallbacks()` 创建 Hook 映射

### 7.2 callbacks.Handler 映射到 Hook 系统

```go
func newAgentCallbacks(st *sessionState) callbacks.Handler {
    return &agentCallbacks{session: st}
}

type agentCallbacks struct {
    session *sessionState
}

func (c *agentCallbacks) OnToolStart(ctx context.Context, info *callbacks.ToolInfo) error {
    hc := &hookContext{
        ToolName: info.Name,
        Input:    info.Input,
    }
    if blocked := triggerHooks(EventPreToolUse, hc); blocked != nil {
        return errorx.New(*blocked) // 返回 error 阻止工具执行
    }
    return nil
}

func (c *agentCallbacks) OnToolEnd(ctx context.Context, info *callbacks.ToolInfo, output interface{}) error {
    hc := &hookContext{
        ToolName: info.Name,
        Input:    info.Input,
        Output:   fmt.Sprintf("%v", output),
    }
    triggerHooks(EventPostToolUse, hc)
    return nil
}

func (c *agentCallbacks) OnAgentFinish(ctx context.Context, messages []*schema.Message) error {
    hc := &hookContext{Messages: messages}
    if injected := triggerHooks(EventStop, hc); injected != nil {
        // Stop hook 返回非 nil → 注入消息继续循环
        // 注意: eino ChatModelAgent 不支持直接注入消息，需要通过外部循环控制
        return errorx.New("continue:" + *injected)
    }
    return nil
}
```

### 7.3 Middlewares 的加载顺序

```go
// 按以下顺序添加 middlewares（顺序重要）
agent := adk.NewChatModelAgent(ctx, cfg)

// 1. filesystem — 文件工具（read/write/edit/glob/grep）
agent.Use(filesystem.New(ctx, &filesystem.Config{
    Workdir: workdir,
}))

// 2. plantask — 任务规划（todo_write）
agent.Use(plantask.New(ctx, &plantask.Config{
    Backend: st.todo,
}))

// 3. reduction — 大结果落盘
agent.Use(reduction.New(ctx, &reduction.Config{
    MaxResultSize: toolResultBudgetChars,
}))

// 4. summarization — 上下文压缩
agent.Use(summarization.New(ctx, &summarization.Config{
    TokenThreshold: autoCompactThreshold,
}))

// 5. skill — 技能加载
agent.Use(skill.New(ctx, &skill.Config{
    SkillsDir: filepath.Join(workdir, "skills"),
}))

// 6. agentsmd — AGENTS.md 注入
agent.Use(agentsmd.New(ctx, &agentsmd.Config{
    Path: filepath.Join(workdir, "AGENTS.md"),
}))
```

## 8. 消息类型定义

### 8.1 eino schema.Message 与 anthropic.MessageParam 的映射

| eino schema.Message | anthropic.MessageParam | 说明 |
|---------------------|------------------------|------|
| `schema.UserMessage(content)` | `UserMessage(TextBlock)` | 用户输入 |
| `schema.AssistantMessage(content, toolCalls)` | `AssistantMessage(TextBlock, ToolUse)` | 模型响应 |
| `schema.ToolResultMessage(id, content, isError)` | `UserMessage(ToolResultBlock)` | 工具结果 |
| `schema.SystemMessage(content)` | (system parameter) | 系统提示 |

### 8.2 工具调用消息格式

**eino schema.ToolCall**:
```go
type ToolCall struct {
    ID       string          // 工具调用 ID，如 "toolu_1"
    Function *ToolCallFunction // 函数信息
}

type ToolCallFunction struct {
    Name      string          // 工具名，如 "bash"
    Arguments json.RawMessage // 工具参数 JSON，如 {"command": "echo hi"}
}
```

**映射到 anthropic.ContentBlockUnion**:
```go
anthropic.ContentBlockUnion{
    Type:  "tool_use",
    ID:    tc.ID,
    Name:  tc.Function.Name,
    Input: tc.Function.Arguments,
}
```

## 9. 算法细节

### 9.1 贪心分组算法 (greedyGroup)

目标：将一批 tool_use 按资源冲突关系分组，组内可并发，组间必须串行。

```
输入: toolUses[] (带原始下标), blocked[] (被 hook 拦截的标记)
输出: []toolGroup

初始化: groups = [], current = nil

遍历 i, tu := range toolUses:
  如果 blocked[i] → skip

  spec = getToolSpec(tu.call.Function.Name)
  resources = spec.resources(tu.call.Function.Arguments)

  如果 current == nil:
    current = &toolGroup{tools: [tu], hasUnsafe: !spec.concurrencySafe}
    continue

  分支 A: spec.concurrencySafe == true (safe 工具)
    如果 current.hasUnsafe && conflictsWithGroup(resources, current):
      结束当前组 → groups = append(groups, *current)
      新开一组 → current = &toolGroup{tools: [tu], hasUnsafe: false}
    否则:
      加入当前组 → current.tools = append(current.tools, tu)

  分支 B: spec.concurrencySafe == false (unsafe 工具)
    如果 current.hasUnsafe:
      结束当前组 → groups = append(groups, *current)
      新开一组 → current = &toolGroup{tools: [tu], hasUnsafe: true, unsafeSpec: spec}
    否则:
      conflicting, nonConflicting = splitByConflict(tu, spec, current)
      如果 len(conflicting) > 0:
        如果 len(nonConflicting) > 0:
          current.tools = nonConflicting
          groups = append(groups, *current)
        groups = append(groups, toolGroup{tools: conflicting, hasUnsafe: false})
      current = &toolGroup{tools: append(nonConflicting, tu), hasUnsafe: true, unsafeSpec: spec}

flush 最后一组:
  如果 current != nil && len(current.tools) > 0:
    groups = append(groups, *current)

返回 groups
```

### 9.2 资源冲突判定 (conflicts)

```
函数签名: func conflicts(a, b resourceSet) bool

规则（按优先级）:
1. 任一方 global=true → 冲突（bash/task/MCP 等无法静态分析的工具）
2. a.writes ∩ b.writes ≠ ∅ → 冲突（总是冲突，即使路径不同，避免两个写互相覆盖）
3. a.reads ∩ b.writes ≠ ∅ → 冲突（精确路径匹配或前缀匹配: r == w || HasPrefix(r, w+"/")）
4. a.writes ∩ b.reads ≠ ∅ → 冲突（同上，反向检查）
5. 以上均不满足 → 无冲突，可并发
```

### 9.3 工具注册表 (toolRegistry)

```go
var toolRegistry = map[string]*toolSpec{}

// init() 中注册全部 19 个工具:
```

| 工具名 | concurrencySafe | 资源类型 | 说明 |
|--------|----------------|---------|------|
| bash | false | global | 无法静态分析 |
| read_file | true | reads: [path] | 只读，无共享副作用 |
| write_file | false | writes: [path] | 写操作，独占 |
| edit_file | false | writes: [path] | 写操作，独占 |
| glob | true | reads: [searchRoot] | 只读搜索 |
| grep | true | reads: [searchRoot] | 只读搜索 |
| todo_write | false | global | 涉及会话状态 |
| load_skill | false | global | 涉及磁盘读取 |
| compact | false | global | 标记位操作 |
| create_task | false | global | 任务图操作 |
| update_task | false | global | 任务图操作 |
| claim_task | false | global | 任务图操作 |
| complete_task | false | global | 任务图操作 |
| get_task | false | global | 涉及磁盘读取 |
| list_tasks | false | global | 涉及磁盘读取 |
| connect_mcp | false | global | MCP 连接操作 |
| task | false | global | 子代理是 global |

路径越界（safePath 失败）时，资源函数返回 `resourceSet{global: true}` 保守处理。

## 10. 错误处理

### 10.1 LLM 调用错误

| 错误类型 | 检测方式 | 处理 |
|---------|---------|------|
| prompt_too_long | 错误消息包含 "prompt_too_long" 或 "too many tokens" | reactive compact，最多重试 1 次 |
| 其他 API 错误 | — | 直接返回 messages, err |
| 子代理 API 失败 | err != nil | 返回 "Error: subagent failed: ..." |
| maxTurns 超限 | turn >= MaxTurns | 返回 messages, error("max turns exceeded") |

### 10.2 工具执行错误

| 错误场景 | 处理方式 |
|---------|---------|
| 工具 panic | recover → 返回 "Error: tool xxx panicked: ..." |
| ctx 取消 | 未启动的调用返回 "Error: canceled" |
| 未知工具 | 返回 "Error: unknown tool name" |
| MCP 未连接 | 返回 "MCP error: no MCP servers connected" |
| MCP 名字格式错误 | 返回 "MCP error: malformed tool name" |

### 10.3 压缩错误

| 错误场景 | 处理方式 |
|---------|---------|
| auto_compact 失败 | 累计 consecutiveFailures，历史原样保留 |
| auto_compact 连续失败 3 次 | 熔断，不再触发 auto_compact |
| reactive_compact summary 失败 | 使用兜底文本 "(summary unavailable; see transcript)" |
| compact_history 失败 | 打印错误到终端，保留原 messages |

## 11. token 统计格式

每轮模型返回后在 stdout 打印 token 用量（当轮 + 会话累计）：

```
[tokens] in=1,234 out=567 cache_create=0 cache_read=0 | Σ in=1,234 out=567
```

字段说明：
- `in` / `out`: 当轮 input/output tokens
- `cache_create` / `cache_read`: 当轮 cache 相关 tokens
- `Σ in` / `Σ out`: 会话级累计 input/output tokens
- 所有数值使用千分位格式（commaTokens 函数）

## 12. 子代理 (runSubagent)

### 12.1 常量

```go
const subagentMaxTurns  = 30   // 子代理循环轮数上限
const subagentMaxTokens = 8000 // 子代理每次生成的 max_tokens
const taskToolName      = "task" // 子代理工具集中被排除的工具
```

### 12.2 子代理 system prompt

```
"You are a subagent working on a single delegated task. "
"Use the provided tools to complete it, then reply with a concise final summary "
"of what you found or did. Act, don't explain."
```

### 12.3 subTools()

```
函数签名: func subTools(st *sessionState) []tool.InvokableTool

流程:
1. all := assembleToolPool(st) — 获取完整工具池
2. 遍历 all，跳过 name == "task" 的工具
3. 返回过滤后的工具列表
```

### 12.4 runSubagent()

```
函数签名: func runSubagent(st *sessionState, prompt string) string

流程:
1. 创建子代理 LLM 客户端
   - 使用 eino-ext/claude 创建独立的 ChatModel
   - 配置与父 agent 相同（Model, BaseURL, MaxTokens）

2. 创建子代理 ChatModelAgent
   - subAgent, _ := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
       Model: subClient,
       Tools: subTools(st),
       Callbacks: st.callbacks, // 共享父 agent 的 callbacks
   })

3. 初始化 messages
   - messages := [schema.UserMessage(prompt)]

4. 循环 for turn := 0; turn < subagentMaxTurns (30); turn++:
   a. tools := subTools(st) — 每轮重算（MCP 工具可能变化）
   b. resp, err := subAgent.Invoke(ctx, messages)
   c. 错误处理: 返回 "Error: subagent failed: " + err.Error()
   d. messages = append(messages, resp)

   e. 如果 resp.ToolCalls 为空 → 返回 subagentText(resp.Content)

   f. results = executeToolBatch(ctx, st, resp.ToolCalls, preHook, postHook)
      - preHook: triggerHooks(EventPreToolUse, hc) — 与父相同的权限/hook
      - postHook: triggerHooks(EventPostToolUse, hc)

   g. messages = append(messages, schema.ToolResultMessage(results...))

5. 超过 30 轮 → 返回 "Subagent.stopped after 30 turns without a final answer."
```

### 12.5 subagentText()

```
函数签名: func subagentText(content string) string

流程:
1. TrimSpace 后非空 → 返回 content
2. 为空 → 返回 "(no summary)"
```

### 12.6 taskHandler()

```
函数签名: func taskHandler(st *sessionState, args json.RawMessage) string

流程:
1. 解析 JSON 参数 { "prompt": "..." }
2. 校验 prompt 非空
3. 调用 runSubagent(st, in.Prompt) 并返回结果
```

## 13. 测试要点

### 13.1 Agent 核心循环单元测试

1. **正常流程**: 用户输入 → LLM 返回 text → 退出循环
2. **工具调用流程**: LLM 返回 tool_use → 执行工具 → 回填 tool_result → 继续循环
3. **无工具调用退出**: LLM 返回纯文本 → Stop hook → 返回
4. **prompt_too_long 重试**: 第一次触发 reactive compact → 重试成功 → reactiveRetries 重置
5. **prompt_too_long 超限**: 重试 1 次后仍失败 → 返回错误
6. **maxTurns 限制**: 设置 MaxTurns=5，验证第 5 轮后返回错误
7. **todo 提醒**: 连续 3 轮无 todo_write → 第 3 轮注入 reminder
8. **token 统计**: 验证千分位格式和会话累计

### 13.2 sessionState 测试

1. **newSessionState**: 验证 skillLoader、taskStore、memoryStore、backgroundManager 正确初始化
2. **readState mtime 去重**: 同一文件、同一区间、mtime 未变 → 返回 stub
3. **read-before-write 门控**: 未读文件 → write/edit 返回错误
4. **markWritten**: write/edit 后刷新 reads 状态

### 13.3 子代理测试

1. **正常完成**: 子代理在 30 轮内返回最终文本
2. **超限退出**: 30 轮后无最终答复 → 返回超时消息
3. **工具排除**: 子代理不可使用 task 工具（只允许一层委派）
4. **独立 messages**: 子代理的中间工具调用不进入父的 messages
5. **共享 hook**: 子代理的 PreToolUse/PostToolUse hook 与父相同

### 13.4 executeToolBatch 测试

1. **纯并发**: 多个 read_file 无冲突 → 同组并发执行
2. **纯串行**: 多个 write_file → 各自独占组
3. **混合调度**: read + write 同一文件 → 按顺序分组
4. **hook 拦截**: PreToolUse 返回非 nil → 工具不执行，产出 error result
5. **panic 恢复**: 工具 panic → 转为 error result，不影响其他工具
6. **ctx 取消**: 未启动的调用产出 "canceled" 结果
7. **并发上限**: 验证 maxToolConcurrency = 8 的信号量限制

### 13.5 资源冲突判定测试

1. **global vs 任何**: 冲突
2. **writes vs writes**: 总是冲突（即使路径不同）
3. **reads vs writes 同路径**: 冲突
4. **reads vs writes 前缀匹配**: 冲突（r == w || HasPrefix(r, w+"/")）
5. **reads vs reads 不同路径**: 无冲突
6. **reads vs reads 同路径**: 无冲突（多个读可并发）

### 13.6 eino 集成测试

1. **ChatModelAgent 创建**: 验证 agent 正确初始化
2. **callbacks.Handler 映射**: 验证 OnToolStart/OnToolEnd/OnAgentFinish 正确触发
3. **Middlewares 加载**: 验证 6 个 middleware 按顺序加载
4. **消息格式转换**: 验证 eino schema.Message ↔ anthropic.MessageParam 转换正确
