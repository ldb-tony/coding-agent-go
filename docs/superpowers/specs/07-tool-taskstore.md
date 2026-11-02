# 07 - 任务系统工具规格

## 1. 职责

任务系统工具提供持久化任务图管理和子代理委派能力：

1. **任务 CRUD** — 创建、读取、更新、删除（标记）持久化任务
2. **依赖建图** — 两阶段构建 DAG：先创建节点，再设置依赖边
3. **状态机管理** — 维护 pending → in_progress → completed/deleted 生命周期
4. **认领与完成** — 支持多 Agent 认领任务、校验归属后标记完成
5. **环检测** — 添加依赖边时通过 DFS 检测并阻止环形依赖
6. **子代理委派** — 将独立子任务委派给嵌套 Agent Loop 执行

**与 todo_write 的区别**:
- `todo_write` 是会话内清单（由 eino plantask middleware 管理），整体替换，随会话结束消失
- 任务图（taskStore）是持久化 DAG，每个任务独立文件，跨会话可恢复，支持依赖关系和多 Agent 协作

## 2. 文件位置

```
pkg/taskstore/
└── taskstore.go       # taskStore 结构体、所有 CRUD 方法、环检测

pkg/tool/agent/
└── task.go            # 7 个任务工具 + 1 个子代理工具的 eino 适配层
```

运行时数据目录:
```
.tasks/
├── task_a1b2c3d4.json
├── task_e5f6a7b8.json
└── task_c9d0e1f2.json
```

## 3. 依赖

- `encoding/json` — 任务序列化/反序列化
- `os` / `path/filepath` / `io/fs` — 文件系统操作
- `crypto/rand` — 随机 ID 生成
- `fmt` — 格式化
- `strings` — 字符串处理
- `sort` — 按 ID 排序
- `errors` — 错误构造
- `github.com/cloudwego/eino/components/tool` — eino 工具接口
- `github.com/cloudwego/eino/components/tool/utils` — `utils.InferTool` 泛型构建器

## 4. 数据结构

### 4.1 taskStatus

```go
type taskStatus string

const (
    taskStatusPending    taskStatus = "pending"
    taskStatusInProgress taskStatus = "in_progress"
    taskStatusCompleted  taskStatus = "completed"
)
```

**注**: 删除操作不引入新状态，而是将任务文件从 `.tasks/` 目录移除（硬删除）或标记为 deleted 状态（软删除）。本规格采用软删除方案，新增 `taskStatusDeleted` 枚举值。

```go
const (
    taskStatusDeleted taskStatus = "deleted" // 软删除标记
)
```

### 4.2 Task

```go
type Task struct {
    ID          string     `json:"id"`
    Subject     string     `json:"subject"`
    Description string     `json:"description"`
    Status      taskStatus `json:"status"`
    Owner       *string    `json:"owner"`       // 未认领时为 null
    BlockedBy   []string   `json:"blockedBy"`   // 本任务依赖的任务 ID 列表
    Blocks      []string   `json:"blocks"`      // 依赖本任务的任务 ID 列表（反向边）
    ActiveForm  string     `json:"activeForm"`  // 进行时展示文案（可选）
}
```

字段说明:
- `ID`: 格式为 `"task_"` + 8 位随机十六进制，如 `"task_a1b2c3d4"`
- `Subject`: 任务标题，不可为空
- `Description`: 任务详细描述，可为空
- `Status`: 四态枚举 — pending / in_progress / completed / deleted
- `Owner`: 认领者标识，nil 表示未认领，默认值为 `"agent"`
- `BlockedBy`: 本任务依赖的任务 ID 列表；全部 completed 后任务才能 claim
- `Blocks`: 依赖本任务的任务 ID 列表（反向边，用于完成时通知解锁）
- `ActiveForm`: 可选的进行时展示文案，如 "Implementing auth module"

### 4.3 taskStore

```go
type taskStore struct {
    dir string // 任务目录路径，如 ".tasks"
}
```

taskStore 自身无内存状态（与 memoryStore 同款设计），全部状态都在文件中。

### 4.4 JSON 序列化格式

每个任务文件内容格式（缩进 2 空格 + 尾部换行）:

```json
{
  "id": "task_a1b2c3d4",
  "subject": "Implement auth module",
  "description": "Write the authentication middleware",
  "status": "pending",
  "owner": null,
  "blockedBy": ["task_e5f6a7b8"],
  "blocks": ["task_c9d0e1f2"],
  "activeForm": "Implementing auth module"
}
```

## 5. 关键常量

```go
const (
    tasksDirName     = ".tasks"    // 任务目录名
    defaultTaskOwner = "agent"     // 默认认领者
)

// ID 格式: "task_" + 8 位随机十六进制
// ID 总长度: len("task_") + 8 = 13
// ID 生成重试上限: 8 次
```

