package bash

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// resetBackgroundTasks 换上一个干净的进程级管理器，测试结束后恢复。
func resetBackgroundTasks(t *testing.T) {
	t.Helper()
	old := BackgroundTasks
	BackgroundTasks = NewBackgroundManager()
	t.Cleanup(func() { BackgroundTasks = old })
}

func TestShouldRunBackground(t *testing.T) {
	inputs := []struct {
		toolName string
		input    string
		want     bool
	}{
		{"bash", `{"command":"echo hi","run_in_background":true}`, true},
		{"bash", `{"command":"echo hi"}`, false},
		{"read_file", `{"path":"/tmp/x","run_in_background":true}`, false},
		{"bash", `{}`, false},
	}
	for _, tc := range inputs {
		t.Run(tc.toolName+"/"+tc.input, func(t *testing.T) {
			got := ShouldRunBackground(tc.toolName, json.RawMessage(tc.input))
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBackgroundManagerLifecycle(t *testing.T) {
	resetBackgroundTasks(t)
	m := NewBackgroundManager()

	id := m.start("echo hello world")
	if id == "" {
		t.Fatal("expected task id")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && m.taskStatus(id) == "running" {
		time.Sleep(5 * time.Millisecond)
	}

	if m.taskStatus(id) != "completed" {
		t.Fatalf("status after completion = %q, want completed", m.taskStatus(id))
	}

	// collect 应取出结果并清除
	results := m.collect()
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if !strings.Contains(results[0], "hello world") {
		t.Fatalf("result should contain output, got %q", results[0])
	}

	// collect 第二次应返回空
	if remaining := m.collect(); len(remaining) != 0 {
		t.Fatalf("got %d remaining results after first collect, want 0", len(remaining))
	}
}

func TestBackgroundManagerFailed(t *testing.T) {
	resetBackgroundTasks(t)
	m := NewBackgroundManager()

	id := m.start("exit 42")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && m.taskStatus(id) == "running" {
		time.Sleep(5 * time.Millisecond)
	}

	if m.taskStatus(id) != "failed" {
		t.Fatalf("status of exit 42 task = %q, want failed", m.taskStatus(id))
	}

	results := m.collect()
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestBackgroundManagerTimeout(t *testing.T) {
	resetBackgroundTasks(t)
	m := NewBackgroundManager()

	id := m.start("sleep 30")
	time.Sleep(100 * time.Millisecond)

	if m.taskStatus(id) != "running" {
		t.Fatalf("status immediately after start = %q, want running", m.taskStatus(id))
	}
}

func TestBackgroundManagerStopAll(t *testing.T) {
	resetBackgroundTasks(t)
	m := NewBackgroundManager()

	id := m.start("sleep 30")
	time.Sleep(50 * time.Millisecond)

	m.StopAll()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && m.taskStatus(id) == "running" {
		time.Sleep(10 * time.Millisecond)
	}

	if s := m.taskStatus(id); s != "failed" {
		t.Fatalf("status after StopAll = %q, want failed", s)
	}
}

func TestStartBackgroundTaskPlaceholder(t *testing.T) {
	resetBackgroundTasks(t)

	out := StartBackgroundTask(json.RawMessage(`{"command":"echo hi"}`))
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

func TestInjectBackgroundResults(t *testing.T) {
	resetBackgroundTasks(t)

	// 启动一个后台任务并等待它完成（不 collect）
	BackgroundTasks.start("echo hello-from-bg")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if BackgroundTasks.taskStatus("bg_0001") == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // 让结果进就绪队列

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("do work")),
	}
	enriched := InjectBackgroundResults(messages)

	// 结果应作为独立的 user message（tool_result 形式）注入
	if len(enriched) <= len(messages) {
		t.Fatalf("expected additional messages injected, got %d (original %d)", len(enriched), len(messages))
	}

	var found bool
	for _, m := range enriched {
		if m.Role != anthropic.MessageParamRoleUser {
			continue
		}
		for _, block := range m.Content {
			if block.OfText != nil && strings.Contains(block.OfText.Text, "hello-from-bg") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("expected injected message with bg output content, got messages: %+v", enriched)
	}
}
