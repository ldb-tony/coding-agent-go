// store.go 实现任务系统：把"执行清单"升级为可恢复的任务图。
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
package task

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

	"coding-agent-go/pkg/tools/filesystem"
)

// TasksDirName 是任务文件的目录名。
const TasksDirName = ".tasks"

// Status 是任务的状态枚举。
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

// DefaultOwner 是 claim/complete 未显式给 owner 时的默认认领者。
const DefaultOwner = "agent"

// Task 是一条持久化任务记录，与 .tasks/{id}.json 的内容一一对应。
type Task struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Description string   `json:"description"`
	Status      Status   `json:"status"`
	Owner       *string  `json:"owner"`     // 未认领时为 null
	BlockedBy   []string `json:"blockedBy"` // 依赖的任务 ID；建图分两阶段，创建时为空列表
}

// Store 管理 .tasks/ 目录：每条任务一个 JSON 文件，跨会话恢复。
// 全部状态都在文件里，Store 自身无内存状态（memoryStore 同款设计）。
type Store struct {
	dir string
}

// Dir 返回任务目录。
func (s *Store) Dir() string { return s.dir }

func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// ValidateID 校验任务 ID 形如 task_ + 8 位十六进制。ID 直接拼进文件路径，
// 宽松的格式会让 "../x" 之类的输入逃出 .tasks/ 目录。
func ValidateID(id string) error {
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

// newID 生成 task_ + 8 位随机十六进制。
func newID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("task_%x", b), nil
}

// Create 校验 subject、分配随机 ID 并写 .tasks/{id}.json。
// 文件以 O_EXCL 独占创建：ID 撞号时丢弃重来，保证不会覆盖已有任务。
func (s *Store) Create(subject, description string) (*Task, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, errors.New("subject is required")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 8; attempt++ {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		t := &Task{
			ID:          id,
			Subject:     subject,
			Description: description,
			Status:      StatusPending,
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

// Load 读取并解析一条任务记录；目录里不存在该文件视为任务不存在。
func (s *Store) Load(id string) (*Task, error) {
	if err := ValidateID(id); err != nil {
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

// save 校验 ID 后整体覆写任务文件（走原子写，避免半截状态）。
func (s *Store) save(t *Task) error {
	if err := ValidateID(t.ID); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return filesystem.AtomicWriteFile(filepath.Join(s.dir, t.ID+".json"), append(data, '\n'), 0o644)
}

// List 返回全部任务记录，按 ID 排序保证顺序稳定；目录不存在 → nil。
// 读不了或损坏的文件跳过，不阻塞其余任务。
func (s *Store) List() []*Task {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var tasks []*Task
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		t, err := s.Load(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	return tasks
}

// IncompleteDependencies 返回尚未满足的依赖 ID：依赖任务不是 completed，
// 或其文件已不存在（丢失的记录不能当作已完成）。
func (s *Store) IncompleteDependencies(t *Task) []string {
	var out []string
	for _, dep := range t.BlockedBy {
		d, err := s.Load(dep)
		if err != nil || d.Status != StatusCompleted {
			out = append(out, dep)
		}
	}
	return out
}

// CanStart 报告任务是否可以认领：全部 blockedBy 依赖均已 completed。
func (s *Store) CanStart(id string) bool {
	t, err := s.Load(id)
	if err != nil {
		return false
	}
	return len(s.IncompleteDependencies(t)) == 0
}

// dependsOn 递归判断 from 是否（传递地）依赖 target。
// 图中已被拒绝成环，因此递归必然终止。
func (s *Store) dependsOn(from, target string) bool {
	t, err := s.Load(from)
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

// UpdateDependencies 给 pending 且未认领的任务追加 blockedBy 依赖边。
// 保存前校验整个变更：目标与依赖必须存在、目标必须仍是 pending 且无 owner、
// 不允许自依赖与成环；重复边（与已有或同批次内重复）安全去重。
func (s *Store) UpdateDependencies(taskID string, addBlockedBy []string) (*Task, error) {
	t, err := s.Load(taskID)
	if err != nil {
		return nil, err
	}
	if t.Status != StatusPending {
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
		if _, err := s.Load(dep); err != nil {
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

// Claim 认领任务：pending → in_progress 并记录 owner。
// 任务不是 pending 或存在未完成依赖时拒绝（返回原因文本，不是错误）。
func (s *Store) Claim(taskID, owner string) (string, error) {
	if strings.TrimSpace(owner) == "" {
		owner = DefaultOwner
	}
	t, err := s.Load(taskID)
	if err != nil {
		return "", err
	}
	if t.Status != StatusPending {
		return fmt.Sprintf("Task %s is %s, cannot claim", t.ID, t.Status), nil
	}
	if deps := s.IncompleteDependencies(t); len(deps) > 0 {
		return fmt.Sprintf("Blocked by: %s", strings.Join(deps, ", ")), nil
	}
	o := owner
	t.Owner = &o
	t.Status = StatusInProgress
	if err := s.save(t); err != nil {
		return "", err
	}
	return fmt.Sprintf("Claimed %s (%s)", t.ID, t.Subject), nil
}

// OwnerLabel 把可能为 nil 的 owner 渲染成展示文本。
func OwnerLabel(owner *string) string {
	if owner == nil {
		return "none"
	}
	return *owner
}

// Complete 完成任务：in_progress → completed，并报告因此解锁的下游任务。
// owner 校验归属：只有认领者能完成任务。"刚解锁"以完成前的快照为准——
// 完成前就已可开始的 pending 任务不会重复上报。
func (s *Store) Complete(taskID, owner string) (string, error) {
	if strings.TrimSpace(owner) == "" {
		owner = DefaultOwner
	}
	t, err := s.Load(taskID)
	if err != nil {
		return "", err
	}
	if t.Status != StatusInProgress {
		return fmt.Sprintf("Task %s is %s, cannot complete", t.ID, t.Status), nil
	}
	if t.Owner == nil || *t.Owner != owner {
		return fmt.Sprintf("Task %s is owned by %s, not %s", t.ID, OwnerLabel(t.Owner), owner), nil
	}
	readyBefore := map[string]bool{}
	for _, other := range s.List() {
		if other.Status == StatusPending && len(other.BlockedBy) > 0 && s.CanStart(other.ID) {
			readyBefore[other.ID] = true
		}
	}
	t.Status = StatusCompleted
	if err := s.save(t); err != nil {
		return "", err
	}
	var unblocked []string
	for _, other := range s.List() {
		if other.Status == StatusPending && len(other.BlockedBy) > 0 &&
			!readyBefore[other.ID] && s.CanStart(other.ID) {
			unblocked = append(unblocked, other.Subject)
		}
	}
	msg := fmt.Sprintf("Completed %s (%s)", t.ID, t.Subject)
	if len(unblocked) > 0 {
		msg += "\nUnblocked: " + strings.Join(unblocked, ", ")
	}
	return msg, nil
}

// TaskJSON 渲染任务的完整 JSON（get_task / create_task / update_task 的返回值）。
func TaskJSON(t *Task) string {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "Error: " + err.Error()
	}
	return string(data)
}
