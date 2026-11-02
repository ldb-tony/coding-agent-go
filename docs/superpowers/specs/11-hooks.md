# 11 - Hook 系统规格

## 1. 职责

Hook 系统负责在 Agent 生命周期的关键节点注入自定义逻辑：

1. **UserPromptSubmit** — 用户输入后、LLM 调用前触发（输入校验、上下文注入）
2. **PreToolUse** — 工具执行前触发，可拦截（权限检查、危险操作警告）
3. **PostToolUse** — 工具执行后触发（大输出预警、工具执行日志）
4. **Stop** — 循环即将退出时触发，可注入消息继续循环（退出摘要、记忆抽取）

**设计理念**:
- 通过 eino `callbacks.Handler` 映射到 Hook 系统
- Hook 注册和分发机制独立于 Agent 核心循环
- 支持多个 Hook 按注册顺序依次调用
- 任一 Hook 返回 non-nil → 短路，不再调用后续 Hook
- 扩展通过 `registerHook()` 挂载，Agent 循环只调用 `triggerHooks()`

## 2. 文件位置

```
pkg/hooks/
├── hooks.go         # Hook 类型定义、注册表、分发机制
└── builtin.go       # 内置 Hook 实现
```

## 3. 依赖

- `encoding/json` — 工具参数解析
- `fmt` — 终端输出
- `strings` — 字符串处理
- `github.com/cloudwego/eino/callbacks` — callbacks.Handler 接口
- `github.com/cloudwego/eino/schema` — Message 类型

无外部包依赖，全部使用 Go 标准库 + eino callbacks。

## 4. 数据结构

### 4.1 HookEvent

Hook 事件类型，标识生命周期节点。

```go
type HookEvent string

const (
    EventUserPromptSubmit HookEvent = "UserPromptSubmit"
    EventPreToolUse       HookEvent = "PreToolUse"
    EventPostToolUse      HookEvent = "PostToolUse"
    EventStop             HookEvent = "Stop"
)
```

### 4.2 hookContext

Hook 上下文，携带事件相关的上下文信息。不同事件使用不同的字段。

```go
type hookContext struct {
    Query    string                   // EventUserPromptSubmit: 用户原始输入
    ToolName string                   // EventPreToolUse / EventPostToolUse: 工具名
    Input    json.RawMessage          // EventPreToolUse / EventPostToolUse: 工具原始 JSON 参数
    Output   string                   // EventPostToolUse: 工具结果文本
    Messages []*schema.Message        // EventStop: 即将退出时的完整对话
}
```

**字段使用说明**:
- `EventUserPromptSubmit`: 只使用 `Query` 字段
- `EventPreToolUse`: 使用 `ToolName` 和 `Input` 字段
- `EventPostToolUse`: 使用 `ToolName`、`Input` 和 `Output` 字段
- `EventStop`: 只使用 `Messages` 字段

### 4.3 hookFunc

Hook 回调函数签名。返回 non-nil 表示"接管本次事件"。

```go
type hookFunc func(*hookContext) *string
```

**返回值语义**:
- `nil` — 继续执行后续 Hook，不干预控制流
- `non-nil` — 短路，返回值含义取决于事件类型:
  - `EventPreToolUse`: 拦截工具执行，返回值作为 tool_result（isError=true）
  - `EventStop`: 注入消息到对话，强制循环继续
  - 其他事件: 返回值不影响控制流，仅记录日志

### 4.4 Hook 注册表

```go
var hooks = map[HookEvent][]hookFunc{
    EventUserPromptSubmit: {},
    EventPreToolUse:       {},
    EventPostToolUse:      {},
    EventStop:             {},
}
```

**注册表结构**:
- Key: HookEvent（事件类型）
- Value: []hookFunc（该事件的回调列表，按注册顺序执行）

## 5. 核心函数

### 5.1 registerHook()

```
函数签名: func registerHook(event HookEvent, cb hookFunc)

流程:
1. 将回调追加到指定事件的回调列表末尾
   - hooks[event] = append(hooks[event], cb)

2. 无返回值

注意:
- 注册顺序决定执行顺序
- 同一事件可注册多个回调
- 回调按注册顺序依次调用
```

### 5.2 triggerHooks()

