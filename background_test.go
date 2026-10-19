package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// resetBackgroundTasks 换上一个干净的进程级管理器，测试结束后恢复，
// 避免就绪队列里的残留通知泄进其他 loop 级测试。
func resetBackgroundTasks(t *testing.T) {
	t.Helper()
	old := backgroundTasks
	backgroundTasks = newBackgroundManager()
	t.Cleanup(func() { backgroundTasks = old })
}

// waitForTaskDone 轮询直到任务离开 running 状态（completed/failed），超时 5s 报错。
func waitForTaskDone(t *testing.T, m *backgroundManager, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := m.taskStatus(id); s != "running" && s != "" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("background task %s did not finish within 5s (status %q)", id, m.taskStatus(id))
}

func TestShouldRunBackground(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  bool
	}{
		{"bash with explicit true", "bash", `{"command":"ls","run_in_background":true}`, true},
		{"bash with false", "bash", `{"command":"ls","run_in_background":false}`, false},
		{"bash without the flag", "bash", `{"command":"ls"}`, false},
		{"non-bash tool ignores the flag", "read_file", `{"path":"a","run_in_background":true}`, false},
		{"malformed input", "bash", `not-json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRunBackground(tc.tool, json.RawMessage(tc.input)); got != tc.want {
				t.Fatalf("shouldRunBackground(%q, %s) = %v, want %v", tc.tool, tc.input, got, tc.want)
			}
		})
	}
}

func TestBackgroundManagerLifecycle(t *testing.T) {
	m := newBackgroundManager()
	id := m.start("echo hello")
	if !strings.HasPrefix(id, "bg_") {
		t.Fatalf("task id %q should have bg_ prefix", id)
	}
	if s := m.taskStatus(id); s != "running" {
		t.Fatalf("fresh task status = %q, want running", s)
	}

	waitForTaskDone(t, m, id)
	if s := m.taskStatus(id); s != "completed" {
		t.Fatalf("status after completion = %q, want completed", s)
	}

	notes := m.collect()
	if len(notes) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notes))
	}
	n := notes[0]
	for _, want := range []string{"<task_notification>", "</task_notification>", id, "echo hello", "completed", "hello"} {
		if !strings.Contains(n, want) {
			t.Fatalf("notification missing %q:\n%s", want, n)
		}
	}

	// collect 即弹出：第二次收集应为空
	if again := m.collect(); len(again) != 0 {
		t.Fatalf("collect should empty the queue, got %d notifications", len(again))
	}
}

func TestBackgroundManagerFailed(t *testing.T) {
	m := newBackgroundManager()
	id := m.start("exit 3")
	waitForTaskDone(t, m, id)

	if s := m.taskStatus(id); s != "failed" {
		t.Fatalf("status = %q, want failed", s)
	}
	notes := m.collect()
	if len(notes) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notes))
	}
	if n := notes[0]; !strings.Contains(n, "failed") || !strings.Contains(n, "(exit 3)") {
		t.Fatalf("notification should mark failed with exit code:\n%s", n)
	}
}

func TestBackgroundManagerTimeout(t *testing.T) {
	m := newBackgroundManager()
	m.timeout = 500 * time.Millisecond
	id := m.start("sleep 2")
	waitForTaskDone(t, m, id)

	if s := m.taskStatus(id); s != "failed" {
		t.Fatalf("status = %q, want failed", s)
	}
	notes := m.collect()
	if len(notes) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notes))
	}
	if n := notes[0]; !strings.Contains(n, "(timeout)") || !strings.Contains(n, "Timeout") {
		t.Fatalf("notification should mark timeout:\n%s", n)
	}
}

func TestBackgroundManagerStopAll(t *testing.T) {
	m := newBackgroundManager()
	id := m.start("sleep 5")

	m.stopAll() // SIGTERM 整组 → shell 退出 → 任务以 failed 落盘
	waitForTaskDone(t, m, id)
	if s := m.taskStatus(id); s != "failed" {
		t.Fatalf("status after stopAll = %q, want failed", s)
	}
}

func TestStartBackgroundTaskPlaceholder(t *testing.T) {
	resetBackgroundTasks(t)

	out := startBackgroundTask(json.RawMessage(`{"command":"echo hi"}`))
	if !strings.Contains(out, "[Background task bg_0001 started]") {
		t.Fatalf("placeholder should contain the task id, got %q", out)
	}
	if !strings.Contains(out, "<task_notification>") {
		t.Fatalf("placeholder should mention the notification format, got %q", out)
	}
	// 命令输出（"hi" 独占一行）不得出现在占位结果里
	if strings.Contains(out, "\nhi") {
		t.Fatalf("placeholder must not contain the command output, got %q", out)
	}
}

// bgWaitClient 在返回第二次及以后的响应前，先等指定后台任务跑完，
// 保证 loop 级测试里通知的注入时点是确定的（对齐真实时序：任务完成后下一轮收集）。
type bgWaitClient struct {
	inner *mockClient
	id    string
}

func (c *bgWaitClient) Send(ctx context.Context, system string, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam, maxTokens int64) (*anthropic.Message, error) {
	if c.inner.calls >= 1 {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && backgroundTasks.taskStatus(c.id) == "running" {
			time.Sleep(5 * time.Millisecond)
		}
	}
	return c.inner.Send(ctx, system, messages, tools, maxTokens)
}

// TestAgentLoopBackgroundNotification 验证完整链路：
// bg bash 返回占位 tool_result → loop 继续 → 下一轮开头以 <task_notification> 收集结果。
func TestAgentLoopBackgroundNotification(t *testing.T) {
	setupLogPaths(t)
	resetBackgroundTasks(t)

	bgResp := mustMessage(t, `{"id":"msg_bg","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_bg","name":"bash","input":{"command":"echo hi","run_in_background":true}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	syncResp := mustMessage(t, `{"id":"msg_sync","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_sync","name":"bash","input":{"command":"echo two"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)

	inner := &mockClient{responses: []*anthropic.Message{bgResp, syncResp, textResponse(t)}}
	client := &bgWaitClient{inner: inner, id: "bg_0001"}
	initial := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("run echo hi in the background")),
	}

	messages, err := agentLoop(context.Background(), client, initial, "test request")
	if err != nil {
		t.Fatalf("agentLoop: %v", err)
	}
	if inner.calls != 3 {
		t.Fatalf("expected 3 Send calls, got %d", inner.calls)
	}

	// user / assistant(bg) / user(占位) / user(通知) / assistant(sync) / user(tool_result) / assistant(text)
	if len(messages) != 7 {
		t.Fatalf("expected 7 messages, got %d", len(messages))
	}

	// 占位 tool_result：带 bg_id，不含命令输出
	results := toolResultTexts(t, messages[2])
	if len(results) != 1 || results[0].ToolUseID != "toolu_bg" {
		t.Fatalf("expected 1 tool_result for toolu_bg in messages[2], got %v", results)
	}
	var placeholder string
	for _, c := range results[0].Content {
		if c.OfText != nil {
			placeholder += c.OfText.Text
		}
	}
	if !strings.Contains(placeholder, "bg_0001") {
		t.Fatalf("placeholder should contain bg_0001, got %q", placeholder)
	}
	if strings.Contains(placeholder, "\nhi") {
		t.Fatalf("placeholder must not contain command output, got %q", placeholder)
	}

	// 通知消息：独立 user 事件，不复用 tool_use_id。注入发生在每轮开头（Send 之前），
	// 快命令在注入时点是否已完成是竞态，因此按内容扫描而不是固定消息下标。
	var note string
	for _, m := range messages {
		if m.Role != anthropic.MessageParamRoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.OfText != nil {
				note += b.OfText.Text
			}
		}
	}
	for _, want := range []string{"<task_notification>", "echo hi", "completed", "\nhi\n"} {
		if !strings.Contains(note, want) {
			t.Fatalf("notification missing %q:\n%s", want, note)
		}
	}
}
