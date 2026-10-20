# s13: Agent Teams — 实施计划

## Summary

按 s13 README spec，在现有 Go harness 上实现 Lead 管理的团队运行时：

- **Lead**（现有主 loop）：新增 7 个团队工具（spawn_teammate / send_message / shutdown_request / request_plan / respond_plan_request / create_worktree / list_teammates），system prompt 加入"先提案、等确认"约束。
- **Teammate**：持久运行时（goroutine），独立 messages/system prompt/受限工具集，WORK → IDLE 循环，支持自动认领任务板上的就绪任务。
- **MessageBus**：`.mailboxes/<name>.jsonl` 文件邮箱 + 通道通知，lead 与队友双向通信。
- **REPL 运行时投递**：main loop 同时等待 stdin 与 lead 邮箱，事件到达时消费邮箱、注入 `[Team events]`、开启新一轮 Lead turn。
- **任务认领原子化**：taskStore 加锁 + owner-in-progress 检查；认领绑定工作目录（可选 worktree）。
- **Worktree**：`Task.worktree` 可选字段 + `create_worktree`（Lead-only 模型工具）+ `remove_worktree`（host 专属，REPL 命令）。
- **Typed 协议**：shutdown_request/response 与 plan 审批用 request_id 关联；plan gate 在 required/pending/rejected 状态下封锁 bash/write_file/edit_file。

不引入 s11 background / s12 scheduled（README 明确本章不携带）。

## Current State Analysis

| 现有组件 | 位置 | 与 s13 的关系 |
|---|---|---|
| 主 REPL loop | main.go | 单通道等待 stdin，需改造为 stdin + lead 邮箱双路 select |
| agentLoop | agent.go | Lead loop 保持不变；teammate 用自己的简化 loop（不放 cpt/memory/todo 逻辑） |
| 工具注册 | tools.go | `toolHandlers` + `allTools()`；文件工具依赖包级 `workdir` 与 `safePath`，需要 baseDir 化 |
| s10 任务系统 | taskstore.go | `.tasks/{id}.json`；claim 无锁、无 owner-in-progress 检查、Task 无 worktree/result 字段 |
| s06 子代理 | task.go | one-shot 嵌套 loop，teammate 是它的持久化演化（复用其简化 loop 结构） |
| s11 后台任务 | background.go | 包级全局模式（`backgroundTasks`）可复制到 team/bus；teammate bash 不走后台 |
| 权限管线 | permission.go / hooks.go | `askUser` 读 stdin —— teammate 线程不能阻塞等用户输入，需非交互版本 |
| LLM 客户端 | llm.go | `LLMClient` 接口可注入 mock，system prompt 需加团队指引 |

## Proposed Changes

### 1. 新文件 team.go（核心，约 600 行）

**MessageBus**（文件为事实源，channel 做唤醒）：

```go
type Message struct {
    From, To, Content, Type string // message | result | idle_notification |
                                    // shutdown_request|shutdown_response |
                                    // plan_request|plan_approval_request|plan_approval_response
    Metadata map[string]any         // request_id / approve / feedback / task_id ...
}
type messageBus struct {
    dir       string               // <workdir>/.mailboxes/
    mu        sync.Mutex
    notifiers map[string]chan struct{}
}
var bus = newMessageBus(filepath.Join(workdir, ".mailboxes"))
```
- `send(from, to, content, type, metadata)`：O_APPEND 追加 JSONL + 非阻塞 notify(to)。**`from`/`to` 是完全对称的任意成员名**——`lead → teammate`、`teammate → lead`、`teammate → teammate` 走同一条代码路径，不存在"只能发给 lead"的隐含约束。
- `waitMessages(agent, timeout)`：持锁查文件，空则 select notifier/timer；**只读不消费**。
- `readInbox(agent)`：读 + 删文件（消费）。每个成员邮箱的唯一消费者是它自己的 runtime：`lead` 由 `consumeLeadInbox()` 消费，`<teammate>` 由其 runtime 的 `tm.handleInbox()` 消费。
- `registerNotifier(name)`：在 spawn 阶段（goroutine 启动前）为成员注册 `notifiers[name]`，保证 `send` 到该成员时唤醒通道已就绪，不存在"消息已落盘但 nobody 可被唤醒"的窗口。

