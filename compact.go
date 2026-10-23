// compact.go 实现 s08 的上下文压缩流水线（对齐 Claude Code 的设计）：
// 每轮模型调用前按固定顺序执行，成本与信息损失递增：
//
//	Step 1 tool_result_budget — 当轮 tool_result 合计超预算时，把单条超大结果
//	                            落盘到 .task_outputs/tool-results/，上下文里只留预览
//	Step 2 micro_compact      — 空闲 ≥60 分钟（prompt cache 必然失效）时，掏空
//	                            白名单工具的旧 tool_result（替换为占位符），
//	                            只保留最近 5 个；不动消息结构，保证配对合法
//	Step 3 auto_compact       — token 估算达到窗口阈值（200k → 167k）时才调模型
//	                            生成九章节 summary，历史替换为
//	                            [Compacted] 摘要消息 + 附件（最近文件/plan/skills）+ 最近尾部
//
// 与旧版（按 50 条消息硬切 snip、按 50K JSON 字节触发）的差异：
//   - 计数以 API usage 为锚点、粗估增量（tokenCount），阈值与真实窗口对齐
//   - microcompact 只在空闲期触发（收益是缩小下轮全量重写，而非保护缓存——
//     本项目未启用 cache_control，见 docs/compact-gap-analysis.md）
//   - auto-compact 失败不再把错误串写进历史，连续 3 次失败熔断
//
// 另有 reactive_compact：API 直接返回 prompt_too_long 时兜底，保留最近 5 条，
// 最多重试 1 次，第二次失败抛给调用方。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
)

const (
	// Step 1: tool_result_budget
	toolResultBudgetChars   = 200000 // 一轮 tool_result 合计字符上限
	largeResultCharLimit    = 30000  // 单条 tool_result 触发落盘的门槛
	largeResultPreviewChars = 2000   // 落盘后留在上下文里的预览长度

	// 窗口与阈值（对齐设计稿 §2.1；AUTOCOMPACT_THRESHOLD = window - 20k - 13k）
	defaultContextWindow          = 200000
	maxOutputTokensForSummary     = 20000 // 压缩调用的输出预留（API 硬校验 input+max_tokens ≤ window）
	autoCompactBufferTokens       = 13000 // 触发提前量：容忍估算误差与新消息追加延迟
	maxConsecutiveCompactFailures = 3     // auto-compact 连续失败熔断次数

	// Step 2: micro_compact（time-based；对齐设计稿 §2.2）
	mcGapMinutes       = 60 // 距最后一条 assistant 响应 ≥60 分钟才触发
	mcKeepRecent       = 5  // 保留最近 5 个可压缩工具结果，更旧的被掏空
	clearedPlaceholder = "[Old tool result content cleared]"

	// Step 3: auto_compact
	autoCompactKeepRecentMsgs = 4    // 压缩后保留的最近消息条数（≥2 轮）
	summaryMaxTokens          = 8000 // 摘要调用输出上限
	recentFileMaxCount        = 5    // 附件：最近读过的文件最多 N 个
	recentFileTokensEach      = 5000 // 附件：单文件预算（token）
	recentFileTokensTotal     = 50000
	skillTokensEach           = 5000 // 附件：单个 skill 预算（token）
	skillTokensTotal          = 25000
	charsPerToken             = 4 // 文本粗估：≈ chars / 4

	// reactive_compact
	reactiveKeepRecent = 5
	maxReactiveRetries = 1
)

// compactableTools 是 micro_compact 可清理的工具白名单（对齐设计稿 §2.3）。
// 白名单外（todo_write、task、load_skill 等）的结果永不触碰——丢了不可廉价重取。
var compactableTools = map[string]bool{
	"read_file":  true,
	"bash":       true,
	"grep":       true,
	"glob":       true,
	"write_file": true,
	"edit_file":  true,
}