## 6. 工具清单

### 6.1 任务系统 7 个工具

| # | 工具名 | 功能 | eino 参数结构体 |
|---|--------|------|----------------|
| 1 | `create_task` | 创建持久化任务 | CreateTaskArgs{Subject, Description} |
| 2 | `get_task` | 获取任务详情 | GetTaskArgs{ID} |
| 3 | `list_tasks` | 列出所有任务 | ListTasksArgs{} (无参数) |
| 4 | `update_task` | 更新任务属性 | UpdateTaskArgs{ID, Status, Subject, Description, Owner} |
| 5 | `delete_task` | 标记删除任务 | DeleteTaskArgs{ID} |
| 6 | `add_dependency` | 添加依赖边 | AddDependencyArgs{TaskID, AddBlockedBy, AddBlocks} |
| 7 | `task` | 委派子代理 | TaskArgs{Prompt} |

### 6.2 create_task

```go
type CreateTaskArgs struct {
    Subject     string `json:"subject" jsonschema:"required" jsonschema_description:"Task subject"`
    Description string `json:"description" jsonschema:"required" jsonschema_description:"Task description"`
    ActiveForm  string `json:"activeForm,omitempty" jsonschema_description:"Present continuous form shown in spinner"`
}
```

返回值: 完整任务 JSON（含运行时生成的 ID）

### 6.3 get_task

```go
type GetTaskArgs struct {
    ID string `json:"id" jsonschema:"required" jsonschema_description:"Task ID"`
}
```

返回值: 完整任务 JSON（含 description、blockedBy、blocks 等全部字段）

### 6.4 list_tasks

```go
type ListTasksArgs struct{} // 无参数
```

返回值: 每条任务一行摘要，格式:
```
{ID} [{status}] {subject} (owner: {owner}, blocked by: {dep1}, {dep2})
```
空库时返回 `"(no tasks)"`。deleted 状态的任务不出现在列表中。

### 6.5 update_task

```go
type UpdateTaskArgs struct {
    ID          string `json:"id" jsonschema:"required" jsonschema_description:"Task ID"`
    Status      string `json:"status,omitempty" jsonschema_description:"New status (pending/in_progress/completed)"`
    Subject     string `json:"subject,omitempty" jsonschema_description:"New subject"`
    Description string `json:"description,omitempty" jsonschema_description:"New description"`
    ActiveForm  string `json:"activeForm,omitempty" jsonschema_description:"New activeForm"`
    Owner       string `json:"owner,omitempty" jsonschema_description:"New owner"`
}
```

返回值: 更新后的完整任务 JSON

约束:
- status 变更必须遵循状态机规则
- subject/description/activeForm/owner 可在 pending 或 in_progress 状态下修改
- completed/deleted 状态不可修改

### 6.6 delete_task

```go
type DeleteTaskArgs struct {
    ID string `json:"id" jsonschema:"required" jsonschema_description:"Task ID to delete"`
}
```

返回值: 确认消息

行为: 软删除，将 status 设为 `deleted`。已删除的任务不出现在 list_tasks 结果中，其他任务不可依赖已删除的任务。

### 6.7 add_dependency

```go
type AddDependencyArgs struct {
    TaskID       string   `json:"task_id" jsonschema:"required" jsonschema_description:"Task ID to add dependencies to"`
    AddBlockedBy []string `json:"addBlockedBy,omitempty" jsonschema_description:"Task IDs that this task depends on"`
    AddBlocks    []string `json:"addBlocks,omitempty" jsonschema_description:"Task IDs that depend on this task"`
}
```

返回值: 更新后的完整任务 JSON

约束:
- 目标任务必须是 pending 状态且未被认领
- 不允许自依赖
- 不允许成环（DFS 检测）
- 重复边安全跳过
- 所有引用的任务 ID 必须存在且未被删除

### 6.8 task（子代理委派）

```go
type TaskArgs struct {
    Prompt string `json:"prompt" jsonschema:"required" jsonschema_description:"Task prompt for subagent"`
}
```

返回值: 子代理的最终文本摘要

## 7. 核心函数

### 7.1 newTaskStore()

```
函数签名: func newTaskStore(dir string) *taskStore

流程:
1. 创建 taskStore{dir: dir}
2. 返回指针

参数:
- dir: 任务目录路径，通常为 filepath.Join(workdir, ".tasks")
```

### 7.2 create()

