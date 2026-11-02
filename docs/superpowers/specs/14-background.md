# 14 - 后台任务系统规格

## 1. 职责

后台任务系统负责管理异步后台任务的启动、监控、结果收集:

1. **启动** — 将命令放入后台 goroutine 执行，立即返回 `bg_id` 占位结果
2. **执行** — 在独立 goroutine 中运行命令，管理超时和进程组
3. **收集** — 在后续轮次开头收集已完成任务的结果，格式化为 `<task_notification>`
4. **注入** — 将收集到的通知作为独立 user 事件追加进对话消息
5. **清理** — Agent 退出时停止所有运行中的后台任务，清理进程组

**与同步 bash 的区别**:
- 同步 bash: tool_call → 阻塞等待 → 命令返回 → tool_result（一次 tool_use 对应一次 tool_result）
- 后台 bash: tool_call → start() → 立即返回 bg_id 占位 tool_result → 命令结束后进入完成队列 → loop 在下一轮开头用 collect() 把结果以 `<task_notification>` 独立事件注入对话

**通知不复用原 tool_use_id**: 原 tool_call 已经用占位 tool_result 应答过了，一次 tool_use 仍然恰好对应一次 tool_result。后台结果通过独立的 user message 注入。

## 2. 文件位置

```
pkg/background/
└── background.go      # backgroundManager、backgroundTask、所有方法
```

辅助文件（进程组管理，平台相关）:
```
procgroup_unix.go      # Unix: Setpgid + kill(-pgid, SIGTERM)
procgroup_windows.go   # Windows: CREATE_NEW_PROCESS_GROUP + taskkill
```

## 3. 依赖

- `os/exec` — 命令执行
- `context` — 超时和取消（context.WithTimeout）
- `sync` — 互斥锁保护并发访问（sync.Mutex）
- `bytes` — stdout/stderr 缓冲
- `fmt` — ID 格式化和通知构建
- `syscall` — 进程组管理（平台相关）
- `time` — 超时控制
- `strings` — 输出处理
- `encoding/json` — 输入参数解析
- `errors` — 错误类型判断（errors.As）
- `unicode/utf8` — 输出截断（按 rune 计数）
- `github.com/anthropics/anthropic-sdk-go` — MessageParam 类型（注入对话）

## 4. 数据结构

### 4.1 backgroundTask

```go
type backgroundTask struct {
    id      string             // bg_0001 格式，自增编号
    command string             // 原始 shell 命令
    status  string             // "running" | "completed" | "failed"
    detail  string             // 通知里 status 后的补充说明
    result  string             // 命令输出（stdout + stderr 合并，截断后）
    cmd     *exec.Cmd          // running 期间非 nil，stopAll 用它清理进程组
    cancel  context.CancelFunc // 尚未 Start 时 stopAll 的兜底：取消 ctx 让 Run 立即失败
}
```

字段说明:
- `id`: 格式为 `"bg_"` + 4 位零填充自增编号，如 `"bg_0001"`, `"bg_0002"`
- `command`: 用户提交的原始 shell 命令文本
- `status`: 三态枚举:
  - `"running"` — goroutine 正在执行命令
  - `"completed"` — 命令成功完成（exit 0）
  - `"failed"` — 命令失败（非零退出码 / 超时 / 启动错误 / 被停止）
- `detail`: 附加信息文本:
  - 成功时: `""` (空字符串)
  - 非零退出: `" (exit N)"` (如 `" (exit 1)"`)
  - 超时: `" (timeout)"`
  - 启动错误: `" " (error: ...)"`
  - 被停止: `" (stopped)"`
- `result`: 命令输出，stdout + stderr 合并后 TrimSpace，按 rune 截断到 maxBashOutputRunes
- `cmd`: running 期间非 nil；命令结束后设为 nil；stopAll 用它发信号清理进程组
- `cancel`: context 取消函数；start() 中设置，run() 中 defer 调用；stopAll 在 cmd.Process 为 nil 时兜底调用