// 摘要 prompt 按设计稿 §5.1 拼装：PREAMBLE + CORE + TRAILER。
// 本项目摘要调用不传 tools（无 cache_control，继承工具集没有缓存收益，
// 反而增加模型误调风险），PREAMBLE 的禁工具列举改为本项目的工具名。
const summaryPreamble = `CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.

- Do NOT use read_file, bash, grep, glob, edit_file, write_file, or ANY other tool.
- You already have all the context you need in the conversation.
- Tool calls will be REJECTED and will waste your only turn — you will fail the task.
- Your entire response must be plain text: an <analysis> block followed by a <summary> block.

`

const summaryCore = `Your task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests and your previous actions.
This summary should be thorough in capturing technical details, code patterns, and architectural decisions that would be essential for continuing development work without losing context.

Before providing your final summary, wrap your analysis in <analysis> tags to organize your thoughts and ensure you've covered all necessary points. In your analysis process:

1. Chronologically analyze each message and section of the conversation. For each section thoroughly identify:
   - The user's explicit requests and intents
   - Your approach to addressing the user's requests
   - Key decisions, technical concepts and code patterns
   - Specific details like:
     - file names
     - full code snippets
     - function signatures
     - file edits
   - Errors that you ran into and how you fixed them
   - Pay special attention to specific user feedback that you received, especially if the user told you to do something differently.
2. Double-check for technical accuracy and completeness, addressing each required element thoroughly.

Your summary should include the following sections:

1. Primary Request and Intent: Capture all of the user's explicit requests and intents in detail
2. Key Technical Concepts: List all important technical concepts, technologies, and frameworks discussed.
3. Files and Code Sections: Enumerate specific files and code sections examined, modified, or created. Pay special attention to the most recent messages and include full code snippets where applicable and include a summary of why this file read or edit is important.
4. Errors and fixes: List all errors that you ran into, and how you fixed them. Pay special attention to specific user feedback that you received, especially if the user told you to do something differently.
5. Problem Solving: Document problems solved and any ongoing troubleshooting efforts.
6. All user messages: List ALL user messages that are not tool results. These are critical for understanding the users' feedback and changing intent.
7. Pending Tasks: Outline any pending tasks that you have explicitly been asked to work on.
8. Current Work: Describe in detail precisely what was being worked on immediately before this summary request, paying special attention to the most recent messages from both user and assistant. Include file names and code snippets where applicable.
9. Optional Next Step: List the next step that you will take that is related to the most recent work you were doing. IMPORTANT: ensure that this step is DIRECTLY in line with the user's most recent explicit requests, and the task you were working on immediately before this summary request. If your last task was concluded, then only list next steps if they are explicitly in line with the users request. Do not start on tangential requests or really old requests that were already completed without confirming with the user first.
                       If there is a next step, include direct quotes from the most recent conversation showing exactly what task you were working on and where you left off. This should be verbatim to ensure there's no drift in task interpretation.

Please provide your summary based on the conversation so far, following this structure and ensuring precision and thoroughness in your response.
`

const summaryTrailer = `
REMINDER: Do NOT call any tools. Respond with plain text only — an <analysis> block followed by a <summary> block. Tool calls will be rejected and you will fail the task.`

// summaryPrompt 是 compact_history / reactive_compact 共用的总结指令（作为 system 传入）。
const summaryPrompt = summaryPreamble + summaryCore + summaryTrailer

// compactor 执行上下文压缩。每个 agentLoop 持有一个实例（需要 client 调 summarize）。
type compactor struct {
	baseDir        string
	transcriptsDir string
	toolResultsDir string
	client         LLMClient
	contextWindow  int

	// usage 锚点：上次成功响应的官方计数（resp.ToParam() 会丢 usage，
	// 所以必须在 compactor 侧单独记录）。
	anchorTokens    int64 // 该响应的 input+output+cache 全量 token
	anchorMsgCount  int   // 该响应对应的 messages 条数
	hasAnchor       bool
	lastAssistantAt time.Time // 最近一次收到 assistant 响应的时间（time-based microcompact 用）

	consecutiveFailures int
	now                 func() time.Time // 测试注入
}