```
函数签名: func triggerHooks(event HookEvent, hc *hookContext) *string

流程:
1. 遍历 hooks[event] 中的所有回调
   - 按注册顺序依次调用

2. 对每个回调 cb:
   a. 调用 result := cb(hc)
   b. 如果 result != nil:
      - 立即返回 result（短路）
      - 不再调用后续回调

3. 全部回调返回 nil → 返回 nil

返回值:
- nil: 所有回调都返回 nil，继续正常流程
- non-nil: 某个回调返回了拦截消息，按事件类型处理
```

### 5.3 registerBuiltinHooks()

```
函数签名: func registerBuiltinHooks()

流程:
1. 注册 UserPromptSubmit Hook
   - registerHook(EventUserPromptSubmit, contextInjectHook)

2. 注册 PreToolUse Hook（按顺序）
   - registerHook(EventPreToolUse, permissionHook)
   - registerHook(EventPreToolUse, logHook)

3. 注册 PostToolUse Hook
   - registerHook(EventPostToolUse, largeOutputHook)

4. 注册 Stop Hook
   - registerHook(EventStop, summaryHook)

注意:
- 抽成函数是为了让测试能重建一套干净注册表
- 测试时可调用 resetHooks() 清空注册表，再调用 registerBuiltinHooks() 重建
```

### 5.4 resetHooks()

```
函数签名: func resetHooks()

流程:
1. 清空所有事件的回调列表
   - hooks = map[HookEvent][]hookFunc{
       EventUserPromptSubmit: {},
       EventPreToolUse:       {},
       EventPostToolUse:      {},
       EventStop:             {},
   }

2. 无返回值

注意:
- 仅用于测试，生产环境不应调用
- 测试时隔离各测试对包级 hooks 全局状态的改动
```

## 6. 内置 Hook 实现

### 6.1 contextInjectHook (UserPromptSubmit)

```go
func contextInjectHook(hc *hookContext) *string {
    // 记录当前工作目录
    fmt.Printf("\033[90m[HOOK] UserPromptSubmit: working in %s\033[0m\n", workdir)
    appLog.Info("hook", "event", "UserPromptSubmit", "workdir", workdir)
    
    // 未来扩展: 注入后台任务结果、注入记忆召回结果
    // 当前实现: 仅记录日志，返回 nil（不干预控制流）
    return nil
}
```

**职责**:
- 用户输入后、LLM 调用前触发
- 记录工作目录
- 未来可扩展: 注入后台任务结果、注入记忆召回结果

**返回值**: 始终返回 nil（不干预控制流）

### 6.2 permissionHook (PreToolUse)

```go
func permissionHook(hc *hookContext) *string {
    // 三闸门权限校验
    if err := permissionChecker.Check(hc.ToolName, hc.Input); err != nil {
        msg := err.Error()
        return &msg // 拦截工具执行
    }
    return nil // 放行
}
```

**职责**:
- 工具执行前进行权限检查
- 三闸门任意一关拒绝时返回拦截消息

**返回值**:
- nil: 权限检查通过，继续执行工具
- non-nil: 权限检查失败，拦截工具执行，返回值作为 tool_result

### 6.3 logHook (PreToolUse)

```go
func logHook(hc *hookContext) *string {
    // 记录每一次工具调用
    fmt.Printf("\033[90m[HOOK] %s(%s)\033[0m\n", hc.ToolName, previewArgs(hc.Input))
    appLog.Info("hook", "event", "PreToolUse", "tool", hc.ToolName, "input", truncateForLog(string(hc.Input)))
    
    return nil // 不干预控制流
}
```

**职责**:
- 记录工具调用日志
- 打印工具名和参数预览（截断到 60 字符）

**返回值**: 始终返回 nil（不干预控制流）

**注意**: 
- 注册在 permissionHook 之后
- 如果 permissionHook 拦截，logHook 不会执行（短路机制）

### 6.4 largeOutputHook (PostToolUse)

```go
const largeOutputRunes = 40000

func largeOutputHook(hc *hookContext) *string {
    // 检查输出是否超过阈值
    if n := len([]rune(hc.Output)); n > largeOutputRunes {
        fmt.Printf("\033[33m[HOOK] Large output from %s: %d chars\033[0m\n", hc.ToolName, n)
        appLog.Warn("hook", "event", "PostToolUse", "tool", hc.ToolName, "output_chars", n)
    }
    
    return nil // 返回值不影响控制流
}
```

