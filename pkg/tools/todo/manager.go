// Package todo 提供会话级待办列表管理。
package todo

import (
	"fmt"
	"strings"
)

// Status 是待办事项的状态枚举。
type Status string

const (
	TodoPending    Status = "pending"
	TodoInProgress Status = "in_progress"
	TodoCompleted  Status = "completed"
)

const (
	maxTodoItems = 20
)

// Item 是单条待办事项。
type Item struct {
	Content string `json:"content"`
	Status  Status `json:"status"`
}

// Manager 管理会话级的待办列表：校验、渲染、状态跟踪。
// 挂在 sessionState 上，生命周期同 agentLoop。
type Manager struct {
	items []Item
}

// NewManager 创建空的 todoManager。
func NewManager() *Manager {
	return &Manager{items: []Item{}}
}

// Update 用新列表替换当前列表；做校验后落内存并返回渲染结果。
// 校验规则：
//   - 最多 maxTodoItems 条
//   - 每条 content 非空
//   - 最多一条 in_progress（超过则只保留第一条，其余降为 pending）
func (tm *Manager) Update(items []Item) (string, error) {
	if len(items) > maxTodoItems {
		return "", fmt.Errorf("too many items (%d), max %d", len(items), maxTodoItems)
	}

	// 归一化 status 并检查空 content
	seenInProgress := false
	validated := make([]Item, 0, len(items))
	for i, it := range items {
		content := strings.TrimSpace(it.Content)
		if content == "" {
			return "", fmt.Errorf("item %d: content must not be empty", i)
		}

		status := Status(strings.TrimSpace(string(it.Status)))
		if status == "" {
			status = TodoPending
		}
		switch status {
		case TodoPending, TodoInProgress, TodoCompleted:
		default:
			return "", fmt.Errorf("item %d: invalid status %q (must be pending, in_progress, or completed)", i, status)
		}

		if status == TodoInProgress {
			if seenInProgress {
				status = TodoPending // 只保留第一条 in_progress
			} else {
				seenInProgress = true
			}
		}

		validated = append(validated, Item{Content: content, Status: status})
	}

	tm.items = validated
	return tm.Render(), nil
}

// Render 把当前列表渲染为多行字符串，每行形如 "[ ]  content" / "[>] content" / "[x] content"。
func (tm *Manager) Render() string {
	if len(tm.items) == 0 {
		return "(empty todo list)"
	}
	var b strings.Builder
	for _, it := range tm.items {
		var mark string
		switch it.Status {
		case TodoCompleted:
			mark = "[x]"
		case TodoInProgress:
			mark = "[>]"
		default:
			mark = "[ ]"
		}
		fmt.Fprintf(&b, "%s %s\n", mark, it.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// CountInProgress 返回当前 in_progress 项数（用于断言/调试）。
func (tm *Manager) CountInProgress() int {
	n := 0
	for _, it := range tm.items {
		if it.Status == TodoInProgress {
			n++
		}
	}
	return n
}
