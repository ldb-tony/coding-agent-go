// ops.go 实现 filesystem 工具的核心操作（read / write / edit / glob / grep）。
package filesystem

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxListResults 是 glob / grep 的最大输出条数。
	MaxListResults = 200
	// DefaultReadLines 是 read_file 未显式给 limit 时的默认分页行数。
	DefaultReadLines = 2000
	// MaxReadFileBytes 是 read_file 读前 stat 的总文件大小门槛。
	MaxReadFileBytes = 256 * 1024
	// MaxReadOutputRunes 是 read_file 实际输出的字符预算。
	MaxReadOutputRunes = 25000
)

// FileUnchangedStub 是重复读取未变化区间时返回的短提示（约 30 token），
// 替代把整份内容再次塞进上下文。
const FileUnchangedStub = "File unchanged since last read. The content from the earlier Read tool_result in this conversation is still current — refer to that instead of re-reading."

// ReadState 记录一次成功读取的视图：用于 mtime 去重，
// 也是 read-before-write/edit 门控的状态基础（Write 与 Edit 共用）。
type ReadState struct {
	Mtime    time.Time
	Start    int  // 返回区间起始行（1-based，含）；空文件为 0
	End      int  // 返回区间结束行（1-based，含）；空文件为 0
	FullView bool // 本次读取是否覆盖整个文件
}

// ReadCache 是会话级读取缓存的抽象。Read/Write/Edit 通过它实现 mtime 去重与
// read-before 门控，无需直接依赖会话状态类型（避免循环导入）。
type ReadCache interface {
	Lookup(abs string) (ReadState, bool)
	Store(abs string, rs ReadState)
}

// searchEng 是进程级搜索引擎实例（SearchEngine 接口的具体实现）。
var searchEng SearchEngine = NewSearchEngine()

