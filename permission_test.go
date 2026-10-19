package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// stubReadApproval 替换包级 readApproval 并记录是否被调用，t.Cleanup 恢复原值。
// 涉及包级变量替换，调用方不得使用 t.Parallel()。
func stubReadApproval(t *testing.T, input string) *bool {
	t.Helper()
	called := new(bool)
	old := readApproval
	readApproval = func() string {
		*called = true
		return input
	}
	t.Cleanup(func() { readApproval = old })
	return called
}

func TestCheckDenyList(t *testing.T) {
	// 命中清单的命令：断言 ok=true 且返回对应 pattern
	hit := []struct {
		command string
		pattern string
	}{
		{"rm -rf /", "rm -rf /"},
		{"rm -rf /tmp/x", "rm -rf /"},
		{"sudo ls", "sudo"},
		{"shutdown now", "shutdown"},
		{"reboot", "reboot"},
		{"mkfs.ext4 /dev/sda", "mkfs"},
		{"dd if=/dev/zero of=x", "dd if="},
		{"echo hi > /dev/null", "> /dev/"},
	}
	for _, tc := range hit {
		t.Run("hit: "+tc.command, func(t *testing.T) {
			pattern, ok := checkDenyList(tc.command)
			if !ok {
				t.Fatalf("checkDenyList(%q) ok = false, want true", tc.command)
			}
			if pattern != tc.pattern {
				t.Fatalf("checkDenyList(%q) pattern = %q, want %q", tc.command, pattern, tc.pattern)
			}
		})
	}

	// 未命中清单的安全命令
	for _, command := range []string{"echo hello", "ls -la"} {
		t.Run("miss: "+command, func(t *testing.T) {
			if pattern, ok := checkDenyList(command); ok {
				t.Fatalf("checkDenyList(%q) = (%q, true), want ok=false", command, pattern)
			}
		})
	}
}

func TestContainsDestructiveCommand(t *testing.T) {
	trigger := []string{
		"rm test.txt",
		"del test.txt",
		"DEL test.txt",
		"ls && rm -rf build",
		"chmod 777 x",
		"echo hi > /etc/x",
	}
	for _, command := range trigger {
		t.Run("trigger: "+command, func(t *testing.T) {
			if !containsDestructiveCommand(command) {
				t.Fatalf("containsDestructiveCommand(%q) = false, want true", command)
			}
		})
	}

	// 单词边界与引号场景：含 rm/del 子串但不在命令位置，不应误报
	safe := []string{
		"model",
		"delimiter",
		"echo del test.txt",
		"echo hello",
		"ls -la",
	}
	for _, command := range safe {
		t.Run("safe: "+command, func(t *testing.T) {
			if containsDestructiveCommand(command) {
				t.Fatalf("containsDestructiveCommand(%q) = true, want false", command)
			}
		})
	}
}

func TestCheckRules(t *testing.T) {
	t.Run("bash destructive command matches", func(t *testing.T) {
		reason, matched := checkRules("bash", json.RawMessage(`{"command":"rm test.txt"}`))
		if !matched {
			t.Fatalf("matched = false, want true")
		}
		if reason != "Potentially destructive command" {
			t.Fatalf("reason = %q, want %q", reason, "Potentially destructive command")
		}
	})

	t.Run("bash normal command does not match", func(t *testing.T) {
		if reason, matched := checkRules("bash", json.RawMessage(`{"command":"echo hello"}`)); matched {
			t.Fatalf("matched = true (reason %q), want false", reason)
		}
	})

	t.Run("non-bash tool has no rules", func(t *testing.T) {
		if reason, matched := checkRules("read_file", json.RawMessage(`{"path":"/etc/passwd"}`)); matched {
			t.Fatalf("matched = true (reason %q), want false", reason)
		}
	})
}

func TestAskUser(t *testing.T) {
	approve := []string{"y\n", "yes\n", "Y\n", "YES\n"}
	for _, input := range approve {
		t.Run("approve: "+strings.TrimSpace(input), func(t *testing.T) {
			stubReadApproval(t, input)
			if !askUser("bash", json.RawMessage(`{"command":"rm x"}`), "reason") {
				t.Fatalf("askUser with input %q = false, want true", input)
			}
		})
	}

	deny := []string{"n\n", "\n", "maybe\n"}
	for _, input := range deny {
		name := strings.TrimSpace(input)
		if name == "" {
			name = "empty"
		}
		t.Run("deny: "+name, func(t *testing.T) {
			stubReadApproval(t, input)
			if askUser("bash", json.RawMessage(`{"command":"rm x"}`), "reason") {
				t.Fatalf("askUser with input %q = true, want false", input)
			}
		})
	}
}

