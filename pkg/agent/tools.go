package agent

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"coding-agent-go/pkg/session"
	"coding-agent-go/pkg/tools/bash"
	pkgcompact "coding-agent-go/pkg/tools/compact"
	"coding-agent-go/pkg/tools/filesystem"
)

// flexInt 兼容 JSON 数字与数字字符串（模型偶发把 offset/limit 发成 "2000"），
// 对齐 Claude Code 的 semanticNumber；"abc"、2.5 等非法值在 Unmarshal 阶段报错，
// 避免旧实现里字符串数字静默归零、误变成"全量读"的危险语义。
type flexInt int

func (n *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		s = strings.TrimSpace(str)
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("invalid integer %q", s)
	}
	*n = flexInt(v)
	return nil
}

// re-export for handler compatibility
type readState = filesystem.ReadState

type sessionState = session.State

// safePath / matchGlob / matchSegs / maxGrepFileSize 已迁移至 pkg/tools/filesystem/

// toolHandler 接收会话状态与模型给出的原始 JSON 参数，返回工具结果文本。
type toolHandler func(st *sessionState, args json.RawMessage) string

// resourceSet 表示工具调用涉及的资源集合，用于路径感知调度。
type resourceSet struct {
	reads  []string // 读资源路径列表（已规范化）
	writes []string // 写资源路径列表（已规范化）
	global bool     // 是否为全局资源（bash/task/MCP 等无法静态分析的工具）
}

// toolSpec 是工具的内部元数据，包含处理器、并发安全性和资源函数。
// concurrencySafe 和 resources 只用于调度，绝不写入 Anthropic tool schema。
type toolSpec struct {
	handler         toolHandler
	concurrencySafe bool                                   // 是否可并发执行（默认 false，只对显式声明的只读工具置 true）
	resources       func(args json.RawMessage) resourceSet // 返回工具调用涉及的资源集合
}

// toolRegistry：工具名 → 内部元数据。取代原有的 toolHandlers map，
// 将 handler / 并发元数据 / 资源函数合为单一注册表，避免三处漂移。
var toolRegistry = map[string]*toolSpec{}

