# 19 - 集成与测试规格

## 1. 职责

本文档描述系统的端到端集成流程和完整测试矩阵，包括：

1. 应用初始化流程
2. 每轮对话的完整数据流
3. 子代理委派流程
4. 所有模块的测试场景和验证点
5. 性能基准指标
6. 验收清单

## 2. 端到端集成流程

### 2.1 初始化阶段

```
main() →
  │
  ├── initLogger()
  │     └── 打开 coding-agent-go.log (append 模式)
  │     └── 创建 bracketHandler
  │     └── 替换全局 appLog
  │
  ├── resetLogs()
  │     └── 截断 messages.json → 0 字节
  │     └── 截断 messages-sub.json → 0 字节
  │     └── 保留 coding-agent-go.log（跨进程历史）
  │
  ├── DefaultAgentConfig()
  │     └── godotenv.Overload() 加载 .env
  │     └── 读取 MODEL_ID（必需）
  │     └── 读取 ANTHROPIC_BASE_URL（可选）
  │     └── 读取 MAX_TOKENS（可选，默认 8000）
  │     └── 返回 AgentConfig
  │
  ├── readline 初始化
  │     └── Prompt: "\033[36magent >> \033[0m"
  │     └── HistoryFile: ".coding-agent-history"
  │
  ├── 注册信号处理器 (SIGINT)
  │     └── goroutine 监听
  │     └── 收到信号 → rl.Close() → stopAll() → exit(0)
  │
  ├── 打印欢迎信息
  │     └── "coding-agent-go — teaching coding agent"
  │     └── "Conversation logged to messages.json"
  │     └── "Sub-agent conversations logged to messages-sub.json"
  │     └── "Enter a question, press Enter to send. Type q to quit."
  │
  └── 进入主循环 (for {})
```

### 2.2 每轮对话流程

```
用户输入 (readline.Readline)
    │
    ├── 错误处理:
    │     ErrInterrupt → break (Ctrl-C)
    │     EOF → break (Ctrl-D)
    │     其他 → 打印错误, break
    │
    ├── 预处理:
    │     query = strings.ToLower(strings.TrimSpace(input))
    │     "" → continue
    │     "q" | "exit" → break
    │
    ├── 记录日志:
    │     appLog.Info("user query", "query", query)
    │
    ├── 触发 Hook:
    │     triggerHooks(EventUserPromptSubmit, &hookContext{Query: query})
    │
    ├── 追加用户消息:
    │     history = append(history, NewUserMessage(query))
    │
    └── agentLoop(ctx, cfg, history, query):
          │
          ├── newSessionState(cfg)
          │     └── workDir = os.Getwd()
          │     └── gitBranch = getGitBranch(workDir)
          │     └── skillLoader = newSkillLoader("skills/")
          │     └── skillLoader.scan()
          │     └── mcpServers = connectMCPServers()
          │
          ├── newCompactor(workDir, client)
          │
          ├── st.recallMemories(query)
          │     └── LLM 选择相关记忆
          │     └── 关键词回退
          │
          └── 主循环 (for turn = 0; turn < maxTurns; turn++):
                │
                ├── injectBackgroundResults(messages)
                │     └── 收集已完成的后台任务结果
                │     └── 注入为 tool_result 消息
                │
                ├── cpt.prepare(ctx, st, messages, query)
                │     ├── Step 1: toolResultBudget(messages)
                │     ├── Step 2: microCompact(messages)  [如果空闲 >= 60 分钟]
                │     └── Step 3: autoCompact(ctx, messages, query)  [如果超阈值]
                │
                ├── logMessages(turn, systemPrompt(st), messages)
                │
                ├── client.Send(ctx, systemPrompt(st), messages, tools, maxTokens)
                │     └── 消息格式转换 (anthropic ↔ eino)
                │     └── 工具定义转换
                │     └── 调用 LLM API
                │     └── 响应格式转换
                │
                ├── 处理响应:
                │     │
                │     ├── 无 tool_use (StopReason == "end_turn"):
                │     │     └── 提取 text blocks
                │     │     └── observeUsage(resp.Usage, len(messages))
                │     │     └── 退出主循环
                │     │
                │     └── 有 tool_use (StopReason == "tool_use"):
                │           │
                │           ├── 提取 tool_use blocks
                │           ├── 追加 assistant message 到 history
                │           │
                │           └── executeToolBatch(ctx, st, toolUses):
                │                 │
                │                 ├── 调度分析:
                │                 │     独立读操作 → 并发执行
                │                 │     写操作 → 串行执行
                │                 │     有依赖 → 按序执行
                │                 │
                │                 ├── 对每个 tool_use:
                │                 │     │
                │                 │     ├── triggerHooks(PreToolUse, toolName, args)
                │                 │     │     └── 权限检查:
                │                 │     │           deny list → 硬拒
                │                 │     │           规则匹配 → 破坏性检测
                │                 │     │           用户确认 → y/yes 放行
                │                 │     │
                │                 │     ├── 调度执行:
                │                 │     │     tool.Execute(ctx, args)
                │                 │     │
                │                 │     └── triggerHooks(PostToolUse, toolName, result)
                │                 │           └── 输出检查
                │                 │
                │                 └── 追加 tool_result messages 到 history
                │
                ├── todo 提醒检查:
                │     └── 如果最近响应包含 todo_write 调用
                │     └── 提醒用户未完成的任务
                │
                └── compact 请求处理:
                      └── 如果模型请求 compact
                      └── compactHistory(ctx, st, messages, query)
```

