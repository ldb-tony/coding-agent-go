// taskstore.go 实现任务系统：把"执行清单"升级为可恢复的任务图。
//
// 与 todo_write（会话内清单、整体替换）不同，每个任务是一条独立记录，
// 落盘在 .tasks/{id}.json，跨会话可恢复：
//
//	id       task_ 前缀 + 8 位随机十六进制；文件以 O_EXCL 独占创建，撞号即重生成
//	status   pending | in_progress | completed（状态机：claim / complete 两个动作）
//	blockedBy 依赖的任务 ID 列表；全部 completed 后任务才能 claim（文件丢失视为未完成）
//	owner    认领者；claim 时设置，complete 时校验归属
//
// 建图分两阶段：先 create_task 建出全部节点（拿到运行时生成的 ID），再用
// update_task 加边——同一响应里的兄弟工具调用在结果产生前就已定型，无法引用
// 彼此新生成的 ID。update_task 在保存前校验整个变更：目标与依赖必须存在、
// 目标必须仍是 pending 且未被认领、不允许自依赖与环；重复加同一条边是安全的。
//
// 注意：task.go 里的 task 工具是子代理委派，与本文件的 6 个任务工具无关。

package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const tasksDirName = ".tasks"

// taskStatus 是任务的状态枚举。
type taskStatus string

const (
	taskStatusPending    taskStatus = "pending"
	taskStatusInProgress taskStatus = "in_progress"
	taskStatusCompleted  taskStatus = "completed"
)

// defaultTaskOwner 是 claim/complete 未显式给 owner 时的默认认领者。
const defaultTaskOwner = "agent"

// Task 是一条持久化任务记录，与 .tasks/{id}.json 的内容一一对应。
type Task struct {
	ID          string     `json:"id"`
	Subject     string     `json:"subject"`
	Description string     `json:"description"`
	Status      taskStatus `json:"status"`
	Owner       *string    `json:"owner"`      // 未认领时为 null
	BlockedBy   []string   `json:"blockedBy"`  // 依赖的任务 ID；建图分两阶段，创建时为空列表
}

// taskStore 管理 .tasks/ 目录：每条任务一个 JSON 文件，跨会话恢复。
// 全部状态都在文件里，store 自身无内存状态（memoryStore 同款设计）。
type taskStore struct {
	dir string
}

func newTaskStore(dir string) *taskStore {
	return &taskStore{dir: dir}
}

// validateTaskID 校验任务 ID 形如 task_ + 8 位十六进制。ID 直接拼进文件路径，
// 宽松的格式会让 "../x" 之类的输入逃出 .tasks/ 目录。
func validateTaskID(id string) error {
	const prefix, hexLen = "task_", 8
	if len(id) != len(prefix)+hexLen || !strings.HasPrefix(id, prefix) {
		return fmt.Errorf("invalid task id %q (want %s + %d hex chars)", id, prefix, hexLen)
	}
	for _, c := range id[len(prefix):] {
		if !isHexDigit(byte(c)) {
			return fmt.Errorf("invalid task id %q (want %s + %d hex chars)", id, prefix, hexLen)
		}
	}
	return nil
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
}

// newTaskID 生成 task_ + 8 位随机十六进制。
func newTaskID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("task_%x", b), nil
}

// create 校验 subject、分配随机 ID 并写 .tasks/{id}.json。
// 文件以 O_EXCL 独占创建：ID 撞号时丢弃重来，保证不会覆盖已有任务。
func (s *taskStore) create(subject, description string) (*Task, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, errors.New("subject is required")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 8; attempt++ {
		id, err := newTaskID()
		if err != nil {
			return nil, err
		}
		t := &Task{
			ID:          id,
			Subject:     subject,
			Description: description,
			Status:      taskStatusPending,
			BlockedBy:   []string{},
		}
		data, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return nil, err
		}
		path := filepath.Join(s.dir, id+".json")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue // ID 撞号：丢弃重新生成
		}
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		return t, nil
	}
	return nil, errors.New("could not allocate a unique task id")
}

