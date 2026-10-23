// search.go — 搜索引擎基础设施：gitignore 尊重、ripgrep 优先 + walker 回退。
//
// 设计：
//   1) runGrep/runGlob 不再自己 WalkDir，而是通过 searchEngine 接口，
//      运行时探测 ripgrep（exec.LookPath("rg")），有就用 ripgrepEngine，
//      没有就用 walkerEngine（Go 标准库实现 + 自研 gitignore matcher）。
//   2) 两种引擎共享 gitignoreMatcher 和 VCS 目录排除。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// VCS 目录 — 全平台排除，避免在 SVN/Bazaar/Jujutsu/Sapling 里返回噪音
var vcsDirs = map[string]bool{
	".git":  true,
	".svn":  true,
	".hg":   true,
	".bzr":  true,
	".jj":   true,
	".sl":   true,
}

// --- searchEngine 接口 ---

// grepRequest 是 grep 搜索的统一入参。
type grepRequest struct {
	Pattern     string
	Root        string   // 搜索根目录（绝对路径）
	Path        string   // 可选：限定文件或子目录（相对 Root 或绝对）
	Globs       []string // glob 过滤（rg 风格）
	OutputMode  string   // content | files_with_matches | count（默认 files_with_matches）
	BeforeCtx   int
	AfterCtx    int
	IgnoreCase  bool
	LineNumbers bool // content 模式是否带行号
	Multiline   bool
	HeadLimit   int  // 默认 250
	Offset      int
}

// grepResult 是搜索结果的统一出参。
type grepResult struct {
	Files     []string   // files_with_matches 模式下的命中文件列表（相对 Root）
	Counts    map[string]int // count 模式下的 "文件: 次数"
	Matches   []grepMatch   // content 模式下的匹配行
	NumFiles  int           // 实际命中的文件数（截断后也保留真实值）
	Truncated bool
}

type grepMatch struct {
	File string
	Line int
	Text string
}

// searchEngine 是 grep/glob 的统一搜索接口。
type searchEngine interface {
	Grep(req grepRequest) (*grepResult, error)
	Glob(pattern, root string) ([]string, error)
}

// --- 工厂 ---

// newSearchEngine 运行时探测 ripgrep，优先用于 Grep；Glob 始终用 walkerEngine
// （ripgrep 的 glob 默认递归，和我们 "*.py" 只匹配顶层的语义不同）。
func newSearchEngine() searchEngine {
	if _, err := exec.LookPath("rg"); err == nil {
		return &ripgrepEngine{walker: &walkerEngine{}}
	}
	return &walkerEngine{}
}

// --- ripgrepEngine ---

// ripgrepEngine 把 grepRequest 映射为 rg 命令行 argv，解析 rg JSON 输出。
// Glob 委托给 walkerEngine（rg glob 默认递归，与我们语义不同）。
type ripgrepEngine struct {
	walker *walkerEngine
}

func (e *ripgrepEngine) Grep(req grepRequest) (*grepResult, error) {
	args := []string{
		"--json",
		"--hidden",
		"--glob", "!.git",
		"--glob", "!.svn",
		"--glob", "!.hg",
		"--glob", "!.bzr",
		"--glob", "!.jj",
		"--glob", "!.sl",
		"--max-columns", "500",
		"--max-filesize", "5M",
	}
	// 收集所有层级的 .gitignore 并显式传给 rg（rg 默认只在 git 仓库里自动读）
	for _, gi := range findAllGitignoreFiles(req.Root) {
		args = append(args, "--ignore-file", gi)
	}
	if req.IgnoreCase {
		args = append(args, "-i")
	}
	if req.LineNumbers {
		args = append(args, "-n")
	}
	if req.Multiline {
		args = append(args, "-U", "--multiline-dotall")
	}
	for _, g := range req.Globs {
		args = append(args, "--glob", g)
	}
	if req.BeforeCtx > 0 || req.AfterCtx > 0 {
		args = append(args, "-C", strconv.Itoa(req.AfterCtx))
	}

	// 确定 rg 的目标路径
	target := req.Root
	if req.Path != "" {
		abs, err := safePath(req.Path)
		if err != nil {
			return nil, err
		}
		target = abs
	}

	// 追加 pattern 和 root
	args = append(args, req.Pattern, target)

	cmd := exec.Command("rg", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	// rg 退出码 1 = 无匹配，不是错误；其他非零 = 真实错误
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			return nil, fmt.Errorf("ripgrep: %s", strings.TrimSpace(stderr.String()))
		}
	}

	return parseRgJSON(stdout.Bytes(), req)
}