**包级共享状态**（仿 backgroundTasks 模式，全部带锁）：

```go
var teamRegistry   // name -> *teammateRuntime
var pendingRequests map[string]*protocolState  // request_id -> {type,status,payload,sender,workVersion,taskID}
var planGates      map[string]*planGate        // name -> {status: not_required|required|pending|approved|rejected, approvedVersion}
var teammateAssignments map[string]assignment  // owner -> {taskID, cwd}（轮末租约）
```

**teammateRuntime**：

```go
type teammateRuntime struct {
    name        string
    client      LLMClient
    st          *sessionState          // baseDir = 认领任务的 cwd
    messages    []anthropic.MessageParam
    workVersion int                    // claim/release 时 +1，使旧 plan 审批失效
    phase       string                 // working | idle（atomic，供 list_teammates）
    stopCh      chan struct{}
}
```

- `spawnTeammate(name, taskID string, requirePlan bool) string`：校验名字（非空、`^[a-zA-Z][a-zA-Z0-9_-]{0,31}$`、保留名 `lead`/`agent` 拒绝、不重复）→ **先原子 claim 初始任务，失败则不启动线程** → requirePlan 时置 gate=required → `bus.registerNotifier(name)` → 写入初始 user 消息（任务 id/subject/description/cwd）→ `go tm.run()`。
- `tm.run()` 外层循环：
  - **WORK**：简化内层 loop（≤30 轮，maxTokens 8000）：`Send(teammateSystemPrompt(tm), messages, teammateTools())` → `runTeammateTool` 分发 → 追加 tool_result；**每轮开头以 timeout=0 非阻塞拉 inbox**（`tm.handleInbox()`），因此 WORK 期间到达的队友消息会在下一次迭代注入，无需等本任务结束。无 tool_use → 结束：有 assignment 则向 lead 发 `result`（最终文本）+ `idle_notification` 两条事件，释放 assignment（workVersion++）。
  - **IDLE**：`bus.waitMessages(name, idleScanInterval)`；消息优先于任务板：shutdown → 优雅退出；任意来源的普通消息（lead 指令或队友消息）→ 回 WORK；无消息则 `claimNextTask()`（扫描 pending+无 owner+canStart → 原子 claim → 注入 `[Auto-claimed task …]` user 消息 → 回 WORK）。
- `tm.handleInbox() bool`（队友侧消息分发，**唯一消费者**）：readInbox(name) 后按 `type` + `from` 分派：
  - `from=lead` + `shutdown_request` → 结束当前步骤后回 `shutdown_response`，返回 "stop"；
  - `from=lead` + `plan_approval_response` → 按 request_id 更新 gate（approved/rejected + 记录 workVersion）；
  - `type=message`（**任意来源**，含队友）→ 追加 `[Message from <from>] <content>` 的 user 消息注入到自己的 messages；注入发生在**轮边界**（模型调用之前），当前工具批次跑完继续，不打断正在执行的工具；
  - `type=result` / `idle_notification` 按 README 语义只由 runtime 发往 lead，队友邮箱里出现即视为异常消息，仅作普通 message 注入并在日志里标注。
  普通队友消息**不修改** workVersion / task 归属 / plan gate 状态——它只是对话输入，与 README §12 "an ordinary message changes neither the task identity nor the approval state" 一致。
