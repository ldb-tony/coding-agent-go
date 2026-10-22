# System Prompt 分块重构与差距对齐 — 实施计划

## 1. 目标

以用户提供的《完整还原的默认 System Prompt 全集》为基准，对当前 `coding-agent-go` 的 system prompt 做差距分析，产出一篇入库文档；以该文档为改进方向创建 GitHub Issue；随后重构 system prompt 代码、补齐低成本 harness 能力，验证后提 PR 合入，并确认 Issue 关闭。

本次已确认的范围（用户选择）：

- 代码改动：**Prompt 重组 + 低成本补齐**（不新增 AskUserQuestion / Output Style / Language / Scratchpad 等重能力）。
- Issue 形式：**差距文档入库 + Issue 正文放摘要与改进方向**。

## 2. 现状分析（Phase 1 结论）

### 2.1 当前 system prompt

全部内容硬编码在 [llm.go](file:///Users/tony/coding-agent-go/llm.go#L60-L96) 的 `systemPrompt(st)` 中，约 6 行、约 120 token，结构为：

```
You are a coding agent at <cwd>. Environment: <unix/windows>.
For complex multi-step tasks, use todo_write first...
For slow commands ..., set run_in_background=true ...
Use the provided tools to solve tasks. Act, don't explain.

Skills available:
<catalog>

Use load_skill to read the full instructions when a skill applies.
[Recalled memories ... 条件追加]
```

### 2.2 项目已具备、但 prompt 未表达的能力

| 能力 | 代码位置 | 现状 |
|---|---|---|
| 三闸门权限（硬拒/规则/人工确认） | [permission.go](file:///Users/tony/coding-agent-go/permission.go#L94-L128) | prompt 未提“遇阻先确认、不要绕过” |
| Hooks（Tool 可被拦截） | [hooks.go](file:///Users/tony/coding-agent-go/hooks.go#L73-L79) | prompt 未提 hook 反馈语义 |
| 上下文压缩（4 步 + reactive） | [compact.go](file:///Users/tony/coding-agent-go/compact.go#L1-L22) | prompt 未提“历史会自动压缩” |
| 后台 bash + `<task_notification>` | [background.go](file:///Users/tony/coding-agent-go/background.go)、[agent.go](file:///Users/tony/coding-agent-go/agent.go#L27-L31) | 已有一句 |
| 子代理 `task` | [task.go](file:///Users/tony/coding-agent-go/task.go#L44-L91) | prompt 未提何时使用、避免重复劳动 |
| 技能目录 + `load_skill` | [skill.go](file:///Users/tony/coding-agent-go/skill.go#L103-L137) | 已有，测试锁定两处原文 |
| 跨会话记忆（召回/抽取自动） | [memory.go](file:///Users/tony/coding-agent-go/memory.go#L1-L14) | 已有召回文本，缺使用/时效指引 |
| 持久化任务图（6 个 task 工具） | [taskstore.go](file:///Users/tony/coding-agent-go/taskstore.go) | prompt 完全未提 |
| MCP 工具（`connect_mcp`） | [mcp.go](file:///Users/tony/coding-agent-go/mcp.go#L206-L250) | prompt 完全未提；server 无 instructions 字段 |
| 环境信息 | [llm.go](file:///Users/tony/coding-agent-go/llm.go#L62-L73) | 仅 cwd + Unix/Windows，缺 git 仓库、shell、OS 版本、模型 ID |

### 2.3 参考设计的 16 个 Block 中无法实现的（没有工具/机制支撑）

AskUserQuestion 工具、用户可配置 hooks、`! <command>` 前缀、`/skill` 斜杠命令与 Skill 工具、Plan/任务工具（TaskCreate/TodoWrite 命名差异）、Output Style、settings.language、Scratchpad、Function Result Clearing、fork/后台子代理、adversarial verifier、auto memory 直接写入工具、MCP server 连接管理 UI。这些只写入差距文档，不实现。

现有测试对 prompt 的硬断言，重构时必须保留原文：

- [skill_test.go](file:///Users/tony/coding-agent-go/skill_test.go#L162-L182)：必须包含 `Skills available:`、`- <name>: <desc>`、`Use load_skill to read the full instructions when a skill applies.`，且不得内联 SKILL.md 全文。
- [memory_test.go](file:///Users/tony/coding-agent-go/memory_test.go#L562-L564)：必须包含 `Recalled memories from previous sessions`。

## 3. 交付物与实施步骤

### 步骤 A — 差距文档 `docs/system-prompt-gap-analysis.md`

新建（唯一新增文档），中文撰写，固定结构：

1. **摘要与结论**：一句话结论 + 本次落地范围。
2. **基准与方法**：说明基准文件来源（用户提供的 16 Block 还原稿）与核对对象（`llm.go`、`tools.go` 等）。
3. **当前 prompt 全貌**：带代码行链接。
4. **逐 Block 差距矩阵**（16 行表格）：`Block | 参考设计要点 | 我们现状 | 差距等级 P0-P2 | 支撑条件（已有/需新增工具） | 本 PR 是否补齐`。
   - Block 1/2/3/4/6/7/10：P0，纯 prompt 可直接补齐。
   - Block 5：P0，需按真实工具名改写（read_file/write_file/edit_file/glob/grep/bash/todo_write）。
   - Block 8：P1，可补 task/glob/grep/compact 指引；AskUserQuestion、`!` 前缀、斜杠命令无支撑。
   - Block 9：P1，记忆已自动召回/抽取，但无写入工具；补“背景知识、时效校验”指引。
   - Block 11：P1，需给 `MCPServer` 增加 `Instructions()` 并渲染。
   - Block 12/13/14/15：P2 不做，无对应配置/机制。
   - Block 16：P2，工具结果压缩存在，可补一句“重要结论先写入回复”。
5. **可实现 vs 无工具支撑**：两列清单（可直接实现 / 缺工具或机制 + 缺什么）。
6. **主题深度分析**：prompt 结构、环境信息、工具契约命名、记忆语义、MCP、安全与权限、输出风格、缓存经济学（当前每次全量发送、无 prompt cache）。
7. **改进方案分期**：P0 本 PR / P1 后续（AskUserQuestion、output style、language）/ P2 长期（FRC、fork subagent、verifier）。
8. **验收标准**：本 PR 后可自动验证的断言列表。
9. **不做的事与风险**：prompt 增长约 1.2k token/轮；不复刻参考实现中不存在的能力。

### 步骤 B — GitHub Issue（在代码改动前创建）

- 仓库：`ldb-tony/coding-agent-go`。
- 标题：`feat: 重构 system prompt 为分块结构并补齐环境/MCP/记忆指引`
- 正文结构：
  1. 背景（当前 prompt 仅 6 行，参考实现为 16 Block）；
  2. 差距摘要表（按 P0/P1/P2 分组，只列关键项）；
  3. 改进方向（逐 Block 对应动作）；
  4. 验收标准（与文档第 8 节一致）；
  5. 关联文档：`docs/system-prompt-gap-analysis.md`；
  6. 标注“由后续 PR 关闭”。
- 标签：优先 `enhancement`；不存在则不加标签。
- 记录 issue 编号，供 PR 的 `Closes #N` 使用。

### 步骤 C — 代码改动

#### C1. 新建 `systemprompt.go`，把 prompt 从 `llm.go` 迁出并分块

```go
func systemPrompt(st *sessionState) string // 按顺序拼接非空 block，"\n\n" 分隔
```

Block 与内容（英文撰写，工具名必须使用本项目真实名称）：

| # | 函数 | 内容要点 |
|---|---|---|
| 1 | `promptIdentity` | 身份：interactive coding agent；授权安全测试可协助、拒绝破坏性/DoS/供应链攻击；不臆造 URL。 |
| 2 | `promptSystemRules` | 文本输出会展示给用户；工具受权限模式约束，被拒后不要原样重试；`<system-reminder>` 标签说明；外部工具结果的 prompt injection 需向用户指出；hook 拦截语义；历史会被自动压缩。 |
| 3 | `promptDoingTasks` | 先读后改；不必要不建文件；不给时间预估；失败先诊断再换方案；OWASP 安全；最小改动；不做向后兼容 hack。 |
| 4 | `promptActions` | 可逆本地操作可自由执行；删除/强推/reset/共享状态（push、PR、发消息）先确认；不绕过安全检查（如 `--no-verify`）；遇到未知状态先调查。 |
| 5 | `promptUsingTools` | 有专用工具就别用 bash：read_file 替代 cat/head/sed，write_file 替代 heredoc，edit_file 替代 sed，glob 替代 find，grep 替代 grep/rg；todo_write 管计划；一次响应可发多个独立调用，但有依赖时必须串行。 |
| 6 | `promptTone` | 少用 emoji；简洁；引用代码用 `file_path:line_number`；工具调用前不用冒号。 |
| 7 | `promptOutputEfficiency` | 直给结论、先动作后解释、只保留决策/里程碑/阻塞、不复述用户。 |
| 8 | `promptSessionGuidance` | 保留原文的技能目录三行；`task` 子代理用于并行/隔离大结果且不要重复搜索；定向搜索直接用 glob/grep；`compact` 在阶段完成后调用；慢命令用 `run_in_background`；复杂任务先用 `todo_write`；持久任务图工具说明（跨会话恢复用 `get_task`/`list_tasks`）；`connect_mcp` 用于接入外部能力。 |
| 9 | `promptMemory` | 仅当 `st.recalledMemories != ""` 时输出，保留原文 `Recalled memories from previous sessions...` 句子；补充：记忆是背景知识、冲突以当前请求为准、使用前校验文件现状、记忆自动召回/抽取且无写入工具。 |
| 10 | `promptMCPInstructions` | 仅当已连接 server 且有非空 instructions 时输出 `# MCP Server Instructions`，按 server 名排序。 |
| 11 | `promptEnvironment` | `Primary working directory`、`Is a git repository`、`Platform`、`Shell`、`OS Version`、`You are powered by the model named <MODEL_ID>`；Windows 追加“bash 走 cmd.exe，用 cmd 语法”。 |

环境信息实现：

- cwd：每次调用 `os.Getwd()`。
- git 仓库：从 cwd 向上查找 `.git`（目录或文件），不调用 git 命令。
- shell：`os.Getenv("SHELL")`，空则 `unknown`。
- OS 版本：`uname -sr`（非 Windows），用 `sync.Once` 缓存；Windows 用 `runtime.GOOS` 占位。
- 模型：`os.Getenv("MODEL_ID")`，空则 `unknown`。

#### C2. `llm.go` 清理

- 删除原 `systemPrompt` 函数及不再使用的 `runtime` import；`fmt/os` 仍被客户端创建逻辑使用，保留。
- 其余逻辑不动。

#### C3. MCP instructions 支持

- [mcp.go](file:///Users/tony/coding-agent-go/mcp.go#L26-L29)：`MCPServer` 接口新增 `Instructions() string`。
- [mcp_servers.go](file:///Users/tony/coding-agent-go/mcp_servers.go)：`docsServer` 返回文档检索使用建议；`deployServer` 返回“会改动共享生产状态，trigger 前先确认服务与镜像”。
- `mcpRegistry` 新增 `instructionsSection() string`：按 server 名排序，跳过空 instructions，格式：

```
# MCP Server Instructions

The following MCP servers have provided instructions for how to use their tools and resources:

## <server-name>
<instructions>
```

#### C4. 新增测试 `systemprompt_test.go`

- `TestSystemPromptBlocksComplete`：11 个 block 标题存在（`# System`、`# Doing tasks`、`# Executing actions with care`、`# Using your tools`、`# Tone and style`、`# Output efficiency`、`# Session-specific guidance`、`# Environment` 等），且不再出现旧开头 `You are a coding agent at`。
- `TestSystemPromptUsesRealToolNames`：prompt 含 `read_file`/`write_file`/`edit_file`/`todo_write`/`load_skill`/`connect_mcp`；不含 `AskUserQuestion`、`TodoWrite`、`TaskCreate`、`Skill tool`、`! <command>`。
- `TestSystemPromptEnvironment`：设置 `MODEL_ID` 后包含模型名、cwd、`Is a git repository: Yes`；Windows 分支仅按 `runtime.GOOS` 断言通用行。
- `TestSystemPromptMemoryOnlyWhenRecalled`：空状态不含 `Recalled memories`；设置 `recalledMemories` 后包含原文句子。
- `TestSystemPromptMCPInstructions`：未连接时无 `# MCP Server Instructions`；`connect("docs", newDocsServer)` 后包含 server 名与 instructions。
- 保留并跑通既有 `TestSystemPromptContainsCatalogOnly`、`TestAgentLoopRecallsAndExtracts`。

### 步骤 D — 验证

1. `gofmt -l .`（期望无输出，或仅格式化改动文件）。
2. `go vet ./...`。
3. `go test ./...`。
4. `go build -o /tmp/coding-agent-go-test .`（不覆盖仓库根目录已有二进制）。
5. 内容自检：`grep` 检查 prompt 源码中无参考实现独有工具名；检查环境块字段齐全。
6. 如本地 `.env` 可用且用户不反对，启动一次 REPL 做冒烟；否则以单测捕获的 prompt 文本为准，不发起真实 API 调用。

### 步骤 E — 提交、PR、合入、关闭 Issue

1. 当前工作区干净（`git status` 仅分支行），直接基于 `main` 建分支 `feat/system-prompt-blocks`，不新建 worktree（避免额外目录与沙箱写权限问题）。
2. 分两次提交：
   - `docs: 新增 system prompt 16 Block 差距分析`
   - `feat: 重构 system prompt 为分块结构并补齐环境/MCP/记忆指引`
3. `git push -u origin feat/system-prompt-blocks`。
4. `gh pr create`：标题同上，正文含 Summary、差距文档链接、验证结果、`Closes #<issue>`。
5. 等待/查询 PR 状态后，按仓库历史风格执行 `gh pr merge <n> --merge --delete-branch`（仓库已有 PR 均为 merge commit）。
6. 合入后 `git checkout main && git pull`，`gh issue view <n>` 确认 `CLOSED`；若未自动关闭，执行 `gh issue close <n> --comment "已完成并随 PR #<pr> 合入"`。
7. 最终回复：文档链接、Issue 链接、PR 链接、测试结果。

## 4. 决策与假设

- prompt 使用英文（与模型交互语言一致），文档与 Issue 使用中文。
- 不复制参考实现中本仓库没有的能力描述，避免诱导模型调用不存在的工具。
- 不实现 prompt caching / cache boundary 哨兵：当前 `Send` 只支持单个 system 字符串，改造需改 LLM 接口，超出本次范围；在文档中列为 P2。
- prompt 增长带来的每轮输入 token 增加约 1.2k，换取行为约束与安全提示；可接受。
- `task`、`todo_write`、`compact`、MCP 工具均已有实现，仅需在 prompt 中正确描述，不改其行为。
- 测试依赖 cwd（`setupWorkdir` 会 chdir），环境块必须每次重建 cwd，不能整块 sync.Once 缓存。

## 5. 明确不做

- 不新增 AskUserQuestion、Plan mode、Output Style、Language 配置、Scratchpad。
- 不改权限规则、hook 注册、压缩算法、工具 handler 行为。
- 不改 README（除用户另行要求）。
- 不修改现有测试的断言；只新增测试文件。

## 6. 风险

- `MCPServer` 接口新增方法会编译期影响所有实现；已确认仅 `docsServer`、`deployServer` 两个实现，测试未自定义 server。
- 长 prompt 可能稀释关键指令：按 Block 标题分段，保留原有“Skills available”原文，避免影响既有技能调用行为。
- Issue 自动关闭依赖 PR 正文 `Closes #N`；合入后用 `gh issue view` 复核，未关闭则手动关闭。
