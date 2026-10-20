# Agent Teams 对比分析：s13 教程实现 vs Claude Code 官方实现

## 摘要

对比对象：

- **A = s13 Agent Teams**：learn-claude-code 教程的 Python 教学实现（见上传 README），单文件 harness，目标是让协调协议的每个状态迁移显式可见。
- **B = Claude Code 官方 agent teams**：Anthropic 生产工具中的实验特性（`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`，默认关闭，仅交互式会话）。以下 B 侧事实全部来自 2026-10 时点的官方文档（code.claude.com/docs/en/agent-teams、/sub-agents），已逐条核实。

一句话结论：**两者骨架同构**（Lead + 持久 teammate、文件邮箱、共享任务板文件锁认领、plan 审批门、shutdown 握手、无嵌套团队、Lead 固定），说明 A 的设计方向是对的；差异在**设计哲学的优先级**——A 优先协议显式性与 fail-closed（教学），B 优先降低协调摩擦与生产可用性（自动批准 plan、权限冒泡、停止后可唤醒恢复、官方明示的降级与限制）。A 缺的不是骨架而是"生产化层"：peer 消息拓扑的显式开放、用户直达 teammate、权限冒泡、transcript 持久化、注入防护、规模/成本护栏。

---

## 二、设计哲学差异（理解所有具体差异的钥匙）

| | A：s13 | B：Claude Code |
|---|---|---|
| 优化目标 | 让每个协议状态迁移**显式、可审计、可教** | 让多实例协作**低摩擦、可扩展、可降级** |
| 典型取舍 | spawn 前必须用户确认；plan 审批绑定 task+work_version；worktree 删除只能 host 手动；teammate 危险操作直接拒绝 | **spawn 不询问确认**（"Claude Code doesn't ask you to confirm the launch"）；plan 请求到达即由 lead 会话**自动批准**，摩擦转移到权限提示；team 目录会话结束自动清理；停止的 teammate 可被消息唤醒 |
| 失败姿态 | fail-closed：坏 worktree 绑定→拒绝 workspace 工具；无任务→工具不可用 | 优雅降级 + 明示限制：官方文档列出 9 条已知限制（任务状态滞后、shutdown 慢、/resume 不恢复 teammate 等） |

---

## 三、架构组件逐项对照

### 3.1 团队生成与组成

| | A：s13 | B：Claude Code |
|---|---|---|
| 触发方式 | Lead 先提议分工，**用户确认后**才能 `spawn_teammate`（system prompt 强约束） | Lead 调 Agent 工具并带 `name` 即生成 teammate，**不询问用户确认**；开启 teams 后 Lead 自主命名的 subagent 也会变成 teammate（可设 `0` 关闭）；fork 或显式 `isolation` 的调用仍是 subagent |
| Teammate 本质 | 同进程 daemon 线程，固定同一套工具与模型 | 完整独立 Claude Code 实例：独立 context window，自动加载 CLAUDE.md / MCP / skills；不继承 Lead 对话历史，只收到 spawn prompt |
| 个体配置 | 无。全体 teammate 同模型、同固定工具集 | 模型选择链：spawn prompt 点名 > subagent 定义的 `model` > `CLAUDE_CODE_SUBAGENT_MODEL` > Lead 模型；effort 继承 Lead 且 **spawn 时冻结**（`/model`、`/fast` 只作用于 Lead）；可引用 `.claude/agents/` 的 subagent 定义复用角色（tools/model/disallowedTools/effort/body 等部分字段生效） |
| 成员发现 | 保留名 `lead`/`agent`；无成员发现机制 | team config（`~/.claude/teams/{team}/config.json`）含 `members` 数组（名字+agent ID），**teammate 可读此文件互相发现** |
| 团队数量/嵌套 | 未限制团队数；teammate 无 spawn 工具 → 无嵌套 | 每会话**恰一个团队**；**无嵌套团队**（teammate 不能 spawn teammate）；Lead 终身固定不可转移 |

### 3.2 通信

| | A：s13 | B：Claude Code |
|---|---|---|
| 邮箱载体 | `.mailboxes/<name>.jsonl` 追加写 + Condition 唤醒 | `~/.claude/teams/{team}/inboxes/{agent}.json` **单个 JSON 文件**；读取时逐条校验，非法条目报错并移除；**"发送成功"以写入对端邮箱成功为准**，写失败则发送方收到错误 |
| 拓扑 | 事实上的**星型**：协议叙事只覆盖 teammate↔Lead；MessageBus 传输层支持任意 from/to 但未开放 | **网状**：任意 agent 按名字互发 SendMessage；用户也可直发任一 teammate；广播需逐人发送（双方都无 broadcast） |
| 投递模型 | runtime 消费 Lead 邮箱，注入下一轮 `[Team events]`；`check_inbox` 不是模型工具（推送非轮询） | 自动投递，Lead 无需轮询（同为推送） |
| 结果上报 | `result` 与 `idle_notification` **拆成两个事件**（"产出什么"与"能否接新活"是两个事实） | idle 通知自动发给 Lead 并**附最终答案**（一个通知承载两者）；API 错误失败也会通知 Lead 并附错误文本 |
| 消息信任 | 未定义 | 接收方被明确告知消息来自另一个 Claude 会话而非用户；**teammate 不能代用户批准权限、不能转发"已批准"声明、被拒操作不能转投他人绕过**；auto 模式下分类器**逐条审查 agent 间消息**（含 shutdown/plan 等结构化消息），被拦截的消息永不投递 |