func newCompactor(baseDir string, client LLMClient) *compactor {
	window := defaultContextWindow
	// AUTO_COMPACT_WINDOW 仅向下覆盖（min 语义），兼容 128k 等更小窗口的端点
	if v := os.Getenv("AUTO_COMPACT_WINDOW"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < window {
			window = n
		}
	}
	return &compactor{
		baseDir:        baseDir,
		transcriptsDir: filepath.Join(baseDir, ".transcripts"),
		toolResultsDir: filepath.Join(baseDir, ".task_outputs", "tool-results"),
		client:         client,
		contextWindow:  window,
		now:            time.Now,
	}
}

// prepare 在每轮模型调用前运行完整流水线。activeRequest 是当前用户请求，
// 由调用方显式传入（tool_result 也是 role=user，无法从历史里自动区分）。
func (c *compactor) prepare(ctx context.Context, st *sessionState, messages []anthropic.MessageParam, activeRequest string) []anthropic.MessageParam {
	var budgetChanged bool
	messages, budgetChanged = c.toolResultBudget(messages)
	if budgetChanged {
		c.resetAnchor() // 当轮内容被替换，锚点计数不再可信
	}
	messages, _ = c.microCompactIdle(messages)
	if !c.shouldAutoCompact(messages) {
		return messages
	}
	tokens := c.tokenCount(messages)
	fmt.Println("[auto compact]")
	out, err := c.autoCompact(ctx, st, messages, activeRequest)
	if err != nil {
		// 失败不写任何消息（历史原样保留），累计熔断计数
		c.consecutiveFailures++
		appLog.Error("auto_compact failed", "err", err, "failures", c.consecutiveFailures)
		return messages
	}
	c.consecutiveFailures = 0
	appLog.Info("auto_compact", "tokens", tokens, "threshold", c.autoCompactThreshold(),
		"failures", c.consecutiveFailures, "messages_before", len(messages), "messages_after", len(out))
	return out
}

// ============ token 计数（usage 锚点 + 粗估增量） ============

// autoCompactThreshold 返回 auto-compact 触发线：200k 窗口 → 167k。
func (c *compactor) autoCompactThreshold() int {
	return c.contextWindow - maxOutputTokensForSummary - autoCompactBufferTokens
}

// tokensForRunes 按 chars/4 粗估 token（向上取整）。
func tokensForRunes(runes int) int {
	if runes <= 0 {
		return 0
	}
	return (runes + charsPerToken - 1) / charsPerToken
}

// roughTokens 对单条消息做字符粗估：text/thinking 计正文，tool_use 计 name+input，
// tool_result 计文本；不计 id/role 等包装字段（×4/3 的整体放大覆盖这部分）。
func roughTokens(m anthropic.MessageParam) int {
	runes := 0
	for i := range m.Content {
		block := &m.Content[i]
		switch {
		case block.OfText != nil:
			runes += utf8.RuneCountInString(block.OfText.Text)
		case block.OfThinking != nil:
			runes += utf8.RuneCountInString(block.OfThinking.Thinking)
		case block.OfToolUse != nil:
			runes += utf8.RuneCountInString(block.OfToolUse.Name)
			if data, err := json.Marshal(block.OfToolUse.Input); err == nil {
				runes += utf8.RuneCountInString(string(data))
			}
		case block.OfToolResult != nil:
			runes += utf8.RuneCountInString(toolResultText(block.OfToolResult))
		}
	}
	return tokensForRunes(runes)
}

// estimateTokens 对消息集合做保守估算：累加粗估后 ×4/3 放大（覆盖 JSON 包装开销）。
func (c *compactor) estimateTokens(messages []anthropic.MessageParam) int {
	total := 0
	for i := range messages {
		total += roughTokens(messages[i])
	}
	return (total*4 + 2) / 3
}