### 4.2 backgroundManager

```go
type backgroundManager struct {
    mu      sync.Mutex                  // 保护所有字段的并发访问
    next    int                         // bg_id 自增计数器，从 0 开始，每次 start 先 +1
    timeout time.Duration               // 单条命令超时，默认 defaultBashTimeout (120s)
    tasks   map[string]*backgroundTask  // id → task 映射
    ready   []string                    // 已完成待收集的任务 id，FIFO 队列
}
```

### 4.3 全局实例

```go
var backgroundTasks = newBackgroundManager()
```

backgroundTasks 是进程级全局管理器。REPL 每轮用户输入都新建 agentLoop/sessionState，而后台任务的生命周期跨越多轮对话，因此必须挂在包级。

## 5. 关键常量

```go
const (
    defaultBashTimeout = 120 * time.Second  // 单条命令超时（与同步 bash 相同）
    maxBashOutputRunes = 50000              // 输出截断上限（按 rune 计数）
)

// bg_id 格式: "bg_" + 4 位零填充十进制
// 示例: "bg_0001", "bg_0002", ..., "bg_9999"
// 计数器在 backgroundManager.next 中，进程级生命周期
```

## 6. 核心函数

### 6.1 newBackgroundManager()

```
函数签名: func newBackgroundManager() *backgroundManager

流程:
1. 创建 backgroundManager:
   &backgroundManager{
     timeout: defaultBashTimeout,   // 120 秒
     tasks:   map[string]*backgroundTask{},
   }
2. 返回（next 默认为 0，ready 默认为 nil）
```

### 6.2 start()

```
函数签名: func (m *backgroundManager) start(command string) string

流程:
1. 加锁 (m.mu.Lock())
   defer m.mu.Unlock()

2. 生成 ID:
   m.next++
   id := fmt.Sprintf("bg_%04d", m.next)
   // 示例: "bg_0001", "bg_0002", ...

3. 创建带超时的 context:
   ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
   // 超时时间: defaultBashTimeout = 120 秒

4. 构建 shell 命令:
   cmd := shellCommand(ctx, command)
   // 等价于: exec.CommandContext(ctx, "sh", "-c", command)

5. 设置进程组属性（确保可以一起杀死所有子进程）:
   cmd.SysProcAttr = sysProcAttr()
   // Unix: &syscall.SysProcAttr{Setpgid: true}
   // Windows: &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}

6. 创建 stdout/stderr 缓冲:
   var stdout, stderr bytes.Buffer
   cmd.Stdout = &stdout
   cmd.Stderr = &stderr

7. 注册任务（在持锁阶段）:
   t := &backgroundTask{
     id:      id,
     command: command,
     status:  "running",
     cmd:     cmd,
     cancel:  cancel,
   }
   m.tasks[id] = t

   注意: cmd 在持锁阶段同步登记到任务上，保证 stopAll 从 start 返回后
   的任何时刻都能看到运行中的进程组，不会错过清理窗口。

8. 启动 goroutine:
   go m.run(t, ctx, &stdout, &stderr)

9. 记录日志:
   appLog.Info("background task started", "id", id, "command", command)

10. 返回 id
```

### 6.3 run()

