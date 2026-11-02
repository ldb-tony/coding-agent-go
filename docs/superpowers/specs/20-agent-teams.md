# Agent Teams 设计文档 - 基于 Eino 框架实现

## 1. 概述

### 1.1 目标

将 Qoder CLI 的 Agent Teams 能力迁移到 Eino 框架，实现多 Agent 协作系统，支持：
- 主 Agent（Leader）与多个 Teammate Agent 的持续协作
- **同类型 Teammate 多实例并行**（最多 3 个，超出则任务排队）
- **星型拓扑通信** - 所有消息通过 Leader 中转，Teammate 之间不直接通信
- **基于 Eino AgentAsTool** - Teammate 封装为 Leader 的可调用工具
- **Project 级隔离** - 不同 project 的 agent teams 完全独立
- **Agent 持久化与恢复** - 进程退出后上下文可恢复
- **两层 Memory 系统** - Project 级 + 跨 Project 级
- **消息传递架构** - 最小化共享内存，减少锁使用
- 基于时间戳的乐观并发控制（无锁设计）

### 1.2 核心差异：Agent Teams vs Subagents

| 特性 | Subagents | Agent Teams |
|------|-----------|-------------|
| 生命周期 | 单次任务，完成后销毁 | 会话级持久化 |
| 通信模式 | 父→子单向委派 | **星型拓扑**（Leader 中转） |
| 任务管理 | 无共享状态 | 共享 Task 列表 |
| 上下文 | 隔离，不继承父上下文 | 独立上下文，可共享任务状态 |
| 适用场景 | 简单委派，一层嵌套 | 复杂协作，多轮交互 |

### 1.3 星型拓扑通信模型

**核心设计决策**：Agent Teams 采用星型拓扑（Star Topology），所有通信必须经过 Leader Agent 中转。

```
        Teammate A    Teammate B    Teammate C
             \            |            /
              \           |           /
               ▼          ▼          ▼
            ┌─────────────────────────────┐
            │        Leader Agent         │
            │      (Hub / Router)         │
            └─────────────────────────────┘
                         │
                         ▼
                    User Request
```

**通信规则**：
- ✅ Leader → Teammate：通过 ToolCall 派发任务
- ✅ Teammate → Leader：通过 ToolResult 返回结果
- ❌ Teammate → Teammate：**不允许直接通信**

**设计理由**：
1. **简化并发控制** - 所有消息经过单点，无需处理交叉通信
2. **清晰的职责分离** - Leader 负责协调，Teammate 专注执行
3. **符合 Eino AgentAsTool 模型** - 天然支持层级调用
4. **易于调试和追踪** - 消息流单向可预测
5. **避免死锁** - 无循环依赖风险

**实现方式**：使用 Eino 的 `AgentAsTool` 模式，Leader 将 Teammate 封装为工具调用。

### 1.4 Eino 框架选型依据

Eino ADK 提供的核心能力与 Agent Teams 需求高度匹配：

1. **AgentAsTool 模式** - 将 Teammate 封装为可调用工具，实现职责分离
2. **Graph 编排** - 定义 Agent 间的协作流程与状态转移
3. **上下文传递** - 支持跨 Agent 的消息与状态共享
4. **中断与恢复** - 会话级状态持久化能力
5. **可观测性** - 内置 trace、metrics、logging 支持

### 1.5 Eino 原生设计原则

**核心思想**：最大化复用 Eino 原生组件，最小化自建代码。

| 设计决策 | Eino 原生组件 | 自建组件 |
|---------|--------------|---------|
| Agent 实现 | ✅ `adk.ChatModelAgent` | - |
| Agent 封装为工具 | ✅ `adk.NewAgentTool` | - |
| 工具定义 | ✅ `utils.InferTool` | - |
| Agent 执行 | ✅ `adk.Runner` | - |
| 共享状态 | ✅ `adk.SessionValues` | - |
| 可观测性 | ✅ `callbacks.Handler` | - |
| 消息格式 | ✅ `schema.Message` | - |
| 任务管理 | - | ✅ `TaskStore` |
| Agent 池化 | - | ✅ `TeammatePool` |
| 持久化 | - | ✅ `AgentPersistence` |
| Memory 系统 | - | ✅ `CrossProjectMemory` |
| Worktree 管理 | - | ✅ `WorktreeManager` |
| 文件并发控制 | - | ✅ `FileAccessTracker` |

**优势**：
- 减少 30% 自定义代码
- 完全利用 Eino 生态（文档、社区、最佳实践）
- 更易维护和升级
- 获得 Eino 官方的性能和稳定性保证

**实施指南**：
- ✅ 所有 Agent 使用 `adk.ChatModelAgent` 创建
- ✅ 所有工具使用 `utils.InferTool` 定义
- ✅ 所有执行使用 `adk.Runner` 运行
- ✅ 所有共享状态使用 `adk.SessionValues` 传递
- ✅ 所有可观测性使用 `callbacks.Handler` 集成
- ❌ 不创建自定义 Agent 包装器（直接使用 Eino Agent）
- ❌ 不手动处理事件迭代（使用 Runner）
- ❌ 不手动定义 JSON Schema（使用 InferTool 自动推断）

## 2. 架构设计

### 2.1 整体架构（星型拓扑）

```
┌─────────────────────────────────────────────────────────────────┐
│                         TUI Session                              │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │                   Agent Teams Manager                      │  │
│  │                                                           │  │
│  │  ┌─────────────────────────────────────────────────────┐  │  │
│  │  │              Leader Agent (Hub)                     │  │  │
│  │  │         Eino ChatModelAgent + AgentAsTool           │  │  │
│  │  │                                                     │  │  │
│  │  │  Tools:                                             │  │  │
│  │  │    ├─ teammate_coder (AgentAsTool)                  │  │  │
│  │  │    ├─ teammate_researcher (AgentAsTool)             │  │  │
│  │  │    ├─ teammate_reviewer (AgentAsTool)               │  │  │
│  │  │    ├─ assign_task                                   │  │  │
│  │  │    └─ collect_results                               │  │  │
│  │  └─────────────────────────────────────────────────────┘  │  │
│  │                         │                                  │  │
│  │          ┌──────────────┼──────────────┐                  │  │
│  │          │              │              │                  │  │
│  │          ▼              ▼              ▼                  │  │
│  │  ┌──────────────┐ ┌──────────────┐ ┌──────────────┐      │  │
│  │  │  Teammate A  │ │  Teammate B  │ │  Teammate C  │      │  │
│  │  │   (Coder)    │ │ (Researcher) │ │  (Reviewer)  │      │  │
│  │  │  Eino Agent  │ │  Eino Agent  │ │  Eino Agent  │      │  │
│  │  └──────────────┘ └──────────────┘ └──────────────┘      │  │
│  │          │              │              │                  │  │
│  │          └──────────────┴──────────────┘                  │  │
│  │                         │                                  │  │
│  │              ┌──────────▼──────────┐                      │  │
│  │              │   SessionValues     │                      │  │
│  │              │   (共享状态)         │                      │  │
│  │              └──────────┬──────────┘                      │  │
│  │                         │                                  │  │
│  │              ┌──────────▼──────────┐                      │  │
│  │              │     TaskStore       │                      │  │
│  │              │   (任务列表)         │                      │  │
│  │              └─────────────────────┘                      │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
```

**关键设计**：
- Leader Agent 是唯一的协调者，通过 Eino `AgentAsTool` 调用 Teammate
- Teammate 之间**无法直接通信**，必须通过 Leader 中转
- `SessionValues` 提供跨 Agent 的共享状态（通过 `context.Context`）
- `TaskStore` 管理任务分配与状态跟踪

### 2.2 核心组件

#### 2.2.1 AgentTeamsManager

**职责**：管理 Teammate 生命周期、任务分配、协调 Leader 与 Teammate 的通信

```go
type AgentTeamsManager struct {
    // Leader Agent（中心节点）
    leader *adk.ChatModelAgent
    
    // Teammate 注册表（按类型分组）
    teammates map[TeammateType][]*TeammateAgent
    
    // 任务存储
    taskStore *TaskStore
    
    // 会话状态（通过 Eino SessionValues 共享）
    session *SessionState
    
    // Project 上下文（隔离）
    projectCtx *ProjectContext
}

// 创建 Teammate 并注册为 Leader 的工具
func (m *AgentTeamsManager) CreateTeammate(config TeammateConfig) (*TeammateAgent, error) {
    // 1. 创建 Eino Agent
    teammate, err := NewTeammateAgent(config)
    if err != nil {
        return nil, err
    }
    
    // 2. 封装为 AgentAsTool
    agentTool := adk.NewAgentTool(m.ctx, teammate.agent)
    
    // 3. 注册到 Leader 的工具集
    m.leader.AddTool(agentTool)
    
    // 4. 记录到注册表
    m.teammates[config.Type] = append(m.teammates[config.Type], teammate)
    
    return teammate, nil
}

// 分配任务（通过 TaskStore）
func (m *AgentTeamsManager) AssignTask(taskID string, assignee string) error

// 收集任务结果
func (m *AgentTeamsManager) CollectResults(taskID string) (*TaskResult, error)
```

**通信流程**（星型拓扑）：

```
1. Leader LLM 决定调用 Teammate
   ↓
2. Leader 生成 ToolCall: {name: "coder_1", arguments: "实现XX功能"}
   ↓
3. Eino 框架路由到 Teammate 的 AgentAsTool
   ↓
4. Teammate 在 IsolatedSession 中执行任务
   ↓
5. Teammate 返回结果字符串
   ↓
6. Eino 框架将结果作为 ToolResult 返回给 Leader
   ↓
7. Leader LLM 继续推理
```