- `runTeammateTool`：plan gate 检查（bash/write_file/edit_file 在 gate ∉ {not_required, approved} 或 approved 版本不匹配时返回 `Blocked: plan status is X.`）→ **非交互权限检查**（硬拒/破坏性规则命中直接返回错误文本，不调 askUser——teammate 线程禁止读 stdin）→ 复用 `runTool`。
- teammate 工具集：bash / read_file / write_file / edit_file / glob / grep（cwd 按 assignment）+ list_tasks / claim_task / complete_task / get_task + send_message + submit_plan。**不含** task / spawn_teammate / create_worktree / update_task / create_task / compact / todo_write / load_skill。
- **`send_message` 是 Lead 与队友共用的同一个工具**（同一份 schema、同一个 handler），即对等通信的唯一入口：`to` 接受 `lead` 或任一存活队友名，**拒绝自己、未知名字、已 shutdown 的成员**；语义始终 `type=message`，队友**不能**借此发起 shutdown_request / request_plan / plan_approval_* 等控制协议（控制协议严格保持 Lead ↔ Teammate 单向握手，见 §11-12）。消息不做自动回执、不产生隐含回复语义，避免队友间 ping-pong 风暴。
- Lead 侧的 `send_message` 与队友侧走同一 handler，无特权差异——Lead 只是"恰好还能调用控制类工具"的成员。
- `consumeLeadInbox() string`：readInbox("lead") → `_response` 类型按 request_id 匹配 pendingRequests 更新状态（shutdown_response→approved）→ 渲染 `[Team events]` 文本（含 plan_approval_request 的 request_id 与 plan 正文）。
- Lead 工具处理器：spawn/send/shutdown_request/request_plan/respond_plan_request/list_teammates（见第 4 节 schema）。

### 2. 新文件 worktree.go（约 150 行）

- `taskWorktreeCWD(task *Task) (string, error)`：`Worktree` 为 nil → workdir（仓库目录）；否则 `<workdir>/.worktrees/<name>`，目录缺失 → error（**fail closed，不回退仓库目录**）。
- `createWorktree(name, taskID string) string`（Lead-only 工具）：名字 `^[a-z0-9][a-z0-9-]{0,63}$`；任务须 pending、无 owner、未绑定；`.worktrees/<name>` 与分支 `wt/<name>` 不得已存在；`git worktree add -b wt/<name> .worktrees/<name>`（cwd=workdir）成功后写任务绑定。git 报错但已留下分支/checkout → 报告 partial operation、任务保持未绑定、保留现场。
- `removeWorktree(name string, discard bool) string`（**host 专属，非模型工具**）：绑定任务 pending/in_progress 或存在当前轮租约 → 拒绝；工作区脏（tracked/untracked/ignored）且未 discard → 拒绝；否则 `git worktree remove`，保留 `wt/<name>` 分支，成功后清除任务绑定。
- main.go 增加 REPL 命令 `/remove-worktree <name> [--discard]` 调用 host helper。

### 3. 修改 taskstore.go

- `Task` 增加 `Worktree *string`（`json:"worktree,omitempty"`）与 `Result string`（`json:"result,omitempty"`）。
- `taskStore` 增加 `mu sync.Mutex`，守护 create / updateDependencies / claim / complete 的读-改-写窗口（进程内原子；本 harness 单进程，文件锁不做，README 的跨进程场景注释说明取舍）。
- `claim`：新增 **owner-in-progress 检查**（该 owner 已有 in_progress 任务 → 拒绝）与 **worktree 校验**（`taskWorktreeCWD` 报错 → 拒绝 claim，fail closed）。
- `complete`：签名加 `result string`，成功时写入 `t.Result`；assignment 租约的保留/释放由 team 层在 IDLE 时处理（complete 本身不清租约）。
- `list_tasks` 渲染：有 worktree 绑定时追加显示。

### 4. 修改 tools.go — baseDir 化 + 新工具 schema

**cwd 作用域改造**（teammate 的任务可能绑定 worktree）：
- `sessionState` 增加 `baseDir string`；`newSessionState` 默认 = `workdir`（Lead 行为完全不变）。
- `safePath(path)` 保留（签名不变，继续用包级 workdir，兼容既有测试）；新增 `st.resolvePath(path)` 按 `st.baseDir` 校验。
- `runRead/runWrite/runEdit/runGlob/runGrep` 与 `bashHandler` 全部改走 `st.baseDir`（editHandler/globHandler/grepHandler 现在忽略 st，改为使用）；`runBashIn(dir, command)` 变体供 bashHandler 使用（`cmd.Dir = dir`）；glob/grep 的 rel 路径相对 baseDir 计算。

