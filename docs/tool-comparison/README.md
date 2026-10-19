# 我们的工具 vs Claude Code 工具：逐工具深度对比

本系列报告将当前 Go coding agent（s02）的 6 个工具，逐个与 Claude Code 的对应内置工具做源码级对比。每份报告独立成文，固定 10 节结构（设计哲学 → 描述 → schema → 执行机制 → 输出契约 → 安全 → 量化推演 → 差距评级 → 对齐草案 → 参考资料）。

> 替代说明：本系列是 [.trae/documents/tools_gap_analysis_plan.md](file:///Users/tony/coding-agent-go/.trae/documents/tools_gap_analysis_plan.md) 所附总览分析的正式深化版；总览中的结论已拆入各工具报告与本文。本系列仅做分析与设计，不含代码实施。

## 报告索引

| # | 我们的工具 | Claude Code | 一句话结论 | 详报 |
|---|---|---|---|---|
| 1 | `read_file` | `Read` | 我们会"沉默地给你残缺内容"；CC 通过行号、分页、双层预算、mtime 去重让读文件**短、准、可恢复** | [01](01-read_file-vs-Read.md) |
| 2 | `write_file` | `Write` | 我们允许盲覆盖且非原子；CC 用 read-before-write 状态门控 + mtime/内容双检 + 原子写消灭陈旧覆盖 | [02](02-write_file-vs-Write.md) |
| 3 | `edit_file` | `Edit` / `MultiEdit` | 我们"静默替换第一处"是歧义操作；CC 强制唯一匹配 + `replace_all` + 一次多处原子编辑 | [03](03-edit_file-vs-Edit.md) |
| 4 | `grep` | `Grep` | 我们重写了一个忽略 gitignore、只有一种输出的玩具 rg；CC 直接暴露 ripgrep 的 13 个旋钮和三态输出 | [04](04-grep-vs-Grep.md) |
| 5 | `glob` | `Glob` | 我们的 `**` 匹配质量尚可，但缺 path、忽略规则、结构化输出与并发安全声明 | [05](05-glob-vs-Glob.md) |
| 6 | `bash` | `Bash` | s03 三闸门权限管线（deny 清单→破坏性规则→用户确认）已落地，但 shell 本身仍是硬截断、无状态、退出码无语义；CC 是分类器/权限/沙箱/语义退出码/落盘溢出/持久 cwd 的系统工程 | [06](06-bash-vs-Bash.md) |

## 方法论与资料版本

- **Claude Code 侧**：官方文档 v2.1.x（[Tools reference](https://code.claude.com/docs/en/tools-reference)、[环境变量](https://code.claude.com/docs/en/env-vars)，访问于 2026-10-16）+ 第三方对 v0.2.x 的源码解构（[claude-harness.dev](https://claude-harness.dev/) 第 21/22/23 篇，含源文件路径、函数名、常量）。两个版本间可能漂移的数值（Glob 100/Grep 250 上限、自动后台阈值等）在各报告中双标来源。
- **我方侧**：以仓库当前未提交工作区代码为准——[tools.go](file:///Users/tony/coding-agent-go/tools.go)（390 行）、[bash.go](file:///Users/tony/coding-agent-go/bash.go)（53 行）、[permission.go](file:///Users/tony/coding-agent-go/permission.go)（112 行，s03 三闸门权限管线，撰写本系列期间新增）、[agent.go](file:///Users/tony/coding-agent-go/agent.go)（62 行），引用精确到行。
- **时间说明**：本系列撰写期间，工作区落地了 s03 初版权限管线（原子串黑名单从 bash.go 迁入 [permission.go](file:///Users/tony/coding-agent-go/permission.go) 的 Gate 1 deny 清单，新增破坏性命令规则与终端用户确认门，见 [s03 spec](file:///Users/tony/coding-agent-go/.trae/specs/add-permission-pipeline/spec.md)）。06 报告已按落地后的代码更新；其余报告中"随 s03 落地"的条目表示需要在现有管线之上继续扩展。
- **量化推演**：均为标注假设的估算（代码文本按约 4 字符/token、中文按约 1.7 字符/token 估算），不是实测账单；用于比较数量级，不作为精确成本承诺。

## 跨工具总差距矩阵

| 维度 | Claude Code（通用模式） | 我们（通用模式） | 涉及工具 |
|---|---|---|---|
| 描述质量 | 描述即教学：何时用/别用、参数语义、输出形态、错误恢复全写进 schema description | 一句话描述，无行为契约 | 全部 |
| 输出形态 | 结构化/可决策：截断标记、appliedLimit、numFiles、输出文件路径 | 自由文本，固定 200/50K 上限的硬截断 | 全部 |
| 大输入处理 | fail-fast：超限给短错误并指导分页/窄化；溢出落盘返回路径 | 静默截断，模型不知道内容残缺 | read/bash/grep/glob |
| 分页/去重 | offset 分页、mtime unchanged stub（~30 token） | 无 | read/grep/glob |
| 编辑确定性 | 唯一匹配强制、read-before 门控、mtime 陈旧检测、原子写 | 第一处匹配静默替换、无门控、直接写 | write/edit |
| 搜索质量 | ripgrep：gitignore、编码检测、上下文行、type、mtime 排序、长行防护 | 自研 WalkDir：只跳 .git、噪音大、参数少 | grep/glob |
| Shell 能力 | cwd 持久、timeout 可调、后台任务、输出落盘 | 每条命令独立 sh、固定 120s、超限丢弃 | bash |
| 安全模型 | 命令分类 + 权限模式 + 路径规则 + 可选沙箱 + 语义化警告，分层评估 | s03 三闸门（7 项 deny 子串 + rm 类破坏性规则 + y/N 确认），无命令分类、无注入检查、无沙箱 | bash（文件工具只有 safePath） |
| 工具间联动 | 权限规则一处配置联动 rg 排除、Read/Edit 隐含授权与连带禁止 | 工具彼此孤立 | 全部 |
| harness 状态 | 有会话状态（readFileState、shell 会话、任务注册表） | 全是无状态纯函数 | 全部 |

## 三条共性结论：Claude Code 的工具为什么更好

### 1. Token 经济学是第一设计约束，贯穿每个工具

每一个工具结果都在为"最短、信息密度最高"而设计：Read 行号+分页+25K token 预算+unchanged stub；Grep 默认只给文件名（files_with_matches），需要细节才进 content；Glob 路径相对化去掉重复的根前缀；Bash 溢出落盘而不是灌回 30K 垃圾；Grep 用 `--max-columns 500` 防 base64 长行。对 Agent 而言，工具结果进入上下文后在后续每一轮都重复计费，**工具输出不是一次性成本，而是持续租金**——这也是我方工具当前最大的系统性差距。

### 2. 确定性优先，fail-fast；错误结果也要可恢复

Edit 唯一匹配、Write 的"未读拒写/陈旧拒写"（errorCode 2/3）、Read 超限抛 ~100 字节错误而非截断返回 ~25K token 内容（官方在 #21841 实测：截断让错误率下降但平均 token 反升）、Bash 的 grep/rg 退出码 1 解释为"无匹配"而非失败。共同模式：**宁可立刻给模型一个明确、短、带下一步指引的错误，也不让它基于残缺或歧义信息继续行动**。我们的工具普遍选择"静默吞掉边界"（截断、替换第一处、任何非零退出都报 Error），把发现问题的成本推迟到后续轮次。

### 3. 安全与能力都靠"分层系统 + 状态"，不靠单点黑名单

CC 的 bash 安全是命令分类器（管道全段判定）→ 权限模式/规则 → 23 项注入检查 → 可选 OS 沙箱 → 信息性警告的多层防线，且文件工具的读状态、shell 的会话状态、权限的拒绝规则彼此联动（deny Read 连带禁写、deny 路径自动变 rg 排除参数）。我们 s03 落地的三闸门管线（[permission.go](file:///Users/tony/coding-agent-go/permission.go)）已搭对"硬拒→规则→确认"的骨架，但 Gate 1 仍是子串枚举：`r""m -rf /`、变量拼接、base64、`rm -rf $HOME` 都可轻易绕过 deny 清单；而 Gate 2 只有 `rm`/`del` 一个正则，尚无管道感知的只读分类（也就没有只读命令自动放行）、无注入检查、无沙箱。且因为无 harness 状态，门控类能力（先读后写、cwd 持久）在当前架构里仍无处安放。

## 对齐路线图摘要（仅备忘，详细设计见各报告第 9 节）

- **里程碑 A · 工具内功（纯 Go、零外部依赖）**：① 描述/schema 契约化 → ② harness 会话状态（readFileState、shell cwd）→ ③ Read 对齐（行号/offset/双层预算/stub）→ ④ Write/Edit 对齐（唯一性、replace_all、multi_edit、门控、原子写）
- **里程碑 B · 搜索与 shell**：⑤ Grep/Glob 对齐 ripgrep 语义（运行时探测 rg，缺失则回退自研+gitignore 解析）→ ⑥ Bash 增强（cwd 持久、timeout、溢出落盘、命令分类、退出码语义）
- **里程碑 C · 安全与长任务**：⑦ 权限系统 —— **初版已落地**（三闸门，见 [permission.go](file:///Users/tony/coding-agent-go/permission.go)）；后续：命令分类器 + 只读自动放行、规则格式 `Bash(pat)`/`Read(glob)`/`Edit(glob)`、注入检查 → ⑧ 后台任务（run_in_background / TaskStop / 完成通知）
- **远期**：TodoWrite、Task 子代理、WebFetch/WebSearch、LSP 诊断联动、MCP

## 参考资料（总）

- [Claude Code Tools reference（v2.1.x）](https://code.claude.com/docs/en/tools-reference)
- [Claude Code 环境变量（BASH_MAX_OUTPUT_LENGTH 等）](https://code.claude.com/docs/en/env-vars)
- [The File Operations Trio: Design Philosophy of Read, Write, and Edit（源码解构 21）](https://claude-harness.dev/en/articles/21-file-operations)
- [BashTool: Letting AI Safely Execute Shell Commands（源码解构 22）](https://claude-harness.dev/en/articles/22-bash-tool)
- [The Search System: Glob + Grep（源码解构 23）](https://claude-harness.dev/en/articles/23-search-system)
- [Agent SDK 工具 schema（Python）](https://platform.claude.com/docs/en/agents-and-tools)
