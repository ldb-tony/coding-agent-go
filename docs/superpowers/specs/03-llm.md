# 03 - LLM 层规格

## 1. 职责

LLM 层负责与语言模型通信，提供统一的接口供 Agent 核心调用：

1. 封装 eino ChatModel（Claude 适配器）
2. 提供 `Send()` 方法，接收 system prompt + messages + tools，返回模型响应
3. 处理消息格式转换（anthropic ↔ eino schema）
4. 处理工具定义转换（eino tool.InvokableTool → anthropic ToolParam）

## 2. 文件位置

```
pkg/agent/llm.go
```

## 3. 依赖

- `github.com/cloudwego/eino-ext/components/model/claude` — Claude 适配器
- `github.com/cloudwego/eino/schema` — eino 消息格式
- `github.com/anthropics/anthropic-sdk-go` — anthropic 消息格式（仅用于类型定义）

## 4. 核心接口

### 4.1 LLMClient

```go
type LLMClient interface {
    // Send 发送对话到 LLM 并返回响应
    //
    // 参数:
    //   - ctx: 上下文（用于超时/取消）
    //   - system: system prompt 文本
    //   - messages: 对话历史（anthropic 格式）
    //   - tools: 可用工具列表（eino 格式）
    //   - maxTokens: 每次生成的最大 token 数
    //
    // 返回:
    //   - *anthropic.Message: 模型响应（anthropic 格式）
    //   - error: 通信错误
    Send(ctx context.Context, system string, messages []anthropic.MessageParam, tools []tool.InvokableTool, maxTokens int64) (*anthropic.Message, error)
}
```

**设计决策**: 
- 接口方法使用 `anthropic.MessageParam` 和 `anthropic.Message`，因为这是 Agent 核心的数据格式
- 工具参数使用 `tool.InvokableTool`，因为这是 eino 的标准接口
- 内部实现负责格式转换

## 5. 数据结构

### 5.1 AgentConfig

```go
type AgentConfig struct {
    Model     string      // 模型 ID，如 "claude-sonnet-4-5"
    BaseURL   string      // 可选，API base URL
    MaxTokens int         // 可选，每次生成的最大 token 数（默认 8000）
    Client    LLMClient   // 可选，注入的客户端（仅用于测试）
}
```

### 5.2 einoClient

```go
type einoClient struct {
    model *claude.ChatModel  // eino Claude 适配器实例
}
```

## 6. 核心函数

### 6.1 NewLLMClient()

```
函数签名: func NewLLMClient(cfg AgentConfig) LLMClient

流程:
1. 验证 cfg.Model 非空
   - 为空 → 打印错误, os.Exit(1)

2. 构建 claude.Config
   - Model: cfg.Model
   - MaxTokens: cfg.MaxTokens
   - BaseURL: &cfg.BaseURL (如果不为空)

3. 处理 API Key
   - 如果 BaseURL 不为空:
     * 清除 ANTHROPIC_AUTH_TOKEN 环境变量（避免冲突）

4. 创建 ChatModel
   - claude.NewChatModel(ctx, config)
   - 失败 → 打印错误, os.Exit(1)

5. 返回 &einoClient{model: chatModel}
```

### 6.2 einoClient.Send()

```
函数签名: func (c *einoClient) Send(ctx, system, messages, tools, maxTokens) (*anthropic.Message, error)

流程:
1. 转换消息格式
   - einoMessages := convertToEinoMessages(system, messages)

2. 绑定工具（如果有）
   - if len(tools) > 0:
     a. 提取工具信息:
        for _, t := range tools:
          info, _ := t.Info(ctx)
          toolInfos = append(toolInfos, info)
     b. 绑定到模型:
        c.model.BindTools(toolInfos)

3. 调用模型
   - resp, err := c.model.Generate(ctx, einoMessages)
   - 失败 → 返回 nil, err

4. 转换响应格式
   - return convertToAnthropicMessage(resp), nil
```

### 6.3 convertToEinoMessages()

```
函数签名: func convertToEinoMessages(system string, messages []anthropic.MessageParam) []*schema.Message

流程:
1. 创建结果数组

2. 添加 system message（如果非空）
   - result = append(result, schema.SystemMessage(system))

3. 遍历 messages，逐条转换:
   
   case "user":
     - 遍历 Content blocks
     - 拼接所有 text block 的 Text
     - result = append(result, schema.UserMessage(content))
   
   case "assistant":
     - 遍历 Content blocks
     - 拼接所有 text block 的 Text
     - result = append(result, schema.AssistantMessage(content, nil))
   
   case "tool_result" (在 user message 中):
     - 暂不处理（eino 会自动处理工具结果）

4. 返回 result

注意: 
- 当前实现只处理 text block，忽略 tool_use/tool_result
- eino ChatModel 会自动处理工具调用的输入输出
```

### 6.4 convertToAnthropicMessage()