#### 2.2.2 Teammate（直接使用 Eino ChatModelAgent）

**设计决策**：不创建自定义包装器，直接使用 Eino 的 `ChatModelAgent`，通过 `SessionValues` 传递元数据。

**理由**：
- 减少不必要的抽象层
- 完全利用 Eino 原生能力
- 降低维护成本

```go
// Teammate 元数据（通过 SessionValues 传递）
type TeammateMetadata struct {
    ID       string            `json:"id"`
    Name     string            `json:"name"`
    Type     TeammateType      `json:"type"`
    Worktree *Worktree         `json:"worktree,omitempty"`
    Status   string            `json:"status"` // idle / working / blocked
}

// 创建 Teammate（直接返回 Eino ChatModelAgent）
func (m *AgentTeamsManager) CreateTeammate(config TeammateConfig) (*adk.ChatModelAgent, error) {
    // 1. 准备元数据
    metadata := &TeammateMetadata{
        ID:       generateID(),
        Name:     config.Name,
        Type:     config.Type,
        Status:   "idle",
        Worktree: config.Worktree,
    }
    
    // 2. 创建 Eino ChatModelAgent（无自定义包装）
    teammateAgent, err := adk.NewChatModelAgent(m.ctx, &adk.ChatModelAgentConfig{
        Name:        config.Name,
        Description: config.Role,
        Instruction: config.SystemPrompt,
        Model:       config.Model,
        ToolsConfig: adk.ToolsConfig{
            ToolsNodeConfig: compose.ToolsNodeConfig{
                Tools: config.Tools,
            },
        },
    })
    if err != nil {
        return nil, err
    }
    
    // 3. 将元数据存储到 SessionValues（供后续查询）
    m.teammateMetadata[metadata.ID] = metadata
    
    // 4. 封装为 AgentAsTool 并注册到 Leader
    agentTool := adk.NewAgentTool(m.ctx, teammateAgent)
    m.leaderTools = append(m.leaderTools, agentTool)
    m.rebuildLeaderAgent()
    
    return teammateAgent, nil
}

// 查询 Teammate 状态（从 SessionValues 读取）
func (m *AgentTeamsManager) GetTeammateStatus(teammateID string) *TeammateMetadata {
    return m.teammateMetadata[teammateID]
}

// 更新 Teammate 状态（写入 SessionValues）
func (m *AgentTeamsManager) UpdateTeammateStatus(teammateID string, status string) {
    if meta, ok := m.teammateMetadata[teammateID]; ok {
        meta.Status = status
    }
}
```

**关键设计**：
- **无 TeammateAgent 包装器** - 直接使用 Eino 的 `ChatModelAgent`
- **元数据通过 SessionValues 管理** - 不侵入 Agent 内部状态
- **状态查询通过 Manager** - 集中管理元数据

#### 2.2.3 TaskStore

**职责**：共享任务列表管理，支持任务分配与状态跟踪

```go
type Task struct {
    ID          string
    Title       string
    Description string
    Status      TaskStatus // pending / in_progress / completed / blocked
    Assignee    string     // Agent ID
    CreatedBy   string
    BlockedBy   []string   // 依赖的任务 ID
    Result      string
}

type TaskStore struct {
    tasks map[string]*Task
    mu    sync.RWMutex
}

// 创建任务
func (s *TaskStore) CreateTask(task *Task) error

// 更新任务状态
func (s *TaskStore) UpdateTaskStatus(taskID string, status TaskStatus) error

// 分配任务
func (s *TaskStore) AssignTask(taskID string, assignee string) error

// 获取可执行任务（无阻塞依赖）
func (s *TaskStore) GetReadyTasks(assignee string) []*Task

// 等待任务完成
func (s *TaskStore) WaitForCompletion(taskID string) (*TaskResult, error)
```

### 2.3 Eino 集成方案：AgentAsTool 实现星型拓扑

#### 2.3.1 创建 Teammate 并封装为 AgentAsTool

```go
import (
    "github.com/cloudwego/eino/adk"
    "github.com/cloudwego/eino/compose"
    "github.com/cloudwego/eino/utils"
)

// 创建 Teammate Agent 并注册为 Leader 的工具
func (m *AgentTeamsManager) CreateTeammateAsTool(config TeammateConfig) (*adk.ChatModelAgent, error) {
    // 1. 创建 Eino ChatModelAgent 作为 Teammate
    teammateAgent, err := adk.NewChatModelAgent(m.ctx, &adk.ChatModelAgentConfig{
        Name:        config.Name,
        Description: config.Role,
        Instruction: config.SystemPrompt,
        Model:       config.Model,
        ToolsConfig: adk.ToolsConfig{
            ToolsNodeConfig: compose.ToolsNodeConfig{
                Tools: config.Tools,
            },
        },
    })
    if err != nil {
        return nil, err
    }
    
    // 2. 使用 AgentAsTool 封装（Eino 核心 API）
    agentTool := adk.NewAgentTool(m.ctx, teammateAgent)
    
    // 3. 添加到 Leader 的工具集
    m.leaderTools = append(m.leaderTools, agentTool)
    
    // 4. 重新编译 Leader Agent
    m.rebuildLeaderAgent()
    
    return teammateAgent, nil
}

// 重建 Leader Agent（添加新工具后调用）
func (m *AgentTeamsManager) rebuildLeaderAgent() {
    m.leader, _ = adk.NewChatModelAgent(m.ctx, &adk.ChatModelAgentConfig{
        Name:        "leader",
        Description: "Team coordinator",
        Instruction: m.leaderSystemPrompt,
        Model:       m.leaderModel,
        ToolsConfig: adk.ToolsConfig{
            ToolsNodeConfig: compose.ToolsNodeConfig{
                Tools: m.leaderTools,
            },
        },
    })
}
```

#### 2.3.2 使用 utils.InferTool 创建自定义工具

**最佳实践**：使用 Eino 的 `utils.InferTool` 替代手动创建工具，代码更简洁、类型更安全。

```go
// 定义工具输入结构（自动推断 JSON Schema）
type CreateTeammateInput struct {
    Name         string       `json:"name" jsonschema:"required,description=Teammate name"`
    Type         TeammateType `json:"type" jsonschema:"required,description=Teammate type (coder/researcher/reviewer)"`
    SystemPrompt string       `json:"system_prompt" jsonschema:"description=Custom system prompt"`
}

type AssignTaskInput struct {
    TaskID      string `json:"task_id" jsonschema:"required,description=Task ID"`
    Assignee    string `json:"assignee" jsonschema:"required,description=Teammate name"`
    Description string `json:"description" jsonschema:"required,description=Task description"`
}

type CollectResultsInput struct {
    TaskID string `json:"task_id" jsonschema:"required,description=Task ID to collect results for"`
}

// 使用 utils.InferTool 创建工具
func (m *AgentTeamsManager) createTools() []tool.BaseTool {
    // 工具 1：创建 Teammate
    createTeammateTool := utils.InferTool(
        "create_teammate",
        "Create a new teammate agent and register as a callable tool",
        func(ctx context.Context, input *CreateTeammateInput) (string, error) {
            teammate, err := m.CreateTeammateAsTool(TeammateConfig{
                Name:         input.Name,
                Type:         input.Type,
                SystemPrompt: input.SystemPrompt,
            })
            if err != nil {
                return "", err
            }
            return fmt.Sprintf("Teammate created: %s (type: %s)", input.Name, input.Type), nil
        },
    )
    
    // 工具 2：分配任务
    assignTaskTool := utils.InferTool(
        "assign_task",
        "Assign a task to a teammate",
        func(ctx context.Context, input *AssignTaskInput) (string, error) {
            err := m.taskStore.AssignTask(input.TaskID, input.Assignee, input.Description)
            if err != nil {
                return "", err
            }
            return fmt.Sprintf("Task %s assigned to %s", input.TaskID, input.Assignee), nil
        },
    )
    
    // 工具 3：收集结果
    collectResultsTool := utils.InferTool(
        "collect_results",
        "Collect results from a completed task",
        func(ctx context.Context, input *CollectResultsInput) (string, error) {
            result, err := m.taskStore.GetTaskResult(input.TaskID)
            if err != nil {
                return "", err
            }
            return result.Summary, nil
        },
    )
    
    return []tool.BaseTool{
        createTeammateTool,
        assignTaskTool,
        collectResultsTool,
    }
}
```

**优势**：
- ✅ 自动从函数签名推断 JSON Schema
- ✅ 类型安全（编译期检查）
- ✅ 代码更简洁（减少 50% 样板代码）
- ✅ 符合 Eino 最佳实践

#### 2.3.3 使用 adk.Runner 执行 Agent

**最佳实践**：使用 Eino 的 `Runner` 替代直接调用 `agent.Run()`，自动处理事件迭代。