```
函数签名: func (m *backgroundManager) run(t *backgroundTask, ctx context.Context, stdout, stderr *bytes.Buffer)

流程:
1. defer t.cancel()
   // 确保 context 资源释放，无论命令成功/失败/超时

2. 等待命令完成:
   err := t.cmd.Run()

3. 清理子进程:
   stopProcessGroup(t.cmd)
   // 生命周期清理：结束时停掉原始进程组里的残留子进程
   // Unix: syscall.Kill(-pgid, SIGTERM)
   // Windows: taskkill /T /F /PID

4. 判断最终状态并构建结果:
   status, detail, result := "completed", "", combineBashOutput(*stdout, *stderr)

   a. 超时检查:
      if ctx.Err() == context.DeadlineExceeded:
        status = "failed"
        detail = " (timeout)"
        result = fmt.Sprintf("Error: Timeout (%ds)", int(m.timeout.Seconds()))
        // 注意: 超时时的 result 是固定格式文本，不是命令输出

   b. 错误检查:
      else if err != nil:
        status = "failed"
        var ee *exec.ExitError
        if errors.As(err, &ee):
          detail = fmt.Sprintf(" (exit %d)", ee.ExitCode())
        else:
          detail = " (error: " + err.Error() + ")"

   c. 成功:
      // status 保持 "completed"，detail 保持 ""

5. 加锁，更新状态并追加到 ready 队列:
   m.mu.Lock()
   t.status = status
   t.detail = detail
   t.result = result
   t.cmd = nil                    // 释放 cmd 引用
   m.ready = append(m.ready, t.id)
   m.mu.Unlock()

6. 记录日志:
   if status == "completed":
     appLog.Info("background task finished", "id", t.id, "status", status)
   else:
     appLog.Warn("background task failed", "id", t.id, "status", status, "detail", detail)
```

### 6.4 collect()

```
函数签名: func (m *backgroundManager) collect() []string

流程:
1. 加锁 (m.mu.Lock())
   defer m.mu.Unlock()

2. 检查 ready 队列:
   if len(m.ready) == 0:
     return nil

3. 弹出全部 ready id:
   ids := m.ready
   m.ready = nil    // 清空队列

4. 格式化通知:
   var notes []string
   for _, id := range ids:
     t := m.tasks[id]
     body := t.result
     if body == "":
       body = "(no output)"
     note := fmt.Sprintf(
       "<task_notification>\nBackground task %s (%s) %s%s:\n%s\n</task_notification>",
       t.id, t.command, t.status, t.detail, body,
     )
     notes = append(notes, note)

5. 返回 notes（按 FIFO 顺序，即完成时间顺序）

通知格式示例:
  <task_notification>
  Background task bg_0001 (go test ./...) completed:
  ok    github.com/example/pkg    0.5s
  </task_notification>
```

### 6.5 stopAll()

```
函数签名: func (m *backgroundManager) stopAll()

用途: 在 Agent 退出路径上调用（正常退出与 SIGINT/SIGTERM 均覆盖）

流程:
1. 加锁 (m.mu.Lock())
   defer m.mu.Unlock()

2. 遍历所有任务:
   stopped := 0
   for _, t := range m.tasks:
     if t.status != "running":
       continue    // 已完成/失败的任务不需要停止

     a. 进程已启动（cmd.Process != nil）:
        stopProcessGroup(t.cmd)
        // Unix: kill(-pgid, SIGTERM) → 等 100ms → kill(-pgid, SIGKILL)
        // Windows: taskkill /T /F /PID

     b. 进程未启动（Process 还是 nil）:
        t.cancel()
        // 取消 ctx，让随后的 cmd.Run() 立即以错误返回

     c. stopped++

3. 记录日志:
   if stopped > 0:
     appLog.Info("background tasks stopped", "count", stopped)
```

### 6.6 taskStatus()

```
函数签名: func (m *backgroundManager) taskStatus(id string) string

用途: 供测试与调试使用

流程:
1. 加锁 (m.mu.Lock())
   defer m.mu.Unlock()

2. 查找任务:
   t, ok := m.tasks[id]
   if !ok:
     return ""

3. 返回 t.status
```

## 7. 辅助函数

### 7.1 combineBashOutput()

```
函数签名: func combineBashOutput(stdout, stderr bytes.Buffer) string

流程:
1. 合并 stdout 和 stderr:
   output := stdout.String() + stderr.String()

2. 去除首尾空白:
   output = strings.TrimSpace(output)

3. 按 rune 截断:
   runes := []rune(output)
   if len(runes) > maxBashOutputRunes (50000):
     output = string(runes[:maxBashOutputRunes])

4. 返回 output
```

### 7.2 shouldRunBackground()

