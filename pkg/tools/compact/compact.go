// Package compact 实现上下文压缩流水线（对齐 Claude Code 的设计）：
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
//   - 计数以 API usage 为锚点、粗估增量（TokenCount），阈值与真实窗口对齐
//   - microcompact 只在空闲期触发（收益是缩小下轮全量重写，而非保护缓存——
//     本项目未启用 cache_control）
//   - auto-compact 失败不再把错误串写进历史，连续 3 次失败熔断
//
// 另有 ReactiveCompact：API 直接返回 prompt_too_long 时兜底，保留最近 5 条，
// 最多重试 1 次，第二次失败抛给调用方。
package compact

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

	"coding-agent-go/pkg/llm"
	"coding-agent-go/pkg/log"
	"coding-agent-go/pkg/session"
	"coding-agent-go/pkg/textutil"
)

const (
	// Step 1: tool_result_budget
	ToolResultBudgetChars   = 200000 // 一轮 tool_result 合计字符上限
	LargeResultCharLimit    = 30000  // 单条 tool_result 触发落盘的门槛
	LargeResultPreviewChars = 2000   // 落盘后留在上下文里的预览长度

	// 窗口与阈值（对齐设计稿 §2.1；AUTOCOMPACT_THRESHOLD = window - 20k - 13k）
	DefaultContextWindow          = 200000
	MaxOutputTokensForSummary     = 20000 // 压缩调用的输出预留（API 硬校验 input+max_tokens ≤ window）
	AutoCompactBufferTokens       = 13000 // 触发提前量：容忍估算误差与新消息追加延迟
	MaxConsecutiveCompactFailures = 3     // auto-compact 连续失败熔断次数

	// Step 2: micro_compact（time-based；对齐设计稿 §2.2）
	McGapMinutes       = 60 // 距最后一条 assistant 响应 ≥60 分钟才触发
	McKeepRecent       = 5  // 保留最近 5 个可压缩工具结果，更旧的被掏空
	ClearedPlaceholder = "[Old tool result content cleared]"

	// Step 3: auto_compact
	AutoCompactKeepRecentMsgs = 4    // 压缩后保留的最近消息条数（≥2 轮）
	SummaryMaxTokens          = 8000 // 摘要调用输出上限
	RecentFileMaxCount        = 5    // 附件：最近读过的文件最多 N 个
	RecentFileTokensEach      = 5000 // 附件：单文件预算（token）
	RecentFileTokensTotal     = 50000
	SkillTokensEach           = 5000 // 附件：单个 skill 预算（token）
	SkillTokensTotal          = 25000
	CharsPerToken             = 4 // 文本粗估：≈ chars / 4

	// reactive_compact
	ReactiveKeepRecent = 5
	MaxReactiveRetries = 1
)