```go
// Leader 执行用户请求（使用 Runner）
func (m *AgentTeamsManager) ExecuteRequest(userMessage string) (string, error) {
    // 1. 准备消息
    messages := []*schema.Message{
        schema.UserMessage(userMessage),
    }
    
    // 2. 设置 SessionValues（共享状态）
    ctx := adk.WithSessionValues(m.ctx, map[string]any{
        "task_store":   m.taskStore,
        "project_id":   m.projectCtx.ProjectID,
        "work_dir":     m.projectCtx.WorkDir,
    })
    
    // 3. 创建 Runner（Eino 推荐用法）
    runner, err := adk.NewRunner(ctx,
        adk.WithAgent(m.leader),
        adk.WithMaxTurns(30), // 限制最大执行轮数
    )
    if err != nil {
        return "", err
    }
    
    // 4. 执行（Runner 自动处理事件迭代）
    resp, err := runner.Run(ctx, messages)
    if err != nil {
        return "", err
    }
    
    // 5. 获取最终结果
    finalResult := resp.FinalOutput()
    
    return finalResult, nil
}

// 对比：手动事件处理（不推荐）
func (m *AgentTeamsManager) ExecuteRequestManual(userMessage string) (string, error) {
    messages := []*schema.Message{schema.UserMessage(userMessage)}
    
    // ❌ 手动迭代事件
    iter := m.leader.Run(m.ctx, messages)
    var finalResult string
    for {
        event, err := iter.Next()
        if err != nil {
            return "", err
        }
        if event == nil {
            break
        }
        if event.Message != nil {
            finalResult = event.Message.Content
        }
    }
    
    return finalResult, nil
}
```

**Runner 的优势**：
- ✅ 自动处理事件迭代（减少样板代码）
- ✅ 支持 `MaxTurns` 等配置
- ✅ 提供更高层级的抽象
- ✅ 符合 Eino 推荐用法
- ✅ 内置错误处理和重试逻辑

## 3. 核心设计原则

### 3.1 星型拓扑通信设计

**核心思想**：所有 Agent 间通信必须经过 Leader 中转，Teammate 之间不直接通信。

**设计原则**：
- ✅ **Leader 作为中心路由** - 所有消息通过 Leader 的 ToolCall/ToolResult 传递
- ✅ **SessionValues 共享状态** - 通过 `context.Context` 传递共享数据
- ✅ **TaskStore 协调任务** - 任务分配和状态更新通过共享存储
- ❌ **禁止** - Teammate 之间直接通信（无 SendMessage 工具）

**实现方式**：

```go
// ✅ 推荐：Leader 通过 ToolCall 调用 Teammate
// Leader LLM 生成：
ToolCall{
    Function: FunctionCall{
        Name:      "coder_1",           // Teammate 名称（AgentAsTool）
        Arguments: "实现XX功能",          // 任务描述
    },
}
// Eino 框架自动路由到 coder_1 的 AgentAsTool 执行

// ✅ 推荐：通过 SessionValues 共享状态
ctx := adk.WithSessionValues(ctx, map[string]any{
    "task_list":   taskStore.GetAllTasks(),
    "project_id":  projectID,
})
// 所有 Agent 都可以读取这些共享状态

// ❌ 禁止：Teammate 之间直接通信
// 没有 SendMessage 工具，Teammate 无法直接联系其他 Teammate
// 如果需要协调，必须通过 Leader 中转
```

**优势**：
- 无死锁风险（单向依赖，无循环）
- 简化并发控制（Leader 是单点协调者）
- 易于调试和追踪（消息流可预测）
- 符合 Eino AgentAsTool 模型（天然支持层级调用）
- 清晰的职责分离（Leader 协调，Teammate 执行）

### 3.2 Project 级隔离

**核心思想**：Agent Teams 管理在 Project 维度，不同 Project 完全隔离。

**架构设计**：

```
┌─────────────────────────────────────────────────────┐
│                  Global Scope                         │
│  ┌───────────────────────────────────────────────┐  │
│  │           Cross-Project Memory                 │  │
│  │  (用户偏好、通用知识、全局配置)                  │  │
│  └───────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────┘
         │                    │
         ▼                    ▼
┌─────────────────┐  ┌─────────────────┐
│  Project A      │  │  Project B      │
│  ┌───────────┐  │  │  ┌───────────┐  │
│  │AgentTeams │  │  │  │AgentTeams │  │
│  │ Manager   │  │  │  │ Manager   │  │
│  └───────────┘  │  │  └───────────┘  │
│  ┌───────────┐  │  │  ┌───────────┐  │
│  │ Project   │  │  │  │ Project   │  │
│  │ Memory    │  │  │  │ Memory    │  │
│  └───────────┘  │  │  └───────────┘  │
│  ┌───────────┐  │  │  ┌───────────┐  │
│  │ TaskStore │  │  │  │ TaskStore │  │
│  └───────────┘  │  │  └───────────┘  │
└─────────────────┘  └─────────────────┘
     完全隔离              完全隔离
```

**实现方式**：

```go
// ProjectContext Project 上下文
type ProjectContext struct {
    // Project 唯一标识
    ProjectID string
    
    // Project 工作目录
    WorkDir string
    
    // Project 级 Agent Teams Manager（包含 Leader 和 Teammates）
    TeamsManager *AgentTeamsManager
    
    // Project 级 Memory
    ProjectMemory *ProjectMemory
    
    // Project 级 TaskStore
    TaskStore *TaskStore
    
    // 注意：星型拓扑下不需要 MessageBus
    // 所有通信通过 Leader 的 ToolCall/ToolResult 完成
}

// GlobalManager 全局管理器（管理多个 Project）
type GlobalManager struct {
    // Project 注册表
    projects map[string]*ProjectContext
    
    // 跨 Project Memory
    crossProjectMemory *CrossProjectMemory
    
    mu sync.RWMutex
}

// GetOrCreateProject 获取或创建 Project 上下文
func (g *GlobalManager) GetOrCreateProject(projectID string) (*ProjectContext, error) {
    g.mu.RLock()
    if ctx, exists := g.projects[projectID]; exists {
        g.mu.RUnlock()
        return ctx, nil
    }
    g.mu.RUnlock()
    
    // 创建新的 Project 上下文
    g.mu.Lock()
    defer g.mu.Unlock()
    
    ctx := &ProjectContext{
        ProjectID:    projectID,
        TeamsManager: NewAgentTeamsManager(),
        ProjectMemory: NewProjectMemory(projectID),
        TaskStore:    NewTaskStore(),
        // 星型拓扑下不需要 MessageBus
    }
    
    g.projects[projectID] = ctx
    return ctx, nil
}
```

**隔离保证**：
- 每个 Project 有独立的 AgentTeamsManager（包含 Leader 和 Teammates）
- 每个 Project 有独立的 TaskStore（任务不共享）
- 每个 Project 有独立的 Worktree 空间
- 跨 Project 只能通过 CrossProjectMemory（显式共享）
- 通信通过 Eino 的 SessionValues（Project 内共享）和 ToolCall/ToolResult（Leader-Teammate）

### 3.3 同类型 Agent 数量限制与任务排队

**核心思想**：同类型 Teammate 最多创建 3 个实例，超出时任务进入队列等待。

**设计原因**：
- 避免资源过度消耗（LLM API 调用、文件系统、内存）
- 强制任务优先级排序
- 简化管理和调试
- 符合实际团队协作模式（小团队高效）

**实现方式**：

```go
const MaxTeammatesPerType = 3

type TeammateType string

const (
    TeammateTypeCoder      TeammateType = "coder"
    TeammateTypeResearcher TeammateType = "researcher"
    TeammateTypeReviewer   TeammateType = "reviewer"
    TeammateTypeTester     TeammateType = "tester"
)

type TeammatePool struct {
    // 按类型分组
    pools map[TeammateType][]*TeammateAgent
    
    // 任务队列（按类型）
    taskQueues map[TeammateType]*TaskQueue
    
    mu sync.RWMutex
}

// TaskQueue 任务队列
type TaskQueue struct {
    tasks []*QueuedTask
    
    // 通知通道（有新任务时通知）
    notify chan struct{}
}

type QueuedTask struct {
    Task       *Task
    Priority   int       // 优先级（0 最高）
    EnqueuedAt time.Time
}

// CreateTeammate 创建 Teammate（带类型限制）
func (p *TeammatePool) CreateTeammate(config TeammateConfig) (*TeammateAgent, error) {
    p.mu.Lock()
    defer p.mu.Unlock()
    
    agentType := TeammateType(config.Role)
    
    // 检查是否达到上限
    if len(p.pools[agentType]) >= MaxTeammatesPerType {
        return nil, &TeammateLimitError{
            Type:  agentType,
            Limit: MaxTeammatesPerType,
            Message: fmt.Sprintf(
                "Maximum %d teammates of type '%s' already exist. "+
                "Task will be queued until one becomes available.",
                MaxTeammatesPerType, agentType,
            ),
        }
    }
    
    // 创建 Teammate
    teammate := NewTeammateAgent(config, ...)
    p.pools[agentType] = append(p.pools[agentType], teammate)
    
    return teammate, nil
}

// SubmitTask 提交任务（自动排队或分配）
func (p *TeammatePool) SubmitTask(task *Task, agentType TeammateType) error {
    p.mu.Lock()
    defer p.mu.Unlock()
    
    // 1. 检查是否有空闲的 Teammate
    available := p.findAvailableTeammate(agentType)
    if available != nil {
        // 直接分配
        return p.assignTask(available, task)
    }
    
    // 2. 检查是否可以创建新的 Teammate
    if len(p.pools[agentType]) < MaxTeammatesPerType {
        // 创建新的 Teammate 并分配
        teammate, _ := p.createTeammate(agentType)
        return p.assignTask(teammate, task)
    }
    
    // 3. 加入队列等待
    queued := &QueuedTask{
        Task:       task,
        Priority:   task.Priority,
        EnqueuedAt: time.Now(),
    }
    p.taskQueues[agentType].Enqueue(queued)
    
    return nil
}

// findAvailableTeammate 查找空闲的 Teammate
func (p *TeammatePool) findAvailableTeammate(agentType TeammateType) *TeammateAgent {
    teammates := p.pools[agentType]
    for _, t := range teammates {
        if t.status == "idle" {
            return t
        }
    }
    return nil
}

// OnTeammateIdle 当 Teammate 完成时触发
func (p *TeammatePool) OnTeammateIdle(teammate *TeammateAgent) {
    p.mu.Lock()
    defer p.mu.Unlock()
    
    agentType := TeammateType(teammate.role)
    queue := p.taskQueues[agentType]
    
    // 检查队列中是否有等待的任务
    if nextTask := queue.Dequeue(); nextTask != nil {
        // 分配给当前 Teammate
        p.assignTask(teammate, nextTask.Task)
    } else {
        // 标记为空闲
        teammate.status = "idle"
    }
}
```

