package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// resetHooks 把注册表替换为一套空的事件列表，并在测试结束后恢复原值，
// 用于隔离各测试对包级 hooks 全局状态的改动。涉及包级变量替换，调用方不得使用 t.Parallel()。
func resetHooks(t *testing.T) {
	t.Helper()
	old := hooks
	hooks = map[HookEvent][]hookFunc{
		EventUserPromptSubmit: {},
		EventPreToolUse:       {},
		EventPostToolUse:      {},
		EventStop:             {},
	}
	t.Cleanup(func() { hooks = old })
}

func TestRegisterBuiltinHooks(t *testing.T) {
	resetHooks(t)
	registerBuiltinHooks()

	want := map[HookEvent]int{
		EventUserPromptSubmit: 1, // contextInjectHook
		EventPreToolUse:       2, // permissionHook + logHook
		EventPostToolUse:      1, // largeOutputHook
		EventStop:             1, // summaryHook
	}
	for event, n := range want {
		if got := len(hooks[event]); got != n {
			t.Fatalf("hooks[%s] len = %d, want %d", event, got, n)
		}
	}
}

func TestTriggerHooksEmpty(t *testing.T) {
	resetHooks(t)
	if got := triggerHooks(EventPreToolUse, &hookContext{}); got != nil {
		t.Fatalf("triggerHooks on empty registry = %q, want nil", *got)
	}
}

func TestTriggerHooksOrderAndShortCircuit(t *testing.T) {
	resetHooks(t)

	var order []string
	registerHook(EventPreToolUse, func(*hookContext) *string {
		order = append(order, "a")
		return nil
	})
	registerHook(EventPreToolUse, func(*hookContext) *string {
		order = append(order, "b")
		stop := "stop"
		return &stop
	})
	registerHook(EventPreToolUse, func(*hookContext) *string {
		order = append(order, "c")
		return nil
	})

	got := triggerHooks(EventPreToolUse, &hookContext{})
	if got == nil || *got != "stop" {
		t.Fatalf("triggerHooks = %v, want \"stop\"", got)
	}
	if joined := strings.Join(order, ","); joined != "a,b" {
		t.Fatalf("hook order = %q, want \"a,b\" (third hook must be skipped after short-circuit)", joined)
	}
}

func TestRegisterHookAppendsPerEvent(t *testing.T) {
	resetHooks(t)

	calls := 0
	registerHook(EventStop, func(*hookContext) *string { calls++; return nil })
	registerHook(EventStop, func(*hookContext) *string { calls++; return nil })
	registerHook(EventPostToolUse, func(*hookContext) *string {
		t.Fatal("PostToolUse hook fired while triggering Stop")
		return nil
	})

	triggerHooks(EventStop, &hookContext{})
	if calls != 2 {
		t.Fatalf("Stop hooks called %d times, want 2", calls)
	}
}

func TestPermissionHook(t *testing.T) {
	t.Run("deny list returns denial", func(t *testing.T) {
		called := stubReadApproval(t, "y\n")
		got := permissionHook(&hookContext{ToolName: "bash", Input: json.RawMessage(`{"command":"rm -rf /"}`)})
		if got == nil || *got != "Permission denied." {
			t.Fatalf("permissionHook(rm -rf /) = %v, want \"Permission denied.\"", got)
		}
		if *called {
			t.Fatalf("readApproval was called; deny list must not ask")
		}
	})

	t.Run("allowed command returns nil", func(t *testing.T) {
		called := stubReadApproval(t, "n\n")
		if got := permissionHook(&hookContext{ToolName: "bash", Input: json.RawMessage(`{"command":"echo hi"}`)}); got != nil {
			t.Fatalf("permissionHook(echo hi) = %q, want nil", *got)
		}
		if *called {
			t.Fatalf("readApproval was called; safe command must not ask")
		}
	})

	t.Run("destructive command follows user choice", func(t *testing.T) {
		stubReadApproval(t, "n\n")
		if got := permissionHook(&hookContext{ToolName: "bash", Input: json.RawMessage(`{"command":"rm test.txt"}`)}); got == nil {
			t.Fatalf("permissionHook with denial = nil, want non-nil block")
		}
	})
}

func TestContextInjectHookReturnsNil(t *testing.T) {
	if got := contextInjectHook(&hookContext{Query: "hi"}); got != nil {
		t.Fatalf("contextInjectHook = %q, want nil (must not affect control flow)", *got)
	}
}

func TestLargeOutputHookReturnsNil(t *testing.T) {
	small := strings.Repeat("a", 10)
	large := strings.Repeat("a", largeOutputRunes+1)
	for _, out := range []string{small, large} {
		if got := largeOutputHook(&hookContext{ToolName: "bash", Output: out}); got != nil {
			t.Fatalf("largeOutputHook(len=%d) = %q, want nil", len(out), *got)
		}
	}
}

