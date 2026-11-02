# 17 - 上下文压缩系统规格

## 1. 职责

上下文压缩系统负责在对话接近模型上下文窗口限制时，自动压缩历史消息，包括：

1. **tool_result_budget** — 单轮大工具结果落盘，替换为预览
2. **micro_compact** — 空闲期清理旧的工具结果
3. **auto_compact** — 超阈值时调用 LLM 生成摘要
4. **reactive_compact** — prompt_too_long 错误时的兜底压缩

## 2. 设计理念

- **三步压缩流水线**，成本与信息损失递增：
  1. `tool_result_budget` — 零成本，仅截断大结果
  2. `micro_compact` — 零成本，清理空闲期旧结果
  3. `auto_compact` — 有成本（调 LLM），生成语义摘要
- **reactive_compact** 作为最后防线，在 API 返回 prompt_too_long 时触发
- 压缩策略从保守到激进，尽量保留近期上下文

## 3. 文件位置

```
pkg/agent/compact.go        # compactor 实现
```

## 4. 依赖

- `encoding/json` — 消息序列化用于 token 估算
- `os` — 落盘文件读写
- `path/filepath` — 目录构建
- `time` — 空闲时间判断
- `context` — 超时控制
- `fmt` — 错误消息
- `strings` — 字符串处理
- LLMClient — 摘要生成（auto_compact 使用）

## 5. 关键常量

### 5.1 Step 1: tool_result_budget

```go
const toolResultBudgetChars   = 200000   // 一轮 tool_result 合计上限
const largeResultCharLimit    = 30000    // 单条大结果落盘门槛
const largeResultPreviewChars = 2000     // 落盘后预览长度
```

### 5.2 窗口与阈值

```go
const defaultContextWindow          = 200000  // 默认上下文窗口（tokens）
const maxOutputTokensForSummary     = 20000   // 摘要时预留的输出 token
const autoCompactBufferTokens       = 13000   // 触发提前量
const maxConsecutiveCompactFailures = 3       // 连续失败熔断阈值
```

### 5.3 Step 2: micro_compact

```go
const mcGapMinutes       = 60     // 距上次响应 >= 60 分钟才触发
const mcKeepRecent       = 5      // 保留最近 5 个可压缩结果
const clearedPlaceholder = "[Old tool result content cleared]"
```

### 5.4 Step 3: auto_compact

```go
const autoCompactKeepRecentMsgs = 4      // 保留最近 4 条消息不压缩
const summaryMaxTokens          = 8000   // 摘要最大 token 数
const recentFileMaxCount        = 5      // 附件中最近文件最大数量
const recentFileTokensEach      = 5000   // 每个文件最大 token
const recentFileTokensTotal     = 50000  // 所有文件合计最大 token
const skillTokensEach           = 5000   // 每个技能最大 token
const skillTokensTotal          = 25000  // 所有技能合计最大 token
const charsPerToken             = 4      // 粗估: 字符数 / 4 = token 数
```

### 5.5 reactive_compact

```go
const reactiveKeepRecent = 5     // 保留最近 5 条消息
const maxReactiveRetries = 1     // 最大重试次数
```

### 5.6 compactableTools

```go
// micro_compact 白名单: 只有这些工具的 tool_result 可被清理
var compactableTools = map[string]bool{
    "read_file":  true,
    "bash":       true,
    "grep":       true,
    "glob":       true,
    "write_file": true,
    "edit_file":  true,
}
```

## 6. 数据结构

### 6.1 compactor

```go
type compactor struct {
    baseDir        string      // 项目根目录
    transcriptsDir string      // .transcripts/ — 对话记录
    toolResultsDir string      // .task_outputs/tool-results/ — 落盘结果

    client        LLMClient   // LLM 客户端（用于 auto_compact 生成摘要）
    contextWindow int         // 上下文窗口大小（tokens）

    anchorTokens   int64      // 上次成功响应的总 token 计数
    anchorMsgCount int        // 上次成功响应时的消息数量
    hasAnchor      bool       // 是否已有锚点
    lastAssistantAt time.Time // 上次助手响应时间

    consecutiveFailures int   // 连续压缩失败次数
}
```

### 6.2 摘要 Prompt (summaryPrompt)