### 3.3 任务共享

| | A：s13 | B：Claude Code |
|---|---|---|
| 存储 | `.tasks/{id}.json` | `~/.claude/tasks/{team}/`（会话结束后保留，受 `cleanupPeriodDays` 控制） |
| 状态机 | pending / in_progress / completed + blockedBy 依赖 | 同三态 + 依赖（pending 任务依赖未满足不可认领） |
| 认领并发 | 进程锁 + 文件锁双层原子 claim；每人同时只能一个 in_progress；坏 worktree 绑定使 claim fail-closed | 文件锁防竞争；Lead 显式分配或 teammate 完成后自认领 |
| 建图契约 | **两阶段**：先 create_task 拿运行时 ID，再 update_task 加依赖边（同一响应内兄弟工具调用无法引用彼此新生成 ID） | 文档未定义建图契约；**依赖解锁自动**（完成任务自动解锁下游，无需人工介入） |
| 结构变更权限 | 团队运行期间只有 Lead 持有 update_task | 未明确限制 |
| 已知短板 | — | 官方承认：**任务状态可能滞后**（teammate 忘标 completed 会阻塞下游，需人工或 Lead 催办） |

### 3.4 工作目录隔离

| | A：s13 | B：Claude Code |
|---|---|---|
| 隔离粒度 | **worktree 绑定进任务认领模型**：`Task.worktree` 可选，claim 时解析 cwd 写入租约，teammate 的 bash/read/write/edit/glob 全部走该目录 | 团队协作**不绑定 worktree**；官方最佳实践是**按文件划地盘**（每个 teammate 拥有不同文件集避免覆盖）；worktree 隔离是 subagent 级 frontmatter（`isolation: worktree`，无改动自动清理），agent-teams 文档把 worktree 定位为"人工并行会话"的替代方案 |
| 失败姿态 | 坏绑定 fail-closed（拒绝 workspace 工具，不回落仓库目录）；重启后 `assignment_cwd()` 可重建租约 | subagent 的 worktree 有工作目录与 git 重定向校验，目录被删则命令报错 |
| 删除纪律 | **模型无删除权限**；host 移除有拒绝矩阵（pending/running 拒绝、changed 需用户决策、保留 `wt/<name>` 分支） | subagent worktree **clean 时自动清理** |

### 3.5 控制协议

| | A：s13 | B：Claude Code |
|---|---|---|
| Shutdown | `shutdown_request(request_id)` → teammate 完成当前步骤 → `shutdown_response(request_id)`；ID 防错配、类型防串扰、状态防重放（重复 response 不二次生效）；teammate **只能接受** | Lead 发 shutdown 请求；teammate **可批准（优雅退出）或拒绝并附理由**；官方承认 **shutdown 可能慢**（要等当前请求/工具调用完成）；会话结束自动清理 team 目录 |
| Plan 门 | `required/pending/rejected` 三态阻塞 bash/write/edit；**审批绑定 task + work_version**，claim/release 任务使旧审批失效；Lead 人工审（plan_approval_response） | Lead 在 plan mode 下生成的 teammate 先只读规划；计划请求发给 Lead 后**由 lead 会话自动批准，Lead 不审**；批准后退出 plan mode，编辑/命令仍走正常权限提示 |
| 权限 | teammate 不读用户输入；危险操作**直接返回 permission error**，由 Lead 找用户（死路式拒绝） | teammate 继承 Lead 权限模式（`dontAsk` 除外）；**权限提示冒泡到 lead 会话由用户实时批准**（plan 批准是唯一例外，自动放行）；spawn 后可可改单个 teammate 的权限模式，spawn 时不可指定 |
| 质量门 hook | 无 | `TeammateIdle` / `TaskCreated` / `TaskCompleted` 三个钩子，exit code 2 可阻断并反馈（如"测试不过不许 idle"） |

### 3.6 生命周期与持久化

