# System Prompt 与 Claude Code 默认 System Prompt 的差距分析

> 基准：《完整还原的默认 System Prompt 全集》（16 个 Block，下称“参考稿”）
> 对照对象：`coding-agent-go` 当前实现（`main` 分支）
> 结论日期：2026-10-22

## 1. 摘要

当前 `coding-agent-go` 的 system prompt 只有 6 行、约 120 token，只描述了工作目录、Unix/Windows 环境、`todo_write` 用法、后台 bash、技能目录与记忆召回。参考稿则按“身份与安全 → 系统规则 → 任务政策 → 行动风险 → 工具使用 → 语气 → 输出效率 → 会话指引 → 记忆 → 环境（→ MCP → Output Style → 语言 → Scratchpad → 结果清理）”组织成 16 个 Block，总量约 3k token，并且大量条款与具体工具/机制绑定。

差距可以归纳为三类：

1. **已有能力没有写进 prompt**：权限三闸门、Hook 拦截、上下文自动压缩、子代理、持久化任务图、MCP、跨会话记忆的语义都没有被表达，模型只能靠工具描述猜测。
2. **prompt 缺少行为契约**：没有“先读后改”“不加需求外功能”“不绕过安全检查”“危险/共享操作先确认”“报告要忠实”等约束，也没有输出风格与引用格式要求。
3. **参考稿依赖的不少能力在本项目不存在**：`AskUserQuestion`、用户可配置 hooks、`!` 命令前缀、斜杠命令/Skill 工具、Plan 模式、Output Style、`settings.language`、Scratchpad、Function Result Clearing、fork/后台子代理、对抗式 verifier、可直接写入的 auto memory 工具。

本次改造落地第 1、2 类中成本低、无需新增工具的部分：把 prompt 拆成 11 个 Block，补齐环境信息、MCP server instructions 与记忆使用指引；第 3 类只记录，不实现。

## 2. 当前实现全貌

### 2.1 prompt 组装

