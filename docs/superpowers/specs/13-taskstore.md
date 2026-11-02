# 13 - 持久化任务图规格

## 1. 职责

持久化任务图系统负责管理跨会话的 DAG 任务依赖关系:

1. **存储** — 每个任务独立持久化为 `.tasks/{id}.json` 文件，跨会话可恢复
2. **状态管理** — 维护 pending / in_progress / completed 三态生命周期，严格校验状态转换合法性
3. **依赖建图** — 两阶段建图: 先 create 创建全部节点（拿到运行时生成的 ID），再 update 加边建立依赖关系
4. **认领与完成** — 支持多 Agent 认领任务、校验依赖完成后标记完成
5. **环检测** — 在添加依赖边时通过 DFS 递归检测并阻止环形依赖

**与 07-tool-taskstore.md 的关系**:
- 本模块（taskstore）是底层存储引擎，负责任务数据的持久化、状态机校验、依赖图维护
- 07-tool-taskstore.md 描述的是暴露给 LLM 的 6 个工具接口（create_task / update_task / claim_task / complete_task / get_task / list_tasks），这些工具调用本模块的核心操作
- 工具层负责参数解析和结果格式化，本模块负责业务逻辑和数据完整性

**与 todo_write 的区别**:
- todo_write 是会话内清单，整体替换，随会话结束消失
- taskStore 是持久化任务图，每个任务独立文件，支持依赖关系，跨会话可恢复

## 2. 文件位置

```
pkg/taskstore/
└── taskstore.go       # taskStore 结构体、所有 CRUD 方法、环检测
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
- `os` / `path/filepath` — 文件系统操作
- `crypto/rand` — 随机 ID 生成
- `fmt` — ID 格式化与错误消息
- `strings` — 字符串处理
- `io/fs` — 目录遍历
- `sort` — 结果排序
- `errors` — 错误判断（errors.Is）
- 无第三方依赖

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

### 4.2 Task

```go
type Task struct {
    ID          string     `json:"id"`          // 格式: "task_" + 8 位随机十六进制
    Subject     string     `json:"subject"`      // 任务标题，不可为空
    Description string     `json:"description"`  // 任务详细描述，可为空
    Status      taskStatus `json:"status"`       // pending | in_progress | completed
    Owner       *string    `json:"owner"`        // 认领者，未认领时为 null
    BlockedBy   []string   `json:"blockedBy"`    // 依赖的任务 ID 列表；创建时为空列表
}
```

字段说明:
- `ID`: 格式为 `"task_"` + 8 位随机十六进制（如 `"task_a1b2c3d4"`），总长度固定为 13 字符
- `Subject`: 任务标题，创建时校验非空（TrimSpace 后），不可修改
- `Description`: 任务详细描述，可为空字符串
- `Status`: 三态枚举，状态转换受严格校验（见第 10 节状态机）
- `Owner`: 认领者指针，nil 表示未认领；claim 时设置，complete 时校验归属
- `BlockedBy`: 依赖的任务 ID 列表；建图分两阶段，创建时初始化为 `[]string{}`（非 nil）

### 4.3 taskStore

```go
type taskStore struct {
    dir string // 任务目录路径，如 ".tasks"
}
```

taskStore 是无状态存储引擎（类似 memoryStore），全部数据都在文件中，store 自身无内存缓存。每次操作直接读写文件。

## 5. 关键常量

```go
const (
    tasksDirName     = ".tasks"   // 任务目录名
    defaultTaskOwner = "agent"    // claim/complete 未显式给 owner 时的默认认领者
)

