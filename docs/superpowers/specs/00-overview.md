# 00 - 项目概述与架构

## 1. 项目目标

实现一个基于 eino 框架的交互式 Coding Agent，具备以下核心能力：

1. **REPL 交互** — 命令行对话界面
2. **Agent 循环** — 多轮对话，工具调用
3. **文件操作** — 读/写/编辑/搜索文件
4. **Shell 执行** — 运行 bash 命令
5. **任务管理** — 待办清单 + 持久化任务图
6. **子代理** — 委派任务给独立 Agent
7. **技能系统** — 按需加载 SKILL.md
8. **跨会话记忆** — 长期知识存储
9. **MCP 工具** — 连接外部服务
10. **权限控制** — 三闸门安全管线
11. **上下文压缩** — 自动摘要长对话

## 2. 技术架构

### 2.1 核心组件

```
┌─────────────────────────────────────────────────────────────────┐
│                         REPL (main.go)                          │
│  readline + signal handler + logger                             │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                    Agent Core (pkg/agent/)                      │
│  ┌───────────────────────────────────────────────────────────┐ │
│  │              eino adk.ChatModelAgent                      │ │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────────┐  │ │
│  │  │ ChatModel   │  │   Tools     │  │   Middlewares   │  │ │
│  │  │ (claude)    │  │  (19 tools) │  │  (6 built-in)   │  │ │
│  │  └─────────────┘  └─────────────┘  └─────────────────┘  │ │
│  │                                                           │ │
│  │  ┌─────────────────────────────────────────────────────┐ │ │
│  │  │  Callbacks (Hook mapping)                           │ │ │
│  │  │  OnToolStart → permission check                     │ │ │
│  │  │  OnToolEnd → large output warning                   │ │ │
│  │  │  OnAgentFinish → exit summary                       │ │ │
│  │  └─────────────────────────────────────────────────────┘ │ │
│  └───────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
                              │
          ┌───────────────────┼───────────────────┐
          ▼                   ▼                   ▼
┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
│ pkg/memory/     │ │ pkg/taskstore/  │ │ pkg/background/ │
│ 跨会话记忆       │ │ 持久化任务图     │ │ 后台任务         │
└─────────────────┘ └─────────────────┘ └─────────────────┘
```

### 2.2 eino 组件使用

| 功能 | eino 组件 | 说明 |
|------|----------|------|
| Agent 编排 | `adk.ChatModelAgent` | REACT 模式 Agent |
| LLM 调用 | `model.ChatModel` (claude) | 通过 eino-ext 适配器 |
| 工具定义 | `tool.InvokableTool` | 所有工具的标准接口 |
| 文件工具 | `adk/middlewares/filesystem` | 自动注入 read/write/edit/glob/grep |
| 任务规划 | `adk/middlewares/plantask` | 自动注入 todo_write |
| 上下文压缩 | `adk/middlewares/reduction` + `summarization` | 大结果落盘 + 自动摘要 |
| 技能加载 | `adk/middlewares/skill` | SKILL.md 扫描和注入 |
| AGENTS.md | `adk/middlewares/agentsmd` | 自动注入项目说明 |
| Hook 机制 | `callbacks.Handler` | 生命周期回调 |
| 子代理 | `adk/prebuilt/supervisor` | 多 Agent 监督 |

### 2.3 保留的独立模块

| 模块 | 功能 | 保留原因 |
|------|------|---------|
| `pkg/memory/` | 跨会话记忆 | eino 无长期记忆管理 |
| `pkg/taskstore/` | 持久化任务图 | eino plantask 无 DAG 能力 |
| `pkg/background/` | 后台任务 | eino 无异步后台任务管理 |
| `pkg/permission/` | 权限管线 | 业务逻辑，通过 callbacks 集成 |
| `pkg/search/` | 搜索引擎 | filesystem 工具的内部实现 |

## 3. 数据流

### 3.1 用户输入流程

