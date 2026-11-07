package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"coding-agent-go/pkg/session"
)

// TestTodoWriteHandler 通过 runTool 分发验证 handler 行为。
func TestTodoWriteHandler(t *testing.T) {
	st := session.NewState()

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