**Lead 团队工具**（schema 加进 `allTools()`，handler 在 team.go 实现并注册进 `toolHandlers`——这些 handler 不回调 runTool，无初始化环，直接进 map 字面量）：

| 工具 | 参数 | 行为 |
|---|---|---|
| spawn_teammate | name, task_id, require_plan? | spawnTeammate；result 提示 Lead 结束本轮 |
| send_message ★ | to, content | **Lead 与队友共用**：校验 `to` ∈ {`lead`} ∪ 存活队友，拒绝自己/未知/已 shutdown；恒为 `type=message`，不支持控制协议 |
| shutdown_request | teammate | 生成 `req_`+8hex，pendingRequests 登记，发 typed 消息（**Lead-only**） |
| request_plan | teammate | gate→pending，发 plan_request（**Lead-only**） |
| respond_plan_request | request_id, approve, feedback | 匹配 pending 请求 → gate=approved/rejected（记录 version）→ 发 plan_approval_response（**Lead-only**） |
| create_worktree | name, task_id | 见 worktree.go（**Lead-only**） |
| list_teammates | — | 成员名 + working/idle + 当前任务（**Lead-only**） |

★ = 同时在 `teammateTools()` 中注册；其余六项只出现在 Lead 的工具集里，队友即使伪造工具名也会被 `runTeammateTool` 的白名单拒绝。

### 5. 修改 main.go — REPL 双通道（并发模型详设）

**代码骨架**：

```go
stdinCh := make(chan string, 1)      // bufio.Scanner 移入 goroutine，EOF 时 close
teamEvents := make(chan struct{}, 1) // bus 通知 → 非阻塞投递
for {
    select {
    case query := <-stdinCh:  // 现有流程原样保留
    case <-teamEvents:
        if events := consumeLeadInbox(); events != "" {
            history = append(history, user "[Team events]\n"+events)
            history = agentLoop(ctx, client, history, events) // 开启新一轮 Lead turn
            打印最终 text
        }
    }
}
```

**5.1 Goroutine 清单**（teammate 之外每类至多一个实例）：

| goroutine | 职责 | 独占状态 |
|---|---|---|
| 主 goroutine | select 循环；**无论哪个分支触发，agentLoop 都在主 goroutine 串行执行** | history（无并发写） |
| stdin reader | 唯一的 stdin 读取方；`Scan()` 读完整行后再发 stdinCh（cap 1），EOF 时 close | 无（行先读后发，阻塞 send 只延迟不丢失） |
| mailbox watcher | 循环 `bus.waitMessages("lead", timeout)`（只 peek 不消费）；非空则非阻塞投 teamEvents。**只盯 lead 邮箱**——队友邮箱由各自 runtime 的 `waitMessages(name, idleScanInterval)` 盯守 | 无 |
| teammate × N | 独立 sessionState / messages / baseDir；通过 bus 与 lead **及其他队友**双向通信 | 各自的会话状态 |
| SIGINT handler（已有） | 退出时追加 `team.stopAll()` | 无 |

**5.2 唤醒链路（事件驱动，无轮询，无丢唤醒）**：

- `bus.notifiers`：`map[string]chan struct{}`（cap 1，脏标记语义，随 bus 创建常驻注册）。
- `send()`：bus.mu 下追加 JSONL → 对目标 notifier **非阻塞**发送 token。
- `waitMessages(name, timeout)`：bus.mu 下检查邮箱文件 → 非空直接返回；空则 `select{<-notifier（消费 token）| timer | stopCh}` → 醒来后回到 mu 下复查文件并循环。token 兜底"检查与等待之间"的竞态窗口 —— 这是 channel 版 condition variable，数学上无丢唤醒。
- watcher peek 到非空 → `teamEvents` 非阻塞投递；已有 token 则丢弃：**一个 token 只表示"可能有新事件"**，主循环 `consumeLeadInbox` 一次排空全部积压消息；Lead turn 期间新到的事件由 watcher 再次投 token。净效果：事件不丢、至多一次冗余唤醒。
- Lead turn 忙时事件到达：token 停留在 teamEvents 缓冲，turn 结束回到 select 即刻消费。

