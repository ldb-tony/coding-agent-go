package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestCommaTokens(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{678, "678"},
		{12345, "12,345"},
		{1000000, "1,000,000"},
		{-12345, "-12,345"},
	}
	for _, c := range cases {
		if got := commaTokens(c.in); got != c.want {
			t.Errorf("commaTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// captureStdout 临时接管 os.Stdout，运行 fn 后返回捕获到的输出。

type mockClient struct {
	responses []*anthropic.Message
	calls     int
}

func (m *mockClient) Send(ctx context.Context, system string, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam, maxTokens int64) (*anthropic.Message, error) {
	resp := m.responses[m.calls]
	m.calls++
	return resp, nil
}

func mustMessage(t *testing.T, raw string) *anthropic.Message {
	t.Helper()
	var msg anthropic.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal mock message: %v", err)
	}
	return &msg
}

func textResponse(t *testing.T) *anthropic.Message {
	t.Helper()
	return mustMessage(t, `{"id":"msg_text","type":"message","role":"assistant","model":"mock","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
}

func setupLogPaths(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	oldPath := messagesLogPath
	oldSubPath := subMessagesLogPath
	messagesLogPath = filepath.Join(dir, "messages.json")
	subMessagesLogPath = filepath.Join(dir, "messages-sub.json")
	t.Cleanup(func() {
		messagesLogPath = oldPath
		subMessagesLogPath = oldSubPath
	})
	setMemoryEnabled(t, false)
}

func toolResultTexts(t *testing.T, msg anthropic.MessageParam) []*anthropic.ToolResultBlockParam {
	t.Helper()
	var results []*anthropic.ToolResultBlockParam
	for _, block := range msg.Content {
		if block.OfToolResult != nil {
			results = append(results, block.OfToolResult)
		}
	}
	return results
}

func TestAgentLoopExitsWithoutToolUse(t *testing.T) {
	setupLogPaths(t)

	client := &mockClient{responses: []*anthropic.Message{textResponse(t)}}
	initial := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("hello")),
	}

	messages, err := agentLoop(context.Background(), client, initial, "test request")
	if err != nil {
		t.Fatalf("agentLoop: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("expected 1 Send call, got %d", client.calls)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[1].Role != anthropic.MessageParamRoleAssistant {
		t.Fatalf("expected last message role assistant, got %q", messages[1].Role)
	}
	if got := toolResultTexts(t, messages[1]); len(got) != 0 {
		t.Fatalf("expected no tool_result blocks in last message")
	}
}

func TestAgentLoopContinuesWithToolUse(t *testing.T) {
	setupLogPaths(t)

	toolUseResp := mustMessage(t, `{"id":"msg_tool","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"echo hi"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	client := &mockClient{responses: []*anthropic.Message{toolUseResp, textResponse(t)}}
	initial := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("run echo")),
	}

	messages, err := agentLoop(context.Background(), client, initial, "test request")
	if err != nil {
		t.Fatalf("agentLoop: %v", err)
	}
	if client.calls != 2 {
		t.Fatalf("expected 2 Send calls, got %d", client.calls)
	}
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(messages))
	}

	if messages[0].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("messages[0]: expected role user, got %q", messages[0].Role)
	}
	if messages[1].Role != anthropic.MessageParamRoleAssistant {
		t.Fatalf("messages[1]: expected role assistant, got %q", messages[1].Role)
	}
	if messages[2].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("messages[2]: expected role user, got %q", messages[2].Role)
	}
	if messages[3].Role != anthropic.MessageParamRoleAssistant {
		t.Fatalf("messages[3]: expected role assistant, got %q", messages[3].Role)
	}

	results := toolResultTexts(t, messages[2])
	if len(results) != 1 {
		t.Fatalf("expected 1 tool_result in messages[2], got %d", len(results))
	}
	if results[0].ToolUseID != "toolu_1" {
		t.Fatalf("expected tool_use_id toolu_1, got %q", results[0].ToolUseID)
	}
	found := false
	for _, c := range results[0].Content {
		if c.OfText != nil && strings.Contains(c.OfText.Text, "hi") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected tool_result content to contain %q", "hi")
	}
}