**职责**:
- 工具执行后检查输出大小
- 超过 40000 字符时给出预警

**返回值**: 始终返回 nil（PostToolUse Hook 的返回值不影响控制流）

**阈值说明**:
- 参考实现用 100000，但本项目的工具本身已把输出截断（bash 50K、read 25K 字符）
- 100000 永远触发不到，下调到 40000 让提醒真正可用

### 6.5 summaryHook (Stop)

```go
func summaryHook(hc *hookContext) *string {
    // 统计本次会话的工具调用次数
    toolCalls := countToolResults(hc.Messages)
    fmt.Printf("\033[90m[HOOK] Stop: session used %d tool calls\033[0m\n", toolCalls)
    appLog.Info("hook", "event", "Stop", "tool_calls", toolCalls)
    
    // 未来扩展: 抽取记忆
    // 当前实现: 仅打印统计，返回 nil（允许循环退出）
    return nil
}
```

**职责**:
- 循环即将退出时打印会话统计
- 统计工具调用次数

**返回值**:
- nil: 允许循环正常退出
- non-nil: 注入消息到对话，强制循环继续（当前实现未使用）

### 6.6 辅助函数

#### previewArgs()

```go
func previewArgs(input json.RawMessage) string {
    s := strings.TrimSpace(string(input))
    runes := []rune(s)
    if len(runes) > 60 {
        return string(runes[:60]) + "..."
    }
    return s
}
```

**职责**: 把工具参数压成单行、最多 60 字符的预览

#### countToolResults()

```go
func countToolResults(messages []*schema.Message) int {
    count := 0
    for _, m := range messages {
        if m.Role == schema.ToolResult {
            count++
        }
    }
    return count
}
```

**职责**: 统计对话里 tool_result 消息的数量，即已执行的工具调用次数

#### truncateForLog()

```go
func truncateForLog(s string) string {
    const maxLogLength = 500
    if len(s) > maxLogLength {
        return s[:maxLogLength] + "..."
    }
    return s
}
```

**职责**: 截断字符串用于日志记录，避免日志过大

## 7. 与 eino callbacks.Handler 的映射

### 7.1 映射关系

| Hook 事件 | eino Callback | 触发时机 | 返回值语义 |
|----------|---------------|---------|-----------|
| `EventUserPromptSubmit` | (手动触发) | agent.Invoke 之前 | 不影响控制流 |
| `EventPreToolUse` | `OnToolStart` | 工具执行前 | non-nil 拦截工具 |
| `EventPostToolUse` | `OnToolEnd` | 工具执行后 | 不影响控制流 |
| `EventStop` | `OnAgentFinish` | 循环即将退出 | non-nil 强制继续 |

### 7.2 agentCallbacks 实现

```go
type agentCallbacks struct {
    session    *sessionState
    permission *PermissionChecker
}

func newAgentCallbacks(st *sessionState, perm *PermissionChecker) callbacks.Handler {
    return &agentCallbacks{session: st, permission: perm}
}

// OnToolStart 映射到 PreToolUse Hook
func (c *agentCallbacks) OnToolStart(ctx context.Context, info *callbacks.ToolInfo) error {
    hc := &hookContext{
        ToolName: info.Name,
        Input:    info.Input,
    }
    if blocked := triggerHooks(EventPreToolUse, hc); blocked != nil {
        // 返回 error 阻止工具执行
        return errorx.New(*blocked)
    }
    return nil
}

// OnToolEnd 映射到 PostToolUse Hook
func (c *agentCallbacks) OnToolEnd(ctx context.Context, info *callbacks.ToolInfo, output interface{}) error {
    hc := &hookContext{
        ToolName: info.Name,
        Input:    info.Input,
        Output:   fmt.Sprintf("%v", output),
    }
    triggerHooks(EventPostToolUse, hc)
    return nil // PostToolUse 的返回值不影响控制流
}

// OnAgentFinish 映射到 Stop Hook
func (c *agentCallbacks) OnAgentFinish(ctx context.Context, messages []*schema.Message) error {
    hc := &hookContext{Messages: messages}
    if injected := triggerHooks(EventStop, hc); injected != nil {
        // Stop Hook 返回 non-nil → 注入消息继续循环
        // 注意: eino ChatModelAgent 不支持直接注入消息，需要通过外部循环控制
        return errorx.New("continue:" + *injected)
    }
    return nil
}
```