| | A：s13 | B：Claude Code |
|---|---|---|
| Teammate 上下文 | 线程内存，shutdown/进程退出即丢失；只有任务文件与租约可重建 | 对话可保存：in-process teammate 停止后被消息呼叫时，**同会话内带历史唤醒恢复**（项目级 agent 定义需目录信任才恢复）；但 `/resume`、`/rewind` **不恢复** in-process teammate（官方已知限制） |
| 失败恢复 | 未定义 | Lead/teammate 的消息可唤醒等待 API 重试的 teammate 立即重试；teammate 卡死可直发指令或 spawn 替补 |
| 清理 | worktree 保留待人工处置 | team config 目录会话结束自动删；task 目录按 `cleanupPeriodDays` 保留 |

### 3.7 可观测性与交互

| | A：s13 | B：Claude Code |
|---|---|---|
| 用户视野 | 终端打印 bus 事件流（claim/result/idle/wake） | agent 面板：↑/↓ 选 teammate、Enter 看 transcript 并直发消息、`x` 停止、Ctrl+T 任务列表；idle 行 30 秒后隐藏；>3 个 idle 折叠成一行；或 tmux/iTerm2 分屏每人一窗格 |
| 用户↔teammate | **无直连**，只能跟 Lead 说话 | 任意 teammate 可直达；查看 teammate 时普通文本发给该 teammate，内置命令仍归 Lead（`/compact` 等需确认） |

### 3.8 成本与护栏

| | A：s13 | B：Claude Code |
|---|---|---|
| 成本叙事 | 未讨论 | 官方明示：token 随 teammate 数**线性增长**，显著贵于单会话；建议 **3–5 个起步**、每人 5–6 个任务；任务粒度过小协调开销吞掉收益 |
| Prompt cache | 未讨论 | **in-process teammate 的请求落在主对话 cache TTL 桶之外**，默认只有 5 分钟缓存；`subagentPromptCacheTtl: 1h` 可延长但 1 小时 cache 写按更高费率计费——多实例并发的 cache 经济学是官方显式管理的维度 |
| 规模护栏 | 无 | subagent 描述合计 >15k token 启动告警；嵌套深度、并发等有环境变量上限（subagent 维度）；teammate 无硬上限但官方劝退规模化 |

---

## 四、A 缺失的功能（B 有而 A 没有，按影响排序）

1. **网状消息拓扑的显式开放**。A 的 MessageBus 传输层已支持任意寻址，但协议叙事把 teammate↔teammate 通道关掉了，跨 teammate 协调流量全挤 Lead 的上下文。B 的 peer 直发 + 成员发现（team config 的 members 数组可读）是一等公民。
2. **权限冒泡审批**。A 对 teammate 危险操作直接 permission error——teammate 停摆等 Lead 重新决策；B 把提示冒泡到 lead 会话，用户实时批准 teammate 不停摆。这是 A 的"死路式拒绝"与 B 的"挂起待批"之间的体验鸿沟。
3. **停止后可唤醒恢复**。A 的 teammate 上下文随线程消失；B 的 in-process teammate 停止后可被消息带历史唤醒（同会话内）。
4. **Shutdown 拒绝协商**。A 只会接受；B 可拒绝并附理由，Lead 需处理拒绝分支。
5. **用户直达 teammate 通道**。A 的用户被 Lead 垄断；B 的面板/分屏让用户可绕过 Lead 直接纠偏任一 teammate（配合"teammate 消息不算用户授权"的信任标注，直达不等于放权）。
6. **声明式 per-teammate 配置**。B 可按人指定模型/工具子集/effort（如 Haiku 跑测试、Opus 跑重构），A 全体同构无降本空间。
7. **团队事件 hooks**。B 的 TeammateIdle/TaskCreated/TaskCompleted（exit code 2 阻断）提供质量门扩展点；A 的协议内无 hook。
8. **规模/成本护栏与 cache 管理**。A 完全没有成本维度；B 有团队规模建议、token 线性增长警告、teammate 独立 cache TTL 桶与 `subagentPromptCacheTtl` 旋钮。
9. **消息信任标注与审查**。B 标记"此消息来自 Claude 会话非用户" + auto 模式分类器逐条审；A 把 teammate 事件原样注入 Lead 上下文，无防注入设计。
10. **邮箱健壮性语义**。B 定义了读时校验/非法条目剔除/发送=写入成功；A 的 JSONL 追加模型没有损坏条目处理。

## 五、A 更严格或独有的设计（B 没有或未覆盖）

