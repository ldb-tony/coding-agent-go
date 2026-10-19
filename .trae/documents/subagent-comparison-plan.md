# s06 子代理 vs Claude Code 子代理 — 对比分析文档 实施计划

## Summary

不写任何业务代码。产出一份**深度对比分析文档**，对比「我方 s06 子代理（`task` / `run_subagent`）」与「Claude Code 子代理能力（Agent 工具 + 子代理系统）」，量化差距并给出需要补齐的功能清单与 Go 侧对齐草案。沿用 `docs/tool-comparison/` 系列既有的固定 10 节结构与中文写作标准。

范围：**仅聚焦子代理能力**（用户已确认），不扩展到 skill loading / MCP / 后台任务全景等后续 lessons。

## 主要差异（按重要性降序，序号越小越关键）

1. **没有类型化子代理**：我方只有单一通用 `task`；CC 有类型注册表（内置 Explore / Plan / general-purpose + frontmatter/`AgentDefinition` 自定义），按 `description` 路由。
2. **没有 per-subagent 工具与权限差异**：我方 `SUB_TOOLS` 固定为全部 base 工具、共享同一套 hooks/permission；CC 可为每个子代理限定 `tools`/`disallowedTools`（含只读类型 Write/Edit 被拒）与 `permissionMode`。
3. **没有模型与成本路由**：我方固定 `MODEL`；CC 可 per-subagent `model`（route 到 Haiku 等）与 `effort` 档位。
4. **没有自动委派**：我方只能显式调 `task`；CC 由子代理 `description` 匹配自动委派，也可按名显式调用。
5. **执行模型为同步阻塞单层**：我方 `run_subagent` 阻塞父 loop、无并发；CC Agent 调用默认后台执行（`run_in_background`），并有 depth / concurrency / spend 上限。
6. **没有 `maxTurns` 部分结果 + resume**：我方 30 轮硬停只返回固定文本 `"stopped after 30 turns…"`；CC 达限返回标记为 partial 的输出且可 resume 续跑。
7. **没有 auto-compaction**：子代理上下文超限时我方无处理；CC 有 auto-compact（含 `autoCompactWindow`）。
8. **没有子代理输出扫描**：我方最终文本直接回父；CC（v2.1.210+）扫描 instruction-shaped pattern（control-tag 仿冒、`Human:`/`Assistant:` 轮次标记）并做最小转义。
9. **没有 API 错误语义隔离**：CC 明确子代理 API 错误（如限流）绝不作为其结果返回；我方片段未处理该路径。
10. **没有启动内容控制**：CC 可控制子代理初始上下文（项目 CLAUDE.md、`omitClaudeMd`、extended thinking 继承）；我方只有一段固定 `SUB_SYSTEM`。
11. **没有 skills 预载 / memory / MCP 作用域**：CC 支持 `skills`/`memory`/`mcpServers`；我方无。
12. **没有 fork 模式**：CC 可 fork 当前对话（继承父上下文）作为子代理；我方一律 fresh messages。
13. **没有子代理名字与宿主可视化**：CC transcript 以 `name(desc)` 呈现子代理调用；我方无命名/宿主集成。
14. **返回形态**：两方都只回最终文本，但 CC 额外带 partial 标记与调用可探测性（`detect subagent invocation`），我方为纯字符串。

**相同点（非差异，撰写时需点明）**：全新 `messages[]`（上下文隔离）、同进程同 `WORKDIR`（文件系统共享）、只回最终文本、共享父的权限/生命周期 hook——这四点 s06 与 CC 的设计取向一致，差距在**深度**而非方向。

## Current State Analysis

### 我方现状（s06 参考实现，即本次上传的 README 描述的对象）