```
函数签名: func (s *taskStore) create(subject, description, activeForm string) (*Task, error)

流程:
1. 校验 subject 非空（TrimSpace 后）
   - 为空 → 返回 nil, error("subject is required")

2. 创建 .tasks/ 目录（如不存在）
   - os.MkdirAll(s.dir, 0o755)
   - 失败 → 返回 nil, error

3. 生成随机 ID（最多重试 8 次）:
   for attempt := 0; attempt < 8; attempt++:
     a. id, err := newTaskID()
     b. 构建 Task 对象:
        task := &Task{
          ID:          id,
          Subject:     subject,
          Description: description,
          Status:      taskStatusPending,
          Owner:       nil,
          BlockedBy:   []string{},
          Blocks:      []string{},
          ActiveForm:  activeForm,
        }
     c. JSON 序列化（缩进 2 空格）+ 尾部换行
     d. 以 O_EXCL 独占创建文件:
        path = s.dir + "/" + id + ".json"
        os.OpenFile(path, O_CREATE|O_EXCL|O_WRONLY, 0o644)
        - 文件已存在 (fs.ErrExist) → continue（ID 撞号，重试）
        - 其他错误 → 返回 nil, error
        - 成功 → 写入数据，关闭文件
     e. 返回 task, nil

4. 8 次重试全部撞号 → 返回 nil, error("could not allocate a unique task id")
```

### 7.3 load()

```
函数签名: func (s *taskStore) load(id string) (*Task, error)

流程:
1. 校验 ID 格式
   - validateTaskID(id) 失败 → 返回 nil, error

2. 构建文件路径
   - path = filepath.Join(s.dir, id+".json")

3. 读取文件
   - os.ReadFile(path)
   - 文件不存在 (fs.ErrNotExist) → 返回 nil, fmt.Errorf("task %s not found", id)
   - 其他错误 → 返回 nil, error

4. JSON 反序列化
   - json.Unmarshal(data, &task)
   - 解析失败 → 返回 nil, fmt.Errorf("task %s is corrupted: %w", id, err)

5. nil 字段保护
   - task.BlockedBy == nil → task.BlockedBy = []string{}
   - task.Blocks == nil → task.Blocks = []string{}

6. 返回 &task, nil
```

### 7.4 save()

```
函数签名: func (s *taskStore) save(t *Task) error

流程:
1. 校验 ID 格式
   - validateTaskID(t.ID) 失败 → 返回 error

2. JSON 序列化（缩进 2 空格 + 尾部换行）
   - data, _ := json.MarshalIndent(t, "", "  ")
   - data = append(data, '\n')

3. 原子写入（走 atomicWriteFile，避免半截状态）
   - path = filepath.Join(s.dir, t.ID+".json")
   - atomicWriteFile(path, data, 0o644)
   - 失败 → 返回 error

4. 返回 nil
```

### 7.5 list()

```
函数签名: func (s *taskStore) list() []*Task

流程:
1. 读取 s.dir 目录下所有条目
   - 目录不存在 → 返回 nil（空任务库是合法状态）
   - 读取失败 → 返回 nil

2. 过滤并加载:
   - 跳过子目录
   - 只处理 .json 后缀文件
   - 提取 ID: strings.TrimSuffix(filename, ".json")
   - 调用 load(id)
   - 加载失败 → 跳过（损坏文件不阻塞其余任务）

3. 过滤 deleted 状态的任务（不返回给调用方）

4. 按 ID 升序排序
   - sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })

5. 返回 []*Task
```

### 7.6 deleteTask()

```
函数签名: func (s *taskStore) deleteTask(id string) (*Task, error)

流程:
1. 加载任务
   - load(id) 失败 → 返回 nil, error

2. 校验状态
   - status == completed → 返回 nil, error("cannot delete completed task")
   - status == deleted → 返回 nil, error("task already deleted")

3. 设置状态为 deleted
   - task.Status = taskStatusDeleted

4. 清理反向边
   - 遍历 task.BlockedBy 中的每个依赖 depID:
     a. 加载 dep 任务
     b. 从 dep.Blocks 中移除 task.ID
     c. 保存 dep 任务
   - 遍历 task.Blocks 中的每个下游 downID:
     a. 加载 down 任务
     b. 从 down.BlockedBy 中移除 task.ID
     c. 保存 down 任务

5. 保存任务
   - save(task)

6. 返回 task, nil
```

### 7.7 updateDependencies()

