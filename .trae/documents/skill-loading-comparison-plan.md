# s07 Skill Loading vs Claude Code Skills — 对比分析文档 实施计划

## Summary

不写任何业务代码。产出一份**深度对比分析文档**，对比「s07 教学实现（`SkillLoader` 扫描 + system prompt 目录 + `load_skill` 工具）」与「Claude Code Skills 系统（Agent Skills 开放标准 + `Skill` 工具 + 多位置分发 + 调用控制）」，量化差距并给出 P0–P2 补齐清单与 Go 侧对齐草案。沿用 `docs/tool-comparison/` 系列固定 10 节结构与中文写作标准。

范围（用户已确认）：**Skill 机制对比为主体**，另在 §1 设一小节扫过 CC 相邻的"能力按需加载"机制（ToolSearch 延迟加载 MCP 工具、子代理 skills 预载、CLAUDE.md 分级）并声明边界。

## Current State Analysis

### 我方对象：上传的 s07 参考实现

来自上传的 s07 README（[attachments/…_README.md](file:///Users/tony/.trae-cn/attachments/f949976d-a22b-4942-a711-34e8b8cd2dfc_README.md)），关键机制：

- `SkillLoader.scan()`：启动时扫描 `skills/*/SKILL.md`，解析 YAML frontmatter 取 `name`/`description`；name 缺省回退目录名，description 缺省回退正文首行（去 `#` 前缀）；扫描时做 `is_relative_to` 路径逃逸校验
- `catalog()`：只产出 `- name: description` 行，由 `build_system_prompt()` 拼进 system prompt（`"Skills available:\n{catalog}\n\nUse load_skill to read the full instructions when a skill applies."`）
- `load_skill(name)` 工具：按名查注册表（**非文件路径**），命中返回 `SKILL.md` 全文，未命中返回 `Error: Unknown skill '{name}'. Available: {列表}`；结果经 agent loop 作为 `tool_result` 入消息列表
- README 自带两级表：name+description → system prompt（启动时）；SKILL.md 全文 → tool_result（调用时）
- 解决的问题（README 原文）：全量规范文档塞 system prompt 导致每次调用都为无关文档付 token

### 本项目（coding-agent-go）现状

已实现 s01–s05 + todo_write，**无任何 skill 机制**（全仓库 grep `(?i)skill` 仅命中 `.trae/documents/` 计划文件）。可复用接缝：

- 工具注册：[tools.go](file:///Users/tony/coding-agent-go/tools.go) 的 `allTools()` + `toolHandlers`（"加一个工具只加一行映射"）
- 会话状态：[tools.go](file:///Users/tony/coding-agent-go/tools.go) 的 `sessionState`（todo/read 去重已用）
- System prompt 拼装：[llm.go](file:///Users/tony/coding-agent-go/llm.go)
- 路径安全：`safePath`（tools.go，与 s07 的 `is_relative_to` 校验同构）

### 系列现状与编号

[docs/tool-comparison/README.md](file:///Users/tony/coding-agent-go/docs/tool-comparison/README.md) 定义固定 10 节结构；现有 01–06 + 索引。**07 已被 [subagent-comparison-plan.md](file:///Users/tony/coding-agent-go/.trae/documents/subagent-comparison-plan.md) 占用、08 顺延给 [todo-write-comparison-plan.md](file:///Users/tony/coding-agent-go/.trae/documents/todo-write-comparison-plan.md)（均未落盘），本篇占用 09。**

### Claude Code 侧事实（已核实，2026-10-19 访问）

来源：官方 [Skills](https://code.claude.com/docs/en/skills)、[Tools reference](https://code.claude.com/docs/en/tools-reference)、[MCP](https://code.claude.com/docs/en/mcp)、[Agent Skills 开放标准](https://agentskills.io/specification)。

- **开放标准**：CC skills 遵循 Agent Skills 标准。SKILL.md = YAML frontmatter + Markdown 正文；frontmatter 标准字段：`name`（≤64 字符、小写+连字符、须匹配目录名）、`description`（≤1024 字符）、`license`、`compatibility`（≤500 字符）、`metadata`、`allowed-tools`（实验性，空格分隔预批准工具）
- **三级渐进披露**（标准原文）：① Metadata ~100 tokens——所有 skill 的 name+description 启动时加载；② Instructions <5K tokens 推荐——激活时加载 SKILL.md 全文（建议 <500 行）；③ Resources 按需——`scripts/`（可执行，不进上下文）、`references/`（按需 Read）、`assets/`（模板/数据）；文件引用建议一层深
- **Skill 工具**（tools-reference 原文）：*"Executes a skill within the main conversation"*；权限规则格式 `Skill(deploy *)`
- **加载位置 7 类**：Enterprise（managed settings）、Personal（`~/.claude/skills`）、Project（`.claude/skills`）、Nested（子目录，会话进入时加载）、Additional（`--add-dir`）、Plugin（`<plugin>/skills/`，命名为 `/plugin:skill`）、claude.ai 账户同步；支持 symlink；保留名 `synced`/`anthropic-skills`；同名冲突有解析规则
- **调用控制**：模型按 description 自动调用 + 用户 `/skill-name` 直调；`disable-model-invocation: true` 可禁止模型调用；自定义命令已并入 skills（`.claude/commands/x.md` ≡ `.claude/skills/x/SKILL.md`）
- **动态上下文注入**：SKILL.md 中 `` !`command` `` 行在 Claude 看到内容前先执行并替换为输出；注入命令有权限检查
- **bundled skills**：`/doctor`、`/code-review`、`/batch`、`/debug`、`/loop`、`/claude-api`、`/run`、`/verify` 等；`disableBundledSkills` 可关；`skillOverrides` 可见性控制
- **其他**：skill 可在子代理中执行（fork）；live change detection（会话中编辑 skill）；`allowed-tools` 预批准与 managed permission 交互；skill-creator 可跑 eval
- **相邻机制**（§1 简述用）：`ToolSearch` 工具——*"Searches for and loads deferred tools when tool search is enabled"*（tools-reference 原文）；tool search 默认开启（Opus/Sonnet/Haiku 4.5+，`ENABLE_TOOL_SEARCH=false` 关闭，关闭时用 `WaitForMcpServers` 兜底）
- **执行时补查**：CC 专有 frontmatter 完整字段表（`user-invocable`、`argument-hint`、`model`、`effort`、`context: fork`、`hooks` 等）在 skills 页 Frontmatter reference 小节，本次 WebFetch 被截断未取全，撰写时再做一次定向抓取补全并标注

## Proposed Changes

### 1. 新建 `docs/tool-comparison/09-load_skill-vs-Skill.md`

沿用 10 节结构（与 01–06 对齐）。各节纲要：

| 节 | 内容要点 |
|---|---|
| 1. 定位与设计哲学 | s07：教学最小机制——"目录进 system prompt、全文按需进 tool_result"，直接回答"全量塞 system prompt 浪费 token"问题；CC：把同一件事做成**开放标准 + 产品化生态**（标准字段约束、三级披露、7 类位置、plugin/同步分发、命令合并）。点明 s07 实现了三级披露的前两级（元数据→目录、全文→tool_result），缺第三级（resources 按需）。**小节 1.3 扫过相邻机制**：CC 的"能力按需加载"全景——Skills（知识）、ToolSearch（MCP 工具 schema 延迟加载）、子代理 skills 预载、CLAUDE.md 分级——并声明本篇只深比 Skills |
| 2. 目录与描述（catalog）对比 | s07 `catalog()` 纯文本拼接、无长度管理 vs 标准 name/description 字段约束（64/1024 字符、name 须匹配目录名）+ CC 描述截断告警（troubleshooting: descriptions are cut short）+ `skillOverrides` 可见性；description 即"模型路由依据"两方一致，但 CC 有写作规范与故障排查文档 |
| 3. API/Schema 逐项对比 | s07 `load_skill(name)` 单参数 vs CC `Skill` 工具（主对话内执行）+ `/skill-name` 用户直调 + 参数传递；frontmatter 字段逐项表（s07 仅用 2 个标准字段且为可选回退 vs 标准 6 字段 + CC 扩展字段）；错误契约：s07 `Unknown skill + Available 列表` vs CC 侧行为 |
| 4. 执行机制对比（源码级） | ① 发现：启动一次性扫描 + 逃逸校验 vs 7 类位置 + symlink + 保留名 + 同名解析 + live change detection；② 加载：全文一次性进 tool_result vs 三级披露（~100 tok/<5K tok/按需，引标准原文）+ scripts 执行不占上下文 + references 按需 Read；③ 调用：仅模型主动 vs 模型自动 + 斜杠命令 + `disable-model-invocation` 控制 + `` !`cmd` `` 动态注入（先执行后呈现）+ fork 子代理执行；④ 注入点：s07 catalog 在 system prompt vs CC 目录注入形态；⑤ 失效：s07 unknown 错误 vs CC 失败报告语义 |
| 5. 输出契约与边界行为 | s07：tool_result 全文，无大小概念（大 SKILL.md 一次全返）vs CC：<5K tokens/<500 行写作约束、超限拆 references；注入命令失败语义；skill 内容作为指令在主对话生效 |
| 6. 状态、安全与权限 | s07：扫描期 `is_relative_to` 逃逸校验、无权限模型 vs CC：`Skill(deploy *)` 权限规则（tools-reference 原文）、`allowed-tools` 预批准、managed permission、注入命令权限检查、enterprise 位置分发即管控 |
| 7. 量化场景推演（估算） | ① README 自带场景（3 个规范文档）：全量塞 system prompt vs s07 两级 vs CC 三级+references 的每轮 token 对比；② 规模化：20 个 skill 的目录成本（s07 无约束 vs CC ~100 tok×20 + 描述截断告警）；③ 大 skill：8K token SKILL.md 时 s07 一次全返 vs CC 拆 references 按需加载。全部标注"估算"及假设（~4 字符/token） |
| 8. 差距清单评级 | P0–P2 表（见下） |
| 9. 对齐设计草案（仅设计，不实施） | Go 侧分阶段：① `skill.go` `SkillLoader`（scan/catalog/load）+ `load_skill` 工具注册进 `toolHandlers` + `buildSystemPrompt` 拼目录（对齐 s07 行为，复用 `safePath` 思路）；② frontmatter 完整解析与校验（name 规范/匹配目录名、description ≤1024）+ 目录截断管理；③ supporting files：references 经现有 `read_file` 按需读、scripts 经 `bash` 执行（结果进上下文而非文件全文）；④ 权限规则 `Skill(name *)` 接入 [permission.go](file:///Users/tony/coding-agent-go/permission.go) 管线；⑤ REPL `/skill-name` 入口与 `disable-model-invocation`。含函数签名、schema 草案、测试清单 |
| 10. 参考资料 | skills / tools-reference / agentskills.io spec / MCP tool search / s07 README，均标注 2026-10-19 访问 |

**差距清单**（§8 表）：

- **P0**：无 frontmatter 字段约束与校验（name 规范、description 上限、name 匹配目录名）；无 supporting files 概念（scripts/references/assets）——skill 只能是单文件全文，无第三级披露；无调用控制（模型/用户双通道、`disable-model-invocation`）
- **P1**：无多位置加载分层（personal/project/plugin/enterprise）；无动态上下文注入（`` !`cmd` ``）；无 fork 子代理执行；无参数传递；catalog 无描述长度管理与截断告警；无权限规则（`Skill(name *)`、`allowed-tools`）
- **P2**：未对齐开放标准（agentskills.io 跨工具可移植）；无 bundled skills 与命令合并；无 live change detection；无 `skillOverrides`；无 eval 工具链；无 claude.ai 同步

### 2. 更新 `docs/tool-comparison/README.md`

- 索引表新增一行（`load_skill`/s07 skill loading ↔ Claude Code `Skill`/Skills 系统），一句话结论指向 09 文档（07/08 号位按计划留空）
- 跨工具总差距矩阵新增维度「知识与能力加载」（CC：开放标准 + 三级渐进披露 + 多位置分发 + 调用控制 + 权限规则；我们：catalog + load_skill 最小机制，无 supporting files、无权限）
- 视需要把"6 个工具"表述扩为"6 个工具 + 委派/规划/知识加载"

## Assumptions & Decisions

- **我方 = 上传的 s07 Python 参考实现**（README 所述 `SkillLoader`/`load_skill`），**不是**本项目 Go 代码（尚无 skill 机制）；§9 给 Go 对齐草案，保持系列惯例
- **编号 09**：尊重既有计划对 07/08 的占用；索引表允许号位空缺
- **不写码、不改 `.go`/测试**，仅文档 + README 索引更新
- **CC 事实源**：官方 skills/tools-reference/MCP 文档 + agentskills.io 标准（标注"2026-10-19 访问"）；Frontmatter reference 完整字段表执行时补一次定向抓取，取不到的字段标注"未核实"
- 写作风格、`file://` 行级链接、量化推演"估算"标注，全部对齐既有 6 份报告

## Verification

- 新文档含完整 10 节，结构与 01–06 一致（中文、行级代码链接、外链、相邻机制小节边界声明）
- §8 差距清单覆盖上述 P0–P2 全部条目，CC 侧逐条有官方来源支撑（blockquote 原文 + URL + 访问日期）
- s07 引用片段逐字对齐上传 README；标准字段约束（64/1024/500 字符、三层 token 预算）与 agentskills.io 原文一致
- `README.md` 索引表与矩阵更新后格式正确、无断链；号位 07/08 空缺不影响渲染
- 量化推演均标注"估算"及假设，不伪装实测
