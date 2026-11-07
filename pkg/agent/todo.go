// todo.go — handler 留在主包（依赖 sessionState）。
// TodoManager / TodoItem / TodoStatus 纯逻辑已迁移至 pkg/tools/todo/manager.go。
package agent

import (
	"encoding/json"

	"coding-agent-go/pkg/tools/todo"
)

// re-export for sessionState struct field compatibility
type (
	todoItem    = todo.Item
	todoManager = todo.Manager
	todoStatus  = todo.Status
)

const (
	todoPending    = todo.TodoPending
	todoInProgress = todo.TodoInProgress
	todoCompleted  = todo.TodoCompleted
	maxTodoItems   = 20
)

func newTodoManager() *todoManager { return todo.NewManager() }

// todoWriteHandler 是 todo_write 工具的处理函数。
func todoWriteHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		Todos []todo.Item `json:"todos"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	output, err := st.Todo.Update(in.Todos)
	if err != nil {
		return "Error: " + err.Error()
	}
	return output
}