```
CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.

- Do NOT use read_file, bash, grep, glob, edit_file, write_file, or ANY other tool.
- You already have all the context you need in the conversation.
- Tool calls will be REJECTED and will waste your only turn.

Your task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests and your previous actions, with a focus on why you made decisions and what you learned.

Among other important areas, you should take special care to accurately summarize:

1. Primary Request and Intent:
   Describe the user's original request in detail, including any specific requirements, constraints, or preferences they mentioned.

2. Key Technical Concepts:
   List all important technical concepts discussed, including programming languages, frameworks, libraries, and design patterns.

3. Files and Code Sections:
   For each file discussed, note the full file path, explain why it is relevant, and summarize the changes made or proposed. Include code snippets only when the exact text is load-bearing.

4. Errors and Fixes:
   Document any errors encountered and how they were resolved.

5. Problem Solving:
   Summarize problems identified and solutions implemented.

6. All User Messages:
   Include a verbatim list of all user messages (not just summaries). This is critical for maintaining context.

7. Pending Tasks:
   List any tasks that were explicitly requested but not yet completed. Include task IDs if applicable.

8. Current Work:
   Describe in detail the work that was in progress immediately before this summary was requested. Include file paths and code snippets as appropriate.

9. Optional Next Step:
   If the user's request is still being worked on, describe the next step that should be taken. Include direct quotes from the user's request to ensure accuracy.
```

## 7. 核心函数

### 7.1 newCompactor()

```
函数签名: func newCompactor(baseDir string, client LLMClient) *compactor

流程:
1. 构建目录路径
   - transcriptsDir = filepath.Join(baseDir, ".transcripts")
   - toolResultsDir = filepath.Join(baseDir, ".task_outputs", "tool-results")

2. 创建 compactor 实例
   - c := &compactor{
       baseDir:        baseDir,
       transcriptsDir: transcriptsDir,
       toolResultsDir: toolResultsDir,
       client:         client,
       contextWindow:  defaultContextWindow,  // 200000
   }

3. 返回 c
```

### 7.2 observeUsage()

```
函数签名: func (c *compactor) observeUsage(usage Usage, msgCount int)

流程:
1. 计算总 token 数
   - totalTokens := usage.InputTokens + usage.OutputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens

2. 更新锚点
   - c.anchorTokens = totalTokens
   - c.anchorMsgCount = msgCount
   - c.hasAnchor = true
   - c.lastAssistantAt = time.Now()

用途: 每次收到助手响应后调用，记录当前 token 使用量作为压缩决策的基准
```

### 7.3 prepare()

```
函数签名: func (c *compactor) prepare(ctx context.Context, st *sessionState, messages []MessageParam, activeRequest string) ([]MessageParam, error)

流程:
1. Step 1: tool_result_budget
   - messages = c.toolResultBudget(messages)

2. Step 2: micro_compact（如果空闲 >= 60 分钟）
   - if c.hasAnchor && time.Since(c.lastAssistantAt) >= mcGapMinutes * time.Minute:
       messages = c.microCompact(messages)

3. Step 3: auto_compact（如果 token 估算超阈值）
   - messages, err = c.autoCompact(ctx, messages, activeRequest)
   - if err != nil → 返回 nil, err

4. 返回 messages, nil
```

### 7.4 toolResultBudget()

```
函数签名: func (c *compactor) toolResultBudget(messages []MessageParam) []MessageParam

流程:
1. 统计当轮所有 tool_result 的总字符数
   - totalChars := 0
   - for _, msg := range messages:
       if msg.Role == "user":
         for _, block := range msg.Content:
           if block.Type == "tool_result":
             totalChars += len(block.Text)

2. 如果未超上限 → 原样返回
   - if totalChars <= toolResultBudgetChars (200000):
       return messages

3. 超上限: 找到最大的 tool_result 并落盘
   - 遍历所有 tool_result block
   - 找到字符数最大的一个
   - if 最大 block 字符数 > largeResultCharLimit (30000):
     a. 确保 toolResultsDir 存在 (os.MkdirAll)
     b. 写入文件: toolResultsDir/{tool_call_id}.txt
     c. 生成预览: 前 largeResultPreviewChars (2000) 字符
     d. 替换 block 内容为:
        preview + "\n\n...truncated and saved to " + relPath + "..."
     e. 重新计算 totalChars
     f. 如果仍超上限 → 递归处理下一个最大的（循环直到不超限或无更多大结果）

4. 返回处理后的 messages
```

