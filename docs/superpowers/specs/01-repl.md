# 01 - REPL 入口规格

## 1. 职责

REPL (Read-Eval-Print Loop) 是应用程序的入口，负责：

1. 初始化日志系统
2. 初始化命令行交互
3. 处理信号（Ctrl-C、Ctrl-D）
4. 循环读取用户输入并调用 Agent
5. 打印模型响应

## 2. 文件位置

```
cmd/agent/main.go
```

## 3. 依赖

- `github.com/chzyer/readline` — 命令行交互
- `pkg/agent/` — Agent 核心
- `internal/logger/` — 日志系统

## 4. 数据结构

### 4.1 无复杂数据结构

REPL 层只维护：
- `readline.Instance` — 命令行实例
- `[]anthropic.MessageParam` — 对话历史（跨轮次保留）

## 5. 核心函数

### 5.1 main()

```
函数签名: func main()

流程:
1. 初始化日志 (initLogger)
   - 失败时打印错误并退出 (exit code 1)

2. 注册 panic 处理器
   - defer func { recover() -> 打印堆栈到日志, exit code 2 }

3. 重置日志文件 (resetLogs)
   - 清空 messages.json, messages-sub.json, coding-agent-go.log
   - 失败时打印警告但不退出

4. 注册信号处理器 (SIGINT)
   - 启动 goroutine 监听
   - 收到信号时:
     a. 关闭 readline (还原终端设置)
     b. 清理后台任务 (backgroundTasks.stopAll)
     c. exit code 0

5. 打印欢迎信息
   - "coding-agent-go — teaching coding agent"
   - "Conversation logged to messages.json"
   - "Sub-agent conversations logged to messages-sub.json"
   - "Enter a question, press Enter to send. Type q to quit."

6. 创建 Agent 配置 (DefaultAgentConfig)
   - 从环境变量加载:
     * MODEL_ID (必需)
     * ANTHROPIC_BASE_URL (可选)
     * MAX_TOKENS (可选, 默认 8000)

7. 初始化对话历史 (history = [])

8. 创建 readline 实例
   - Prompt: "\033[36magent >> \033[0m" (青色提示符)
   - HistoryFile: ".coding-agent-history"

9. 主循环 (for {})
   a. 读取用户输入 (rl.Readline)
      - 错误处理:
        * ErrInterrupt → break (Ctrl-C)
        * EOF → break (Ctrl-D)
        * 其他错误 → 打印错误, break

   b. 预处理输入
      - 转小写并 trim 空格
      - 空字符串 → continue (仅重新提示)
      - "q" 或 "exit" → break

   c. 记录日志
      - appLog.Info("user query", "query", query)

   d. 触发 Hook
      - triggerHooks(EventUserPromptSubmit, &hookContext{Query: query})

   e. 追加用户消息到历史
      - history = append(history, NewUserMessage(query))

   f. 调用 Agent
      - history, err = agentLoop(ctx, cfg, history, query)
      - 错误处理:
        * 打印错误到 stderr
        * continue (不退出)

   g. 打印模型响应
      - 遍历 history 最后一条消息的 Content blocks
      - 打印所有 text block

   h. 打印空行分隔

10. 退出前清理
    - backgroundTasks.stopAll()
```

### 5.2 DefaultAgentConfig()

```
函数签名: func DefaultAgentConfig() AgentConfig

流程:
1. 加载 .env 文件 (godotenv.Overload)

2. 读取环境变量:
   - Model: os.Getenv("MODEL_ID")
   - BaseURL: os.Getenv("ANTHROPIC_BASE_URL")
   - MaxTokens: 8000 (默认值)

3. 可选: 读取 MAX_TOKENS 环境变量覆盖默认值

4. 返回 AgentConfig 结构体
```

## 6. 环境变量

| 变量 | 必需 | 默认值 | 说明 |
|------|------|--------|------|
| `MODEL_ID` | 是 | — | 模型 ID，如 "claude-sonnet-4-5" |
| `ANTHROPIC_BASE_URL` | 否 | "" | 自定义 API 端点 |
| `ANTHROPIC_API_KEY` | 是 | — | API Key |
| `MAX_TOKENS` | 否 | 8000 | 每次生成的最大 token 数 |

## 7. 文件路径

| 文件 | 用途 |
|------|------|
| `.coding-agent-history` | readline 命令历史（跨进程保留） |
| `messages.json` | 主 Agent 对话日志 |
| `messages-sub.json` | 子 Agent 对话日志 |
| `coding-agent-go.log` | 应用运行日志 |

## 8. 错误处理

### 8.1 初始化阶段

| 错误 | 处理 | Exit Code |
|------|------|-----------|
| 日志初始化失败 | 打印错误, 退出 | 1 |
| 日志重置失败 | 打印警告, 继续 | — |
| readline 初始化失败 | 打印错误, 退出 | 1 |
| Agent 配置无效 | 打印错误, 退出 | 1 |

### 8.2 运行阶段

| 错误 | 处理 |
|------|------|
| 用户输入 EOF | 正常退出 |
| 用户输入 Ctrl-C | 正常退出 |
| readline 错误 | 打印错误, 退出循环 |
| agentLoop 错误 | 打印错误, 继续循环 |

### 8.3 退出阶段

| 场景 | 清理动作 |
|------|---------|
| 正常退出 (q/EOF) | stopAll() |
| Ctrl-C | stopAll() |
| panic | 打印堆栈, stopAll() |

## 9. 输出格式

### 9.1 欢迎信息

```
coding-agent-go — teaching coding agent
Conversation logged to messages.json
Sub-agent conversations logged to messages-sub.json
Enter a question, press Enter to send. Type q to quit.

```

### 9.2 提示符

```
青色 "agent >> "
```

### 9.3 模型响应

直接打印 text block 内容，多个 block 之间无分隔，最后打印一个空行。

## 10. 测试要点

1. **正常流程**: 输入问题 → 收到响应 → 继续输入
2. **退出条件**: q / exit / Ctrl-C / Ctrl-D
3. **错误恢复**: agentLoop 失败后仍可继续输入
4. **历史保留**: 重启后可用 ↑↓ 翻页
5. **信号处理**: Ctrl-C 时终端设置正确还原

## 11. 注意事项

1. **readline 必须在主 goroutine 创建** — 终端设置与进程绑定
2. **信号处理器要提前声明 rl 变量** — 闭包捕获，确保 exit 前能 Close
3. **空输入仅重新提示，不退出** — 对齐参考实现
4. **退出前必须 stopAll()** — 清理后台任务进程组
5. **panic 处理器必须打印堆栈** — 便于定位问题