**5.3 单消费者与所有权不变式**：

- 邮箱消费权：`lead` 邮箱 → 主 goroutine 的 `consumeLeadInbox`；`<teammate>` 邮箱 → **它自己的 runtime** 的 `tm.handleInbox`（IDLE 的 `waitMessages` 只 peek，唤醒后由 `handleInbox` 读+删）。配偶原则：一个邮箱在任意时刻只有一个消费者，读+删在 bus.mu 内原子完成。
- 对等通信的唤醒路径完全复用同一套机制：A 给 B 发消息 → B 的 `notifiers[B]` 被 notify → B 若在 IDLE 则 `waitMessages` 立刻返回并回 WORK；B 若在 WORK 则消息落盘等待，B 下一轮迭代的 timeout=0 检查取走。**队友消息不需要 Lead 中转或知晓**。
- history 由主 goroutine 独占 → `[Team events]` 聚合为**单条 user 消息**注入（对齐 `<task_notification>` 惯例，避免无 tool_use 的 tool_result 400）。队友的 messages 由其自身 runtime 独占，同一规则——`[Message from X]` 同样聚合成单条 user 消息。
- Lead 对等消息的可见性来自 Task board（owner/状态/result）+ 队友主动上报的 result，而非偷看队友邮箱（不做 cc/嗅探，保持每箱单消费者）。

**5.4 锁清单**（全部叶锁：临界区不嵌套获取、不跨 LLM 调用持有，handler 只做内存/小文件操作）：

| 锁 | 保护范围 |
|---|---|
| bus.mu | 邮箱文件 append / read+delete、notifiers map |
| teamRegistry.mu | 成员表 |
| pendingRequests.mu | 协议请求表 |
| planGates.mu | 计划闸门状态 |
| assignments.mu | 认领租约 |
| taskStore.mu | claim / complete 的读-改-写 |

**5.5 关闭次序**：q / EOF / SIGINT → `team.stopAll()`（close 各 tm.stopCh → IDLE 的 select 与 WORK 每轮开头检查退出；WaitGroup 等待 + 超时兜底，因 LLM 调用可能阻塞）→ 现有 `backgroundTasks.stopAll()` → 进程退出。banner/prompt 改为 `s13`。

**5.6 select 同时就绪**：Go 随机选分支；两个分支都是合法的 Lead 触发器，各自注入各自的 user 消息后串行执行 turn，顺序不影响正确性。

**5.7 多条消息同时到达的处理**（"多个消息同时进来"是本节的核心不变式）：

分四段，每段各自消除一类竞态：

1. **写入侧——串行化**：任意数量的 `send`（多队友、多来源、甚至 led 与队友同时）都在 `bus.mu` 内串行执行 `O_APPEND`，因此 JSONL 里的顺序 = 落盘顺序，字节不会交错损坏。并发写同一邮箱是安全的，无需调用方协调。
2. **唤醒侧——合并**：notifier 是 cap 1 的**脏标记**，N 条消息只投递至多 1 个 token，多余 token 直接丢弃。所以"同时进来 100 条消息"对唤醒层等价于"1 个信号"——唤醒语义不是"每条消息一次通知"，而是"邮箱非空"。丢 token 不丢消息：等待方醒来后必到 `bus.mu` 下复查文件。
3. **消费侧——一次排空**：`readInbox` 在 `bus.mu` 内执行 `read + delete`，**一次就拿走自上次消费以来的整批消息**。因为读与删在同一临界区内完成，任何 append 要么在这之前（已包含在本次批次）、要么在这之后（留给下次），不存在"读一半时被追加导致丢件"的窗口。每次消费都是一次原子批次交付，不丢、不重。
4. **注入侧——聚合成单条**：整批消息渲染成**一条** user 消息（Lead 侧 `[Team events]`，队友侧逐条 `[Message from X]` 但合并进同一条 user 消息），而不是按条数膨胀成 N 条 user 消息。理由有二：保持 README "一次 turn 一次事件注入" 的语义；避免同一批次在消息数组里产生多个连续 user 消息造成的中间态。