### 7.5 microCompact()

```
函数签名: func (c *compactor) microCompact(messages []MessageParam) []MessageParam

流程:
1. 前置检查
   - if !c.hasAnchor → return messages
   - if time.Since(c.lastAssistantAt) < mcGapMinutes * time.Minute → return messages

2. 收集可压缩的 tool_result 索引
   - compressible := []int{}
   - for i, msg := range messages:
       if msg.Role == "user":
         for _, block := range msg.Content:
           if block.Type == "tool_result" && compactableTools[block.ToolName]:
             compressible = append(compressible, i)

3. 保留最近 mcKeepRecent (5) 个
   - if len(compressible) <= mcKeepRecent → return messages
   - toCompress := compressible[:len(compressible)-mcKeepRecent]

4. 替换为占位符
   - result := make([]MessageParam, len(messages))
   - copy(result, messages)
   - for _, idx := range toCompress:
       result[idx].Content = []ContentBlock{{
           Type: "text",
           Text: clearedPlaceholder,
       }}

5. 返回 result
```

### 7.6 autoCompact()

```
函数签名: func (c *compactor) autoCompact(ctx context.Context, messages []MessageParam, activeRequest string) ([]MessageParam, error)

流程:
1. 估算当前 token 数
   - jsonBytes, _ := json.Marshal(messages)
   - estimatedTokens := len(jsonBytes) / charsPerToken  // charsPerToken = 4

2. 计算阈值
   - maxOutputTokens := cfg.MaxTokens  // 通常 8000
   - threshold := c.contextWindow - maxOutputTokens - autoCompactBufferTokens
   // 200000 - 8000 - 13000 = 179000（实际按配置）

3. 未超阈值 → 原样返回
   - if estimatedTokens <= threshold → return messages, nil

4. 熔断检查
   - if c.consecutiveFailures >= maxConsecutiveCompactFailures (3):
       return nil, fmt.Errorf("auto-compact fused after %d consecutive failures", c.consecutiveFailures)

5. 分离近期消息
   - keepCount := autoCompactKeepRecentMsgs  // 4
   - if len(messages) <= keepCount → return messages, nil（无法压缩）
   - oldMessages := messages[:len(messages)-keepCount]
   - recentMessages := messages[len(messages)-keepCount:]

6. 构建摘要 prompt
   - prompt := summaryPrompt
   - prompt += "\n\nConversation to summarize:\n"
   - for _, msg := range oldMessages:
       prompt += formatMessage(msg)

7. 调用 LLM 生成摘要
   - summaryMsg := []MessageParam{{Role: "user", Content: [{Type: "text", Text: prompt}]}}
   - resp, err := c.client.Send(ctx, "", summaryMsg, nil, summaryMaxTokens)
   - if err != nil:
       c.consecutiveFailures++
       return nil, fmt.Errorf("summary generation failed: %w", err)

8. 构建压缩后的消息
   - compactedSummary := "[Compacted] " + resp.Text
   - newMessages := []MessageParam{
       {Role: "user", Content: [{Type: "text", Text: compactedSummary}]},
     }
   - 添加附件（最近文件、plan、skills）
   - newMessages = append(newMessages, recentMessages...)

9. 成功: 重置失败计数
   - c.consecutiveFailures = 0

10. 返回 newMessages, nil

错误处理:
- LLM 调用失败 → consecutiveFailures++，返回 error
- 连续失败 >= 3 次 → 熔断，返回 error
- 消息数 <= keepCount → 不压缩，原样返回
```

### 7.7 reactiveCompact()

```
函数签名: func (c *compactor) reactiveCompact(ctx context.Context, messages []MessageParam, activeRequest string) []MessageParam

流程:
1. 保留最近消息
   - keepCount := reactiveKeepRecent  // 5
   - if len(messages) <= keepCount → return messages

2. 分离旧消息和近期消息
   - oldMessages := messages[:len(messages)-keepCount]
   - recentMessages := messages[len(messages)-keepCount:]

3. 构建简单摘要（不调 LLM，纯占位符）
   - summary := fmt.Sprintf("[Compacted: %d messages removed to free context space]", len(oldMessages))

4. 构建新消息
   - newMessages := []MessageParam{
       {Role: "user", Content: [{Type: "text", Text: summary}]},
     }
   - newMessages = append(newMessages, recentMessages...)

5. 返回 newMessages

用途: 在 API 返回 prompt_too_long 错误时作为兜底，快速释放上下文空间
```