### 2.3 子代理流程

```
task 工具调用（主 Agent 发起）
    │
    ├── runSubagent(st, prompt):
    │     │
    │     ├── 创建独立上下文:
    │     │     subMessages := []MessageParam{NewUserMessage(prompt)}
    │     │     subClient := NewLLMClient(cfg)
    │     │
    │     ├── 构建子代理 system prompt:
    │     │     subSystemPrompt := systemPrompt(st)  // 复用主 Agent prompt
    │     │
    │     └── 子循环 (for subTurn = 0; subTurn < 30; subTurn++):
    │           │
    │           ├── subTools(st)
    │           │     └── 排除 task 工具（防止递归委派）
    │           │
    │           ├── subClient.Send(subSystemPrompt, subMessages, subTools, maxTokens)
    │           │
    │           ├── 处理响应:
    │           │     │
    │           │     ├── 无 tool_use:
    │           │     │     └── 提取 text → 返回给主 Agent
    │           │     │
    │           │     └── 有 tool_use:
    │           │           └── executeToolBatch(ctx, st, toolUses)
    │           │           └── 追加结果到 subMessages
    │           │           └── 继续子循环
    │           │
    │           └── 超过 30 轮:
    │                 └── 返回超时消息: "[Subagent exceeded maximum turns]"
    │
    └── 返回子代理文本结果
          └── 追加为 tool_result 到主 Agent history
```

## 3. 测试矩阵

### 3.1 REPL 测试

| 测试场景 | 验证点 | 优先级 |
|---------|--------|--------|
| 正常输入/输出 | 提示符显示正确、响应文本正确打印 | P0 |
| 退出条件: q | 输入 "q" 后正常退出，清理后台任务 | P0 |
| 退出条件: exit | 输入 "exit" 后正常退出 | P0 |
| 退出条件: Ctrl-C | 信号处理器触发，终端设置还原 | P0 |
| 退出条件: Ctrl-D | EOF 处理，正常退出 | P0 |
| 空输入 | 仅重新显示提示符，不发送请求 | P1 |
| 大小写不敏感 | "Q"、"EXIT" 同样触发退出 | P2 |
| 历史保留 | 重启后可用方向键翻历史 | P2 |

### 3.2 LLM 层测试

| 测试场景 | 验证点 | 优先级 |
|---------|--------|--------|
| 简单对话 | 消息格式正确转换 (anthropic ↔ eino) | P0 |
| 工具调用 | tool_use block 正确解析和转换 | P0 |
| tool_result 回传 | tool_result 正确追加到 history | P0 |
| prompt_too_long | 触发 reactive compact | P1 |
| token 计数 | Usage 正确记录到 anchorTokens | P1 |
| 多 Content Block | text + tool_use 混合响应正确处理 | P1 |
| API 超时 | context 取消正确处理 | P2 |
| 自定义 BaseURL | ANTHROPIC_BASE_URL 环境变量生效 | P2 |

