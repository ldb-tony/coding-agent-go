package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// ============ 测试辅助 ============

// newTestCompactor 把 compactor 的 baseDir 指到 t.TempDir()，避免污染工作区。
func newTestCompactor(t *testing.T, client LLMClient) *compactor {
	t.Helper()
	return newCompactor(t.TempDir(), client)
}

// fakeClock 把 compactor 的时钟换成可推进的假时钟，返回推进函数。
// lastAssistantAt 由 observeUsage 写入，micro_compact 的空闲判定用它。
func fakeClock(c *compactor, base time.Time) func(time.Duration) {
	cur := base
	c.now = func() time.Time { return cur }
	return func(d time.Duration) { cur = cur.Add(d) }
}

// userText 构造一条纯文本 user message。
func userText(text string) anthropic.MessageParam {
	return anthropic.NewUserMessage(anthropic.NewTextBlock(text))
}

// assistantToolUse 构造一条 assistant 消息，只含一个 tool_use block。
func assistantToolUse(id, name string) anthropic.MessageParam {
	return anthropic.NewAssistantMessage(anthropic.ContentBlockParamUnion{
		OfToolUse: &anthropic.ToolUseBlockParam{ID: id, Name: name},
	})
}

// userToolResult 构造一条 user 消息，只含一个 tool_result block（单 text content）。
func userToolResult(toolUseID, content string) anthropic.MessageParam {
	return anthropic.NewUserMessage(anthropic.NewToolResultBlock(toolUseID, content, false))
}

// toolResultOf 返回消息里第 bi 个 block 的 OfToolResult（不存在则 fail）。
func toolResultOf(t *testing.T, msg anthropic.MessageParam, bi int) *anthropic.ToolResultBlockParam {
	t.Helper()
	if bi >= len(msg.Content) {
		t.Fatalf("block index %d out of range (content len %d)", bi, len(msg.Content))
	}
	tr := msg.Content[bi].OfToolResult
	if tr == nil {
		t.Fatalf("block %d is not a tool_result", bi)
	}
	return tr
}

// textOf 返回消息里所有 text block 拼接后的内容（不存在则 fail）。
func textOf(t *testing.T, msg anthropic.MessageParam) string {
	t.Helper()
	var sb strings.Builder
	for _, block := range msg.Content {
		if block.OfText != nil {
			sb.WriteString(block.OfText.Text)
		}
	}
	if sb.Len() == 0 {
		t.Fatal("message has no text block")
	}
	return sb.String()
}

// summaryMockClient 对 summarize 调用返回固定文本。
type summaryMockClient struct {
	summary string
	calls   int
}