### 7.3 UserPromptSubmit 的手动触发

```go
// 在 Agent.Invoke 中
func (a *Agent) Invoke(ctx context.Context, messages []*schema.Message) ([]*schema.Message, error) {
    // 手动触发 UserPromptSubmit Hook
    lastUserMsg := extractLastUserMessage(messages)
    hc := &hookContext{Query: lastUserMsg}
    triggerHooks(EventUserPromptSubmit, hc)
    
    // 继续执行 agent.Invoke
    // ...
}
```

**说明**:
- `EventUserPromptSubmit` 没有对应的 eino callback
- 在调用 `agent.Invoke` 之前手动触发
- 用于输入校验、上下文注入等

## 8. Hook 注册和分发机制

### 8.1 注册机制

```go
// 每个 Hook 类型可注册多个 handler
var hooks = map[HookEvent][]hookFunc{
    EventUserPromptSubmit: {},
    EventPreToolUse:       {},
    EventPostToolUse:      {},
    EventStop:             {},
}

// 注册函数
func registerHook(event HookEvent, cb hookFunc) {
    hooks[event] = append(hooks[event], cb)
}
```

**特点**:
- 使用 map 存储事件到回调列表的映射
- 回调列表是 slice，按注册顺序执行
- 支持同一事件注册多个回调

### 8.2 分发机制

```go
func triggerHooks(event HookEvent, hc *hookContext) *string {
    for _, cb := range hooks[event] {
        if result := cb(hc); result != nil {
            return result // 短路，返回第一个 non-nil 结果
        }
    }
    return nil
}
```

**特点**:
- 按注册顺序依次调用回调
- 任一回调返回 non-nil → 短路，不再调用后续回调
- 返回第一个 non-nil 结果
- 全部返回 nil → 返回 nil

### 8.3 短路语义

不同事件的短路语义不同:

**EventPreToolUse**:
- 返回 non-nil → 拦截工具执行
- 返回值作为 tool_result（isError=true）返回给模型
- 后续 PreToolUse Hook 不再执行

**EventStop**:
- 返回 non-nil → 注入消息到对话，强制循环继续
- 后续 Stop Hook 不再执行

**EventUserPromptSubmit / EventPostToolUse**:
- 返回值不影响控制流
- 短路仅用于优化（跳过后续无意义的回调）

## 9. Hook 上下文传递

### 9.1 各事件可用的上下文信息

| 事件类型 | Query | ToolName | Input | Output | Messages |
|---------|-------|----------|-------|--------|----------|
| UserPromptSubmit | ✓ | - | - | - | - |
| PreToolUse | - | ✓ | ✓ | - | - |
| PostToolUse | - | ✓ | ✓ | ✓ | - |
| Stop | - | - | - | - | ✓ |

### 9.2 上下文构建

```go
// UserPromptSubmit
hc := &hookContext{
    Query: lastUserMessage,
}

// PreToolUse
hc := &hookContext{
    ToolName: toolName,
    Input:    toolArgs,
}

// PostToolUse
hc := &hookContext{
    ToolName: toolName,
    Input:    toolArgs,
    Output:   toolResult,
}

// Stop
hc := &hookContext{
    Messages: allMessages,
}
```

## 10. 错误处理

### 10.1 Hook 执行失败

```go
func triggerHooks(event HookEvent, hc *hookContext) *string {
    for _, cb := range hooks[event] {
        func() {
            defer func() {
                if r := recover(); r != nil {
                    // Hook panic → 记录日志，不中断主循环
                    appLog.Error("hook panicked", "event", event, "panic", r)
                }
            }()
            if result := cb(hc); result != nil {
                // 返回结果，短路
                return result
            }
        }()
    }
    return nil
}
```

**处理规则**:
- Hook 执行失败（panic）→ 记录日志，不中断主循环
- 视为该 Hook 返回 nil（继续执行后续 Hook）
- 主循环继续正常执行

### 10.2 Hook 超时

当前实现不支持 Hook 超时机制。

**未来扩展**:
```go
func triggerHooks(event HookEvent, hc *hookContext) *string {
    for _, cb := range hooks[event] {
        done := make(chan *string, 1)
        go func() {
            done <- cb(hc)
        }()
        
        select {
        case result := <-done:
            if result != nil {
                return result
            }
        case <-time.After(5 * time.Second):
            // Hook 超时 → 跳过，使用默认行为
            appLog.Warn("hook timed out", "event", event)
            continue
        }
    }
    return nil
}
```

