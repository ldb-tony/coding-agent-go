package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
)

// workdir 是文件工具的根目录，路径越界（../ 逃逸）会被 safePath 拒绝。
var workdir = mustWorkdir()

func mustWorkdir() string {
	wd, err := os.Getwd()
	if err != nil {
		log.Fatal("get working directory: ", err)
	}
	return wd
}

const (
	maxListResults     = 200             // glob / grep 的最大输出条数
	defaultReadLines   = 2000            // read_file 未显式给 limit 时的默认分页行数
	maxReadFileBytes   = 256 * 1024      // read_file 读前 stat 的总文件大小门槛
	maxReadOutputRunes = 25000           // read_file 实际输出的字符预算
	maxGrepFileSize    = 5 * 1024 * 1024 // grep 跳过超过该大小的文件
)

// fileUnchangedStub 是重复读取未变化区间时返回的短提示（约 30 token），
// 替代把整份内容再次塞进上下文。
const fileUnchangedStub = "File unchanged since last read. The content from the earlier Read tool_result in this conversation is still current — refer to that instead of re-reading."

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

// readState 记录一次成功读取的视图：用于 mtime 去重，
// 也是 read-before-write/edit 门控的状态基础（Write 与 Edit 共用）。
type readState struct {
	mtime    time.Time
	start    int  // 返回区间起始行（1-based，含）；空文件为 0
	end      int  // 返回区间结束行（1-based，含）；空文件为 0
	fullView bool // 本次读取是否覆盖整个文件
}

// sessionState 是会话级状态，挂在 agentLoop 上而不是全局变量。
type sessionState struct {
	reads            map[string]readState // absPath -> 最近一次成功读取
	readsMu          sync.Mutex           // 保护 reads map 的并发访问
	todo             *todoManager         // 会话级待办列表
	tasks            *taskStore           // .tasks/ 持久化任务图（跨会话）
	skills           *skillLoader         // 会话启动时扫描出的技能注册表
	memory           *memoryStore         // 跨会话记忆存储（.memory/）
	recalledMemories string               // 会话开始时召回的相关记忆全文（进 system prompt）
	client           LLMClient            // 子代理的嵌套 loop 复用父的 LLM 客户端
	ctx              context.Context      // 子代理继承父 loop 的 context（超时/取消）
	shellCwd         string               // bash cwd 持久化（跨调用保持工作目录）
	mcp              *mcpRegistry         // 已连接 MCP 服务注册表（首次 connect_mcp 时初始化）
}

func newSessionState() *sessionState {
	skills := newSkillLoader(filepath.Join(workdir, "skills"))
	skills.scan()
	return &sessionState{
		reads:  map[string]readState{},
		todo:   newTodoManager(),
		tasks:  newTaskStore(filepath.Join(workdir, tasksDirName)), // 任务目录随 workdir
		skills: skills,
		memory: newMemoryStore(filepath.Join(workdir, memoryDirName)), // 记忆目录随 workdir
	}
}

// safePath 把路径解析为绝对路径，并确保它位于 workdir 内（对齐参考实现的 safe_path）。
func safePath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workdir, path)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(workdir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("access denied: path escapes workspace: %s", path)
	}
	return abs, nil
}

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
			if abs, err := safePath(in.Path); err == nil {
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
			if abs, err := safePath(in.Path); err == nil {
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
			if abs, err := safePath(in.Path); err == nil {
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
				searchRoot = workdir
			}
			if abs, err := safePath(searchRoot); err == nil {
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
				searchRoot = workdir
			}
			if abs, err := safePath(searchRoot); err == nil {
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
		handler:         compactHandler,
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
		if st.mcp == nil {
			return "MCP error: no MCP servers connected — call connect_mcp first"
		}
		server, safeTool, ok := parseMCPName(name)
		if !ok {
			return fmt.Sprintf("MCP error: malformed tool name %q (expected mcp__{server}__{tool})", name)
		}
		client, exists := st.mcp.clients[server]
		if !exists {
			return fmt.Sprintf("MCP error: server %q not connected", server)
		}
		return client.callTool(safeTool, input)
	}
	return fmt.Sprintf("Error: unknown tool %q", name)
}

func bashHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(args, &in)
	output, newCwd := runBashWithCwd(in.Command, st.shellCwd)
	if newCwd != "" {
		st.shellCwd = newCwd
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
	return runRead(st, in.Path, offset, limit)
}

func writeHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments"
	}
	return runWrite(st, in.Path, in.Content)
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
	return runEdit(st, in.Path, in.OldText, in.NewText, in.ReplaceAll)
}

func globHandler(_ *sessionState, args json.RawMessage) string {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments"
	}
	return runGlob(in.Pattern, in.Path)
}