来自上传的 s06 README（[attachments/…_README.md](file:///Users/tony/.trae-cn/attachments/f9162af0-cf10-4070-95bc-cca7257c64d8_README.md)），关键机制：

- `run_subagent(prompt)`：新建 `messages = [{"role":"user","content":prompt}]`（**全新**，父历史不复制）
- `SUB_TOOLS = list(BASE_TOOLS)`——**不含 `task`**，即只允许一层委派
- 循环上限 **30 轮**，每轮 `client.messages.create(model=MODEL, system=SUB_SYSTEM, tools=SUB_TOOLS, max_tokens=8000)`
- 无 `tool_use` 时返回 `extract_text(response.content) or "(no summary)"`
- 工具调用**逐个串行**执行（`execute_tool(block, SUB_HANDLERS)`），结果作为 user 消息里的 `tool_result`
- 超过 30 轮未收敛返回固定文本 `"Subagent stopped after 30 turns without a final answer."`
- 与父 **同进程、同 `WORKDIR`**（消息隔离而非进程/文件系统隔离），**共享权限与生命周期 hook**
- 父只得到**最终文本**，子的中间对话不入父 `messages[]`
- 边界表（README 原文）：Conversation = Fresh messages[]；Execution = Same process & WORKDIR；Return = Final text only；Depth = No `task` in SUB_TOOLS（一层）；Tool policy = Shared hooks

### 本项目（coding-agent-go）现状

已实现 s01–s05，**尚无 `task`/子代理**（全仓库 grep `subagent|task tool|run_subagent|SUB_TOOLS` 无命中）。可复用的架构接缝：

- 工具分发：[tools.go L112-L128](file:///Users/tony/coding-agent-go/tools.go#L112-L128)（`toolHandlers` map + `runTool`）
- 会话状态：[tools.go L79-L89](file:///Users/tony/coding-agent-go/tools.go#L79-L89)（`sessionState`，同时被 read 去重、todo 复用）
- Loop：[agent.go L10-L85](file:///Users/tony/coding-agent-go/agent.go#L10-L85)
- Hook 机制：[hooks.go](file:///Users/tony/coding-agent-go/hooks.go)（PreToolUse 权限、PostToolUse、Stop）
- System prompt 与 LLM 封装：[llm.go L59-L77](file:///Users/tony/coding-agent-go/llm.go#L59-L77)

### 已有对比系列与惯例

[README.md](file:///Users/tony/coding-agent-go/docs/tool-comparison/README.md) 定义了固定 10 节结构与写作标准（中文、CC 行为必须有来源、我方行为引用到行、量化推演标"估算"、报告间互链）。现有 01–06 + 索引，**07 号位空缺**（[todo-write-comparison-plan.md](file:///Users/tony/coding-agent-go/.trae/documents/todo-write-comparison-plan.md) 曾计划占用 07 但未落盘）。

### Claude Code 侧事实（已核实，供撰写时引用）

来源：官方 [Create custom subagents](https://code.claude.com/docs/en/sub-agents) 与 [Subagents in the SDK](https://code.claude.com/docs/en/agent-sdk/subagents)（访问于 2026-10-19）。

- **子代理是独立上下文窗口 + 自定义 system prompt + 指定工具集 + 独立权限**；按 `description` 匹配自动委派，也可按名字显式调用
- **内置子代理类型**：Explore（只读，Write/Edit 被拒，支持 quick/medium/very thorough 三档彻底度）、Plan（只读，plan mode 研究）、general-purpose（全工具）；另有 `claude`/`statusline-setup`/`claude-code-guide` 等辅助代理
- **自定义定义**：Markdown + YAML frontmatter 或 SDK `AgentDefinition`。字段：`description`、`prompt`、`tools`、`disallowedTools`、`model`、`skills`、`memory`、`mcpServers`、`initialPrompt`、`maxTurns`、`autoCompactWindow`、`background`、`omitClaudeMd`、`effort`、`permissionMode`
- **继承规则**：非 fork 子代理上下文**全新**，父→子只传 Agent 工具的 `prompt` 字符串；继承项目 CLAUDE.md（除非 `omitClaudeMd`）、工具定义、扩展思考配置；**不继承**父对话历史/tool_result、父 system prompt、未列出的预载技能
- **执行**：`Agent` 工具调用默认**后台执行**（省略 `run_in_background` 即后台；需结果时才传 `run_in_background:false`）；子代理**可再生成子代理**，但有 depth / concurrency / spend 上限；`maxTurns` 达限返回标记为 partial 的输出且可 resume
- **模型/成本**：per-subagent `model`（别名 `fable/opus/sonnet/haiku/inherit` 或全 ID）；`CLAUDE_CODE_SUBAGENT_MODEL`；`effort` 档位
- **输出与安全**：v2.1.210+ 对子代理最终消息做 **instruction-shaped pattern 扫描**（control-tag 仿冒就地转义、权限配置引用保留、`Human:`/`Assistant:` 轮次标记插反斜杠，命中加 `[harness: …]` 标记行）；**子代理 API 错误绝不作为其结果返回**
- **其他**：可 resume 子代理、auto-compaction、fork 当前对话（继承父对话，与普通子代理相对）、子代理名字用于 transcript 可视化、`Agent` 调用缺 `subagent_type` 且无 general-purpose 时报错

## Proposed Changes

### 1. 新建 `docs/tool-comparison/07-subagent-vs-agent-tool.md`

沿用 10 节结构（与 01–06 完全对齐）。各节内容纲要：

| 节 | 内容要点 |
|---|---|
| 1. 定位与设计哲学 | 我方：s06 教学实现——"给子任务一个独立上下文"的**最小机制**（fresh messages + 只回最终文本），单一通用子代理、单层委派、共享 hook；CC：**类型化子代理系统**——把委派上升为一等公民（类型注册表、工具/模型/权限差异化、后台并发、深度/成本上限、输出扫描）。点明二者**同源**（上下文隔离）但**深度不同**（教学骨架 vs 生产系统工程） |
| 2. 功能描述（description）对比 | 我方 `task` 一句话描述（"Run a subagent with fresh conversation context and return its final text."）vs CC Agent 工具描述 + per-type `description`（委派匹配的依据）；分析"描述即路由表"如何影响自动委派 |
| 3. API/Schema 逐项对比 | 我方 `{prompt}`（唯一参数）vs CC Agent 工具 `{prompt, description, subagent_type, model, resume, run_in_background}`；我方无定义文件 vs CC frontmatter/`AgentDefinition` 15 字段；逐项表 + 差异点评 |
| 4. 执行机制对比（源码级） | ① fresh messages 相同点；② **同步阻塞单层** vs CC 默认后台 + 并发上限 + 嵌套深度上限；③ 30 轮硬上限 vs `maxTurns`（partial+resume）；④ 无 auto-compaction vs 有；⑤ hook 共享 vs per-subagent `permissionMode`/工具限制；⑥ 同进程同 WORKDIR 两方一致 |
| 5. 输出契约与边界行为 | 我方：`extract_text or "(no summary)"`；超轮固定文本；子 API 错误直接崩或吞 vs CC：final report + 扫描（三态处理）+ partial 标记 + API 错误绝不外泄 + 可 resume |
| 6. 状态、安全与权限 | 我方共享 `hooks`/permission、无 per-type 工具限制、无模型隔离；CC：`tools`/`disallowedTools`/只读类型/`permissionMode` 继承/`mcpServers` 作用域/skills 预载/memory；指出"文件系统共享"是二者共识 |
| 7. 量化场景推演（估算） | 至少 2–3 个场景：① "读全仓定位某框架"→ 子代理 30 轮内多轮读文件，父仅收 1 段结论，对比 CC 自动委派 Explore（只读+低模型）成本差异；② "并行研究 3 个模块"→ 我方串行阻塞 3×，CC 后台并发；③ 子代理中途超轮/超上下文 → 我方丢结果，CC partial+resume。标注估算假设 |
| 8. 差距清单评级 | P0–P2 表（见下方"待补齐功能清单"） |
| 9. 对齐设计草案（仅设计，不实施） | Go 侧分阶段：① `task.go` + `taskHandler` + `runSubagent`（嵌套 agent loop，复用 `runTool`/`hooks`，独立 `messages`，`SUB_TOOLS = allTools()` 去掉 `task`）；② 类型化子代理（`subagentType` 枚举 + `description` 路由 + per-type `tools` 白/黑名单）；③ 模型/effort 与 `maxTurns`/partial；④ 后台 `run_in_background` + 并发/深度上限 + TaskStop/通知。含函数签名、schema 草案、测试清单、破坏性变更提示 |
| 10. 参考资料 | 官方 sub-agents / agent-sdk subagents 文档（访问日期）+ s06 README + 我方代码链接 |

**待补齐功能清单**（§8 表，即用户要的"需要补齐"结论）：

- **P0**：无 `task` 工具本体（本项目尚未实现）；无 `subagentType`/类型注册表（Explore/Plan/general-purpose 及自定义）；无 per-subagent 工具限制；无 per-subagent 模型选择（成本路由）；无 description 驱动自动委派
- **P1**：同步阻塞单层（无后台/并发/深度上限）；无 `maxTurns` partial+resume；无 auto-compaction；无子代理输出扫描（instruction-shaped pattern）；无 API 错误语义隔离；无 per-subagent `permissionMode`；无启动内容控制（CLAUDE.md/`omitClaudeMd`/extended thinking 继承）
- **P2**：无 skill 预载 / memory / MCP 作用域；无 fork 模式；无子代理名字与宿主可视化；无结构化返回（仅 final text）

### 2. 更新 `docs/tool-comparison/README.md`

- 索引表新增一行（`task` / 子代理 ↔ Claude Code `Agent`/子代理系统），一句话结论指向文档
- 跨工具总差距矩阵新增一行维度「委派与上下文隔离」（CC：类型化子代理 + 自动委派 + 后台/深度上限 + 输出扫描；我们：单层同步 fresh-context 委派，无类型/无隔离策略）
- 视需要把概述里的"6 个工具"表述改为"6 个工具 + 子代理委派"

## Assumptions & Decisions

- **我方 = 上传的 s06 参考实现**（README 所述 `task`/`run_subagent`），**不是**本项目 Go 代码（本项目尚无子代理）。§9 专门给出 Go 对齐草案，保持系列惯例。
- **文件名采用 `07-subagent-vs-agent-tool.md`**：现有 07 号位空缺，本次占用；[todo-write-comparison-plan.md](file:///Users/tony/coding-agent-go/.trae/documents/todo-write-comparison-plan.md) 对应的文档若日后落盘，顺延为 08。
- **不写码、不改 `.go`/测试**，仅文档 + README 索引更新。
- **CC 事实源**：以官方 sub-agents 与 agent-sdk subagents 文档为权威（标注"2026-10-19 访问"）；数值/行为凡可能随版本漂移处标注版本。我方引用片段逐字对齐上传的 README。
- 写作风格、互链、量化推演"估算"标注，全部对齐既有 6 份报告。

## Verification

- 新文档含完整 10 节，结构与 01–06 一致（中文、`file://` 代码链接、外链）
- 「待补齐功能清单」覆盖上表 P0–P2 全部条目，逐条有 CC 侧来源支撑
- 我方引用的 README 片段逐字准确；CC 事实核对官方文档对应小节
- `docs/tool-comparison/README.md` 索引表与跨工具矩阵更新后格式正确、无断链（Markdown lint：表格对齐、链接可达）
- 量化推演均标注"估算"及假设，不伪装实测
