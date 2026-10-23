# coding-agent-go

一个用 Go 实现的、教学取向的 Coding Agent：REPL + Agent Loop + 16 个工具，覆盖权限、Hook、上下文压缩、子代理、技能加载、跨会话记忆、持久化任务图与后台任务等完整机制。模型层使用 Anthropic Messages API（兼容任何 Anthropic 协议的端点）。

## 快速开始

### 1. 克隆与依赖

```bash
git clone <repo-url>
cd coding-agent-go
go mod download   # 需要 Go 1.24+
```

### 2. 配置环境变量

复制 [.env.example](.env.example) 为 `.env`，按需修改：

```bash
cp .env.example .env
```

| 变量 | 必填 | 说明 |
|------|------|------|
| `ANTHROPIC_API_KEY` | 是 | API Key，Anthropic 官方在 https://console.anthropic.com/ 申请；也可填任何 Anthropic 兼容服务的 Key |
| `MODEL_ID` | 是 | 模型 ID，如 `claude-sonnet-4-5` 或兼容服务提供的模型名 |
| `ANTHROPIC_BASE_URL` | 否 | 自定义 Anthropic 兼容端点（如各类 MaaS 网关）；留空则走 Anthropic 官方 API |

### 3. 启动

```bash
go run .
# 或先编译再运行
go build -o coding-agent-go && ./coding-agent-go
```

启动后进入 REPL：输入问题回车发送，`q` / `exit` / 空行 / Ctrl-D 退出，Ctrl-C 安全退出（自动清理后台任务进程组）。

### 4. 运行测试

```bash
go test ./...
```

---

## 技术架构

整体是一个**单进程、单二进制**的 Agent：REPL 入口 → Agent Loop → 工具分发 → LLM。各项能力以"正交机制"叠加在 loop 上，而非写死在 loop 里。