全部内容硬编码在 [llm.go](file:///Users/tony/coding-agent-go/llm.go#L60-L96)：

- 第 1 行：`You are a coding agent at <cwd>. Environment: <Unix-like|Windows>.`
- 第 2 行：复杂任务先用 `todo_write` 规划。
- 第 3 行：慢命令用 `run_in_background=true`，结果稍后以 `<task_notification>` 返回。
- 第 4 行：`Use the provided tools to solve tasks. Act, don't explain.`
- 第 5–7 行：`Skills available:` + 技能目录（name + description）。
- 第 8 行：`Use load_skill to read the full instructions when a skill applies.`
- 条件追加：会话开始召回的记忆全文（[memory.go](file:///Users/tony/coding-agent-go/memory.go#L211)）。

发送路径：[llm.go](file:///Users/tony/coding-agent-go/llm.go#L50-L58) 把整段字符串作为单个 `TextBlockParam` 放进 `System`，没有 `cache_control`，每轮全量重发。

### 2.2 已有工具与机制

| 类别 | 实现 | prompt 是否提及 |
|---|---|---|
| 文件读写编辑 | `read_file` / `write_file` / `edit_file`（[tools.go](file:///Users/tony/coding-agent-go/tools.go#L695-L732)） | 否 |
| 搜索 | `glob` / `grep`（[tools.go](file:///Users/tony/coding-agent-go/tools.go#L733-L756)） | 否 |
| Shell | `bash`（同步/后台） | 部分（后台） |
| 计划 | `todo_write` + 连续 3 轮未更新提醒（[agent.go](file:///Users/tony/coding-agent-go/agent.go#L137-L149)） | 是 |
| 子代理 | `task`（[task.go](file:///Users/tony/coding-agent-go/task.go#L44-L91)） | 否 |
| 技能 | 目录进 prompt + `load_skill`（[skill.go](file:///Users/tony/coding-agent-go/skill.go#L103-L137)） | 是 |
| 历史压缩 | 4 步确定性压缩 + reactive compact（[compact.go](file:///Users/tony/coding-agent-go/compact.go#L1-L22)） | 否 |
| 持久任务图 | `create_task` / `update_task` / `claim_task` / `complete_task` / `get_task` / `list_tasks`（[taskstore.go](file:///Users/tony/coding-agent-go/taskstore.go)） | 否 |
| 跨会话记忆 | 自动召回 + 轮末抽取 + 合并（[memory.go](file:///Users/tony/coding-agent-go/memory.go#L1-L14)） | 仅召回文本，无使用规则 |
| MCP | `connect_mcp` + `mcp__{server}__{tool}`（[mcp.go](file:///Users/tony/coding-agent-go/mcp.go#L206-L250)） | 否 |
| 权限 | 三闸门 + MCP host-side 策略（[permission.go](file:///Users/tony/coding-agent-go/permission.go#L94-L128)） | 否 |
| Hook | 4 个生命周期事件，内置权限/日志/大输出/上下文注入/Stop 汇总（[hooks.go](file:///Users/tony/coding-agent-go/hooks.go#L73-L79)） | 否 |

## 3. 逐 Block 差距矩阵

等级定义：P0 = 影响正确性与安全，纯 prompt 可补；P1 = 有明显收益，需少量代码；P2 = 参考稿有、本项目缺机制，本期不做。

| Block | 参考稿要点 | 我们现状 | 等级 | 支撑条件 | 本 PR |
|---|---|---|---|---|---|
| 1 身份与安全 | 交互式工程 agent；授权安全测试可协助、拒绝破坏性/DoS/供应链攻击；不臆造 URL | 仅一句“coding agent at <cwd>” | P0 | 纯 prompt | ✅ |
| 2 系统规则 | 文本即用户可见；权限模式与“被拒后不要原样重试”；`<system-reminder>` 语义；外部结果的 prompt injection；hooks 反馈；自动压缩 | 无 | P0 | 权限/Hook/压缩均已实现，纯 prompt | ✅ |
| 3 编码任务政策 | 先读后改、必要才建文件、不给时间预估、失败先诊断、OWASP、最小改动、不做兼容 hack、/help 与反馈渠道 | 无 | P0 | 纯 prompt；`/help` 与反馈渠道无支撑，删除 | ✅（删不可用条） |
| 4 行动风险控制 | 可逆本地操作自由执行；删除/强推/reset/共享状态先确认；不绕过安全检查；未知状态先调查 | 无；仅 CLI 三闸门会拦截 | P0 | 权限闸门已存在，prompt 补“先确认、勿绕过” | ✅ |
| 5 工具使用 | 专用工具优先于 bash；任务管理；一次响应可发多个独立调用 | 无；工具描述里各自说明 | P0 | 全部工具已存在，需按真实名称改写 | ✅ |
| 6 语气与风格 | 少 emoji；简洁；`file_path:line_number`；GitHub `owner/repo#123`；工具调用前不用冒号 | 无 | P0 | 纯 prompt；GitHub 引用格式可保留（无 gh 工具也可读） | ✅ |
| 7 输出效率 | 先结论后解释、只保留决策/里程碑/阻塞、不复述用户 | 仅“Act, don't explain.” | P0 | 纯 prompt | ✅ |
| 8 会话级指引 | AskUserQuestion、`!` 前缀、Agent/Explore 子代理、Glob/Grep 直用、`/skill` | 仅技能目录两行 | P1 | `task`/`glob`/`grep`/`compact` 已有；AskUserQuestion、`!`、斜杠命令无支撑 | ✅（仅可用条） |
| 9 持久记忆 | 四类记忆、写入流程、MEMORY.md 索引、时效校验、何时访问 | 只注入召回全文 | P1 | 自动召回/抽取已存在；**无记忆写入工具**，无法照抄“立即保存” | ✅（改为自动记忆语义） |
| 10 环境 | cwd、git 仓库、平台、shell、OS 版本、模型名/ID、知识截止、产品矩阵 | 仅 cwd + Unix/Windows | P0 | 均可本地探测 | ✅ |
| 11 MCP 指令 | 按 server 渲染 instructions，唯一 uncached section | 无；`MCPServer` 无 instructions 字段 | P1 | 需给接口加 `Instructions()` | ✅ |
| 12 Output Style | 自定义输出风格 | 无配置系统 | P2 | 缺 settings 与 style 注入 | ❌ |
| 13 语言偏好 | `settings.language` | 无 | P2 | 缺配置 | ❌ |
| 14 Scratchpad | 会话临时目录 | 无；临时文件走 `.task_outputs/` | P2 | 缺会话级 scratchpad 管理 | ❌ |
| 15 工具结果清理 | FRC 保留最近 N 条 | 有 micro_compact（按“已消费”替换），语义不同 | P2 | 机制不等价，不能照抄文案 | ❌ |
| 16 结果留存提醒 | 重要信息写进回复，原结果可能被清理 | 无 | P1 | micro_compact 会替换旧结果，可补一句 | ✅ |

## 4. 可实现 vs 无工具/机制支撑

### 4.1 当前可直接实现（本次落地）

| 能力 | 落地方式 |
|---|---|
| 身份、安全边界、URL 约束 | prompt Block |
| 权限模式与拒绝语义 | prompt Block（权限闸门已存在） |
| Hook 拦截反馈 | prompt Block（Hook 已存在） |
| 自动上下文压缩 | prompt Block（压缩流水线已存在） |
| 任务政策、最小改动、先读后改 | prompt Block（read-before-write 门控已存在） |
| 行动风险与确认 | prompt Block（三闸门已存在） |
| 工具优先级与并行/串行规则 | prompt Block（工具池已存在） |
| 语气、引用格式、输出效率 | prompt Block |
| 子代理、定向搜索、compact、后台 bash、持久任务图、MCP 连接 | 会话指引 Block |
| 记忆背景知识语义与时效校验 | 记忆 Block（自动召回已存在） |
| 环境（cwd/git/platform/shell/OS/模型） | 动态环境 Block |
| MCP server instructions | 接口新增 `Instructions()` + 渲染 Block |
| 工具结果留存提醒 | 输出效率/系统规则 Block |

### 4.2 没有工具或机制支撑（本次不实现）

| 参考稿能力 | 缺什么 |
|---|---|
| `AskUserQuestion` 工具 | 无该工具；当前确认由 CLI 三闸门 `[y/N]` 完成，模型无法主动提问 |
| 用户可配置 hooks | Hook 仅内置注册，无 settings 加载与用户脚本 |
| `! <command>` 前缀 | REPL 只把输入当用户消息，无本地命令执行通道 |
| `/<skill-name>` 斜杠命令、Skill 工具 | 只有 `load_skill(name)`，无用户可调用命令与技能发现工具 |
| Plan 模式 / Plan 审批 | 无模式切换与计划文件机制 |
| Output Style | 无 settings / style prompt 注入 |
| `settings.language` | 无配置系统 |
| Scratchpad 目录 | 无会话级临时目录管理 |
| Function Result Clearing（保留最近 N 条） | 只有 micro_compact，按“已消费”而非“最近 N 条” |
| fork 子代理 / 后台子代理 | `task` 是同步嵌套 loop，只允许一层 |
| 对抗式 verifier / 完成验证合同 | 无独立验证代理 |
| auto memory 直接写入 | 记忆写入由轮末自动抽取完成，模型无 memory 工具 |
| MCP 连接管理/instructions 透传 | 本次补 instructions；真实 stdio/SSE 传输仍未实现（当前为进程内 mock） |
| 知识截止日期、产品矩阵 | 无模型元数据表，且与本地教学项目无关 |

## 5. 主题深度分析

### 5.1 Prompt 结构

参考稿的每个 Block 对应源码里的独立函数，按条件拼装；我们只有一段 `fmt.Sprintf`。后果：

- 无法按能力启停：MCP 未连接时也会看到无用文字，连接后又没有任何说明。
- 变更耦合：任何一句话都要改同一段格式化字符串，测试也只能做子串断言。
- 缓存不可控：单块 system 无法做“稳定前缀 + 动态后缀”的缓存切分（当前也无 cache 实现）。

### 5.2 环境信息

参考稿环境块包含 cwd、是否 git 仓库、平台、shell、OS 版本、模型名与 ID、知识截止、产品可用形态。我们只有 cwd 与一个二值 Unix/Windows 判断。模型因此无法判断：

- 是否能安全使用 git 命令（是否在仓库内）；
- shell 是 zsh 还是 bash（影响语法与内建命令）；
- 自身模型身份（影响自我描述与能力边界）。

本次补齐除知识截止与产品矩阵外的全部字段；OS 版本用 `uname -sr` 一次性缓存，git 仓库用向上查找 `.git` 判断，不调用外部命令。

### 5.3 工具契约与命名

参考稿按 Claude Code 工具名（Read/Write/Edit/Glob/Grep/Bash/TaskCreate）描述优先级；本项目工具名是 `read_file` / `write_file` / `edit_file` / `glob` / `grep` / `bash` / `todo_write`，直接照抄会让模型调用不存在的工具。本次全部改写为本项目真实名称，并新增测试禁止 prompt 出现 `AskUserQuestion`、`TodoWrite`、`TaskCreate`、`Skill tool`、`! <command>` 等无支撑文案。

另有一处能力差异必须在 prompt 中如实表达：参考稿鼓励并行工具调用；本项目通过 [scheduler.go](file:///Users/tony/coding-agent-go/scheduler.go) 的路径感知调度器实现有条件并发：独立的只读调用（read_file / glob / grep）可并发执行；写操作（write_file / edit_file / bash 等）独占串行；读写同一资源的调用按发出顺序执行。因此文案写成“独立只读调用会并发；写类调用不会重叠；读写同一资源按发出顺序执行；结果按发出顺序返回；有依赖的调用仍要串行”，如实反映并发语义。

### 5.4 记忆语义

参考稿的 auto memory 是“模型主动写文件 + MEMORY.md 索引 + 四类记忆”。本项目是 harness 自动行为：

- 会话开始：把记忆目录发给模型选最多 5 条，失败回退关键词匹配，全文注入 system；
- 轮末：模型给候选，只有 `scope=persistent` 且通过校验才落盘；
- 记录数达到 10 条触发合并。

模型没有写入工具，也不能“用户说记住就立即保存”。照抄参考稿会造成两类错误：模型声称已保存（实际没保存）、模型尝试调用不存在的 memory 工具。本次文案明确：记忆是自动的、是背景知识、冲突时以当前请求为准、使用前校验文件现状；用户要求记住时在回复中明确陈述，轮末抽取可能持久化，但不承诺一定落盘。

参考稿的 `MEMORY.md` 索引、frontmatter、四类记忆结构在本项目已经存在（[memory.go](file:///Users/tony/coding-agent-go/memory.go#L48-L64)），只是写入方不是模型，所以结构部分不必进 prompt。

### 5.5 MCP

参考稿把 MCP instructions 作为唯一逐轮重算的 uncached section。本项目 `MCPServer` 只有 `ListTools` / `CallTool`，server 的用途与使用约束只能写在每个工具的 description 里，缺少 server 级说明（例如“deploy 会改动共享生产状态”）。本次给接口补 `Instructions() string`，由 `mcpRegistry` 按 server 名排序渲染；未连接时不输出该 Block。

### 5.6 安全与权限

- 参考稿把“危险/共享操作先确认”作为模型侧行为约束；
- 本项目把“硬拒清单 + 破坏性规则 + 人工确认”放在 harness 侧（[permission.go](file:///Users/tony/coding-agent-go/permission.go#L52-L64)）。

两者是互补关系而非替代：harness 只能匹配已知模式，模型仍可能用未见过的命令或 `--no-verify` 绕过检查。本次 prompt 明确“遇到障碍不要用破坏性手段绕过安全检查；不确定先确认”，与三闸门形成双层防护。

### 5.7 输出风格与缓存经济学

参考稿把语气、输出效率、内部版“沟通”合并成独立 Block，并强调“先动作后解释、只写决策/里程碑/阻塞”。本项目只有“Act, don't explain.”，过于粗糙，容易导致模型在不该省略时省略（例如不报告失败测试）。

缓存方面：参考稿用 `__SYSTEM_PROMPT_DYNAMIC_BOUNDARY__` 把稳定前缀与动态内容分开，配合 `cache_control` 跨会话缓存。本项目 `Send` 只接受单个字符串且未设置 `cache_control`，prompt 从约 120 token 增至约 1.2k token 后，每轮输入成本会线性上升。本期接受该成本；缓存切分列为 P2，需要改 `LLMClient` 接口与请求构造。

## 6. 改进方案分期

### P0（本次 PR）

1. 新建 `systemprompt.go`，按 11 个 Block 组装：
   - 身份与安全、系统规则、任务政策、行动风险、工具使用、语气、输出效率；
   - 会话指引（技能目录、`task`、定向搜索、`compact`、后台 bash、`todo_write`、持久任务图、`connect_mcp`）；
   - 记忆（仅召回时）、MCP instructions（仅连接且有内容时）、环境。
2. 环境块动态化：cwd、git 仓库、platform、shell、OS 版本、模型 ID。
3. `MCPServer` 增加 `Instructions()`，docs/deploy 两个 mock server 给出 server 级说明。
4. 新增 `systemprompt_test.go`，锁定 Block、真实工具名、条件块与环境字段。
5. 差距文档入库。

### P1（后续 issue）

- `AskUserQuestion` 工具（模型主动提问，替代纯 CLI 确认）；
- Output Style / language 配置；
- Scratchpad 会话目录；
- MCP 真实 stdio/SSE 传输与连接配置。

### P2（长期）

- system prompt 缓存切分（`cache_control`，稳定前缀 + 动态后缀）；
- Function Result Clearing 等价机制；
- fork/后台子代理与对抗式 verifier。

## 7. 验收标准

本 PR 合入后应满足：

1. `systemPrompt(newSessionState())` 包含：`# System`、`# Doing tasks`、`# Executing actions with care`、`# Using your tools`、`# Tone and style`、`# Output efficiency`、`# Session-specific guidance`、`# Environment`、`# Memory`（有召回时）、`# MCP Server Instructions`（有连接且有内容时）。
2. 不再出现旧开头 `You are a coding agent at`。
3. prompt 中只出现本项目真实工具名；不出现 `AskUserQuestion`、`TodoWrite`、`TaskCreate`、`Skill tool`、`! <command>`。
4. 环境块包含 cwd、`Is a git repository`、`Platform`、`Shell`、`OS Version`、模型 ID。
5. 既有技能目录测试与记忆召回测试保持通过。
6. `gofmt` 无差异，`go vet ./...`、`go test ./...`、`go build` 全部通过。

## 8. 不做的事与风险

- 不新增 AskUserQuestion、Plan 模式、Output Style、Language、Scratchpad。
- 不改权限规则、Hook 注册、压缩算法与任何工具 handler 行为。
- 不实现 prompt caching；接受 prompt 变长带来的每轮输入成本。
- 风险：长 prompt 可能稀释指令。通过 Block 标题分段、保留原有技能/记忆原文、用测试锁定关键句子降低风险。
- 风险：接口新增方法影响所有 `MCPServer` 实现。已确认仅有 `docsServer`、`deployServer` 两个实现，测试未自定义 server。
