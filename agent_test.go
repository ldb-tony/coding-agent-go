package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

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
	oldAgent, oldInput := agentLogPath, inputLogPath
	agentLogPath = filepath.Join(dir, "agent.json")
	inputLogPath = filepath.Join(dir, "input.json")
	t.Cleanup(func() {
		agentLogPath = oldAgent
		inputLogPath = oldInput
	})
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

	messages, err := agentLoop(context.Background(), client, initial)
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

	messages, err := agentLoop(context.Background(), client, initial)
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

	messages, err := agentLoop(context.Background(), client, initial)
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