### 7.8 compactHistory()

```
函数签名: func (c *compactor) compactHistory(ctx context.Context, st *sessionState, messages []MessageParam, activeRequest string) ([]MessageParam, error)

流程:
1. 等同于调用 autoCompact
   - return c.autoCompact(ctx, messages, activeRequest)

用途: 手动触发（用户调用 compact 工具时）
```

## 8. 算法细节

### 8.1 Token 估算

```
方法: JSON 序列化后字符数 / charsPerToken (4)
精度: 粗糙估计，误差约 ±20%
用途: 判断是否需要触发 auto_compact

公式:
  jsonBytes, _ = json.Marshal(messages)
  estimatedTokens = len(jsonBytes) / 4

阈值计算:
  threshold = contextWindow - maxOutputTokens - buffer
  = 200000 - 8000 - 13000 = 179000
```

### 8.2 落盘策略 (tool_result_budget)

```
触发条件: 单轮所有 tool_result 合计 > 200000 字符
处理方式:
  1. 找到最大的单条 tool_result
  2. 如果 > 30000 字符:
     a. 完整内容写入 .task_outputs/tool-results/{tool_call_id}.txt
     b. 替换为前 2000 字符 + 截断提示
  3. 重新计算合计，如果仍超限则继续处理下一个
```

### 8.3 空闲检测 (micro_compact)

```
触发条件: time.Since(lastAssistantAt) >= 60 分钟
含义: 用户离开超过 1 小时后回来继续对话
策略:
  - 保留最近 5 个可压缩工具的 tool_result
  - 其余替换为 "[Old tool result content cleared]"
  - 只处理 compactableTools 白名单中的工具
```

### 8.4 熔断机制

```
目的: 防止连续压缩失败导致无限循环
规则:
  - 每次 autoCompact 失败 → consecutiveFailures++
  - 每次 autoCompact 成功 → consecutiveFailures = 0
  - consecutiveFailures >= 3 → 熔断，返回错误
  - 熔断后需要用户手动干预或等待条件变化
```

### 8.5 消息保留策略

```
auto_compact:
  - 保留最近 4 条消息（autoCompactKeepRecentMsgs）
  - 旧消息由 LLM 生成摘要替代
  - 附件: 最近文件 + plan + skills

reactive_compact:
  - 保留最近 5 条消息（reactiveKeepRecent）
  - 旧消息直接替换为占位符（不调 LLM）
  - 速度优先，质量次之
```

## 9. 目录结构

```
{baseDir}/
├── .transcripts/              # 对话记录（transcriptsDir）
│   └── *.json
└── .task_outputs/
    └── tool-results/          # 落盘的大工具结果（toolResultsDir）
        └── {tool_call_id}.txt
```

## 10. eino middleware 集成

### 10.1 reduction middleware

替代 `toolResultBudget`，配置参数：

```go
import "github.com/cloudwego/eino/adk/middlewares/reduction"

reductionMiddleware := reduction.NewMiddleware(&reduction.Config{
    MaxResultSize: largeResultCharLimit,    // 30000
    BudgetChars:   toolResultBudgetChars,   // 200000
    PreviewChars:  largeResultPreviewChars, // 2000
    OutputDir:     toolResultsDir,
})
```

### 10.2 summarization middleware

替代 `autoCompact`，配置参数：

```go
import "github.com/cloudwego/eino/adk/middlewares/summarization"

summaryMiddleware := summarization.NewMiddleware(&summarization.Config{
    TokenThreshold:    autoCompactThreshold,  // ~179000
    MaxSummaryTokens:  summaryMaxTokens,      // 8000
    KeepRecentMsgs:    autoCompactKeepRecentMsgs,  // 4
    SummaryPrompt:     summaryPrompt,
    MaxFailures:       maxConsecutiveCompactFailures,  // 3
})
```

### 10.3 对应关系

| 手写实现 | eino middleware |
|---------|----------------|
| toolResultBudget() | reduction middleware |
| autoCompact() | summarization middleware |
| microCompact() | 无直接对应，需手写 |
| reactiveCompact() | 无直接对应，需手写 |
| prepare() | middleware 链自动编排 |

## 11. 错误处理