// tokenCount 混合计数：锚点（上次 API 实测 usage）+ 锚点之后新增消息的粗估。
// 历史被替换/结构性修改后锚点由 resetAnchor 清空，退回纯估算。
func (c *compactor) tokenCount(messages []anthropic.MessageParam) int {
	if c.hasAnchor && c.anchorMsgCount <= len(messages) {
		return int(c.anchorTokens) + c.estimateTokens(messages[c.anchorMsgCount:])
	}
	return c.estimateTokens(messages)
}

// observeUsage 在每次成功响应后记录官方计数，作为下一轮 tokenCount 的锚点。
// msgCount 是该响应对应的发送时消息条数（agent.go 在 append 响应之前调用）。
func (c *compactor) observeUsage(u anthropic.Usage, msgCount int) {
	c.anchorTokens = u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	c.anchorMsgCount = msgCount
	c.lastAssistantAt = c.now()
	c.hasAnchor = true
}

// resetAnchor 清空锚点（历史被替换或结构性修改后调用）。
func (c *compactor) resetAnchor() {
	c.anchorTokens = 0
	c.anchorMsgCount = 0
	c.hasAnchor = false
}

// ============ Step 1: tool_result_budget ============

// toolResultBudget 只检查最新一批 tool_result（最后一个 user message）：
// 合计字符超 budget 时按大小降序把 >largeResultCharLimit 的单条结果落盘，
// 上下文里替换为"[Large output saved at <path>. Preview ...]"。
// 返回 changed 表示是否有内容被替换（替换后锚点失效）。
func (c *compactor) toolResultBudget(messages []anthropic.MessageParam) ([]anthropic.MessageParam, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	last := &messages[len(messages)-1]
	if last.Role != anthropic.MessageParamRoleUser {
		return messages, false
	}

	type entry struct {
		blockIdx int
		size     int
	}
	var entries []entry
	total := 0
	for bi := range last.Content {
		block := &last.Content[bi]
		if block.OfToolResult == nil {
			continue
		}
		size := len(toolResultText(block.OfToolResult))
		entries = append(entries, entry{blockIdx: bi, size: size})
		total += size
	}
	if total <= toolResultBudgetChars {
		return messages, false
	}

	// 大结果优先落盘；每替换一条重新累计，直到总量回到预算内
	sort.Slice(entries, func(i, j int) bool { return entries[i].size > entries[j].size })
	changed := false
	for _, e := range entries {
		if total <= toolResultBudgetChars {
			break
		}
		if e.size <= largeResultCharLimit {
			continue
		}
		tr := last.Content[e.blockIdx].OfToolResult
		original := toolResultText(tr)
		persisted := c.persistLargeOutput(tr.ToolUseID, original)
		replaceToolResultText(tr, persisted)
		total = total - e.size + len(persisted)
		changed = true
	}
	return messages, changed
}

// persistLargeOutput 把 content 全量写到 <toolUseID>.txt，返回带预览的引用串。
func (c *compactor) persistLargeOutput(toolUseID, content string) string {
	path, err := c.saveOutput(toolUseID, content)
	if err != nil {
		return fmt.Sprintf("[Failed to persist large output: %v]", err)
	}
	preview := truncateRunes(content, largeResultPreviewChars)
	return fmt.Sprintf("[Large output saved at %s. Preview (first %d chars):\n%s\n]",
		path, largeResultPreviewChars, preview)
}

// ============ Step 2: micro_compact（time-based） ============

