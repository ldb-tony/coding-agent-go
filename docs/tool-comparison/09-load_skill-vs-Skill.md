# 09 · load_skill（s07 skill loading）vs Claude Code Skill / Skills 系统

[返回索引](README.md) ｜ 上一篇：[todo_write vs Task 系统](07-todo_write-vs-TaskSystem.md)

> 本篇对比对象说明：**我方侧是 s07 教学参考实现**（Python，`SkillLoader` + `load_skill`），不是本项目 Go 代码——coding-agent-go 目前尚无 skill 机制（全仓库 grep `skill` 仅命中计划文档）。Go 侧对齐设计见第 9 节。CC 侧事实来源：官方 [Skills](https://code.claude.com/docs/en/skills)、[Tools reference](https://code.claude.com/docs/en/tools-reference)、[MCP](https://code.claude.com/docs/en/mcp) 与 [Agent Skills 开放标准](https://agentskills.io/specification)，均访问于 2026-10-19。

---

## 1. 定位与设计哲学

### 1.1 s07：回答一个具体问题的最小机制

s07 要解决的问题在 README 里写得很直白：项目里有 React 规范、SQL 规范、API 设计三份文档，最直接的作法是全部拼进 system prompt——结果每次 LLM 调用都为三份文档付 token，哪怕当前任务只改 React 组件：

> *"Every LLM call sends the full text of all three documents to the model. …the SQL style guide and API design document still consume input tokens and context-window space that could hold code, conversation, and tool results."*

s07 的答案是两级加载，README 自带这张表：

| 内容 | 进入模型的位置 | 时机 |
|---|---|---|
| Skill name + description | system prompt | 启动时 |
| `SKILL.md` 全文 | `tool_result` | `load_skill` 被调用时 |

`SkillLoader.scan()` 启动时扫描 `skills/*/SKILL.md` 解析 frontmatter；`catalog()` 只产出 `- name: description` 行拼进 system prompt；`load_skill(name)` 被模型调用时返回全文，经 agent loop 作为 `tool_result` 入消息列表。整个机制一个类加两个方法，是"知识按需加载"的**教学骨架**。

### 1.2 Claude Code：同一思想的产品化与标准化

CC 把同一件事做成了三层体系。**底层是开放标准**：CC skills 遵循 [Agent Skills](https://agentskills.io/) 标准（跨 AI 工具可移植），标准定义了**三级渐进披露**（progressive disclosure，spec 原文）：

> 1. **Metadata** (~100 tokens): The `name` and `description` fields are loaded at startup for all skills
> 2. **Instructions** (< 5000 tokens recommended): The full `SKILL.md` body is loaded when the skill is activated
> 3. **Resources** (as needed): Files (e.g. those in `scripts/`, `references/`, or `assets/`) are loaded only when required

**中层是产品化分发**：7 类加载位置（enterprise/personal/project/nested/`--add-dir`/plugin/claude.ai 同步）、bundled skills、自定义命令并入 skills（`.claude/commands/deploy.md` 与 `.claude/skills/deploy/SKILL.md` 等价）。**上层是调用与管控**：模型自动调用 + 用户 `/skill-name` 直调双通道、`disable-model-invocation`/`user-invocable` 调用控制、`allowed-tools` 权限预批准、`Skill(name *)` 权限规则、动态上下文注入、fork 子代理执行。

对照着看：**s07 实现了三级披露的前两级**（元数据进 system prompt ≈ level 1，全文进 tool_result ≈ level 2），**缺第三级**——skill 在 s07 里只能是单文件全文，没有 scripts/references/assets 的概念，一份 8K token 的规范文档只能整份进上下文，无法拆出"按需再读"的部分。这是教学骨架与生产系统之间最本质的结构差距。

### 1.3 边界声明：CC 的"能力按需加载"不止 Skills

CC 用同一套"目录常驻 + 按需展开"思想处理了至少四类能力，本篇只深比 Skills：

- **MCP 工具 schema**：`ToolSearch` 工具——*"Searches for and loads deferred tools when tool search is enabled"*（[Tools reference](https://code.claude.com/docs/en/tools-reference)）。MCP 服务器可带来数百个工具，schema 全量进上下文不可行，CC 默认（Opus/Sonnet/Haiku 4.5+）把大部分工具置为 deferred，由 ToolSearch 按关键词检索后加载；`ENABLE_TOOL_SEARCH=false` 时退化为 `WaitForMcpServers` 等待连接。
- **子代理 skills 预载**：子代理定义可带 `skills` 字段预载技能（`disable-model-invocation: true` 同时会阻止预载，见第 3 节字段表）。
- **CLAUDE.md 分级**：CLAUDE.md 是"always-loaded"的事实性记忆，skills 文档官方定位即*"Unlike CLAUDE.md content, a skill's body loads only when it's used"*——`/doctor` 甚至会把过长的 CLAUDE.md 段落建议迁移为按需加载的 skill。

这些机制与 Skills 共享同一设计约束（目录成本常驻、正文延迟付费），但各自的目录格式、加载触发与权限语义不同，不在本篇深比范围。

## 2. 目录与描述（catalog）对比

### 2.1 s07：纯文本拼接，无长度管理

s07 的 catalog 生成逻辑（README 摘录）：

```python
raw_name = metadata.get("name")
name = raw_name.strip() if isinstance(raw_name, str) else ""
name = name or manifest.parent.name            # name 缺省回退目录名
raw_description = metadata.get("description")
description = (raw_description.strip()
               if isinstance(raw_description, str) else "")
description = description or body.split("\n", 1)[0]   # 回退正文首行
description = " ".join(str(description).lstrip("# ").split())
```

回退链设计合理（无 frontmatter 的 SKILL.md 也能进目录），但**没有任何长度与格式约束**：description 回退到正文首行时，一行 500 字符的标题就原样进 system prompt；skill 数量从 3 个涨到 30 个时，catalog 无界增长。system prompt 是每轮必付的固定租金，目录质量直接决定基础成本。

### 2.2 CC：字段约束 + 截断上限 + 写作规范

CC 侧目录条目受三层约束：

**标准层**（agentskills.io spec）：`name` 最长 64 字符、仅小写字母数字与连字符、不得以前缀/后缀连字符、**必须与父目录名一致**；`description` 最长 1024 字符、非空、要求"既说明做什么也说明何时用"。spec 还给了正反例——好例：*"Extracts text and tables from PDF files, fills PDF forms… Use when working with PDF documents or when the user mentions PDFs…"*；坏例：*"Helps with PDFs."*

**CC 层**（[Skills](https://code.claude.com/docs/en/skills) frontmatter reference）：目录清单里 description 有硬截断——

> *"Put the key use case first: the combined `description` and `when_to_use` text is truncated at 1,536 characters in the skill listing to reduce context usage."*

**运维层**：troubleshooting 文档专门有 "Skill descriptions are cut short"、"Skill not triggering"、"Skill triggers too often" 三小节，教用户调描述；`skillOverrides` 设置可从外部控制 skill 可见性而不改文件。

### 2.3 对比表

| 维度 | s07 | Claude Code |
|---|---|---|
| 目录内容 | `- name: description` 纯文本行 | name + description（+`when_to_use` 追加），skill listing 形态 |
| name 约束 | 无，回退目录名 | ≤64 字符、小写+连字符、须匹配目录名（标准）；命令名可经 frontmatter `name` 覆盖（CC） |
| description 约束 | 无，回退正文首行（去 `#`） | ≤1024 字符（标准）；listing 中 description+when_to_use 截断至 1536 字符（CC） |
| 目录注入点 | 拼进 system prompt 字符串 | skill listing（附截断与可见性控制） |
| 描述写作指导 | 无 | spec 正反例 + troubleshooting 三小节 |
| 规模化管理 | 无（无界增长） | 截断上限 + `skillOverrides` + `/doctor` 提示未使用 skill 的上下文成本 |

两方的共同点是**都把 description 当作模型路由的唯一依据**——s07 靠 "Use load_skill to read the full instructions when a skill applies" 这句提示让模型自行匹配；CC 则把这件事文档化、指标化（触发不了/触发太频繁都有排查路径）。

## 3. API/Schema 逐项对比

### 3.1 加载工具本体

| 维度 | s07 `load_skill` | CC `Skill` |
|---|---|---|
| 官方描述 | 无（教学代码未示 schema 描述） | *"Executes a skill within the main conversation"*（Tools reference 原文） |
| 参数 | `name`（按名查注册表，**不解释为文件路径**——README 明示） | skill 名 + 可选参数（`$ARGUMENTS`/`$0`/`$name` 替换，见 3.3） |
| 返回 | 命中：`SKILL.md` 全文；未命中：`Error: Unknown skill '{name}'. Available: {列表}` | 渲染后的 skill 内容进入主对话（含注入命令执行结果） |
| 用户通道 | 无（仅模型可调） | `/skill-name` 斜杠命令与 `Skill` 工具等价（frontmatter `user-invocable: false` 可关闭用户通道） |

s07 的"name 不是路径"是一个容易被低估的设计点：模型传 `../../etc/passwd` 也只会在注册表里查不到，天然免疫路径穿越——代价是 skill 集合在启动时固化，会话中新增 skill 不会被发现。

### 3.2 frontmatter 字段对比

s07 只消费 `name`、`description` 两个字段且均为可选回退。Agent Skills 标准定义 6 个字段，CC 在此基础上扩展了一整层产品字段（[Skills frontmatter reference](https://code.claude.com/docs/en/skills)，2026-10-19 访问，逐项见下表）：

| 字段 | 来源 | 作用 | s07 |
|---|---|---|---|
| `name` | 标准 | 命令名；≤64 字符小写连字符、匹配目录名 | ✅（无约束校验） |
| `description` | 标准 | 路由依据；≤1024 字符，listing 截断 1536 | ✅（无约束校验） |
| `license` / `compatibility` / `metadata` | 标准 | 许可 / 环境要求（≤500 字符）/ 自由键值；CC 接受但不行动 | ❌ |
| `allowed-tools` | 标准（实验性） | 预批准工具，skill 激活回合内免确认，下一消息失效 | ❌ |
| `when_to_use` | CC 扩展 | 触发时机补充，追加进 listing、计入 1536 上限 | ❌ |
| `argument-hint` / `arguments` | CC 扩展 | 自动补全提示 / 命名位置参数 | ❌ |
| `disable-model-invocation` | CC 扩展 | `true` 禁止模型自动加载（同时阻止子代理预载与定时任务触发），仅 `/name` 手动 | ❌ |
| `user-invocable` | CC 扩展 | `false` 从 `/` 菜单隐藏，仅模型可调（背景知识类 skill） | ❌ |
| `disallowed-tools` | CC 扩展 | skill 激活期间从工具池移除指定工具 | ❌ |
| `model` / `effort` | CC 扩展 | skill 激活期间覆盖模型 / effort 档位（`context: fork` 时作用于 fork 子代理） | ❌ |
| `context` / `agent` / `background` | CC 扩展 | `context: fork` 在 fork 子代理中执行；`agent` 指定子代理类型；`background` 控制是否等结果（v2.1.218+） | ❌ |
| `hooks` | CC 扩展 | skill 被调用时注册、会话内持续生效的 hooks | ❌ |
| `paths` | CC 扩展 | glob 限定自动激活的文件范围（与 memory path-specific rules 同格式） | ❌ |
| `shell` | CC 扩展 | 注入命令用 bash 还是 powershell | ❌ |

CC 侧的解析鲁棒性规则也值得记录（同页原文）：*"A field name must match the table exactly, hyphens included: Claude Code ignores a field it doesn't recognize without reporting an error."*——未知字段静默忽略；frontmatter 仅当文件首行即 `---` 时解析，否则整个文件按正文处理；YAML 解析失败则 skill 以无字段状态加载。布尔字段自 v2.1.218 起接受 `yes/no/on/off/1/0` 任意大小写。

另外注意标准与实现的分界：CC 接受上表全部字段，但走出 CC（claude.ai 上传、Skills API、`package_skill.py` 打包）只允许标准 6 字段，混入 CC 扩展字段会硬报错 `Unexpected key(s) in SKILL.md frontmatter`——**可移植性是靠字段白名单显式维护的**。

### 3.3 参数与字符串替换

s07 的 skill 是静态文本，无任何参数化能力。CC 支持完整的位置/命名参数与环境替换（同页 "Available string substitutions"）：`$ARGUMENTS`、`$ARGUMENTS[N]`、`$N`、`$name`（配合 frontmatter `arguments` 声明）、`${CLAUDE_SESSION_ID}`、`${CLAUDE_EFFORT}`、`${CLAUDE_SKILL_DIR}`、`${CLAUDE_PROJECT_DIR}`、`${CLAUDE_PLUGIN_ROOT}`、`${CLAUDE_PLUGIN_DATA}`。这让 skill 从"静态文档"变成"带参模板"——`/deploy staging` 里的 `staging` 能精确落到指令文本里。

## 4. 执行机制对比（源码级）

### 4.1 发现：一次性单目录扫描 vs 七类位置 + 冲突解析 + 热更新

**s07**（README 摘录）：

```python
for manifest in sorted(self.skills_dir.glob("*/SKILL.md")):
    if (not manifest.is_file()
            or not manifest.resolve().is_relative_to(skills_root)):
        continue
```

启动时扫一次 `skills/*/SKILL.md`，`is_relative_to` 拒绝符号链接逃逸；之后不再 rescan。单目录、无优先级、无冲突概念（同名后者覆盖前者，dict 赋值语义）。

**CC** 的发现是一张七行位置表（[Skills](https://code.claude.com/docs/en/skills) 原文整理）：Enterprise（managed settings 目录）→ Personal（`~/.claude/skills`）→ Project（`.claude/skills`）→ Nested（子目录 `.claude/skills`，会话进入该范围时加载）→ Additional（`--add-dir`）→ Plugin（`<plugin>/skills/`，命令名空间化为 `/plugin:skill`）→ claude.ai 账户同步（`~/.claude/skills/synced/`）。另有：符号链接位置条目合法（多处指向同一目标只加载一次）；保留名 `synced`、`anthropic-skills`；同名冲突有完整优先级表——

> *"Two of enterprise, personal, and project: Enterprise over personal, and personal over project."* 自有 skill 可覆盖 bundled skill 与内置命令但不覆盖其别名；plugin skill 因命名空间共存；嵌套 skill 与根 skill 共存。

以及**热更新**（live change detection）：会话中编辑受监听目录里的 skill 文件即生效，删除后在 `/skills` 中即时消失。s07 要新增 skill 必须重启进程。

### 4.2 加载：全文一次性 vs 三级披露 + scripts 免上下文

**s07**：`load(name)` 返回 `skill["content"]`——整个 SKILL.md 原文进 `tool_result`。没有大小概念：一份 5000 行的规范全文一次性入消息列表，之后每轮重复计费。

**CC**：三级披露（spec 原文已在 §1.2 引用）。第三级是关键差异——skill 目录可以放：

- `scripts/`：可执行代码，*"agents can run"*——**执行结果进上下文，脚本源码不进**；
- `references/`：*"Agents load these on demand, so smaller files mean less use of context"*——模型用 Read 按需读；
- `assets/`：模板、图片、数据文件。

spec 还建议 *"Keep your main `SKILL.md` under 500 lines"*、文件引用*"one level deep"*。也就是说 CC 的 skill 作者被引导把 SKILL.md 写成**路由层**（什么场景读哪个 references 文件、跑哪个 script），而 s07 的 SKILL.md 只能是**全部内容的载体**。

### 4.3 调用：单一模型通道 vs 双通道 + 五路控制

**s07**：只有模型主动调 `load_skill` 一条路，触发完全依赖 description 质量与 system prompt 里的一句指引。

**CC** 的调用矩阵：

| 维度 | 机制 |
|---|---|
| 模型自动 | 按 description/`when_to_use` 匹配；`paths` frontmatter 可按文件 glob 限定自动激活范围 |
| 用户直调 | `/skill-name`，带参数补全提示（`argument-hint`） |
| 模型通道开关 | `disable-model-invocation: true`（同时禁止子代理预载与定时任务触发） |
| 用户通道开关 | `user-invocable: false`（`/` 菜单隐藏） |
| 外部覆盖 | `skillOverrides` 设置（`"off"` / `"user-invocable-only"` 等），不改文件即可调可见性 |

### 4.4 动态上下文注入：s07 没有的能力维度

CC 的 SKILL.md 正文里可以写 `` !`command` ``（如 `` !`git diff HEAD` ``），官方语义（[Skills](https://code.claude.com/docs/en/skills) "Inject dynamic context"）：

> *"Claude Code runs the command and replaces the line with its output before Claude sees the skill content, so the instructions arrive with the current diff already inlined."*

**先执行、后呈现**——模型拿到的指令里已经内联了实时数据，不必再自己花一轮工具调用去取。注入命令有独立权限检查与失败处理章节（同页 "Permission checks on injected commands"、"When an injected command fails"）。s07 的 skill 是静态文本，任何实时数据都要模型加载后再自行调工具补齐，多一轮往返且依赖模型记得去取。

### 4.5 执行位置：主对话内 vs 可 fork 进子代理

CC 的 `context: fork` + `agent` + `background`（v2.1.218+）让 skill 可以在 fork 的子代理上下文里跑——长流程 skill（如 `/verify` 构建并驱动应用）不污染主对话。s07 的 skill 内容只能进主对话消息列表，与本系列 07 号位（子代理对比）讨论的是同一个正交维度：CC 里 skill 与 subagent 是**可组合**的两个系统，教学实现里两者都还不存在。

### 4.6 失效与诊断

| 场景 | s07 | CC |
|---|---|---|
| 模型叫了不存在的 skill | `Error: Unknown skill '{name}'. Available: {列表}`——短错误 + 可选项列表，模型可自我纠正 | 模型只能调用 listing 里的 skill（目录即白名单） |
| 名字含非法字符 | 无此概念 | 拒绝加载，`claude --debug` 日志搜 `[names]`，交互会话有启动通知 |
| frontmatter 写错 | 回退链兜底（name→目录名，description→首行） | 未知字段静默忽略；YAML 解析失败按无字段加载；troubleshooting 文档引导排查 |
| 触发不了/触发太频繁 | 无诊断手段 | 官方 troubleshooting 三小节（not triggering / triggers too often / stops following） |

## 5. 输出契约与边界行为

**s07**：`load_skill` 的输出契约极简——命中即全文，未命中即带可用列表的错误串。没有大小上限、没有重复加载去重（模型连调两次 `load_skill("pdf")`，两份全文都会进消息列表）、没有卸载概念（加载过的全文永远在上下文里付租金）。

**CC** 的内容生命周期（[Skills](https://code.claude.com/docs/en/skills) "Skill content lifecycle" 原文）：

> *"When you or Claude invoke a skill, the rendered `SKILL.md` content enters the conversation as a single message and stays there across later turns."* ——渲染结果作为**一条消息**驻留，后续轮次不重读文件。
>
> *"When Claude re-invokes a skill whose rendered content is identical to the copy already in context, Claude Code adds a short note that the skill is already loaded rather than a second copy of the content. When the rendered content differs, because the arguments changed or a dynamic context command produced new output, Claude Code appends the full content again."* ——**内容相同则去重**（短提示替代全文），参数/注入输出不同才追加新副本。

这条"相同内容重复调用只回短提示"与本系列 01 篇 Read 的 mtime unchanged stub 是同一手法：**重复信息不重复付费**。权限与内容的生命周期也被显式分离——`allowed-tools` 授权在下一消息即失效，而指令内容常驻（*"This persistence applies to the skill's instructions, not its permissions"*）。

写作侧的契约约束（spec）：SKILL.md 建议 <500 行 / <5000 tokens，超出部分应拆进 references——**大小管理前置给了作者**，配合 §4.2 的按需加载，模型实际为单份 skill 付的租金有上限。s07 没有任何对应约束，契约是"你放多少，模型付多少"。

## 6. 状态、安全与权限

**s07** 的安全面只有一处：扫描期的 `is_relative_to` 逃逸校验（防 skills 目录里的符号链接指向外部）。工具调用期反而天然安全——`load_skill` 按名查表不碰文件系统（§3.1）。skill 内容本身不携带任何权限语义：模型读完 SQL 规范后想跑 `DROP TABLE`，权限管线看到的只是一个普通的 bash 调用。

**CC** 把 skill 纳入了整个权限与管控体系：

- **权限规则**：`Skill(deploy *)` 通配格式（[Tools reference](https://code.claude.com/docs/en/tools-reference) 规则表原文行），可在 settings / `--allowedTools` / SDK `allowedTools` 中统一配置；*"Restrict Claude's skill access"* 章节支持按名限制模型可用的 skill 集合。
- **skill 自带授权**：`allowed-tools` 在 skill 激活回合内免确认指定工具（*"The grant clears when you send your next message"*），`disallowed-tools` 反向摘除（语义对齐 deny 规则）；managed permission 环境下有专门章节界定生效边界。
- **注入命令权限**：`` !`cmd` `` 先经权限检查再执行（§4.4）。
- **分发即管控**：Enterprise 位置（managed settings 目录）让企业统一部署/收回 skill；`/doctor` 审计未使用 skill 的上下文成本。
- **名字即攻击面**：非法字符（零宽空格、方向控制符等）会被拒绝加载并留调试痕迹——skill 名会进 `/` 菜单与 prompt，CC 把它当 UI 注入面处理。

## 7. 量化场景推演（估算）

> 以下均为估算：英文/代码文本按约 4 字符/token，仅比较数量级，非实测账单。

### 场景 A：README 自带场景（3 份规范文档）

假设 react-style.md 4KB、sql-style.md 3KB、api-design.md 5KB，合计 12KB ≈ 3000 token；任务只改 React 组件，会话 20 轮。

| 方案 | 启动成本 | 任务中加载 | 20 轮累计租金 |
|---|---|---|---|
| 全量塞 system prompt | 3000 tok | — | 3000 × 20 = **60 000 tok** |
| s07 两级 | catalog 3 行 ≈ 60 tok | React 全文 ≈1000 tok 一次 | (60+1000) × 20 ≈ **21 200 tok** |
| CC 三级 | ~100 tok × 3 = 300 tok | SKILL.md 路由层（按建议 <5000 tok，取 800 tok）+ 按需读 references 一节（300 tok 一次） | (300+800+300) × 20 ≈ **28 000 tok** |

s07 相对全量方案省 ~65%，验证了两级披露的核心价值。CC 在单技能小文档场景与 s07 同量级（甚至因 100 tok/技能的目录成本略高）——**三级披露的收益不在小文档，在下面的规模化场景**。

### 场景 B：20 个 skill 的目录成本

| 方案 | 目录成本 | 说明 |
|---|---|---|
| s07 | 无约束：若每个 description 平均 200 字符 → 4000 字符 ≈ 1000 tok；回退首行失控时无上限 | catalog 拼在 system prompt，每轮付 |
| CC | ~100 tok × 20 = 2000 tok，且单条 description+when_to_use 截断在 1536 字符 | listing 有硬顶；`/doctor` 会提示未使用 skill 的成本 |

数量级相同，差距在**可预期性**：s07 的目录成本随内容质量漂移（一行长标题就膨胀），CC 有截断上限兜底。

### 场景 C：单份 8K token 的大规范

一份 8000 token 的 API 设计规范（约 32KB），任务只需要其中"错误码"一节（约 600 token）：

| 方案 | 首次加载 | 后续每轮租金 | 说明 |
|---|---|---|---|
| s07 | 8000 tok 全文进 tool_result | 8000 tok | 无拆分概念，用到哪节都付全文 |
| CC | SKILL.md 路由层 500 tok + Read 错误码一节 600 tok | 1100 tok | references 按需读；重复调用同内容只回短提示 |

差距 ~7 倍/轮，且随会话轮数线性放大。s07 的解法只能是作者手工把规范拆成多个小 skill（`api-design-errors`/`api-design-auth`…）——用命名 hack 模拟第三级披露，目录成本随之上涨，回到场景 B 的无界问题。

## 8. 差距清单评级

| 优先级 | 差距 | CC 侧参照 |
|---|---|---|
| **P0** | 无 frontmatter 字段约束与校验（name 规范/匹配目录名、description 上限） | spec 字段表 + CC 1536 字符截断 |
| **P0** | 无 supporting files 概念（scripts/references/assets）——skill 只能是单文件全文，缺第三级披露 | spec 三级披露 + `scripts/`/`references/`/`assets/` 约定 |
| **P0** | 无调用控制：模型单通道，无用户 `/` 直调，无 `disable-model-invocation`/`user-invocable` | CC 调用矩阵（§4.3） |
| **P1** | 无多位置加载分层（personal/project/plugin/enterprise）与同名冲突解析 | CC 七类位置表 + 优先级表 |
| **P1** | 无动态上下文注入（`` !`cmd` `` 先执行后呈现） | §4.4 官方语义 |
| **P1** | 无参数化（`$ARGUMENTS`/`$name`/`argument-hint`） | §3.3 替换表 |
| **P1** | 无 fork 子代理执行（`context: fork`） | §4.5 |
| **P1** | 无权限语义：`Skill(name *)` 规则、`allowed-tools` 预批准、注入命令权限检查 | §6 |
| **P1** | 无重复加载去重与内容生命周期管理 | §5 content lifecycle |
| **P1** | 无 catalog 长度管理（无界增长） | §2.3 |
| **P2** | 未对齐开放标准（agentskills.io 跨工具可移植、字段白名单） | §3.2 末段 |
| **P2** | 无 bundled skills / 命令合并 / plugin 分发 / claude.ai 同步 | §1.2 |
| **P2** | 无热更新（live change detection）、无 `skillOverrides`、无 eval 工具链（skill-creator） | §4.1、§2.2 |

## 9. 对齐设计草案（仅设计，不实施）

Go 侧分五个阶段，每阶段独立可交付。遵循项目既有约定：工具实现进 [tools.go](file:///Users/tony/coding-agent-go/tools.go) 注册体系、权限进 [permission.go](file:///Users/tony/coding-agent-go/permission.go)、避免第三方依赖（含 YAML 解析先自实现子集）。

### 阶段 ① 对齐 s07 最小骨架

新建 `skill.go`：

```go
type skill struct{ name, description, content string }

type skillLoader struct {
    dir    string
    skills map[string]skill
}

func (l *skillLoader) scan() error      // filepath.Glob("*/SKILL.md") + 绝对路径须位于 dir 内
func (l *skillLoader) catalog() string  // "- name: description" 行
func (l *skillLoader) load(name string) string // 查表；未命中返回带可用列表的错误
```

- frontmatter 只解析 `---` 块内 `name:`/`description:` 两个键（自实现行解析，不引 YAML 库）；回退链对齐 s07（name→目录名，description→正文首行去 `#`）
- `allTools()` 加 `load_skill` schema（`{name: string}`），`toolHandlers` 加一行映射——兑现"加一个工具只加一行"
- [llm.go](file:///Users/tony/coding-agent-go/llm.go) 的 system prompt 拼装追加 `Skills available:\n{catalog}` 与使用指引
- 测试：scan 回退链、符号链接逃逸拒绝、catalog 格式、load 命中/未命中、错误串含可用列表

### 阶段 ② 字段校验与目录管理

- name 校验（≤64、小写+连字符、匹配目录名），description 超 1024 字符截断并告警；catalog 总字符设上限，超限按序截断并标注
- 重复 `load_skill` 同内容去重：返回短提示 "skill already loaded"（对齐 CC content lifecycle）

### 阶段 ③ supporting files（第三级披露）

- skill 目录约定 `references/`、`scripts/`、`assets/`；SKILL.md 正文用相对路径引用
- references 由模型用现有 `read_file` 按需读（无需新工具）；scripts 由现有 `bash` 执行（**输出进上下文、源码不进**）
- system prompt 指引补充："SKILL.md 是路由层，长文档拆 references"

### 阶段 ④ 权限与调用控制

- [permission.go](file:///Users/tony/coding-agent-go/permission.go) 闸门新增 `Skill(name *)` 规则解析；`allowed-tools` frontmatter 在当回合注入临时许可（下一条用户消息失效）
- frontmatter 支持 `disable-model-invocation`（catalog 中剔除，仅 `/` 可调）

### 阶段 ⑤ 用户通道与参数化

- REPL（[main.go](file:///Users/tony/coding-agent-go/main.go)）支持 `/skill-name [args]`：直接将渲染后内容作为用户消息注入，等价 CC 的 `user-invocable` 通道
- `$ARGUMENTS`/`$0`/`$name` 替换 + `argument-hint`

**破坏性变更提示**：阶段 ① 后 system prompt 含 catalog，旧会话无缓存兼容性承诺；阶段 ④ 会改变权限管线入参结构，需同步更新 [permission_test.go](file:///Users/tony/coding-agent-go/permission_test.go)。

## 10. 参考资料

- [Claude Code Skills](https://code.claude.com/docs/en/skills)（2026-10-19 访问：frontmatter reference、七类位置、同名冲突、动态注入、content lifecycle、调用控制）
- [Claude Code Tools reference](https://code.claude.com/docs/en/tools-reference)（2026-10-19 访问：`Skill` 工具描述、`Skill(deploy *)` 权限规则、`ToolSearch` 描述）
- [Agent Skills 开放标准](https://agentskills.io/specification)（2026-10-19 访问：三级渐进披露、字段约束、目录约定）
- [Claude Code MCP](https://code.claude.com/docs/en/mcp)（2026-10-19 访问：tool search 默认开启与 `ENABLE_TOOL_SEARCH` 行为）
- s07 教学实现 README（[上传附件](file:///Users/tony/.trae-cn/attachments/f949976d-a22b-4942-a711-34e8b8cd2dfc_README.md)）
- 本项目接缝：[tools.go](file:///Users/tony/coding-agent-go/tools.go)、[llm.go](file:///Users/tony/coding-agent-go/llm.go)、[permission.go](file:///Users/tony/coding-agent-go/permission.go)、[main.go](file:///Users/tony/coding-agent-go/main.go)
