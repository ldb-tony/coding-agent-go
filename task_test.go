package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// subClient 记录每次 Send 的入参，并按 calls 顺序返回脚本化响应；
// 响应耗尽后重复最后一条（便于测试 30 轮上限）。
type subClient struct {
	responses    []*anthropic.Message
	calls        int
	seenSystem   []string
	seenMessages [][]anthropic.MessageParam
	seenTools    [][]anthropic.ToolUnionParam
}

func (c *subClient) Send(ctx context.Context, system string, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam, maxTokens int64) (*anthropic.Message, error) {
	c.seenSystem = append(c.seenSystem, system)
	c.seenMessages = append(c.seenMessages, messages)
	c.seenTools = append(c.seenTools, tools)
	idx := c.calls
	if idx >= len(c.responses) {
		idx = len(c.responses) - 1
	}
	resp := c.responses[idx]
	c.calls++
	return resp, nil
}

func textMessage(t *testing.T, text string) *anthropic.Message {
	t.Helper()
	raw := fmt.Sprintf(
		`{"id":"msg_t","type":"message","role":"assistant","model":"mock","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		text)
	return mustMessage(t, raw)
}

func readToolUseMessage(t *testing.T) *anthropic.Message {
	t.Helper()
	return mustMessage(t, `{"id":"msg_r","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"hello.txt"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
}

func newSubState(client LLMClient) *sessionState {
	st := newSessionState()
	st.client = client
	st.ctx = context.Background()
	return st
}

// TestSubToolsExcludesTask 验证子代理工具集不含 task（只允许一层委派），其余基础工具保留。
func TestSubToolsExcludesTask(t *testing.T) {
	tools := subTools()
	if len(tools) == 0 {
		t.Fatal("subTools returned no tools")
	}
	var names []string
	for _, tp := range tools {
		if tp.OfTool == nil {
			t.Fatalf("tool entry with nil OfTool")
		}
		names = append(names, tp.OfTool.Name)
	}
	if len(names) != len(allTools())-1 {
		t.Fatalf("expected %d tools (all minus task), got %d", len(allTools())-1, len(names))
	}
	for _, n := range names {
		if n == taskToolName {
			t.Fatalf("subTools must not include %q", taskToolName)
		}
	}
	for _, want := range []string{"bash", "read_file", "write_file", "edit_file", "glob", "grep", "todo_write"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("subTools missing base tool %q", want)
		}
	}
}

// TestRunSubagentFreshContextAndFinalText 验证：子代理从全新 messages 开始（不含父历史），
// 工具调用走共享 runTool（能读到共享 WORKDIR 的文件），最终返回文本。
func TestRunSubagentFreshContextAndFinalText(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "hello.txt", "hello from subagent file")

	client := &subClient{responses: []*anthropic.Message{
		readToolUseMessage(t),
		textMessage(t, "Found hello in hello.txt."),
	}}
	st := newSubState(client)

	got := runSubagent(st, "read hello.txt and summarize")
	if got != "Found hello in hello.txt." {
		t.Fatalf("got %q, want final text", got)
	}
	if client.calls != 2 {
		t.Fatalf("expected 2 Send calls, got %d", client.calls)
	}

	// 首次 Send：全新 messages（只有 prompt 一条），使用子代理 system prompt，工具集不含 task。
	if len(client.seenMessages[0]) != 1 {
		t.Fatalf("first Send should start with a single fresh message, got %d", len(client.seenMessages[0]))
	}
	if client.seenSystem[0] != subSystemPrompt {
		t.Fatalf("subagent should use subSystemPrompt, got %q", client.seenSystem[0])
	}
	for _, tp := range client.seenTools[0] {
		if tp.OfTool != nil && tp.OfTool.Name == taskToolName {
			t.Fatalf("subagent tools must not include %q", taskToolName)
		}
	}

	// 父历史不应被复制：首次 Send 的第一条消息 role 是 user，内容就是 prompt。
	first := client.seenMessages[0][0]
	if first.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("first message role = %q, want user", first.Role)
	}

	// 第二次 Send 的末条消息应带回 tool_result，且含共享 WORKDIR 中的文件内容。
	last := client.seenMessages[1][len(client.seenMessages[1])-1]
	results := toolResultTexts(t, last)
	if len(results) != 1 {
		t.Fatalf("expected 1 tool_result, got %d", len(results))
	}
	found := false
	for _, c := range results[0].Content {
		if c.OfText != nil && strings.Contains(c.OfText.Text, "hello from subagent file") {
			found = true
		}
	}
	if !found {
		t.Fatalf("subagent tool_result should contain shared-workdir file content")
	}
}

