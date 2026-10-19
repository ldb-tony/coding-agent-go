package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTodoManagerRender 验证空列表和混合状态的渲染输出。
func TestTodoManagerRender(t *testing.T) {
	tm := newTodoManager()

	t.Run("empty list", func(t *testing.T) {
		if got := tm.render(); got != "(empty todo list)" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("mixed statuses", func(t *testing.T) {
		items := []todoItem{
			{Content: "step one", Status: todoCompleted},
			{Content: "step two", Status: todoInProgress},
			{Content: "step three", Status: todoPending},
		}
		out, err := tm.update(items)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		want := "[x] step one\n[>] step two\n[ ] step three"
		if out != want {
			t.Fatalf("got:\n%s\nwant:\n%s", out, want)
		}
	})

	t.Run("default status is pending", func(t *testing.T) {
		out, err := tm.update([]todoItem{{Content: "just a task"}})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if !strings.HasPrefix(out, "[ ]") {
			t.Fatalf("default status should be pending, got %q", out)
		}
	})
}

// TestTodoManagerValidation 覆盖校验路径：超量、空 content、非法 status、多条 in_progress 归一。
func TestTodoManagerValidation(t *testing.T) {
	tm := newTodoManager()

	t.Run("exceeds max items", func(t *testing.T) {
		items := make([]todoItem, maxTodoItems+1)
		for i := range items {
			items[i] = todoItem{Content: "item"}
		}
		if _, err := tm.update(items); err == nil {
			t.Fatal("expected error for too many items")
		} else if !strings.Contains(err.Error(), "too many") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("empty content is rejected", func(t *testing.T) {
		if _, err := tm.update([]todoItem{{Content: "", Status: todoPending}}); err == nil {
			t.Fatal("expected error for empty content")
		} else if !strings.Contains(err.Error(), "content must not be empty") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("whitespace-only content is rejected", func(t *testing.T) {
		if _, err := tm.update([]todoItem{{Content: "   ", Status: todoPending}}); err == nil {
			t.Fatal("expected error for whitespace-only content")
		}
	})

	t.Run("invalid status is rejected", func(t *testing.T) {
		if _, err := tm.update([]todoItem{{Content: "a", Status: "doing"}}); err == nil {
			t.Fatal("expected error for invalid status")
		} else if !strings.Contains(err.Error(), "invalid status") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("multiple in_progress demotes extras", func(t *testing.T) {
		out, err := tm.update([]todoItem{
			{Content: "first", Status: todoInProgress},
			{Content: "second", Status: todoInProgress},
			{Content: "third", Status: todoInProgress},
		})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if tm.countInProgress() != 1 {
			t.Fatalf("should have exactly 1 in_progress, got %d", tm.countInProgress())
		}
		// 第二条和第三条应被降为 pending
		lines := strings.Split(out, "\n")
		if len(lines) != 3 {
			t.Fatalf("got %d lines", len(lines))
		}
		if !strings.HasPrefix(lines[0], "[>]") {
			t.Fatalf("first should be in_progress: %q", lines[0])
		}
		if !strings.HasPrefix(lines[1], "[ ]") {
			t.Fatalf("second should be demoted to pending: %q", lines[1])
		}
		if !strings.HasPrefix(lines[2], "[ ]") {
			t.Fatalf("third should be demoted to pending: %q", lines[2])
		}
	})
}

// TestTodoManagerReplace 验证 update 是全量替换而非追加。
func TestTodoManagerReplace(t *testing.T) {
	tm := newTodoManager()

	if _, err := tm.update([]todoItem{{Content: "old one", Status: todoPending}}); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if _, err := tm.update([]todoItem{{Content: "new one", Status: todoCompleted}}); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if got := tm.render(); !strings.Contains(got, "new one") || strings.Contains(got, "old one") {
		t.Fatalf("update should replace, got %q", got)
	}
}

// TestTodoWriteHandler 通过 runTool 分发验证 handler 行为。
func TestTodoWriteHandler(t *testing.T) {
	st := newSessionState()

	t.Run("valid update via runTool", func(t *testing.T) {
		got := runTool(st, "todo_write", json.RawMessage(`{
			"todos": [
				{"content":"plan","status":"completed"},
				{"content":"execute","status":"in_progress"}
			]
		}`))
		if strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q", got)
		}
		if !strings.Contains(got, "[x] plan") || !strings.Contains(got, "[>] execute") {
			t.Fatalf("unexpected output: %q", got)
		}
	})

	t.Run("invalid JSON args", func(t *testing.T) {
		got := runTool(st, "todo_write", json.RawMessage(`{bad`))
		if !strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("validation error surfaces to handler", func(t *testing.T) {
		got := runTool(st, "todo_write", json.RawMessage(`{"todos":[{"content":"","status":"pending"}]}`))
		if !strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q, want Error prefix", got)
		}
	})
}

// TestTodoManagerWhitespaceTrimming 验证 content 和 status 两端空白被 trim。
func TestTodoManagerWhitespaceTrimming(t *testing.T) {
	tm := newTodoManager()
	out, err := tm.update([]todoItem{{Content: "  do the thing  ", Status: "  pending  "}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if strings.Contains(out, "  do the thing  ") {
		t.Fatalf("content should be trimmed, got %q", out)
	}
	if !strings.Contains(out, "do the thing") {
		t.Fatalf("content should be present without surrounding spaces: %q", out)
	}
}