| 错误场景 | 处理方式 | 是否中断 |
|---------|---------|---------|
| toolResultsDir 创建失败 | 跳过落盘，保留原始内容 | 否 |
| 文件写入失败 | 跳过该结果，记录日志 | 否 |
| JSON 序列化失败 | 使用保守估算，触发压缩 | 否 |
| LLM 摘要调用失败 | consecutiveFailures++，返回 error | 是 |
| 连续失败 >= 3 | 熔断，返回 error | 是 |
| 消息数 <= keepCount | 不压缩，原样返回 | 否 |
| reactive 压缩后仍超限 | 返回 error，由上层处理 | 是 |

## 12. 测试要点

### 12.1 单元测试

1. **toolResultBudget — 不触发**:
   - 构造总字符 < 200000 的 messages
   - 验证原样返回

2. **toolResultBudget — 触发落盘**:
   - 构造总字符 > 200000 的 messages，含一条 > 30000 的结果
   - 验证文件写入正确路径
   - 验证替换后内容为预览 + 截断提示

3. **toolResultBudget — 多条大结果**:
   - 构造多条 > 30000 的结果
   - 验证逐个处理直到总字符 < 200000

4. **microCompact — 未空闲**:
   - lastAssistantAt 在 30 分钟前
   - 验证不触发压缩

5. **microCompact — 已空闲**:
   - lastAssistantAt 在 90 分钟前
   - 构造 10 个可压缩结果
   - 验证保留最近 5 个，其余替换为占位符

6. **microCompact — 白名单过滤**:
   - 包含 compactableTools 内外的工具结果
   - 验证只压缩白名单内的

7. **autoCompact — 未超阈值**:
   - 估算 token < threshold
   - 验证原样返回

8. **autoCompact — 超阈值**:
   - 估算 token > threshold
   - 验证调用 LLM 生成摘要
   - 验证新消息结构: [摘要] + [最近 4 条]

9. **autoCompact — 熔断**:
   - 连续失败 3 次
   - 验证返回熔断错误

10. **autoCompact — 成功重置计数**:
    - 先失败 2 次，再成功 1 次
    - 验证 consecutiveFailures = 0

11. **reactiveCompact — 正常压缩**:
    - 构造 10 条消息
    - 验证保留最近 5 条 + 摘要占位符

12. **reactiveCompact — 消息数不足**:
    - 构造 3 条消息（< reactiveKeepRecent）
    - 验证原样返回

13. **observeUsage — 锚点更新**:
    - 调用 observeUsage
    - 验证 anchorTokens、hasAnchor、lastAssistantAt 正确更新

### 12.2 集成测试

1. **完整流水线**:
   - 构造大对话 → prepare() → 验证三步依次执行

2. **LLM 摘要端到端**:
   - mock LLM client → autoCompact → 验证摘要内容注入

3. **落盘文件可恢复**:
   - toolResultBudget 落盘 → 读取文件 → 验证内容完整

4. **熔断恢复**:
   - 连续失败触发熔断 → 修复后重置 → 验证恢复正常

## 13. 性能基准

| 指标 | 目标值 | 测量方法 |
|------|--------|---------|
| toolResultBudget 耗时 | < 10ms | 100 条消息 |
| microCompact 耗时 | < 5ms | 100 条消息 |
| autoCompact 耗时 | < 5s | 含 LLM 调用 |
| reactiveCompact 耗时 | < 1ms | 纯内存操作 |
| 内存占用增量 | < 50MB | 落盘后 |

## 14. 注意事项

1. **token 估算精度**: 使用字符数/4 是粗糙估计，实际 token 数可能偏差 ±20%。阈值设置留有 buffer（autoCompactBufferTokens = 13000）来吸收误差。

2. **落盘文件清理**: 当前不自动清理 .task_outputs/tool-results/ 中的旧文件，需要外部清理策略。

3. **micro_compact 的不可逆性**: 一旦 tool_result 被替换为占位符，原始内容无法恢复（除非从落盘文件读取）。

4. **reactive_compact 不调 LLM**: 与 auto_compact 不同，reactive 版本使用简单占位符替代摘要，以速度为优先。

5. **并发安全**: compactor 设计为单线程使用（在 agent loop 中顺序调用），无需加锁。

6. **摘要质量**: summaryPrompt 明确要求保留 9 个维度的信息，确保压缩后不丢失关键上下文。