### 3.3 bash 工具测试

| 测试场景 | 验证点 | 优先级 |
|---------|--------|--------|
| 正常命令 | 输出正确、退出码 0 | P0 |
| 超时处理 | 超过 defaultBashTimeout (120s) 后终止 | P0 |
| 后台执行 | 返回 bg_id，完成后通知注入 | P1 |
| cwd 持久化 | cd 后后续命令保持新目录 | P1 |
| 输出截断 | 超过 maxBashOutputRunes (25000) 时截断 | P1 |
| 非零退出码 | 正确传递退出码到 tool_result | P2 |
| stderr 处理 | stderr 输出正确捕获 | P2 |
| 管道命令 | 复杂管道正确执行 | P2 |

### 3.4 文件系统工具测试

| 测试场景 | 验证点 | 优先级 |
|---------|--------|--------|
| read_file 正常读取 | 行号格式输出 (1\tcontent) | P0 |
| read_file 分页 | offset/limit 参数正确 | P1 |
| read_file mtime 去重 | 同一文件未修改时 stub 返回 | P1 |
| read_file 大文件 | 超过 maxReadOutputRunes (25000) 截断 | P1 |
| write_file 新建文件 | 原子写入，父目录自动创建 | P0 |
| write_file 覆盖文件 | read-before-write 门控检查 | P1 |
| edit_file 唯一匹配 | 精确替换 old_string → new_string | P0 |
| edit_file 0 处匹配 | 返回错误提示 "no match found" | P0 |
| edit_file 多处匹配 | 返回 replace_all 提示 | P1 |
| glob * 匹配 | 单层通配符正确 | P0 |
| glob ** 匹配 | 递归通配符正确 | P1 |
| grep 正则搜索 | content 模式输出匹配行 | P0 |
| grep 文件列表 | files_with_matches 模式输出文件路径 | P1 |
| grep 计数 | count 模式输出匹配数量 | P1 |
| grep 大文件跳过 | 超过 maxGrepFileSize (5MB) 跳过 | P2 |

### 3.5 任务管理测试

| 测试场景 | 验证点 | 优先级 |
|---------|--------|--------|
| todo_write 创建 | 列表正确渲染 | P0 |
| todo_write 状态更新 | in_progress 唯一性约束 | P0 |
| todo_write 超限 | 超过 20 条时拒绝 | P1 |
| create_task 创建 | ID 格式正确、文件创建 | P0 |
| update_task 加边 | 环检测正确 | P1 |
| claim_task 认领 | blockedBy 校验正确 | P1 |
| complete_task 完成 | owner 校验正确 | P1 |
| 任务图持久化 | 重启后任务状态恢复 | P1 |
| DAG 校验 | 环路检测拒绝非法边 | P1 |

### 3.6 子代理测试

| 测试场景 | 验证点 | 优先级 |
|---------|--------|--------|
| 委派正常流程 | 独立上下文、正确返回结果 | P0 |
| 最大轮数限制 | 30 轮后返回超时消息 | P1 |
| task 工具排除 | 子代理不可使用 task 工具（防递归） | P1 |
| 对话日志分离 | 子代理对话写入 messages-sub.json | P1 |
| 结果回传 | 子代理文本正确追加为 tool_result | P0 |

### 3.7 记忆系统测试

| 测试场景 | 验证点 | 优先级 |
|---------|--------|--------|
| 存储 | frontmatter 格式正确写入 | P0 |
| 召回 | LLM 选择 + 关键词回退 | P0 |
| 抽取 | 轮末检查并提取新记忆 | P1 |
| 合并 | 达到 consolidateThreshold (10) 时触发 | P1 |
| 正文截断 | 超过 maxMemoryBodyRunes (4000) 时截断 | P2 |
| 召回数量限制 | 不超过 maxRecallMemories (5) | P2 |

### 3.8 MCP 测试

| 测试场景 | 验证点 | 优先级 |
|---------|--------|--------|
| connect | 工具发现正确 | P0 |
| 调用 | 命名规范 mcp__server__tool 正确 | P0 |
| 权限 | host-side 策略正确执行 | P1 |
| 连接断开 | 优雅降级，不影响主流程 | P1 |
| 多 server | 多个 MCP server 同时连接 | P2 |