// Read 读取 UTF-8 文本文件，返回 cat -n 风格的行号文本。
func Read(cache ReadCache, baseDir, path string, offset int, limit *int) string {
	if offset < 0 {
		return fmt.Sprintf("Error: offset must be >= 1 (1-based starting line), got %d", offset)
	}
	if limit != nil && *limit < 0 {
		return fmt.Sprintf("Error: limit must be >= 0, got %d", *limit)
	}
	abs, err := SafePath(path, baseDir)
	if err != nil {
		return "Error: " + err.Error()
	}

	info, err := os.Stat(abs)
	if err != nil {
		return "Error: " + err.Error()
	}
	if offset == 0 && limit == nil && info.Size() > MaxReadFileBytes {
		return fmt.Sprintf(
			"Error: %s is %d bytes (>%dKB). Read with offset/limit (default page %d lines).",
			path, info.Size(), MaxReadFileBytes/1024, DefaultReadLines)
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

	start, end, fullView := 0, 0, true
	if n > 0 {
		if offset > n {
			return fmt.Sprintf("Error: offset %d is beyond end of file (%d lines)", offset, n)
		}
		start = offset
		end = n
		pageLines := DefaultReadLines
		if limit != nil {
			pageLines = *limit
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

	if r := len([]rune(output)); r > MaxReadOutputRunes {
		msg := fmt.Sprintf(
			"Error: output for lines %d-%d is %d characters, exceeding the %d-character budget. "+
				"Retry with a smaller limit.",
			start, end, r, MaxReadOutputRunes)
		if end < n {
			msg += fmt.Sprintf(" Use offset=%d to continue from the next page.", end+1)
		}
		return msg
	}

	if cache != nil {
		if prev, ok := cache.Lookup(abs); ok &&
			prev.Mtime.Equal(info.ModTime()) &&
			prev.Start == start && prev.End == end && prev.FullView == fullView {
			return FileUnchangedStub
		}
		cache.Store(abs, ReadState{Mtime: info.ModTime(), Start: start, End: end, FullView: fullView})
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

// checkWritable 实现 read-before-write/edit 门控。
func checkWritable(cache ReadCache, abs, path string) (os.FileMode, error) {
	fi, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0o644, nil
		}
		return 0o644, err
	}
	perm := fi.Mode().Perm()
	if cache != nil {
		last, ok := cache.Lookup(abs)
		if !ok || !last.FullView {
			return perm, fmt.Errorf("%s has not been read yet. Read it first before writing to it.", path)
		}
		if fi.ModTime().After(last.Mtime) {
			return perm, fmt.Errorf("%s has been modified since read. Read it again before writing to it.", path)
		}
	}
	return perm, nil
}

// markWritten 在 write/edit 成功落盘后刷新读取状态。
func markWritten(cache ReadCache, abs, content string) {
	if cache == nil {
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
	cache.Store(abs, ReadState{Mtime: fi.ModTime(), Start: start, End: end, FullView: true})
}

// Write 全量覆写文件。
func Write(cache ReadCache, baseDir, path, content string) string {
	abs, err := SafePath(path, baseDir)
	if err != nil {
		return "Error: " + err.Error()
	}
	perm, err := checkWritable(cache, abs, path)
	if err != nil {
		return "Error: " + err.Error()
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "Error: " + err.Error()
	}
	if err := AtomicWriteFile(abs, []byte(content), perm); err != nil {
		return "Error: " + err.Error()
	}
	markWritten(cache, abs, content)
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), path)
}

// Edit 对文件做精确子串替换。
func Edit(cache ReadCache, baseDir, path, oldText, newText string, all bool) string {
	if oldText == "" {
		return "Error: old_text must not be empty"
	}
	if newText == oldText {
		return "Error: new_text must be different from old_text"
	}
	abs, err := SafePath(path, baseDir)
	if err != nil {
		return "Error: " + err.Error()
	}
	perm, err := checkWritable(cache, abs, path)
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
	if err := AtomicWriteFile(abs, []byte(content), perm); err != nil {
		return "Error: " + err.Error()
	}
	markWritten(cache, abs, content)
	return fmt.Sprintf("Edited %s", path)
}

// Glob 在 baseDir（或指定子路径）下按 pattern 查找匹配文件。
func Glob(baseDir, pattern, path string) string {
	if pattern == "" {
		return "Error: pattern is required"
	}
	root := baseDir
	if path != "" {
		abs, err := SafePath(path, baseDir)
		if err != nil {
			return "Error: " + err.Error()
		}
		root = abs
	}
	matches, err := searchEng.Glob(pattern, root)
	if err != nil {
		return "Error: " + err.Error()
	}
	shown := matches
	if len(shown) > MaxListResults {
		shown = append(shown[:MaxListResults],
			fmt.Sprintf("... (more matches omitted; %d total, narrow the pattern)", len(matches)))
	}
	if len(shown) == 0 {
		return "(no matches)"
	}
	return strings.Join(shown, "\n")
}

// GrepParams 是 grep 的完整入参集（handler 解 JSON 用）。
type GrepParams struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	OutputMode string `json:"output_mode"`
	HeadLimit  int    `json:"head_limit"`
	Offset     int    `json:"offset"`
	IgnoreCase bool   `json:"ignore_case"`
}

// Grep 在 baseDir 下用 ripgrep 或内置 walker 执行正则搜索。
func Grep(baseDir string, params GrepParams) string {
	if params.Pattern == "" {
		return "Error: pattern is required"
	}
	if params.OutputMode == "" {
		params.OutputMode = "files_with_matches"
	}
	if params.HeadLimit == 0 {
		params.HeadLimit = MaxListResults
	}
	if params.Path != "" {
		if _, err := SafePath(params.Path, baseDir); err != nil {
			return "Error: " + err.Error()
		}
	}

	var globs []string
	if params.Glob != "" {
		globs = append(globs, params.Glob)
	}

	req := GrepRequest{
		Pattern:     params.Pattern,
		Root:        baseDir,
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
		lines = append(lines, "... (more matches omitted; use a more specific pattern or raise head_limit)")
	}
	if len(lines) == 0 {
		return "(no matches)"
	}
	return strings.Join(lines, "\n")
}