// ID 格式常量
// prefix: "task_" (5 字符)
// hexLen: 8 (8 位十六进制)
// 总长度: 13 字符
```

### 5.1 存储格式 — .tasks/{id}.json

每个任务文件内容格式（JSON 缩进 2 空格 + 尾部换行）:

```json
{
  "id": "task_a1b2c3d4",
  "subject": "Implement auth module",
  "description": "Write the authentication middleware",
  "status": "pending",
  "owner": null,
  "blockedBy": ["task_e5f6a7b8"]
}
```

JSON 序列化规则:
- 使用 `json.MarshalIndent(t, "", "  ")` 序列化，缩进 2 空格
- 序列化后追加 `\n` 尾部换行
- `Owner` 字段为 `*string` 类型，未认领时序列化为 `null`
- `BlockedBy` 字段初始化为 `[]string{}`（非 nil），序列化为 `[]`（非 null）
- 反序列化后需检查 `BlockedBy == nil` 的情况，自动设为 `[]string{}`

文件锁策略:
- 创建: 使用 `O_EXCL` 独占创建，保证不覆盖已有任务文件
- 更新: 使用 `atomicWriteFile`（写临时文件 → rename），避免半截状态
- 无显式文件锁（flock）: taskStore 设计为单进程使用，由上层 Agent 串行化调用

## 6. 核心函数

### 6.1 newTaskStore()

```
函数签名: func newTaskStore(dir string) *taskStore

流程:
1. 创建 taskStore{dir: dir}
2. 返回指针

参数:
- dir: 任务目录路径，通常为 ".tasks"
```

### 6.2 create()

```
函数签名: func (s *taskStore) create(subject, description string) (*Task, error)

流程:
1. 校验 subject:
   - subject = strings.TrimSpace(subject)
   - subject == "" → 返回 nil, error("subject is required")

2. 确保目录存在:
   - os.MkdirAll(s.dir, 0o755)
   - 失败 → 返回 nil, error

3. 生成随机 ID（最多重试 8 次）:
   for attempt := 0; attempt < 8; attempt++:
     a. id, err := newTaskID()
        - 失败 → 返回 nil, error
     b. 构建 Task 对象:
        t := &Task{
          ID:          id,
          Subject:     subject,
          Description: description,
          Status:      taskStatusPending,
          BlockedBy:   []string{},
        }
     c. JSON 序列化:
        data, _ := json.MarshalIndent(t, "", "  ")
        data = append(data, '\n')
     d. 独占创建文件:
        path = filepath.Join(s.dir, id+".json")
        f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
        - errors.Is(err, fs.ErrExist) → continue（ID 撞号，丢弃重来）
        - 其他 err → 返回 nil, error
     e. 写入文件:
        f.Write(data)
        - 失败 → 关闭文件，返回 nil, error
     f. 关闭文件:
        f.Close()
        - 失败 → 返回 nil, error
     g. 返回 t, nil

4. 8 次重试全部撞号 → 返回 nil, error("could not allocate a unique task id")
```

### 6.3 load()

```
函数签名: func (s *taskStore) load(id string) (*Task, error)

流程:
1. 校验 ID 格式:
   - validateTaskID(id) 失败 → 返回 nil, error

2. 构建文件路径:
   - path = filepath.Join(s.dir, id+".json")

3. 读取文件:
   - data, err := os.ReadFile(path)
   - errors.Is(err, fs.ErrNotExist) → 返回 nil, error("task {id} not found")
   - 其他 err → 返回 nil, error

4. JSON 反序列化:
   - var t Task
   - json.Unmarshal(data, &t)
   - 失败 → 返回 nil, error("task {id} is corrupted: {err}")

5. nil 安全处理:
   - if t.BlockedBy == nil → t.BlockedBy = []string{}

6. 返回 &t, nil
```

### 6.4 save()

```
函数签名: func (s *taskStore) save(t *Task) error

流程:
1. 校验 ID 格式:
   - validateTaskID(t.ID) 失败 → 返回 error

2. JSON 序列化:
   - data, err := json.MarshalIndent(t, "", "  ")
   - 失败 → 返回 error

3. 原子写入文件:
   - path = filepath.Join(s.dir, t.ID+".json")
   - atomicWriteFile(path, append(data, '\n'), 0o644)
   - 失败 → 返回 error

4. 返回 nil

atomicWriteFile 实现细节（写临时文件 → rename）:
1. 在同目录创建临时文件: os.CreateTemp(dir, ".tmp-*")
2. 写入全部数据: f.Write(data)
3. 同步到磁盘: f.Sync()
4. 关闭文件: f.Close()
5. 设置权限: os.Chmod(tmp, perm)
6. 原子重命名: os.Rename(tmp, abs)
7. 失败路径: defer os.Remove(tmp) 清理临时文件
```

### 6.5 list()

```
函数签名: func (s *taskStore) list() []*Task

