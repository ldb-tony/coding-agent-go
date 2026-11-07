// taskstore.go 保留 6 个任务工具 handler（依赖 sessionState）。
// 任务图 Store 的全部逻辑已迁移至 pkg/tools/task/store.go。
//
// 注意：task.go 里的 task 工具是子代理委派，与本文件的 6 个任务工具无关。

package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	pkgtask "coding-agent-go/pkg/tools/task"
)

// ============ 工具处理器 ============

// createTaskHandler 是 create_task 工具的处理函数：返回含运行时 ID 的完整任务 JSON。
func createTaskHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		Subject     string `json:"subject"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	t, err := st.Tasks.Create(in.Subject, in.Description)
	if err != nil {
		return "Error: " + err.Error()
	}
	return pkgtask.TaskJSON(t)
}

// updateTaskHandler 是 update_task 工具的处理函数：给任务追加 blockedBy 依赖边。
func updateTaskHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		TaskID       string   `json:"task_id"`
		AddBlockedBy []string `json:"addBlockedBy"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	t, err := st.Tasks.UpdateDependencies(in.TaskID, in.AddBlockedBy)
	if err != nil {
		return "Error: " + err.Error()
	}
	return pkgtask.TaskJSON(t)
}

// claimTaskHandler 是 claim_task 工具的处理函数：pending → in_progress 并设置 owner。
func claimTaskHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		TaskID string `json:"task_id"`
		Owner  string `json:"owner"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	msg, err := st.Tasks.Claim(in.TaskID, in.Owner)
	if err != nil {
		return "Error: " + err.Error()
	}
	return msg
}

// completeTaskHandler 是 complete_task 工具的处理函数：in_progress → completed 并解锁下游。
func completeTaskHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		TaskID string `json:"task_id"`
		Owner  string `json:"owner"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	msg, err := st.Tasks.Complete(in.TaskID, in.Owner)
	if err != nil {
		return "Error: " + err.Error()
	}
	return msg
}

// getTaskHandler 是 get_task 工具的处理函数：返回任务完整 JSON（含 description 与依赖）。
func getTaskHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	t, err := st.Tasks.Load(in.TaskID)
	if err != nil {
		return "Error: " + err.Error()
	}
	return pkgtask.TaskJSON(t)
}

// listTaskHandler 是 list_tasks 工具的处理函数：每条任务一行摘要，空库给占位文本。
func listTaskHandler(st *sessionState, _ json.RawMessage) string {
	tasks := st.Tasks.List()
	if len(tasks) == 0 {
		return "(no tasks)"
	}
	lines := make([]string, 0, len(tasks))
	for _, t := range tasks {
		line := fmt.Sprintf("%s [%s] %s (owner: %s", t.ID, t.Status, t.Subject, pkgtask.OwnerLabel(t.Owner))
		if len(t.BlockedBy) > 0 {
			line += ", blocked by: " + strings.Join(t.BlockedBy, ", ")
		}
		lines = append(lines, line+")")
	}
	return strings.Join(lines, "\n")
}
