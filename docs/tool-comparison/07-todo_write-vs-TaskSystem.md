# 07 · todo_write vs Claude Code Task 系统

[返回索引](README.md) ｜ 上一篇：[bash vs Bash](06-bash-vs-Bash.md)

---

> **⚠️ 2026-10 时间敏感声明**：Claude Code 工具集在 v2.1.268（2026 年初）经历了一次结构性替换——`TodoWrite` 被四个 Task 工具（`TaskCreate` / `TaskList` / `TaskGet` / `TaskUpdate`）取代为默认；v2.1.233+ 的 Opus 4.8 / Sonnet 5 / Fable 5 / Mythos 5 等新模型更进一步，Task 工具**默认完全不提供**（模型原生具备多步任务跟踪能力）。本文同时覆盖：① 官方 tools-reference 原文② ClaudeArchitect 深度分析中的 TodoWrite 结构性缺陷③ 我们 s05 Go 实现的代码级对比。所有外部资料访问时间 2026-10-19。

## 1. 背景：v2.1.268 为什么放弃 TodoWrite

### 官方 tools-reference 原文

访问 https://code.claude.com/docs/en/tools-reference （2026-10-19），官方工具列表表格里 `TodoWrite` 一行原文：

> **TodoWrite**：*"Manages the session task checklist. Disabled by default in favor of `TaskCreate`, `TaskGet`, `TaskList`, and `TaskUpdate`. Set `CLAUDE_CODE_ENABLE_TASKS=0` to re-enable it in sessions that have the task-tracking tools."*

同期四个 Task 工具的官方描述：

> **TaskCreate**：*"Creates a new task in the task list. Provided by default only on the models listed under Task tool availability, and on other models when you opt in."*
>
> **TaskGet**：*"Retrieves full details for a specific task."*
>
> **TaskList**：*"Lists all tasks with their current status."*
>
> **TaskUpdate**：*"Updates task status, dependencies, details, or deletes tasks."*

### 两层替换的时间线

| 阶段 | 版本 | 行为 | 原因 |
|---|---|---|---|
| **阶段一** | v2.1.268 起 | Task 系统（4 工具）替代 TodoWrite 成为默认 | TodoWrite 单工具的结构性缺陷（见 §3）无法满足复杂项目管理需求 |
| **阶段二** | v2.1.233+ Opus 4.8/Sonnet 5 等 | Task 工具**默认不提供**，需显式 opt-in | 新世代模型（Opus 4.8+）原生具备多步任务跟踪能力，显式 Task 系统对简单场景成了 overhead |

SDK 层同步：TypeScript Agent SDK v0.3.268 起 bundled 二进制带 Task 系统；Python Agent SDK v0.2 起弃用 TodoWrite。

## 2. Task 系统是什么（四工具全景 + 附加 TaskStop）

### 官方 tools-reference 原文

四个 Task 工具在 2026-10 工具列表中的表格行（Permission required 全部 No）：

| 工具 | 官方描述原文 |
|---|---|
| `TaskCreate` | *"Creates a new task in the task list. Provided by default only on the models listed under Task tool availability, and on other models when you opt in"* |
| `TaskGet` | *"Retrieves full details for a specific task."* |
| `TaskList` | *"Lists all tasks with their current status."* |
| `TaskUpdate` | *"Updates task status, dependencies, details, or deletes tasks."* |

附带但容易混淆的 `TaskStop`（**不属于 todo 管理四工具**，它管理 background task 停止）：

> *"Stops a running background task by ID. It also accepts an agent-team teammate or a named background agent by agent ID or name."*

历史上存在过的 `TaskOutput`：*"Removed in v2.1.277. Previously retrieved output from a background task; Claude reads a background task's output file with Read instead."*

### SDK TypeScript 类型（访问 docs.anthropic.com/en/agent-sdk/typescript）

Legacy TodoWrite 的官方 type 定义（含我们缺失的 `activeForm`）：