流程:
1. 读取目录:
   - entries, err := os.ReadDir(s.dir)
   - err != nil → 返回 nil（目录不存在不是错误）

2. 过滤并加载:
   - 遍历 entries:
     * 跳过目录（e.IsDir()）
     * 跳过非 .json 文件
     * 提取 ID: strings.TrimSuffix(e.Name(), ".json")
     * 调用 s.load(id) 加载
     * 加载失败 → 跳过该文件（不阻塞其余任务）
     * 成功 → 追加到 tasks 切片

3. 排序:
   - sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
   - 按 ID 升序排序，保证顺序稳定

4. 返回 tasks
```

### 6.6 updateDependencies()

```
函数签名: func (s *taskStore) updateDependencies(taskID string, addBlockedBy []string) (*Task, error)

流程:
1. 加载目标任务:
   - t, err := s.load(taskID)
   - 失败 → 返回 nil, error

2. 校验目标状态:
   - t.Status != taskStatusPending → 返回 nil, error("task {id} is {status}, cannot update dependencies")
   - t.Owner != nil → 返回 nil, error("task {id} is owned by {owner}, cannot update dependencies")

3. 构建已有依赖集合（去重）:
   - seen := make(map[string]bool, len(t.BlockedBy))
   - for _, dep := range t.BlockedBy: seen[dep] = true

4. 逐条处理新增依赖:
   for _, dep := range addBlockedBy:
     a. 自依赖检查:
        - dep == t.ID → 返回 nil, error("task cannot depend on itself")
     b. 重复边检查:
        - seen[dep] → continue（安全跳过，不重复记录）
     c. 存在性检查:
        - _, err := s.load(dep)
        - err != nil → 返回 nil, error("dependency {dep}: {err}")
     d. 标记已见:
        - seen[dep] = true
        - added = append(added, dep)

5. 追加依赖边:
   - t.BlockedBy = append(t.BlockedBy, added...)

6. 环检测:
   for _, dep := range added:
     if s.dependsOn(dep, t.ID):
       返回 nil, error("adding {dep} would create a dependency cycle through task {t.ID}")

7. 保存:
   - s.save(t)
   - 失败 → 返回 nil, error

8. 返回 t, nil
```

### 6.7 claim()

```
函数签名: func (s *taskStore) claim(taskID, owner string) (string, error)

流程:
1. 处理默认 owner:
   - strings.TrimSpace(owner) == "" → owner = defaultTaskOwner ("agent")

2. 加载任务:
   - t, err := s.load(taskID)
   - 失败 → 返回 "", error

3. 校验状态:
   - t.Status != taskStatusPending
     → 返回 "Task {id} is {status}, cannot claim", nil（注意: 返回的是消息文本，不是 error）

4. 校验依赖:
   - deps := s.incompleteDependencies(t)
   - len(deps) > 0
     → 返回 "Blocked by: {dep1}, {dep2}, ...", nil

5. 设置认领:
   - o := owner
   - t.Owner = &o
   - t.Status = taskStatusInProgress

6. 保存:
   - s.save(t)
   - 失败 → 返回 "", error

7. 返回 "Claimed {id} ({subject})", nil
```

### 6.8 complete()

```
函数签名: func (s *taskStore) complete(taskID, owner string) (string, error)

流程:
1. 处理默认 owner:
   - strings.TrimSpace(owner) == "" → owner = defaultTaskOwner ("agent")

2. 加载任务:
   - t, err := s.load(taskID)
   - 失败 → 返回 "", error

3. 校验状态:
   - t.Status != taskStatusInProgress
     → 返回 "Task {id} is {status}, cannot complete", nil

4. 校验 Owner 归属:
   - t.Owner == nil || *t.Owner != owner
     → 返回 "Task {id} is owned by {ownerLabel}, not {caller}", nil
   - ownerLabel 由 completeTaskOwnerLabel(t.Owner) 计算:
     nil → "none"; 非 nil → *owner