**任务队列策略**：
- **FIFO** - 默认先进先出
- **优先级** - 支持任务优先级（0 最高）
- **超时** - 队列中的任务可以设置超时时间
- **取消** - 支持取消队列中的任务

### 3.4 Agent 持久化与恢复

**核心思想**：Agent 上下文支持持久化，进程退出后可以恢复并继续工作。

**持久化内容**：

```go
type AgentSnapshot struct {
    // 基础信息
    ID        string    `json:"id"`
    Name      string    `json:"name"`
    Role      string    `json:"role"`
    CreatedAt time.Time `json:"created_at"`
    
    // 状态
    Status string `json:"status"` // idle / working / blocked
    
    // 对话历史（Eino 的 messages）
    ConversationHistory []Message `json:"conversation_history"`
    
    // 当前任务
    CurrentTask *Task `json:"current_task,omitempty"`
    
    // 工作目录
    WorkDir string `json:"work_dir"`
    
    // Worktree 信息（如果有）
    Worktree *WorktreeSnapshot `json:"worktree,omitempty"`
    
    // 文件访问记录
    FileAccessLog []FileAccessRecord `json:"file_access_log"`
    
    // 自定义状态（Agent 可以存储任意数据）
    CustomState map[string]interface{} `json:"custom_state,omitempty"`
}

type WorktreeSnapshot struct {
    ID     string `json:"id"`
    Path   string `json:"path"`
    Branch string `json:"branch"`
    Status string `json:"status"`
}

type FileAccessRecord struct {
    FilePath  string    `json:"file_path"`
    Operation string    `json:"operation"` // read / write
    Timestamp time.Time `json:"timestamp"`
    MTime     time.Time `json:"mtime"`
}
```

**持久化存储**：

```go
type AgentPersistence struct {
    // 存储目录：{project_dir}/.agent-teams/snapshots/
    storageDir string
    
    // 压缩（可选）
    compression bool
}

// Save 保存 Agent 快照
func (p *AgentPersistence) Save(agent *TeammateAgent) error {
    snapshot := agent.CreateSnapshot()
    
    data, err := json.MarshalIndent(snapshot, "", "  ")
    if err != nil {
        return err
    }
    
    // 可选：压缩
    if p.compression {
        data = compress(data)
    }
    
    filename := fmt.Sprintf("%s.json", agent.id)
    filepath := filepath.Join(p.storageDir, filename)
    
    return os.WriteFile(filepath, data, 0644)
}

// Load 加载 Agent 快照
func (p *AgentPersistence) Load(agentID string) (*AgentSnapshot, error) {
    filename := fmt.Sprintf("%s.json", agentID)
    filepath := filepath.Join(p.storageDir, filename)
    
    data, err := os.ReadFile(filepath)
    if err != nil {
        return nil, err
    }
    
    // 可选：解压
    if p.compression {
        data = decompress(data)
    }
    
    var snapshot AgentSnapshot
    err = json.Unmarshal(data, &snapshot)
    if err != nil {
        return nil, err
    }
    
    return &snapshot, nil
}

// List 列出所有保存的 Agent
func (p *AgentPersistence) List() ([]*AgentSnapshot, error) {
    files, err := os.ReadDir(p.storageDir)
    if err != nil {
        return nil, err
    }
    
    var snapshots []*AgentSnapshot
    for _, file := range files {
        if filepath.Ext(file.Name()) == ".json" {
            agentID := strings.TrimSuffix(file.Name(), ".json")
            snapshot, err := p.Load(agentID)
            if err != nil {
                continue // 跳过损坏的文件
            }
            snapshots = append(snapshots, snapshot)
        }
    }
    
    return snapshots, nil
}

// Delete 删除 Agent 快照
func (p *AgentPersistence) Delete(agentID string) error {
    filename := fmt.Sprintf("%s.json", agentID)
    filepath := filepath.Join(p.storageDir, filename)
    return os.Remove(filepath)
}
```

**恢复流程**：

```go
// RestoreAgent 恢复 Agent
func (m *AgentTeamsManager) RestoreAgent(agentID string) (*TeammateAgent, error) {
    // 1. 加载快照
    snapshot, err := m.persistence.Load(agentID)
    if err != nil {
        return nil, fmt.Errorf("failed to load snapshot: %w", err)
    }
    
    // 2. 恢复 Worktree（如果有）
    if snapshot.Worktree != nil {
        // 检查 worktree 是否还存在
        if !m.worktreeMgr.WorktreeExists(snapshot.Worktree.ID) {
            return nil, fmt.Errorf("worktree no longer exists: %s", snapshot.Worktree.ID)
        }
    }
    
    // 3. 重建 Agent
    config := TeammateConfig{
        Name:         snapshot.Name,
        Role:         snapshot.Role,
        SystemPrompt: m.getSystemPromptForRole(snapshot.Role),
        MaxTurns:     30,
    }
    
    agent := NewTeammateAgent(config, ...)
    agent.id = snapshot.ID
    agent.status = snapshot.Status
    agent.conversationHistory = snapshot.ConversationHistory
    
    // 4. 恢复文件访问记录
    for _, record := range snapshot.FileAccessLog {
        m.fileTracker.RestoreRecord(agent.id, record.FilePath, record.MTime)
    }
    
    // 5. 恢复自定义状态
    agent.customState = snapshot.CustomState
    
    // 6. 注册到消息总线
    m.messageBus.RegisterAgent(agent.name)
    
    // 7. 继续未完成的任务（如果有）
    if snapshot.CurrentTask != nil {
        go agent.ExecuteTask(context.Background(), snapshot.CurrentTask)
    }
    
    return agent, nil
}

// ResumeProject 恢复整个 Project 的 Agent Teams
func (g *GlobalManager) ResumeProject(projectID string) error {
    ctx, err := g.GetOrCreateProject(projectID)
    if err != nil {
        return err
    }
    
    // 列出所有保存的 Agent
    snapshots, err := ctx.TeamsManager.persistence.List()
    if err != nil {
        return err
    }
    
    // 恢复每个 Agent
    for _, snapshot := range snapshots {
        _, err := ctx.TeamsManager.RestoreAgent(snapshot.ID)
        if err != nil {
            appLog.Warn("Failed to restore agent", "id", snapshot.ID, "error", err)
            continue
        }
    }
    
    return nil
}
```

**自动保存策略**：

```go
// AutoSave 自动保存 Agent 状态
func (a *TeammateAgent) AutoSave(interval time.Duration) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    
    for {
        select {
        case <-ticker.C:
            if err := a.persistence.Save(a); err != nil {
                appLog.Error("Auto-save failed", "error", err)
            }
        case <-a.stopChan:
            return
        }
    }
}

// 在关键事件时保存
func (a *TeammateAgent) OnTaskComplete() {
    a.persistence.Save(a)
}

func (a *TeammateAgent) OnMessageReceived() {
    a.persistence.Save(a)
}
```

### 3.5 两层 Memory 系统

**核心思想**：Memory 分为 Project 级（项目特定）和跨 Project 级（全局共享）。

**架构设计**：

```
┌──────────────────────────────────────────────────────┐
│                 Cross-Project Memory                  │
│  ┌────────────────────────────────────────────────┐  │
│  │  User Preferences (用户偏好)                    │  │
│  │  - 编码风格偏好                                 │  │
│  │  - 常用工具配置                                 │  │
│  │  - 通信语言偏好                                 │  │
│  └────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────┐  │
│  │  Global Knowledge (通用知识)                    │  │
│  │  - Eino 框架使用经验                            │  │
│  │  - Go 最佳实践                                  │  │
│  │  - 通用设计模式                                 │  │
│  └────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────┘
         │
         │ 查询
         ▼
┌──────────────────────────────────────────────────────┐
│              Project Memory (Project A)               │
│  ┌────────────────────────────────────────────────┐  │
│  │  Project Context (项目上下文)                   │  │
│  │  - 当前重构计划                                 │  │
│  │  - 已知问题与解决方案                           │  │
│  │  - 架构决策记录                                 │  │
│  └────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────┐  │
│  │  Project Knowledge (项目知识)                   │  │
│  │  - 代码库结构                                   │  │
│  │  - 模块依赖关系                                 │  │
│  │  - 测试覆盖情况                                 │  │
│  └────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────┘
```

**实现方式**：

