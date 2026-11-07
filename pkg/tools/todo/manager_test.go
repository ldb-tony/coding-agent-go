package todo

import (
	"strings"
	"testing"
)

// TestManagerRender 验证空列表和混合状态的渲染输出。
func TestManagerRender(t *testing.T) {
	tm := NewManager()

	t.Run("empty list", func(t *testing.T) {
		if got := tm.Render(); got != "(empty todo list)" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("mixed statuses", func(t *testing.T) {
		items := []Item{
			{Content: "step one", Status: TodoCompleted},
			{Content: "step two", Status: TodoInProgress},
			{Content: "step three", Status: TodoPending},
		}
		out, err := tm.Update(items)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		want := "[x] step one\n[>] step two\n[ ] step three"
		if out != want {
			t.Fatalf("got:\n%s\nwant:\n%s", out, want)
		}
	})

	t.Run("default status is pending", func(t *testing.T) {
		out, err := tm.Update([]Item{{Content: "just a task"}})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if !strings.HasPrefix(out, "[ ]") {
			t.Fatalf("default status should be pending, got %q", out)
		}
	})
}

// TestManagerValidation 覆盖校验路径：超量、空 content、非法 status、多条 in_progress 归一。
func TestManagerValidation(t *testing.T) {
	tm := NewManager()

	t.Run("exceeds max items", func(t *testing.T) {
		items := make([]Item, maxTodoItems+1)
		for i := range items {
			items[i] = Item{Content: "item"}
		}
		if _, err := tm.Update(items); err == nil {
			t.Fatal("expected error for too many items")
		} else if !strings.Contains(err.Error(), "too many") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("empty content is rejected", func(t *testing.T) {
		if _, err := tm.Update([]Item{{Content: "", Status: TodoPending}}); err == nil {
			t.Fatal("expected error for empty content")
		} else if !strings.Contains(err.Error(), "content must not be empty") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("whitespace-only content is rejected", func(t *testing.T) {
		if _, err := tm.Update([]Item{{Content: "   ", Status: TodoPending}}); err == nil {
			t.Fatal("expected error for whitespace-only content")
		}
	})

	t.Run("invalid status is rejected", func(t *testing.T) {
		if _, err := tm.Update([]Item{{Content: "a", Status: "doing"}}); err == nil {
			t.Fatal("expected error for invalid status")
		} else if !strings.Contains(err.Error(), "invalid status") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("multiple in_progress demotes extras", func(t *testing.T) {
		out, err := tm.Update([]Item{
			{Content: "first", Status: TodoInProgress},
			{Content: "second", Status: TodoInProgress},
			{Content: "third", Status: TodoInProgress},
		})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if tm.CountInProgress() != 1 {
			t.Fatalf("should have exactly 1 in_progress, got %d", tm.CountInProgress())
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

// TestManagerReplace 验证 update 是全量替换而非追加。
func TestManagerReplace(t *testing.T) {
	tm := NewManager()

	if _, err := tm.Update([]Item{{Content: "old one", Status: TodoPending}}); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if _, err := tm.Update([]Item{{Content: "new one", Status: TodoCompleted}}); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if got := tm.Render(); !strings.Contains(got, "new one") || strings.Contains(got, "old one") {
		t.Fatalf("update should replace, got %q", got)
	}
}

// TestManagerWhitespaceTrimming 验证 content 和 status 两端空白被 trim。
func TestManagerWhitespaceTrimming(t *testing.T) {
	tm := NewManager()
	out, err := tm.Update([]Item{{Content: "  do the thing  ", Status: "  pending  "}})
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