5. 快照"完成前已就绪"的任务（用于去重报告）:
   - readyBefore := map[string]bool{}
   - for _, other := range s.list():
       if other.Status == taskStatusPending
          && len(other.BlockedBy) > 0
          && s.canStart(other.ID):
         readyBefore[other.ID] = true

6. 设置完成状态:
   - t.Status = taskStatusCompleted

7. 保存:
   - s.save(t)
   - 失败 → 返回 "", error

8. 计算新解锁的任务:
   - var unblocked []string
   - for _, other := range s.list():
       if other.Status == taskStatusPending
          && len(other.BlockedBy) > 0
          && !readyBefore[other.ID]    // 完成前还未就绪
          && s.canStart(other.ID):      // 现在已就绪
         unblocked = append(unblocked, other.Subject)

9. 构建返回消息:
   - msg = "Completed {id} ({subject})"
   - if len(unblocked) > 0:
       msg += "\nUnblocked: " + strings.Join(unblocked, ", ")

10. 返回 msg, nil
```

### 6.9 辅助函数

#### 6.9.1 incompleteDependencies()

```
函数签名: func (s *taskStore) incompleteDependencies(t *Task) []string

流程:
1. 遍历 t.BlockedBy 中的每个依赖 ID
2. 对每个 dep:
   a. d, err := s.load(dep)
   b. err != nil（文件不存在/损坏）→ 视为未完成，加入结果
   c. d.Status != taskStatusCompleted → 视为未完成，加入结果
3. 返回未完成的依赖 ID 列表
```

#### 6.9.2 canStart()

```
函数签名: func (s *taskStore) canStart(id string) bool

流程:
1. t, err := s.load(id)
   - err != nil → 返回 false
2. 返回 len(s.incompleteDependencies(t)) == 0
```

#### 6.9.3 dependsOn() — 递归依赖检查

```
函数签名: func (s *taskStore) dependsOn(from, target string) bool

目的: 递归判断 from 是否（传递地）依赖 target。
      图中已被拒绝成环，因此递归必然终止。

流程:
1. t, err := s.load(from)
   - err != nil → 返回 false
2. for _, dep := range t.BlockedBy:
     if dep == target → 返回 true
     if s.dependsOn(dep, target) → 返回 true
3. 返回 false
```

#### 6.9.4 validateTaskID()

```
函数签名: func validateTaskID(id string) error

规则:
1. 长度必须恰好为 13: len("task_") + 8 = 5 + 8 = 13
2. 前 5 个字符必须是 "task_"
3. 后 8 个字符必须全部是十六进制小写: [0-9a-f]

实现:
  const prefix, hexLen = "task_", 8
  if len(id) != len(prefix)+hexLen → error
  if !strings.HasPrefix(id, prefix) → error
  for _, c := range id[len(prefix):]:
    if !isHexDigit(byte(c)) → error
  return nil

安全考虑: ID 直接拼进文件路径（filepath.Join(s.dir, id+".json")），
宽松格式会让 "../x" 之类的输入逃出 .tasks/ 目录。
```

#### 6.9.5 newTaskID()

```
函数签名: func newTaskID() (string, error)

流程:
1. 生成 4 字节随机数:
   b := make([]byte, 4)
   rand.Read(b)
2. 格式化:
   return fmt.Sprintf("task_%x", b), nil

结果示例: "task_a1b2c3d4"
```

#### 6.9.6 taskJSON()

```
函数签名: func taskJSON(t *Task) string

用途: 渲染任务的完整 JSON（get_task / create_task / update_task 的返回值）

流程:
1. data, err := json.MarshalIndent(t, "", "  ")
2. err != nil → 返回 "Error: " + err.Error()
3. 返回 string(data)
```

## 7. 算法细节

### 7.1 环检测算法

环检测在 `updateDependencies()` 中执行，采用"添加后递归回溯"策略:

```
添加依赖边后的环检测:

输入: 任务 t，新增依赖列表 added
目标: 检查是否存在 added 中某个 dep 传递依赖回 t