// task 的处理器在 init 里注册：taskHandler 经 runSubagent 复用 runTool，而 runTool
// 读取 toolRegistry —— 若在 map 字面量里直接引用 taskHandler，会构成包级初始化循环
// （initialization cycle）。init 阶段赋值可打破该循环。
func init() {
	// 注册所有工具到 toolRegistry，包含并发元数据和资源函数
	toolRegistry["bash"] = &toolSpec{
		handler:         bashHandler,
		concurrencySafe: false, // bash 是 global，无法静态分析
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
	toolRegistry["read_file"] = &toolSpec{
		handler:         readHandler,
		concurrencySafe: true, // 只读，无共享副作用
		resources: func(args json.RawMessage) resourceSet {
			var in struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(args, &in)
			if abs, err := filesystem.SafePath(in.Path, session.Workdir()); err == nil {
				return resourceSet{reads: []string{abs}}
			}
			return resourceSet{global: true} // 路径越界，保守为 global
		},
	}
	toolRegistry["write_file"] = &toolSpec{
		handler:         writeHandler,
		concurrencySafe: false, // 写操作，独占
		resources: func(args json.RawMessage) resourceSet {
			var in struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(args, &in)
			if abs, err := filesystem.SafePath(in.Path, session.Workdir()); err == nil {
				return resourceSet{writes: []string{abs}}
			}
			return resourceSet{global: true}
		},
	}
	toolRegistry["edit_file"] = &toolSpec{
		handler:         editHandler,
		concurrencySafe: false, // 写操作，独占
		resources: func(args json.RawMessage) resourceSet {
			var in struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(args, &in)
			if abs, err := filesystem.SafePath(in.Path, session.Workdir()); err == nil {
				return resourceSet{writes: []string{abs}}
			}
			return resourceSet{global: true}
		},
	}
	toolRegistry["glob"] = &toolSpec{
		handler:         globHandler,
		concurrencySafe: true, // 只读搜索
		resources: func(args json.RawMessage) resourceSet {
			var in struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(args, &in)
			searchRoot := in.Path
			if searchRoot == "" {
				searchRoot = session.Workdir()
			}
			if abs, err := filesystem.SafePath(searchRoot, session.Workdir()); err == nil {
				return resourceSet{reads: []string{abs}}
			}
			return resourceSet{global: true}
		},
	}
	toolRegistry["grep"] = &toolSpec{
		handler:         grepHandler,
		concurrencySafe: true, // 只读搜索
		resources: func(args json.RawMessage) resourceSet {
			var in struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(args, &in)
			searchRoot := in.Path
			if searchRoot == "" {
				searchRoot = session.Workdir()
			}
			if abs, err := filesystem.SafePath(searchRoot, session.Workdir()); err == nil {
				return resourceSet{reads: []string{abs}}
			}
			return resourceSet{global: true}
		},
	}
	toolRegistry["todo_write"] = &toolSpec{
		handler:         todoWriteHandler,
		concurrencySafe: false, // 涉及会话状态，保守为 unsafe
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
	toolRegistry["load_skill"] = &toolSpec{
		handler:         loadSkillHandler,
		concurrencySafe: false, // 涉及磁盘读取与惰性加载，保守为 unsafe
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
	toolRegistry["compact"] = &toolSpec{
		handler:         pkgcompact.CompactHandler,
		concurrencySafe: false, // 标记位操作，保守为 unsafe
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
	toolRegistry["create_task"] = &toolSpec{
		handler:         createTaskHandler,
		concurrencySafe: false, // 任务图操作，保守为 unsafe
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
	toolRegistry["update_task"] = &toolSpec{
		handler:         updateTaskHandler,
		concurrencySafe: false,
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
	toolRegistry["claim_task"] = &toolSpec{
		handler:         claimTaskHandler,
		concurrencySafe: false,
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
	toolRegistry["complete_task"] = &toolSpec{
		handler:         completeTaskHandler,
		concurrencySafe: false,
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
	toolRegistry["get_task"] = &toolSpec{
		handler:         getTaskHandler,
		concurrencySafe: false, // 涉及磁盘读取，保守为 unsafe
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
	toolRegistry["list_tasks"] = &toolSpec{
		handler:         listTaskHandler,
		concurrencySafe: false, // 涉及磁盘读取，保守为 unsafe
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
	toolRegistry["connect_mcp"] = &toolSpec{
		handler:         connectMCPHandler,
		concurrencySafe: false, // MCP 连接操作，保守为 unsafe
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
	// task 工具单独注册，避免包级初始化循环
	toolRegistry["task"] = &toolSpec{
		handler:         taskHandler,
		concurrencySafe: false, // 子代理是 global
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
}

func runTool(st *sessionState, name string, input json.RawMessage) string {
	spec, ok := toolRegistry[name]
	if ok {
		return spec.handler(st, input)
	}
	// MCP 外部工具 fallback —— connect_mcp 注册的动态工具不在静态 map 里
	if isMCPTool(name) {
		if st.Mcp == nil {
			return "MCP error: no MCP servers connected — call connect_mcp first"
		}
		server, safeTool, ok := parseMCPName(name)
		if !ok {
			return fmt.Sprintf("MCP error: malformed tool name %q (expected mcp__{server}__{tool})", name)
		}
		client, exists := st.Mcp.Clients()[server]
		if !exists {
			return fmt.Sprintf("MCP error: server %q not connected", server)
		}
		return client.CallTool(safeTool, input)
	}
	return fmt.Sprintf("Error: unknown tool %q", name)
}

func bashHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(args, &in)
	output, newCwd := bash.RunBashWithCwd(in.Command, st.ShellCwd)
	if newCwd != "" {
		st.ShellCwd = newCwd
	}
	return output
}

func readHandler(st *sessionState, args json.RawMessage) string {
	// Offset/Limit 用指针 + flexInt：区分"未提供"与"显式 0"，并容忍字符串数字。
	var in struct {
		Path   string   `json:"path"`
		Offset *flexInt `json:"offset"`
		Limit  *flexInt `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	var offset int
	if in.Offset != nil {
		offset = int(*in.Offset)
	}
	var limit *int
	if in.Limit != nil {
		v := int(*in.Limit)
		limit = &v
	}
	return filesystem.Read(st, session.Workdir(), in.Path, offset, limit)
}

func writeHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments"
	}
	return filesystem.Write(st, session.Workdir(), in.Path, in.Content)
}

func editHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		Path       string `json:"path"`
		OldText    string `json:"old_text"`
		NewText    string `json:"new_text"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments"
	}
	return filesystem.Edit(st, session.Workdir(), in.Path, in.OldText, in.NewText, in.ReplaceAll)
}

func globHandler(_ *sessionState, args json.RawMessage) string {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments"
	}
	return filesystem.Glob(session.Workdir(), in.Pattern, in.Path)
}

func grepHandler(_ *sessionState, args json.RawMessage) string {
	var in filesystem.GrepParams
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments"
	}
	return filesystem.Grep(session.Workdir(), in)
}

// assembleToolPool 每轮 LLM 调用前组装工具池：基础工具 + 已连接 MCP 服务发现的工具。
// 由静态 allTools() 改为接受 sessionState，动态合并 MCP 工具（connect_mcp 之后下一轮生效）。
func assembleToolPool(st *sessionState) []anthropic.ToolUnionParam {
	tool := func(name, desc string, props map[string]any, required []string) anthropic.ToolUnionParam {
		t := anthropic.ToolParam{
			Name:        name,
			Description: anthropic.String(desc),
			InputSchema: anthropic.ToolInputSchemaParam{
				Type:       "object",
				Properties: props,
				Required:   required,
			},
		}
		return anthropic.ToolUnionParam{OfTool: &t}
	}
	tools := []anthropic.ToolUnionParam{
		tool("bash",
			"Run a shell command. For slow commands (dependency installs, builds, long test runs) "+
				"set run_in_background=true: the call returns a task id immediately without blocking, "+
				"and the result is delivered as a <task_notification> in a later turn.",
			map[string]any{
				"command":           map[string]string{"type": "string"},
				"run_in_background": map[string]any{"type": "boolean", "description": "Run the command in the background and return a task id immediately."},
			},
			[]string{"command"}),
		tool("read_file",
			"Read a UTF-8 text file relative to the workspace. Output is line-numbered cat -n style, "+
				"e.g. `     1→content`: line numbers are stable coordinates, NOT file content — never "+
				"include them in edit_file's old_text. Reads are paged, at most 2000 lines by default; "+
				"use offset (1-based starting line) and limit (line count) to page through large files. "+
				"Files over 256KB require explicit offset/limit. A page exceeding the output budget "+
				"returns a short error telling you where to continue — retry with smaller limit/offset; "+
				"content is never silently truncated. Re-reading an unchanged range returns a short notice. "+
				"May run concurrently with other non-conflicting read-only calls in the same response.",
			map[string]any{
				"path":   map[string]string{"type": "string"},
				"offset": map[string]string{"type": "integer", "description": "1-based line number to start reading from. Only provide for large files."},
				"limit":  map[string]string{"type": "integer", "description": "Number of lines to read (default 2000). Only provide for large files; 0 disables the line cap."},
			},
			[]string{"path"}),
		tool("write_file",
			"Write content to a file, overwriting it entirely. If the file already exists, you must "+
				"have read it with read_file first (a full read; a partial page does not count), and the "+
				"file must not have changed since that read — otherwise the write is refused and you must "+
				"read it again. Creating a new file needs no prior read. Content is always written with LF line endings. "+
				"Reads to the same path are serialized relative to this write.",
			map[string]any{
				"path":    map[string]string{"type": "string"},
				"content": map[string]string{"type": "string"},
			},
			[]string{"path", "content"}),
		tool("edit_file",
			"Performs exact string replacement in a file. old_text must match the file content exactly, "+
				"including indentation and whitespace. The edit FAILS unless old_text occurs exactly once in "+
				"the file: provide a larger string with more surrounding context to make it unique, or set "+
				"replace_all=true to change every occurrence (useful for renaming a variable, for instance). "+
				"The file must have been fully read with read_file earlier in the conversation and must not "+
				"have changed since that read, otherwise the edit is refused and you must read it again. "+
				"Reads to the same path are serialized relative to this edit.",
			map[string]any{
				"path":        map[string]string{"type": "string"},
				"old_text":    map[string]string{"type": "string"},
				"new_text":    map[string]string{"type": "string", "description": "The text to replace it with (must be different from old_text)"},
				"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences of old_text (default false)"},
			},
			[]string{"path", "old_text", "new_text"}),
		tool("glob",
			"Find files by pattern. Respects .gitignore. "+
				"Supports ** for recursive matching. Results are relative to the search root. "+
				"May run concurrently with other non-conflicting read-only calls in the same response.",
			map[string]any{
				"pattern": map[string]string{"type": "string", "description": "Glob pattern (e.g. '**/*.go', 'src/*.tsx')"},
				"path":    map[string]string{"type": "string", "description": "Directory to search in (default: workspace root)"},
			},
			[]string{"pattern"}),
		tool("grep",
			"Search file contents with a regex. Respects .gitignore. "+
				"Three output modes: files_with_matches (default, fast — just lists files), "+
				"content (shows matching lines with numbers), count (files:hit count). "+
				"Use glob to filter filenames, e.g. glob='*.py' or glob='**/*.go'. "+
				"Use head_limit/offset to page through large result sets. "+
				"May run concurrently with other non-conflicting read-only calls in the same response.",
			map[string]any{
				"pattern":     map[string]string{"type": "string", "description": "Regex pattern to search for"},
				"path":        map[string]string{"type": "string", "description": "Directory or file to search in (default: workspace root)"},
				"glob":        map[string]string{"type": "string", "description": "Glob filter for filenames (e.g. '*.ts', '**/*.go')"},
				"output_mode": map[string]any{"type": "string", "enum": []string{"files_with_matches", "content", "count"}, "description": "files_with_matches (default): list files that match — fastest. content: show matching lines with numbers. count: show file:hit_count."},
				"head_limit":  map[string]string{"type": "integer", "description": "Max results to return (default 200)"},
				"offset":      map[string]string{"type": "integer", "description": "Skip first N results (for pagination)"},
				"ignore_case": map[string]string{"type": "boolean", "description": "Case-insensitive matching (default false)"},
			},
			[]string{"pattern"}),
		tool("todo_write",
			"Create and manage a task list to plan complex work before executing. "+
				"Use this to break multi-step tasks into ordered items. An update replaces the entire current list — "+
				"include all items, not just changes. Status values: pending (not started), in_progress (exactly one), "+
				"completed (done). At most 20 items per update.",
			map[string]any{
				"todos": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content": map[string]string{"type": "string", "description": "The task description"},
							"status": map[string]any{
								"type":        "string",
								"enum":        []string{"pending", "in_progress", "completed"},
								"description": "Task status: pending (default), in_progress (exactly one at a time), or completed",
							},
						},
					},
				},
			},
			[]string{"todos"}),
		tool("load_skill",
			"Load the full instructions of a skill listed under \"Skills available\" in the system prompt. "+
				"Call this when a skill's description matches the current task, then follow the returned instructions. "+
				"Returns the skill's full SKILL.md text; an unknown name returns the list of available skills.",
			map[string]any{"name": map[string]string{"type": "string", "description": "Skill name as it appears in the catalog"}},
			[]string{"name"}),
		tool("task", "Run a subagent with fresh conversation context and return its final text.",
			map[string]any{"prompt": map[string]string{"type": "string"}},
			[]string{"prompt"}),
		tool("compact",
			"Summarize earlier conversation to free context space. Call this after completing a stage "+
				"when the next stage needs only a summary. The compaction runs after the current tool "+
				"batch completes, so other tool calls in the same response still execute first.",
			map[string]any{},
			[]string{}),
		// 持久化任务图。建图两阶段：先创建全部节点拿到 ID，再用 update_task 加边
		//（同一响应里的兄弟调用无法引用彼此新生成的 ID）。
		tool("create_task",
			"Create a persistent task record in .tasks/ that survives across sessions. "+
				"Returns the full task JSON including its generated id. New tasks start pending with no "+
				"dependencies. To build a task graph, create every task first, then wire dependencies with "+
				"update_task using the returned ids.",
			map[string]any{
				"subject":     map[string]string{"type": "string", "description": "Short imperative summary of the task"},
				"description": map[string]string{"type": "string", "description": "Details needed to resume the task later (optional)"},
			},
			[]string{"subject"}),
		tool("update_task",
			"Add blockedBy dependency edges to a pending, unowned task: it cannot be claimed until every "+
				"dependency is completed. The target and each dependency must exist; self-dependencies and "+
				"cycles are rejected; repeating an existing edge is safe and does not duplicate it.",
			map[string]any{
				"task_id":      map[string]string{"type": "string", "description": "ID returned by create_task"},
				"addBlockedBy": map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "description": "Task IDs this task must wait for"},
			},
			[]string{"task_id"}),
		tool("claim_task",
			"Claim a pending task before working on it: sets owner and moves pending → in_progress. "+
				"Rejected when the task is not pending or any blockedBy dependency is not completed.",
			map[string]any{
				"task_id": map[string]string{"type": "string"},
				"owner":   map[string]string{"type": "string", "description": "Claimer name (default: agent)"},
			},
			[]string{"task_id"}),
		tool("complete_task",
			"Mark an in_progress task owned by you as completed. Reports downstream pending tasks that "+
				"just became unblocked (all their dependencies are now completed).",
			map[string]any{
				"task_id": map[string]string{"type": "string"},
				"owner":   map[string]string{"type": "string", "description": "Must match the claimer (default: agent)"},
			},
			[]string{"task_id"}),
		tool("get_task",
			"Return the full task record as JSON, including description and blockedBy dependency details. "+
				"Use this to recover the exact work to continue after a session restart.",
			map[string]any{"task_id": map[string]string{"type": "string"}},
			[]string{"task_id"}),
		tool("list_tasks",
			"List one-line summaries of all persisted tasks (id, status, subject, owner, dependencies).",
			map[string]any{},
			[]string{}),
		// MCP 外部服务连接工具 —— bring-in 外部工具的入口
		tool("connect_mcp",
			"Connect to an external MCP (Model Context Protocol) server to discover and use additional tools. "+
				"After connecting, the new tools (prefixed mcp__{server}__...) become available starting the next turn. "+
				"Use this when you need capabilities not provided by the built-in tools.",
			map[string]any{
				"name": map[string]string{"type": "string", "description": "Name of the MCP server to connect (e.g. 'docs', 'deploy')"},
			},
			[]string{"name"}),
	}
	// 服务端工具：web_search（联网搜索）与 web_fetch（网页抓取）
	// 由百炼服务端执行，无需客户端 handler，不在 toolRegistry 中注册。
	tools = append(tools,
		anthropic.ToolUnionParam{
			OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{
				MaxUses: param.NewOpt[int64](10),
			},
		},
		anthropic.ToolUnionParam{
			OfWebFetchTool20250910: &anthropic.WebFetchTool20250910Param{
				MaxUses: param.NewOpt[int64](5),
			},
		},
	)
	// 追加已连接 MCP 服务发现的工具（命名 mcp__{server}__{tool}）
	if st != nil && st.Mcp != nil {
		mcpDefs, _ := st.Mcp.AssembleToolDefs()
		tools = append(tools, mcpDefs...)
	}
	return tools
}

// allTools 是兼容包装：只返回基础工具（无 MCP 连接状态）。既有测试走这里；
// 主 loop 与子代理都应使用 assembleToolPool(st)。
func allTools() []anthropic.ToolUnionParam {
	return assembleToolPool(nil)
}
