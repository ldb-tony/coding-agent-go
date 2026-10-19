package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// todoStatus 是待办事项的状态枚举。
type todoStatus string

const (
	todoPending    todoStatus = "pending"
	todoInProgress todoStatus = "in_progress"
	todoCompleted  todoStatus = "completed"
)

const (
	maxTodoItems = 20
)

// todoItem 是单条待办事项。
type todoItem struct {
	Content string     `json:"content"`
	Status  todoStatus `json:"status"`
}

// todoManager 管理会话级的待办列表：校验、渲染、状态跟踪。
// 挂在 sessionState 上，生命周期同 agentLoop。
type todoManager struct {
	items []todoItem
}

func newTodoManager() *todoManager {
	return &todoManager{items: []todoItem{}}
}

// update 用新列表替换当前列表；做校验后落内存并返回渲染结果。
// 校验规则：
//   - 最多 maxTodoItems 条
//   - 每条 content 非空
//   - 最多一条 in_progress（超过则只保留第一条，其余降为 pending）
func (tm *todoManager) update(items []todoItem) (string, error) {
	if len(items) > maxTodoItems {
		return "", fmt.Errorf("too many items (%d), max %d", len(items), maxTodoItems)
	}

	// 归一化 status 并检查空 content
	seenInProgress := false
	validated := make([]todoItem, 0, len(items))
	for i, it := range items {
		content := strings.TrimSpace(it.Content)
		if content == "" {
			return "", fmt.Errorf("item %d: content must not be empty", i)
		}

		status := todoStatus(strings.TrimSpace(string(it.Status)))
		if status == "" {
			status = todoPending
		}
		switch status {
		case todoPending, todoInProgress, todoCompleted:
		default:
			return "", fmt.Errorf("item %d: invalid status %q (must be pending, in_progress, or completed)", i, status)
		}

		if status == todoInProgress {
			if seenInProgress {
				status = todoPending // 只保留第一条 in_progress
			} else {
				seenInProgress = true
			}
		}

		validated = append(validated, todoItem{Content: content, Status: status})
	}

	tm.items = validated
	return tm.render(), nil
}

// render 把当前列表渲染为多行字符串，每行形如 "[ ]  content" / "[>] content" / "[x] content"。
func (tm *todoManager) render() string {
	if len(tm.items) == 0 {
		return "(empty todo list)"
	}
	var b strings.Builder
	for _, it := range tm.items {
		var mark string
		switch it.Status {
		case todoCompleted:
			mark = "[x]"
		case todoInProgress:
			mark = "[>]"
		default:
			mark = "[ ]"
		}
		fmt.Fprintf(&b, "%s %s\n", mark, it.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// countInProgress 返回当前 in_progress 项数（用于断言/调试）。
func (tm *todoManager) countInProgress() int {
	n := 0
	for _, it := range tm.items {
		if it.Status == todoInProgress {
			n++
		}
	}
	return n
}

// todoWriteHandler 是 todo_write 工具的处理函数。
func todoWriteHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		Todos []todoItem `json:"todos"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	output, err := st.todo.update(in.Todos)
	if err != nil {
		return "Error: " + err.Error()
	}
	return output
}