func grepHandler(_ *sessionState, args json.RawMessage) string {
	var in runGrepParams
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments"
	}
	return runGrep(in)
}

// runRead 读取 UTF-8 文本文件，返回 cat -n 风格的行号文本。
//
// 分页协议：offset 为 1-based 起始行（入参 0 表示未提供）；limit == nil 表示模型
// 未显式给值，默认读 defaultReadLines 行。未带任何分页信号且文件超过
// maxReadFileBytes 时读前直接返回短错误；带 offset 或 limit 的显式分页请求绕过
// 字节门槛（保证短错误提示的 offset 重试路径不会二次撞墙）；显式 limit=0 是
// "逃生舱"（不限行数），但仍受 maxReadOutputRunes 预算约束。任何超限都返回
// 指导分页的短错误，绝不静默截断。
func runRead(st *sessionState, path string, offset int, limit *int) string {
	if offset < 0 {
		return fmt.Sprintf("Error: offset must be >= 1 (1-based starting line), got %d", offset)
	}
	if limit != nil && *limit < 0 {
		return fmt.Sprintf("Error: limit must be >= 0, got %d", *limit)
	}
	abs, err := safePath(path)
	if err != nil {
		return "Error: " + err.Error()
	}

	info, err := os.Stat(abs)
	if err != nil {
		return "Error: " + err.Error()
	}
	// 预算第一层：读前 stat 总大小门槛。未显式分页（既无 offset 也无 limit）的
	// 大文件快速失败，短错误比 25K token 的截断切片便宜；模型带 offset/limit
	// 任一信号重试即放行，由第二层输出预算兜底。
	if offset == 0 && limit == nil && info.Size() > maxReadFileBytes {
		return fmt.Sprintf(
			"Error: %s is %d bytes (>%dKB). Read with offset/limit (default page %d lines).",
			path, info.Size(), maxReadFileBytes/1024, defaultReadLines)
	}
	if offset == 0 {
		offset = 1
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return "Error: " + err.Error()
	}
	if !utf8.Valid(data) {
		return fmt.Sprintf("Error: %s is not a UTF-8 text file", path)
	}

	var lines []string
	if trimmed := strings.TrimRight(string(data), "\n"); trimmed != "" {
		lines = strings.Split(trimmed, "\n")
	}
	n := len(lines)

	// 返回区间 [start, end]，均为 1-based 含端点；空文件为 (0, 0)。
	start, end, fullView := 0, 0, true
	if n > 0 {
		if offset > n {
			return fmt.Sprintf("Error: offset %d is beyond end of file (%d lines)", offset, n)
		}
		start = offset
		end = n
		pageLines := defaultReadLines // nil limit：默认页
		if limit != nil {
			pageLines = *limit // 显式 0 = 不限行数（逃生舱）
		}
		if pageLines > 0 {
			if e := offset - 1 + pageLines; e < end {
				end = e
			}
		}
		fullView = start == 1 && end == n
	}

	var b strings.Builder
	if n > 0 {
		width := lineNumberWidth(n)
		for i := start; i <= end; i++ {
			fmt.Fprintf(&b, "%*d→%s\n", width, i, lines[i-1])
		}
	}
	output := strings.TrimRight(b.String(), "\n")

	// 预算第二层：实际输出字符预算，超限返回错误而非内容（破坏性变更）。
	if r := len([]rune(output)); r > maxReadOutputRunes {
		msg := fmt.Sprintf(
			"Error: output for lines %d-%d is %d characters, exceeding the %d-character budget. "+
				"Retry with a smaller limit.",
			start, end, r, maxReadOutputRunes)
		if end < n {
			msg += fmt.Sprintf(" Use offset=%d to continue from the next page.", end+1)
		}
		return msg
	}

	// mtime 去重：同一文件、同一区间、mtime 未变 → 返回约 30 token 的 stub。
	// 区间不同（例如翻页）不命中，保证 offset 分页永远拿得到内容。
	if st != nil {
		st.readsMu.Lock()
		if prev, ok := st.reads[abs]; ok &&
			prev.mtime.Equal(info.ModTime()) &&
			prev.start == start && prev.end == end && prev.fullView == fullView {
			st.readsMu.Unlock()
			return fileUnchangedStub
		}
		st.reads[abs] = readState{mtime: info.ModTime(), start: start, end: end, fullView: fullView}
		st.readsMu.Unlock()
	}
	return output
}

// lineNumberWidth 返回行号字段宽度（按文件总行数自适应，同一文件翻页时宽度稳定）。
func lineNumberWidth(totalLines int) int {
	w := 1
	for totalLines >= 10 {
		totalLines /= 10
		w++
	}
	return w
}