// microCompactIdle 在空闲 ≥mcGapMinutes（prompt cache TTL，缓存必然失效的时机）
// 时掏空旧的白名单工具结果：替换为 clearedPlaceholder，保留最近 mcKeepRecent 个。
// 收益是缩小下一轮必然发生的全量重写，而非保护缓存（本项目未启用 cache_control）。
// 只改 tool_result 的 content，不动消息结构与 assistant 的 tool_use，保证配对合法。
func (c *compactor) microCompactIdle(messages []anthropic.MessageParam) ([]anthropic.MessageParam, bool) {
	if c.lastAssistantAt.IsZero() {
		return messages, false
	}
	gap := c.now().Sub(c.lastAssistantAt)
	if gap < mcGapMinutes*time.Minute {
		return messages, false
	}

	// 收集白名单工具的 tool_use id（按出现顺序）
	var ids []string
	for mi := range messages {
		if messages[mi].Role != anthropic.MessageParamRoleAssistant {
			continue
		}
		for bi := range messages[mi].Content {
			tu := messages[mi].Content[bi].OfToolUse
			if tu != nil && compactableTools[tu.Name] {
				ids = append(ids, tu.ID)
			}
		}
	}
	keep := mcKeepRecent
	if len(ids) < keep {
		keep = len(ids)
	}
	clearSet := map[string]bool{}
	for _, id := range ids[:len(ids)-keep] {
		clearSet[id] = true
	}
	if len(clearSet) == 0 {
		return messages, false
	}

	placeholderRunes := utf8.RuneCountInString(clearedPlaceholder)
	savedTokens, cleared := 0, 0
	for mi := range messages {
		if messages[mi].Role != anthropic.MessageParamRoleUser {
			continue
		}
		for bi := range messages[mi].Content {
			tr := messages[mi].Content[bi].OfToolResult
			if tr == nil || !clearSet[tr.ToolUseID] {
				continue
			}
			content := toolResultText(tr)
			// 幂等：已是占位符或替换不划算（不比占位符长）的跳过
			if content == clearedPlaceholder || utf8.RuneCountInString(content) <= placeholderRunes {
				continue
			}
			savedTokens += tokensForRunes(utf8.RuneCountInString(content)) - tokensForRunes(placeholderRunes)
			replaceToolResultText(tr, clearedPlaceholder)
			cleared++
		}
	}
	if savedTokens == 0 {
		return messages, false
	}
	// 按清理量扣减锚点（下限 0），保持 tokenCount 反映真实占用
	if c.hasAnchor {
		c.anchorTokens -= int64(savedTokens)
		if c.anchorTokens < 0 {
			c.anchorTokens = 0
		}
	}
	appLog.Info("micro_compact", "gap_minutes", int(gap.Minutes()), "cleared", cleared, "tokens_saved", savedTokens)
	return messages, true
}

// ============ Step 3: auto_compact ============

// shouldAutoCompact 判定是否触发 auto-compact：未熔断且 tokenCount 达到阈值。
func (c *compactor) shouldAutoCompact(messages []anthropic.MessageParam) bool {
	if c.consecutiveFailures >= maxConsecutiveCompactFailures {
		return false
	}
	return c.tokenCount(messages) >= c.autoCompactThreshold()
}

// autoCompact 写归档 → 调模型生成九章节 summary → 组装
// [Compacted] 摘要消息 + 附件（最近文件/plan/skills）+ 最近尾部。
// 任何失败返回 (原 messages, err)，绝不把半成品/错误串写进历史。
func (c *compactor) autoCompact(ctx context.Context, st *sessionState, messages []anthropic.MessageParam, activeRequest string) ([]anthropic.MessageParam, error) {
	preTokens := c.tokenCount(messages)
	transcript := c.writeTranscript(messages)
	fmt.Printf("[transcript saved: %s]\n", transcript)

	summary, err := c.summarizeHistory(ctx, messages)
	if err != nil {
		return messages, err
	}

	out := []anthropic.MessageParam{c.summaryMessage("Compacted", activeRequest, summary, transcript, preTokens)}
	out = append(out, c.buildAttachments(st)...)
	out = append(out, recentTail(messages, autoCompactKeepRecentMsgs)...)
	c.resetAnchor()
	appLog.Info("compact history", "transcript", transcript, "messages", len(messages), "pre_tokens", preTokens)
	return out, nil
}

