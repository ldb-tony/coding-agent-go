// state.go 定义会话级状态容器 State。
//
// sessionState 挂在 agentLoop 上而不是全局变量：每次新的用户输入都会新建一个，
// 因此文件读取缓存、shell cwd、已连接 MCP 等状态在轮次之间隔离。
package session

import (
	"context"
	"path/filepath"
	"sync"

	"coding-agent-go/pkg/llm"
	"coding-agent-go/pkg/tools/filesystem"
	"coding-agent-go/pkg/tools/mcp"
	"coding-agent-go/pkg/tools/memory"
	"coding-agent-go/pkg/tools/skill"
	"coding-agent-go/pkg/tools/task"
	"coding-agent-go/pkg/tools/todo"
)

// State 是会话级状态。
type State struct {
	Reads            map[string]filesystem.ReadState // absPath -> 最近一次成功读取
	ReadsMu          sync.Mutex                      // 保护 Reads map 的并发访问
	Todo             *todo.Manager                   // 会话级待办列表
	Tasks            *task.Store                     // .tasks/ 持久化任务图（跨会话）
	Skills           *skill.Loader                   // 会话启动时扫描出的技能注册表
	Memory           *memory.Store                   // 跨会话记忆存储（.memory/）
	RecalledMemories string                          // 会话开始时召回的相关记忆全文（进 system prompt）
	Client           llm.LLMClient                   // 子代理的嵌套 loop 复用父的 LLM 客户端
	Ctx              context.Context                 // 子代理继承父 loop 的 context（超时/取消）
	ShellCwd         string                          // bash cwd 持久化（跨调用保持工作目录）
	Mcp              *mcp.McpRegistry                // 已连接 MCP 服务注册表（首次 connect_mcp 时初始化）
}

// Lookup 实现 filesystem.ReadCache：返回指定路径的最近一次读取记录。
func (s *State) Lookup(abs string) (filesystem.ReadState, bool) {
	s.ReadsMu.Lock()
	defer s.ReadsMu.Unlock()
	rs, ok := s.Reads[abs]
	return rs, ok
}

// Store 实现 filesystem.ReadCache：写入指定路径的读取记录。
func (s *State) Store(abs string, rs filesystem.ReadState) {
	s.ReadsMu.Lock()
	defer s.ReadsMu.Unlock()
	s.Reads[abs] = rs
}

// ReadsSnapshot 返回读取缓存的加锁快照，供需要遍历全部记录的调用方使用
// （遍历返回的副本无需持锁）。
func (s *State) ReadsSnapshot() map[string]filesystem.ReadState {
	s.ReadsMu.Lock()
	defer s.ReadsMu.Unlock()
	out := make(map[string]filesystem.ReadState, len(s.Reads))
	for k, v := range s.Reads {
		out[k] = v
	}
	return out
}

// NewState 按当前 Workdir 初始化会话状态。
func NewState() *State {
	skills := skill.NewLoader(filepath.Join(Workdir(), "skills"))
	skills.Scan()
	return &State{
		Reads:  map[string]filesystem.ReadState{},
		Todo:   todo.NewManager(),
		Tasks:  task.NewStore(filepath.Join(Workdir(), task.TasksDirName)),
		Skills: skills,
		Memory: memory.NewStore(filepath.Join(Workdir(), memory.DirName)),
	}
}