```
函数签名: func convertToAnthropicMessage(resp *schema.Message) *anthropic.Message

流程:
1. 创建基础响应
   - msg := &anthropic.Message{
       Role: "assistant",
       Content: []anthropic.ContentBlockUnion{{
         Type: "text",
         Text: resp.Content,
       }},
     }

2. 转换 token 使用情况
   - if resp.ResponseMeta?.Usage != nil:
     msg.Usage = anthropic.Usage{
       InputTokens:  resp.ResponseMeta.Usage.PromptTokens,
       OutputTokens: resp.ResponseMeta.Usage.CompletionTokens,
     }

3. 转换 finish reason
   - if resp.ResponseMeta != nil:
     msg.StopReason = resp.ResponseMeta.FinishReason

4. 转换工具调用
   - if len(resp.ToolCalls) > 0:
     a. 重建 Content 数组:
        - 先添加 text block（如果 resp.Content 非空）
        - 遍历 resp.ToolCalls:
          * argsJSON := json.RawMessage(tc.Function.Arguments)
          * Content = append(Content, {
              Type: "tool_use",
              ID: tc.ID,
              Name: tc.Function.Name,
              Input: argsJSON,
            })

5. 返回 msg
```

## 7. 消息格式转换详解

### 7.1 anthropic → eino

| anthropic.MessageParam | eino.schema.Message |
|------------------------|---------------------|
| Role: "user" | Role: schema.User |
| Role: "assistant" | Role: schema.Assistant |
| Content: [TextBlock] | Content: string (拼接) |
| Content: [ToolUseBlock] | ToolCalls: []ToolCall |
| Content: [ToolResultBlock] | (由 eino 自动处理) |

### 7.2 eino → anthropic

| eino.schema.Message | anthropic.Message |
|---------------------|-------------------|
| Role: schema.User | Role: "user" |
| Role: schema.Assistant | Role: "assistant" |
| Content: string | Content: [TextBlock] |
| ToolCalls: []ToolCall | Content: [ToolUseBlock] |
| ResponseMeta.Usage | Usage |
| ResponseMeta.FinishReason | StopReason |

## 8. 工具定义转换

### 8.1 eino tool.InvokableTool → anthropic ToolParam

```
流程:
1. 调用 tool.Info(ctx) 获取 *schema.ToolInfo
2. 构建 anthropic.ToolParam:
   - Name: info.Name
   - Description: info.Desc
   - InputSchema: 从 info.ParamsOneOf 提取 JSON Schema

3. 提取 JSON Schema:
   - jsonSchema, _ := info.ParamsOneOf.ToJSONSchema()
   - schemaJSON, _ := json.Marshal(jsonSchema)
   - var inputSchema anthropic.ToolInputSchemaParam
   - json.Unmarshal(schemaJSON, &inputSchema)

4. 包装为 ToolUnionParam:
   - anthropic.ToolUnionParam{OfTool: &anthropicTool}
```

## 9. 错误处理

| 错误场景 | 处理方式 |
|---------|---------|
| Model ID 为空 | 打印错误, os.Exit(1) |
| ChatModel 创建失败 | 打印错误, os.Exit(1) |
| 工具信息提取失败 | 返回 error |
| 工具绑定失败 | 返回 error |
| 模型调用失败 | 返回 error |
| 消息转换失败 | 记录日志, 返回原始内容 |

## 10. 配置选项

### 10.1 支持的模型参数

| 参数 | 类型 | 说明 |
|------|------|------|
| Model | string | 模型 ID（必需） |
| BaseURL | string | 自定义 API 端点 |
| MaxTokens | int | 最大输出 token 数 |
| Temperature | float32 | 温度（可选，默认由模型决定） |

### 10.2 环境变量

| 变量 | 说明 |
|------|------|
| MODEL_ID | 模型 ID |
| ANTHROPIC_BASE_URL | API 端点 |
| ANTHROPIC_API_KEY | API Key |
| MAX_TOKENS | 最大 token 数 |

## 11. 测试要点

### 11.1 单元测试

1. **消息转换**: 
   - user message → eino UserMessage
   - assistant message → eino AssistantMessage
   - 多 text block → 拼接

2. **响应转换**:
   - text response → anthropic TextBlock
   - tool_calls → anthropic ToolUseBlock
   - usage → anthropic Usage

3. **工具定义转换**:
   - tool.Info() → anthropic ToolParam
   - JSON Schema 正确提取

### 11.2 集成测试

1. **真实 API 调用**:
   - 发送简单对话
   - 验证响应格式
   - 验证 token 计数

2. **工具调用**:
   - 绑定工具
   - 模型返回 tool_use
   - 验证 tool_use 格式正确

## 12. 性能考虑

1. **消息转换**: 每次 Send 都重新转换所有消息，大对话时开销显著
   - 优化方向: 缓存已转换的消息

2. **工具绑定**: 每次 Send 都重新绑定所有工具
   - 优化方向: 工具不变时复用绑定

3. **JSON 序列化**: 工具定义的 JSON Schema 转换涉及多次序列化
   - 优化方向: 预计算并缓存

## 13. 注意事项

1. **API Key 处理**: 
   - 使用自定义 BaseURL 时，必须清除 ANTHROPIC_AUTH_TOKEN
   - 避免环境变量冲突

2. **工具调用语义**:
   - eino ChatModel 会自动处理工具调用的输入输出
   - 不需要手动构建 tool_result message

3. **流式输出**:
   - 当前实现使用 Generate()（同步）
   - 流式支持需要改用 Stream()（后续扩展）

4. **错误重试**:
   - 当前不实现自动重试
   - 由 Agent 核心处理 prompt_too_long 等特定错误