```go
// Memory Memory 接口
type Memory interface {
    // 存储
    Store(key string, value interface{}) error
    
    // 查询
    Recall(query string) ([]MemoryEntry, error)
    
    // 删除
    Delete(key string) error
    
    // 列出所有
    List() ([]MemoryEntry, error)
}

type MemoryEntry struct {
    Key       string                 `json:"key"`
    Value     interface{}            `json:"value"`
    Metadata  map[string]interface{} `json:"metadata"`
    CreatedAt time.Time              `json:"created_at"`
    UpdatedAt time.Time              `json:"updated_at"`
}

// CrossProjectMemory 跨 Project Memory
type CrossProjectMemory struct {
    storageDir string // ~/.qoder/memory/
    
    entries map[string]*MemoryEntry
    mu      sync.RWMutex
}

// NewCrossProjectMemory 创建跨 Project Memory
func NewCrossProjectMemory() *CrossProjectMemory {
    storageDir := filepath.Join(os.Getenv("HOME"), ".qoder", "memory")
    os.MkdirAll(storageDir, 0755)
    
    mem := &CrossProjectMemory{
        storageDir: storageDir,
        entries:    make(map[string]*MemoryEntry),
    }
    
    // 加载已有数据
    mem.load()
    
    return mem
}

// Store 存储（自动持久化）
func (m *CrossProjectMemory) Store(key string, value interface{}) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    entry := &MemoryEntry{
        Key:       key,
        Value:     value,
        CreatedAt: time.Now(),
        UpdatedAt: time.Now(),
    }
    
    m.entries[key] = entry
    
    // 持久化
    return m.save()
}

// Recall 查询（支持语义搜索）
func (m *CrossProjectMemory) Recall(query string) ([]MemoryEntry, error) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    // 简单实现：关键词匹配
    // TODO: 集成向量数据库实现语义搜索
    var results []MemoryEntry
    for _, entry := range m.entries {
        if strings.Contains(strings.ToLower(query), strings.ToLower(entry.Key)) {
            results = append(results, *entry)
        }
    }
    
    return results, nil
}

// ProjectMemory Project 级 Memory
type ProjectMemory struct {
    projectID  string
    storageDir string // {project_dir}/.qoder/memory/
    
    entries map[string]*MemoryEntry
    mu      sync.RWMutex
    
    // 引用跨 Project Memory
    crossProjectMemory *CrossProjectMemory
}

// NewProjectMemory 创建 Project Memory
func NewProjectMemory(projectID, projectDir string, crossMem *CrossProjectMemory) *ProjectMemory {
    storageDir := filepath.Join(projectDir, ".qoder", "memory")
    os.MkdirAll(storageDir, 0755)
    
    mem := &ProjectMemory{
        projectID:          projectID,
        storageDir:         storageDir,
        entries:            make(map[string]*MemoryEntry),
        crossProjectMemory: crossMem,
    }
    
    // 加载已有数据
    mem.load()
    
    return mem
}

// Store 存储到 Project Memory
func (m *ProjectMemory) Store(key string, value interface{}) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    entry := &MemoryEntry{
        Key:       key,
        Value:     value,
        Metadata:  map[string]interface{}{"project": m.projectID},
        CreatedAt: time.Now(),
        UpdatedAt: time.Now(),
    }
    
    m.entries[key] = entry
    
    return m.save()
}

// Recall 查询（先查 Project，再查 Cross-Project）
func (m *ProjectMemory) Recall(query string) ([]MemoryEntry, error) {
    // 1. 查询 Project Memory
    projectResults, _ := m.recallLocal(query)
    
    // 2. 查询 Cross-Project Memory
    crossResults, _ := m.crossProjectMemory.Recall(query)
    
    // 3. 合并结果（Project 优先）
    results := append(projectResults, crossResults...)
    
    return results, nil
}

// recallLocal 只查询本地 Project Memory
func (m *ProjectMemory) recallLocal(query string) ([]MemoryEntry, error) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    var results []MemoryEntry
    for _, entry := range m.entries {
        if strings.Contains(strings.ToLower(query), strings.ToLower(entry.Key)) {
            results = append(results, *entry)
        }
    }
    
    return results, nil
}
```

**使用示例**：

```go
// Agent 使用 Memory
func (a *TeammateAgent) rememberSomething(important string) {
    // 存储到 Project Memory
    a.projectMemory.Store("important-fact", important)
}

func (a *TeammateAgent) recallSomething(query string) {
    // 查询（自动搜索 Project + Cross-Project）
    results, _ := a.projectMemory.Recall(query)
    
    // 使用查询结果
    for _, entry := range results {
        a.context = append(a.context, entry.Value)
    }
}

// 用户偏好（跨 Project）
func SaveUserPreference(key string, value interface{}) {
    crossMem := GetCrossProjectMemory()
    crossMem.Store("user-preference-"+key, value)
}

func GetUserPreference(key string) interface{} {
    crossMem := GetCrossProjectMemory()
    results, _ := crossMem.Recall("user-preference-" + key)
    if len(results) > 0 {
        return results[0].Value
    }
    return nil
}
```

**Memory 持久化格式**：

```json
// ~/.qoder/memory/cross-project.json
{
  "user-preference-language": {
    "key": "user-preference-language",
    "value": "chinese",
    "metadata": {"type": "preference"},
    "created_at": "2026-11-01T10:00:00Z",
    "updated_at": "2026-11-01T10:00:00Z"
  },
  "global-knowledge-eino-patterns": {
    "key": "global-knowledge-eino-patterns",
    "value": "AgentAsTool is recommended for multi-agent...",
    "metadata": {"type": "knowledge", "domain": "eino"},
    "created_at": "2026-11-01T10:00:00Z",
    "updated_at": "2026-11-01T10:00:00Z"
  }
}

// {project_dir}/.qoder/memory/project.json
{
  "refactor-plan-eino-migration": {
    "key": "refactor-plan-eino-migration",
    "value": "Vertical-slice migration: 7 slices (~27 days)...",
    "metadata": {"type": "plan", "status": "in-progress"},
    "created_at": "2026-11-01T10:00:00Z",
    "updated_at": "2026-11-01T10:00:00Z"
  }
}
```

## 4. 并行 Teammate 与 Worktree 隔离

### 4.1 同类型 Teammate 多实例并行

**设计原则**：支持创建多个相同角色的 Teammate（如多个 Coder），只要任务解耦，它们可以在独立的 git worktree 中并行工作，最后合并到主 worktree。

**合理性分析**：
- ✅ **符合真实团队协作模式** - 多个工程师并行开发不同功能
- ✅ **物理隔离避免冲突** - 每个 worktree 有独立的文件系统
- ✅ **提升吞吐量** - 无依赖的任务可以真正并行执行
- ✅ **降低上下文污染** - 每个 Teammate 只关注自己的任务

**使用场景示例**：

```
用户请求："重构认证模块、优化数据库查询、更新 API 文档"

Main Agent 分解任务：
- Task 1: 重构认证模块 → Coder-1 (worktree: feature/auth-refactor)
- Task 2: 优化数据库查询 → Coder-2 (worktree: perf/db-optimization)
- Task 3: 更新 API 文档 → Coder-3 (worktree: docs/api-update)

三个 Coder 并行工作，互不干扰
完成后合并到主 worktree
```

### 4.2 Worktree 管理架构

```go
type WorktreeManager struct {
    // 主 worktree 路径
    mainWorktree string
    
    // 活跃的 worktree 列表
    worktrees map[string]*Worktree
    
    // worktree 命名规则：{role}-{taskID}-{timestamp}
    // 例如：coder-task001-20261101-143052
}

type Worktree struct {
    // Worktree 唯一标识
    ID string
    
    // 文件系统路径
    Path string
    
    // Git 分支名
    Branch string
    
    // 分配的 Teammate
    TeammateID string
    
    // 关联的任务
    TaskID string
    
    // 创建时间
    CreatedAt time.Time
    
    // 状态：active / merged / discarded
    Status string
}

// 创建隔离的 worktree
func (m *WorktreeManager) CreateWorktree(teammateID, taskID string) (*Worktree, error) {
    // 1. 生成唯一名称
    timestamp := time.Now().Format("20060102-150405")
    name := fmt.Sprintf("teammate-%s-%s", teammateID, timestamp)
    branch := fmt.Sprintf("teammate/%s/%s", teammateID, taskID)
    path := filepath.Join(os.TempDir(), "agent-teams-worktrees", name)
    
    // 2. 创建 worktree
    cmd := exec.Command("git", "worktree", "add", "-b", branch, path)
    if err := cmd.Run(); err != nil {
        return nil, fmt.Errorf("failed to create worktree: %w", err)
    }
    
    // 3. 记录元数据
    wt := &Worktree{
        ID:         name,
        Path:       path,
        Branch:     branch,
        TeammateID: teammateID,
        TaskID:     taskID,
        CreatedAt:  time.Now(),
        Status:     "active",
    }
    m.worktrees[name] = wt
    
    return wt, nil
}

// 合并 worktree 到主分支
func (m *WorktreeManager) MergeWorktree(worktreeID string) error {
    wt, ok := m.worktrees[worktreeID]
    if !ok {
        return fmt.Errorf("worktree not found: %s", worktreeID)
    }
    
    // 1. 切换到主 worktree
    if err := os.Chdir(m.mainWorktree); err != nil {
        return err
    }
    
    // 2. 合并分支
    cmd := exec.Command("git", "merge", wt.Branch, "--no-ff", 
        "-m", fmt.Sprintf("Merge teammate work: %s", wt.TaskID))
    if err := cmd.Run(); err != nil {
        return fmt.Errorf("merge failed: %w (may need manual resolution)", err)
    }
    
    // 3. 清理 worktree
    cmd = exec.Command("git", "worktree", "remove", wt.Path)
    cmd.Run()
    
    // 4. 删除分支
    cmd = exec.Command("git", "branch", "-d", wt.Branch)
    cmd.Run()
    
    // 5. 更新状态
    wt.Status = "merged"
    
    return nil
}
```