- [Agent Loop 与工具分发](#agent-loop-与工具分发) — loop 只负责收发消息与查表分发，加工具 = 加一行映射
- [文件工具与路径安全](#文件工具与路径安全) — read/write/edit/glob/grep，safePath 防逃逸、read-before-write 门控、原子写
- [Bash 工具](#bash-工具) — 超时控制、输出截断、退出码语义化、cwd 跨调用持久化
- [三闸门权限管线](#三闸门权限管线) — 硬拒清单 → 规则匹配 → 人工确认
- [Hook 机制](#hook-机制) — 四个生命周期事件，扩展逻辑挂在外面而不是写进 loop
- [上下文压缩流水线](#上下文压缩流水线) — usage 锚点计数 + 空闲期微压缩 + 超窗口自动压缩 + 超限额底重试
- [Todo 规划与提醒](#todo-规划与提醒) — 会话内待办清单 + 连续 3 轮未更新自动注入 reminder
- [子代理（Subagent）](#子代理subagent) — 全新 messages[] 的嵌套 loop，只带回最终文本
- [技能按需加载](#技能按需加载) — system prompt 只放目录，SKILL.md 全文用时才取
- [跨会话记忆](#跨会话记忆) — 存储 / 召回 / 抽取 / 合并四个子系统，落盘在 .memory/
- [持久化任务图](#持久化任务图) — .tasks/*.json 跨会话可恢复的 DAG，两阶段建图
- [后台任务](#后台任务) — run_in_background 立即返回，结果以 `<task_notification>` 后续注入
- [MCP 外部工具连接](#mcp-外部工具连接) — 运行时 connect_mcp bring-in 外部服务发现的工具，mcp__{server}__{tool} 命名
- [搜索引擎抽象](#搜索引擎抽象) — ripgrep 优先 + Go walker 回退，统一尊重 .gitignore

---

## 技术详解

### Agent Loop 与工具分发

[agent.go](agent.go) 的 `agentLoop` 是核心：每轮把 messages 发给模型 → 按 `response.content` 原始顺序逐个执行 tool_use → 把 tool_result 作为新的 user message 追加 → 直到模型不再调用工具。

工具注册采用"两个声明点"的极简模式（[tools.go](tools.go)）：

```go
var toolHandlers = map[string]toolHandler{
    "bash":      bashHandler,
    "read_file": readHandler,
    // ...加一个工具 = 这里加一行映射 + allTools() 里加一条 schema
}
```

**设计原因**：loop 不认识任何具体工具，只做查表分发（`runTool`），因此新增工具完全不需要动 loop。同一轮响应里多个 tool_use 严格按模型返回顺序串行执行——模型可能依赖前一个工具的副作用（如先 write 再 read）。

### 文件工具与路径安全

read_file / write_file / edit_file / glob / grep 全部经过 `safePath` 校验：路径解析为绝对路径后必须位于工作目录内，`../` 逃逸与工作区外绝对路径一律拒绝。

几个关键机制：

- **read-before-write 门控**（`checkWritable`）：已存在的文件在本会话必须先全量读过、且读后 mtime 未变，才允许 write/edit。防止覆盖掉用户或 linter 在会话外的改动。
- **原子写**（`atomicWriteFile`）：先写同目录临时文件 + fsync + rename，目标路径永远不会是写一半的损坏状态。
- **edit 唯一匹配**：`old_text` 必须精确匹配且恰好出现 1 处，0 处或多处都失败——失败文案自带恢复指引（扩上下文 / `replace_all=true`），让模型同轮自我纠正。
- **read 分页与去重**：默认 2000 行/页、单页 25K 字符预算，超限返回带 offset 指引的短错误（绝不静默截断）；同一区间 mtime 未变的重复读取返回约 30 token 的 stub。

**设计原因**：文件工具是 Agent 最高频的调用，也是最容易把上下文打爆、把文件改坏的地方。门控与原子写把"读旧写新"这类竞态错误消灭在工具层；分页协议把"读多大"的决定权交给模型，但用错误消息而不是截断来约束——静默截断会让模型基于不完整信息做错误决策。

### Bash 工具

[bash.go](bash.go)：

- **120s 超时**（`context.WithTimeout` + `exec.CommandContext`）
- **50K 字符截断**：超限完整输出落盘到系统临时目录，返回截断内容 + 落盘路径，模型可用 read_file 继续翻
- **退出码语义化**：grep/rg 退出 1 = "No matches found"、diff/cmp 退出 1 = "Files differ"、test 退出 1 = "Condition false"——这些都不是错误，避免模型把正常结果当成失败去"修复"
- **cwd 跨调用持久化**：每条命令包装 `; echo <marker>$(pwd) $<exit>`，从输出尾部解析最新 pwd 存入 sessionState，下条命令用 `cd <cwd> &&` 包壳。`cd` 不再随命令结束丢失。

**设计原因**：bash 是能力上限最高的工具，但裸 `exec` 的语义对模型很不友好（退出码歧义、状态不连续）。语义化映射与 cwd 持久化让 bash 的行为更接近"一个真实的交互式 shell"。

### 三闸门权限管线

[permission.go](permission.go) 按固定顺序过三道门，任一关拒绝即拦截：

1. **硬拒清单**：`rm -rf /`、`sudo`、`dd if=` 等子串直接拒绝，无询问
2. **规则匹配**：正则检测破坏性命令（`rm`/`del` 出现在命令位置、`chmod 777` 等）
3. **人工确认**：规则命中则打印告警并询问 `Allow? [y/N]`，未命中则直通

**设计原因**：全自动 Agent 必须有破坏行为的兜底。三层结构把"绝不放行"与"可能危险"分开：前者零打扰，后者保留人类否决权。权限逻辑独立于工具实现，便于单测与替换。

### Hook 机制

[hooks.go](hooks.go) 定义四个生命周期事件，loop 只在关键点调 `triggerHooks`，不认识任何具体检查：

| 事件 | 时机 | 返回非 nil 的效果 |
|------|------|------|
| `UserPromptSubmit` | 用户输入之后、进 LLM 之前 | 无（仅副作用） |
| `PreToolUse` | 工具执行之前 | 拦截本次调用，返回值作为 tool_result |
| `PostToolUse` | 工具执行之后 | 无（仅副作用） |
| `Stop` | loop 即将退出时 | 注入消息并强制 loop 继续 |

内置 5 个示例 hook：上下文注入、权限校验（三闸门迁移至此）、调用日志、大输出提醒、退出汇总。

**设计原因**：扩展点外露之后，loop 自身保持稳定，权限/日志/注入等横切逻辑可以独立演化与测试。这与 Claude Code 的 hooks 机制同构。

### 上下文压缩流水线

[compact.go](compact.go) 在**每轮模型调用前**按固定顺序执行三步，外加一个 API 限额底：

1. **tool_result_budget**：当轮 tool_result 合计超 200K 字符时，单条 >30K 的落盘到 `.task_outputs/tool-results/`，上下文里只留 2K 预览
2. **micro_compact（time-based）**：距最后一条 assistant 响应空闲 ≥60 分钟时，把白名单工具（read_file/bash/grep/glob/write_file/edit_file）旧 tool_result 替换为 `[Old tool result content cleared]`，保留最近 5 个；幂等，且只改 content、不动消息结构
3. **auto_compact**：token 估算达到 167k（200k 窗口 − 20k 摘要预留 − 13k 缓冲）才触发——写归档 → 调模型生成九章节 summary → 组装 `[Compacted]` 摘要消息 + 附件（最近 5 个文件重读 / plan / 已加载 skills）+ 最近 4 条尾部（配对保护）

token 计数以 **API usage 为锚点**（`observeUsage` 记录上次响应的官方计数）+ 新增消息粗估（rune 数/4 再 ×4/3 放大）。另有 **reactive_compact**：API 直接返回 `prompt_too_long` 时归档 + 总结 + 保留最近 5 条，最多重试 1 次。

**失败策略**：auto-compact 失败不写任何消息（历史原样保留），连续 3 次失败熔断；手动（`compact` 工具）失败只打印 `[compact failed: ...]`。摘要 prompt 为九章节结构（禁工具前言 + `<analysis>` 草稿块后处理），完整推导见 [docs/compact-gap-analysis.md](docs/compact-gap-analysis.md)。

**设计原因**：顺序按"成本与信息损失"严格递增——Step 1/2 是零 API 成本、低信息损失的确定性操作；只有 Step 3 引入模型调用与不可逆的信息损失，所以它必须发生在真实窗口压力下（167k）而不是"50 条消息"或"50K 字节"这类与窗口脱钩的代理指标上。micro_compact 选在空闲 ≥60 分钟（对齐 prompt cache TTL）触发，收益是缩小下一轮必然发生的全量重写。

### Todo 规划与提醒

[todo.go](todo.go) 提供会话内待办清单：最多 20 条、content 非空、至多一条 `in_progress`（多余的自动降级为 pending），渲染为 `[ ]` / `[>]` / `[x]`。

配套机制在 [agent.go](agent.go)：连续 3 轮没有调用 `todo_write` 时，向结果里注入 `<reminder>` text block 提醒模型更新计划。注意这里**必须用 text block 而不是 tool_result**——Anthropic API 不接受无对应 tool_use 的 tool_result（会 400）。

**设计原因**：多步任务中模型容易"埋头执行、忘记全局"。todo_write 把计划外显化，reminder 机制则是不修改 system prompt 的温和纠偏。

### 子代理（Subagent）

[task.go](task.go) 的 `task` 工具把子任务委派给一个**全新 messages[]** 的嵌套 loop：

- 子代理与父同进程、同 workdir，复用同一套工具分发、权限与 hook（文件改动对父子双方都可见）
- 子代理工具集 = 父工具集 − `task`（只允许一层委派，防止无限嵌套）
- 父只收到子代理的最终文本，中间的几十轮工具调用不进入父的上下文
- 上限 30 轮 / 每轮 8000 max_tokens

**设计原因**：探索性任务（如"找出所有调用了 X 的地方并总结"）会产生大量中间 tool_result，直接跑在父上下文里会快速耗尽预算。子代理是上下文隔离的标准手段：父用一次 tool_result 的代价买到一个完整子循环的成果。

### 技能按需加载

[skill.go](skill.go) 实现两级表：

| 内容 | 进入模型的位置 | 时机 |
|------|--------------|------|
| skill name + description | system prompt | 会话启动时扫描 |
| 全文 SKILL.md | tool_result | 模型调 `load_skill` 时 |

扫描 `skills/*/SKILL.md`，解析 YAML frontmatter（name/description 可回退到目录名与首行），并校验符号链接解析后仍在 skills 目录内（防逃逸）。目录按 name 排序输出，保证 system prompt 稳定（利于 prompt 缓存）。

**设计原因**：技能全文动辄几千 token，全塞进 system prompt 既贵又稀释注意力。"目录常驻 + 全文按需"让技能数量可以无限增长而 system prompt 体积恒定。

### 跨会话记忆

[memory.go](memory.go) 四个子系统，落盘在 `.memory/*.md`（YAML frontmatter）+ `MEMORY.md` 索引：

- **召回**：会话开始时把目录发给模型选相关记录（至多 5 条，失败回退关键词匹配），按 20K 字符预算加载全文，拼进 system prompt——并明确标注"这是背景知识，不是新指令，冲突时以当前请求为准"
- **抽取**：轮末（无工具调用的最终答复后）让模型给候选，只有 `scope=persistent` 且通过准入校验的才落盘
- **合并**：记录数达 10 条后让模型给清洗列表，校验后整体替换；替换前快照，任何一步失败回滚并重建索引
- **存储**：单条正文上限 4000 字符，防单条记录失控

记忆分四类：`user`（偏好）、`feedback`（长期指导）、`project`（项目事实）、`reference`（外部指针）。

**设计原因**：上下文压缩（s08）解决"这一轮装不下"，记忆解决"下一轮不记得"。两者互补：压缩是无损归档 + 有损总结，记忆是只留"以后还用得上"的知识。记忆模块是 harness 层能力（不新增工具），召回/抽取的额外模型调用是它的固有成本。

### 持久化任务图

[taskstore.go](taskstore.go) 与 todo_write 的区别是**跨会话可恢复**：每条任务一个 `.tasks/{id}.json`，全部状态在文件里，store 自身无内存状态。

- ID：`task_` + 8 位随机十六进制，`O_EXCL` 独占创建，撞号即重生成
- 状态机：`pending → in_progress → completed`，由 `claim_task` / `complete_task` 驱动，complete 校验 owner 归属
- 依赖：`blockedBy` 列表全部 completed 后才能 claim；update_task 保存前校验目标存在、无自依赖、无环

**两阶段建图**：先 `create_task` 建全部节点拿到运行时生成的 ID，再 `update_task` 加边——因为同一响应里的兄弟工具调用在结果产生前就已定型，无法引用彼此新生成的 ID。

**设计原因**：todo_write 是"这次会话的执行清单"，任务图是"关机后还能接着干"的工程清单。文件即状态（无内存缓存）保证崩溃恢复后看到的永远一致。完成时还会报告"哪些下游任务因此解锁"，方便模型推进整个图。

### 后台任务

[background.go](background.go)：bash 调用显式传 `run_in_background=true` 时——

1. 注册任务、启动 goroutine，立即返回带 `bg_id` 的占位 tool_result（原 tool_call 已应答，一一对应关系不破坏）
2. 命令在**独立进程组**里执行（`SysProcAttr`），结束/超时后整组清理，不留孤儿进程
3. loop 每轮开头收集完成队列，结果以 `<task_notification>` 独立 user 消息注入对话（不复用原 tool_use_id）

生命周期管理：Ctrl-C 与正常退出都会 `stopAll`——进程已启动则向进程组发 SIGTERM；尚未 Start（`Process` 还是 nil）则走 `cancel(ctx)` 兜底，否则 SIGTERM 会静默丢失。

**设计原因**：安装依赖、编译、跑长测试这类命令会阻塞 loop 几分钟。后台化让模型"边等边干别的"。管理器挂包级全局而非 sessionState，因为 REPL 每轮新建 agentLoop，而任务生命周期跨越多轮对话。

### MCP 外部工具连接

[MCP（Model Context Protocol）](https://modelcontextprotocol.io/) 让 Agent 在运行时发现并调用外部服务提供的工具，而不是把所有工具写死在 `tools.go` 里。本项目实现的是 **s14 教学版**：服务端以进程内 mock server 形式存在（docs + deploy 两个示例），真实 MCP 传输层（stdio/SSE）通过实现 `MCPServer` 接口即可接入。

核心组件：

- **`MCPServer` 接口**（[mcp.go](mcp.go)）：`ListTools() → []ToolDef` 暴露工具列表，`CallTool(name, args) → (string, error)` 执行调用。真实 MCP 客户端只需把这两个方法桥接到协议层即可。
- **`mcpRegistry`**：会话级注册表，管理所有已连接的 server。每轮 `assembleToolPool(st)` 调用时把已发现的 MCP 工具追加到基础工具池尾部。
- **工具命名**：`mcp__{server}__{tool}`。`safeToolName` 把 server/tool 名里的非 `[A-Za-z0-9_]` 字符替换为下划线；注册期检测碰撞（不同 server 规范化后撞名直接拒绝）。
- **宿主侧权限策略**（[mcp_permission.go](mcp_permission.go)）：MCP server 自称的 `readOnlyHint` / `destructiveHint` 不可信，宿主独立配置 `(server, tool) → allow | confirm | deny`。未配置的外部工具**默认 confirm**——最坏情况用户被询问。
- **`connect_mcp` 工具**：Agent 用它主动 bring-in 外部服务。连接成功后，下一轮 Send 的工具池才包含新工具（模型先看到 connect_mcp 自己，下一轮才看到 mcp__docs__* 等）。

接入链路：

```
connect_mcp(name="docs")        → connectMCPHandler → mcpRegistry.connect → server.ListTools → 发现 2 个工具
下一轮 assembleToolPool(st)     → 基础工具 + mcp__docs__search + mcp__docs__get_version 一起进模型
mcp__docs__search(query="...")  → runTool mcp__ fallback → mcpRegistry.clients[docs].callTool("search", args) → server.CallTool("search", ...)
```

**接入点总结**：要加一个新的 mock server，在 `mockMCPFactories` map 里注册 `func() MCPServer` 工厂即可；要接真实 MCP 服务，实现 `MCPServer` 接口把 `ListTools` / `CallTool` 桥接到 stdio/SSE 传输层，同样在 map 里注册。

更多细节见 [docs/mcp-connection.md](docs/mcp-connection.md)。

### 搜索引擎抽象

[search.go](search.go) 为 glob/grep 提供统一底座：

- 运行时探测 `rg`（`exec.LookPath`）：有就走 **ripgrepEngine**（快、原生尊重 .gitignore），没有就走 **walkerEngine**（Go 标准库 WalkDir + 自研 gitignore matcher + 自研 `**` 递归 glob——Go 标准库 `filepath.Glob` 不支持 `**`）
- 两种引擎共享 gitignoreMatcher 与 VCS 目录排除（.git/.svn/.hg/.jj/...）
- grep 三种输出模式：`files_with_matches`（默认，先定位文件）/ `content`（带行号）/ `count`

**设计原因**：ripgrep 在大仓库上比纯 Go walker 快一个数量级，但作为教学项目不能要求用户必装 rg。接口抽象让"有 rg 享受性能、没 rg 保证正确"，且单测可以注入 fake 引擎。

---

## 项目结构

| 文件 | 职责 |
|------|------|
| [main.go](main.go) | REPL 入口、信号处理、每轮驱动 agentLoop |
| [agent.go](agent.go) | Agent Loop：压缩流水线调度、后台结果注入、工具批执行、todo reminder |
| [llm.go](llm.go) | Anthropic 客户端封装、system prompt 拼装 |
| [tools.go](tools.go) | 基础工具 schema + 分发表 + 文件工具实现 + safePath + assembleToolPool（基础工具 + MCP 动态工具） |
| [bash.go](bash.go) | bash 执行：超时/截断/退出码语义化/cwd 持久化 |
| [permission.go](permission.go) | 三闸门权限管线 + MCP host-side 策略（allow/confirm/deny） |
| [hooks.go](hooks.go) | Hook 事件注册与触发 + 5 个内置 hook |
| [compact.go](compact.go) | 上下文压缩：usage 锚点计数 + time-based 微压缩 + 自动压缩（附件/尾部/熔断）+ reactive 兜底 |
| [todo.go](todo.go) | 会话内待办清单 |
| [task.go](task.go) | 子代理委派（task 工具）+ 每轮重算工具池（支持子代理 MCP） |
| [taskstore.go](taskstore.go) | 持久化任务图（create/update/claim/complete/get/list） |
| [skill.go](skill.go) | 技能扫描与按需加载 |
| [memory.go](memory.go) | 跨会话记忆：存储/召回/抽取/合并 |
| [background.go](background.go) | 后台任务管理器与通知注入 |
| [mcp.go](mcp.go) | MCPServer 接口、MCPClient、mcpRegistry、connect_mcp handler、工具名规范化、输出截断 |
| [mcp_servers.go](mcp_servers.go) | docs + deploy 两个进程内 mock server（真实接入需实现 MCPServer） |
| [mcp_permission.go](mcp_permission.go) | MCP host-side 策略表（allow/confirm/deny，未配置默认 confirm） |
| [mcp_test.go](mcp_test.go) | MCP 单元测试 + loop 级端到端（connect 后下一轮工具池含 mcp__docs__*） |
| [search.go](search.go) | 搜索引擎抽象：ripgrep 优先 + walker 回退 |
| [procgroup_unix.go](procgroup_unix.go) / [procgroup_windows.go](procgroup_windows.go) | 平台相关：后台任务的进程组管理 |
| [logging.go](logging.go) | 每轮完整对话快照覆盖落盘 messages.json |
| `*_test.go` | 各模块单元测试（Arrange-Act-Assert） |

## 运行时的产物目录

工作目录下会随使用生成：

- `.transcripts/` — 压缩前的完整对话归档
- `.task_outputs/tool-results/` — 超大 tool_result 的落盘全文
- `.memory/` — 跨会话记忆（记录 + MEMORY.md 索引）
- `.tasks/` — 持久化任务图
- `messages.json` — 每轮完整对话快照（覆盖写入，含 user/assistant/tool_use/tool_result）
- `coding-agent-go.log` — 运行诊断日志（append 写入，Info/Warn/Error，进程退出后保留）