**超时处理**:
- 默认超时时间: 5 秒
- 超时 → 跳过该 Hook，继续执行后续 Hook
- 记录警告日志

## 11. 算法细节

### 11.1 Hook 注册顺序

```
init() {
    registerBuiltinHooks()
}

registerBuiltinHooks() {
    // UserPromptSubmit: 1 个 Hook
    registerHook(EventUserPromptSubmit, contextInjectHook)
    
    // PreToolUse: 2 个 Hook（按顺序）
    registerHook(EventPreToolUse, permissionHook)  // 先检查权限
    registerHook(EventPreToolUse, logHook)         // 再记录日志
    
    // PostToolUse: 1 个 Hook
    registerHook(EventPostToolUse, largeOutputHook)
    
    // Stop: 1 个 Hook
    registerHook(EventStop, summaryHook)
}
```

**顺序说明**:
- PreToolUse 中 permissionHook 在 logHook 之前
- 如果权限检查失败，logHook 不会执行（短路）
- 这样避免记录被拦截的工具调用日志

### 11.2 短路优化

```go
// 在 triggerHooks 中
for _, cb := range hooks[event] {
    if result := cb(hc); result != nil {
        return result // 短路，不再调用后续回调
    }
}
```

**优化效果**:
- PreToolUse: permissionHook 拦截 → logHook 不执行
- Stop: summaryHook 返回 non-nil → 后续 Hook 不执行（当前只有 1 个）

### 11.3 Hook 上下文构建

```go
// 在 executeToolBatch 中
for i, tu := range toolUses {
    // 构建 PreToolUse 上下文
    hc := &hookContext{
        ToolName: tu.call.Function.Name,
        Input:    tu.call.Function.Arguments,
    }
    
    // 触发 PreToolUse Hook
    if blocked := triggerPreHook(hc); blocked != nil {
        // 拦截，构建 tool_result
        results[i] = schema.ToolResultMessage(tu.call.ID, *blocked, true)
        outputs[i] = *blocked
        blocked[i] = true
    }
}
```

## 12. 内置 Hook 示例

### 12.1 UserPromptSubmit: 注入后台任务结果

```go
// 未来扩展
func backgroundInjectHook(hc *hookContext) *string {
    notes := backgroundTasks.collect()
    if len(notes) == 0 {
        return nil
    }
    
    // 将后台任务结果注入到对话
    // 注意: UserPromptSubmit 的返回值不影响控制流
    // 需要通过其他方式注入（如修改 messages 数组）
    appLog.Info("background tasks injected", "count", len(notes))
    return nil
}
```

### 12.2 UserPromptSubmit: 注入记忆召回结果

```go
// 未来扩展
func memoryInjectHook(hc *hookContext) *string {
    if hc.Query == "" {
        return nil
    }
    
    // 召回相关记忆
    memories := memoryStore.recall(hc.Query)
    if memories == "" {
        return nil
    }
    
    // 将记忆注入到 system prompt
    // 注意: UserPromptSubmit 的返回值不影响控制流
    // 需要通过其他方式注入（如修改 system prompt）
    appLog.Info("memories recalled", "length", len(memories))
    return nil
}
```

### 12.3 PreToolUse: 危险操作警告

```go
// 未来扩展
func dangerousOpWarningHook(hc *hookContext) *string {
    if hc.ToolName != "bash" {
        return nil
    }
    
    var in struct {
        Command string `json:"command"`
    }
    _ = json.Unmarshal(hc.Input, &in)
    
    // 检查是否包含危险操作
    if strings.Contains(in.Command, "rm -rf") {
        warning := "⚠ Warning: This command will delete files recursively. Proceed with caution."
        fmt.Println(warning)
        appLog.Warn("dangerous operation", "command", in.Command)
        // 不拦截，仅警告
    }
    
    return nil
}
```

### 12.4 PostToolUse: 工具执行日志

```go
// 未来扩展
func toolExecLogHook(hc *hookContext) *string {
    appLog.Info("tool executed",
        "tool", hc.ToolName,
        "input_length", len(hc.Input),
        "output_length", len(hc.Output),
    )
    return nil
}
```

### 12.5 Stop: 记忆抽取