### 4.3 Teammate 与 Worktree 绑定

```go
type TeammateAgent struct {
    // ... 其他字段 ...
    
    // 分配的 worktree（可选）
    worktree *Worktree
    
    // 工作目录（覆盖全局 workdir）
    workDir string
}

// 创建带 worktree 隔离的 Teammate
func (m *AgentTeamsManager) CreateIsolatedTeammate(config TeammateConfig, taskID string) (*TeammateAgent, error) {
    // 1. 创建 worktree
    worktree, err := m.worktreeMgr.CreateWorktree(config.Name, taskID)
    if err != nil {
        return nil, err
    }
    
    // 2. 创建 Teammate
    teammate, err := m.CreateTeammate(config)
    if err != nil {
        return nil, err
    }
    
    // 3. 绑定 worktree
    teammate.worktree = worktree
    teammate.workDir = worktree.Path
    
    // 4. 在 system prompt 中注入 worktree 信息
    teammate.systemPrompt += fmt.Sprintf(
        "\n\nYou are working in an isolated worktree: %s\n"+
        "All file operations should use this directory as the working directory.\n"+
        "When you complete your task, your changes will be merged back to the main branch.",
        worktree.Path,
    )
    
    return teammate, nil
}
```

### 4.4 合并策略与冲突处理

**自动合并流程**：

```
Teammate 完成任务
  ↓
Main Agent 收到完成通知
  ↓
尝试自动合并 (git merge --no-ff)
  ↓
合并成功？
  ├─ 是 → 清理 worktree，标记任务完成
  └─ 否 → 进入冲突解决流程
```

**冲突处理方案**：

```go
type MergeConflict struct {
    WorktreeID string
    Files      []string // 冲突文件列表
    Reason     string
}

// 处理合并冲突
func (m *AgentTeamsManager) HandleMergeConflict(conflict *MergeConflict) error {
    // 方案 1：返回错误给 Leader，让 Leader LLM 决定如何处理
    // Leader 会在下一个 ToolResult 中收到冲突信息
    return &MergeConflictError{
        Files:   conflict.Files,
        Message: fmt.Sprintf("Merge conflict in files: %v. Please resolve manually or assign to a specialist.", conflict.Files),
    }
    
    // 方案 2：Leader 决定创建专门的 "conflict-resolver" Teammate
    // （这由 Leader LLM 自主决定，通过 ToolCall 调用）
    // resolver, _ := m.CreateTeammateAsTool(TeammateConfig{
    //     Name: "conflict-resolver",
    //     Type: TeammateTypeReviewer,
    //     Role: "Git merge conflict resolution specialist",
    // })
    // 然后 Leader 调用 resolver 工具来解决冲突
    
    // 方案 3：回滚并让原 Teammate 重新工作
    // （适用于冲突严重且无法自动解决的情况）
}
```

## 5. 并发控制：基于时间戳的乐观锁

### 5.1 设计原则

**不使用互斥锁**，而是采用**乐观并发控制**（Optimistic Concurrency Control）：
- 每个 Agent 在写文件前必须先读过该文件
- 写入时检查文件的时间戳（mtime）
- 如果 mtime 与读取时不同，说明文件已被其他 Agent 修改
- 写入失败，返回错误信息让模型重新思考

**优势**：
- ✅ 无死锁风险
- ✅ 性能更好（无锁等待）
- ✅ 符合 Git 的冲突解决哲学
- ✅ 强制 Agent 基于最新状态工作

### 5.2 文件访问跟踪

```go
type FileAccessTracker struct {
    // Agent 对文件的读取记录
    // key: agentID + filePath, value: last read timestamp
    reads map[string]time.Time
    
    // 文件当前的 mtime 缓存
    mtimes map[string]time.Time
    
    mu sync.RWMutex
}

// 记录 Agent 读取文件
func (t *FileAccessTracker) RecordRead(agentID, filePath string) error {
    t.mu.Lock()
    defer t.mu.Unlock()
    
    // 获取文件当前 mtime
    info, err := os.Stat(filePath)
    if err != nil {
        return err
    }
    
    key := agentID + ":" + filePath
    t.reads[key] = info.ModTime()
    t.mtimes[filePath] = info.ModTime()
    
    return nil
}

// 检查 Agent 是否可以写入文件
func (t *FileAccessTracker) CheckWritePermission(agentID, filePath string) (bool, string) {
    t.mu.RLock()
    defer t.mu.RUnlock()
    
    // 1. 检查 Agent 是否读过该文件
    key := agentID + ":" + filePath
    lastRead, hasRead := t.reads[key]
    if !hasRead {
        return false, "You must read the file before writing to it"
    }
    
    // 2. 获取文件当前 mtime
    info, err := os.Stat(filePath)
    if err != nil {
        return false, fmt.Sprintf("Failed to stat file: %v", err)
    }
    currentMtime := info.ModTime()
    
    // 3. 比较时间戳
    if !currentMtime.Equal(lastRead) {
        return false, fmt.Sprintf(
            "File has been modified by another agent since you last read it.\n"+
            "Last read: %v\n"+
            "Current:   %v\n"+
            "Please re-read the file and reconsider your changes.",
            lastRead.Format(time.RFC3339),
            currentMtime.Format(time.RFC3339),
        )
    }
    
    return true, ""
}
```

### 5.3 集成到文件工具

```go
// 修改 write_file 工具实现
func writeFileTool(ctx context.Context, input *WriteFileInput) (string, error) {
    agentID := getAgentIDFromContext(ctx)
    tracker := getFileAccessTrackerFromContext(ctx)
    
    // 1. 检查写入权限
    allowed, reason := tracker.CheckWritePermission(agentID, input.Path)
    if !allowed {
        return "", &FileAccessError{
            Message: reason,
            Code:    "CONCURRENT_MODIFICATION",
        }
    }
    
    // 2. 执行写入
    err := os.WriteFile(input.Path, [](input.Content), 0644)
    if err != nil {
        return "", err
    }
    
    // 3. 更新 mtime 缓存
    tracker.RecordWrite(agentID, input.Path)
    
    return "File written successfully", nil
}

// 修改 read_file 工具实现
func readFileTool(ctx context.Context, input *ReadFileInput) (string, error) {
    agentID := getAgentIDFromContext(ctx)
    tracker := getFileAccessTrackerFromContext(ctx)
    
    // 1. 读取文件内容
    content, err := os.ReadFile(input.Path)
    if err != nil {
        return "", err
    }
    
    // 2. 记录读取操作
    tracker.RecordRead(agentID, input.Path)
    
    return string(content), nil
}
```

### 5.4 错误处理与模型反馈

```go
type FileAccessError struct {
    Message string
    Code    string // CONCURRENT_MODIFICATION / NOT_READ_FIRST / etc.
}

// 在 Agent Loop 中处理文件访问错误
func handleFileAccessError(err *FileAccessError, messages []anthropic.MessageParam) []anthropic.MessageParam {
    // 注入错误信息到对话，让模型重新思考
    errorMsg := fmt.Sprintf(
        "<file_access_error code=\"%s\">\n%s\n</file_access_error>",
        err.Code,
        err.Message,
    )
    
    messages = append(messages, anthropic.NewUserMessage(
        anthropic.NewTextBlock(errorMsg),
    ))
    
    return messages
}
```

### 5.5 Worktree 隔离下的并发控制

**关键洞察**：当使用 Worktree 隔离时，并发控制变得简单：
- 不同 Teammate 工作在不同的 worktree，文件系统完全隔离
- 不存在并发写入同一文件的问题
- 冲突只在合并阶段出现，由 Git 处理

```go
// Worktree 模式下的简化并发控制
func (t *TeammateAgent) writeFileInWorktree(path, content string) error {
    // 1. 转换为 worktree 内的路径
    worktreePath := filepath.Join(t.workDir, path)
    
    // 2. 直接写入（无并发冲突）
    return os.WriteFile(worktreePath, []byte(content), 0644)
}

// 合并时由 Git 处理冲突
func (m *WorktreeManager) MergeWorktree(worktreeID string) error {
    // Git merge 会自动检测文件冲突
    // 如果冲突，返回错误让 Main Agent 决策
}
```

**推荐策略**：
- **小任务、高耦合** → 共享 worktree + 时间戳并发控制
- **大任务、低耦合** → 独立 worktree + Git 合并冲突处理
- **混合模式** → Main Agent 根据任务特性动态选择

### 5.6 星型拓扑下的任务协作流程

**通信机制**：Leader 通过 ToolCall 调用 Teammate，Teammate 通过 ToolResult 返回结果