func TestCountToolResults(t *testing.T) {
	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("hi")),
		anthropic.NewUserMessage(
			anthropic.NewToolResultBlock("toolu_a", "out a", false),
			anthropic.NewToolResultBlock("toolu_b", "out b", false),
		),
		anthropic.NewUserMessage(anthropic.NewToolResultBlock("toolu_c", "out c", true)),
	}
	if got := countToolResults(messages); got != 3 {
		t.Fatalf("countToolResults = %d, want 3", got)
	}
	if got := countToolResults(nil); got != 0 {
		t.Fatalf("countToolResults(nil) = %d, want 0", got)
	}
}

func TestSummaryHookReturnsNil(t *testing.T) {
	if got := summaryHook(&hookContext{}); got != nil {
		t.Fatalf("summaryHook = %q, want nil (must allow loop to stop)", *got)
	}
}

func TestPreviewArgs(t *testing.T) {
	short := json.RawMessage(`{"path":"a.txt"}`)
	if got := previewArgs(short); got != `{"path":"a.txt"}` {
		t.Fatalf("previewArgs(short) = %q", got)
	}
	long := json.RawMessage(`{"content":"` + strings.Repeat("x", 100) + `"}`)
	got := previewArgs(long)
	if len([]rune(got)) != 63 { // 60 runes + "..."
		t.Fatalf("previewArgs(long) len = %d, want 63", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("previewArgs(long) = %q, want ... suffix", got)
	}
}

// 以下为 loop 级集成测试：注册表在 resetHooks 下被清空，因此这里只跑安全的 echo 命令，
// 避免在没有权限 hook 的情况下执行破坏性命令。
func TestAgentLoopPreToolUseBlocksTool(t *testing.T) {
	setupLogPaths(t)
	resetHooks(t)

	blocked := "blocked by test hook"
	registerHook(EventPreToolUse, func(hc *hookContext) *string {
		if hc.ToolName == "bash" {
			return &blocked
		}
		return nil
	})

	toolUseResp := mustMessage(t, `{"id":"msg_block","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"echo hi"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	client := &mockClient{responses: []*anthropic.Message{toolUseResp, textResponse(t)}}
	initial := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("run echo"))}

	messages, err := agentLoop(context.Background(), client, initial, "test request")
	if err != nil {
		t.Fatalf("agentLoop: %v", err)
	}
	results := toolResultTexts(t, messages[2])
	if len(results) != 1 {
		t.Fatalf("expected 1 tool_result, got %d", len(results))
	}
	if len(results[0].Content) != 1 || results[0].Content[0].OfText == nil ||
		results[0].Content[0].OfText.Text != blocked {
		t.Fatalf("content = %+v, want exactly %q", results[0].Content, blocked)
	}
	if !results[0].IsError.Value {
		t.Fatalf("IsError = false, want true for hook-blocked call")
	}
}

func TestAgentLoopPostToolUseSeesOutput(t *testing.T) {
	setupLogPaths(t)
	resetHooks(t)

	var seen string
	registerHook(EventPostToolUse, func(hc *hookContext) *string {
		seen = hc.Output
		return nil
	})

	toolUseResp := mustMessage(t, `{"id":"msg_post","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"echo hi"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	client := &mockClient{responses: []*anthropic.Message{toolUseResp, textResponse(t)}}
	initial := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("run echo"))}

	if _, err := agentLoop(context.Background(), client, initial, "test request"); err != nil {
		t.Fatalf("agentLoop: %v", err)
	}
	if !strings.Contains(seen, "hi") {
		t.Fatalf("PostToolUse saw output %q, want it to contain %q", seen, "hi")
	}
}

func TestAgentLoopStopHookForcesContinuation(t *testing.T) {
	setupLogPaths(t)
	resetHooks(t)

	fired := 0
	registerHook(EventStop, func(*hookContext) *string {
		fired++
		if fired == 1 {
			msg := "keep going"
			return &msg
		}
		return nil
	})

	client := &mockClient{responses: []*anthropic.Message{textResponse(t), textResponse(t)}}
	initial := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hello"))}

	messages, err := agentLoop(context.Background(), client, initial, "test request")
	if err != nil {
		t.Fatalf("agentLoop: %v", err)
	}
	if client.calls != 2 {
		t.Fatalf("expected 2 Send calls (one per stop trigger), got %d", client.calls)
	}
	if fired != 2 {
		t.Fatalf("Stop hook fired %d times, want 2", fired)
	}
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages after injected continuation, got %d", len(messages))
	}
}