```
函数签名: func shouldRunBackground(toolName string, input json.RawMessage) bool

用途: 判断本次工具调用是否走后台。
     仅当 bash 调用显式把 run_in_background 设为 true 时返回 true。
     不再从 install/build/test 等关键词猜测。

流程:
1. 检查工具名:
   if toolName != "bash":
     return false

2. 解析 JSON 输入:
   var args struct {
     RunInBackground bool `json:"run_in_background"`
   }
   json.Unmarshal(input, &args)
   // 解析失败 → return false（默认不走后台）

3. 返回 args.RunInBackground
```

### 7.3 startBackgroundTask()

```
函数签名: func startBackgroundTask(input json.RawMessage) string

用途: 解析 bash 参数并启动后台任务，返回给模型的占位结果

流程:
1. 解析 command:
   var args struct {
     Command string `json:"command"`
   }
   json.Unmarshal(input, &args)

2. 调用启动:
   id := backgroundTasks.start(args.Command)

3. 返回确认消息:
   return fmt.Sprintf(
     "[Background task %s started] The result will be delivered as a " +
     "<task_notification> in a later turn. Continue with other work in the meantime.",
     id,
   )
```

### 7.4 injectBackgroundResults()

```
函数签名: func injectBackgroundResults(messages []anthropic.MessageParam) []anthropic.MessageParam

用途: 在每轮模型调用前收集已完成的后台任务，把通知作为独立的 user 事件追加进对话。
     没有就绪结果时原样返回，不产生额外消息。

流程:
1. 收集结果:
   notes := backgroundTasks.collect()

2. 无结果 → 原样返回:
   if len(notes) == 0:
     return messages

3. 拼接所有通知:
   combined := strings.Join(notes, "\n\n")

4. 追加为 user message:
   messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(combined)))

5. 返回 messages
```

## 8. 任务完成通知机制

### 8.1 通知格式

每个完成的任务生成一个 `<task_notification>` 块:

```
<task_notification>
Background task {id} ({command}) {status}{detail}:
{result}
</task_notification>
```

字段说明:
- `{id}`: bg_0001 格式的任务 ID
- `{command}`: 原始 shell 命令
- `{status}`: completed / failed
- `{detail}`: 空字符串 / `" (exit N)"` / `" (timeout)"` / `" (error: ...)"`
- `{result}`: 命令输出（截断后），空输出时显示 `"(no output)"`

### 8.2 多个通知拼接

多个完成任务的通知用 `"\n\n"`（双换行）拼接为一条 user message:

```
<task_notification>
Background task bg_0001 (go test ./...) completed:
ok  	github.com/example/pkg	0.5s
</task_notification>

<task_notification>
Background task bg_0002 (npm run build) failed (exit 1):
Error: Module not found
</task_notification>
```

### 8.3 排序规则

通知按 FIFO 顺序排列，即按任务完成时间排序（先进入 ready 队列的排在前面）。

## 9. 与 Agent 循环的集成

### 9.1 集成点

```
Agent Loop 每轮执行流程:

1. 接收用户输入
2. ★ injectBackgroundResults(messages)  ← 在此收集后台结果
3. 调用 LLM（messages 中已包含后台通知）
4. LLM 返回 tool_calls
5. 执行工具调用
   - 如果 tool_call 是 bash + run_in_background=true:
     ★ startBackgroundTask(input) → 返回占位结果
   - 其他工具: 正常同步执行
6. 将工具结果追加到 messages
7. 回到步骤 3（继续循环直到 LLM 不再返回 tool_calls）
```

### 9.2 注入时机

- 在每轮 LLM 调用前（步骤 2）
- 通过 `injectBackgroundResults()` 函数实现
- 注入为独立的 user message（不是 system message）

### 9.3 注入格式

注入的消息格式:
```
User: <task_notification>
      Background task bg_0001 (go test ./...) completed:
      ok  	github.com/example/pkg	0.5s
      </task_notification>
```