// load 读取并解析一条任务记录；目录里不存在该文件视为任务不存在。
func (s *taskStore) load(id string) (*Task, error) {
	if err := validateTaskID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("task %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("task %s is corrupted: %w", id, err)
	}
	if t.BlockedBy == nil {
		t.BlockedBy = []string{}
	}
	return &t, nil
}

// save 校验 ID 后整体覆写任务文件（走 atomicWriteFile，避免半截状态）。
func (s *taskStore) save(t *Task) error {
	if err := validateTaskID(t.ID); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(s.dir, t.ID+".json"), append(data, '\n'), 0o644)
}

// list 返回全部任务记录，按 ID 排序保证顺序稳定；目录不存在 → nil。
// 读不了或损坏的文件跳过，不阻塞其余任务。
func (s *taskStore) list() []*Task {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var tasks []*Task
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		t, err := s.load(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	return tasks
}

// incompleteDependencies 返回尚未满足的依赖 ID：依赖任务不是 completed，
// 或其文件已不存在（丢失的记录不能当作已完成）。
func (s *taskStore) incompleteDependencies(t *Task) []string {
	var out []string
	for _, dep := range t.BlockedBy {
		d, err := s.load(dep)
		if err != nil || d.Status != taskStatusCompleted {
			out = append(out, dep)
		}
	}
	return out
}

// canStart 报告任务是否可以认领：全部 blockedBy 依赖均已 completed。
func (s *taskStore) canStart(id string) bool {
	t, err := s.load(id)
	if err != nil {
		return false
	}
	return len(s.incompleteDependencies(t)) == 0
}

// dependsOn 递归判断 from 是否（传递地）依赖 target。
// 图中已被拒绝成环，因此递归必然终止。
func (s *taskStore) dependsOn(from, target string) bool {
	t, err := s.load(from)
	if err != nil {
		return false
	}
	for _, dep := range t.BlockedBy {
		if dep == target || s.dependsOn(dep, target) {
			return true
		}
	}
	return false
}

// updateDependencies 给 pending 且未认领的任务追加 blockedBy 依赖边。
// 保存前校验整个变更：目标与依赖必须存在、目标必须仍是 pending 且无 owner、
// 不允许自依赖与成环；重复边（与已有或同批次内重复）安全去重。
func (s *taskStore) updateDependencies(taskID string, addBlockedBy []string) (*Task, error) {
	t, err := s.load(taskID)
	if err != nil {
		return nil, err
	}
	if t.Status != taskStatusPending {
		return nil, fmt.Errorf("task %s is %s, cannot update dependencies", t.ID, t.Status)
	}
	if t.Owner != nil {
		return nil, fmt.Errorf("task %s is owned by %s, cannot update dependencies", t.ID, *t.Owner)
	}
	seen := make(map[string]bool, len(t.BlockedBy))
	for _, dep := range t.BlockedBy {
		seen[dep] = true
	}
	var added []string
	for _, dep := range addBlockedBy {
		if dep == t.ID {
			return nil, errors.New("task cannot depend on itself")
		}
		if seen[dep] {
			continue // 重复边安全跳过，不重复记录
		}
		if _, err := s.load(dep); err != nil {
			return nil, fmt.Errorf("dependency %s: %w", dep, err)
		}
		seen[dep] = true
		added = append(added, dep)
	}
	t.BlockedBy = append(t.BlockedBy, added...)
	for _, dep := range added {
		if s.dependsOn(dep, t.ID) {
			return nil, fmt.Errorf("adding %s would create a dependency cycle through task %s", dep, t.ID)
		}
	}
	if err := s.save(t); err != nil {
		return nil, err
	}
	return t, nil
}

// claim 认领任务：pending → in_progress 并记录 owner。
// 任务不是 pending 或存在未完成依赖时拒绝（返回原因文本，不是错误）。
func (s *taskStore) claim(taskID, owner string) (string, error) {
	if strings.TrimSpace(owner) == "" {
		owner = defaultTaskOwner
	}
	t, err := s.load(taskID)
	if err != nil {
		return "", err
	}
	if t.Status != taskStatusPending {
		return fmt.Sprintf("Task %s is %s, cannot claim", t.ID, t.Status), nil
	}
	if deps := s.incompleteDependencies(t); len(deps) > 0 {
		return fmt.Sprintf("Blocked by: %s", strings.Join(deps, ", ")), nil
	}
	o := owner
	t.Owner = &o
	t.Status = taskStatusInProgress
	if err := s.save(t); err != nil {
		return "", err
	}
	return fmt.Sprintf("Claimed %s (%s)", t.ID, t.Subject), nil
}

// completeTaskOwnerLabel 把可能为 nil 的 owner 渲染成展示文本。
func completeTaskOwnerLabel(owner *string) string {
	if owner == nil {
		return "none"
	}
	return *owner
}

// complete 完成任务：in_progress → completed，并报告因此解锁的下游任务。
// owner 校验归属：只有认领者能完成任务。"刚解锁"以完成前的快照为准——
// 完成前就已可开始的 pending 任务不会重复上报。
func (s *taskStore) complete(taskID, owner string) (string, error) {
	if strings.TrimSpace(owner) == "" {
		owner = defaultTaskOwner
	}
	t, err := s.load(taskID)
	if err != nil {
		return "", err
	}
	if t.Status != taskStatusInProgress {
		return fmt.Sprintf("Task %s is %s, cannot complete", t.ID, t.Status), nil
	}
	if t.Owner == nil || *t.Owner != owner {
		return fmt.Sprintf("Task %s is owned by %s, not %s", t.ID, completeTaskOwnerLabel(t.Owner), owner), nil
	}
	readyBefore := map[string]bool{}
	for _, other := range s.list() {
		if other.Status == taskStatusPending && len(other.BlockedBy) > 0 && s.canStart(other.ID) {
			readyBefore[other.ID] = true
		}
	}
	t.Status = taskStatusCompleted
	if err := s.save(t); err != nil {
		return "", err
	}
	var unblocked []string
	for _, other := range s.list() {
		if other.Status == taskStatusPending && len(other.BlockedBy) > 0 &&
			!readyBefore[other.ID] && s.canStart(other.ID) {
			unblocked = append(unblocked, other.Subject)
		}
	}
	msg := fmt.Sprintf("Completed %s (%s)", t.ID, t.Subject)
	if len(unblocked) > 0 {
		msg += "\nUnblocked: " + strings.Join(unblocked, ", ")
	}
	return msg, nil
}

// taskJSON 渲染任务的完整 JSON（get_task / create_task / update_task 的返回值）。
func taskJSON(t *Task) string {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "Error: " + err.Error()
	}
	return string(data)
}

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
	t, err := st.tasks.create(in.Subject, in.Description)
	if err != nil {
		return "Error: " + err.Error()
	}
	return taskJSON(t)
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
	t, err := st.tasks.updateDependencies(in.TaskID, in.AddBlockedBy)
	if err != nil {
		return "Error: " + err.Error()
	}
	return taskJSON(t)
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
	msg, err := st.tasks.claim(in.TaskID, in.Owner)
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
	msg, err := st.tasks.complete(in.TaskID, in.Owner)
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
	t, err := st.tasks.load(in.TaskID)
	if err != nil {
		return "Error: " + err.Error()
	}
	return taskJSON(t)
}

// listTaskHandler 是 list_tasks 工具的处理函数：每条任务一行摘要，空库给占位文本。
func listTaskHandler(st *sessionState, _ json.RawMessage) string {
	tasks := st.tasks.list()
	if len(tasks) == 0 {
		return "(no tasks)"
	}
	lines := make([]string, 0, len(tasks))
	for _, t := range tasks {
		line := fmt.Sprintf("%s [%s] %s (owner: %s", t.ID, t.Status, t.Subject, completeTaskOwnerLabel(t.Owner))
		if len(t.BlockedBy) > 0 {
			line += ", blocked by: " + strings.Join(t.BlockedBy, ", ")
		}
		lines = append(lines, line+")")
	}
	return strings.Join(lines, "\n")
}