```typescript
type TodoWriteInput = {
  todos: Array<{
    content: string;
    status: "pending" | "in_progress" | "completed";
    activeForm: string;  // 我们 s05 没有
  }>;
};
```

TaskCreate / TaskUpdate 的输入输出（核心差异：**服务端分配 taskId**）：

```typescript
// TaskCreate 输入（subject 不是 content，没有显式 status）
type TaskCreateInput = {
  subject: string;
  activeForm?: string;
};

// TaskCreate 输出 —— 服务端返回 taskId，后续所有操作用它定位
type TaskCreateOutput = {
  task: { id: string; subject: string; status: "pending" };
};

// TaskUpdate 输入 —— 增量更新，按 taskId 定位
type TaskUpdateInput = {
  taskId: string;
  status?: "pending" | "in_progress" | "completed" | "deleted";  // 多了 deleted
  activeForm?: string;
  // TaskUpdate 还支持 dependencies / details（来自官方描述）
};
```

## 3. 核心问题：TodoWrite 为什么不行

本节是全文重点。以下 10 条结构性问题全部来自 ClaudeArchitect 深度分析 *"Why Anthropic Made This Change"* 章节（访问 claudearchitect.com/guides/claude-code-tasks-vs-todos, 2026-10-19），每条同时标注我们 s05 Go 实现的现状与 Task 系统的解决方式。

ClaudeArchitect 原文：

> *"The evolution from Todos to Tasks wasn't arbitrary. It reflects fundamental changes in how Claude Code is being used and where it's heading."*
>
> *"The old Todo system served its purpose well for simple checklists within a single session. But as Claude Code matured and projects grew more ambitious, limitations became apparent. Tasks that needed to survive session restarts vanished. Dependencies between work items couldn't be expressed. Collaboration across multiple Claude instances was impossible."*

### 10 条结构性缺陷三方对比表