### 3.9 权限管线测试

| 测试场景 | 验证点 | 优先级 |
|---------|--------|--------|
| deny list | 硬拒列表中的操作 | P0 |
| 规则匹配 | 破坏性操作检测正确 | P0 |
| 用户确认 | y/yes 放行，其他拒绝 | P0 |
| PreToolUse 拦截 | hook 返回 deny 时阻止执行 | P1 |
| PostToolUse 检查 | hook 可检查输出内容 | P1 |

### 3.10 Hook 机制测试

| 测试场景 | 验证点 | 优先级 |
|---------|--------|--------|
| UserPromptSubmit | 用户输入时触发 | P1 |
| PreToolUse | 工具执行前触发，可拦截 | P0 |
| PostToolUse | 工具执行后触发，可检查输出 | P1 |
| Stop | 退出时触发，可控制退出行为 | P1 |
| Hook 错误处理 | hook 执行失败不中断主流程 | P2 |

### 3.11 上下文压缩测试

| 测试场景 | 验证点 | 优先级 |
|---------|--------|--------|
| toolResultBudget 大结果落盘 | 文件写入正确路径，预览替换正确 | P0 |
| toolResultBudget 不触发 | 总字符 < 200000 时原样返回 | P1 |
| microCompact 空闲触发 | 距上次响应 >= 60 分钟时清理 | P1 |
| microCompact 保留最近 | 最近 5 个不被清理 | P1 |
| autoCompact 超阈值 | 调用 LLM 生成摘要 | P0 |
| autoCompact 熔断 | 连续 3 次失败后熔断 | P1 |
| reactiveCompact 兜底 | prompt_too_long 时快速压缩 | P0 |
| 完整流水线 | prepare() 按序执行三步 | P1 |

### 3.12 调度器测试

| 测试场景 | 验证点 | 优先级 |
|---------|--------|--------|
| 并发读 | 独立 read_file 操作并行执行 | P1 |
| 串行写 | 写操作独占执行 | P0 |
| 读写顺序 | 有依赖时按序执行 | P1 |
| 混合调度 | 读+写混合时正确分组 | P2 |

### 3.13 后台任务测试

| 测试场景 | 验证点 | 优先级 |
|---------|--------|--------|
| 启动 | 返回 bg_id | P0 |
| 完成通知 | 结果注入下一轮对话 | P0 |
| stopAll 清理 | 退出时所有后台进程终止 | P0 |
| 多任务并行 | 多个后台任务同时运行 | P1 |
| 超时处理 | 后台任务超时正确终止 | P2 |

## 4. 性能基准

| 指标 | 目标值 | 测量方法 | 说明 |
|------|--------|---------|------|
| TTFT (Time To First Token) | < 500ms | 用户输入到首个 token 输出的时间 | 不含网络延迟 |
| 工具吞吐 | > 10 ops/s | 并发 read_file 操作速率 | 稳态测量 |
| 内存占用 | < 200MB | 100 轮对话后的 RSS | 含对话历史和缓存 |
| 压缩耗时 | < 5s | auto_compact 调用到完成 | 含 LLM 摘要生成 |
| 启动时间 | < 2s | main() 到 readline 就绪 | 含技能扫描和 MCP 连接 |
| 日志写入 | < 1ms/条 | 单条 JSON Lines 写入耗时 | 不含 fsync |

## 5. 验收清单

### 5.1 核心功能

- [ ] REPL 正常启动和退出（q / exit / Ctrl-C / Ctrl-D）
- [ ] 19 个工具全部可用（bash, read_file, write_file, edit_file, glob, grep, web_search, web_fetch, todo_write, task, create_task, update_task, claim_task, complete_task, delete_task, list_tasks, mcp_connect, mcp_call, mcp_list）
- [ ] 对话历史跨轮次正确维护
- [ ] 工具调用结果正确回传

### 5.2 安全管线

- [ ] 权限管线正确拦截 deny list 操作
- [ ] 权限规则匹配正确检测破坏性操作
- [ ] 用户确认流程正确（y/yes 放行，其他拒绝）
- [ ] Hook 机制正确触发（UserPromptSubmit, PreToolUse, PostToolUse, Stop）

### 5.3 子代理

