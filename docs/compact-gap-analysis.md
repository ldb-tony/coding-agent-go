# 上下文压缩与 Claude Code 设计稿的差距分析

> 基准：《上下文压缩系统设计文档（Microcompact + Auto-Compact）》（基于 Claude Code `src/services/compact/` 逆向提炼，下称"设计稿"）
> 对照对象：`coding-agent-go` 改造前的 `main` 分支实现
> 结论日期：2026-10-23
> 本期改造：已按"标准版"范围落地（见第 6 节），代码见 [compact.go](../compact.go)

## 1. 摘要

三条核心结论：

1. **阈值单位错了**：旧实现用 `json.Marshal` 字节数（50_000）当上下文占用，≈ 12k token，只有 200k 窗口的 6%；真正的压缩线应在 167k token（窗口 83.5%）。旧实现约早响了 **14 倍**。
2. **50 条消息硬切无意义**：触发只看消息条数，与 token/窗口占用完全无关；且每轮 `prepare` 都会重跑，超 50 条后每轮都重切一次、全量重写 `.transcripts/`。已删除。
3. **auto-compact 丢状态且失败即毁上下文**：单条 `[Compacted]` 覆盖全历史后，文件内容/计划/技能全部消失（`st.reads` 却仍标记"已读"，模型会盲写 `edit_file`）；摘要一次网络失败就把错误串当摘要写进历史。

本期落地：usage 锚点 + token 阈值（G1）、删除 snip 层（G2）、auto-compact 附件 + 尾部保留（G3）、九章节摘要 prompt + 后处理（G4）、失败不覆盖历史 + 3 次熔断（G5）、time-based microcompact（G6）。PTL 剥洋葱、blocking limit、手动 `/compact`、prompt caching 仅记录不实现（第 7 节）。

## 2. 改造前的实现全貌

### 2.1 调用链

- [agent.go](../agent.go)：每轮 `client.Send` 前调用 `cpt.prepare(...)`；`Send` 失败且错误含 `prompt_too_long` 时 `reactiveCompact`，最多重试 1 次；模型调 `compact` 工具时在整批 tool_result 落库后执行 `compactHistory`。
- 旧 `prepare` 四步流水线：`toolResultBudget` → `snipCompact` → `microCompact`/`fitToolResults` → `compactHistory`。
- `Usage` 只用于日志，从不参与压缩判定。`anthropic.MessageParam` 不含 usage 与时间戳（`resp.ToParam()` 会把 usage 丢掉），因此"usage 锚点"必须在 compactor 侧单独记录——这也是旧实现只能用字节估算的根因。

### 2.2 旧参数表

| 参数 | 旧值 | 实际语义 |
|---|---|---|
| `contextCharLimit` | 50_000 | JSON **序列化字节数**（`json.Marshal`），非 token |
| `snipMaxMessages` | 50 | 消息条数 > 50 即归档中段，保留首 3 + 尾 46 |
| `microMinLen` | 120 | **短于 120 字符的旧结果不替换**（不是"截断到 120 字符"） |
| `keepRecentConsumed` | 3 | micro_compact 保留最近 3 条"已消费"结果 |
| `toolResultBudgetChars` | 200_000 | 单轮 tool_result 合计字符预算（保留，未改） |
| `largeResultCharLimit` / 预览 | 30_000 / 2_000 | 单条大结果落盘 + 2K 预览（保留，未改） |
| 摘要调用 | `maxTokens=2000` | 5 行事实清单 prompt |
| 摘要失败 | 返回错误串 | **作为摘要替换全历史** |

### 2.3 `st.reads` 与压缩的交互

`sessionState.reads` 记录"最近一次成功读取"（mtime + 区间），是 read-before-write 门控的状态基础。旧 `compactHistory` 把整段历史换成一条 summary 后，`st.reads` 原样保留——模型"看不到内容却通过写入门控"，可能用臆造的 `old_text` 调 `edit_file`。设计稿的"PreCompact 快照 + 清空 + 附件重读"正是为此。

### 2.4 其他关键事实

- 工具自身已限流：`bash` 50k 字符、`read_file` 25k 字符，单轮 tool_result 另有 200k 字符预算——"源头限流"已存在，缺的是"何时整体压缩"的正确判定。
- 子代理（`task`）不走 `agentLoop` 的 `prepare` 主链路，time-based microcompact 天然不会进子代理，符合设计稿。
- 项目未启用 prompt caching（`llm.go` 无 `cache_control`），"缓存 TTL 到期才瘦身"的前提目前不成立（第 7 节）。
- skills 无"已加载"记录，压缩后重建附件无从下手。