// checkWritable 实现 read-before-write/edit 门控（Write 与 Edit 共用）：
// 已存在的文件在本会话中必须有全量读取记录（部分读不算），且读取之后 mtime
// 未变——变了说明用户/linter/并行工具改动过，需重新 Read；新建文件（ENOENT）
// 豁免门控。返回落盘应使用的权限位（已存在文件保留原权限位，新建 0o644）。
func checkWritable(st *sessionState, abs, path string) (os.FileMode, error) {
	fi, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0o644, nil // 新建文件，豁免门控
		}
		return 0o644, err
	}
	perm := fi.Mode().Perm()
	if st != nil {
		st.readsMu.Lock()
		last, ok := st.reads[abs]
		st.readsMu.Unlock()
		if !ok || !last.fullView {
			return perm, fmt.Errorf("%s has not been read yet. Read it first before writing to it.", path)
		}
		if fi.ModTime().After(last.mtime) {
			return perm, fmt.Errorf("%s has been modified since read. Read it again before writing to it.", path)
		}
	}
	return perm, nil
}

// markWritten 在 write/edit 成功落盘后刷新读取状态：落盘内容即本会话的最新认知，
// 允许连续写入/编辑，也让后续 Read 的 mtime 去重正确命中。区间计算与 runRead 保持一致。
func (st *sessionState) markWritten(abs, content string) {
	if st == nil {
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return
	}
	start, end := 0, 0
	if trimmed := strings.TrimRight(content, "\n"); trimmed != "" {
		start, end = 1, len(strings.Split(trimmed, "\n"))
	}
	st.readsMu.Lock()
	st.reads[abs] = readState{mtime: fi.ModTime(), start: start, end: end, fullView: true}
	st.readsMu.Unlock()
}

// runWrite 全量覆写文件。内容强制以 LF 行尾落盘，消除 CRLF 噪音 diff；
// 写入走同目录临时文件 + rename，目标路径永远不会是半截状态。
// read-before 门控语义见 checkWritable。
func runWrite(st *sessionState, path, content string) string {
	abs, err := safePath(path)
	if err != nil {
		return "Error: " + err.Error()
	}
	perm, err := checkWritable(st, abs, path)
	if err != nil {
		return "Error: " + err.Error()
	}

	// 强制 LF：CRLF 与孤立 CR 统一归一为 \n。
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "Error: " + err.Error()
	}
	if err := atomicWriteFile(abs, []byte(content), perm); err != nil {
		return "Error: " + err.Error()
	}

	st.markWritten(abs, content)
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), path)
}

// atomicWriteFile 先写同目录临时文件再 rename，保证目标路径要么旧内容完整、
// 要么新内容完整（WriteFile 直接 O_TRUNC，写一半崩溃会留下损坏文件）。
func atomicWriteFile(abs string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(abs), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp) // 失败路径清理临时文件
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, abs); err != nil {
		return err
	}
	tmp = "" // rename 成功，取消清理
	return nil
}

// runEdit 对文件做精确子串替换，对齐 Claude Code Edit 的语义：
// old_text 必须与文件内容精确匹配（含缩进与空白），且在文件中唯一（恰好 1 处）——
// 0 处或多处均失败，失败文案本身携带恢复指引（扩上下文 / replace_all=true），
// 让模型在同一轮次内自我纠正；replace_all=true 显式声明替换全部匹配（重命名场景）。
// 与 Write 共用 read-before 门控：编辑前本会话必须全量读过该文件且读后未被外部改动，
// 落盘走原子写并保留原权限位。
func runEdit(st *sessionState, path, oldText, newText string, all bool) string {
	if oldText == "" {
		return "Error: old_text must not be empty"
	}
	if newText == oldText {
		return "Error: new_text must be different from old_text"
	}
	abs, err := safePath(path)
	if err != nil {
		return "Error: " + err.Error()
	}
	perm, err := checkWritable(st, abs, path)
	if err != nil {
		return "Error: " + err.Error()
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "Error: " + err.Error()
	}
	content := string(data)
	count := strings.Count(content, oldText)
	switch {
	case count == 0:
		return fmt.Sprintf("Error: text not found in %s — the file may have been modified since it was read; read it again", path)
	case count > 1 && !all:
		return fmt.Sprintf("Error: old_text is not unique in %s (%d matches). Add surrounding context to make it unique, or set replace_all=true to replace every occurrence", path, count)
	}
	if all {
		content = strings.ReplaceAll(content, oldText, newText)
	} else {
		content = strings.Replace(content, oldText, newText, 1)
	}
	if err := atomicWriteFile(abs, []byte(content), perm); err != nil {
		return "Error: " + err.Error()
	}
	st.markWritten(abs, content)
	return fmt.Sprintf("Edited %s", path)
}