```
用户输入 (readline)
    │
    ▼
triggerHooks(UserPromptSubmit)
    │
    ▼
background.collect() → 注入后台任务结果
    │
    ▼
agent.Invoke(ctx, messages)
    │
    ├── ChatModel.Generate()
    │       │
    │       ▼
    │   模型响应 (text / tool_use)
    │       │
    │       ▼
    │   if tool_use:
    │       │
    │       ├── triggerHooks(PreToolUse) → 权限检查
    │       │
    │       ├── executeTool(tool_name, args)
    │       │       │
    │       │       ▼
    │       │   tool_result
    │       │
    │       ├── triggerHooks(PostToolUse)
    │       │
    │       └── 追加 tool_result 到 messages，继续循环
    │
    └── 返回最终 text 响应
```

### 3.2 工具调用流程

```
模型返回 tool_use
    │
    ▼
┌─────────────────────────────────────────┐
│  路径感知调度 (eino compose.Parallel)   │
│                                         │
│  1. 分析资源依赖 (读/写哪些文件)         │
│  2. 分组:                               │
│     - 无冲突 → 并发执行                  │
│     - 有冲突 → 串行执行                  │
│  3. 按组执行，结果按原顺序回填            │
└─────────────────────────────────────────┘
    │
    ▼
tool_result 追加到 messages
```

## 4. 目录结构

```
coding-agent-go/
├── cmd/agent/
│   └── main.go              # REPL 入口
├── pkg/
│   ├── agent/
│   │   ├── agent.go         # Agent 核心
│   │   ├── config.go        # 配置
│   │   └── llm.go           # LLM 封装
│   ├── tool/
│   │   ├── bash.go          # bash 工具
│   │   ├── task.go          # 子代理工具
│   │   ├── taskstore.go     # 任务图工具 (6个)
│   │   ├── mcp/
│   │   │   ├── connect.go   # MCP 连接工具
│   │   │   ├── wrapper.go   # MCP 工具包装器
│   │   │   └── registry.go  # MCP 注册表
│   │   └── registry.go      # 工具组装
│   ├── memory/
│   │   ├── store.go         # 存储
│   │   ├── recall.go        # 召回
│   │   └── extract.go       # 抽取
│   ├── taskstore/
│   │   └── taskstore.go     # 持久化任务图
│   ├── background/
│   │   └── background.go    # 后台任务
│   ├── permission/
│   │   └── permission.go    # 权限管线
│   └── search/
│       └── search.go        # 搜索引擎
├── internal/
│   └── logger/
│       └── logger.go        # 日志
├── skills/                  # 技能目录
├── .memory/                 # 记忆存储 (运行时)
├── .tasks/                  # 任务存储 (运行时)
└── go.mod
```

## 5. 关键常量

```go
// 文件工具
const maxReadLines = 2000           // read_file 默认行数
const maxReadOutputRunes = 25000    // read_file 输出字符预算
const maxGrepFileSize = 5 * 1024 * 1024  // grep 跳过文件大小

// bash 工具
const defaultBashTimeout = 120 * time.Second  // 默认超时
const maxBashOutputRunes = 25000              // 输出截断

// 上下文压缩
const toolResultBudgetChars = 200000   // 一轮 tool_result 合计上限
const largeResultCharLimit = 30000     // 单条大结果落盘门槛
const largeResultPreviewChars = 2000   // 落盘后预览长度
const contextWindow = 200000           // 上下文窗口
const autoCompactThreshold = 167000    // 自动压缩阈值

// 任务系统
const maxTodoItems = 20                // 待办清单上限
const subagentMaxTurns = 30            // 子代理最大轮数

// 记忆系统
const maxRecallMemories = 5            // 单次召回记忆数
const maxMemoryBodyRunes = 4000        // 单条记忆正文上限
const consolidateThreshold = 10        // 合并触发阈值
```

## 6. 实现顺序

按以下顺序实现，确保每一步都有可运行的基础：

1. **Phase 0**: 基础设施 (01-repl, 02-logger, 03-llm)
2. **Phase 1**: 工具层 (04-tools-overview, 05-tool-bash, 06-tool-filesystem, 07-tool-taskstore, 08-tool-mcp)
3. **Phase 2**: Agent 核心 (09-agent-core, 10-permission, 11-hooks)
4. **Phase 3**: 独立子系统 (12-memory, 13-taskstore, 14-background, 15-search)
5. **Phase 4**: Prompt 相关 (16-skill, 17-compact, 18-system-prompt)
6. **Phase 5**: 集成测试 (19-integration)