```
函数签名: func (s *taskStore) updateDependencies(taskID string, addBlockedBy, addBlocks []string) (*Task, error)

流程:
1. 加载目标任务
   - load(taskID) 失败 → 返回 nil, error

2. 前置校验:
   - task.Status != taskStatusPending → 返回 nil, error("task is {status}, cannot update dependencies")
   - task.Owner != nil → 返回 nil, error("task is owned by {owner}, cannot update dependencies")

3. 处理 addBlockedBy（本任务依赖的其他任务）:
   - 构建 seen map（已有 BlockedBy + 本次新增，去重）
   - 对每个 dep in addBlockedBy:
     a. dep == taskID → 返回 nil, error("task cannot depend on itself")
     b. seen[dep] == true → 跳过（重复边）
     c. load(dep) 失败 → 返回 nil, error("dependency {dep}: task not found")
     d. dep.Status == deleted → 返回 nil, error("dependency {dep}: task deleted")
     e. seen[dep] = true, 加入 added 列表

4. 处理 addBlocks（依赖本任务的其他任务）:
   - 对每个 down in addBlocks:
     a. down == taskID → 返回 nil, error("task cannot depend on itself")
     b. load(down) 失败 → 返回 nil, error
     c. down.Status == deleted → 返回 nil, error
     d. 将 taskID 加入 down.BlockedBy（去重）
     e. 保存 down 任务

5. 环检测:
   - 将 added 追加到 task.BlockedBy
   - 对每个 dep in added:
     if s.dependsOn(dep, taskID) →
       回滚 task.BlockedBy（移除刚追加的 added）
       返回 nil, error("adding {dep} would create a dependency cycle through task {taskID}")

6. 保存任务
   - save(task)

7. 返回 task, nil
```

### 7.8 claim()

```
函数签名: func (s *taskStore) claim(taskID, owner string) (string, error)

流程:
1. owner 为空时取默认值:
   - strings.TrimSpace(owner) == "" → owner = defaultTaskOwner ("agent")

2. 加载任务
   - load(taskID) 失败 → 返回 "", error

3. 状态校验:
   - task.Status != taskStatusPending →
     返回 "Task {id} is {status}, cannot claim", nil（注意：返回文本而非 error）

4. 依赖校验:
   - deps := s.incompleteDependencies(task)
   - len(deps) > 0 →
     返回 "Blocked by: {dep1}, {dep2}", nil

5. 设置认领:
   - o := owner
   - task.Owner = &o
   - task.Status = taskStatusInProgress

6. 保存
   - save(task) 失败 → 返回 "", error

7. 返回 "Claimed {id} ({subject})", nil
```

### 7.9 complete()

```
函数签名: func (s *taskStore) complete(taskID, owner string) (string, error)

流程:
1. owner 为空时取默认值:
   - strings.TrimSpace(owner) == "" → owner = defaultTaskOwner

2. 加载任务
   - load(taskID) 失败 → 返回 "", error

3. 状态校验:
   - task.Status != taskStatusInProgress →
     返回 "Task {id} is {status}, cannot complete", nil

4. Owner 归属校验:
   - task.Owner == nil || *task.Owner != owner →
     返回 "Task {id} is owned by {taskOwner}, not {owner}", nil

5. 快照当前可开始的 pending 任务（用于计算"新解锁"的任务）:
   readyBefore := map[string]bool{}
   for _, other := range s.list():
     if other.Status == pending && len(other.BlockedBy) > 0 && s.canStart(other.ID):
       readyBefore[other.ID] = true

6. 设置完成状态:
   - task.Status = taskStatusCompleted

7. 保存
   - save(task) 失败 → 返回 "", error

8. 计算新解锁的任务:
   var unblocked []string
   for _, other := range s.list():
     if other.Status == pending && len(other.BlockedBy) > 0 &&
        !readyBefore[other.ID] && s.canStart(other.ID):
       unblocked = append(unblocked, other.Subject)

9. 构建返回消息:
   msg = "Completed {id} ({subject})"
   if len(unblocked) > 0:
     msg += "\nUnblocked: {subj1}, {subj2}"

10. 返回 msg, nil
```

### 7.10 incompleteDependencies()

```
函数签名: func (s *taskStore) incompleteDependencies(t *Task) []string

流程:
1. 遍历 t.BlockedBy 中的每个 dep:
   a. d, err := s.load(dep)
   b. err != nil（文件丢失）→ 视为未完成，加入 out
   c. d.Status != taskStatusCompleted → 加入 out

2. 返回 out
```

### 7.11 canStart()

```
函数签名: func (s *taskStore) canStart(id string) bool

流程:
1. t, err := s.load(id)
   - err != nil → return false
2. return len(s.incompleteDependencies(t)) == 0
```

### 7.12 dependsOn() — 递归依赖检测

```
函数签名: func (s *taskStore) dependsOn(from, target string) bool

流程:
1. t, err := s.load(from)
   - err != nil → return false
2. 遍历 t.BlockedBy 中的每个 dep:
   - dep == target → return true
   - s.dependsOn(dep, target) → return true（递归）
3. return false

终止性保证: 图中已被拒绝成环，因此递归深度有限。
```

