# Agent Teams 实施计划 - 基于 Eino 框架（更新版）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现基于 Eino 框架的 Agent Teams 多 Agent 协作系统，支持 Project 级隔离、同类型 Teammate 限制（最多 3 个）、任务排队、Agent 持久化与恢复、两层 Memory 系统、消息传递架构。

**Architecture:** 
- **GlobalManager** 管理多个 ProjectContext，每个 Project 完全隔离
- **AgentTeamsManager** 管理 Teammate 生命周期（同类型最多 3 个，超出则排队）
- **MessageBus** 处理消息通信（消息传递优于共享内存）
- **TaskStore** 管理共享任务与任务队列
- **WorktreeManager** 提供文件系统隔离
- **FileAccessTracker** 实现基于时间戳的乐观并发控制
- **AgentPersistence** 支持 Agent 上下文持久化与恢复
- **两层 Memory** - ProjectMemory（项目级）+ CrossProjectMemory（全局）

**Tech Stack:** 
- Go 1.21+
- Eino ADK (github.com/cloudwego/eino)
- Git worktree（文件系统隔离）
- JSON 文件持久化
- 现有 anthropic-sdk-go（渐进迁移）

**Spec:** [docs/design/agent-teams-eino-design.md](file:///Users/tony/coding-agent-go-eino/docs/design/agent-teams-eino-design.md)

## Global Constraints

- 每个 Task 必须产出可运行的代码（增量重构原则）
- **消息传递架构** - 通过通信传递信息，最小化共享状态，减少锁使用
- **Project 级隔离** - 不同 Project 的 agent teams 完全独立
- **同类型限制** - 同类型 Teammate 最多 3 个，超出则任务排队
- **乐观并发控制** - 基于时间戳，不使用互斥锁
- **持久化支持** - Agent 上下文可保存和恢复
- **两层 Memory** - Project 级 + 跨 Project 级
- 保持与现有 Subagent 的兼容性（渐进迁移）
- 所有公开 API 必须有完整的单元测试
- 集成 Eino 的可观测性能力（trace、metrics、logging）

---

## Phase 1: 基础架构与 Project 隔离（Task 1-4）

### Task 1: 核心数据结构与接口定义

**Files:**
- Create: `agent_teams_types.go` - 核心类型定义
- Create: `agent_teams_types_test.go` - 类型验证测试

**Interfaces:**
- Consumes: 无（起始任务）
- Produces: `Message`, `Task`, `TaskStatus`, `MsgType`, `TeammateConfig`, `TeammateType`, `ProjectContext` 等基础类型

- [ ] **Step 1: 编写类型定义测试**

```go
// agent_teams_types_test.go
package main

import (
    "testing"
    "time"
)

func TestMessageCreation(t *testing.T) {
    msg := &Message{
        ID:        "msg-001",
        From:      "main",
        To:        "coder-1",
        Type:      MsgTypeChat,
        Content:   "Hello",
        Timestamp: time.Now(),
    }
    
    if msg.ID != "msg-001" {
        t.Errorf("Expected ID 'msg-001', got '%s'", msg.ID)
    }
    if msg.Type != MsgTypeChat {
        t.Errorf("Expected type MsgTypeChat, got %v", msg.Type)
    }
}

func TestTaskStatusTransitions(t *testing.T) {
    task := &Task{
        ID:     "task-001",
        Status: TaskStatusPending,
    }
    
    if !task.CanTransitionTo(TaskStatusInProgress) {
        t.Error("Pending should transition to InProgress")
    }
    if task.CanTransitionTo(TaskStatusCompleted) {
        t.Error("Pending should not transition directly to Completed")
    }
}

func TestTeammateType(t *testing.T) {
    coder := TeammateTypeCoder
    researcher := TeammateTypeResearcher
    
    if coder == researcher {
        t.Error("Different types should not be equal")
    }
}
```

- [ ] **Step 2: 运行测试验证失败**

Run: `go test -v -run TestMessageCreation ./...`
Expected: FAIL（类型未定义）

- [ ] **Step 3: 实现核心类型定义**

```go
// agent_teams_types.go
package main

import (
    "time"
)

// MsgType 消息类型
type MsgType string

const (
    MsgTypeTaskAssign   MsgType = "task_assign"
    MsgTypeTaskResult   MsgType = "task_result"
    MsgTypeChat         MsgType = "chat"
    MsgTypeBroadcast    MsgType = "broadcast"
    MsgTypeStatusUpdate MsgType = "status_update"
    MsgTypeConflict     MsgType = "conflict"
    MsgTypeRequest      MsgType = "request"
    MsgTypeResponse     MsgType = "response"
)

// TaskStatus 任务状态
type TaskStatus string

const (
    TaskStatusPending    TaskStatus = "pending"
    TaskStatusInProgress TaskStatus = "in_progress"
    TaskStatusCompleted  TaskStatus = "completed"
    TaskStatusBlocked    TaskStatus = "blocked"
    TaskStatusFailed     TaskStatus = "failed"
    TaskStatusQueued     TaskStatus = "queued"
)

// TeammateType Teammate 类型
type TeammateType string

const (
    TeammateTypeCoder      TeammateType = "coder"
    TeammateTypeResearcher TeammateType = "researcher"
    TeammateTypeReviewer   TeammateType = "reviewer"
    TeammateTypeTester     TeammateType = "tester"
)

// Message Agent 间消息
type Message struct {
    ID        string                 `json:"id"`
    From      string                 `json:"from"`
    To        string                 `json:"to"`
    Type      MsgType                `json:"type"`
    Content   string                 `json:"content"`
    Payload   interface{}            `json:"payload,omitempty"`
    Timestamp time.Time              `json:"timestamp"`
    Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// Task 共享任务
type Task struct {
    ID          string     `json:"id"`
    Title       string     `json:"title"`
    Description string     `json:"description"`
    Status      TaskStatus `json:"status"`
    Assignee    string     `json:"assignee"`
    CreatedBy   string     `json:"created_by"`
    BlockedBy   []string   `json:"blocked_by"`
    Result      string     `json:"result"`
    Priority    int        `json:"priority"` // 0 最高
    CreatedAt   time.Time  `json:"created_at"`
    UpdatedAt   time.Time  `json:"updated_at"`
    EnqueuedAt  *time.Time `json:"enqueued_at,omitempty"`
}

// CanTransitionTo 检查状态转换是否合法
func (t *Task) CanTransitionTo(newStatus TaskStatus) bool {
    validTransitions := map[TaskStatus][]TaskStatus{
        TaskStatusPending:    {TaskStatusInProgress, TaskStatusBlocked, TaskStatusFailed, TaskStatusQueued},
        TaskStatusInProgress: {TaskStatusCompleted, TaskStatusFailed, TaskStatusBlocked, TaskStatusQueued},
        TaskStatusBlocked:    {TaskStatusInProgress, TaskStatusFailed, TaskStatusQueued},
        TaskStatusQueued:     {TaskStatusPending, TaskStatusInProgress, TaskStatusFailed},
        TaskStatusFailed:     {TaskStatusPending, TaskStatusQueued},
        TaskStatusCompleted:  {},
    }
    
    allowed, exists := validTransitions[t.Status]
    if !exists {
        return false
    }
    
    for _, s := range allowed {
        if s == newStatus {
            return true
        }
    }
    return false
}

// TeammateConfig Teammate 配置
type TeammateConfig struct {
    Name         string       `json:"name"`
    Role         string       `json:"role"`
    Type         TeammateType `json:"type"`
    SystemPrompt string       `json:"system_prompt"`
    Tools        []string     `json:"tools"`
    MaxTurns     int          `json:"max_turns"`
    UseWorktree  bool         `json:"use_worktree"`
    Priority     int          `json:"priority"`
}

// ProjectContext Project 上下文
type ProjectContext struct {
    ProjectID     string             `json:"project_id"`
    WorkDir       string             `json:"work_dir"`
    TeamsManager  *AgentTeamsManager `json:"-"`
    ProjectMemory *ProjectMemory     `json:"-"`
    TaskStore     *TaskStore         `json:"-"`
    MessageBus    *MessageBus        `json:"-"`
    CreatedAt     time.Time          `json:"created_at"`
}

// QueuedTask 队列中的任务
type QueuedTask struct {
    Task       *Task     `json:"task"`
    Priority   int       `json:"priority"`
    EnqueuedAt time.Time `json:"enqueued_at"`
}
```

- [ ] **Step 4: 运行测试验证通过**

Run: `go test -v -run TestMessageCreation ./...`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add agent_teams_types.go agent_teams_types_test.go
git commit -m "feat(agent-teams): 定义核心数据结构与接口（含 Project 隔离支持）"
```

---

### Task 2: Project 隔离与全局管理

**Files:**
- Create: `project_manager.go` - Project 管理器
- Create: `project_manager_test.go` - Project 隔离测试

**Interfaces:**
- Consumes: `agent_teams_types.go` 的 `ProjectContext`
- Produces: `GlobalManager` 及其方法 `GetOrCreateProject`, `ListProjects`, `DeleteProject`

**Dependencies:** Task 1 完成

- [ ] **Step 1: 编写 Project 隔离测试**

```go
// project_manager_test.go
package main

import (
    "testing"
)

func TestGlobalManager_GetOrCreateProject(t *testing.T) {
    manager := NewGlobalManager()
    
    ctxA, err := manager.GetOrCreateProject("project-a", "/tmp/project-a")
    if err != nil {
        t.Fatalf("GetOrCreateProject failed: %v", err)
    }
    
    if ctxA.ProjectID != "project-a" {
        t.Errorf("Expected project ID 'project-a', got '%s'", ctxA.ProjectID)
    }
    
    ctxA2, _ := manager.GetOrCreateProject("project-a", "/tmp/project-a")
    if ctxA != ctxA2 {
        t.Error("Should return same instance for same project ID")
    }
    
    ctxB, _ := manager.GetOrCreateProject("project-b", "/tmp/project-b")
    if ctxA == ctxB {
        t.Error("Different projects should have different contexts")
    }
    
    if ctxA.MessageBus == ctxB.MessageBus {
        t.Error("MessageBus should not be shared between projects")
    }
    if ctxA.TaskStore == ctxB.TaskStore {
        t.Error("TaskStore should not be shared between projects")
    }
}

func TestGlobalManager_ListProjects(t *testing.T) {
    manager := NewGlobalManager()
    
    manager.GetOrCreateProject("project-a", "/tmp/a")
    manager.GetOrCreateProject("project-b", "/tmp/b")
    manager.GetOrCreateProject("project-c", "/tmp/c")
    
    projects := manager.ListProjects()
    if len(projects) != 3 {
        t.Errorf("Expected 3 projects, got %d", len(projects))
    }
}

func TestGlobalManager_DeleteProject(t *testing.T) {
    manager := NewGlobalManager()
    
    manager.GetOrCreateProject("project-a", "/tmp/a")
    
    err := manager.DeleteProject("project-a")
    if err != nil {
        t.Errorf("DeleteProject failed: %v", err)
    }
    
    projects := manager.ListProjects()
    if len(projects) != 0 {
        t.Error("Project should be deleted")
    }
}
```

- [ ] **Step 2: 运行测试验证失败**

Run: `go test -v -run TestGlobalManager ./...`
Expected: FAIL

- [ ] **Step 3: 实现 GlobalManager**

```go
// project_manager.go
package main

import (
    "fmt"
    "sync"
    "time"
)

// GlobalManager 全局管理器（管理多个 Project）
type GlobalManager struct {
    projects           map[string]*ProjectContext
    crossProjectMemory *CrossProjectMemory
    
    mu sync.RWMutex
}

// NewGlobalManager 创建全局管理器
func NewGlobalManager() *GlobalManager {
    return &GlobalManager{
        projects:           make(map[string]*ProjectContext),
        crossProjectMemory: NewCrossProjectMemory(),
    }
}

// GetOrCreateProject 获取或创建 Project 上下文
func (g *GlobalManager) GetOrCreateProject(projectID, workDir string) (*ProjectContext, error) {
    g.mu.RLock()
    if ctx, exists := g.projects[projectID]; exists {
        g.mu.RUnlock()
        return ctx, nil
    }
    g.mu.RUnlock()
    
    g.mu.Lock()
    defer g.mu.Unlock()
    
    if ctx, exists := g.projects[projectID]; exists {
        return ctx, nil
    }
    
    ctx := &ProjectContext{
        ProjectID:     projectID,
        WorkDir:       workDir,
        TeamsManager:  NewAgentTeamsManager(projectID, workDir),
        ProjectMemory: NewProjectMemory(projectID, workDir, g.crossProjectMemory),
        TaskStore:     NewTaskStore(),
        MessageBus:    NewMessageBus(),
        CreatedAt:     time.Now(),
    }
    
    ctx.MessageBus.RegisterAgent("main")
    
    g.projects[projectID] = ctx
    
    return ctx, nil
}

// ListProjects 列出所有 Project
func (g *GlobalManager) ListProjects() []*ProjectContext {
    g.mu.RLock()
    defer g.mu.RUnlock()
    
    projects := make([]*ProjectContext, 0, len(g.projects))
    for _, ctx := range g.projects {
        projects = append(projects, ctx)
    }
    
    return projects
}

// DeleteProject 删除 Project
func (g *GlobalManager) DeleteProject(projectID string) error {
    g.mu.Lock()
    defer g.mu.Unlock()
    
    ctx, exists := g.projects[projectID]
    if !exists {
        return fmt.Errorf("project not found: %s", projectID)
    }
    
    ctx.TeamsManager.Shutdown()
    
    delete(g.projects, projectID)
    
    return nil
}

// GetCrossProjectMemory 获取跨 Project Memory
func (g *GlobalManager) GetCrossProjectMemory() *CrossProjectMemory {
    return g.crossProjectMemory
}

// ResumeProject 恢复 Project 的 Agent Teams
func (g *GlobalManager) ResumeProject(projectID, workDir string) error {
    ctx, err := g.GetOrCreateProject(projectID, workDir)
    if err != nil {
        return err
    }
    
    return ctx.TeamsManager.ResumeAllAgents()
}
```

- [ ] **Step 4: 运行测试验证通过**

Run: `go test -v -run TestGlobalManager ./...`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add project_manager.go project_manager_test.go
git commit -m "feat(agent-teams): 实现 Project 级隔离与全局管理"
```

---

### Task 3: 两层 Memory 系统

**Files:**
- Create: `memory.go` - Memory 系统实现
- Create: `memory_test.go` - Memory 测试

**Interfaces:**
- Consumes: 无
- Produces: `CrossProjectMemory`, `ProjectMemory`, `MemoryEntry`

**Dependencies:** Task 1 完成

- [ ] **Step 1: 编写 Memory 系统测试**

```go
// memory_test.go
package main

import (
    "os"
    "path/filepath"
    "testing"
)

func TestCrossProjectMemory_Store(t *testing.T) {
    tmpDir := t.TempDir()
    mem := NewCrossProjectMemoryWithDir(tmpDir)
    
    err := mem.Store("user-preference-language", "chinese")
    if err != nil {
        t.Fatalf("Store failed: %v", err)
    }
    
    results, _ := mem.Recall("language")
    if len(results) == 0 {
        t.Error("Should find stored entry")
    }
}

func TestCrossProjectMemory_Recall(t *testing.T) {
    tmpDir := t.TempDir()
    mem := NewCrossProjectMemoryWithDir(tmpDir)
    
    mem.Store("eino-pattern-1", "AgentAsTool is recommended")
    mem.Store("eino-pattern-2", "Graph for orchestration")
    mem.Store("go-best-practice", "Use interfaces")
    
    results, _ := mem.Recall("eino")
    if len(results) != 2 {
        t.Errorf("Expected 2 results, got %d", len(results))
    }
}

func TestProjectMemory_Store(t *testing.T) {
    tmpDir := t.TempDir()
    projectDir := filepath.Join(tmpDir, "project")
    os.MkdirAll(projectDir, 0755)
    
    crossMem := NewCrossProjectMemoryWithDir(filepath.Join(tmpDir, "cross"))
    projectMem := NewProjectMemory("test-project", projectDir, crossMem)
    
    err := projectMem.Store("refactor-plan", "Vertical-slice migration")
    if err != nil {
        t.Fatalf("Store failed: %v", err)
    }
    
    results, _ := projectMem.Recall("refactor")
    if len(results) == 0 {
        t.Error("Should find stored entry")
    }
}

func TestProjectMemory_RecallWithCrossProject(t *testing.T) {
    tmpDir := t.TempDir()
    projectDir := filepath.Join(tmpDir, "project")
    os.MkdirAll(projectDir, 0755)
    
    crossMem := NewCrossProjectMemoryWithDir(filepath.Join(tmpDir, "cross"))
    crossMem.Store("global-knowledge", "Go best practices")
    
    projectMem := NewProjectMemory("test-project", projectDir, crossMem)
    projectMem.Store("project-knowledge", "Project architecture")
    
    results, _ := projectMem.Recall("knowledge")
    if len(results) < 2 {
        t.Errorf("Expected at least 2 results (project + cross), got %d", len(results))
    }
}
```

- [ ] **Step 2: 运行测试验证失败**

Run: `go test -v -run TestCrossProjectMemory ./...`
Expected: FAIL

- [ ] **Step 3: 实现 Memory 系统**

```go
// memory.go
package main

import (
    "encoding/json"
    "os"
    "path/filepath"
    "strings"
    "sync"
    "time"
)

// MemoryEntry Memory 条目
type MemoryEntry struct {
    Key       string                 `json:"key"`
    Value     interface{}            `json:"value"`
    Metadata  map[string]interface{} `json:"metadata"`
    CreatedAt time.Time              `json:"created_at"`
    UpdatedAt time.Time              `json:"updated_at"`
}

// CrossProjectMemory 跨 Project Memory
type CrossProjectMemory struct {
    storageDir string
    entries    map[string]*MemoryEntry
    mu         sync.RWMutex
}

// NewCrossProjectMemory 创建跨 Project Memory
func NewCrossProjectMemory() *CrossProjectMemory {
    storageDir := filepath.Join(os.Getenv("HOME"), ".qoder", "memory")
    return NewCrossProjectMemoryWithDir(storageDir)
}

// NewCrossProjectMemoryWithDir 创建指定目录的 CrossProjectMemory
func NewCrossProjectMemoryWithDir(storageDir string) *CrossProjectMemory {
    os.MkdirAll(storageDir, 0755)
    
    mem := &CrossProjectMemory{
        storageDir: storageDir,
        entries:    make(map[string]*MemoryEntry),
    }
    
    mem.load()
    
    return mem
}

// Store 存储
func (m *CrossProjectMemory) Store(key string, value interface{}) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    entry := &MemoryEntry{
        Key:       key,
        Value:     value,
        Metadata:  make(map[string]interface{}),
        CreatedAt: time.Now(),
        UpdatedAt: time.Now(),
    }
    
    m.entries[key] = entry
    
    return m.save()
}

// Recall 查询
func (m *CrossProjectMemory) Recall(query string) ([]MemoryEntry, error) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    var results []MemoryEntry
    queryLower := strings.ToLower(query)
    
    for _, entry := range m.entries {
        if strings.Contains(strings.ToLower(entry.Key), queryLower) {
            results = append(results, *entry)
        }
    }
    
    return results, nil
}

// Delete 删除
func (m *CrossProjectMemory) Delete(key string) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    delete(m.entries, key)
    return m.save()
}

// List 列出所有
func (m *CrossProjectMemory) List() ([]MemoryEntry, error) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    results := make([]MemoryEntry, 0, len(m.entries))
    for _, entry := range m.entries {
        results = append(results, *entry)
    }
    
    return results, nil
}

func (m *CrossProjectMemory) save() error {
    data, err := json.MarshalIndent(m.entries, "", "  ")
    if err != nil {
        return err
    }
    
    filepath := filepath.Join(m.storageDir, "cross-project.json")
    return os.WriteFile(filepath, data, 0644)
}

func (m *CrossProjectMemory) load() {
    filepath := filepath.Join(m.storageDir, "cross-project.json")
    data, err := os.ReadFile(filepath)
    if err != nil {
        return
    }
    
    json.Unmarshal(data, &m.entries)
}

// ProjectMemory Project 级 Memory
type ProjectMemory struct {
    projectID  string
    storageDir string
    entries    map[string]*MemoryEntry
    mu         sync.RWMutex
    
    crossProjectMemory *CrossProjectMemory
}

// NewProjectMemory 创建 Project Memory
func NewProjectMemory(projectID, projectDir string, crossMem *CrossProjectMemory) *ProjectMemory {
    storageDir := filepath.Join(projectDir, ".qoder", "memory")
    os.MkdirAll(storageDir, 0755)
    
    mem := &ProjectMemory{
        projectID:          projectID,
        storageDir:         storageDir,
        entries:            make(map[string]*MemoryEntry),
        crossProjectMemory: crossMem,
    }
    
    mem.load()
    
    return mem
}

// Store 存储到 Project Memory
func (m *ProjectMemory) Store(key string, value interface{}) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    entry := &MemoryEntry{
        Key:       key,
        Value:     value,
        Metadata:  map[string]interface{}{"project": m.projectID},
        CreatedAt: time.Now(),
        UpdatedAt: time_time.Now(),
    }
    
    m.entries[key] = entry
    
    return m.save()
}

// Recall 查询（先查 Project，再查 Cross-Project）
func (m *ProjectMemory) Recall(query string) ([]MemoryEntry, error) {
    projectResults, _ := m.recallLocal(query)
    crossResults, _ := m.crossProjectMemory.Recall(query)
    
    results := append(projectResults, crossResults...)
    
    return results, nil
}

func (m *ProjectMemory) recallLocal(query string) ([]MemoryEntry, error) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    var results []MemoryEntry
    queryLower := strings.ToLower(query)
    
    for _, entry := range m.entries {
        if strings.Contains(strings.ToLower(entry.Key), queryLower) {
            results = append(results, *entry)
        }
    }
    
    return results, nil
}

// Delete 删除
func (m *ProjectMemory) Delete(key string) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    delete(m.entries, key)
    return m.save()
}

// List 列出所有
func (m *ProjectMemory) List() ([]MemoryEntry, error) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    results := make([]MemoryEntry, 0, len(m.entries))
    for _, entry := range m.entries {
        results = append(results, *entry)
    }
    
    return results, nil
}

func (m *ProjectMemory) save() error {
    data, err := json.MarshalIndent(m.entries, "", "  ")
    if err != nil {
        return err
    }
    
    filepath := filepath.Join(m.storageDir, "project.json")
    return os.WriteFile(filepath, data, 0644)
}

func (m *ProjectMemory) load() {
    filepath := filepath.Join(m.storageDir, "project.json")
    data, err := os.ReadFile(filepath)
    if err != nil {
        return
    }
    
    json.Unmarshal(data, &m.entries)
}
```

- [ ] **Step 4: 运行测试验证通过**

Run: `go test -v -run TestCrossProjectMemory ./...`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add memory.go memory_test.go
git commit -m "feat(agent-teams): 实现两层 Memory 系统（Project + Cross-Project）"
```

---

### Task 4: FileAccessTracker - 基于时间戳的乐观并发控制

**Files:**
- Create: `file_access_tracker.go` - 文件访问跟踪器
- Create: `file_access_tracker_test.go` - 并发控制测试

**Interfaces:**
- Consumes: 无
- Produces: `FileAccessTracker` 及其方法 `RecordRead`, `CheckWritePermission`, `RecordWrite`, `RestoreRecord`

**Dependencies:** 无

- [ ] **Step 1-5: 实现 FileAccessTracker**

（参考原 Task 2 的完整代码，增加 `RestoreRecord` 方法用于 Agent 恢复）

```bash
git add file_access_tracker.go file_access_tracker_test.go
git commit -m "feat(agent-teams): 实现基于时间戳的乐观并发控制"
```

---

## Phase 2: 核心组件（Task 5-8）

### Task 5: MessageBus - Agent 间消息通信

**Files:**
- Create: `message_bus.go` - 消息总线
- Create: `message_bus_test.go` - 消息通信测试

**Interfaces:**
- Consumes: `Message`, `MsgType`
- Produces: `MessageBus` 及其方法

**Dependencies:** Task 1 完成

- [ ] **Step 1-5: 实现 MessageBus**

（参考原 Task 4 的完整代码）

```bash
git add message_bus.go message_bus_test.go
git commit -m "feat(agent-teams): 实现 Agent 间消息通信总线"
```

---

### Task 6: TaskStore - 共享任务列表与队列管理

**Files:**
- Create: `taskstore_teams.go` - 任务存储（含队列）
- Create: `taskstore_teams_test.go` - 任务管理测试

**Interfaces:**
- Consumes: `Task`, `TaskStatus`, `QueuedTask`
- Produces: `TaskStore`, `TaskQueue` 及其方法 `CreateTask`, `EnqueueTask`, `DequeueTask`, `GetReadyTasks`

**Dependencies:** Task 1 完成

- [ ] **Step 1: 编写任务管理测试**

```go
// taskstore_teams_test.go
package main

import (
    "testing"
    "time"
)

func TestTaskStore_CreateTask(t *testing.T) {
    store := NewTaskStore()
    
    task := &Task{
        ID:          "task-001",
        Title:       "Implement feature",
        Description: "Implement new feature X",
        Status:      TaskStatusPending,
        CreatedBy:   "main",
        CreatedAt:   time.Now(),
    }
    
    err := store.CreateTask(task)
    if err != nil {
        t.Fatalf("CreateTask failed: %v", err)
    }
    
    retrieved, err := store.GetTask("task-001")
    if err != nil {
        t.Fatalf("GetTask failed: %v", err)
    }
    if retrieved.Title != task.Title {
        t.Errorf("Expected title '%s', got '%s'", task.Title, retrieved.Title)
    }
}

func TestTaskStore_EnqueueTask(t *testing.T) {
    store := NewTaskStore()
    
    task := &Task{
        ID:       "task-001",
        Status:   TaskStatusPending,
        Priority: 1,
    }
    
    err := store.EnqueueTask("coder", task)
    if err != nil {
        t.Fatalf("EnqueueTask failed: %v", err)
    }
    
    queue := store.GetQueue("coder")
    if queue.Length() != 1 {
        t.Errorf("Expected queue length 1, got %d", queue.Length())
    }
}

func TestTaskStore_DequeueTask(t *testing.T) {
    store := NewTaskStore()
    
    task1 := &Task{ID: "task-001", Priority: 2}
    task2 := &Task{ID: "task-002", Priority: 1}
    
    store.EnqueueTask("coder", task1)
    store.EnqueueTask("coder", task2)
    
    dequeued := store.DequeueTask("coder")
    if dequeued.ID != "task-002" {
        t.Errorf("Expected task-002 (higher priority), got %s", dequeued.ID)
    }
}

func TestTaskStore_GetReadyTasks(t *testing.T) {
    store := NewTaskStore()
    
    task1 := &Task{
        ID:       "task-001",
        Status:   TaskStatusPending,
        Assignee: "coder-1",
    }
    store.CreateTask(task1)
    
    task2 := &Task{
        ID:        "task-002",
        Status:    TaskStatusPending,
        Assignee:  "coder-1",
        BlockedBy: []string{"task-001"},
    }
    store.CreateTask(task2)
    
    ready := store.GetReadyTasks("coder-1")
    if len(ready) != 1 {
        t.Errorf("Expected 1 ready task, got %d", len(ready))
    }
    if ready[0].ID != "task-001" {
        t.Errorf("Expected task-001 to be ready")
    }
    
    store.UpdateTaskStatus("task-001", TaskStatusCompleted)
    
    ready = store.GetReadyTasks("coder-1")
    if len(ready) != 1 {
        t.Errorf("Expected 1 ready task after completion")
    }
    if ready[0].ID != "task-002" {
        t.Errorf("Expected task-002 to be ready")
    }
}
```

- [ ] **Step 2-5: 实现 TaskStore 与 TaskQueue**

（参考设计文档中的完整代码）

```bash
git add taskstore_teams.go taskstore_teams_test.go
git commit -m "feat(agent-teams): 实现共享任务列表与队列管理"
```

---

### Task 7: WorktreeManager - Git Worktree 隔离管理

**Files:**
- Create: `worktree_manager.go` - Worktree 管理器
- Create: `worktree_manager_test.go` - Worktree 管理测试

**Interfaces:**
- Consumes: 无
- Produces: `WorktreeManager`, `Worktree` 及其方法

**Dependencies:** 无

- [ ] **Step 1-5: 实现 WorktreeManager**

（参考原 Task 5 的完整代码）

```bash
git add worktree_manager.go worktree_manager_test.go
git commit -m "feat(agent-teams): 实现 Git Worktree 隔离管理"
```

---

### Task 8: TeammatePool - 同类型 Agent 限制与排队

**Files:**
- Create: `teammate_pool.go` - Teammate 池管理
- Create: `teammate_pool_test.go` - 池管理测试

**Interfaces:**
- Consumes: `TeammateAgent`, `TaskQueue`, `TeammateType`
- Produces: `TeammatePool` 及其方法 `CreateTeammate`, `SubmitTask`, `OnTeammateIdle`

**Dependencies:** Task 6, Task 9 完成

- [ ] **Step 1: 编写 Teammate 池测试**

```go
// teammate_pool_test.go
package main

import (
    "fmt"
    "testing"
)

func TestTeammatePool_CreateTeammate_Limit(t *testing.T) {
    pool := NewTeammatePool(nil, nil, nil, nil)
    
    for i := 0; i < 3; i++ {
        config := TeammateConfig{
            Name: fmt.Sprintf("coder-%d", i),
            Type: TeammateTypeCoder,
            Role: "coder",
        }
        _, err := pool.CreateTeammate(config)
        if err != nil {
            t.Errorf("CreateTeammate %d failed: %v", i, err)
        }
    }
    
    config := TeammateConfig{
        Name: "coder-3",
        Type: TeammateTypeCoder,
        Role: "coder",
    }
    _, err := pool.CreateTeammate(config)
    if err == nil {
        t.Error("Should fail when limit reached")
    }
    
    limitErr, ok := err.(*TeammateLimitError)
    if !ok {
        t.Error("Should return TeammateLimitError")
    }
    if limitErr.Limit != 3 {
        t.Errorf("Expected limit 3, got %d", limitErr.Limit)
    }
}

func TestTeammatePool_SubmitTask_Queuing(t *testing.T) {
    pool := NewTeammatePool(nil, nil, nil, nil)
    
    for i := 0; i < 3; i++ {
        config := TeammateConfig{
            Name: fmt.Sprintf("coder-%d", i),
            Type: TeammateTypeCoder,
        }
        teammate, _ := pool.CreateTeammate(config)
        teammate.status = "working"
    }
    
    task := &Task{
        ID:       "task-004",
        Priority: 0,
    }
    
    err := pool.SubmitTask(task, TeammateTypeCoder)
    if err != nil {
        t.Errorf("SubmitTask failed: %v", err)
    }
    
    queue := pool.taskStore.GetQueue(string(TeammateTypeCoder))
    if queue.Length() != 1 {
        t.Errorf("Expected 1 task in queue, got %d", queue.Length())
    }
}

func TestTeammatePool_OnTeammateIdle(t *testing.T) {
    pool := NewTeammatePool(nil, nil, nil, nil)
    
    config := TeammateConfig{
        Name: "coder-1",
        Type: TeammateTypeCoder,
    }
    teammate, _ := pool.CreateTeammate(config)
    
    task := &Task{ID: "task-001"}
    pool.taskStore.EnqueueTask(string(TeammateTypeCoder), task)
    
    pool.OnTeammateIdle(teammate)
    
    if teammate.status != "working" {
        t.Errorf("Expected teammate to be working")
    }
}
```

- [ ] **Step 2-5: 实现 TeammatePool**

（参考设计文档 3.3 节的完整代码）

```bash
git add teammate_pool.go teammate_pool_test.go
git commit -m "feat(agent-teams): 实现同类型 Agent 限制与任务排队"
```

---

## Phase 3: Agent 持久化与集成（Task 9-12）

### Task 9: TeammateAgent - Teammate 封装

**Files:**
- Create: `teammate_agent.go` - Teammate Agent 封装
- Create: `teammate_agent_test.go` - Teammate 测试

**Interfaces:**
- Consumes: `MessageBus`, `TaskStore`, `FileAccessTracker`, `WorktreeManager`
- Produces: `TeammateAgent` 及其方法 `HandleMessage`, `ExecuteTask`, `SendReply`, `CreateSnapshot`

**Dependencies:** Task 5, Task 6, Task 4, Task 7 完成

- [ ] **Step 1-5: 实现 TeammateAgent**

（参考原 Task 6，增加 `CreateSnapshot` 和 `RestoreFromSnapshot` 方法）

```bash
git add teammate_agent.go teammate_agent_test.go
git commit -m "feat(agent-teams): 实现 Teammate Agent 封装（含持久化支持）"
```

---

### Task 10: AgentPersistence - Agent 持久化与恢复

**Files:**
- Create: `agent_persistence.go` - Agent 持久化
- Create: `agent_persistence_test.go` - 持久化测试

**Interfaces:**
- Consumes: `TeammateAgent`, `AgentSnapshot`
- Produces: `AgentPersistence` 及其方法 `Save`, `Load`, `List`, `Delete`

**Dependencies:** Task 9 完成

- [ ] **Step 1: 编写持久化测试**

```go
// agent_persistence_test.go
package main

import (
    "fmt"
    "os"
    "path/filepath"
    "testing"
)

func TestAgentPersistence_Save(t *testing.T) {
    tmpDir := t.TempDir()
    persistence := NewAgentPersistence(tmpDir)
    
    agent := &TeammateAgent{
        id:   "agent-001",
        name: "coder-1",
        role: "coder",
    }
    
    err := persistence.Save(agent)
    if err != nil {
        t.Fatalf("Save failed: %v", err)
    }
    
    filepath := filepath.Join(tmpDir, "agent-001.json")
    if _, err := os.Stat(filepath); os.IsNotExist(err) {
        t.Error("Snapshot file not created")
    }
}

func TestAgentPersistence_Load(t *testing.T) {
    tmpDir := t.TempDir()
    persistence := NewAgentPersistence(tmpDir)
    
    agent := &TeammateAgent{
        id:     "agent-001",
        name:   "coder-1",
        role:   "coder",
        status: "working",
    }
    
    persistence.Save(agent)
    
    snapshot, err := persistence.Load("agent-001")
    if err != nil {
        t.Fatalf("Load failed: %v", err)
    }
    
    if snapshot.Name != "coder-1" {
        t.Errorf("Expected name 'coder-1', got '%s'", snapshot.Name)
    }
    if snapshot.Status != "working" {
        t.Errorf("Expected status 'working', got '%s'", snapshot.Status)
    }
}

func TestAgentPersistence_List(t *testing.T) {
    tmpDir := t.TempDir()
    persistence := NewAgentPersistence(tmpDir)
    
    for i := 0; i < 3; i++ {
        agent := &TeammateAgent{
            id:   fmt.Sprintf("agent-%d", i),
            name: fmt.Sprintf("coder-%d", i),
        }
        persistence.Save(agent)
    }
    
    snapshots, err := persistence.List()
    if err != nil {
        t.Fatalf("List failed: %v", err)
    }
    
    if len(snapshots) != 3 {
        t.Errorf("Expected 3 snapshots, got %d", len(snapshots))
    }
}
```

- [ ] **Step 2-5: 实现 AgentPersistence**

（参考设计文档 3.4 节的完整代码）

```bash
git add agent_persistence.go agent_persistence_test.go
git commit -m "feat(agent-teams): 实现 Agent 持久化与恢复"
```

---

### Task 11: AgentTeamsManager - 核心管理器（更新版）

**Files:**
- Create: `agent_teams_manager.go` - Agent Teams 管理器
- Create: `agent_teams_manager_test.go` - 管理器测试

**Interfaces:**
- Consumes: `TeammatePool`, `AgentPersistence`, `ProjectMemory`
- Produces: `AgentTeamsManager` 及其方法 `CreateTeammate`, `SubmitTask`, `ResumeAllAgents`

**Dependencies:** Task 8, Task 10 完成

- [ ] **Step 1-5: 实现 AgentTeamsManager**

（整合 TeammatePool 和 AgentPersistence，增加 `ResumeAllAgents` 方法）

```bash
git add agent_teams_manager.go agent_teams_manager_test.go
git commit -m "feat(agent-teams): 实现 Agent Teams 核心管理器（含持久化与池管理）"
```

---

### Task 12: 集成到现有 Agent Loop

**Files:**
- Modify: `agent.go` - 集成 Agent Teams
- Modify: `main.go` - 初始化 GlobalManager
- Create: `agent_teams_integration_test.go` - 集成测试

**Interfaces:**
- Consumes: `GlobalManager`, `ProjectContext`
- Produces: 集成后的 agent loop

**Dependencies:** Task 11 完成

- [ ] **Step 1-5: 集成实现**

（修改 main.go 初始化 GlobalManager，修改 agent.go 集成工具）

```bash
git add agent.go main.go agent_teams_integration_test.go
git commit -m "feat(agent-teams): 集成到现有 Agent Loop"
```

---

## Phase 4: 文档与优化（Task 13-14）

### Task 13: 工具集成与文档

**Files:**
- Create: `agent_teams_tools.go` - 工具定义
- Create: `docs/agent-teams-guide.md` - 使用指南
- Create: `examples/agent_teams_demo.go` - 示例代码

- [ ] **Step 1-5: 实现工具与文档**

```bash
git add agent_teams_tools.go docs/agent-teams-guide.md examples/agent_teams_demo.go
git commit -m "docs(agent-teams): 添加工具集成与使用文档"
```

---

### Task 14: 测试与优化

**Files:**
- Modify: 所有测试文件
- Create: `benchmarks/agent_teams_bench_test.go` - 性能测试

- [ ] **Step 1-5: 完善测试与优化**

```bash
git add benchmarks/
git commit -m "test(agent-teams): 完善测试覆盖与性能优化"
```

---

## 实施时间线（更新版）

| Phase | Task | 预计时间 | 依赖 |
|-------|------|---------|------|
| 1 | Task 1-4 | 1.5 周 | 无 |
| 2 | Task 5-8 | 1.5 周 | Phase 1 |
| 3 | Task 9-12 | 1.5 周 | Phase 2 |
| 4 | Task 13-14 | 1 周 | Phase 3 |

**总计**：5.5 周（不含 Eino 集成）

---

## 验收标准（更新版）

1. ✅ Project 级完全隔离
2. ✅ 同类型 Teammate 最多 3 个，超出自动排队
3. ✅ Agent 上下文可持久化与恢复
4. ✅ 两层 Memory 系统正常工作
5. ✅ 消息传递架构，最小化锁使用
6. ✅ 基于时间戳的乐观并发控制
7. ✅ 完整的单元测试覆盖（>80%）
8. ✅ 详细的使用文档与示例

---

## 风险与缓解（更新版）

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| Project 隔离导致资源重复 | 中 | 共享 CrossProjectMemory，只隔离必要状态 |
| 任务队列过长 | 中 | 支持优先级与超时，避免无限等待 |
| 持久化文件损坏 | 高 | JSON 格式校验 + 备份机制 |
| Memory 查询性能 | 低 | 后续可集成向量数据库优化语义搜索 |
| 消息丢失 | 高 | 消息历史持久化 + 确认机制 |

