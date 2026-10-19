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
	"regexp"
	"sort"
	"strconv"
	"strings"
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
// 也是后续 read-before-write 门控的状态基础（与 Write 同期启用）。
type readState struct {
	mtime    time.Time
	start    int  // 返回区间起始行（1-based，含）；空文件为 0
	end      int  // 返回区间结束行（1-based，含）；空文件为 0
	fullView bool // 本次读取是否覆盖整个文件
}

// sessionState 是会话级状态，挂在 agentLoop 上而不是全局变量。
type sessionState struct {
	reads  map[string]readState // absPath -> 最近一次成功读取
	todo   *todoManager         // s05: 会话级待办列表
	skills *skillLoader         // s07: 会话启动时扫描出的技能注册表
	client LLMClient            // s06: 子代理的嵌套 loop 复用父的 LLM 客户端
	ctx    context.Context      // s06: 子代理继承父 loop 的 context（超时/取消）
}

func newSessionState() *sessionState {
	// s07：技能目录在会话启动时扫描一次，之后 load_skill 只查内存注册表。
	skills := newSkillLoader(filepath.Join(workdir, "skills"))
	skills.scan()
	return &sessionState{
		reads:  map[string]readState{},
		todo:   newTodoManager(),
		skills: skills,
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

// TOOL_HANDLERS：工具名 → 处理函数。加一个工具 = 这里加一行映射。
var toolHandlers = map[string]toolHandler{
	"bash":       bashHandler,
	"read_file":  readHandler,
	"write_file": writeHandler,
	"edit_file":  editHandler,
	"glob":       globHandler,
	"grep":       grepHandler,
	"todo_write": todoWriteHandler, // s05: 待办规划
	"load_skill": loadSkillHandler, // s07: 技能按需加载
}

// task 的处理器在 init 里注册：taskHandler 经 runSubagent 复用 runTool，而 runTool
// 读取 toolHandlers —— 若在 map 字面量里直接引用 taskHandler，会构成包级初始化循环
// （initialization cycle）。init 阶段赋值可打破该循环。
func init() {
	toolHandlers["task"] = taskHandler
}

func runTool(st *sessionState, name string, input json.RawMessage) string {
	handler, ok := toolHandlers[name]
	if !ok {
		return fmt.Sprintf("Error: unknown tool %q", name)
	}
	return handler(st, input)
}

func bashHandler(_ *sessionState, args json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(args, &in)
	return runBash(in.Command)
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

func editHandler(_ *sessionState, args json.RawMessage) string {
	var in struct {
		Path    string `json:"path"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments"
	}
	return runEdit(in.Path, in.OldText, in.NewText)
}

func globHandler(_ *sessionState, args json.RawMessage) string {
	var in struct {
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments"
	}
	return runGlob(in.Pattern)
}

func grepHandler(_ *sessionState, args json.RawMessage) string {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Include string `json:"include"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments"
	}
	return runGrep(in.Pattern, in.Path, in.Include)
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
		if prev, ok := st.reads[abs]; ok &&
			prev.mtime.Equal(info.ModTime()) &&
			prev.start == start && prev.end == end && prev.fullView == fullView {
			return fileUnchangedStub
		}
		st.reads[abs] = readState{mtime: info.ModTime(), start: start, end: end, fullView: fullView}
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

// runWrite 全量覆写文件。对齐 Claude Code Write 的门控语义：
// 覆盖已存在文件前，本会话必须有该文件的全量读取记录（部分读不算），
// 且读取之后 mtime 未变（变了说明用户/linter/并行工具改动过，需重新 Read）；
// 新建文件豁免门控。内容强制以 LF 行尾落盘，消除 CRLF 噪音 diff。
// 写入走同目录临时文件 + rename，目标路径永远不会是半截状态。
func runWrite(st *sessionState, path, content string) string {
	abs, err := safePath(path)
	if err != nil {
		return "Error: " + err.Error()
	}

	perm := os.FileMode(0o644)
	if fi, err := os.Stat(abs); err == nil {
		perm = fi.Mode().Perm() // 覆写保留原文件权限位
		if st != nil {
			last, ok := st.reads[abs]
			if !ok || !last.fullView {
				return fmt.Sprintf("Error: %s has not been read yet. Read it first before writing to it.", path)
			}
			if fi.ModTime().After(last.mtime) {
				return fmt.Sprintf("Error: %s has been modified since read. Read it again before writing to it.", path)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "Error: " + err.Error()
	} // ENOENT → 新建文件，豁免门控

	// 强制 LF：CRLF 与孤立 CR 统一归一为 \n。
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "Error: " + err.Error()
	}
	if err := atomicWriteFile(abs, []byte(content), perm); err != nil {
		return "Error: " + err.Error()
	}

	// 写后刷新读取状态：刚写入的内容即本会话的最新认知，允许连续 Write，
	// 也让后续 Read 的 mtime 去重正确命中。区间计算与 runRead 保持一致。
	if st != nil {
		if fi, err := os.Stat(abs); err == nil {
			start, end := 0, 0
			if trimmed := strings.TrimRight(content, "\n"); trimmed != "" {
				start, end = 1, len(strings.Split(trimmed, "\n"))
			}
			st.reads[abs] = readState{mtime: fi.ModTime(), start: start, end: end, fullView: true}
		}
	}
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

func runEdit(path, oldText, newText string) string {
	if oldText == "" {
		return "Error: old_text must not be empty"
	}
	abs, err := safePath(path)
	if err != nil {
		return "Error: " + err.Error()
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "Error: " + err.Error()
	}
	text := string(data)
	if !strings.Contains(text, oldText) {
		return fmt.Sprintf("Error: text not found in %s", path)
	}
	if err := os.WriteFile(abs, []byte(strings.Replace(text, oldText, newText, 1)), 0o644); err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("Edited %s", path)
}

func runGlob(pattern string) string {
	if pattern == "" {
		return "Error: pattern is required"
	}
	var matches []string
	err := filepath.WalkDir(workdir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过无权限等读不了的路径
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(workdir, p)
		if relErr != nil {
			return nil
		}
		if matchGlob(pattern, filepath.ToSlash(rel)) {
			matches = append(matches, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return "Error: " + err.Error()
	}
	sort.Strings(matches)
	shown := matches
	if len(shown) > maxListResults {
		shown = append(shown[:maxListResults], "... (more matches omitted; narrow the pattern)")
	}
	if len(shown) == 0 {
		return "(no matches)"
	}
	return strings.Join(shown, "\n")
}

func runGrep(pattern, path, include string) string {
	if pattern == "" {
		return "Error: pattern is required"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "Error: invalid regex: " + err.Error()
	}
	root := workdir
	if path != "" {
		abs, err := safePath(path)
		if err != nil {
			return "Error: " + err.Error()
		}
		root = abs
	}
	info, err := os.Stat(root)
	if err != nil {
		return "Error: " + err.Error()
	}

	var out []string
	appendMatch := func(rel string, lineNo int, line string) bool {
		out = append(out, fmt.Sprintf("%s:%d:%s", rel, lineNo, strings.TrimRight(line, "\r")))
		return len(out) >= maxListResults
	}

	scan := func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if fi, err := d.Info(); err != nil || fi.Size() > maxGrepFileSize {
			return nil
		}
		rel, relErr := filepath.Rel(workdir, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if include != "" && !matchGlob(include, filepath.Base(rel)) && !matchGlob(include, rel) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || !utf8.Valid(data) {
			return nil // 跳过读不了或二进制文件
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) && appendMatch(rel, i+1, line) {
				out = append(out, "... (more matches omitted)")
				return filepath.SkipAll
			}
		}
		return nil
	}

	// root 是单个文件时 WalkDir 也会正确调用一次 scan
	if info.IsDir() {
		if err := filepath.WalkDir(root, scan); err != nil {
			return "Error: " + err.Error()
		}
	} else {
		if err := scan(root, fs.FileInfoToDirEntry(info), nil); err != nil {
			return "Error: " + err.Error()
		}
	}
	if len(out) == 0 {
		return "(no matches)"
	}
	return strings.Join(out, "\n")
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

// allTools 返回全部工具定义。加一个工具 = 这里加一个条目。
func allTools() []anthropic.ToolUnionParam {
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
	return []anthropic.ToolUnionParam{
		tool("bash", "Run a shell command.",
			map[string]any{"command": map[string]string{"type": "string"}},
			[]string{"command"}),
		tool("read_file",
			"Read a UTF-8 text file relative to the workspace. Output is line-numbered cat -n style, "+
				"e.g. `     1→content`: line numbers are stable coordinates, NOT file content — never "+
				"include them in edit_file's old_text. Reads are paged, at most 2000 lines by default; "+
				"use offset (1-based starting line) and limit (line count) to page through large files. "+
				"Files over 256KB require explicit offset/limit. A page exceeding the output budget "+
				"returns a short error telling you where to continue — retry with smaller limit/offset; "+
				"content is never silently truncated. Re-reading an unchanged range returns a short notice.",
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
				"read it again. Creating a new file needs no prior read. Content is always written with LF line endings.",
			map[string]any{
				"path":    map[string]string{"type": "string"},
				"content": map[string]string{"type": "string"},
			},
			[]string{"path", "content"}),
		tool("edit_file", "Replace text in a file once (first occurrence).",
			map[string]any{
				"path":     map[string]string{"type": "string"},
				"old_text": map[string]string{"type": "string"},
				"new_text": map[string]string{"type": "string"},
			},
			[]string{"path", "old_text", "new_text"}),
		tool("glob", "Find files by pattern relative to the workspace (supports **).",
			map[string]any{"pattern": map[string]string{"type": "string"}},
			[]string{"pattern"}),
		tool("grep", "Search file contents with a regex. Optional path to search in, optional glob filter for filenames.",
			map[string]any{
				"pattern": map[string]string{"type": "string"},
				"path":    map[string]string{"type": "string"},
				"include": map[string]string{"type": "string"},
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
	}
}