### 7.13 validateTaskID()

```
函数签名: func validateTaskID(id string) error

规则:
1. 长度必须恰好为 13: len("task_") + 8 = 5 + 8 = 13
2. 前 5 个字符必须是 "task_"
3. 后 8 个字符必须全部是十六进制小写: [0-9a-f]

失败时返回:
  fmt.Errorf("invalid task id %q (want task_ + 8 hex chars)", id)

设计目的: ID 直接拼进文件路径，宽松的格式会让 "../x" 之类的输入逃出 .tasks/ 目录。
```

### 7.14 newTaskID()

```
函数签名: func newTaskID() (string, error)

流程:
1. 生成 4 字节随机数:
   b := make([]byte, 4)
   rand.Read(b)
2. 格式化为十六进制字符串:
   return fmt.Sprintf("task_%x", b), nil

结果示例: "task_a1b2c3d4"
```

## 8. 子代理工具 (task)

### 8.1 概述

`task` 工具将独立子任务委派给一个嵌套的 Agent Loop（子代理）执行。子代理与父同进程、同 WORKDIR，复用同一套工具和权限系统，但拥有全新的 messages[]。

### 8.2 关键常量

```go
const (
    subagentMaxTurns  = 30   // 子代理循环轮数上限
    subagentMaxTokens = 8000 // 子代理每次生成的 max_tokens
    taskToolName      = "task"
)
```

### 8.3 子代理 system prompt

```go
const subSystemPrompt = "You are a subagent working on a single delegated task. " +
    "Use the provided tools to complete it, then reply with a concise final summary " +
    "of what you found or did. Act, don't explain."
```

### 8.4 runSubagent()

```
函数签名: func runSubagent(st *sessionState, prompt string) string

流程:
1. 创建子代理 LLM 客户端:
   - 如果 st.config.Client != nil（测试场景）→ 复用
   - 否则 → NewLLMClient(st.config)

2. 初始化消息历史:
   messages := [anthropic.NewUserMessage(prompt)]

3. 循环执行（最多 subagentMaxTurns = 30 轮）:
   for turn := 0; turn < 30; turn++:
   
   a. 计算子代理工具池:
      tools := subTools(st)  // 与父相同的工具池，但排除 task 工具
   
   b. 调用 LLM:
      resp, err := subClient.Send(st.ctx, subSystemPrompt, messages, tools, subagentMaxTokens)
      - 失败 → 返回 "Error: subagent failed: {err}"
   
   c. 追加助手响应到 messages
   
   d. 执行工具批次:
      results := executeToolBatch(st.ctx, st, resp.Content, preHook, postHook)
      - preHook: PreToolUse hook（与父相同的权限检查）
      - postHook: PostToolUse hook
   
   e. 无工具调用 → 本轮为最终答复:
      return subagentText(resp.Content)
   
   f. 有工具调用 → 追加结果到 messages，继续下一轮

4. 30 轮耗尽 → 返回 "Subagent stopped after 30 turns without a final answer."
```

### 8.5 subTools() — 子代理工具池

```
函数签名: func subTools(st *sessionState) []tool.InvokableTool

流程:
1. all := assembleToolPool(st)  // 获取完整工具池
2. 遍历 all，跳过 name == "task" 的工具
3. 返回过滤后的工具列表

设计目的: 只允许一层委派，子代理不能再委派子代理。
```

### 8.6 subagentText()

```
函数签名: func subagentText(content []anthropic.ContentBlockUnion) string

流程:
1. 拼接响应里全部 type=="text" 的 block
2. TrimSpace 后非空 → 返回拼接文本
3. 为空 → 返回 "(no summary)"
```

### 8.7 taskHandler()

```
函数签名: func taskHandler(st *sessionState, args json.RawMessage) string

流程:
1. 解析参数:
   var in struct { Prompt string `json:"prompt"` }
   json.Unmarshal(args, &in)
   - 失败 → "Error: invalid arguments: {err}"

2. 校验 prompt 非空:
   - strings.TrimSpace(in.Prompt) == "" → "Error: prompt is required"

3. 检查运行环境:
   - st.client == nil || st.ctx == nil → "Error: subagent is unavailable outside the agent loop"

4. 调用子代理:
   return runSubagent(st, in.Prompt)
```

## 9. 状态机

```
         create()
           │
           ▼
      ┌─────────┐
      │ pending  │◄──────────────────┐
      └────┬─────┘                   │
           │                         │
           │ claim()                 │ (特殊情况: owner 清空后回退)
           │ (blockedBy 全部         │
           │  completed)             │
           ▼                         │
      ┌─────────────┐               │
      │ in_progress  │───────────────┘
      └────┬────────┘
           │
           ├──── complete() ──────► ┌───────────┐
           │      (owner 匹配)     │ completed  │ (终态)
           │                       └───────────┘
           │
           └──── delete() ────────► ┌───────────┐
                                    │ deleted   │ (终态)
                                    └───────────┘
```

