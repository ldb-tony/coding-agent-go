package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// ============ 测试辅助 ============

// newTestCompactor 把 compactor 的 baseDir 指到 t.TempDir()，避免污染工作区。
func newTestCompactor(t *testing.T, client LLMClient) *compactor {
	t.Helper()
	return newCompactor(t.TempDir(), client)
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

// summaryMockClient 是 mockClient 的扩展：对 summarize 调用返回固定文本。
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

// ============ Step 1: tool_result_budget ============

func TestToolResultBudgetBelowBudget(t *testing.T) {
	c := newTestCompactor(t, nil)
	messages := []anthropic.MessageParam{
		userText("q"),
		userToolResult("tu_1", strings.Repeat("a", 1000)),
	}
	out := c.toolResultBudget(messages)
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

	out := c.toolResultBudget(messages)

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

// ============ Step 2: snip_compact ============

func TestSnipCompactBelowThreshold(t *testing.T) {
	c := newTestCompactor(t, nil)
	messages := []anthropic.MessageParam{userText("q1"), userText("q2")}
	out := c.snipCompact(messages)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages unchanged, got %d", len(out))
	}
}

func TestSnipCompactArchivesOldMessages(t *testing.T) {
	c := newTestCompactor(t, nil)
	// 造 60 条 user 文本消息（无 tool_use/tool_result，切口安全）
	messages := make([]anthropic.MessageParam, 60)
	for i := range messages {
		messages[i] = userText(fmt.Sprintf("msg %d", i))
	}
	out := c.snipCompact(messages)

	// 应该压到 snipMaxMessages=50 条：head 3 + marker 1 + tail 46
	if len(out) != snipMaxMessages {
		t.Fatalf("expected %d messages, got %d", snipMaxMessages, len(out))
	}
	// 中间 marker 是 "[N messages archived at ...]"
	markerText := out[snipHeadKeep].Content[0].OfText.Text
	if !strings.Contains(markerText, "messages archived at") {
		t.Fatalf("expected archive marker, got %q", markerText)
	}
	// 归档文件已写入
	entries, err := os.ReadDir(c.transcriptsDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected transcript file in %s: %v", c.transcriptsDir, err)
	}
}

func TestSnipCompactProtectsToolPairs(t *testing.T) {
	c := newTestCompactor(t, nil)
	// 造 60 条：前 55 条是 user text，最后 5 条是 assistant(tool_use) + user(tool_result) 交替
	messages := make([]anthropic.MessageParam, 0, 60)
	for i := 0; i < 55; i++ {
		messages = append(messages, userText(fmt.Sprintf("msg %d", i)))
	}
	messages = append(messages, assistantToolUse("tu_a", "bash"))
	messages = append(messages, userToolResult("tu_a", "ok"))
	messages = append(messages, assistantToolUse("tu_b", "bash"))
	messages = append(messages, userToolResult("tu_b", "ok"))
	messages = append(messages, userText("tail"))

	out := c.snipCompact(messages)

	// 验证没有孤儿 tool_result：任何 user(tool_result) 的前一条必须是 assistant(tool_use)
	for i := 1; i < len(out); i++ {
		if isToolResult(out[i]) && !hasToolUse(out[i-1]) {
			t.Fatalf("orphan tool_result at index %d; prev message has no tool_use", i)
		}
	}
}

// ============ Step 3: micro_compact ============

func TestMicroCompactReplacesOldConsumedResults(t *testing.T) {
	c := newTestCompactor(t, nil)
	// 5 个已消费的大结果（每个后跟 assistant 消息）+ 1 个未消费的大结果
	messages := []anthropic.MessageParam{userText("start")}
	for i := 0; i < 5; i++ {
		messages = append(messages,
			userToolResult(fmt.Sprintf("tu_old_%d", i), strings.Repeat("x", 5000)),
			assistantToolUse(fmt.Sprintf("tu_next_%d", i), "bash"),
		)
	}
	// 最后一条是未消费的（其后没有 assistant）
	messages = append(messages, userToolResult("tu_new", strings.Repeat("y", 5000)))

	target := c.estimateChars(messages) // 初始值；要求压缩后严格更小
	out := c.microCompact(messages, target-1)

	// 已消费的前 2 个应该被替换（保留最近 3 个已消费 + 1 个未消费）
	replaced := 0
	for mi, m := range out {
		for bi := range m.Content {
			tr := m.Content[bi].OfToolResult
			if tr == nil {
				continue
			}
			if strings.Contains(toolResultText(tr), "[Earlier tool result saved at") {
				replaced++
				_ = mi
				_ = bi
			}
		}
	}
	if replaced == 0 {
		t.Fatal("expected at least one consumed result to be replaced")
	}

	// 未消费的最后一条必须保留完整（其后没有 assistant 消息）
	lastTR := toolResultOf(t, out[len(out)-1], 0)
	if got := toolResultText(lastTR); !strings.HasPrefix(got, "yyy") {
		t.Fatalf("unseen result must remain complete, got prefix %.20q", got)
	}
}

func TestMicroCompactSkipsShortResults(t *testing.T) {
	c := newTestCompactor(t, nil)
	messages := []anthropic.MessageParam{
		userText("start"),
		userToolResult("tu_short", "short"), // < microMinLen=120，不应替换
		assistantToolUse("tu_x", "bash"),
		userToolResult("tu_new", strings.Repeat("z", 5000)),
	}
	target := c.estimateChars(messages) - 1
	out := c.microCompact(messages, target)
	// 短结果保留原样
	if got := toolResultText(toolResultOf(t, out[1], 0)); got != "short" {
		t.Fatalf("short result should remain unchanged, got %q", got)
	}
}

// ============ Step 4: compact_history ============

func TestCompactHistoryReplacesWithSummaryMessage(t *testing.T) {
	client := &summaryMockClient{summary: "did step 1, decided X, remaining: step 2"}
	c := newTestCompactor(t, client)
	messages := []anthropic.MessageParam{
		userText("original request"),
		userText("intermediate"),
		userText("more"),
	}

	out := c.compactHistory(context.Background(), messages, "current query")

	if len(out) != 1 {
		t.Fatalf("expected 1 compacted message, got %d", len(out))
	}
	text := out[0].Content[0].OfText.Text
	if !strings.Contains(text, "[Compacted]") {
		t.Errorf("expected [Compacted] tag, got %q", text)
	}
	if !strings.Contains(text, "Current user request: current query") {
		t.Errorf("expected active request preserved, got %q", text)
	}
	if !strings.Contains(text, "did step 1") {
		t.Errorf("expected summary embedded, got %q", text)
	}
	if !strings.Contains(text, "Full transcript:") {
		t.Errorf("expected transcript path, got %q", text)
	}
	if client.calls != 1 {
		t.Errorf("expected 1 summarize call, got %d", client.calls)
	}
	// 归档文件存在
	entries, _ := os.ReadDir(c.transcriptsDir)
	if len(entries) == 0 {
		t.Error("expected transcript file written")
	}
}

// ============ reactive_compact ============

func TestReactiveCompactKeepsRecentMessages(t *testing.T) {
	client := &summaryMockClient{summary: "older summary"}
	c := newTestCompactor(t, client)

	messages := make([]anthropic.MessageParam, 0, 10)
	for i := 0; i < 8; i++ {
		messages = append(messages, userText(fmt.Sprintf("old %d", i)))
	}
	messages = append(messages, userText("recent 1"), userText("recent 2"))

	out := c.reactiveCompact(context.Background(), messages, "active req")

	// 1 个 [Reactive compact] marker + 最近 5 条
	if len(out) != 1+reactiveKeepRecent {
		t.Fatalf("expected %d messages, got %d", 1+reactiveKeepRecent, len(out))
	}
	text := out[0].Content[0].OfText.Text
	if !strings.Contains(text, "[Reactive compact]") {
		t.Errorf("expected [Reactive compact] tag, got %q", text)
	}
	if !strings.Contains(text, "Current user request: active req") {
		t.Errorf("expected active request, got %q", text)
	}
}

func TestReactiveCompactProtectsToolPairs(t *testing.T) {
	client := &summaryMockClient{summary: "summary"}
	c := newTestCompactor(t, client)

	// 切口位置刚好在 assistant(tool_use) → user(tool_result) 之间
	messages := []anthropic.MessageParam{
		userText("m0"), userText("m1"), userText("m2"), userText("m3"),
		userText("m4"),
		assistantToolUse("tu_x", "bash"), // index 5
		userToolResult("tu_x", "ok"),     // index 6 —— tail_start 算到这里时应左移
		userText("m7"),
		userText("m8"),
	}

	out := c.reactiveCompact(context.Background(), messages, "req")

	// 验证没有孤儿 tool_result
	for i := 1; i < len(out); i++ {
		if isToolResult(out[i]) && !hasToolUse(out[i-1]) {
			t.Fatalf("orphan tool_result at index %d", i)
		}
	}
	// tool_result 和它的 tool_use 要么都在，要么都不在
	foundUse, foundResult := false, false
	for _, m := range out {
		if hasToolUse(m) {
			foundUse = true
		}
		if isToolResult(m) {
			foundResult = true
		}
	}
	if foundUse != foundResult {
		t.Fatalf("tool_use and tool_result must be kept or dropped together (use=%v, result=%v)", foundUse, foundResult)
	}
}

// ============ prepare 集成 ============

func TestPrepareNoOpWhenBelowLimits(t *testing.T) {
	c := newTestCompactor(t, nil)
	messages := []anthropic.MessageParam{userText("hello")}
	out := c.prepare(context.Background(), messages, "req")
	if len(out) != 1 {
		t.Fatalf("expected no-op, got %d messages", len(out))
	}
	if out[0].Content[0].OfText.Text != "hello" {
		t.Fatal("expected content unchanged")
	}
}

func TestPrepareTriggersCompactHistoryWhenOverLimit(t *testing.T) {
	client := &summaryMockClient{summary: "everything summarized"}
	c := newTestCompactor(t, client)

	// 造一堆"短到不会被 micro_compact 替换"的消息，但总量超 50K
	messages := make([]anthropic.MessageParam, 0, 40)
	for i := 0; i < 40; i++ {
		messages = append(messages, userText(strings.Repeat("x", 2000)))
	}

	out := c.prepare(context.Background(), messages, "big request")

	// 应触发 compact_history：1 条 [Compacted] 消息
	if len(out) != 1 {
		t.Fatalf("expected 1 compacted message, got %d", len(out))
	}
	if !strings.Contains(out[0].Content[0].OfText.Text, "[Compacted]") {
		t.Fatalf("expected [Compacted] tag, got %q", out[0].Content[0].OfText.Text)
	}
	if client.calls != 1 {
		t.Fatalf("expected 1 summarize call, got %d", client.calls)
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

func TestUnseenToolResultPositions(t *testing.T) {
	messages := []anthropic.MessageParam{
		userText("q"),
		userToolResult("tu_a", "old"),    // 已消费
		assistantToolUse("tu_b", "bash"), // assistant 截断点
		userToolResult("tu_b", "new"),    // 未消费
	}
	unseen := unseenToolResultPositions(messages)
	if len(unseen) != 1 {
		t.Fatalf("expected 1 unseen, got %d", len(unseen))
	}
	if !unseen[[2]int{3, 0}] {
		t.Fatalf("expected (3,0) to be unseen, got %v", unseen)
	}
}

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