// searchEng 是进程级搜索引擎实例（searchEngine 接口的具体实现）。
var searchEng searchEngine = newSearchEngine()

func runGlob(pattern string, path string) string {
	if pattern == "" {
		return "Error: pattern is required"
	}
	root := workdir
	if path != "" {
		abs, err := safePath(path)
		if err != nil {
			return "Error: " + err.Error()
		}
		root = abs
	}

	matches, err := searchEng.Glob(pattern, root)
	if err != nil {
		return "Error: " + err.Error()
	}

	// 限制结果条数
	shown := matches
	if len(shown) > maxListResults {
		shown = append(shown[:maxListResults],
			fmt.Sprintf("... (more matches omitted; %d total, narrow the pattern)", len(matches)))
	}
	if len(shown) == 0 {
		return "(no matches)"
	}
	return strings.Join(shown, "\n")
}

// runGrepParams 是 grep 的完整入参集（handler 解 JSON 用）。
type runGrepParams struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`        // 替代旧的 include
	OutputMode string `json:"output_mode"` // content | files_with_matches | count
	HeadLimit  int    `json:"head_limit"`
	Offset     int    `json:"offset"`
	IgnoreCase bool   `json:"ignore_case"`
}

func runGrep(params runGrepParams) string {
	if params.Pattern == "" {
		return "Error: pattern is required"
	}
	if params.OutputMode == "" {
		params.OutputMode = "files_with_matches" // 默认：先定位文件
	}
	if params.HeadLimit == 0 {
		params.HeadLimit = maxListResults // 默认上限
	}

	// root 始终是 workdir（用于相对路径计算），path 单独作为搜索目标
	// 如果 path 非空，它可以是文件或子目录
	if params.Path != "" {
		if _, err := safePath(params.Path); err != nil {
			return "Error: " + err.Error()
		}
	}

	var globs []string
	if params.Glob != "" {
		globs = append(globs, params.Glob)
	}

	req := grepRequest{
		Pattern:     params.Pattern,
		Root:        workdir, // 始终用 workdir 做 rel 计算基准
		Path:        params.Path,
		Globs:       globs,
		OutputMode:  params.OutputMode,
		HeadLimit:   params.HeadLimit,
		Offset:      params.Offset,
		IgnoreCase:  params.IgnoreCase,
		LineNumbers: true,
	}

	result, err := searchEng.Grep(req)
	if err != nil {
		// ripgrep 解析失败可能是非法正则，包装成可读错误
		if strings.Contains(err.Error(), "ripgrep") {
			return "Error: " + strings.Replace(err.Error(), "ripgrep: ", "", 1)
		}
		return "Error: " + err.Error()
	}

	var lines []string
	switch params.OutputMode {
	case "files_with_matches":
		lines = result.Files
	case "count":
		keys := make([]string, 0, len(result.Counts))
		for k := range result.Counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			lines = append(lines, fmt.Sprintf("%s:%d", k, result.Counts[k]))
		}
	case "content":
		// 按文件路径 + 行号排序（rg 不保证字典序）
		sort.Slice(result.Matches, func(i, j int) bool {
			if result.Matches[i].File != result.Matches[j].File {
				return result.Matches[i].File < result.Matches[j].File
			}
			return result.Matches[i].Line < result.Matches[j].Line
		})
		for _, m := range result.Matches {
			lines = append(lines, fmt.Sprintf("%s:%d:%s", m.File, m.Line, m.Text))
		}
	}

	if result.Truncated {
		trunc := fmt.Sprintf("... (more matches omitted; use a more specific pattern or raise head_limit)")
		lines = append(lines, trunc)
	}

	if len(lines) == 0 {
		return "(no matches)"
	}
	return strings.Join(lines, "\n")
}

// matchGlob 支持跨目录的 **（Go 标准库 filepath.Glob 不支持 **）。
func matchGlob(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if matchSegs(pat[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, _ := filepath.Match(pat[0], name[0])
	if !ok {
		return false
	}
	return matchSegs(pat[1:], name[1:])
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
	// 追加已连接 MCP 服务发现的工具（命名 mcp__{server}__{tool}）
	if st != nil && st.mcp != nil {
		mcpDefs, _ := st.mcp.assembleToolDefs()
		tools = append(tools, mcpDefs...)
	}
	return tools
}

// allTools 是兼容包装：只返回基础工具（无 MCP 连接状态）。既有测试走这里；
// 主 loop 与子代理都应使用 assembleToolPool(st)。
func allTools() []anthropic.ToolUnionParam {
	return assembleToolPool(nil)
}
