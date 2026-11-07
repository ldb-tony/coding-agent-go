package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coding-agent-go/pkg/session"
	pkgtask "coding-agent-go/pkg/tools/task"
)

// newTestTaskStore 在临时 workdir 下构造 store 与会话状态，返回 (dir, store, st)。
func newTestTaskStore(t *testing.T) (string, *pkgtask.Store, *sessionState) {
	t.Helper()
	dir := setupWorkdir(t)
	store := pkgtask.NewStore(filepath.Join(dir, pkgtask.TasksDirName))
	st := session.NewState()
	if st.Tasks == nil || st.Tasks.Dir() != store.Dir() {
		t.Fatalf("sessionState should carry a taskStore rooted at %s", store.Dir())
	}
	return dir, store, st
}

// taskFileExists 检查任务文件是否落盘。
func taskFileExists(t *testing.T, dir, id string) bool {
	t.Helper()
	if err := pkgtask.ValidateID(id); err != nil {
		t.Fatalf("checking file for invalid id %q: %v", id, err)
	}
	_, err := filepath.Abs(filepath.Join(dir, pkgtask.TasksDirName, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	store := pkgtask.NewStore(filepath.Join(dir, pkgtask.TasksDirName))
	_, err = store.Load(id)
	return err == nil
}

// TestTaskIDFormatAndUniqueness 连续创建的任务 ID 均为 task_ + 8 位十六进制且互不重复。
func TestTaskIDFormatAndUniqueness(t *testing.T) {
	_, store, _ := newTestTaskStore(t)

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		task, err := store.Create("task", "")
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if err := pkgtask.ValidateID(task.ID); err != nil {
			t.Fatalf("created id %q: %v", task.ID, err)
		}
		if seen[task.ID] {
			t.Fatalf("duplicate id %q", task.ID)
		}
		seen[task.ID] = true
	}
}

// TestValidateTaskID 非法 ID（含路径逃逸尝试）一律拒绝。
func TestValidateTaskID(t *testing.T) {
	cases := []struct {
		id    string
		valid bool
	}{
		{"task_0123abcd", true},
		{"task_89abcdef", true},
		{"", false},
		{"task_", false},
		{"task_0123abcde", false}, // 过长
		{"task_0123abc", false},   // 过短
		{"task_0123ABCD", false},  // 大写
		{"task_0123abcg", false},  // 非十六进制
		{"tsak_0123abcd", false},  // 前缀错
		{"task_0123abcd.json", false},
		{"../escape", false},
		{"task_../../etc", false},
	}
	for _, tc := range cases {
		err := pkgtask.ValidateID(tc.id)
		if tc.valid && err != nil {
			t.Errorf("pkgtask.ValidateID(%q) = %v, want valid", tc.id, err)
		}
		if !tc.valid && err == nil {
			t.Errorf("pkgtask.ValidateID(%q) accepted, want rejected", tc.id)
		}
	}
}

// TestCreatePersistsAndStartsPending 创建即落盘：pending、无 owner（null）、空 blockedBy；
// 新的 store 实例（模拟新会话）能从同一目录恢复该任务。
func TestCreatePersistsAndStartsPending(t *testing.T) {
	dir, store, _ := newTestTaskStore(t)

	task, err := store.Create("setup database schema", "create users table")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.Status != pkgtask.StatusPending {
		t.Fatalf("status = %q, want pending", task.Status)
	}
	if task.Owner != nil {
		t.Fatalf("owner = %v, want null", task.Owner)
	}
	if len(task.BlockedBy) != 0 {
		t.Fatalf("blockedBy = %v, want empty", task.BlockedBy)
	}
	if !taskFileExists(t, dir, task.ID) {
		t.Fatalf("task file for %s missing under %s", task.ID, pkgtask.TasksDirName)
	}

	// 跨会话恢复：全新 store 实例读同一目录。
	recovered := pkgtask.NewStore(filepath.Join(dir, pkgtask.TasksDirName))
	got, err := recovered.Load(task.ID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got.Subject != "setup database schema" || got.Description != "create users table" {
		t.Fatalf("recovered %+v", got)
	}
	if got.BlockedBy == nil || len(got.BlockedBy) != 0 {
		t.Fatalf("recovered blockedBy = %v, want empty non-nil", got.BlockedBy)
	}
}

// TestCreateRequiresSubject subject 缺失或纯空白时拒绝创建。
func TestCreateRequiresSubject(t *testing.T) {
	_, store, _ := newTestTaskStore(t)
	for _, subject := range []string{"", "   "} {
		if _, err := store.Create(subject, ""); err == nil {
			t.Errorf("create(%q) accepted, want error", subject)
		}
	}
}

// TestTaskStoreLoadErrors 不存在的任务、非法 ID、损坏文件都要报错。
func TestTaskStoreLoadErrors(t *testing.T) {
	dir, store, _ := newTestTaskStore(t)

	if _, err := store.Load("task_00000000"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("load missing = %v, want not found", err)
	}
	if _, err := store.Load("../escape"); err == nil {
		t.Fatal("load invalid id accepted")
	}

	task, err := store.Create("corrupt me", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pkgtask.TasksDirName, task.ID+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(task.ID); err == nil || !strings.Contains(err.Error(), "corrupted") {
		t.Fatalf("load corrupted = %v, want corrupted error", err)
	}
}

// TestUpdateDependenciesBuildsGraph 两阶段建图：先建节点，再加边；
// 重复边不重复记录；空 addBlockedBy 是无害的 no-op。
func TestUpdateDependenciesBuildsGraph(t *testing.T) {
	_, store, _ := newTestTaskStore(t)

	schema, _ := store.Create("setup database schema", "")
	endpoints, _ := store.Create("create API endpoints", "")
	tests, _ := store.Create("write tests", "")

	if _, err := store.UpdateDependencies(endpoints.ID, []string{schema.ID}); err != nil {
		t.Fatalf("update endpoints: %v", err)
	}
	if _, err := store.UpdateDependencies(tests.ID, []string{endpoints.ID}); err != nil {
		t.Fatalf("update tests: %v", err)
	}
	// 重复加同一条边：安全，不重复记录。
	if _, err := store.UpdateDependencies(endpoints.ID, []string{schema.ID, schema.ID}); err != nil {
		t.Fatalf("repeat edge: %v", err)
	}
	// 空 addBlockedBy：no-op 返回任务本身。
	same, err := store.UpdateDependencies(endpoints.ID, nil)
	if err != nil {
		t.Fatalf("no-op update: %v", err)
	}

	got, err := store.Load(endpoints.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.BlockedBy) != 1 || got.BlockedBy[0] != schema.ID {
		t.Fatalf("endpoints blockedBy = %v, want [%s] exactly once", got.BlockedBy, schema.ID)
	}
	if len(same.BlockedBy) != 1 {
		t.Fatalf("no-op update changed blockedBy to %v", same.BlockedBy)
	}
	// list 能看到边。
	found := false
	for _, tt := range store.List() {
		if tt.ID == tests.ID && len(tt.BlockedBy) == 1 && tt.BlockedBy[0] == endpoints.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("list() should show tests blockedBy endpoints")
	}
}

// TestUpdateDependenciesValidation 目标/依赖必须存在，目标必须 pending 且未认领，
// 自依赖与成环被拒绝，且被拒绝的更新不落盘。
func TestUpdateDependenciesValidation(t *testing.T) {
	_, store, _ := newTestTaskStore(t)

	if _, err := store.UpdateDependencies("task_00000000", nil); err == nil {
		t.Fatal("unknown target accepted")
	}

	a, _ := store.Create("a", "")
	b, _ := store.Create("b", "")

	if _, err := store.UpdateDependencies(a.ID, []string{"task_00000000"}); err == nil {
		t.Fatal("unknown dependency accepted")
	}
	if _, err := store.UpdateDependencies(a.ID, []string{a.ID}); err == nil {
		t.Fatal("self-dependency accepted")
	}

	// a → b 之后，给 b 加 a 成环：拒绝，且 b 的文件保持原样。
	if _, err := store.UpdateDependencies(a.ID, []string{b.ID}); err != nil {
		t.Fatalf("update a: %v", err)
	}
	if _, err := store.UpdateDependencies(b.ID, []string{a.ID}); err == nil {
		t.Fatal("cycle accepted")
	}
	bNow, err := store.Load(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bNow.BlockedBy) != 0 {
		t.Fatalf("rejected update was persisted: blockedBy = %v", bNow.BlockedBy)
	}

	// 已认领 / 已完成的任务不能再改边（用无依赖的任务，避免 claim 被依赖挡住）。
	c, _ := store.Create("c", "")
	if _, err := store.Claim(c.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateDependencies(c.ID, []string{b.ID}); err == nil {
		t.Fatal("update on claimed task accepted")
	}
	if _, err := store.Complete(c.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateDependencies(c.ID, []string{b.ID}); err == nil {
		t.Fatal("update on completed task accepted")
	}
}

// TestCanStartAndIncompleteDependencies 依赖全部 completed 才能开始；
// 依赖文件丢失视为未完成（不能当作已完成放行）。
func TestCanStartAndIncompleteDependencies(t *testing.T) {
	dir, store, _ := newTestTaskStore(t)

	free, _ := store.Create("no dependencies", "")
	if !store.CanStart(free.ID) {
		t.Fatal("task without dependencies should be startable")
	}

	dep, _ := store.Create("dependency", "")
	blocked, _ := store.Create("blocked", "")
	if _, err := store.UpdateDependencies(blocked.ID, []string{dep.ID}); err != nil {
		t.Fatal(err)
	}
	if store.CanStart(blocked.ID) {
		t.Fatal("blocked task should not be startable")
	}
	bt, _ := store.Load(blocked.ID)
	if got := store.IncompleteDependencies(bt); len(got) != 1 || got[0] != dep.ID {
		t.Fatalf("incompleteDependencies = %v, want [%s]", got, dep.ID)
	}

	// 依赖完成 → 解锁。
	if _, err := store.Claim(dep.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(dep.ID, ""); err != nil {
		t.Fatal(err)
	}
	if !store.CanStart(blocked.ID) {
		t.Fatal("blocked task should be startable after dependency completed")
	}

	// 依赖文件丢失 → 重新视为未完成。
	if err := os.Remove(filepath.Join(dir, pkgtask.TasksDirName, dep.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if store.CanStart(blocked.ID) {
		t.Fatal("missing dependency file must count as incomplete")
	}
}

// TestClaimTask 认领：pending → in_progress 并记录 owner；
// 被阻塞、非 pending、不存在的任务分别给出拒绝原因或错误。
func TestClaimTask(t *testing.T) {
	_, store, _ := newTestTaskStore(t)

	free, _ := store.Create("free task", "")
	msg, err := store.Claim(free.ID, "alice")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !strings.HasPrefix(msg, "Claimed "+free.ID) || !strings.Contains(msg, free.Subject) {
		t.Fatalf("claim message = %q", msg)
	}
	got, _ := store.Load(free.ID)
	if got.Status != pkgtask.StatusInProgress || got.Owner == nil || *got.Owner != "alice" {
		t.Fatalf("after claim: status=%s owner=%v", got.Status, got.Owner)
	}

	// 再次认领同一任务：已不是 pending，拒绝。
	msg, err = store.Claim(free.ID, "")
	if err != nil || !strings.Contains(msg, "cannot claim") {
		t.Fatalf("re-claim = (%q, %v), want cannot claim", msg, err)
	}

	// 被阻塞的任务：给出阻塞来源，状态不变。
	dep, _ := store.Create("dependency", "")
	blocked, _ := store.Create("blocked task", "")
	if _, err := store.UpdateDependencies(blocked.ID, []string{dep.ID}); err != nil {
		t.Fatal(err)
	}
	msg, err = store.Claim(blocked.ID, "")
	if err != nil || !strings.HasPrefix(msg, "Blocked by: ") || !strings.Contains(msg, dep.ID) {
		t.Fatalf("blocked claim = (%q, %v), want Blocked by: %s", msg, err, dep.ID)
	}
	bt, _ := store.Load(blocked.ID)
	if bt.Status != pkgtask.StatusPending || bt.Owner != nil {
		t.Fatalf("blocked claim mutated state: %+v", bt)
	}

	// 不存在的任务：错误。
	if _, err := store.Claim("task_00000000", ""); err == nil {
		t.Fatal("claim unknown task accepted")
	}

	// 空 owner 回退默认值 "agent"。
	d, _ := store.Create("default owner", "")
	if _, err := store.Claim(d.ID, "  "); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Load(d.ID)
	if got.Owner == nil || *got.Owner != pkgtask.DefaultOwner {
		t.Fatalf("empty owner should default to %q, got %v", pkgtask.DefaultOwner, got.Owner)
	}
}

// TestCompleteTaskUnblocksDownstream 完成任务解锁下游，且"刚解锁"以完成前快照为准；
// 非 in_progress 或 owner 不匹配时拒绝。
func TestCompleteTaskUnblocksDownstream(t *testing.T) {
	_, store, _ := newTestTaskStore(t)

	schema, _ := store.Create("setup database schema", "")
	endpoints, _ := store.Create("create API endpoints", "")
	tests, _ := store.Create("write tests", "")
	docs, _ := store.Create("write docs", "")
	mustUpdate := func(id string, deps []string) {
		t.Helper()
		if _, err := store.UpdateDependencies(id, deps); err != nil {
			t.Fatalf("update %s: %v", id, err)
		}
	}
	mustUpdate(endpoints.ID, []string{schema.ID})
	mustUpdate(tests.ID, []string{endpoints.ID})
	mustUpdate(docs.ID, []string{schema.ID})

	// owner 不匹配：不能完成别人的任务。
	if _, err := store.Claim(schema.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	msg, err := store.Complete(schema.ID, "bob")
	if err != nil || !strings.Contains(msg, "owned by alice, not bob") {
		t.Fatalf("wrong owner complete = (%q, %v)", msg, err)
	}

	// 完成 schema：endpoints 与 docs 刚解锁（tests 还被 endpoints 挡着）。
	// 解锁列表按 ID 排序返回，断言只看成员不看出场顺序。
	msg, err = store.Complete(schema.ID, "alice")
	if err != nil {
		t.Fatalf("complete schema: %v", err)
	}
	if !strings.Contains(msg, "Completed "+schema.ID) {
		t.Fatalf("complete schema message = %q", msg)
	}
	unblockedPart := msg[strings.Index(msg, "Unblocked:"):]
	for _, subject := range []string{"create API endpoints", "write docs"} {
		if !strings.Contains(unblockedPart, subject) {
			t.Fatalf("complete schema should unblock %q, got %q", subject, msg)
		}
	}
	if strings.Contains(unblockedPart, "write tests") {
		t.Fatalf("write tests must stay blocked until endpoints completes, got %q", msg)
	}
	got, _ := store.Load(schema.ID)
	if got.Status != pkgtask.StatusCompleted {
		t.Fatalf("schema status = %s", got.Status)
	}

	// 完成 endpoints：只剩 tests 刚解锁。
	if _, err := store.Claim(endpoints.ID, ""); err != nil {
		t.Fatal(err)
	}
	msg, err = store.Complete(endpoints.ID, "")
	if err != nil || !strings.Contains(msg, "Unblocked: write tests") || strings.Contains(msg, "docs") {
		t.Fatalf("complete endpoints message = %q", msg)
	}

	// 完成 docs：没有新的下游解锁，消息不含 Unblocked。
	if _, err := store.Claim(docs.ID, ""); err != nil {
		t.Fatal(err)
	}
	msg, err = store.Complete(docs.ID, "")
	if err != nil || strings.Contains(msg, "Unblocked") {
		t.Fatalf("complete docs message should not report unblocked, got %q", msg)
	}

	// 未认领的任务不能直接完成。
	msg, err = store.Complete(tests.ID, "")
	if err != nil || !strings.Contains(msg, "cannot complete") {
		t.Fatalf("complete pending = (%q, %v)", msg, err)
	}
	if _, err := store.Complete("task_00000000", ""); err == nil {
		t.Fatal("complete unknown task accepted")
	}
}

// TestTaskHandlersViaRunTool 六个工具经 runTool 分发可用：参数校验、
// 完整 JSON 返回、列表渲染与错误前缀。
func TestTaskHandlersViaRunTool(t *testing.T) {
	_, store, st := newTestTaskStore(t)

	if got := runTool(st, "list_tasks", nil); got != "(no tasks)" {
		t.Fatalf("empty list_tasks = %q", got)
	}

	// create_task 返回完整 JSON。
	out := runTool(st, "create_task", json.RawMessage(`{"subject":"setup database schema","description":"users table"}`))
	var created pkgtask.Task
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("create_task output not JSON: %v\n%s", err, out)
	}
	if created.Subject != "setup database schema" || created.Status != pkgtask.StatusPending {
		t.Fatalf("create_task returned %+v", created)
	}

	// subject 缺失 → 错误。
	if got := runTool(st, "create_task", json.RawMessage(`{"subject":"  "}`)); !strings.HasPrefix(got, "Error:") {
		t.Fatalf("create_task empty subject = %q", got)
	}

	// update_task 加边后 get_task 能读到完整记录（含 description 与 blockedBy）。
	dep := created
	other := runTool(st, "create_task", json.RawMessage(`{"subject":"create API endpoints"}`))
	var endpoints pkgtask.Task
	if err := json.Unmarshal([]byte(other), &endpoints); err != nil {
		t.Fatalf("create_task output not JSON: %v", err)
	}
	if got := runTool(st, "update_task", json.RawMessage(`{"task_id":"`+endpoints.ID+`","addBlockedBy":["`+dep.ID+`"]}`)); !strings.Contains(got, dep.ID) {
		t.Fatalf("update_task output missing dependency: %s", got)
	}
	gotJSON := runTool(st, "get_task", json.RawMessage(`{"task_id":"`+endpoints.ID+`"}`))
	var full pkgtask.Task
	if err := json.Unmarshal([]byte(gotJSON), &full); err != nil {
		t.Fatalf("get_task output not JSON: %v", err)
	}
	if full.Description != "" || len(full.BlockedBy) != 1 || full.BlockedBy[0] != dep.ID {
		t.Fatalf("get_task returned %+v", full)
	}

	// claim/complete 经 handler 全流程。
	if got := runTool(st, "claim_task", json.RawMessage(`{"task_id":"`+dep.ID+`"}`)); !strings.HasPrefix(got, "Claimed ") {
		t.Fatalf("claim_task = %q", got)
	}
	completed := runTool(st, "complete_task", json.RawMessage(`{"task_id":"`+dep.ID+`","owner":"agent"}`))
	if !strings.Contains(completed, "Completed "+dep.ID) || !strings.Contains(completed, "create API endpoints") {
		t.Fatalf("complete_task = %q", completed)
	}

	// list_tasks 渲染一行摘要：ID、状态、owner、依赖。
	listing := runTool(st, "list_tasks", nil)
	for _, want := range []string{dep.ID + " [completed]", endpoints.ID + " [pending]", "owner: none", "blocked by: " + dep.ID} {
		if !strings.Contains(listing, want) {
			t.Fatalf("list_tasks missing %q:\n%s", want, listing)
		}
	}

	// 未知任务 ID / 非法参数 → Error 前缀。
	for _, tc := range []struct{ name, tool, args string }{
		{"update unknown", "update_task", `{"task_id":"task_00000000"}`},
		{"claim unknown", "claim_task", `{"task_id":"task_00000000"}`},
		{"complete unknown", "complete_task", `{"task_id":"task_00000000"}`},
		{"get unknown", "get_task", `{"task_id":"task_00000000"}`},
		{"update bad json", "update_task", `{bad`},
		{"claim bad json", "claim_task", `{bad`},
		{"complete bad json", "complete_task", `{bad`},
		{"get bad json", "get_task", `{bad`},
		{"create bad json", "create_task", `{bad`},
	} {
		if got := runTool(st, tc.tool, json.RawMessage(tc.args)); !strings.HasPrefix(got, "Error:") {
			t.Errorf("%s: got %q, want Error prefix", tc.name, got)
		}
	}

	// store 指向同目录，handler 与直接调用一致。
	if st.Tasks.Dir() != store.Dir() {
		t.Fatalf("handler store dir = %s, want %s", st.Tasks.Dir(), store.Dir())
	}
}