1. **用户确认的执行边界**：A 把"提议→用户确认→才 spawn"写进协议；B 明确不确认启动。对教学场景，A 的边界更显式。
2. **类型化协议状态机**：request_id 关联 + 类型防串扰 + 状态防重放；B 文档只描述交互流程，未公开协议内部（结构化消息存在但状态机不可见）。
3. **Plan 审批绑定 work_version**：任务变更使旧审批失效，防"批了 A 任务的计划改 B 任务"；B 的 plan 是自动批准，无版本绑定概念（B 把防线放在权限提示而非 plan 内容）。
4. **两阶段建图契约**：针对"同响应内工具调用无法互引新 ID"的明确契约；B 文档未讨论。
5. **原子 claim 完整约束集**：双层锁 + 单人单 in_progress + fail-closed worktree 绑定；B 只有文件锁。
6. **IDLE 优先级纪律**：邮箱消息严格优先于任务板扫描，防机会主义领任务冲掉控制消息；B 未描述此顺序。
7. **Worktree 删除纪律**：模型零删除权限 + host 拒绝矩阵；B 的 clean 自动清理更省事但边界更松。
8. **result / idle 事件拆分**：两个事实两个事件；B 合并进 idle 通知（但 B 多了 API 错误失败通知，A 没有）。
9. **worktree 与任务认领的集成**：A 把目录隔离做成认领模型的一等属性（任务选人、人选目录）；B 的团队靠文件划地盘，worktree 是 subagent 维度的独立特性。

## 六、B 官方承认的已知限制（对比的另一半——B 并非全面领先）

- `/resume`、`/rewind` 不恢复 in-process teammate；恢复会话后 Lead 可能给已消失的 teammate 发消息。
- **任务状态滞后**：teammate 忘标 completed 会阻塞下游任务，需人工介入。
- **Shutdown 慢**：teammate 要完成当前请求/工具调用才退出。
- 每会话恰一个团队；无嵌套团队；Lead 不可转移。
- in-process teammate 的 subagent 只能前台跑（后台工作不能活过 Lead 进程）。
- 权限模式只能 spawn 后改，不能 spawn 时按人指定。
- 分屏依赖 tmux/iTerm2（VS Code 集成终端、Windows Terminal、Ghostty 不支持）。

## 七、A 的可增强项清单（按价值/成本分级）

**高价值 / 低成本**
- E1 开放 teammate↔teammate 点对点消息 + 成员发现（bus 已支持任意寻址，补协议叙事与 Lead 抄送约定）。
- E2 Shutdown 拒绝路径（response 加 `approve: bool + reason`，Lead 处理拒绝分支）。
- E3 团队规模护栏：spawn 上限 + Lead system prompt 写入成本提示（token 随人数线性增长）。

**高价值 / 中成本**
- E4 权限冒泡：teammate 危险操作挂起 loop，审批请求经 MessageBus 路由到 Lead/用户，批准后续跑（替代直接拒绝）。
- E5 Teammate transcript 落盘 + 停止后可被消息带历史唤醒（A 已有任务级租约重建 `assignment_cwd()`，可扩展到消息历史）。
- E6 消息信任标注与注入防护：teammate 事件注入 Lead 上下文前标注来源并中和仿冒系统标记。

**中价值 / 中成本**
- E7 声明式 per-teammate 配置（model / 工具子集 / maxTurns），支持异构团队降本。
- E8 团队事件 hooks（TeammateIdle/TaskCreated/TaskCompleted）做质量门。
- E9 用户直达 teammate 的 REPL 通道（如 `/agent <name> <message>`）。
- E10 teammate 独立 cache 桶意识：设计时把"每个 teammate 的 prompt cache 生命周期独立于主对话"作为显式约束写进文档。

**视场景取舍**
- E11 Worktree clean 时自动清理（会牺牲 A 刻意的保守删除纪律，教学场景可不取）。
- E12 广播辅助（B 也刻意没做，需逐人发送）。

---

## 八、验证与来源

- A 侧条目可回指 README 章节：§1 用户确认、§2 teammate 生命周期、§3-4 MessageBus、§5 result/idle 拆分、§6 IDLE 优先级、§7 原子 claim、§8-10 worktree 集成与删除纪律、§11 shutdown 状态机、§12 plan 门与 work_version。
- B 侧条目已逐条对照官方文档原文核实：
  - code.claude.com/docs/en/agent-teams（.md 全文）：spawn 不确认、邮箱路径与校验语义、team config members 可读、权限冒泡与 plan 自动批准、消息信任标注与分类器审查、shutdown 拒绝、hooks 三件套、cache TTL 桶、9 条已知限制。
  - code.claude.com/docs/en/sub-agents（.md 全文）：frontmatter 字段全集、`isolation: worktree` 与自动清理、maxTurns partial + resume、模型选择链与 FORCE、15k token 描述告警、Task 工具可用性。
- B 为实验特性且版本相关行为频繁变动（文档标注 v2.1.x 版本门槛多处），具体行为以实际启用版本为准。

## 九、范围说明

本交付物仅为对比分析，不含 Go 项目（coding-agent-go）的实现计划（用户已确认范围）。