func (m *summaryMockClient) Send(ctx context.Context, system string, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam, maxTokens int64) (*anthropic.Message, error) {
	m.calls++
	raw := fmt.Sprintf(`{"id":"msg_summary","type":"message","role":"assistant","model":"mock","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, m.summary)
	var msg anthropic.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// failingMockClient 对所有调用返回固定错误（模拟摘要失败）。
type failingMockClient struct {
	err error
}

func (m *failingMockClient) Send(ctx context.Context, system string, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam, maxTokens int64) (*anthropic.Message, error) {
	return nil, m.err
}

// ============ Step 1: tool_result_budget ============

func TestToolResultBudgetBelowBudget(t *testing.T) {
	c := newTestCompactor(t, nil)
	messages := []anthropic.MessageParam{
		userText("q"),
		userToolResult("tu_1", strings.Repeat("a", 1000)),
	}
	out, changed := c.toolResultBudget(messages)
	if changed {
		t.Fatal("below budget should not change anything")
	}
	if got := toolResultText(toolResultOf(t, out[1], 0)); got != strings.Repeat("a", 1000) {
		t.Fatalf("expected content unchanged, got %d chars", len(got))
	}
}

func TestToolResultBudgetPersistsLargeResult(t *testing.T) {
	c := newTestCompactor(t, nil)
	// 构造 5 条 50K 的大结果，合计 250K 超过 200K 预算
	messages := []anthropic.MessageParam{userText("q")}
	var blocks []anthropic.ContentBlockParamUnion
	for i := 0; i < 5; i++ {
		blocks = append(blocks, anthropic.NewToolResultBlock(
			fmt.Sprintf("tu_%d", i), strings.Repeat(fmt.Sprintf("%d", i), 50000), false))
	}
	messages = append(messages, anthropic.NewUserMessage(blocks...))

	out, changed := c.toolResultBudget(messages)
	if !changed {
		t.Fatal("expected changes when over budget")
	}

	// 至少有一条被替换成 "[Large output saved at ...]"，且对应文件已落盘
	persistedCount := 0
	for _, block := range out[1].Content {
		if block.OfToolResult == nil {
			continue
		}
		text := toolResultText(block.OfToolResult)
		if strings.Contains(text, "[Large output saved at") {
			persistedCount++
			path := filepath.Join(c.toolResultsDir, block.OfToolResult.ToolUseID+".txt")
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("expected persisted file %s: %v", path, err)
			}
		}
	}
	if persistedCount == 0 {
		t.Fatal("expected at least one large result to be persisted")
	}
}

// ============ token 计数 ============

func TestTokenCountUsesUsageAnchor(t *testing.T) {
	c := newTestCompactor(t, nil)
	messages := []anthropic.MessageParam{
		userText("aaaa"), userText("bbbb"), userText("cccc"), userText("dddd"),
	}
	// 锚点：前 2 条消息对应的响应实测 1500 token
	c.observeUsage(anthropic.Usage{InputTokens: 1000, OutputTokens: 500}, 2)

	want := 1500 + c.estimateTokens(messages[2:])
	if got := c.tokenCount(messages); got != want {
		t.Fatalf("tokenCount = %d, want anchor(1500) + incremental(%d)", got, want)
	}
	// 锚点必须覆盖前缀：总量应明显大于纯增量部分
	if got := c.tokenCount(messages); got <= c.estimateTokens(messages[2:]) {
		t.Fatalf("anchor tokens not included: %d", got)
	}
	// 锚点失效后退回纯估算
	c.resetAnchor()
	if got := c.tokenCount(messages); got != c.estimateTokens(messages) {
		t.Fatalf("after reset, tokenCount should be pure estimate: %d vs %d",
			got, c.estimateTokens(messages))
	}
}

func TestAutoCompactThresholdFromWindow(t *testing.T) {
	c := newTestCompactor(t, nil)
	if got := c.autoCompactThreshold(); got != 167000 {
		t.Fatalf("default threshold = %d, want 167000 (200k - 20k - 13k)", got)
	}
	// AUTO_COMPACT_WINDOW 向下覆盖
	t.Setenv("AUTO_COMPACT_WINDOW", "100000")
	c2 := newTestCompactor(t, nil)
	if got := c2.autoCompactThreshold(); got != 67000 {
		t.Fatalf("overridden threshold = %d, want 67000", got)
	}
	// 只能向下（min 语义）：更大的值不生效
	t.Setenv("AUTO_COMPACT_WINDOW", "500000")
	c3 := newTestCompactor(t, nil)
	if got := c3.autoCompactThreshold(); got != 167000 {
		t.Fatalf("larger window must be ignored, got threshold %d", got)
	}
}

func TestShouldAutoCompact(t *testing.T) {
	c := newTestCompactor(t, nil)
	messages := []anthropic.MessageParam{userText("hello")}
	if c.shouldAutoCompact(messages) {
		t.Fatal("below threshold should not trigger")
	}
	// 用锚点伪造达到阈值
	c.observeUsage(anthropic.Usage{InputTokens: int64(c.autoCompactThreshold())}, 0)
	if !c.shouldAutoCompact(messages) {
		t.Fatal("at threshold should trigger")
	}
	// 连续失败达上限 → 熔断
	c.consecutiveFailures = maxConsecutiveCompactFailures
	if c.shouldAutoCompact(messages) {
		t.Fatal("breaker open should not trigger")
	}
}

// ============ Step 2: micro_compact（time-based） ============

// microCompactFixture 构造 8 个工具调用对：1 个 todo_write（非白名单）+ 7 个 read_file，
// 已观察锚点、时钟在 base。
func microCompactFixture(t *testing.T, c *compactor) []anthropic.MessageParam {
	t.Helper()
	messages := []anthropic.MessageParam{userText("start")}
	messages = append(messages,
		assistantToolUse("tu_todo", "todo_write"),
		userToolResult("tu_todo", strings.Repeat("plan line\n", 50)))
	for i := 0; i < 7; i++ {
		messages = append(messages,
			assistantToolUse(fmt.Sprintf("tu_read_%d", i), "read_file"),
			userToolResult(fmt.Sprintf("tu_read_%d", i), strings.Repeat("x", 500)))
	}
	c.observeUsage(anthropic.Usage{InputTokens: 10, OutputTokens: 1}, 0)
	return messages
}

func TestMicroCompactIdleSkipsRecentAssistant(t *testing.T) {
	c := newTestCompactor(t, nil)
	advance := fakeClock(c, time.Unix(1700000000, 0))
	messages := microCompactFixture(t, c)

	advance(30 * time.Minute) // 未到 60 分钟空闲线
	out, changed := c.microCompactIdle(messages)
	if changed {
		t.Fatal("gap < 60min must not trigger micro compact")
	}
	if got := toolResultText(toolResultOf(t, out[4], 0)); !strings.HasPrefix(got, "xxx") {
		t.Fatal("content must remain untouched")
	}
}

func TestMicroCompactIdleClearsOldWhitelisted(t *testing.T) {
	c := newTestCompactor(t, nil)
	advance := fakeClock(c, time.Unix(1700000000, 0))
	messages := microCompactFixture(t, c)

	advance(61 * time.Minute)
	out, changed := c.microCompactIdle(messages)
	if !changed {
		t.Fatal("expected old whitelisted results to be cleared")
	}

	// 白名单里只有 7 个 read_file：保留最后 5 个，清掉最早的 2 个
	for i := 0; i < 7; i++ {
		// fixture 中 user(tu_read_i) 的位置是 4 + 2i
		idx := 4 + i*2
		got := toolResultText(toolResultOf(t, out[idx], 0))
		if i < 2 && got != clearedPlaceholder {
			t.Fatalf("tu_read_%d should be cleared, got %.40q", i, got)
		}
		if i >= 2 && !strings.HasPrefix(got, "xxx") {
			t.Fatalf("tu_read_%d should be kept complete, got %.40q", i, got)
		}
	}
	// 非白名单（todo_write）永不清
	if got := toolResultText(toolResultOf(t, out[2], 0)); !strings.HasPrefix(got, "plan line") {
		t.Fatalf("todo_write result must never be cleared, got %.40q", got)
	}
}

func TestMicroCompactIdleIdempotent(t *testing.T) {
	c := newTestCompactor(t, nil)
	advance := fakeClock(c, time.Unix(1700000000, 0))
	messages := microCompactFixture(t, c)

	advance(61 * time.Minute)
	out, changed := c.microCompactIdle(messages)
	if !changed {
		t.Fatal("first pass should clear old results")
	}
	anchorAfterFirst := c.anchorTokens

	// 第二遍：全部已是占位符，不应有任何改动或再扣锚点
	_, changed2 := c.microCompactIdle(out)
	if changed2 {
		t.Fatal("second pass must be a no-op (idempotent)")
	}
	if c.anchorTokens != anchorAfterFirst {
		t.Fatalf("anchorTokens changed on second pass: %d -> %d", anchorAfterFirst, c.anchorTokens)
	}
}

func TestMicroCompactIdlePreservesToolUse(t *testing.T) {
	c := newTestCompactor(t, nil)
	advance := fakeClock(c, time.Unix(1700000000, 0))
	messages := microCompactFixture(t, c)

	advance(61 * time.Minute)
	out, _ := c.microCompactIdle(messages)

	// 清理只动 tool_result 的 content；assistant 的 tool_use 块必须原样保留
	if got, want := countToolUseMessages(out), countToolUseMessages(messages); got != want {
		t.Fatalf("tool_use blocks changed: %d -> %d", want, got)
	}
	// 配对合法性：任何 user(tool_result) 的前一条必须含 tool_use
	for i := 1; i < len(out); i++ {
		if isToolResult(out[i]) && !hasToolUse(out[i-1]) {
			t.Fatalf("orphan tool_result at index %d", i)
		}
	}
}

// countToolUseMessages 统计含 tool_use 块的消息条数。
func countToolUseMessages(msgs []anthropic.MessageParam) int {
	n := 0
	for _, m := range msgs {
		if hasToolUse(m) {
			n++
		}
	}
	return n
}

// ============ Step 3: auto_compact ============

func TestAutoCompactKeepsSummaryTailAndAttachments(t *testing.T) {
	client := &summaryMockClient{summary: "did step 1, remaining step 2"}
	c := newTestCompactor(t, client)
	dir := t.TempDir()

	// 两个已读文件（b.txt 更新，排前面）
	f1 := filepath.Join(dir, "a.txt")
	f2 := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(f1, []byte("alpha content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("beta content"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &sessionState{
		reads:  map[string]readState{},
		todo:   newTodoManager(),
		skills: newSkillLoader(dir),
	}
	st.reads[f1] = readState{mtime: time.Unix(1000, 0)}
	st.reads[f2] = readState{mtime: time.Unix(2000, 0)}
	if _, err := st.todo.update([]todoItem{
		{Content: "step 1", Status: todoInProgress},
		{Content: "step 2", Status: todoPending},
	}); err != nil {
		t.Fatal(err)
	}
	st.skills.skills["demo"] = skill{Name: "demo", Content: "demo skill body"}
	st.skills.loaded = []string{"demo"}

	messages := []anthropic.MessageParam{
		userText("m0"), userText("m1"), userText("m2"), userText("m3"),
		assistantToolUse("tu_x", "read_file"),
		userToolResult("tu_x", "file body"),
		userText("m6"), userText("m7"),
	}

	out, err := c.autoCompact(context.Background(), st, messages, "current request")
	if err != nil {
		t.Fatalf("autoCompact failed: %v", err)
	}
	// 结构：1 摘要 + 3 附件（文件/plan/skills）+ 4 尾部
	if len(out) != 1+3+autoCompactKeepRecentMsgs {
		t.Fatalf("expected %d messages, got %d", 1+3+autoCompactKeepRecentMsgs, len(out))
	}

	first := textOf(t, out[0])
	for _, want := range []string{
		"[Compacted]", "pre_compact_tokens=", "Current user request: current request",
		"did step 1", ".transcripts/transcript-",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("summary message missing %q:\n%.400s", want, first)
		}
	}
	// 附件 1：最近文件（含磁盘当前内容）
	files := textOf(t, out[1])
	if !strings.Contains(files, "alpha content") || !strings.Contains(files, "beta content") {
		t.Errorf("file attachment missing file contents:\n%.300s", files)
	}
	if !strings.Contains(files, f2) || !strings.Contains(files, f1) {
		t.Errorf("file attachment missing paths:\n%.300s", files)
	}
	// 附件 2：plan
	plan := textOf(t, out[2])
	if !strings.Contains(plan, "Current plan") || !strings.Contains(plan, "step 1") {
		t.Errorf("plan attachment wrong:\n%.200s", plan)
	}
	// 附件 3：skills
	skills := textOf(t, out[3])
	if !strings.Contains(skills, "demo skill body") {
		t.Errorf("skills attachment wrong:\n%.200s", skills)
	}
	// 尾部：最近 4 条原样保留
	for i, want := range messages[4:] {
		got := out[1+3+i]
		if len(got.Content) != len(want.Content) {
			t.Fatalf("tail message %d content length changed", i)
		}
	}
	// 尾部配对合法：assistant(tool_use) 后紧跟其 tool_result
	if !hasToolUse(out[4]) || !isToolResult(out[5]) {
		t.Fatal("tool pair must stay together in the tail")
	}
}

func TestAutoCompactFailureKeepsHistory(t *testing.T) {
	c := newTestCompactor(t, &failingMockClient{err: errors.New("network down")})
	messages := []anthropic.MessageParam{userText("m0"), userText("m1"), userText("m2")}

	out, err := c.autoCompact(context.Background(), nil, messages, "req")
	if err == nil {
		t.Fatal("expected error from summarize")
	}
	// 失败时历史必须原样保留（绝不把错误串写进历史）
	if len(out) != len(messages) {
		t.Fatalf("history must be preserved on failure: got %d messages, want %d", len(out), len(messages))
	}
	for i := range messages {
		if textOf(t, out[i]) != textOf(t, messages[i]) {
			t.Fatalf("message %d was modified on failure", i)
		}
	}
	// 熔断计数由 prepare 负责（见 TestPrepareFailureKeepsHistoryAndIncrementsFailures）
}

func TestFormatSummaryStripsAnalysis(t *testing.T) {
	raw := "<analysis>\ninternal thinking draft\n</analysis>\n<summary>\n1. Primary Request and Intent: do X\n</summary>"
	out, err := formatSummary(raw)
	if err != nil {
		t.Fatalf("formatSummary failed: %v", err)
	}
	if strings.Contains(out, "<analysis>") || strings.Contains(out, "internal thinking draft") {
		t.Fatalf("analysis block must be stripped, got %q", out)
	}
	if strings.Contains(out, "<summary>") {
		t.Fatalf("summary tags must be replaced, got %q", out)
	}
	if !strings.HasPrefix(out, "Summary:") || !strings.Contains(out, "do X") {
		t.Fatalf("unexpected formatted summary: %q", out)
	}
	// 空结果视为失败
	if _, err := formatSummary("<analysis>only draft</analysis>"); err == nil {
		t.Fatal("empty summary must be an error")
	}
}

// ============ reactive_compact ============

func TestReactiveCompactFallbackOnSummaryError(t *testing.T) {
	c := newTestCompactor(t, &failingMockClient{err: errors.New("boom")})
	messages := []anthropic.MessageParam{
		userText("m0"), userText("m1"), userText("m2"),
		userText("m3"), userText("m4"), userText("m5"), userText("m6"), userText("m7"),
	}

	out := c.reactiveCompact(context.Background(), messages, "active req")

	// 1 个 marker + 最近 5 条
	if len(out) != 1+reactiveKeepRecent {
		t.Fatalf("expected %d messages, got %d", 1+reactiveKeepRecent, len(out))
	}
	text := textOf(t, out[0])
	for _, want := range []string{
		"[Reactive compact]", "Current user request: active req",
		"(summary unavailable; see transcript)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("marker missing %q:\n%.400s", want, text)
		}
	}
	// 尾部保留
	for i := 0; i < reactiveKeepRecent; i++ {
		if textOf(t, out[1+i]) != textOf(t, messages[3+i]) {
			t.Fatalf("tail message %d changed", i)
		}
	}
}

// ============ prepare 集成 ============

func TestPrepareNoOpWhenBelowLimits(t *testing.T) {
	c := newTestCompactor(t, nil)
	messages := []anthropic.MessageParam{userText("hello")}
	out := c.prepare(context.Background(), nil, messages, "req")
	if len(out) != 1 {
		t.Fatalf("expected no-op, got %d messages", len(out))
	}
	if out[0].Content[0].OfText.Text != "hello" {
		t.Fatal("expected content unchanged")
	}
}

func TestPrepareTriggersAutoCompactOnUsage(t *testing.T) {
	client := &summaryMockClient{summary: "everything summarized"}
	c := newTestCompactor(t, client)
	messages := []anthropic.MessageParam{userText("m0"), userText("m1"), userText("m2")}
	// 用锚点伪造超过阈值（不再依赖消息体积）
	c.observeUsage(anthropic.Usage{InputTokens: int64(c.autoCompactThreshold())}, 0)

	out := c.prepare(context.Background(), nil, messages, "big request")

	// 历史被替换为 1 条 [Compacted]（消息数 < keepRecent，尾部为空）
	if len(out) != 1 {
		t.Fatalf("expected 1 compacted message, got %d", len(out))
	}
	if !strings.Contains(textOf(t, out[0]), "[Compacted]") {
		t.Fatalf("expected [Compacted] tag, got %q", textOf(t, out[0]))
	}
	if client.calls != 1 {
		t.Fatalf("expected 1 summarize call, got %d", client.calls)
	}
	if c.consecutiveFailures != 0 {
		t.Fatalf("success must reset failures, got %d", c.consecutiveFailures)
	}
}

func TestPrepareFailureKeepsHistoryAndIncrementsFailures(t *testing.T) {
	c := newTestCompactor(t, &failingMockClient{err: errors.New("api down")})
	messages := []anthropic.MessageParam{userText("m0"), userText("m1"), userText("m2")}
	c.observeUsage(anthropic.Usage{InputTokens: int64(c.autoCompactThreshold())}, 0)

	out := c.prepare(context.Background(), nil, messages, "req")

	if len(out) != len(messages) {
		t.Fatalf("history must be preserved, got %d messages", len(out))
	}
	for i := range messages {
		if textOf(t, out[i]) != textOf(t, messages[i]) {
			t.Fatalf("message %d modified on failed compact", i)
		}
	}
	if c.consecutiveFailures != 1 {
		t.Fatalf("consecutiveFailures = %d, want 1", c.consecutiveFailures)
	}
}

// ============ compact 工具 handler ============

func TestCompactHandlerReturnsMarker(t *testing.T) {
	got := compactHandler(nil, nil)
	if !strings.Contains(got, "Compaction requested") {
		t.Fatalf("expected marker string, got %q", got)
	}
}

// ============ 辅助函数 ============

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Fatalf("expected unchanged, got %q", got)
	}
	if got := truncateRunes("hello world", 5); got != "hello..." {
		t.Fatalf("expected truncated with ellipsis, got %q", got)
	}
	// 多字节字符不按 byte 截断
	if got := truncateRunes("你好世界", 2); got != "你好..." {
		t.Fatalf("expected rune-safe truncation, got %q", got)
	}
}