// recentTail 取最近 keep 条消息，切口保护 tool_use/tool_result 配对：
// tail 开头是 user(tool_result) 且前一条是 assistant(tool_use) 时左移一格。
// start 退到 0 说明找不到干净切口，返回空（全量由 summary 覆盖）。
func recentTail(messages []anthropic.MessageParam, keep int) []anthropic.MessageParam {
	start := len(messages) - keep
	if start < 0 {
		start = 0
	}
	for start > 0 && isToolResult(messages[start]) && hasToolUse(messages[start-1]) {
		start--
	}
	if start == 0 {
		return nil
	}
	return append([]anthropic.MessageParam(nil), messages[start:]...)
}

// buildAttachments 组装压缩后附件（st == nil 时为空），三项相互独立：
//  1. 最近读过的文件：从磁盘重读当前内容——压缩前的 tool_result 已不可见，
//     若不重读，模型会拿着"已读"标记却看不到内容，写出错误的 edit_file old_text
//  2. 当前 plan（todo_write 只存在于工具返回里，不随 system prompt 重发）
//  3. 已加载的 skills（load_skill 的 tool_result 同样会被压缩掉）
func (c *compactor) buildAttachments(st *sessionState) []anthropic.MessageParam {
	if st == nil {
		return nil
	}
	var out []anthropic.MessageParam
	if msg := c.recentFilesAttachment(st); msg != nil {
		out = append(out, *msg)
	}
	if msg := planAttachment(st); msg != nil {
		out = append(out, *msg)
	}
	if msg := skillsAttachment(st); msg != nil {
		out = append(out, *msg)
	}
	return out
}

// recentFilesAttachment 重读最近读过的文件（按 mtime 降序、最多 5 个、单文件
// 5k token、总预算 50k token）；非 UTF-8 或读失败的跳过。
func (c *compactor) recentFilesAttachment(st *sessionState) *anthropic.MessageParam {
	type fileEntry struct {
		path  string
		mtime time.Time
	}
	files := make([]fileEntry, 0, len(st.reads))
	for path, rs := range st.reads {
		files = append(files, fileEntry{path: path, mtime: rs.mtime})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })
	if len(files) > recentFileMaxCount {
		files = files[:recentFileMaxCount]
	}
	if len(files) == 0 {
		return nil
	}

	perFile := recentFileTokensEach * charsPerToken
	total := recentFileTokensTotal * charsPerToken
	var b strings.Builder
	b.WriteString("Recent files re-read from disk after compaction. Earlier tool_result contents are no longer visible in context — treat the following on-disk content as current:\n")
	used, included := 0, false
	for _, f := range files {
		if used >= total {
			break
		}
		data, err := os.ReadFile(f.path)
		if err != nil || !utf8.Valid(data) {
			continue
		}
		content := string(data)
		if runes := utf8.RuneCountInString(content); runes > perFile {
			content = truncateRunes(content, perFile)
		}
		if runes := utf8.RuneCountInString(content); used+runes > total {
			content = truncateRunes(content, total-used)
		}
		fmt.Fprintf(&b, "\n=== %s ===\n%s\n", f.path, content)
		used += utf8.RuneCountInString(content)
		included = true
	}
	if !included {
		return nil
	}
	msg := anthropic.NewUserMessage(anthropic.NewTextBlock(b.String()))
	return &msg
}

// planAttachment 把当前 todo 计划重发为附件（压缩后计划只会彻底消失，否则）。
func planAttachment(st *sessionState) *anthropic.MessageParam {
	if st.todo == nil {
		return nil
	}
	rendered := st.todo.render()
	// render 对空列表返回占位串，不算真实计划
	if rendered == "" || rendered == "(empty todo list)" {
		return nil
	}
	msg := anthropic.NewUserMessage(anthropic.NewTextBlock("# Current plan (from todo_write)\n" + rendered))
	return &msg
}