- [ ] 子代理正常委派和返回
- [ ] 子代理不可使用 task 工具（防递归）
- [ ] 子代理最大 30 轮限制生效
- [ ] 子代理对话日志写入 messages-sub.json

### 5.4 持久化

- [ ] 跨会话记忆持久化（.memory/ 目录）
- [ ] 任务图持久化和 DAG 校验（.tasks/ 目录）
- [ ] readline 历史持久化（.coding-agent-history）
- [ ] 对话日志持久化（messages.json, messages-sub.json）

### 5.5 后台任务

- [ ] 后台任务执行和通知注入
- [ ] stopAll 正确清理所有后台进程
- [ ] bg_id 正确返回和跟踪

### 5.6 MCP

- [ ] MCP 工具连接和发现
- [ ] MCP 工具调用命名规范（mcp__server__tool）
- [ ] MCP host-side 权限策略

### 5.7 上下文压缩

- [ ] tool_result_budget 大结果落盘
- [ ] micro_compact 空闲期清理
- [ ] auto_compact 超阈值摘要
- [ ] reactive_compact prompt_too_long 兜底
- [ ] 熔断机制（连续 3 次失败）

### 5.8 日志

- [ ] messages.json 正确记录主 Agent 对话
- [ ] messages-sub.json 正确记录子 Agent 对话
- [ ] coding-agent-go.log 正确记录运行日志
- [ ] 日志格式符合规范（[时间][级别] 调用点 消息）

## 6. 模块依赖关系

```
REPL (main.go)
  ├── agent (agentLoop)
  │     ├── llm (LLMClient)
  │     │     └── eino ChatModel (claude)
  │     ├── tool (19 tools)
  │     │     ├── bash
  │     │     ├── filesystem (read/write/edit/glob/grep)
  │     │     ├── web (search/fetch)
  │     │     ├── task (subagent)
  │     │     ├── taskstore (6 tools)
  │     │     ├── todo_write
  │     │     └── mcp (connect/call/list)
  │     ├── permission (三闸门管线)
  │     ├── hooks (生命周期回调)
  │     ├── compact (上下文压缩)
  │     ├── skill (技能系统)
  │     ├── prompt (system prompt 构建)
  │     ├── memory (跨会话记忆)
  │     └── background (后台任务)
  ├── logger (日志系统)
  └── readline (命令行交互)
```

## 7. 测试基础设施

### 7.1 Mock 策略

| 组件 | Mock 方式 | 说明 |
|------|----------|------|
| LLMClient | interface 注入 | 实现 mockLLMClient 返回预设响应 |
| 文件系统 | t.TempDir() | 使用临时目录隔离测试 |
| 时间 | time.Now 替换 | 控制空闲检测逻辑 |
| 环境变量 | t.Setenv() | 隔离配置影响 |
| exec.Command | 替换为 mock | 避免真实 shell 执行 |

### 7.2 测试数据

```
testdata/
├── skills/                    # 技能扫描测试数据
│   ├── valid/SKILL.md
│   ├── no-frontmatter/SKILL.md
│   └── symlink-escape -> /tmp/
├── memories/                  # 记忆存储测试数据
│   └── sample.json
├── tasks/                     # 任务图测试数据
│   └── sample-dag.json
└── prompts/                   # system prompt 测试数据
    └── expected-output.txt
```

### 7.3 测试命令

```bash
# 单元测试
go test ./...

# 带覆盖率
go test -cover ./...

# 带 race 检测
go test -race ./...

# 集成测试（需要 API key）
go test -tags=integration ./...

# 性能基准
go test -bench=. -benchmem ./...
```

## 8. 已知限制与未来改进

### 8.1 当前限制

1. **token 估算精度**: 使用字符数/4 粗糙估计，误差约 ±20%
2. **日志文件大小**: 对话日志每轮完整快照，大对话时 I/O 开销显著
3. **并发安全**: compactor 和 skillLoader 设计为单线程使用
4. **MCP 重连**: 连接断开后不自动重连
5. **子代理嵌套**: 子代理不可再委派（task 工具被排除）

### 8.2 未来改进方向

1. 精确 token 计数（使用 tokenizer 库）
2. 增量日志写入
3. compactor 并发安全
4. MCP 自动重连机制
5. 子代理有限嵌套（深度限制）