算法:
  for _, dep := range added:
    if s.dependsOn(dep, t.ID):
      return error("cycle detected")

dependsOn(from, target) 是深度优先搜索:
  1. 加载 from 任务
  2. 遍历 from.BlockedBy 中的每个 dep:
     a. dep == target → 找到路径，返回 true
     b. dependsOn(dep, target) → 递归搜索
  3. 无路径 → 返回 false

终止性保证: 图中已被拒绝成环，所以每次添加新边前图都是 DAG，
            DFS 在 DAG 上必然终止。

示例:
  已有: A blockedBy [B], B blockedBy [C]
  尝试: 给 C 添加 blockedBy [A]
  检查: dependsOn(A, C)?
    → A blockedBy [B] → dependsOn(B, C)?
      → B blockedBy [C] → C == C → true
  结果: 检测到环，拒绝添加
```

### 7.2 两阶段建图

```
建图分两阶段的原因:

阶段 1 — 创建节点:
  调用 create() 创建所有任务节点
  每个任务获得运行时生成的 ID（task_ + 随机 hex）
  此时 BlockedBy 均为空列表

阶段 2 — 建立依赖边:
  调用 updateDependencies() 给任务添加 blockedBy 依赖
  同一响应里的兄弟工具调用在结果产生前就已定型，
  无法引用彼此新生成的 ID

为什么不在 create 时指定依赖:
  LLM 在同一个 tool_calls 响应中可能同时创建 A 和 B，
  并想让 B blockedBy A。但 A 的 ID 在 create 返回前不存在，
  LLM 无法预知。因此必须先全部 create，再 update 加边。
```

### 7.3 原子文件写入

```
atomicWriteFile 保证目标路径要么旧内容完整、要么新内容完整:

流程:
1. 在同目录创建临时文件: os.CreateTemp(dir, ".tmp-*")
2. 写入全部数据: f.Write(data)
3. fsync 同步: f.Sync()
4. 关闭文件: f.Close()
5. 设置权限: os.Chmod(tmp, perm)
6. 原子重命名: os.Rename(tmp, abs)
7. defer os.Remove(tmp) 清理失败路径的临时文件

对比直接 WriteFile:
  WriteFile 先 O_TRUNC 再写入，写一半崩溃会留下损坏文件。
  atomicWriteFile 通过 rename 原子性避免此问题。
```

## 8. 存储格式

### 8.1 目录结构

```
.tasks/                          # 任务根目录（tasksDirName = ".tasks"）
├── task_a1b2c3d4.json           # 每个任务一个独立 JSON 文件
├── task_e5f6a7b8.json
└── task_c9d0e1f2.json
```

### 8.2 文件权限

- 目录: `0o755`（os.MkdirAll）
- 文件: `0o644`（os.OpenFile / atomicWriteFile）

### 8.3 文件格式

```json
{
  "id": "task_a1b2c3d4",
  "subject": "Implement auth module",
  "description": "Write the authentication middleware",
  "status": "pending",
  "owner": null,
  "blockedBy": ["task_e5f6a7b8"]
}
```

- 使用 `json.MarshalIndent` 缩进 2 空格
- 文件末尾有 `\n` 换行
- `owner` 为 null 表示未认领
- `blockedBy` 为空列表时表示无依赖（不是 null）

## 9. 并发安全

- taskStore 本身无内存状态（只有 dir 路径），所有操作直接读写文件
- 文件写入使用 atomicWriteFile（写临时文件 → rename），避免并发写入导致文件损坏
- 文件创建使用 O_EXCL 独占模式，避免 ID 撞号时覆盖
- 建议: 由上层 Agent 串行化调用，避免多 goroutine 同时操作同一任务文件
- list() 读取目录时，其他 goroutine 的文件写入不影响已读取的文件列表

## 10. 状态机

### 10.1 状态转换规则

```
合法转换:
  pending    → in_progress   (claim: 开始工作)
  in_progress → completed    (complete: 完成工作)
  any        → deleted       (标记删除，当前实现中通过从列表中排除来模拟)