func TestCheckPermission(t *testing.T) {
	t.Run("gate 1 short-circuits without asking", func(t *testing.T) {
		called := stubReadApproval(t, "y\n")
		if checkPermission("bash", json.RawMessage(`{"command":"rm -rf /"}`)) {
			t.Fatalf("checkPermission(rm -rf /) = true, want false")
		}
		if *called {
			t.Fatalf("readApproval was called; Gate 1 must not ask")
		}
	})

	t.Run("gate 2 approved", func(t *testing.T) {
		stubReadApproval(t, "y\n")
		if !checkPermission("bash", json.RawMessage(`{"command":"rm test.txt"}`)) {
			t.Fatalf("checkPermission(rm test.txt) with approval = false, want true")
		}
	})

	t.Run("gate 2 denied", func(t *testing.T) {
		stubReadApproval(t, "n\n")
		if checkPermission("bash", json.RawMessage(`{"command":"rm test.txt"}`)) {
			t.Fatalf("checkPermission(rm test.txt) with denial = true, want false")
		}
	})

	t.Run("pass-through bash normal command", func(t *testing.T) {
		called := stubReadApproval(t, "n\n")
		if !checkPermission("bash", json.RawMessage(`{"command":"echo hello"}`)) {
			t.Fatalf("checkPermission(echo hello) = false, want true")
		}
		if *called {
			t.Fatalf("readApproval was called; pass-through must not ask")
		}
	})

	t.Run("pass-through non-bash tool", func(t *testing.T) {
		called := stubReadApproval(t, "n\n")
		if !checkPermission("read_file", json.RawMessage(`{"path":"/tmp/x"}`)) {
			t.Fatalf("checkPermission(read_file) = false, want true")
		}
		if *called {
			t.Fatalf("readApproval was called; pass-through must not ask")
		}
	})
}

func TestAgentLoopPermissionDenied(t *testing.T) {
	setupLogPaths(t)
	called := stubReadApproval(t, "y\n")

	// 第一轮：一个正常命令 + 一个 Gate 1 硬拒命令；第二轮纯文本结束
	toolUseResp := mustMessage(t, `{"id":"msg_perm","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_a","name":"bash","input":{"command":"echo one"}},{"type":"tool_use","id":"toolu_b","name":"bash","input":{"command":"rm -rf /"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	client := &mockClient{responses: []*anthropic.Message{toolUseResp, textResponse(t)}}
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

	results := toolResultTexts(t, messages[2])
	if len(results) != 2 {
		t.Fatalf("expected 2 tool_result blocks in messages[2], got %d", len(results))
	}

	// toolu_a 正常执行，输出含 "one"
	foundOne := false
	for _, c := range results[0].Content {
		if c.OfText != nil && strings.Contains(c.OfText.Text, "one") {
			foundOne = true
		}
	}
	if results[0].ToolUseID != "toolu_a" || !foundOne {
		t.Fatalf("toolu_a result: id=%q, expected content to contain %q", results[0].ToolUseID, "one")
	}

	// toolu_b 被 Gate 1 拒绝：content 为 "Permission denied." 且 IsError=true
	if results[1].ToolUseID != "toolu_b" {
		t.Fatalf("expected tool_use_id toolu_b, got %q", results[1].ToolUseID)
	}
	if len(results[1].Content) != 1 || results[1].Content[0].OfText == nil ||
		results[1].Content[0].OfText.Text != "Permission denied." {
		t.Fatalf("toolu_b content = %+v, want exactly %q", results[1].Content, "Permission denied.")
	}
	if !results[1].IsError.Value {
		t.Fatalf("toolu_b IsError = false, want true")
	}

	// Gate 1 硬拒不询问用户
	if *called {
		t.Fatalf("readApproval was called; Gate 1 must not ask")
	}
}

func TestAgentLoopGate2Ask(t *testing.T) {
	// Gate 2 命中的命令：用户放行则实际执行，拒绝则回喂 Permission denied.
	newClient := func(t *testing.T) *mockClient {
		toolUseResp := mustMessage(t, `{"id":"msg_ask","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_rm","name":"bash","input":{"command":"rm -f nonexistent_s03_test.txt"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
		return &mockClient{responses: []*anthropic.Message{toolUseResp, textResponse(t)}}
	}

	t.Run("user approves and command runs", func(t *testing.T) {
		setupLogPaths(t)
		stubReadApproval(t, "y\n")
		client := newClient(t)
		initial := []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("remove file")),
		}

		messages, err := agentLoop(context.Background(), client, initial)
		if err != nil {
			t.Fatalf("agentLoop: %v", err)
		}
		results := toolResultTexts(t, messages[2])
		if len(results) != 1 {
			t.Fatalf("expected 1 tool_result, got %d", len(results))
		}
		for _, c := range results[0].Content {
			if c.OfText != nil && c.OfText.Text == "Permission denied." {
				t.Fatalf("approved command got Permission denied.; want actual execution")
			}
		}
	})

	t.Run("user denies and command is blocked", func(t *testing.T) {
		setupLogPaths(t)
		stubReadApproval(t, "n\n")
		client := newClient(t)
		initial := []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("remove file")),
		}

		messages, err := agentLoop(context.Background(), client, initial)
		if err != nil {
			t.Fatalf("agentLoop: %v", err)
		}
		results := toolResultTexts(t, messages[2])
		if len(results) != 1 {
			t.Fatalf("expected 1 tool_result, got %d", len(results))
		}
		if len(results[0].Content) != 1 || results[0].Content[0].OfText == nil ||
			results[0].Content[0].OfText.Text != "Permission denied." {
			t.Fatalf("content = %+v, want exactly %q", results[0].Content, "Permission denied.")
		}
		if !results[0].IsError.Value {
			t.Fatalf("IsError = false, want true")
		}
	})
}