LLM 看到此消息后，可以:
- 根据结果继续推理
- 向用户报告后台任务已完成
- 决定下一步操作

## 10. 超时和清理

### 10.1 超时策略

- 单个后台任务超时: `defaultBashTimeout = 120 * time.Second`
- 通过 `context.WithTimeout` 实现
- 超时后:
  - status = "failed"
  - detail = " (timeout)"
  - result = "Error: Timeout (120s)"（固定格式文本，不是命令输出）
- 任务函数无法自行覆盖超时（由 context 强制取消）

### 10.2 进程组清理

命令结束时（无论成功/失败/超时），执行进程组清理:

```
run() 中:
  defer t.cancel()          // 1. 确保 context 释放
  err := t.cmd.Run()        // 2. 等待命令完成
  stopProcessGroup(t.cmd)   // 3. 停止整个进程组的残留子进程

stopAll() 中:
  if t.cmd.Process != nil:
    stopProcessGroup(t.cmd)  // 进程已启动 → 杀进程组
  else:
    t.cancel()               // 进程未启动 → 取消 context
```

平台差异:
- Unix: `syscall.Kill(-pgid, SIGTERM)` → 等 100ms → `syscall.Kill(-pgid, SIGKILL)`
- Windows: `exec.Command("taskkill", "/T", "/F", "/PID", pid).Run()`

### 10.3 会话结束清理

- 在 Agent 退出路径上调用 `backgroundTasks.stopAll()`
- 覆盖场景: 正常退出、SIGINT、SIGTERM
- stopAll 会停止所有 status="running" 的任务
- 已完成/失败的任务不受影响

### 10.4 结果清理

- 已完成任务的结果在 `collect()` 调用后清空（ready 队列置 nil）
- 任务对象本身保留在 tasks map 中（不自动删除）
- 重复 collect 不会返回已收集的结果

## 11. 并发安全

### 11.1 锁保护范围

`backgroundManager.mu` (sync.Mutex) 保护:
- `next` — ID 计数器（读写
- `tasks` — 任务映射（读写）
- `ready` — 就绪队列（读写）

### 11.2 无锁操作（依赖 happens-before）

- `backgroundTask` 的 `status`/`detail`/`result` 字段:
  只在 goroutine 内部写入一次（完成后），然后由 collect() 读取。
  存在 happens-before 关系（通过 mutex unlock → lock）。

- `cmd` 和 `cancel` 字段:
  在 start() 中设置（持锁期间），run() 中读取。
  start() 的 unlock 发生在 goroutine 启动之前，保证可见性。

### 11.3 关键不变量

1. start() 在 unlock 之前注册任务 → goroutine 能看到完整的 task 初始化
2. run() 在加锁后才追加到 ready → collect() 看到的 task 数据是完整的
3. stopAll() 持有锁期间修改 task.status → 与 collect() 互斥
4. t.cmd 设为 nil 在持锁期间 → stopAll 检查 t.cmd != nil 是安全的

## 12. 执行模型时序图

```
同步 bash:
  tool_call → 阻塞等待 → 命令返回 → tool_result

后台 bash:
  tool_call ──→ start() ──→ 立即返回 bg_id 占位 tool_result
                  │
                  └──→ goroutine: run() ──→ 命令完成 ──→ 进入 ready 队列
                                                               │
  下一轮 loop 开头:                                            │
    collect() ←────────────────────────────────────────────────┘
       │
       └──→ <task_notification> ──→ 注入对话（user message）
```

### 12.1 详细时序

```
Turn N:
  User: "run the tests in background"
  Agent: tool_call bash(command="go test ./...", run_in_background=true)
  → shouldRunBackground("bash", input) → true
  → startBackgroundTask(input)
    → backgroundTasks.start("go test ./...")
    → 返回 id = "bg_0001"
  → 返回占位结果: "[Background task bg_0001 started] ..."
  Agent: 继续推理，可能调用其他工具或回复用户

... 后台 goroutine 执行命令 ...

Turn N+1:
  User: "what else can we do?"
  → injectBackgroundResults(messages):
    → backgroundTasks.collect()
    → ["<task_notification>Background task bg_0001 ...</task_notification>"]
    → 追加为 user message
  → Agent 看到通知，知道后台任务已完成
```