### 9.1 状态转换规则

| 当前状态 | 允许的操作 | 目标状态 |
|---------|-----------|---------|
| pending | claim() | in_progress |
| pending | delete() | deleted |
| pending | update_task() 修改属性 | pending（不变） |
| pending | add_dependency() | pending（不变） |
| in_progress | complete() | completed |
| in_progress | delete() | deleted |
| in_progress | update_task() 修改属性 | in_progress（不变） |
| completed | （无，终态） | — |
| deleted | （无，终态） | — |

### 9.2 依赖规则

1. **blockedBy 非空不能 claim**: 所有 blockedBy 中的任务必须处于 completed 状态
2. **文件丢失视为未完成**: 依赖的任务文件不存在时，视为该依赖未完成
3. **deleted 任务不可被依赖**: add_dependency 时引用的任务不能是 deleted 状态
4. **completed 任务不能添加依赖**: 只有 pending 且未认领的任务可以修改依赖
5. **不允许自依赖**: taskID 不能出现在自己的 BlockedBy 中
6. **不允许成环**: 添加依赖边后通过 DFS 检测，存在环则拒绝
7. **重复边安全**: 同一条依赖边添加多次不会产生错误

## 10. 两阶段建图

### 10.1 设计原因

同一响应里的兄弟工具调用在结果产生前就已定型，无法引用彼此新生成的 ID。因此建图分两阶段:

### 10.2 阶段一: 创建所有节点

```
模型在同一批次中调用:
  create_task(subject="Set up database")  → 返回 task_aaaaaaaa
  create_task(subject="Write API layer")  → 返回 task_bbbbbbbb
  create_task(subject="Add tests")        → 返回 task_cccccccc
```

此时所有任务状态为 pending，BlockedBy 和 Blocks 均为空列表。

### 10.3 阶段二: 设置依赖边

```
模型在下一批次中调用:
  add_dependency(task_id="task_bbbbbbbb", addBlockedBy=["task_aaaaaaaa"])
  add_dependency(task_id="task_cccccccc", addBlockedBy=["task_bbbbbbbb"])
```

结果: A → B → C 的依赖链建立。

### 10.4 执行顺序

```
1. claim_task(task_id="task_aaaaaaaa")  → in_progress
2. ... 执行工作 ...
3. complete_task(task_id="task_aaaaaaaa")  → completed, 解锁 B
4. claim_task(task_id="task_bbbbbbbb")  → in_progress
5. ... 执行工作 ...
6. complete_task(task_id="task_bbbbbbbb")  → completed, 解锁 C
7. claim_task(task_id="task_cccccccc")  → in_progress
8. ... 执行工作 ...
9. complete_task(task_id="task_cccccccc")  → completed
```

## 11. 算法细节

### 11.1 环检测算法 (dependsOn)

```
func dependsOn(from, target string) bool:
  1. 加载 from 任务
  2. 遍历 from.BlockedBy 中的每个 dep:
     - dep == target → return true（直接依赖）
     - dependsOn(dep, target) → return true（传递依赖）
  3. return false

终止性: 因为图中已被拒绝成环（每次 add_dependency 都做检测），
所以递归深度有限，不会无限循环。

时间复杂度: O(V + E)，V 为任务数，E 为依赖边数。
```

### 11.2 新解锁任务计算

```
complete() 中计算"因本次完成而新解锁的任务":

1. 快照阶段: 记录完成前已可开始的 pending 任务
   readyBefore = {}
   for each other in list():
     if other.status == pending && len(other.blockedBy) > 0 && canStart(other.id):
       readyBefore[other.id] = true

2. 完成当前任务

3. 扫描阶段: 找出新解锁的任务
   unblocked = []
   for each other in list():
     if other.status == pending && len(other.blockedBy) > 0
        && !readyBefore[other.id] && canStart(other.id):
       unblocked.append(other.subject)

设计目的: "刚解锁"以完成前的快照为准——完成前就已可开始的 pending 任务不会重复上报。
```

### 11.3 ID 校验

```
func validateTaskID(id string) error:
  const prefix = "task_"
  const hexLen = 8
  
  1. len(id) != len(prefix) + hexLen (13) → error
  2. id[:5] != "task_" → error
  3. for c in id[5:]:
     if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')):
       → error
  4. return nil
```

## 12. 与 eino plantask middleware 的关系

### 12.1 职责划分