批次内部的顺序与冲突规则：

- **按落盘顺序处理**，不按消息条数做优先级插队。
- **控制协议幂等**：`shutdown_response` / `plan_approval_response` 以 `request_id` 关联 `pendingRequests` 并检查 `status`，重复或乱序到达的响应只应用一次——所以同一批里两条针对同一 request_id 的响应，最终状态唯一且与顺序无关。
- **同批含 `shutdown_request`**：本批全部消息仍被处理（已投递内容不丢），但置 stop 标志，处理完本批后退出，不再开启新的 WORK。
- **同批控制消息 + 普通队友消息混排**：普通消息只注入对话、不碰 workVersion / task 归属 / plan gate，因此控制状态与对话内容互不干扰。

两个观察时机（其余语义完全相同）：

- **IDLE 期**：`bus.waitMessages(name, idleScanInterval)` 阻塞等待，被唤醒时整批 `readInbox` → 决定 stop（shutdown）或回 WORK（任意来源的普通消息）；无消息才去看任务板。
- **WORK 期**：每轮**模型调用之前**以 timeout=0 非阻塞取批，在轮边界注入；当前工具批次跑完继续，不中途打断正在执行的工具。因此 WORK 期间到达的消息最迟在下一轮模型调用时生效。

Lead 侧同理：watcher 只 peek（不消费）→ 非阻塞投 1 个 teamEvents token → 主 goroutine 的 `consumeLeadInbox` 一次排空整批并渲染成单条 `[Team events]`；Lead 正在跑 turn 时消息继续累积，turn 结束回到 select 时一次性取走。**peek 与 consume 都在 `bus.mu` 内原子完成**，所以 watcher 与主 goroutine 分属两个 goroutine 也不会出现"读到一半被删"。

### 6. 修改 llm.go — system prompt

追加团队指引（对齐 README §1）：

```text
When parallel work would help, first propose a small team with clear responsibilities
and wait for the user's confirmation. Do not call spawn_teammate before the user confirms.
After spawning teammates, end your turn — results, idle notifications, and plan requests
are delivered to you automatically as [Team events] messages in later turns.
```

`teammateSystemPrompt`（team.go 内的常量）除任务上下文外，需显式说明对等通信能力与边界：

```text
You are teammate "<name>". Work on your claimed task in <cwd>.
You may coordinate directly with other teammates via send_message(to, content);
incoming messages arrive as [Message from <name>] and may adjust your approach,
but they never change your task ownership or lift the plan gate.
Report results to Lead by finishing your turn; do not message Lead just to acknowledge.
```

### 7. 其他

- llm.go 无需为 teammate 改动（teammate 用自己的 prompt 常量，放 team.go）。
- `.gitignore` 增加 `.mailboxes/`、`.worktrees/`（`.tasks/` 若已忽略则不动）。
- 日志：teammate 内层 loop 不写 input/agent 日志（appendLog 全量重写文件，多 goroutine 会互相覆盖；lead 日志保持现状）。

## Assumptions & Decisions