非法转换（校验时拒绝）:
  completed  → pending       (已完成不可回退)
  completed  → in_progress   (已完成不可重启)
  deleted    → any           (已删除不可恢复)
  pending    → completed     (未经认领不可直接完成)
```

### 10.2 状态转换图

```
         create()
           │
           ▼
      ┌─────────┐
      │ pending  │
      └────┬─────┘
           │
           │ claim()
           │ (前置条件: blockedBy 全部 completed)
           ▼
      ┌─────────────┐
      │ in_progress  │
      └────┬────────┘
           │
           │ complete()
           │ (前置条件: owner 匹配)
           ▼
      ┌───────────┐
      │ completed  │
      └───────────┘
```

### 10.3 状态校验点

| 操作 | 前置状态要求 | 附加条件 |
|------|------------|---------|
| claim | pending | blockedBy 中所有任务均为 completed |
| complete | in_progress | Owner 匹配（调用者 == 认领者） |
| updateDependencies | pending | Owner == nil（未认领） |

## 11. DAG 依赖校验

### 11.1 开始任务前（claim 时）

```
检查 blockedBy 中的所有任务是否 completed:

func incompleteDependencies(t *Task) []string:
  var out []string
  for _, dep := range t.BlockedBy:
    d, err := s.load(dep)
    if err != nil:         // 文件不存在/损坏 → 视为未完成
      out = append(out, dep)
    else if d.Status != taskStatusCompleted:
      out = append(out, dep)
  return out

claim 时:
  if len(incompleteDependencies(t)) > 0:
    返回 "Blocked by: {dep1}, {dep2}, ..."
```

### 11.2 完成任务后（complete 时）

```
完成一个任务后，检查是否有下游任务因此被解锁:

1. 快照"完成前已就绪"的任务集合 readyBefore
2. 设置 t.Status = completed 并保存
3. 重新扫描所有任务，找出"现在就绪但之前还不在 readyBefore 中"的任务
4. 这些就是"新解锁"的任务，在返回消息中报告

注意: "完成前就已可开始的 pending 任务"不会重复上报（通过 readyBefore 去重）
```

### 11.3 添加依赖时（updateDependencies 时）

```
环检测流程:

1. 校验目标任务状态:
   - 必须是 pending
   - 必须未认领（Owner == nil）

2. 校验每条新依赖:
   - 不允许自依赖（dep == taskID）
   - 依赖任务必须存在（load 成功）
   - 重复边安全跳过（幂等操作）

3. 临时添加边后检测环:
   for _, dep := range added:
     if s.dependsOn(dep, t.ID):
       拒绝，返回错误

