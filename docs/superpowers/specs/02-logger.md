# 02 - 日志系统规格

## 1. 职责

日志系统负责记录应用程序运行时的诊断信息，包括：

1. 应用运行日志（`coding-agent-go.log`）— 调试和故障排查
2. 对话日志（`messages.json`）— 主 Agent 完整对话记录
3. 子代理对话日志（`messages-sub.json`）— 子 Agent 完整对话记录

## 2. 文件位置

```
internal/logger/logger.go
```

## 3. 依赖

- `log/slog` — Go 标准库结构化日志
- `encoding/json` — 对话日志序列化

## 4. 核心组件

### 4.1 应用日志 (appLog)

```
全局变量: var appLog *slog.Logger

初始化:
- 默认: 丢弃输出 (io.Discard)，测试安全
- initLogger(): 打开 coding-agent-go.log (append 模式)
- 日志级别: Info

格式:
[2006-01-02 15:04:05.000][LEVEL] file:line message key=value key2=value2

字段说明:
- 时间戳: 本地时间，毫秒精度
- 级别: DEBUG/INFO/WARN/ERROR
- 调用点: 文件名:行号（跳过 slog 内部帧）
- 消息: 日志描述
- 键值对: 结构化上下文信息
```

### 4.2 对话日志 (messagesLog)

```
文件路径: messages.json

格式: JSON Lines (每行一个 JSON 对象)

每条记录结构:
{
  "timestamp": "2006-01-02T15:04:05.000Z07:00",
  "turn": 0,
  "role": "user" | "assistant" | "system",
  "system_prompt": "...",  // 仅当 role=system 时
  "messages": [...]        // 完整对话快照
}

写入时机:
- 每轮开始时: 记录 system prompt + 当前 messages
- 收到响应后: 追加 assistant message
- 工具执行后: 追加 tool_result messages
```

### 4.3 子代理对话日志 (subMessagesLog)

```
文件路径: messages-sub.json

格式: 与 messages.json 相同

用途: 记录子代理的独立对话上下文
```

## 5. 核心函数

### 5.1 initLogger()

```
函数签名: func initLogger() error

流程:
1. 打开日志文件 (O_APPEND | O_CREATE | O_WRONLY, 0644)
2. 创建 bracketHandler
   - writer: 文件句柄
   - mutex: 保护并发写入
   - level: slog.LevelInfo
3. 替换全局 appLog
4. 返回 nil (成功) 或 error

错误处理:
- 文件打开失败 → 返回 error，由调用方处理
```

### 5.2 resetLogs()

```
函数签名: func resetLogs() error

流程:
1. 清空 messages.json (截断为 0 字节)
2. 清空 messages-sub.json
3. 不清空 coding-agent-go.log (保留跨进程历史)

错误处理:
- 任一文件操作失败 → 返回 error
- 调用方决定是否继续
```

### 5.3 logMessages()

```
函数签名: func logMessages(turn int, systemPrompt string, messages []anthropic.MessageParam)

流程:
1. 构建日志记录:
   - timestamp: time.Now().Format(time.RFC3339Nano)
   - turn: 当前轮次
   - role: "system"
   - system_prompt: 传入的 system prompt
   - messages: 序列化的对话历史

2. 序列化为 JSON

3. 追加到 messages.json (带换行符)

错误处理:
- 序列化失败 → 记录 appLog.Error，不中断
- 写入失败 → 记录 appLog.Error，不中断
```

### 5.4 logSubMessages()

```
函数签名: func logSubMessages(turn int, systemPrompt string, messages []anthropic.MessageParam)

流程: 与 logMessages 相同，但写入 messages-sub.json
```

### 5.5 logTurnSummary()

```
函数签名: func logTurnSummary(turn int, role string, systemPrompt string, messages []anthropic.MessageParam)

用途: 打印本轮对话摘要到终端

流程:
1. 提取 system prompt 前 100 字符
2. 提取最后一条 user message
3. 打印: "[turn N] role: summary..."
```

### 5.6 logModelResponse()

```
函数签名: func logModelResponse(turn int, resp *anthropic.Message)

用途: 打印模型响应摘要到终端

流程:
1. 提取所有 text block
2. 拼接并截断到 200 字符
3. 打印: "[turn N] assistant: summary..."
```