1. **Teammate 用简化内层 loop**（对齐参考实现）：不带 compact/记忆/todo reminder/后台 bash；LLM 失败 → 结束 WORK 并把错误作为 result 上报。
2. **WORK 每轮开头非阻塞拉 inbox**：比参考实现（只在 IDLE 检查）更及时地解除 plan gate 阻塞、接收 lead 追加指令；result/idle 事件仍只在 WORK 结束时发送，语义不变。
3. **进程内互斥锁代替文件锁**：单进程 harness 内原子性足够；跨进程文件锁（flock）不在本次范围，注释说明。
4. **plan gate 的 approved 与 workVersion 绑定**：claim/release 使 version+1，旧 approval 失效回退为 required 行为；普通消息不影响。
5. **remove_worktree 不做成模型工具**：按 README §10 由 host（REPL 命令）持有，需显式 `--discard` 才能丢弃脏工作区；`wt/<name>` 分支始终保留。
6. **teammate 权限检查非交互化**：硬拒清单/破坏性规则命中 → 直接返回权限错误文本（Lead 可与用户协商），绝不调 askUser。
7. **Lead 保留全部现有工具**（含 task 子代理、compact 等），团队工具是增量。
8. 事件投递只在 Lead turn 之间发生（对齐参考实现：消费邮箱后再开启下一轮）。
9. **队友间对等通信是相对 README 的有意扩展**：README §9 只给队友"task + file tools"，本实现额外把 `send_message` 暴露给队友（同一份 schema/handler），并在队友侧新增 `tm.handleInbox` 的任意来源分发。范围严格限定为**点对点单播普通消息**（`type=message`）：
   - 不做广播/群组（`:to="*"` 不实现）；
   - 不做队友间控制协议（shutdown/plan 三类 typed 协议仍严格 Lead ↔ Teammate 单向握手，队友即使伪造工具名也会被工具白名单挡下）；
   - 不做 cc/嗅探/消息中转，Lead 通过 Task board + result 上报获知协作进展。
10. **对等消息不改变协作状态**：不修改 workVersion、task owner/status、plan gate；队友间消息仅作为对话输入注入（`[Message from X]`），因此不会绕过 plan gate 或伪造任务归属。
11. **不引入自动回执/回复语义**：消息投递是单向事实，是否回复由模型自行决定，避免队友间无界 ping-pong。

## Verification

1. `go build ./... && go vet ./...`
2. `go test ./...` —— 全部通过，含新增 team_test.go / worktree_test.go：
   - bus：send/readInbox 消费即删、wait 超时、并发 append 不丢；
   - spawn：保留名/重名/无效任务拒绝、claim 失败不启动 goroutine；
   - claim 原子性：N 个 goroutine 并发 claim 同一任务恰好 1 个成功；owner-in-progress 拒绝；
   - worktree：创建+绑定（真实 git 仓库于 t.TempDir）、分支已存在时 partial 报告且不绑定、remove 对 pending/in_progress/脏工作区拒绝、--discard 清目录留分支；
   - plan gate：required 时 bash/write/edit 返回 Blocked、read 放行；submit_plan → respond(approve) 后放行；workVersion 变化使旧 approval 失效；
   - shutdown 握手：request → response → pendingRequests 状态 approved → goroutine 退出；
   - result + idle 两条独立事件、auto-claim、consumeLeadInbox 渲染与 request_id 匹配、assignment fail-closed、teammate 工具 cwd 落在 worktree。
   - **对等通信（本项为扩展验收）**：
     - B 处于 IDLE 时 A 发消息 → `notifiers[B]` 唤醒 `waitMessages`，B 转入 WORK 并在 messages 里出现 `[Message from a] …`；
     - B 处于 WORK 中 A 发消息 → 不打断当前迭代，但 B 下一次迭代开头（timeout=0 检查）取走该消息；
     - `send_message` 目标校验：自己 / 未知名字 / 已 shutdown 成员 → 返回错误，不落盘；
     - 队友侧白名单：队友调用 `shutdown_request` / `request_plan` / `create_worktree` / `update_task` → 被 `runTeammateTool` 拒绝；
     - 对等消息不动 workVersion / gate：B 在 plan gate=required 时收到队友消息，仍不能执行 bash/write/edit；
     - A 与 B 互相发送 N 条消息后，各自邮箱读+删后为空（无泄漏、无重复消费）。
   - 既有测试适配：taskstore_test.go（新字段/新检查）、tools_test.go（baseDir 默认值不改变行为，预期零改动）。
3. loop 级测试沿用项目惯例：setupLogPaths + setMemoryEnabled(false) + 等待型 mock client + 按内容扫描断言（避免消息下标竞态）。
4. 手动冒烟：真实 API 下跑 README "One Complete Run" 剧本——提案 → "Go ahead" → spawn → worktree 认领 → result/idle 事件唤醒 Lead → shutdown_request 收尾；`.tasks/`、`.mailboxes/`、`.worktrees/` 状态逐一核对；`/remove-worktree` 拒绝/放行路径。