// skillsAttachment 重发已加载 skill 的正文（单个 5k token、总预算 25k token）。
func skillsAttachment(st *sessionState) *anthropic.MessageParam {
	if st.skills == nil {
		return nil
	}
	loaded := st.skills.loadedContents()
	if len(loaded) == 0 {
		return nil
	}
	perSkill := skillTokensEach * charsPerToken
	total := skillTokensTotal * charsPerToken
	var b strings.Builder
	b.WriteString("Loaded skills re-injected after compaction. The original load_skill tool_result is no longer visible in context:\n")
	used := 0
	for _, s := range loaded {
		if used >= total {
			break
		}
		content := s.Content
		if runes := utf8.RuneCountInString(content); runes > perSkill {
			content = truncateRunes(content, perSkill)
		}
		if runes := utf8.RuneCountInString(content); used+runes > total {
			content = truncateRunes(content, total-used)
		}
		fmt.Fprintf(&b, "\n=== skill: %s ===\n%s\n", s.Name, content)
		used += utf8.RuneCountInString(content)
	}
	msg := anthropic.NewUserMessage(anthropic.NewTextBlock(b.String()))
	return &msg
}

// ============ compact_history / reactive_compact ============

// compactHistory 是手动/工具触发的压缩入口：模型调 compact 工具后由 agent loop
// 在整个 batch 的 tool_result 落库后调用。失败返回原 messages + error，
// 由调用方打印（不注入任何消息）；成功复用 autoCompact 的组装逻辑。
func (c *compactor) compactHistory(ctx context.Context, st *sessionState, messages []anthropic.MessageParam, activeRequest string) ([]anthropic.MessageParam, error) {
	appLog.Info("compact history requested (manual)", "messages", len(messages))
	return c.autoCompact(ctx, st, messages, activeRequest)
}

// reactiveCompact 在 API 返回 prompt_too_long 时兜底：归档 + 总结老的 + 保留最近 5 条。
// 这是"必须丢老历史才能装下"的最后手段：摘要失败也继续（固定兜底文本），
// 历史已归档到 .transcripts/，不丢数据。
func (c *compactor) reactiveCompact(ctx context.Context, messages []anthropic.MessageParam, activeRequest string) []anthropic.MessageParam {
	tail := recentTail(messages, reactiveKeepRecent)
	transcript := c.writeTranscript(messages)
	fmt.Printf("[transcript saved: %s]\n", transcript)

	oldHistory := messages
	if len(tail) > 0 {
		oldHistory = messages[:len(messages)-len(tail)]
	}
	summary, err := c.summarizeHistory(ctx, oldHistory)
	if err != nil {
		appLog.Error("reactive compact summary failed, using fallback", "err", err)
		summary = "(summary unavailable; see transcript)"
	}
	marker := c.summaryMessage("Reactive compact", activeRequest, summary, transcript, c.tokenCount(messages))
	out := []anthropic.MessageParam{marker}
	out = append(out, tail...)
	c.resetAnchor()
	return out
}

// summaryMessage 拼 [Compacted] / [Reactive compact] 的单条 user message。
// 本项目无 system role 消息，压缩边界标记（pre_compact_tokens）只能放在首行。
func (c *compactor) summaryMessage(tag, activeRequest, summary, transcript string, preTokens int) anthropic.MessageParam {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] pre_compact_tokens=%d\n\n", tag, preTokens)
	b.WriteString("This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\n")
	fmt.Fprintf(&b, "Current user request: %s\n\n", activeRequest)
	b.WriteString(summary)
	if transcript != "" {
		fmt.Fprintf(&b, "\n\nIf you need specific details from before compaction (like exact code snippets, error messages, or content you generated), read the full transcript at: %s", transcript)
	}
	return anthropic.NewUserMessage(anthropic.NewTextBlock(b.String()))
}