func TestAgentLoopParallelToolUse(t *testing.T) {
	setupLogPaths(t)

	parallelResp := mustMessage(t, `{"id":"msg_parallel","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_a","name":"bash","input":{"command":"echo one"}},{"type":"tool_use","id":"toolu_b","name":"bash","input":{"command":"echo two"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	client := &mockClient{responses: []*anthropic.Message{parallelResp, textResponse(t)}}
	initial := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("run both")),
	}

	messages, err := agentLoop(context.Background(), client, initial, "test request")
	if err != nil {
		t.Fatalf("agentLoop: %v", err)
	}
	if client.calls != 2 {
		t.Fatalf("expected 2 Send calls, got %d", client.calls)
	}
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(messages))
	}
	if messages[2].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("messages[2]: expected role user, got %q", messages[2].Role)
	}

	results := toolResultTexts(t, messages[2])
	if len(results) != 2 {
		t.Fatalf("expected 2 tool_result blocks in one user message, got %d", len(results))
	}
	if results[0].ToolUseID != "toolu_a" || results[1].ToolUseID != "toolu_b" {
		t.Fatalf("unexpected tool_use_ids: %q, %q", results[0].ToolUseID, results[1].ToolUseID)
	}

	want := []string{"one", "two"}
	for i, res := range results {
		found := false
		for _, c := range res.Content {
			if c.OfText != nil && strings.Contains(c.OfText.Text, want[i]) {
				found = true
			}
		}
		if !found {
			t.Fatalf("tool_result %d: expected content to contain %q", i, want[i])
		}
	}
}

// TestAgentLoopConcurrentReads 验证多个 read_file 调用可以并发执行。
func TestAgentLoopConcurrentReads(t *testing.T) {
	setupLogPaths(t)
	dir := setupWorkdir(t)

	// 创建两个测试文件
	for _, name := range []string{"test_read_a.txt", "test_read_b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("content of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 模型返回两个 read_file 调用
	concurrentResp := mustMessage(t, `{"id":"msg_concurrent","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_read_a","name":"read_file","input":{"path":"test_read_a.txt"}},{"type":"tool_use","id":"toolu_read_b","name":"read_file","input":{"path":"test_read_b.txt"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	client := &mockClient{responses: []*anthropic.Message{concurrentResp, textResponse(t)}}
	initial := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("read both files")),
	}

	messages, err := agentLoop(context.Background(), client, initial, "test request")
	if err != nil {
		t.Fatalf("agentLoop: %v", err)
	}
	if client.calls != 2 {
		t.Fatalf("expected 2 Send calls, got %d", client.calls)
	}

	// 验证两个 read_file 都被执行，结果正确
	results := toolResultTexts(t, messages[2])
	if len(results) != 2 {
		t.Fatalf("expected 2 tool_result blocks, got %d", len(results))
	}
	if results[0].ToolUseID != "toolu_read_a" || results[1].ToolUseID != "toolu_read_b" {
		t.Fatalf("unexpected tool_use_ids: %q, %q", results[0].ToolUseID, results[1].ToolUseID)
	}

	// 验证结果按原顺序返回
	want := []string{"content of test_read_a.txt", "content of test_read_b.txt"}
	for i, res := range results {
		found := false
		for _, c := range res.Content {
			if c.OfText != nil && strings.Contains(c.OfText.Text, want[i]) {
				found = true
			}
		}
		if !found {
			t.Fatalf("tool_result %d: expected content to contain %q", i, want[i])
		}
	}
}

// TestAgentLoopTodoReminder 模拟连续 3 轮非 todo_write 的工具调用，验证 reminder 被注入。
func TestAgentLoopTodoReminder(t *testing.T) {
	setupLogPaths(t)

	// 3 轮 bash（每轮一个 tool_use），然后 text response 退出。
	resp1 := mustMessage(t, `{"id":"msg1","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"echo a"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	resp2 := mustMessage(t, `{"id":"msg2","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_2","name":"bash","input":{"command":"echo b"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	resp3 := mustMessage(t, `{"id":"msg3","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_3","name":"bash","input":{"command":"echo c"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)

	client := &mockClient{responses: []*anthropic.Message{resp1, resp2, resp3, textResponse(t)}}
	initial := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("do stuff")),
	}

	messages, err := agentLoop(context.Background(), client, initial, "test request")
	if err != nil {
		t.Fatalf("agentLoop: %v", err)
	}
	if client.calls != 4 {
		t.Fatalf("expected 4 Send calls, got %d", client.calls)
	}

	// 遍历所有 user message 的 text blocks 找 reminder
	var reminderFound bool
	for mi := 0; mi < len(messages); mi++ {
		m := messages[mi]
		if m.Role != anthropic.MessageParamRoleUser {
			continue
		}
		for _, block := range m.Content {
			if block.OfText != nil && strings.Contains(block.OfText.Text, "reminder") {
				reminderFound = true
			}
		}
	}
	if !reminderFound {
		t.Fatal("expected reminder text block after 3 consecutive non-todo_write rounds")
	}
}

// captureStdout 临时接管 os.Stdout，运行 fn 后返回捕获到的输出。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestAgentLoopPrintsTokenUsage 验证每轮模型返回后 stdout 打印 token 用量行，含当轮值与会话累计。
func TestAgentLoopPrintsTokenUsage(t *testing.T) {
	setupLogPaths(t)

	resp1 := mustMessage(t, `{"id":"msg1","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"echo hi"}}],"stop_reason":"tool_use","usage":{"input_tokens":12345,"output_tokens":678,"cache_creation_input_tokens":100,"cache_read_input_tokens":10000}}`)
	resp2 := mustMessage(t, `{"id":"msg2","type":"message","role":"assistant","model":"mock","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":13000,"output_tokens":22}}`)
	client := &mockClient{responses: []*anthropic.Message{resp1, resp2}}
	initial := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("hello")),
	}

	stdout := captureStdout(t, func() {
		if _, err := agentLoop(context.Background(), client, initial, "test request"); err != nil {
			t.Errorf("agentLoop: %v", err)
		}
	})

	count := 0
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "[tokens] ") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected 2 [tokens] lines, got %d in:\n%s", count, stdout)
	}
	if want := "[tokens] in=12,345 out=678 cache_create=100 cache_read=10,000 | Σ in=12,345 out=678"; !strings.Contains(stdout, want) {
		t.Fatalf("expected first-round line %q in:\n%s", want, stdout)
	}
	if want := "[tokens] in=13,000 out=22 cache_create=0 cache_read=0 | Σ in=25,345 out=700"; !strings.Contains(stdout, want) {
		t.Fatalf("expected second-round line %q in:\n%s", want, stdout)
	}
}