// CompactableTools 是 micro_compact 可清理的工具白名单（对齐设计稿 §2.3）。
// 白名单外（todo_write、task、load_skill 等）的结果永不触碰——丢了不可廉价重取。
var CompactableTools = map[string]bool{
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
const SummaryPreamble = `CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.

- Do NOT use read_file, bash, grep, glob, edit_file, write_file, or ANY other tool.
- You already have all the context you need in the conversation.
- Tool calls will be REJECTED and will waste your only turn — you will fail the task.
- Your entire response must be plain text: an <analysis> block followed by a <summary> block.

`

const SummaryCore = `Your task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests and your previous actions.
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

const SummaryTrailer = `
REMINDER: Do NOT call any tools. Respond with plain text only — an <analysis> block followed by a <summary> block. Tool calls will be rejected and you will fail the task.`

// SummaryPrompt 是 compact_history / reactive_compact 共用的总结指令（作为 system 传入）。
const SummaryPrompt = SummaryPreamble + SummaryCore + SummaryTrailer

// Compactor 执行上下文压缩。每个 agentLoop 持有一个实例（需要 client 调 summarize）。
type Compactor struct {
	BaseDir        string
	TranscriptsDir string
	ToolResultsDir string
	Client         llm.LLMClient
	ContextWindow  int

	// usage 锚点：上次成功响应的官方计数（resp.ToParam() 会丢 usage，
	// 所以必须在 Compactor 侧单独记录）。
	AnchorTokens    int64 // 该响应的 input+output+cache 全量 token
	AnchorMsgCount  int   // 该响应对应的 messages 条数
	HasAnchor       bool
	LastAssistantAt time.Time // 最近一次收到 assistant 响应的时间（time-based microcompact 用）

	ConsecutiveFailures int
	Now                 func() time.Time // 测试注入
}

// NewCompactor 创建压缩器实例。AUTO_COMPACT_WINDOW 仅向下覆盖（min 语义），
// 兼容 128k 等更小窗口的端点。
func NewCompactor(baseDir string, client llm.LLMClient) *Compactor {
	window := DefaultContextWindow
	if v := os.Getenv("AUTO_COMPACT_WINDOW"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < window {
			window = n
		}
	}
	return &Compactor{
		BaseDir:        baseDir,
		TranscriptsDir: filepath.Join(baseDir, ".transcripts"),
		ToolResultsDir: filepath.Join(baseDir, ".task_outputs", "tool-results"),
		Client:         client,
		ContextWindow:  window,
		Now:            time.Now,
	}
}

// Prepare 在每轮模型调用前运行完整流水线。activeRequest 是当前用户请求，
// 由调用方显式传入（tool_result 也是 role=user，无法从历史里自动区分）。
func (c *Compactor) Prepare(ctx context.Context, st *session.State, messages []anthropic.MessageParam, activeRequest string) []anthropic.MessageParam {
	var budgetChanged bool
	messages, budgetChanged = c.ToolResultBudget(messages)
	if budgetChanged {
		c.ResetAnchor() // 当轮内容被替换，锚点计数不再可信
	}
	messages, _ = c.MicroCompactIdle(messages)
	if !c.ShouldAutoCompact(messages) {
		return messages
	}
	tokens := c.TokenCount(messages)
	fmt.Println("[auto compact]")
	out, err := c.AutoCompact(ctx, st, messages, activeRequest)
	if err != nil {
		// 失败不写任何消息（历史原样保留），累计熔断计数
		c.ConsecutiveFailures++
		log.AppLog.Error("auto_compact failed", "err", err, "failures", c.ConsecutiveFailures)
		return messages
	}
	c.ConsecutiveFailures = 0
	log.AppLog.Info("auto_compact", "tokens", tokens, "threshold", c.AutoCompactThreshold(),
		"failures", c.ConsecutiveFailures, "messages_before", len(messages), "messages_after", len(out))
	return out
}

// ============ token 计数（usage 锚点 + 粗估增量） ============

// AutoCompactThreshold 返回 auto-compact 触发线：200k 窗口 → 167k。
func (c *Compactor) AutoCompactThreshold() int {
	return c.ContextWindow - MaxOutputTokensForSummary - AutoCompactBufferTokens
}

// TokensForRunes 按 chars/4 粗估 token（向上取整）。
func TokensForRunes(runes int) int {
	if runes <= 0 {
		return 0
	}
	return (runes + CharsPerToken - 1) / CharsPerToken
}

// RoughTokens 对单条消息做字符粗估：text/thinking 计正文，tool_use 计 name+input，
// tool_result 计文本；不计 id/role 等包装字段（×4/3 的整体放大覆盖这部分）。
func RoughTokens(m anthropic.MessageParam) int {
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
			runes += utf8.RuneCountInString(textutil.ToolResultText(block.OfToolResult))
		}
	}
	return TokensForRunes(runes)
}

// EstimateTokens 对消息集合做保守估算：累加粗估后 ×4/3 放大（覆盖 JSON 包装开销）。
func (c *Compactor) EstimateTokens(messages []anthropic.MessageParam) int {
	total := 0
	for i := range messages {
		total += RoughTokens(messages[i])
	}
	return (total*4 + 2) / 3
}

// TokenCount 混合计数：锚点（上次 API 实测 usage）+ 锚点之后新增消息的粗估。
// 历史被替换/结构性修改后锚点由 ResetAnchor 清空，退回纯估算。
func (c *Compactor) TokenCount(messages []anthropic.MessageParam) int {
	if c.HasAnchor && c.AnchorMsgCount <= len(messages) {
		return int(c.AnchorTokens) + c.EstimateTokens(messages[c.AnchorMsgCount:])
	}
	return c.EstimateTokens(messages)
}

// ObserveUsage 在每次成功响应后记录官方计数，作为下一轮 TokenCount 的锚点。
// msgCount 是该响应对应的发送时消息条数（agent.go 在 append 响应之前调用）。
func (c *Compactor) ObserveUsage(u anthropic.Usage, msgCount int) {
	c.AnchorTokens = u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	c.AnchorMsgCount = msgCount
	c.LastAssistantAt = c.Now()
	c.HasAnchor = true
}

// ResetAnchor 清空锚点（历史被替换或结构性修改后调用）。
func (c *Compactor) ResetAnchor() {
	c.AnchorTokens = 0
	c.AnchorMsgCount = 0
	c.HasAnchor = false
}

// ============ Step 1: tool_result_budget ============

// ToolResultBudget 只检查最新一批 tool_result（最后一个 user message）：
// 合计字符超 budget 时按大小降序把 >LargeResultCharLimit 的单条结果落盘，
// 上下文里替换为"[Large output saved at <path>. Preview ...]"。
// 返回 changed 表示是否有内容被替换（替换后锚点失效）。
func (c *Compactor) ToolResultBudget(messages []anthropic.MessageParam) ([]anthropic.MessageParam, bool) {
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
		size := len(textutil.ToolResultText(block.OfToolResult))
		entries = append(entries, entry{blockIdx: bi, size: size})
		total += size
	}
	if total <= ToolResultBudgetChars {
		return messages, false
	}

	// 大结果优先落盘；每替换一条重新累计，直到总量回到预算内
	sort.Slice(entries, func(i, j int) bool { return entries[i].size > entries[j].size })
	changed := false
	for _, e := range entries {
		if total <= ToolResultBudgetChars {
			break
		}
		if e.size <= LargeResultCharLimit {
			continue
		}
		tr := last.Content[e.blockIdx].OfToolResult
		original := textutil.ToolResultText(tr)
		persisted := c.PersistLargeOutput(tr.ToolUseID, original)
		ReplaceToolResultText(tr, persisted)
		total = total - e.size + len(persisted)
		changed = true
	}
	return messages, changed
}

// PersistLargeOutput 把 content 全量写到 <toolUseID>.txt，返回带预览的引用串。
func (c *Compactor) PersistLargeOutput(toolUseID, content string) string {
	path, err := c.SaveOutput(toolUseID, content)
	if err != nil {
		return fmt.Sprintf("[Failed to persist large output: %v]", err)
	}
	preview := textutil.TruncateRunes(content, LargeResultPreviewChars)
	return fmt.Sprintf("[Large output saved at %s. Preview (first %d chars):\n%s\n]",
		path, LargeResultPreviewChars, preview)
}

// ============ Step 2: micro_compact（time-based） ============

// MicroCompactIdle 在空闲 ≥McGapMinutes（prompt cache TTL，缓存必然失效的时机）
// 时掏空旧的白名单工具结果：替换为 ClearedPlaceholder，保留最近 McKeepRecent 个。
// 收益是缩小下一轮必然发生的全量重写，而非保护缓存（本项目未启用 cache_control）。
// 只改 tool_result 的 content，不动消息结构与 assistant 的 tool_use，保证配对合法。
func (c *Compactor) MicroCompactIdle(messages []anthropic.MessageParam) ([]anthropic.MessageParam, bool) {
	if c.LastAssistantAt.IsZero() {
		return messages, false
	}
	gap := c.Now().Sub(c.LastAssistantAt)
	if gap < McGapMinutes*time.Minute {
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
			if tu != nil && CompactableTools[tu.Name] {
				ids = append(ids, tu.ID)
			}
		}
	}
	keep := McKeepRecent
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

	placeholderRunes := utf8.RuneCountInString(ClearedPlaceholder)
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
			content := textutil.ToolResultText(tr)
			// 幂等：已是占位符或替换不划算（不比占位符长）的跳过
			if content == ClearedPlaceholder || utf8.RuneCountInString(content) <= placeholderRunes {
				continue
			}
			savedTokens += TokensForRunes(utf8.RuneCountInString(content)) - TokensForRunes(placeholderRunes)
			ReplaceToolResultText(tr, ClearedPlaceholder)
			cleared++
		}
	}
	if savedTokens == 0 {
		return messages, false
	}
	// 按清理量扣减锚点（下限 0），保持 TokenCount 反映真实占用
	if c.HasAnchor {
		c.AnchorTokens -= int64(savedTokens)
		if c.AnchorTokens < 0 {
			c.AnchorTokens = 0
		}
	}
	log.AppLog.Info("micro_compact", "gap_minutes", int(gap.Minutes()), "cleared", cleared, "tokens_saved", savedTokens)
	return messages, true
}

// ============ Step 3: auto_compact ============

// ShouldAutoCompact 判定是否触发 auto-compact：未熔断且 TokenCount 达到阈值。
func (c *Compactor) ShouldAutoCompact(messages []anthropic.MessageParam) bool {
	if c.ConsecutiveFailures >= MaxConsecutiveCompactFailures {
		return false
	}
	return c.TokenCount(messages) >= c.AutoCompactThreshold()
}

// AutoCompact 写归档 → 调模型生成九章节 summary → 组装
// [Compacted] 摘要消息 + 附件（最近文件/plan/skills）+ 最近尾部。
// 任何失败返回 (原 messages, err)，绝不把半成品/错误串写进历史。
func (c *Compactor) AutoCompact(ctx context.Context, st *session.State, messages []anthropic.MessageParam, activeRequest string) ([]anthropic.MessageParam, error) {
	preTokens := c.TokenCount(messages)
	transcript := c.WriteTranscript(messages)
	fmt.Printf("[transcript saved: %s]\n", transcript)

	summary, err := c.SummarizeHistory(ctx, messages)
	if err != nil {
		return messages, err
	}

	out := []anthropic.MessageParam{c.SummaryMessage("Compacted", activeRequest, summary, transcript, preTokens)}
	out = append(out, c.BuildAttachments(st)...)
	out = append(out, RecentTail(messages, AutoCompactKeepRecentMsgs)...)
	c.ResetAnchor()
	log.AppLog.Info("compact history", "transcript", transcript, "messages", len(messages), "pre_tokens", preTokens)
	return out, nil
}

// RecentTail 取最近 keep 条消息，切口保护 tool_use/tool_result 配对：
// tail 开头是 user(tool_result) 且前一条是 assistant(tool_use) 时左移一格。
// start 退到 0 说明找不到干净切口，返回空（全量由 summary 覆盖）。
func RecentTail(messages []anthropic.MessageParam, keep int) []anthropic.MessageParam {
	start := len(messages) - keep
	if start < 0 {
		start = 0
	}
	for start > 0 && IsToolResult(messages[start]) && HasToolUse(messages[start-1]) {
		start--
	}
	if start == 0 {
		return nil
	}
	return append([]anthropic.MessageParam(nil), messages[start:]...)
}

// BuildAttachments 组装压缩后附件（st == nil 时为空），三项相互独立：
//  1. 最近读过的文件：从磁盘重读当前内容——压缩前的 tool_result 已不可见，
//     若不重读，模型会拿着"已读"标记却看不到内容，写出错误的 edit_file old_text
//  2. 当前 plan（todo_write 只存在于工具返回里，不随 system prompt 重发）
//  3. 已加载的 skills（load_skill 的 tool_result 同样会被压缩掉）
func (c *Compactor) BuildAttachments(st *session.State) []anthropic.MessageParam {
	if st == nil {
		return nil
	}
	var out []anthropic.MessageParam
	if msg := c.RecentFilesAttachment(st); msg != nil {
		out = append(out, *msg)
	}
	if msg := PlanAttachment(st); msg != nil {
		out = append(out, *msg)
	}
	if msg := SkillsAttachment(st); msg != nil {
		out = append(out, *msg)
	}
	return out
}

// RecentFilesAttachment 重读最近读过的文件（按 mtime 降序、最多 5 个、单文件
// 5k token、总预算 50k token）；非 UTF-8 或读失败的跳过。
func (c *Compactor) RecentFilesAttachment(st *session.State) *anthropic.MessageParam {
	type fileEntry struct {
		path  string
		mtime time.Time
	}
	reads := st.ReadsSnapshot()
	files := make([]fileEntry, 0, len(reads))
	for path, rs := range reads {
		files = append(files, fileEntry{path: path, mtime: rs.Mtime})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })
	if len(files) > RecentFileMaxCount {
		files = files[:RecentFileMaxCount]
	}
	if len(files) == 0 {
		return nil
	}

	perFile := RecentFileTokensEach * CharsPerToken
	total := RecentFileTokensTotal * CharsPerToken
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
			content = textutil.TruncateRunes(content, perFile)
		}
		if runes := utf8.RuneCountInString(content); used+runes > total {
			content = textutil.TruncateRunes(content, total-used)
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

// PlanAttachment 把当前 todo 计划重发为附件（压缩后计划只会彻底消失，否则）。
func PlanAttachment(st *session.State) *anthropic.MessageParam {
	if st.Todo == nil {
		return nil
	}
	rendered := st.Todo.Render()
	// render 对空列表返回占位串，不算真实计划
	if rendered == "" || rendered == "(empty todo list)" {
		return nil
	}
	msg := anthropic.NewUserMessage(anthropic.NewTextBlock("# Current plan (from todo_write)\n" + rendered))
	return &msg
}

// SkillsAttachment 重发已加载 skill 的正文（单个 5k token、总预算 25k token）。
func SkillsAttachment(st *session.State) *anthropic.MessageParam {
	if st.Skills == nil {
		return nil
	}
	loaded := st.Skills.LoadedContents()
	if len(loaded) == 0 {
		return nil
	}
	perSkill := SkillTokensEach * CharsPerToken
	total := SkillTokensTotal * CharsPerToken
	var b strings.Builder
	b.WriteString("Loaded skills re-injected after compaction. The original load_skill tool_result is no longer visible in context:\n")
	used := 0
	for _, s := range loaded {
		if used >= total {
			break
		}
		content := s.Content
		if runes := utf8.RuneCountInString(content); runes > perSkill {
			content = textutil.TruncateRunes(content, perSkill)
		}
		if runes := utf8.RuneCountInString(content); used+runes > total {
			content = textutil.TruncateRunes(content, total-used)
		}
		fmt.Fprintf(&b, "\n=== skill: %s ===\n%s\n", s.Name, content)
		used += utf8.RuneCountInString(content)
	}
	msg := anthropic.NewUserMessage(anthropic.NewTextBlock(b.String()))
	return &msg
}

// ============ compact_history / reactive_compact ============

// CompactHistory 是手动/工具触发的压缩入口：模型调 compact 工具后由 agent loop
// 在整个 batch 的 tool_result 落库后调用。失败返回原 messages + error，
// 由调用方打印（不注入任何消息）；成功复用 AutoCompact 的组装逻辑。
func (c *Compactor) CompactHistory(ctx context.Context, st *session.State, messages []anthropic.MessageParam, activeRequest string) ([]anthropic.MessageParam, error) {
	log.AppLog.Info("compact history requested (manual)", "messages", len(messages))
	return c.AutoCompact(ctx, st, messages, activeRequest)
}

// ReactiveCompact 在 API 返回 prompt_too_long 时兜底：归档 + 总结老的 + 保留最近 5 条。
// 这是"必须丢老历史才能装下"的最后手段：摘要失败也继续（固定兜底文本），
// 历史已归档到 .transcripts/，不丢数据。
func (c *Compactor) ReactiveCompact(ctx context.Context, messages []anthropic.MessageParam, activeRequest string) []anthropic.MessageParam {
	tail := RecentTail(messages, ReactiveKeepRecent)
	transcript := c.WriteTranscript(messages)
	fmt.Printf("[transcript saved: %s]\n", transcript)

	oldHistory := messages
	if len(tail) > 0 {
		oldHistory = messages[:len(messages)-len(tail)]
	}
	summary, err := c.SummarizeHistory(ctx, oldHistory)
	if err != nil {
		log.AppLog.Error("reactive compact summary failed, using fallback", "err", err)
		summary = "(summary unavailable; see transcript)"
	}
	marker := c.SummaryMessage("Reactive compact", activeRequest, summary, transcript, c.TokenCount(messages))
	out := []anthropic.MessageParam{marker}
	out = append(out, tail...)
	c.ResetAnchor()
	return out
}

// SummaryMessage 拼 [Compacted] / [Reactive compact] 的单条 user message。
// 本项目无 system role 消息，压缩边界标记（pre_compact_tokens）只能放在首行。
func (c *Compactor) SummaryMessage(tag, activeRequest, summary, transcript string, preTokens int) anthropic.MessageParam {
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

// SummarizeHistory 调模型生成九章节总结；失败返回 error（调用方决定兜底方式，
// 绝不把错误串当摘要写进历史）。
func (c *Compactor) SummarizeHistory(ctx context.Context, messages []anthropic.MessageParam) (string, error) {
	if c.Client == nil {
		return "", errors.New("no LLM client for summary")
	}
	resp, err := c.Client.Send(ctx, SummaryPrompt, messages, nil, SummaryMaxTokens)
	if err != nil {
		log.AppLog.Error("summarize history failed", "err", err)
		return "", err
	}
	var sb strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return FormatSummary(sb.String())
}

var (
	AnalysisRe   = regexp.MustCompile(`(?is)<analysis>.*?</analysis>`)
	BlankLinesRe = regexp.MustCompile(`\n{3,}`)
)

// FormatSummary 后处理模型原始输出：剥掉 <analysis> 草稿块（用完即弃）、
// <summary> 标签换成可读标题、压缩多余空行；结果为空视为失败。
func FormatSummary(raw string) (string, error) {
	s := AnalysisRe.ReplaceAllString(raw, "")
	s = strings.ReplaceAll(s, "<summary>", "Summary:")
	s = strings.ReplaceAll(s, "</summary>", "")
	s = BlankLinesRe.ReplaceAllString(s, "\n\n")
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("empty summary")
	}
	return s, nil
}

// ============ 持久化与辅助 ============

// WriteTranscript 把完整 messages 落盘到 .transcripts/transcript-<ts>.json，返回路径；
// 失败返回空串（调用方将其原样写进 marker，不会 panic）。
func (c *Compactor) WriteTranscript(messages []anthropic.MessageParam) string {
	if err := os.MkdirAll(c.TranscriptsDir, 0o755); err != nil {
		return ""
	}
	name := fmt.Sprintf("transcript-%s.json", time.Now().UTC().Format("20060102-150405.000000000"))
	path := filepath.Join(c.TranscriptsDir, name)
	data, err := json.MarshalIndent(messages, "", "  ")
	if err != nil {
		return ""
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return ""
	}
	return path
}

// SaveOutput 把 tool_result 全量内容写到 .task_outputs/tool-results/<toolUseID>.txt。
func (c *Compactor) SaveOutput(toolUseID, content string) (string, error) {
	if toolUseID == "" {
		toolUseID = "unknown"
	}
	if err := os.MkdirAll(c.ToolResultsDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(c.ToolResultsDir, toolUseID+".txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// HasToolUse 判断消息里是否含 tool_use block。
func HasToolUse(m anthropic.MessageParam) bool {
	for i := range m.Content {
		if m.Content[i].OfToolUse != nil {
			return true
		}
	}
	return false
}

// IsToolResult 判断消息首块是否是 tool_result（对齐参考实现的"首块判定"）。
func IsToolResult(m anthropic.MessageParam) bool {
	return len(m.Content) > 0 && m.Content[0].OfToolResult != nil
}

// ReplaceToolResultText 用单条 text 替换 tool_result 的 Content
// （保留 ToolUseID / IsError 等元数据不变）。
func ReplaceToolResultText(tr *anthropic.ToolResultBlockParam, text string) {
	tr.Content = []anthropic.ToolResultBlockParamContentUnion{
		{OfText: &anthropic.TextBlockParam{Text: text}},
	}
}

// CompactHandler 是 compact 工具的处理函数：本身不压缩，只返回一个确认串。
// 真正的 CompactHistory 在 agent loop 收完整个 tool batch 后执行——
// 否则同一批次里后面的 tool_result 会变成孤儿（没有对应的 tool_use 在历史里）。
func CompactHandler(_ *session.State, _ json.RawMessage) string {
	return "Compaction requested after this tool batch."
}