## 3. "50 条 / 120 字符"问题的准确结论

**问：当前基于 message 长度达到 50 就减少工具的输出是否没有意义？read 的部分被删到 120 个字符后基本没有价值？**

### 3.1 50 条硬切确实无意义

旧 `snipCompact` 的触发只看 `len(messages) > 50`，与 token/窗口占用无关；每次 `prepare` 都会跑，而一轮工具调用 +2 条消息，所以超过 50 条后**每轮都会再切一次**并重写整份 `.transcripts/`。一次 `read_file` 最多 25k 字符 ≈ 6k token，50 条消息通常远不到 200k 窗口。结论：这是"按消息条数拍脑袋"的启发式，收益低、丢失高、还有磁盘抖动。

### 3.2 "120 字符"是误读，但结论方向正确

`microMinLen=120` 的含义是"**短于** 120 字符的旧结果不替换（替换不划算）"，代码**没有**把结果截断到 120 字符。真实行为是：≥120 字符的已消费 `tool_result` 被整条替换为 `[Earlier tool result saved at <path>]`，**预览为 0**，模型只能再读文件。所以"只剩 120 字符"不成立，但"替换后基本没有价值"成立——真正的问题不在 120，而在于：

1. 触发看字符总量（50k 字节）而非 token，与窗口压力脱钩；
2. 替换后不留任何预览；
3. 只要超 50k 字节就发生，60k 也压、170k 也压。

另外旧 `fitToolResults` 会留 1000 字符预览，但它同样只由 50k 字节超限触发。

## 4. 差距矩阵

等级：P0 = 影响正确性/成本，本期修；P1 = 明显收益，本期修；P2 = 本期不实现，仅记录。

| # | 域 | 设计稿 | 改造前现状 | 影响 | 等级 | 本期 |
|---|---|---|---|---|---|---|
| G1 | 计数与阈值 | usage 锚点 + 字符粗估 ×4/3；167k 触发 | JSON 字节 50k（≈12k token） | 过早压缩、频繁改写前缀、成本高 | P0 | ✅ |
| G2 | snip | 无此层（仅 microcompact/auto-compact） | >50 条硬切中段，每轮重复切 | 无谓丢历史、每轮写全量 transcript | P0 | ✅ 删除 |
| G3 | auto-compact 信息保留 | boundary + summary + 附件（最近 5 文件/plan/skills/工具声明） | 单条 `[Compacted]` 覆盖全历史 | 计划/文件/技能全丢；`st.reads` 与内容不一致导致误编辑 | P0 | ✅（简化附件 + 保留尾部） |
| G4 | 摘要 prompt | 禁工具前言 + 九章节 + `<analysis>` 后处理 + verbatim 要求 | 5 行事实清单、maxTokens 2000 | 摘要漂移、输出被截断 | P0 | ✅ |
| G5 | 失败策略 | auto 静默 + 连续 3 次熔断；manual 报错；不落错误串 | 失败时把错误串当摘要**替换全历史**；无熔断 | 一次网络错误即毁掉上下文 | P0 | ✅ |
| G6 | microcompact | time-based（idle ≥60min）、工具白名单、keep 5、占位符幂等、单次估算 | 字符超限触发、无白名单、0 预览、每次替换全量 `json.Marshal` | O(n²)、会清掉 todo/task 结果、收益与压力脱钩 | P1 | ✅ |
| G7 | 缓存语义 | 仅在缓存必然失效时瘦身 | 无 `cache_control` | time-based 收益退化为"减小输入" | P2 | 记录 |
| G8 | blocking limit / 告警线 | 177k 阻断、147k 告警 | 无 | 关掉 auto-compact 后只能靠 400 兜底 | P2 | 记录 |
| G9 | PTL 剥洋葱 | 3 次按 API 轮次丢弃 | reactive 仅重试 1 次，且把老历史全量再喂摘要 | 大概率二次 PTL | P2 | 记录 |
| G10 | 工具声明附件 | 压缩后全量重发 MCP/agent 声明 | 工具每轮由 `assembleToolPool` 全量重发 | 天然满足，无需附件 | — | 记录 |
| G11 | 图片脱水 | 压缩前图片→`[image]` | 无图片输入通道（read 只读 UTF-8，工具结果全文本） | N/A | — | 记录 |
| G12 | 自定义摘要指令 | `/compact <指令>` / 钩子 | 无命令、无配置来源 | 无法照抄 | P2 | 记录 |

