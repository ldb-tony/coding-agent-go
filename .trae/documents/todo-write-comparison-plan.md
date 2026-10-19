# TodoWrite vs Claude Code Task 系统对比文档实施计划

## Repository Research

### 我方当前实现（s05）

- **[todo.go](file:///Users/tony/coding-agent-go/todo.go)**：`todoManager` 单文件实现，挂 `sessionState.todo`；全量 replace 式更新，无 taskId，无依赖，无文件持久化
- **[tools.go#L112-L120](file:///Users/tony/coding-agent-go/tools.go#L112-L120)**：注册 `todoWriteHandler`
- **[agent.go#L70-L82](file:///Users/tony/coding-agent-go/agent.go#L70-L82)**：`roundsSinceTodo` reminder（连续 3 轮无 todo_write 注入 text block）
- **schema**：输入 `{ todos: [{ content, status }] }`；输出多行字符串 `[x]/[>]/[ ] content`；无 activeForm、无 deleted、无 service-side ID

### Claude Code Task 系统官方原文（2026-10-19 访问）

**tools-reference 表格原文**（v2.1.268+）：

> **TaskCreate**：*"Creates a new task in the task list."*
> **TaskGet**：*"Retrieves full details for a specific task."*
> **TaskList**：*"Lists all tasks with their current status."*
> **TaskUpdate**：*"Updates task status, dependencies, details, or deletes tasks."*
> **TodoWrite**：*"Manages the session task checklist. Disabled by default in favor of `TaskCreate`, `TaskGet`, `TaskList`, and `TaskUpdate`. Set `CLAUDE_CODE_ENABLE_TASKS=0` to re-enable it..."*

**SDK TodoWriteInput 原文**：`{ todos: [{ content, status: "pending"|"in_progress"|"completed", activeForm }] }`

### TodoWrite 的结构性问题（Anthropic 迁移动机）

来自 ClaudeArchitect 深度分析（"Why Anthropic Made This Change" 章节），这些是 TodoWrite 无法覆盖、必须被 Task 系统替代的根本问题：

| 问题 | TodoWrite 旧设计 | 我们 s05 现状 | Task 系统解决 |
|---|---|---|---|
| **状态不持久** | In-memory only，session 结束全部消失 | 同样是内存，agentLoop 结束即丢弃 | File-based (`.claude/tasks/`)，跨 session 保留 |
| **无依赖** | 不能表达 "B depends on A" | 无 | Full dependency graph |
| **无 blocker** | 不能标记任务"被什么卡住了" | 无 | Blockers 支持 + reasons |
| **无优先级** | 全部 todo 平等 | 无 | Priority levels |
| **无增量更新** | 全量 replace（传完整列表） | 同样全量 replace | TaskUpdate 按 taskId 增量修改 |
| **无查询能力** | 只有写，不能单独读一个 task | 无 | TaskList（拉全量）、TaskGet（拉单个） |
| **无服务端 ID** | 模型自己维护 item 位置 | 无 | TaskCreate 返回 taskId，后续调用按 ID 操作 |
| **无 cross-session 协作** | 多个 Claude 实例共享任务不可能 | 单 agentLoop | 原生支持 + 广播通知 |
| **activeForm 缺失** | legacy TodoWrite 就有，我们没实现 | 无 | 有，进行中时显示动态描述 |
| **无 deleted 状态** | 只有 pending/in_progress/completed | 同样 3 态 | 4 态 + TaskUpdate 可标记删除 |

ClaudeArchitect 原文：
> *"The old Todo system served its purpose well for simple checklists within a single session. But as Claude Code matured and projects grew more ambitious, limitations became apparent. Tasks that needed to survive session restarts vanished. Dependencies between work items couldn't be expressed. Collaboration across multiple Claude instances was impossible."*
>
> *"The evolution from Todos to Tasks wasn't arbitrary. It reflects fundamental changes in how Claude Code is being used and where it's heading."*

Claude Media（日语）补充：Claude Code v2.1.233 起，Opus 4.8/Sonnet 5/Fable 5/Mythos 5 等新世代模型**连 Task 工具都默认不提供**——模型原生多步跟踪足以覆盖简单场景，显式 task 系统成了复杂项目才 opt-in 的能力。

## Files and Modules

- **新建 `docs/tool-comparison/07-todo_write-vs-TaskSystem.md`**：正式对比文档，10 节结构，Claude Code 官方原文 blockquote 引用
- **更新 `docs/tool-comparison/README.md`**：索引表新增第 7 行

## Implementation Steps

1. 撰写 `07-todo_write-vs-TaskSystem.md`（10 节）：

   | § | 标题 | 核心内容 |
   |---|---|---|
   | 1 | 背景：v2.1.268 为什么放弃 TodoWrite | 官方 tools-reference 原文 quote（"Disabled by default in favor of..."）；版本分水岭；Opus 4.8 连 Task 工具都默认不提供的后续演进 |
   | 2 | Task 系统是什么 | 四个 tools-reference 原文 quote + SDK TodoWriteInput type；Task 与 TodoWrite 共存关系；TaskStop 是 background task 工具（附带但非 todo 管理核心） |
   | 3 | **核心问题：TodoWrite 为什么不行（来自官方/深度分析）** | **ClaudeArchitect 的 10 条结构性问题表格**（上面 Research 表），每条标注"我们 s05 现状"与"Task 系统解决"；ClaudeArchitect 的两段引文 blockquote |
   | 4 | Schema 逐项对比（我们 vs CC legacy vs CC Task） | content vs subject、status 枚举（我们只有 3 态 vs Task 有 deleted）、activeForm（legacy/Task 有，我们没有）、全量 replace vs TaskUpdate 增量、服务端 taskId（我们没有）、输出自由文本 vs 结构化 message+stats |
   | 5 | 执行机制 | 我们的 in_memory replace + 多 in_progress 降级 vs CC TaskCreate 返回 taskId → TaskUpdate 按 ID 增量/删除；文件持久化 vs 内存 |
   | 6 | Reminder vs 原生跟踪 | 我们 agentLoop roundsSinceTodo 硬注入 vs CC 旧模型显式调 Task 工具 vs CC 新模型（Opus 4.8+）原生不提供 Task 工具——模型自己会跟踪多步 |
   | 7 | 输出契约与 SDK 集成 | 我们只有字符串；CC 有结构化 stats、SDK TaskTracker 实时渲染（todo-tracking 文档代码示例片段） |
   | 8 | 量化推演 | 5 步任务：我们的全量 replace 每次传完整列表（假设第 5 步时每次 50 token × 4 次 = 200 token 浪费） vs TaskUpdate 增量每次 ~10 token；reminder 注入开销 |
   | 9 | 差距评级与对齐草案 | 差距从大到小：① 无服务端 taskId（最大）→ ② 无增量更新 → ③ 无 deleted → ④ 无 activeForm → ⑤ 无文件持久化 → ⑥ 无查询工具。对齐分阶段 |
   | 10 | 参考资料 | tools-reference、todo-tracking、SDK TypeScript reference、ClaudeArchitect Tasks vs Todos、Claude Media deprecation（日语），注明访问时间 2026-10-19 |

2. 更新 README.md：索引表增加第 7 行

## Dependencies and Considerations

- **官方原文必须 verbatim blockquote**：tools-reference 表格行、todo-tracking 段落、SDK type 定义，全部用 `>` 标注并给出来源 URL + 访问时间
- **结构性问题表格是本文核心**：10 条，每条标注三方对比（我们 vs legacy TodoWrite vs Task 系统），这条信息决定了对齐优先级
- ClaudeArchitect 和 Claude Media 是**深度社区分析**，不是官方文档；本文需标注它们的来源性质（"深度分析" vs "官方文档"）
- CC 有两次演进：① TodoWrite → Task 系统（v2.1.268）② Task 工具默认不提供给新模型（v2.1.233+ Opus 4.8+）；文档需明确时间线顺序

## Validation

- 所有 Claude Code 官方引用用 blockquote + 出处链接
- 结构性问题表格覆盖全部 10 条、每条有三方对比
- 文档 10 节结构与前 6 份对齐

## Risks

- ClaudeArchitect 不是 Anthropic 官方文档，具体功能（dependencies、blockers、priority）需注意可能有超前描述；处理方式：明确标注来源性质
- Claude Media 是日语，翻译可能有偏差；关键术语用英文原文
- CC 快速迭代：v2.1.277 已移除 TaskOutput 工具，文档需注意 TaskStop 和 Task 四工具的区分