```
用户请求
  ↓
Leader Agent 接收请求
  ↓
Leader LLM 分析并分解任务
  ↓
Leader 创建 Task 列表（TaskStore）
  ↓
Leader 通过 ToolCall 调用 Teammate：
  ├─ ToolCall{name: "coder_1", args: "实现功能A"}
  ├─ ToolCall{name: "coder_2", args: "实现功能B"}
  └─ ToolCall{name: "researcher_1", args: "调研技术方案"}
  ↓
Teammates 并行执行（各自在 IsolatedSession 中）：
  ├─ Coder 1: 执行功能A
  ├─ Coder 2: 执行功能B
  └─ Researcher 1: 调研技术方案
  ↓
Teammates 返回结果（通过 ToolResult）：
  ├─ ToolResult{coder_1: "功能A已完成，代码在..."}
  ├─ ToolResult{coder_2: "功能B已完成，代码在..."}
  └─ ToolResult{researcher_1: "调研结果：推荐使用..."}
  ↓
Leader Agent 收集所有 ToolResult
  ↓
Leader LLM 整合结果并返回给用户
```

**关键点**：
- Teammate **没有 SendMessage 工具** - 无法直接与其他 Teammate 通信
- 所有协调由 Leader LLM 自主决定（通过 ToolCall）
- 结果通过 ToolResult 自动返回给 Leader
- 如果需要 Teammate 之间协调，必须由 Leader 中转：
  1. Leader 调用 Teammate A 获取结果
  2. Leader 将结果作为参数传递给 Teammate B

## 6. 生命周期管理

### 5.1 Teammate 创建

```go
type TeammateConfig struct {
    Name        string            // Agent 名称（用于寻址）
    Role        string            // 角色描述
    SystemPrompt string           // 系统提示词
    Tools       []tool.BaseTool   // 可用工具列表
    Model       model.ChatModel   // LLM 模型
    MaxTurns    int               // 最大执行轮数
}

func (m *AgentTeamsManager) CreateTeammate(config TeammateConfig) (*TeammateAgent, error) {
    // 1. 创建 Eino Agent
    agent, err := adk.NewAgent(
        adk.WithName(config.Name),
        adk.WithDescription(config.Role),
        adk.WithSystemPrompt(config.SystemPrompt),
        adk.WithTools(config.Tools...),
        adk.WithModel(config.Model),
    )
    
    // 2. 包装为 TeammateAgent
    teammate := &TeammateAgent{
        id:     generateID(),
        name:   config.Name,
        role:   config.Role,
        agent:  agent,
        inbox:  make(chan *Message, 100),
        tasks:  m.taskStore,
        status: "idle",
    }
    
    // 3. 注册到 Manager
    m.agents[teammate.id] = teammate
    
    // 4. 启动消息处理循环
    go teammate.messageLoop()
    
    return teammate, nil
}
```

### 5.2 会话级持久化

```go
type SessionState struct {
    // 会话 ID
    SessionID string
    
    // Agent 列表
    Agents map[string]*TeammateAgent
    
    // 任务历史
    TaskHistory []*Task
    
    // 消息历史
    MessageHistory []*Message
    
    // 创建时间
    CreatedAt time.Time
    
    // 最后活跃时间
    LastActiveAt time.Time
}

// 保存到持久化存储
func (s *SessionState) Save() error {
    data, err := json.Marshal(s)
    if err != nil {
        return err
    }
    return os.WriteFile(s.sessionFile(), data, 0644)
}

// 从持久化存储恢复
func LoadSession(sessionID string) (*SessionState, error) {
    data, err := os.ReadFile(sessionFile(sessionID))
    if err != nil {
        return nil, err
    }
    var state SessionState
    err = json.Unmarshal(data, &state)
    return &state, err
}
```

**注意**：根据 Qoder 文档，Teammate 状态在 TUI 会话内持久化，但 **不会在 resume 时恢复**。这意味着：
- 会话期间可以创建和销毁 Teammates
- 关闭 TUI 后 Teammate 状态丢失
- Resume 只恢复对话历史，不恢复 Teammate 实例

### 5.3 清理与销毁

```go
func (m *AgentTeamsManager) DestroyTeammate(agentID string) error {
    teammate, ok := m.agents[agentID]
    if !ok {
        return fmt.Errorf("agent not found: %s", agentID)
    }
    
    // 1. 停止消息处理
    close(teammate.inbox)
    
    // 2. 取消正在执行的任务
    teammate.cancel()
    
    // 3. 从注册表移除
    delete(m.agents, agentID)
    
    return nil
}

func (m *AgentTeamsManager) Shutdown() error {
    // 清理所有 Teammates
    for id := range m.agents {
        m.DestroyTeammate(id)
    }
    
    // 保存会话状态（如果需要）
    return m.session.Save()
}
```

## 7. 与现有 Subagent 的集成

### 6.1 渐进式迁移策略

根据用户的增量重构偏好（每个重构步骤必须产出可运行代码），建议分三阶段迁移：

**阶段 1：并行存在**
- 保留现有 Subagent 实现（`task.go`）
- 新增 Agent Teams Manager
- 用户可通过配置选择使用哪种模式

**阶段 2：功能对齐**
- Agent Teams 支持 Subagent 的所有功能
- 提供迁移工具，将 Subagent 调用转换为 Teammate 委派
- 文档更新，推荐使用 Agent Teams

**阶段 3：统一架构**
- 废弃 Subagent 实现
- Agent Teams 成为唯一的多 Agent 协作方式
- 优化性能与资源占用

### 6.2 兼容层设计

```go
// SubagentCompat 提供与现有 Subagent API 的兼容
type SubagentCompat struct {
    manager *AgentTeamsManager
}

// runSubagent 保持与现有 task.go 相同的签名
func (c *SubagentCompat) runSubagent(st *sessionState, prompt string) string {
    // 1. 创建临时 Teammate
    teammate, _ := c.manager.CreateTeammate(TeammateConfig{
        Name:        "temp-subagent",
        Role:        "Temporary subagent for single task",
        SystemPrompt: subSystemPrompt,
        MaxTurns:    subagentMaxTurns,
    })
    
    // 2. 执行任务
    result, _ := teammate.ExecuteTask(&Task{
        Description: prompt,
    })
    
    // 3. 销毁 Teammate
    c.manager.DestroyTeammate(teammate.id)
    
    return result.Summary
}
```

## 8. 可观测性（Eino Callbacks）

### 8.1 Eino Callbacks 集成

**设计决策**：使用 Eino 原生的 Callbacks 机制实现可观测性，零代码侵入。

```go
import (
    "github.com/cloudwego/eino/callbacks"
    "github.com/cloudwego/eino-ext/callbacks/trace"
    "github.com/cloudwego/eino-ext/callbacks/metrics"
    "github.com/cloudwego/eino-ext/callbacks/logging"
)

// 初始化可观测性（应用启动时调用一次）
func initObservability() {
    callbacks.InitCallbackHandlers([]callbacks.Handler{
        // 1. 分布式追踪（OpenTelemetry 兼容）
        trace.NewTraceHandler(
            trace.WithServiceName("agent-teams"),
            trace.WithEndpoint("http://jaeger:14268/api/traces"),
            trace.WithSampler(trace.AlwaysSample()),
        ),
        
        // 2. 指标监控（Prometheus 兼容）
        metrics.NewMetricsHandler(
            metrics.WithNamespace("agent_teams"),
            metrics.WithSubsystem("execution"),
            metrics.WithDurationBuckets([]float64{0.1, 0.5, 1, 2, 5, 10}),
        ),
        
        // 3. 结构化日志
        logging.NewLoggingHandler(
            logging.WithLevel(logging.InfoLevel),
            logging.WithFormat(logging.JSONFormat),
        ),
    })
}
```

### 8.2 自动收集的信息

使用 Eino Callbacks 后，以下信息会**自动收集**（无需手动埋点）：

| 信息类型 | 收集内容 | 用途 |
|---------|---------|------|
| **ToolCall** | 工具名称、输入参数、调用时间 | 调试 Agent 决策 |
| **ToolResult** | 返回结果、执行时间、是否成功 | 性能分析 |
| **LLM 调用** | 模型名称、token 用量、延迟 | 成本监控 |
| **Agent 执行** | 总轮数、总时间、最终输出 | 端到端追踪 |
| **错误** | 错误类型、堆栈、上下文 | 异常诊断 |

### 8.3 Agent Teams 特定指标

除了 Eino 自动收集的指标，还可以添加业务特定指标：

```go
// 自定义指标（通过 Metrics Handler 记录）
type TeamsMetrics struct {
    // 业务指标
    ActiveTeammates    int64  // 活跃 Teammate 数量
    TasksCompleted     int64  // 完成的任务数
    TasksFailed        int64  // 失败的任务数
    MergeConflicts     int64  // 合并冲突次数
    
    // 性能指标（Eino 自动收集）
    ToolCallDuration   float64 // 工具调用延迟
    LLMTokensUsed      int64   // Token 消耗
    AgentTurnCount     int     // Agent 执行轮数
}

// 记录自定义指标
func (m *AgentTeamsManager) recordMetrics(metric string, value interface{}) {
    // 通过 Eino Callbacks 记录
    callbacks.OnMetric(m.ctx, metric, value)
}
```

### 8.4 追踪示例

一次完整的 Agent Teams 执行会产生如下追踪链路：

```
Trace ID: abc123
├─ Span: leader_agent.execute (10.5s)
│  ├─ Span: llm.call_qwen_max (2.1s, 1500 tokens)
│  ├─ Span: tool.create_teammate (0.3s)
│  ├─ Span: tool.assign_task (0.1s)
│  ├─ Span: teammate_coder.execute (6.2s)
│  │  ├─ Span: llm.call_qwen_max (1.8s, 1200 tokens)
│  │  ├─ Span: tool.read_file (0.2s)
│  │  ├─ Span: tool.write_file (0.3s)
│  │  └─ Span: llm.call_qwen_max (2.1s, 1400 tokens)
│  ├─ Span: tool.collect_results (0.1s)
│  └─ Span: llm.call_qwen_max (1.8s, 1300 tokens)
└─ Total: 10.5s, 6700 tokens
```