```go
// 未来扩展
func memoryExtractHook(hc *hookContext) *string {
    // 从对话中抽取记忆
    if stored := memoryStore.extract(hc.Messages); stored {
        appLog.Info("memories extracted")
        // 触发合并
        memoryStore.consolidate()
    }
    return nil
}
```

## 13. 测试要点

### 13.1 Hook 注册测试

1. **registerBuiltinHooks**: 验证所有内置 Hook 正确注册
   - EventUserPromptSubmit: 1 个 Hook
   - EventPreToolUse: 2 个 Hook（permission + log）
   - EventPostToolUse: 1 个 Hook
   - EventStop: 1 个 Hook

2. **registerHook**: 验证回调追加到列表末尾
   - 注册 2 个 Hook → 列表长度 = 2
   - 注册顺序 = 执行顺序

3. **resetHooks**: 验证清空所有事件
   - resetHooks() → 所有事件列表长度 = 0

### 13.2 Hook 分发测试

1. **triggerHooks 空注册表**: 返回 nil
2. **triggerHooks 单个回调**: 调用回调，返回其结果
3. **triggerHooks 多个回调**: 按顺序调用，首个 non-nil 短路
4. **triggerHooks 全部返回 nil**: 返回 nil

### 13.3 短路机制测试

1. **PreToolUse 短路**: permissionHook 拦截 → logHook 不执行
2. **Stop 短路**: summaryHook 返回 non-nil → 后续 Hook 不执行
3. **无短路**: 所有 Hook 返回 nil → 全部执行

### 13.4 内置 Hook 测试

1. **contextInjectHook**: 始终返回 nil
2. **permissionHook**: 
   - denyList 命中 → 返回拦截消息
   - 权限通过 → 返回 nil
3. **logHook**: 始终返回 nil，验证日志记录
4. **largeOutputHook**: 
   - 输出 > 40000 字符 → 打印预警
   - 输出 <= 40000 字符 → 静默
   - 始终返回 nil
5. **summaryHook**: 始终返回 nil，验证统计正确

### 13.5 Hook 上下文测试

1. **UserPromptSubmit**: Query 字段正确
2. **PreToolUse**: ToolName 和 Input 字段正确
3. **PostToolUse**: ToolName、Input、Output 字段正确
4. **Stop**: Messages 字段正确

### 13.6 错误处理测试

1. **Hook panic**: 记录日志，不中断主循环
2. **Hook 返回 error**: 转换为拦截消息
3. **Hook 超时**（未来）: 跳过，使用默认行为

### 13.7 eino 集成测试

1. **OnToolStart 映射**: 验证 PreToolUse Hook 正确触发
2. **OnToolEnd 映射**: 验证 PostToolUse Hook 正确触发
3. **OnAgentFinish 映射**: 验证 Stop Hook 正确触发
4. **拦截语义**: OnToolStart 返回 error → 工具不执行

### 13.8 集成测试

1. **完整流程**: 用户输入 → UserPromptSubmit → PreToolUse → 工具执行 → PostToolUse → Stop
2. **拦截场景**: PreToolUse 拦截 → 工具不执行 → tool_result 返回拦截消息
3. **强制继续**: Stop 返回 non-nil → 注入消息 → 循环继续
4. **多轮对话**: 验证 Hook 在每轮正确触发

## 14. 注意事项

### 14.1 Hook 顺序重要

- PreToolUse: permissionHook 必须在 logHook 之前
- 短路机制保证权限检查失败时不记录日志

### 14.2 返回值语义因事件而异

- PreToolUse: non-nil 拦截工具执行
- Stop: non-nil 强制循环继续
- UserPromptSubmit / PostToolUse: 返回值不影响控制流

### 14.3 Hook 不应有副作用

- Hook 应该只做检查、记录、拦截
- 不应该修改工具参数或结果
- 不应该修改对话历史（除了 Stop Hook 注入消息）

### 14.4 Hook 应该快速执行

- Hook 在主循环中同步执行
- 慢 Hook 会阻塞整个 Agent
- 未来可添加超时机制

### 14.5 Hook 注册表是全局状态

- 测试时需要 resetHooks() 隔离
- 生产环境在 init() 中一次性注册
- 避免运行时动态注册/注销

### 14.6 Hook 上下文是不可变的

- Hook 不应该修改 hookContext
- 只读取需要的字段
- 返回新的字符串（如果需要）