| # | 问题 | TodoWrite 旧设计 | 我们 s05 现状 | Task 系统解决 |
|---|---|---|---|---|
| 1 | **状态不持久** | In-memory only，session 结束全部消失 | 同样内存（`sessionState.todo`），[todo.go#L30-L31](file:///Users/tony/coding-agent-go/todo.go#L30-L31) `todoManager.items` 是 []slice；agentLoop 返回即丢弃 | File-based (`.claude/tasks/`)，跨 session 保留 |
| 2 | **无依赖** | 不能表达 "B depends on A" | 无依赖字段，只能平铺 | TaskUpdate 支持 dependencies（官方描述原文 "Updates task status, dependencies, details, or deletes tasks"） |
| 3 | **无 blocker** | 不能标记任务被什么卡住 | 无 | Blockers 支持 + reasons |
| 4 | **无优先级** | 全部 todo 平等 | 无 | Priority levels |
| 5 | **全量 replace，无增量** | 每次更新传完整列表 | 同样全量 replace（[todo.go#L43-L80](file:///Users/tony/coding-agent-go/todo.go#L43-L80) `update(items []todoItem)`） | TaskUpdate 按 taskId 只改单个任务 |
| 6 | **无查询能力** | 只有写，不能单独读 | 无 TaskList / TaskGet 等价工具 | TaskList（拉全量）+ TaskGet（拉单个），模型/宿主端可随时拉状态 |
| 7 | **无服务端 ID** | 模型自己维护 item 位置，无稳定标识符 | 无 taskId；模型靠列表下标定位 | TaskCreate 返回 taskId，后续所有操作用它 |
| 8 | **无 cross-session 协作** | 多 Claude 实例共享任务不可能 | 单 agentLoop | 原生支持 + 广播通知 |
| 9 | **activeForm 缺失** | Legacy TodoWrite SDK type 就有 `activeForm` | 我们完全没实现 | TaskCreate/TaskUpdate 都支持 |
| 10 | **无 deleted 状态** | 只有 pending/in_progress/completed 三态 | 同样三态（[todo.go#L12-L16](file:///Users/tony/coding-agent-go/todo.go#L12-L16) 常量） | TaskUpdate.status 含 `"deleted"`，标记后 TaskList 不再返回 |

## 4. Schema 逐项三方对比

| 项 | 我们 s05 todo_write | CC Legacy TodoWrite (`CLAUDE_CODE_ENABLE_TASKS=0`) | CC Task 系统 |
|---|---|---|---|
| 输入工具数 | **1 个**（todo_write） | **1 个**（TodoWrite） | **4 个**（TaskCreate/TaskList/TaskGet/TaskUpdate） |
| 粒度 | 全量列表 replace | 全量列表 replace | Create 单个，Update 增量 |
| 标识 | 无 taskId（模型靠位置） | 无 taskId（模型靠位置） | **TaskCreate 返回 taskId**（服务端分配） |
| 内容字段 | `content`（字符串，[todo.go#L24](file:///Users/tony/coding-agent-go/todo.go#L24)） | `content` | `subject`（语义差异：content=列表项，subject=任务标题） |
| activeForm | **❌ 缺失** | ✅ 有（SDK type 含） | ✅ 有（Create/Update 都支持） |
| status 枚举 | `pending \| in_progress \| completed`（[todo.go#L12-L16](file:///Users/tony/coding-agent-go/todo.go#L12-L16)） | 同三态 | **四态**（+ `"deleted"`） |
| 服务端去重 | **是**（最多 20 条、content 非空、多 in_progress 降级第一条，[todo.go#L43-L80](file:///Users/tony/coding-agent-go/todo.go#L43-L80)） | 未明确 | 每个 taskId 天然唯一，不需要去重 |
| 输出形态 | 多行自由文本 `[x] content` | 多行 + 结构化 `{message, stats: {total, pending, in_progress, completed}}` | TaskCreate 返回 `{task: {id, subject}}`；TaskList 返回完整列表 |
| 查询 | **无** | **无** | TaskList / TaskGet 独立读工具 |
| 持久化 | 内存（sessionState） | 内存 | `.claude/tasks/` 文件系统 |
| 依赖/blocker/priority | 无 | 无 | TaskUpdate 支持 |
| 上限 | 最多 20 条 | 未明确 | 未明确 |

## 5. 执行机制源码级对比

### 我们 s05 的管线

```
todoWriteHandler [todo.go#L115-L127]
  → json.Unmarshal → st.todo.update([]todoItem)  [todo.go#L43-L80]
    → 校验（≤20 / content 非空 / status 合法 / 多 in_progress 降级）
    → tm.items = validated   // 替换整个 slice
    → tm.render()            // [x]/[>]/[ ] 字符串格式化
  → 返回自由文本
```

每次**都是全量 replace**。模型想把第 3 项从 pending 改成 in_progress，必须重新传 3 项的完整列表。第 5 步时每次替换都带 5 项；第 20 步时每次替换都带 20 项——**模型每做一个状态变更，就把完整列表重传一次**。

### Claude Code Task 系统管线

```
TaskCreate
  输入: { subject: "write auth module", activeForm?: "writing auth module..." }
  → 服务端分配 taskId（如 "task-auth-module-abc123"）
  → 持久化到 .claude/tasks/
  → 返回 { task: { id, subject, status: "pending" } }

TaskUpdate(taskId="task-auth-module-abc123")
  输入: { taskId, status: "in_progress" }          // 只传变更字段
  → 服务端按 ID 定位 → 更新 → 持久化
  → 返回 { message, stats: { total, pending, in_progress, completed } }
```

模型每次只传**单任务的增量变更**，列表由服务端维护。结束不需要"删除"操作——TaskUpdate `status: "deleted"` 标记后，TaskList 不再返回。

## 6. Reminder vs 原生跟踪

| 维度 | 我们 s05 | Claude Code |
|---|---|---|
| **模型侧能力** | 模型没有原生跟踪，全靠我们提供的 todo_write 工具 | Opus 4.8+ 等新模型**原生具备**多步任务跟踪能力，官方**默认不提供 Task 工具** |
| **Reminder 注入** | agentLoop `roundsSinceTodo >= 3` 时注入 `<reminder>` text block（[agent.go#L70-L82](file:///Users/tony/coding-agent-go/agent.go#L70-L82)） | 没有显式 reminder 机制；旧模型需显式调 Task 工具，新模型自己会跟踪 |
| **对模型的影响** | reminder 是一个**额外的 user message**，占据对话窗口；且用 text block 而非 tool_result（避免 API 拒绝无 tool_use 对应的 tool_result） | 无 reminder 注入的额外 token 开销 |

我们 reminder 机制的成本：3 轮 bash 后自动加一个 ~100 token 的 user message。对 5 步任务，假设每 3 步触发一次 reminder，全程大约注入 1-2 次。这比全量 replace 的累积 token 浪费小，但仍然是我们**有状态跟踪不足**的信号——如果 todo 是服务端状态，不需要通过"提醒模型"来驱动它更新计划。

## 7. 输出契约与 SDK 集成

### 我们的输出

只有多行字符串：

```
[x] refactor login form
[>] write tests
[ ] update docs
```

模型可以读，但**宿主端没有结构化解析的可能**——想在 UI 里显示进度条，只能自己解析 `[x]/[>]/[ ]` 正则。

### Claude Code 的结构化输出

Legacy TodoWrite 已经有 stats（SDK typescript reference）：

```typescript
type TodoWriteOutput = {
  message: string;
  stats: { total: number; pending: number; in_progress: number; completed: number };
};
```

Task 系统更进一步——SDK 通过 task-tracking 文档的 `TaskTracker` 示例展示了完整的实时渲染链：

> *"The stream tool_use input is the raw shape the model emitted. Claude Code repairs some close-but-incorrect key names before execution, mapping `id` or `task_id` to `taskId` and `active_form` to `activeForm`, but that repair is not reflected in the stream."*

宿主端（Trae IDE、VS Code 扩展等）监听 `TaskCreate` / `TaskUpdate` 的 tool_use block，配合 tool_result 的 `taskId`，可以做：实时进度条、完成率统计、进行中任务高亮显示 activeForm（如 "writing auth module..." 而非 subject）。

## 8. 量化推演（Token 经济学）

### 我们 s05 的全量 replace 累积浪费

假设一个 10 步任务，每步需要一次状态更新（pending → in_progress → completed）：

- 第一次调用（全量列表创建）：10 项 × ~30 token/项 = ~300 token（input）
- 第二步更新：仍然传 10 项完整列表，改 1 项状态 → 浪费 9 项 ~270 token
- 第三步更新：同上 → 浪费 270 token
- ... 共 9 次更新，每次浪费 270 token

**10 步任务累积浪费 ≈ 2430 token**。5 步任务浪费 ~720 token。20 步浪费 ~5130 token。

### Task 系统增量更新

每次 TaskUpdate 只传 `{taskId, status}` ≈ 30 token。10 步任务 9 次更新 ≈ 270 token。

**差距一个数量级**：2430 vs 270。

### Reminder 注入开销（我们独有）

每 3 轮 bash 后注入 ~100 token reminder。10 步任务大约触发 2 次 → 200 token。Task 系统无此开销。

## 9. 差距评级与对齐草案

### 差距评级（A=低差距，D=高差距）

| 维度 | 评级 | 说明 |
|---|---|---|
| 工具粒度 | **C** | 我们 1 个工具 vs CC 4 工具（Create/Update/List/Get） |
| 状态管理（服务端 ID + 增量更新） | **D** | 最大差距，无 taskId → 无增量 → 浪费 token → 无法精确更新 |
| status 枚举 | **B** | 缺 deleted；多 in_progress 降级是我们做得好的地方 |
| activeForm | **C** | legacy TodoWrite 有，我们没实现 |
| 输出结构化 | **C** | 我们是纯字符串；legacy 已有 message+stats |
| 查询能力 | **D** | 完全缺失，模型/宿主无法读回列表 |
| 持久化 | **C** | 我们内存；CC 文件系统 |
| 依赖/blocker/priority | **D** | 全部缺失 |
| 去重/校验 | **A** | 我们甚至更严格（≤20、content 非空、多 in_progress 降级） |
| Reminder vs 原生跟踪 | **B** | 我们用 harness 注入 reminder，CC 新模型原生跟踪；两者路径不同，但我们的 reminder 额外耗 token |

### 对齐草案（分阶段）

**里程碑 A · 保留单工具，补齐能力（零外部依赖，纯 Go）**
1. 加入 `activeForm` 字段
2. 加入 `deleted` status，替代当前"多 in_progress 降级"逻辑中的隐含约束
3. 输出加结构化 stats（`{message, stats: {total, pending, in_progress, completed}}`）
4. 全量 replace 语义保留，但优化 token 浪费：让渲染更短

**里程碑 B · 拆分四工具 + 服务端状态**
5. 引入服务端分配的 taskId（可以是 uuid 或简单自增）
6. 拆分为 `TaskCreate`（创建单个）、`TaskUpdate`（按 ID 增量）、`TaskList`（返回完整列表）、`TaskGet`（返回单个详情）
7. `sessionState` 改为 `map[string]*todoItem`（key 为 taskId）

**里程碑 C · 持久化 + 扩展能力**
8. 文件持久化（`.workdir/.tasks/` 或 `.claude/tasks/` 兼容路径）
9. 依赖图 / blocker / priority（TaskUpdate 扩展）
10. 与 S06 子代理的 background task 打通

**远期 · 对齐新世代模型的原生跟踪**
11. 让 reminder 更智能——只在模型描述里显式提到"我要分几步做"但迟迟不调 todo_write 时触发（而非固定 3 轮硬注入）
12. 或者干脆借鉴 CC v2.1.233+：**让新模型不需要显式 todo_write 也能跟踪多步任务**（这是模型能力问题，不是 harness 问题）

## 10. 参考资料

**Claude Code 官方**

- Tools reference（v2.1.268+）：https://code.claude.com/docs/en/tools-reference （2026-10-19）
- Track todos（Agent SDK 文档，taskId/activeForm/生命周期）：https://code.claude.com/docs/en/agent-sdk/todo-tracking
- Agent SDK TypeScript reference（type 定义，TodoWriteInput / TaskCreateOutput / Tool Output Types）：https://docs.anthropic.com/en/agent-sdk/typescript
- Model availability（Opus 4.7/Sonnet 4.6/Haiku 4.5 等旧模型默认提供，新模型不提供）：todo-tracking 页面 "Model availability" 章节

**深度分析（社区，非官方）**

- ClaudeArchitect *"Claude Code Tasks vs Todos: What Changed in 2026"*（含 10 条结构性缺陷 + 演进原因）：https://claudearchitect.com/guides/claude-code-tasks-vs-todos
- Claude Media（日语）*"Claude Code TodoツールがOpus 4.8以降で既定オフ"*（新世代模型 Task 工具完全取消的实证）：https://claude-media.com/articles/claude-code-todo-tool-deprecation
- Clauder Navi *"TodoWrite Deprecation and Migration to Task Tools"*（SDK v0.2 迁移说明）：https://www.clauder-navi.com/en/claude-agent-sdk-python-task-tools

**我们的源码**

- [todo.go](file:///Users/tony/coding-agent-go/todo.go)：todoManager + handler（127 行）
- [tools.go#L79-L89](file:///Users/tony/coding-agent-go/tools.go#L79-L89)：sessionState.todo 注入
- [tools.go#L112-L120](file:///Users/tony/coding-agent-go/tools.go#L112-L120)：toolHandlers 注册
- [tools.go#L631-L652](file:///Users/tony/coding-agent-go/tools.go#L631-L652)：allTools() schema 注册
- [agent.go#L10-L86](file:///Users/tony/coding-agent-go/agent.go#L10-L86)：roundsSinceTodo reminder