## 13. 错误处理

| 错误场景 | 处理方式 |
|---------|---------|
| 命令执行失败（非零退出码） | status="failed", detail=" (exit N)", result=命令输出 |
| 命令超时（120s） | status="failed", detail=" (timeout)", result="Error: Timeout (120s)" |
| 命令启动失败 | status="failed", detail=" (error: ...)", result=命令输出 |
| 进程组被 stopAll 停止 | 由 stopProcessGroup 发信号终止 |
| JSON 参数解析失败 | shouldRunBackground 返回 false；startBackgroundTask 用零值 command |
| 输出超长（>50000 rune） | 截断到 maxBashOutputRunes，无额外标记 |
| context 取消 | run() 中 defer cancel() 确保资源释放 |
| stopAll 后再 collect | 不会 panic；stopped 的任务不再进入 ready 队列 |

## 14. 测试要点

### 14.1 单元测试

1. **newBackgroundManager**:
   - 返回非 nil 管理器
   - timeout = 120s
   - tasks 为空 map
   - next = 0

2. **start**:
   - 返回 "bg_0001" 格式 ID
   - 连续调用 → ID 自增: bg_0001, bg_0002, ...
   - 任务注册到 tasks map，status="running"
   - goroutine 启动（可通过等待 status 变化验证）

3. **run**:
   - 命令成功 → status="completed", detail="", result=输出
   - 命令失败（exit 1）→ status="failed", detail=" (exit 1)"
   - 命令超时 → status="failed", detail=" (timeout)", result 包含 "Timeout"
   - 完成后追加到 ready 队列
   - cmd 设为 nil

4. **collect**:
   - 无就绪任务 → nil
   - 有就绪任务 → 返回格式化通知切片
   - 通知格式: `<task_notification>...</task_notification>`
   - 收集后 ready 队列清空（再次 collect 返回 nil）
   - 多个任务 → FIFO 顺序

5. **stopAll**:
   - 停止所有 running 任务
   - 已完成任务不受影响
   - 记录停止数量
   - 无 running 任务时不记录日志

6. **taskStatus**:
   - 存在的任务 → 返回当前状态
   - 不存在的任务 → 返回 ""

7. **combineBashOutput**:
   - 正常合并 stdout + stderr
   - 首尾空白去除
   - 超过 50000 rune → 截断
   - 空输出 → 空字符串

8. **shouldRunBackground**:
   - 非 bash 工具 → false
   - bash + run_in_background=false → false
   - bash + run_in_background=true → true
   - bash + JSON 解析失败 → false

### 14.2 集成测试

1. **完整流程**:
   - start → 等待完成 → collect → 验证通知格式和内容

2. **超时处理**:
   - start("sleep 300") with timeout=1s → 验证 timeout 状态和消息

3. **并发启动**:
   - 同时 start 多个命令 → 验证 ID 不冲突
   - 全部完成后 collect → 验证通知数量正确

4. **进程组清理**:
   - start("sleep 100 & sleep 100") → stopAll → 验证所有子进程被杀死

5. **输出截断**:
   - start("yes | head -100000") → 验证输出被截断到 50000 rune

6. **注入对话**:
   - start → 完成 → injectBackgroundResults → 验证 messages 追加了 user message

### 14.3 边界条件

1. 空命令 → 验证行为（sh -c "" 立即返回 0）
2. 立即完成的命令 → collect 在下一轮能拿到
3. 大量后台任务 → ready 队列无上限（由 collect 清空）
4. stopAll 后再 collect → 不应 panic
5. collect 空队列 → 返回 nil，不产生额外消息
6. 命令输出为空 → result 显示 "(no output)"