## 6. bracketHandler 实现

### 6.1 结构体

```go
type bracketHandler struct {
    w     io.Writer      // 输出目标
    mu    *sync.Mutex    // 并发保护
    level slog.Level     // 最低日志级别
    attrs []slog.Attr    // 预置属性
}
```

### 6.2 Enabled()

```
函数签名: func (h *bracketHandler) Enabled(_ context.Context, l slog.Level) bool

返回: l >= h.level
```

### 6.3 Handle()

```
函数签名: func (h *bracketHandler) Handle(_ context.Context, r slog.Record) error

流程:
1. 构建输出缓冲区
2. 写入时间戳: [2006-01-02 15:04:05.000]
3. 写入级别: [INFO]
4. 写入调用点: file:line (通过 getCaller 获取)
5. 写入消息: message
6. 写入属性: key=value key2=value2
7. 写入换行符
8. 加锁写入 h.w
9. 返回 nil

输出示例:
[2026-11-02 10:30:45.123][INFO] main.go:42 user query query=hello
```

### 6.4 getCaller()

```
函数签名: func getCaller() string

流程:
1. runtime.Caller(skip) 获取调用栈
2. skip = 4 (跳过 slog 内部帧: Handle, Enabled, slog 接口, 实际调用)
3. 提取文件名和行号
4. 返回 "file:line" 格式字符串

注意: skip 值需要根据实际调用栈调整，确保显示的是业务代码位置
```

## 7. 日志级别使用规范

| 级别 | 使用场景 | 示例 |
|------|---------|------|
| DEBUG | 开发调试信息 | 工具参数、中间状态 |
| INFO | 正常业务流程 | 用户输入、Agent 启动、工具执行 |
| WARN | 可恢复的异常 | 权限拒绝、压缩失败、重试 |
| ERROR | 不可恢复的错误 | LLM 调用失败、文件读写错误 |

## 8. 对话日志格式详解

### 8.1 messages.json 示例

```json
{"timestamp":"2026-11-02T10:30:45.123Z","turn":0,"role":"system","system_prompt":"You are a coding agent...","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}
{"timestamp":"2026-11-02T10:30:46.456Z","turn":0,"role":"assistant","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"assistant","content":[{"type":"text","text":"Hi! How can I help?"}]}]}
```

### 8.2 字段说明

| 字段 | 类型 | 说明 |
|------|------|------|
| timestamp | string | RFC3339Nano 格式时间戳 |
| turn | int | 对话轮次（从 0 开始） |
| role | string | "system" / "user" / "assistant" |
| system_prompt | string | 仅 role=system 时存在 |
| messages | array | 完整对话快照（anthropic.MessageParam 数组） |

## 9. 并发安全

1. **appLog**: bracketHandler 内置 mutex，支持多 goroutine 并发写入
2. **messages.json**: 单 goroutine 写入（主 Agent 循环），无需额外保护
3. **messages-sub.json**: 子代理各自独立写入，需要 mutex 保护

## 10. 性能考虑

1. **日志文件句柄**: initLogger 时打开，进程生命周期内保持，避免频繁 open/close
2. **bracketHandler**: 每次 Handle 创建新 buffer，可优化为 sync.Pool
3. **对话日志**: 每轮完整快照，大对话时 I/O 开销显著，可考虑增量写入

## 11. 测试要点

1. **日志格式**: 验证输出符合 [时间][级别] 调用点 消息 格式
2. **调用点**: 验证 getCaller 跳过 slog 内部帧
3. **并发写入**: 多 goroutine 同时写 appLog，验证无数据竞争
4. **对话日志**: 验证 messages.json 每行一个完整 JSON
5. **错误处理**: 文件不存在/权限不足时，验证错误正确返回

## 12. 注意事项

1. **日志文件路径**: 硬编码为当前目录，可考虑配置化
2. **日志轮转**: 当前不实现，依赖外部工具（logrotate）
3. **敏感信息**: API Key 等不写入日志
4. **子代理日志**: 需要与主 Agent 日志区分，便于调试