## 5. 重点问题分析（数据推导）

### 5.1 50k 字节 ≈ 多少 token

- 英文 ASCII 主导：`chars/4` 粗估 → 50_000 字节 ≈ **12.5k token**；
- Go 的 `json.Marshal` 会把 `& < >` 转义成 `\u0026` 等 6 字节序列，字节数虚高、token 不变，实际更低；
- 中文 3 字节/字，粗估约 1 token/字 → 50_000 字节 ≈ 16.7k token，是上限。

即旧触发点在 **12k–17k token**，占 200k 窗口的 6%–8.5%。

### 5.2 与 200k 窗口的差距

设计稿的触发线：`200k − 20k（摘要输出预留）− 13k（缓冲）= 167k`（窗口 83.5%）。旧实现约在 6% 处开始压缩，**早响约 14 倍**。后果：大量本可留在上下文的细节被提前替换/摘要，且压缩后的前缀每轮被重写——虽然本项目没有 prompt cache，但"把还没失效的信息扔掉"本身就是信息损失。

### 5.3 snip 每轮重复切 + transcript 磁盘抖动

一轮工具调用 +2 条消息（assistant 的 tool_use + user 的 tool_result）。消息数一旦过 50，之后**每一轮** `prepare` 都会：归档全量 → 重写 `.transcripts/transcript-<ts>.json`（一个新文件）→ 重组消息数组。连续跑 100 轮工具的任务，会写约 50 份几乎相同的 transcript。归档还把中段细节从上下文里剥掉，触发时机却与"窗口是否紧张"毫无关系。

### 5.4 `st.reads` 导致"看不到内容却通过写入门控"

`edit_file` 的 read-before-write 门控只查 `st.reads[path]` 是否存在。压缩后：历史里该文件的内容没了（只剩 summary），`st.reads` 仍在——门控放行，模型只能凭 summary 里的模糊描述猜 `old_text`，轻则编辑失败，重则**错位替换**。本期方案：压缩附件从磁盘重读最近 5 个文件（内容以磁盘为准），让模型在压缩后重新"看到"正在处理的文件。

### 5.5 摘要失败把错误串写进历史

旧 `summarizeHistory` 失败时返回 `"(summary failed: <err>; full transcript archived)"`，`compactHistory` 拿它**替换全部历史**。一次网络抖动 = 上下文里只剩一行错误信息和一条 transcript 路径，active request 也被卷走。设计稿的策略是：自动触发静默失败（下轮重试）、连续 3 次熔断、手动触发报错、绝不把错误串当摘要。

### 5.6 `json.Marshal` 在替换循环中的 O(n²)

旧 `microCompact`/`fitToolResults` 每替换一条就调一次 `estimateChars`（对**全部** messages 做 `json.Marshal`）。n 条消息、k 次替换的代价是 O(k·n)；上下文越大越慢，而此时恰恰是上下文最大的时刻。

## 6. 改造方案（已落地）

### 6.1 新流水线

[compact.go](../compact.go) 每轮 `prepare` 按固定顺序：

1. **tool_result_budget**（保留）：当轮 tool_result 超 200k 字符时落盘大结果、留 2k 预览；有替换则重置 usage 锚点。
2. **micro_compact（time-based）**：距最后一条 assistant 响应 ≥60 分钟时，把白名单工具（`read_file`/`bash`/`grep`/`glob`/`write_file`/`edit_file`）的旧 tool_result 替换为 `[Old tool result content cleared]`，保留最近 5 个；幂等、不动消息结构、按清理量扣减锚点。
3. **auto_compact**：`tokenCount ≥ 167k`（`AUTO_COMPACT_WINDOW` 只能向下覆盖）才触发——写归档 → 九章节摘要 → 组装 `[Compacted] pre_compact_tokens=N` 摘要消息 + 附件（最近 5 文件 ≤50k token / plan / skills ≤25k token）+ 最近 4 条尾部（配对保护）。失败返回原历史并累计熔断（3 次后停试）。

另有 **reactive_compact**（保留）：API 返回 `prompt_too_long` 时归档 + 总结 + 保留最近 5 条；摘要失败改用固定兜底文本，不再写错误串。

### 6.2 token 计数