**优势**：
- ✅ 零代码侵入（只需初始化 Callbacks）
- ✅ 自动记录所有 ToolCall 和 LLM 调用
- ✅ 支持 OpenTelemetry 标准（可导出到 Jaeger/Prometheus）
- ✅ 便于调试和性能优化

// 配置 Logging
logging.NewLoggingHandler()
```

### 7.2 Agent Teams 特定指标

```go
type TeamsMetrics struct {
    // 活跃 Teammate 数量
    ActiveTeammates int64
    
    // 消息发送/接收计数
    MessagesSent     int64
    MessagesReceived int64
    
    // 任务状态统计
    TasksPending   int64
    TasksRunning   int64
    TasksCompleted int64
    TasksFailed    int64
    
    // 执行时间分布
    TaskDurationHistogram *metrics.Histogram
}
```

## 9. 配置与启用

### 8.1 环境变量

```bash
# 启用 Agent Teams（对齐 Qoder CLI）
export QODER_AGENT_TEAMS=1

# 配置最大 Teammate 数量
export AGENT_TEAMS_MAX_TEAMMATES=5

# 配置消息队列大小
export AGENT_TEAMS_MESSAGE_QUEUE_SIZE=100

# 配置任务超时时间
export AGENT_TEAMS_TASK_TIMEOUT=300s
```

### 8.2 配置文件

```yaml
# config.yaml
agent_teams:
  enabled: true
  max_teammates: 5
  message_queue_size: 100
  task_timeout: 300s
  
  # 默认 Teammate 配置
  defaults:
    max_turns: 30
    model: "qwen-max"
    tools:
      - read_file
      - write_file
      - bash
      - search
  
  # 预定义角色模板
  templates:
    researcher:
      system_prompt: "You are a research agent. Find information and summarize findings."
      tools: [web_search, web_fetch, read_file]
    
    coder:
      system_prompt: "You are a coding agent. Implement features and fix bugs."
      tools: [read_file, write_file, bash, edit]
    
    reviewer:
      system_prompt: "You are a code review agent. Check for bugs and suggest improvements."
      tools: [read_file, glob, grep]
```

## 10. 使用示例

### 10.1 创建 Teammates（星型拓扑）

```go
// 在 Leader Agent 的 system prompt 中说明可用能力
leaderPrompt := `You are the leader of an agent team. You can:
1. Create teammates using the create_teammate tool
   - Each teammate becomes a tool you can call
2. Assign tasks using the assign_task tool
3. Collect results using the collect_results tool
4. Invoke teammates directly by calling their tools (e.g., "coder_1", "researcher_1")

Communication model (Star Topology):
- YOU are the central hub - all coordination goes through you
- Teammates CANNOT communicate with each other directly
- To coordinate between teammates, YOU must pass information
- Example: If researcher finds info that coder needs, YOU call coder with that info

When to use teammates:
- Complex tasks that can be parallelized
- Tasks requiring different expertise (research, coding, review)
- Multi-step workflows with dependencies

When NOT to use teammates:
- Simple queries that one agent can handle
- Tasks with strict sequential dependencies
- Quick lookups or single-file operations
`

// 创建 Teammate 工具（封装为 Leader 的 AgentAsTool）
createTeammateTool := tool.NewInvokableTool(
    tool.WithName("create_teammate"),
    tool.WithDescription("Create a new teammate agent and register as a tool"),
    tool.WithInvokableFunc(func(ctx context.Context, input *CreateTeammateInput) (string, error) {
        manager := getTeamsManagerFromContext(ctx)
        
        // 创建 Teammate 并封装为 AgentAsTool
        teammate, err := manager.CreateTeammateAsTool(TeammateConfig{
            Name:         input.Name,
            Type:         input.Type, // coder, researcher, reviewer, etc.
            SystemPrompt: input.SystemPrompt,
            Tools:        input.Tools,
        })
        if err != nil {
            return "", err
        }
        
        return fmt.Sprintf("Teammate created and registered as tool: %s (type: %s)", 
            teammate.name, teammate.role), nil
    }),
)
```

### 10.2 典型工作流（星型拓扑）

```
User: "帮我实现一个用户认证模块，包括设计、编码、测试"

Leader Agent 执行流程：

1. 分解任务：
   - Task 1: 设计认证流程
   - Task 2: 实现认证代码（依赖 Task 1）
   - Task 3: 编写测试用例（依赖 Task 2）

2. 创建 Teammates（注册为 Leader 的工具）：
   - create_teammate(name="researcher_1", type="researcher")
   - create_teammate(name="coder_1", type="coder")
   - create_teammate(name="tester_1", type="tester")

3. 分配任务到 TaskStore：
   - assign_task(task_id="1", assignee="researcher_1")
   - assign_task(task_id="2", assignee="coder_1", blocked_by=["1"])
   - assign_task(task_id="3", assignee="tester_1", blocked_by=["2"])

4. 执行任务（Leader 通过 ToolCall 调用 Teammate）：
   
   Step 4a: Leader 调用 researcher_1
   - ToolCall{name: "researcher_1", args: "设计认证流程..."}
   - researcher_1 在 IsolatedSession 中执行
   - ToolResult: "设计方案：使用 JWT + OAuth2..."
   
   Step 4b: Leader 收到结果，调用 coder_1（传递设计方案）
   - ToolCall{name: "coder_1", args: "基于以下设计实现代码：{设计方案}..."}
   - coder_1 执行
   - ToolResult: "代码已完成，文件：auth.go, jwt.go..."
   
   Step 4c: Leader 收到结果，调用 tester_1（传递代码位置）
   - ToolCall{name: "tester_1", args: "为以下代码编写测试：auth.go, jwt.go..."}
   - tester_1 执行
   - ToolResult: "测试用例已完成，覆盖率 85%..."

5. 汇总结果：
   - Leader 整合所有 ToolResult
   - 生成最终报告给用户
   - 包括：设计方案、代码、测试报告

关键点：
- 所有协调由 Leader 自主决定
- Teammate 之间无直接通信
- Leader 负责在 Teammate 之间传递信息
```

## 11. 技术风险与缓解

### 10.1 并发控制

**风险**：多个 Teammate 同时修改文件导致冲突

**缓解**：
- **乐观并发控制**：基于时间戳的无锁设计（详见第 4 节）
  - 写入前必须读取文件
  - 写入时检查 mtime，若已变更则失败
  - 强制模型基于最新状态重新思考
- **Worktree 隔离**：独立 worktree 避免文件系统级冲突（详见第 3 节）
  - 不同 Teammate 工作在不同 worktree
  - 冲突仅在合并阶段出现，由 Git 处理
- **任务分配策略**：Main Agent 根据任务耦合度选择工作模式
  - 高耦合任务 → 共享 worktree + 时间戳控制
  - 低耦合任务 → 独立 worktree + Git 合并

### 11.2 ToolCall 失败

**风险**：Leader 调用 Teammate 时 Eino AgentAsTool 执行失败

**缓解**：
- Eino 框架自动处理 ToolResult 返回（包括错误）
- Leader LLM 可以根据错误信息决定重试或调整策略
- 任务状态持久化到 TaskStore，支持恢复

### 11.3 资源泄漏

**风险**：Teammate 异常退出导致资源未释放

**缓解**：
- 使用 `defer` 确保清理逻辑执行
- 定期健康检查
- 超时自动销毁

### 10.4 上下文爆炸

**风险**：共享上下文过大导致性能下降

**缓解**：
- 按需传递上下文（Eino 的 `WithState` 支持）
- 定期压缩历史消息（现有 `compact.go`）
- 限制消息队列大小

## 12. 实施路线图

### Phase 1: 基础框架（2 周）
- [ ] 实现 AgentTeamsManager（Leader 管理）
- [ ] 实现 TaskStore
- [ ] 基础测试覆盖

### Phase 2: Eino AgentAsTool 集成（2 周）
- [ ] TeammateAgent 封装为 Eino ChatModelAgent
- [ ] 使用 adk.NewAgentTool 封装 Teammate
- [ ] Leader Agent 工具集动态注册
- [ ] SessionValues 共享状态实现
- [ ] 可观测性集成

### Phase 3: 工具与 API（1 周）
- [ ] CreateTeammate 工具（Leader 创建 Teammate 并注册为 AgentAsTool）
- [ ] AssignTask 工具（Leader 分配任务到 TaskStore）
- [ ] CollectResults 工具（Leader 收集 Teammate 执行结果）
- [ ] Teammate 状态查询 API

### Phase 4: 兼容与迁移（1 周）
- [ ] Subagent 兼容层
- [ ] 迁移工具
- [ ] 文档更新
- [ ] 示例代码

### Phase 5: 优化与测试（2 周）
- [ ] 性能优化
- [ ] 并发压力测试
- [ ] 端到端测试
- [ ] 用户验收测试

## 13. 参考资料

- [Qoder Agent Teams 文档](https://docs.qoder.com/zh/cli/agent-teams)
- [Eino ADK 多 Agent 协作](https://blog.csdn.net/peraglobal/article/details/165005201)
- [Eino AgentAsTool 模式](https://blog.csdn.net/liuhuangle/article/details/167388360)
- [CloudWeGo Eino GitHub](https://github.com/cloudwego/eino)