// TestRunSubagentNoToolUseReturnsImmediately 无 tool_use 的首轮即为最终答复。
func TestRunSubagentNoToolUseReturnsImmediately(t *testing.T) {
	client := &subClient{responses: []*anthropic.Message{textMessage(t, "Just an answer.")}}
	st := newSubState(client)

	if got := runSubagent(st, "answer directly"); got != "Just an answer." {
		t.Fatalf("got %q", got)
	}
	if client.calls != 1 {
		t.Fatalf("expected 1 Send call, got %d", client.calls)
	}
}

// TestRunSubagentEmptyTextFallsBack 无文本时返回 "(no summary)"。
func TestRunSubagentEmptyTextFallsBack(t *testing.T) {
	client := &subClient{responses: []*anthropic.Message{textMessage(t, "   ")}}
	st := newSubState(client)

	if got := runSubagent(st, "no summary"); got != "(no summary)" {
		t.Fatalf("got %q, want (no summary)", got)
	}
}

// TestRunSubagentTurnCap 始终返回 tool_use 时，达到 30 轮上限返回固定文本。
func TestRunSubagentTurnCap(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "hello.txt", "hi")

	client := &subClient{responses: []*anthropic.Message{readToolUseMessage(t)}}
	st := newSubState(client)

	got := runSubagent(st, "loop forever")
	if got != "Subagent stopped after 30 turns without a final answer." {
		t.Fatalf("got %q, want the 30-turn stop message", got)
	}
	if client.calls != subagentMaxTurns {
		t.Fatalf("expected %d Send calls, got %d", subagentMaxTurns, client.calls)
	}
}

// TestTaskHandlerArgs 覆盖 task handler 的参数解析与可用性校验。
func TestTaskHandlerArgs(t *testing.T) {
	cases := []struct {
		name string
		st   *sessionState
		args string
		want string
	}{
		{name: "empty prompt", st: newSubState(&subClient{responses: []*anthropic.Message{textMessage(t, "x")}}), args: `{"prompt":""}`, want: "Error: prompt is required"},
		{name: "whitespace prompt", st: newSubState(&subClient{responses: []*anthropic.Message{textMessage(t, "x")}}), args: `{"prompt":"   "}`, want: "Error: prompt is required"},
		{name: "invalid json", st: newSubState(&subClient{responses: []*anthropic.Message{textMessage(t, "x")}}), args: `{bad`, want: "Error: invalid arguments"},
		{name: "no client", st: newSessionState(), args: `{"prompt":"hi"}`, want: "Error: subagent is unavailable outside the agent loop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := taskHandler(tc.st, json.RawMessage(tc.args))
			if !strings.HasPrefix(got, tc.want) {
				t.Fatalf("got %q, want prefix %q", got, tc.want)
			}
		})
	}

	t.Run("valid prompt dispatches through runTool", func(t *testing.T) {
		client := &subClient{responses: []*anthropic.Message{textMessage(t, "delegated result")}}
		st := newSubState(client)
		got := runTool(st, taskToolName, json.RawMessage(`{"prompt":"do it"}`))
		if got != "delegated result" {
			t.Fatalf("got %q", got)
		}
	})
}