| 维度 | plantask middleware (todo_write) | taskStore (任务图) |
|------|--------------------------------|-------------------|
| 生命周期 | 会话内 | 跨会话持久化 |
| 存储 | sessionState 内存 | .tasks/*.json 文件 |
| 数据结构 | 扁平列表 | DAG 有向无环图 |
| 依赖关系 | 无 | blockedBy / blocks |
| 多 Agent | 不支持 | 支持（owner 认领机制） |
| 工具名 | todo_write | create_task, get_task, list_tasks, update_task, delete_task, add_dependency |
| 状态模型 | 整体替换 | 单任务状态机 |
| 适用场景 | 短期工作清单 | 复杂多步骤任务编排 |

### 12.2 共存方式

两者独立运作，共享同一个 sessionState:
- `sessionState.todo` → todoManager（plantask middleware 管理）
- `sessionState.tasks` → taskStore（任务图系统管理）

模型可以同时使用 todo_write 管理短期清单和 create_task 构建长期任务图。

## 13. 错误处理

### 13.1 create_task

| 错误场景 | 处理方式 | 返回格式 |
|---------|---------|---------|
| subject 为空 | 拒绝创建 | "Error: subject is required" |
| 目录创建失败 | 返回 OS 错误 | "Error: {err}" |
| ID 生成 8 次撞号 | 拒绝创建 | "Error: could not allocate a unique task id" |
| 文件写入失败 | 返回 OS 错误 | "Error: {err}" |

### 13.2 get_task

| 错误场景 | 处理方式 | 返回格式 |
|---------|---------|---------|
| ID 格式非法 | 校验拒绝 | "Error: invalid task id {id}" |
| 任务不存在 | 返回错误 | "Error: task {id} not found" |
| 文件损坏 | JSON 解析失败 | "Error: task {id} is corrupted: {err}" |

### 13.3 update_task

| 错误场景 | 处理方式 | 返回格式 |
|---------|---------|---------|
| 任务不存在 | 返回错误 | "Error: task {id} not found" |
| 终态修改 | 拒绝 | "Error: task {id} is {status}, cannot modify" |
| 非法状态值 | 拒绝 | "Error: invalid status {status}" |

### 13.4 delete_task

| 错误场景 | 处理方式 | 返回格式 |
|---------|---------|---------|
| 任务不存在 | 返回错误 | "Error: task {id} not found" |
| 已完成任务 | 拒绝 | "Error: cannot delete completed task" |
| 已删除任务 | 拒绝 | "Error: task already deleted" |

### 13.5 add_dependency

| 错误场景 | 处理方式 | 返回格式 |
|---------|---------|---------|
| 目标任务非 pending | 拒绝 | "Error: task {id} is {status}, cannot update dependencies" |
| 目标任务已认领 | 拒绝 | "Error ... owned by {owner}, cannot update dependencies" |
| 自依赖 | 拒绝 | "Error: task cannot depend on itself" |
| 依赖任务不存在 | 拒绝 | "Error: dependency {dep}: task not found" |
| 依赖任务已删除 | 拒绝 | "Error {dep}: task deleted" |
| 成环 | 拒绝并回滚 | "ErrorError: adding {dep} would create a dependency cycle" |

### 13.6 claim

| 错误场景 | 处理方式 | 返回格式 |
|---------|---------|---------|
| 任务非 pending | 返回文本（非 error） | "Task {id} is {status}, cannot claim" |
| 依赖未完成 | 返回文本（非 error） | "Blocked by: {dep1}, {dep2}" |
| 保存失败 | 返回 error | "Error: {err}" |

### 13.7 complete

| 错误场景 | 处理方式 | 返回格式 |
|---------|---------|---------|
| 任务非 in_progress | 返回文本（非 error） | "Task {id} is {status}, cannot complete" |
| Owner 不匹配 | 返回文本（非 error） | "Task {id} is owned by {owner}, not {caller}" |
| 保存失败 | 返回 error | "Error: {err}" |

### 13.8 task（子代理）

| 错误场景 | 处理方式 | 返回格式 |
|---------|---------|---------|
| prompt 为空 | 拒绝执行 | "Error: prompt is required" |
| 不在 Agent Loop 中 | 拒绝执行 | "Error: subagent is unavailable outside the agent loop" |
| LLM 调用失败 | 返回错误 | "Error: subagent failed: {err}" |
| 30 轮耗尽 | 返回超时提示 | "Subagent stopped after 30 turns without a final answer." |

## 14. 并发安全

- taskStore 自身无内存状态，所有操作都是文件级的
- 文件写入走 atomicWriteFile（临时文件 + rename），不会出现半截状态
- create() 使用 O_EXCL 独占创建，ID 撞号时重试
- 多个 Agent 并发 claim 同一任务时，依赖文件级原子操作保证一致性
- 建议: 由 Agent 核心串行化调用同一任务的操作，避免 TOCTOU 竞态

## 15. 测试要点

### 15.1 单元测试

1. **validateTaskID**:
   - 合法 ID: "task_a1b2c3d4" → nil
   - 长度不对: "task_a1b2" → error
   - 前缀不对: "tsk_a1b2c3d4" → error
   - 非法字符: "task_A1B2C3D4" → error（大写）
   - 空字符串 → error

2. **newTaskID**:
   - 生成 100 个 ID，验证全部合法
   - 验证无重复（统计唯一性）

3. **create**:
   - 正常创建 → 文件存在，JSON 格式正确，BlockedBy/Blocks 为空列表
   - subject 为空 → error
   - 目录不存在 → 自动创建
   - activeForm 正确保存

4. **load**:
   - 正常加载 → Task 对象字段正确
   - 文件不存在 → "task {id} not found"
   - ID 格式非法 → error
   - JSON 损坏 → "corrupted" error
   - BlockedBy/Blocks 为 null 时初始化为空列表

5. **save**:
   - 正常保存 → 文件内容正确（原子写）
   - ID 格式非法 → error

6. **deleteTask**:
   - pending 任务删除 → status 变为 deleted
   - 反向边清理 → 相关任务的 BlockedBy/Blocks 正确更新
   - completed 任务 → 拒绝删除
   - 已删除任务 → 拒绝重复删除

7. **updateDependencies**:
   - 正常添加 blockedBy → 保存成功
   - 正常添加 blocks → 反向边正确建立
   - 自依赖 → error
   - 环形依赖 → error
   - 非 pending 状态加依赖 → error
   - 已认领任务加依赖 → error
   - 依赖不存在 → error
   - 依赖已删除 → error
   - 重复边 → 安全跳过

8. **claim**:
   - 正常认领 → status=in_progress, owner 设置
   - 有未完成的 blockedBy → 返回 "Blocked by: ..."
   - 非 pending 状态 → 返回 "Task is {status}"
   - blockedBy 文件丢失 → 视为未完成
   - 默认 owner → "agent"

9. **complete**:
   - 正常完成 → status=completed
   - owner 不匹配 → 返回 "owned by {owner}"
   - 非 in_progress 状态 → 返回 "is {status}"
   - 解锁通知 → 正确报告新解锁的任务
   - 快照机制 → 已可开始的任务不重复上报

10. **list**:
    - 空目录 → nil
    - 多任务 → 按 ID 排序
    - 目录不存在 → nil
    - deleted 任务不出现在结果中
    - 损坏文件被跳过

### 15.2 子代理测试

1. **taskHandler**:
   - prompt 为空 → "Error: prompt is required"
   - 不在 Agent Loop → "Error: subagent is unavailable"
   - 正常执行 → 返回子代理最终文本

2. **runSubagent**:
   - 子代理工具池不包含 task 工具
   - 30 轮限制生效
   - LLM 失败 → 返回错误消息
   - 无工具调用时返回文本

3. **subTools**:
   - 排除 task 工具
   - 包含所有其他工具（含 MCP 动态工具）

### 15.3 环检测测试

1. **无环**: A blockedBy [B], B blockedBy [C]，添加 C blockedBy [D] → 允许
2. **直接环**: A blockedBy [B]，添加 B blockedBy [A] → 拒绝
3. **间接环**: A blockedBy [B], B blockedBy [C]，添加 C blockedBy [A] → 拒绝
4. **自环**: A blockedBy [A] → 拒绝
5. **复杂图**: A blockedBy [B,C], B blockedBy [D], C blockedBy [D]，添加 D blockedBy [A] → 拒绝

### 15.4 集成测试

1. **完整生命周期**:
   - create → add_dependency → claim → complete
   - 验证每个阶段文件内容正确
   - 验证解锁通知正确

2. **多任务依赖链**:
   - 创建 A, B, C
   - B blockedBy A, C blockedBy B
   - claim A → complete A → claim B → complete B → claim C
   - 验证依赖检查正确
   - 验证 "Unblocked" 通知在正确时机触发

3. **两阶段建图**:
   - 同一批次 create 多个任务
   - 下一批次 add_dependency 建立关系
   - 验证 ID 引用正确

4. **删除清理**:
   - 创建 A blockedBy B
   - 删除 B → A 的 blockedBy 中 B 被移除
   - A 可以正常 claim

5. **并发安全**:
   - 同时创建多个任务，验证 ID 不冲突
   - 同时 claim 同一任务，只有一个成功