// summarizeHistory 调模型生成九章节总结；失败返回 error（调用方决定兜底方式，
// 绝不把错误串当摘要写进历史）。
func (c *compactor) summarizeHistory(ctx context.Context, messages []anthropic.MessageParam) (string, error) {
	if c.client == nil {
		return "", errors.New("no LLM client for summary")
	}
	resp, err := c.client.Send(ctx, summaryPrompt, messages, nil, summaryMaxTokens)
	if err != nil {
		appLog.Error("summarize history failed", "err", err)
		return "", err
	}
	var sb strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return formatSummary(sb.String())
}

var (
	analysisRe   = regexp.MustCompile(`(?is)<analysis>.*?</analysis>`)
	blankLinesRe = regexp.MustCompile(`\n{3,}`)
)

// formatSummary 后处理模型原始输出：剥掉 <analysis> 草稿块（用完即弃）、
// <summary> 标签换成可读标题、压缩多余空行；结果为空视为失败。
func formatSummary(raw string) (string, error) {
	s := analysisRe.ReplaceAllString(raw, "")
	s = strings.ReplaceAll(s, "<summary>", "Summary:")
	s = strings.ReplaceAll(s, "</summary>", "")
	s = blankLinesRe.ReplaceAllString(s, "\n\n")
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("empty summary")
	}
	return s, nil
}

// ============ 持久化与辅助 ============

// writeTranscript 把完整 messages 落盘到 .transcripts/transcript-<ts>.json，返回路径；
// 失败返回空串（调用方将其原样写进 marker，不会 panic）。
func (c *compactor) writeTranscript(messages []anthropic.MessageParam) string {
	if err := os.MkdirAll(c.transcriptsDir, 0o755); err != nil {
		return ""
	}
	name := fmt.Sprintf("transcript-%s.json", time.Now().UTC().Format("20060102-150405.000000000"))
	path := filepath.Join(c.transcriptsDir, name)
	data, err := json.MarshalIndent(messages, "", "  ")
	if err != nil {
		return ""
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return ""
	}
	return path
}

// saveOutput 把 tool_result 全量内容写到 .task_outputs/tool-results/<toolUseID>.txt。
func (c *compactor) saveOutput(toolUseID, content string) (string, error) {
	if toolUseID == "" {
		toolUseID = "unknown"
	}
	if err := os.MkdirAll(c.toolResultsDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(c.toolResultsDir, toolUseID+".txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// hasToolUse 判断消息里是否含 tool_use block。
func hasToolUse(m anthropic.MessageParam) bool {
	for i := range m.Content {
		if m.Content[i].OfToolUse != nil {
			return true
		}
	}
	return false
}

// isToolResult 判断消息首块是否是 tool_result（对齐参考实现的"首块判定"）。
func isToolResult(m anthropic.MessageParam) bool {
	return len(m.Content) > 0 && m.Content[0].OfToolResult != nil
}

// toolResultText 提取 tool_result block 的全部 text 内容（拼接多个 text block）。
func toolResultText(tr *anthropic.ToolResultBlockParam) string {
	var sb strings.Builder
	for _, c := range tr.Content {
		if c.OfText != nil {
			sb.WriteString(c.OfText.Text)
		}
	}
	return sb.String()
}

// replaceToolResultText 用单条 text 替换 tool_result 的 Content
// （保留 ToolUseID / IsError 等元数据不变）。
func replaceToolResultText(tr *anthropic.ToolResultBlockParam, text string) {
	tr.Content = []anthropic.ToolResultBlockParamContentUnion{
		{OfText: &anthropic.TextBlockParam{Text: text}},
	}
}

// truncateRunes 按 rune 截断到 n（避免截断多字节字符）。
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// compactHandler 是 compact 工具的处理函数：本身不压缩，只返回一个确认串。
// 真正的 compact_history 在 agent loop 收完整个 tool batch 后执行——
// 否则同一批次里后面的 tool_result 会变成孤儿（没有对应的 tool_use 在历史里）。
func compactHandler(_ *sessionState, _ json.RawMessage) string {
	return "Compaction requested after this tool batch."
}