- `observeUsage(usage, msgCount)`：每次成功响应后记录官方计数（input+output+cache），作为锚点；
- `tokenCount` = 锚点 + `estimateTokens(锚点之后的新消息)`；历史被替换/结构性修改时 `resetAnchor()` 退回纯估算；
- 粗估：正文 rune 数 `ceil(runes/4)`，累加后 ×4/3 保守放大（覆盖 JSON 包装）。

### 6.3 新参数表

| 参数 | 新值 | 说明 |
|---|---|---|
| `contextWindow` | 200_000（`AUTO_COMPACT_WINDOW` 仅向下覆盖） | 对齐设计稿 min 语义 |
| 触发线 | 167_000 | `window − 20k − 13k` |
| 摘要 maxTokens | 8_000 | 原 2_000，避免长会话摘要被截断 |
| microcompact 触发 | idle ≥60 分钟 | 替代"50k 字节" |
| microcompact 保留 | 最近 5 个白名单结果 | 替代 keep 3"已消费" |
| 占位符 | `[Old tool result content cleared]` | 幂等标记 |
| 尾部保留 | 4 条（配对保护） | 降低摘要漂移 |
| 附件预算 | 文件 5×5k/50k，skills 5k/25k，plan 全量 | 对齐设计稿 §2.5 |

### 6.4 接线变化

- [agent.go](../agent.go)：`prepare(ctx, st, messages, activeRequest)` 传入会话状态；`Send` 成功后 `observeUsage(resp.Usage, len(messages))`；`compact` 工具触发的压缩失败时打印 `[compact failed: ...]`，不注入消息。
- [skill.go](../skill.go)：`skillLoader` 记录首次加载顺序（`loaded`），供压缩附件重建。
- 验证：`compact_test.go` 覆盖锚点计数、阈值换算、熔断、time-based 清理（含幂等/白名单/配对）、附件与尾部、摘要后处理（见第 8 节）。

## 7. 与设计稿的差异与不实现项

| 项 | 差异/不实现原因 |
|---|---|
| 摘要调用不传 tools | 设计稿为缓存一致性继承完整工具集；本项目无 `cache_control`，继承无收益且增加误调风险，故传 `nil`（PREAMBLE 仍双重禁工具） |
| boundary 消息 | 设计稿用 system role 的 `compact_boundary`；本项目 messages 无 system role，边界标记（`pre_compact_tokens`）放在 summary user 消息首行 |
| 缓存语义（G7） | 未启用 `cache_control`，time-based microcompact 的收益是"空闲恢复后缩小首轮全量重写"，不是保护缓存 |
| blocking limit / 告警线（G8） | 177k 阻断、147k "Context low" 属于 UI/阻断层，本项目无对应通道 |
| PTL 剥洋葱（G9） | reactive 保留"重试 1 次 + 保留尾部"的简单兜底；按 API 轮次分组丢弃是独立后续能力 |
| 工具声明附件（G10） | `assembleToolPool` 每轮全量重发工具，天然满足 |
| 图片脱水（G11） | 无图片输入通道（read 只读 UTF-8，工具结果全文本），N/A |
| 自定义摘要指令（G12） | 无 `/compact <指令>` 与配置来源，待命令系统落地后再接 |
| 手动 `/compact` 命令 | 模型可主动调 `compact` 工具（保留），用户斜杠命令不做 |

## 8. 验证方式

### 8.1 单测清单（compact_test.go）

- `TestTokenCountUsesUsageAnchor` / `TestAutoCompactThresholdFromWindow` / `TestShouldAutoCompact` — 计数与阈值
- `TestMicroCompactIdleSkipsRecentAssistant` / `...ClearsOldWhitelisted` / `...Idempotent` / `...PreservesToolUse` — time-based 清理
- `TestAutoCompactKeepsSummaryTailAndAttachments` / `TestAutoCompactFailureKeepsHistory` — 组装与失败保历史
- `TestFormatSummaryStripsAnalysis` — 摘要后处理
- `TestReactiveCompactFallbackOnSummaryError` — reactive 兜底
- `TestPrepareNoOpWhenBelowLimits` / `TestPrepareTriggersAutoCompactOnUsage` / `TestPrepareFailureKeepsHistoryAndIncrementsFailures` — prepare 集成

### 8.2 建议观察的日志字段

- `micro_compact`：`gap_minutes` / `cleared` / `tokens_saved`
- `auto_compact`：`tokens` / `threshold` / `failures` / `messages_before` / `messages_after`
- `auto_compact failed`：`err` / `failures`（出现 3 次即熔断停试）