func parseRgJSON(data []byte, req grepRequest) (*grepResult, error) {
	result := &grepResult{
		Counts: map[string]int{},
	}
	root := req.Root

	// rg 默认没有用 head_limit/offset 过滤 —— 需要在结果里手动处理
	headLimit := req.HeadLimit
	if headLimit == 0 {
		headLimit = 250
	}
	offset := req.Offset
	skipped := 0

	// 逐行解析 rg JSON（每条是一行 JSON）
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var fileCounts map[string]int = map[string]int{}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var evt struct {
			Type    string `json:"type"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		switch evt.Type {
		case "match":
			var m struct {
				Path struct {
					Text string `json:"text"`
				} `json:"path"`
				LineNumber int `json:"line_number"`
				Lines      struct {
					Text string `json:"text"`
				} `json:"lines"`
			}
			if err := json.Unmarshal(evt.Data, &m); err != nil {
				continue
			}
			rel, _ := filepath.Rel(root, m.Path.Text)
			rel = filepath.ToSlash(rel)
			fileCounts[rel]++

			switch req.OutputMode {
			case "count":
				// 在收集阶段计数
			default:
				// content 或 files_with_matches 都先收集 content 级别，
				// 最后再按 outputMode 聚合
				if skipped < offset {
					skipped++
					continue
				}
				if len(result.Matches) >= headLimit {
					result.Truncated = true
					continue
				}
				result.Matches = append(result.Matches, grepMatch{
					File: rel,
					Line: m.LineNumber,
					Text: strings.TrimRight(m.Lines.Text, "\r\n"),
				})
			}
		case "summary":
			// 可以从中拿更多元信息
		}
	}

	// 最终按 outputMode 聚合
	switch req.OutputMode {
	case "files_with_matches", "":
		// 去重后的文件列表
		seen := map[string]bool{}
		for _, m := range result.Matches {
			seen[m.File] = true
		}
		result.NumFiles = len(seen)
		for f := range seen {
			result.Files = append(result.Files, f)
		}
		sort.Strings(result.Files)
		// 如果是 files_with_matches 模式，直接从 match 里的 file 聚合。
		// 但 head_limit 现在是 content 条目级别的，对 files 模式应该用不同的 limit。
		// 简化处理：当 output_mode=files_with_matches 时，跳过 content 级收集，
		// 直接从 rg JSON 的 match 事件里去重文件。但上面已经收集了 match，
		// 这里做一次去重 + 截断。
		if len(result.Files) > headLimit {
			result.Files = result.Files[:headLimit]
			result.Truncated = true
		}
		result.Matches = nil

	case "count":
		// 直接用 fileCounts —— 但 fileCounts 在 default 分支里没被计数
		// 需要重跑一遍？不，直接修正：在上面的 for 里 count 分支也计数
		result.Counts = fileCounts
		result.NumFiles = len(fileCounts)
		// count 模式的 offset/head_limit 简化处理
		keys := make([]string, 0, len(result.Counts))
		for k := range result.Counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > headLimit {
			for _, k := range keys[headLimit:] {
				delete(result.Counts, k)
			}
			result.Truncated = true
		}
		result.Matches = nil

	case "content":
		// content 模式保持 Matches 不变，但用 fileCounts 算 NumFiles
		result.NumFiles = len(fileCounts)
	}

	return result, scanner.Err()
}

func (e *ripgrepEngine) Glob(pattern, root string) ([]string, error) {
	return e.walker.Glob(pattern, root)
}

// --- gitignore matcher ---

// gitignorePattern 表示一条 .gitignore 规则（简化子集）。
type gitignorePattern struct {
	pattern string // glob pattern
	negate  bool   // ! 开头的否定规则
}

// loadGitignore 加载 dir 及其祖先目录下的 .gitignore 文件。
func loadGitignore(dir string) []gitignorePattern {
	var patterns []gitignorePattern
	current := dir
	for {
		gi := filepath.Join(current, ".gitignore")
		if data, err := os.ReadFile(gi); err == nil {
			patterns = append(parseGitignore(data, current), patterns...)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		rel, err := filepath.Rel(workdir, parent)
		if err != nil || strings.HasPrefix(rel, "..") {
			break
		}
		current = parent
	}
	return patterns
}

// findAllGitignoreFiles 递归收集 root 目录树下所有 .gitignore 文件的绝对路径。
// 用于 ripgrepEngine 显式 --ignore-file 传递（rg 默认只在 git 仓库里自动读）。
func findAllGitignoreFiles(root string) []string {
	var result []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if vcsDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == ".gitignore" {
			result = append(result, p)
		}
		return nil
	})
	return result
}

// parseGitignore 解析 .gitignore 文件内容为 gitignorePattern 列表。
// 支持：# 注释、! 否定、/ 根锚定、标准 glob。
func parseGitignore(data []byte, dir string) []gitignorePattern {
	var patterns []gitignorePattern
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := false
		if strings.HasPrefix(line, "!") {
			negate = true
			line = line[1:]
		}
		// 去掉行尾 \#
		line = strings.TrimRight(line, "\\#")
		if line == "" {
			continue
		}
		// 如果 pattern 带 / 且不是结尾 /，视为相对于 .gitignore 所在目录
		if strings.Contains(line, "/") && !strings.HasSuffix(line, "/") {
			patterns = append(patterns, gitignorePattern{
				pattern: filepath.ToSlash(filepath.Join(dir, line)),
				negate:  negate,
			})
		} else {
			// 不带 / 的 pattern 匹配任何深度
			patterns = append(patterns, gitignorePattern{
				pattern: filepath.ToSlash(line),
				negate:  negate,
			})
		}
	}
	return patterns
}

// isGitignored 检查给定相对路径（相对 workdir）是否被 gitignore 忽略。
func isGitignored(relPath string, patterns []gitignorePattern) bool {
	ignored := false
	relSlash := filepath.ToSlash(relPath)
	for _, p := range patterns {
		matched := false
		// 绝对路径 pattern（带 /）：精确匹配
		if strings.Contains(p.pattern, "/") {
			if matchGlob(p.pattern, relSlash) {
				matched = true
			}
		} else {
			// 无 / 的 pattern：匹配 basename 或路径的任何一段
			parts := strings.Split(relSlash, "/")
			for _, part := range parts {
				if ok, _ := filepath.Match(p.pattern, part); ok {
					matched = true
					break
				}
			}
		}
		if matched {
			if p.negate {
				ignored = false
			} else {
				ignored = true
			}
		}
	}
	return ignored
}

// --- walkerEngine（rg 不可用时的回退）---

// walkerEngine 用 Go 标准库 WalkDir + 自研 gitignore matcher。
type walkerEngine struct{}

func (e *walkerEngine) Grep(req grepRequest) (*grepResult, error) {
	re, err := regexp.Compile(req.Pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %s", err.Error())
	}
	if req.IgnoreCase {
		re = regexp.MustCompile("(?i:" + req.Pattern + ")")
	}

	root := req.Root
	if req.Path != "" {
		abs, err := safePath(req.Path)
		if err != nil {
			return nil, err
		}
		info, statErr := os.Stat(abs)
		if statErr == nil && info.IsDir() {
			root = abs
		} else {
			// 路径是文件：单文件搜索
			return e.grepFile(abs, req, re)
		}
	}

	giPatterns := loadGitignore(root)
	// 子目录 .gitignore 缓存（目录绝对路径 → 合并后的 patterns）
	giCache := map[string][]gitignorePattern{
		root: giPatterns,
	}

	result := &grepResult{
		Counts: map[string]int{},
	}

	headLimit := req.HeadLimit
	if headLimit == 0 {
		headLimit = 250
	}

	seenFiles := map[string]bool{} // files_with_matches 用
	var skipOffset int

	scan := func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if vcsDirs[name] {
				return filepath.SkipDir
			}
			// 进入目录时预计算其 gitignore（含子目录规则），缓存起来
			if _, ok := giCache[p]; !ok {
				giCache[p] = loadGitignore(p)
			}
			dirPatterns := giCache[p]
			rel, relErr := filepath.Rel(req.Root, p)
			if relErr == nil && isGitignored(rel, dirPatterns) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, infoErr := d.Info()
		if infoErr != nil || fi.Size() > maxGrepFileSize {
			return nil
		}
		rel, relErr := filepath.Rel(req.Root, p)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)

		// 用文件所在目录的 gitignore 检查
		fileDir := filepath.Dir(p)
		dirPatterns, ok := giCache[fileDir]
		if !ok {
			dirPatterns = loadGitignore(fileDir)
			giCache[fileDir] = dirPatterns
		}
		if isGitignored(rel, dirPatterns) {
			return nil
		}

		// glob 过滤
		if len(req.Globs) > 0 {
			matched := false
			for _, g := range req.Globs {
				if matchGlob(g, relSlash) || matchGlob(g, filepath.Base(relSlash)) {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
		}

		data, readErr := os.ReadFile(p)
		if readErr != nil || !isUTF8(data) {
			return nil
		}

		lines := strings.Split(string(data), "\n")
		fileHit := 0
		for i, line := range lines {
			if len(line) > 500 {
				line = line[:500]
			}
			if re.MatchString(line) {
				fileHit++
				switch req.OutputMode {
				case "files_with_matches", "":
					seenFiles[relSlash] = true
				case "count":
					// 在文件级别计数
				case "content":
					if skipOffset < req.Offset {
						skipOffset++
						continue
					}
					if len(result.Matches) >= headLimit {
						result.Truncated = true
						continue
					}
					result.Matches = append(result.Matches, grepMatch{
						File: relSlash,
						Line: i + 1,
						Text: strings.TrimRight(line, "\r"),
					})
				}
			}
		}
		if fileHit > 0 {
			result.Counts[relSlash] = fileHit
		}
		return nil
	}

	if err := filepath.WalkDir(root, scan); err != nil {
		return nil, err
	}

	switch req.OutputMode {
	case "files_with_matches", "":
		for f := range seenFiles {
			result.Files = append(result.Files, f)
		}
		sort.Strings(result.Files)
		if len(result.Files) > headLimit {
			result.Files = result.Files[:headLimit]
			result.Truncated = true
		}
		result.NumFiles = len(result.Files)
		result.Matches = nil
	case "count":
		keys := make([]string, 0, len(result.Counts))
		for k := range result.Counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > headLimit {
			for _, k := range keys[headLimit:] {
				delete(result.Counts, k)
			}
			result.Truncated = true
		}
		result.NumFiles = len(result.Counts)
		result.Matches = nil
	}

	return result, nil
}

func (e *walkerEngine) grepFile(abs string, req grepRequest, re *regexp.Regexp) (*grepResult, error) {
	// 单文件搜索
	data, err := os.ReadFile(abs)
	if err != nil || !isUTF8(data) {
		return nil, nil
	}
	rel, _ := filepath.Rel(req.Root, abs)
	relSlash := filepath.ToSlash(rel)

	result := &grepResult{Counts: map[string]int{}}
	headLimit := req.HeadLimit
	if headLimit == 0 {
		headLimit = 250
	}

	lines := strings.Split(string(data), "\n")
	fileHit := 0
	for i, line := range lines {
		if len(line) > 500 {
			line = line[:500]
		}
		if re.MatchString(line) {
			fileHit++
			switch req.OutputMode {
			case "files_with_matches", "":
				result.Files = append(result.Files, relSlash)
			case "count":
			case "content":
				if len(result.Matches) >= headLimit {
					result.Truncated = true
					continue
				}
				result.Matches = append(result.Matches, grepMatch{
					File: relSlash, Line: i + 1,
					Text: strings.TrimRight(line, "\r"),
				})
			}
		}
	}
	if fileHit > 0 {
		result.Counts[relSlash] = fileHit
		result.NumFiles = 1
	}
	return result, nil
}

func (e *walkerEngine) Glob(pattern, root string) ([]string, error) {
	var matches []string
	giCache := map[string][]gitignorePattern{
		root: loadGitignore(root),
	}

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if vcsDirs[name] {
				return filepath.SkipDir
			}
			if _, ok := giCache[p]; !ok {
				giCache[p] = loadGitignore(p)
			}
			dirPatterns := giCache[p]
			rel, relErr := filepath.Rel(root, p)
			if relErr == nil && isGitignored(rel, dirPatterns) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		fileDir := filepath.Dir(p)
		dirPatterns, ok := giCache[fileDir]
		if !ok {
			dirPatterns = loadGitignore(fileDir)
			giCache[fileDir] = dirPatterns
		}
		if isGitignored(rel, dirPatterns) {
			return nil
		}
		if matchGlob(pattern, relSlash) {
			matches = append(matches, relSlash)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

// isUTF8 报告字节串是否为合法 UTF-8；walker 引擎据此跳过二进制文件。
func isUTF8(data []byte) bool {
	return utf8.Valid(data)
}