4. 通过所有校验后保存
```

## 12. 错误处理

| 错误场景 | 处理方式 | 返回值类型 |
|---------|---------|-----------|
| subject 为空 | 返回 error | `error("subject is required")` |
| ID 格式非法 | 返回 error | `error("invalid task id ...")` |
| 任务文件不存在 | 返回 error | `error("task {id} not found")` |
| JSON 文件损坏 | 返回 error | `error("task {id} is corrupted: ...")` |
| ID 撞号（8 次重试） | 返回 error | `error("could not allocate a unique task id")` |
| 非 pending 状态加依赖 | 返回 error | `error("task {id} is {status}, cannot update dependencies")` |
| 已认领任务改依赖 | 返回 error | `error("task {id} is owned by {owner}, cannot update dependencies")` |
| 自依赖 | 返回 error | `error("task cannot depend on itself")` |
| 依赖不存在 | 返回 error | `error("dependency {dep}: ...")` |
| 环形依赖 | 返回 error | `error("adding {dep} would create a dependency cycle through task {id}")` |
| 非 pending 状态认领 | 返回消息文本（非 error） | `"Task {id} is {status}, cannot claim"` |
| 依赖未完成 | 返回消息文本（非 error） | `"Blocked by: {dep1}, {dep2}, ..."` |
| 非 in_progress 状态完成 | 返回消息文本（非 error） | `"Task {id} is {status}, cannot complete"` |
| Owner 不匹配 | 返回消息文本（非 error） | `"Task {id} is owned by {owner}, not {caller}"` |
| 目录不存在 | list() 返回 nil | 不是错误 |
| 文件读取/解析失败 | list() 跳过该文件 | 不阻塞其余任务 |

注意: claim 和 complete 的前置条件不满足时返回的是消息文本（string），不是 error。这允许工具层将消息直接展示给 LLM，由 LLM 决定下一步。

## 13. 测试要点

### 13.1 单元测试

1. **validateTaskID**:
   - 合法 ID: `"task_a1b2c3d4"` → nil
   - 长度不对: `"task_a1b2"` → error
   - 前缀不对: `"tsk_a1b2c3d4"` → error
   - 非法字符: `"task_A1B2C3D4"` → error（大写）
   - 路径逃逸: `"task_../foo"` → error
   - 空字符串 → error

2. **newTaskID**:
   - 生成 100 个 ID，验证全部通过 validateTaskID
   - 验证无重复（统计唯一性）

3. **create**:
   - 正常创建 → 文件存在，JSON 格式正确，BlockedBy 为 `[]`
   - subject 为空（含纯空格）→ error("subject is required")
   - 目录不存在 → 自动创建 .tasks/
   - ID 撞号 → 重试成功（模拟 O_EXCL 冲突）
   - 8 次全部撞号 → error

4. **load**:
   - 正常加载 → Task 对象正确，BlockedBy 非 nil
   - 文件不存在 → "task {id} not found"
   - ID 格式非法 → error
   - JSON 损坏 → "task {id} is corrupted: ...""
   - BlockedBy 为 null → 自动转为 []string{}

5. **save**:
   - 正常保存 → 文件内容正确（原子写入）
   - ID 格式非法 → error

6. **list**:
   - 空目录 → nil（空切片）
   - 目录不存在 → nil
   - 混合 .json 和非 .json 文件 → 只返回 .json
   - 损坏文件 → 跳过，不阻塞
   - 结果按 ID 升序排序

7. **updateDependencies**:
   - 正常添加依赖 → BlockedBy 更新
   - 重复添加同一条边 → 幂等，不重复记录
   - 自依赖 → error
   - 环形依赖 → error
   - 非 pending 状态 → error
   - 已认领任务 → error
   - 依赖不存在 → error

8. **claim**:
   - 正常认领 → status=in_progress, owner 设置
   - 默认 owner → "agent"
   - 有未完成的 blockedBy → 返回 "Blocked by: ..." 消息
   - blockedBy 文件丢失 → 视为未完成
   - 非 pending 状态 → 返回消息

9. **complete**:
   - 正常完成 → status=completed
   - owner 不匹配 → 返回消息
   - 非 in_progress 状态 → 返回消息
   - 解锁下游任务 → 消息中包含 "Unblocked: ..."
   - 完成前已就绪的任务不重复上报

### 13.2 环检测测试

1. **无环**: A→B→C，添加 C→D → 成功
2. **直接环**: A→B，添加 B→A → 检测到环
3. **间接环**: A→B→C，添加 C→A → 检测到环
4. **自环**: A→A → 检测到环
5. **复杂图**: A→B, A→C, B→D, C→D，添加 D→A → 检测到环

### 13.3 集成测试

1. **完整生命周期**:
   - create → updateDependencies(加依赖) → claim → complete
   - 验证每个阶段文件内容正确

2. **多任务依赖链**:
   - 创建 A, B, C
   - B blockedBy A, C blockedBy B
   - claim A → complete A → claim B → complete B → claim C
   - 验证依赖检查正确
   - 验证 complete A 时报告 "Unblocked: B 的 subject"

3. **两阶段建图**:
   - 同一批次 create A, B, C
   - 获取 ID 后 updateDependencies 加边
   - 验证 ID 在 create 后可用于后续 update

4. **并发安全**:
   - 同时创建多个任务，验证 ID 不冲突
   - 验证原子写入不会导致文件损坏

5. **跨会话恢复**:
   - 创建任务后重新 newTaskStore（模拟重启）
   - 验证 list() 能正确恢复所有任务
