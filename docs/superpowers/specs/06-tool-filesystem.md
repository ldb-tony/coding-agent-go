# 06 - 文件操作工具规格

## 1. 职责

文件操作工具提供 5 个工具，覆盖文件的读取、写入、编辑和内容/名称搜索：

1. **read_file** — 读取 UTF-8 文本文件，行号格式化输出，支持分页
2. **write_file** — 全量覆写文件，原子写入，read-before-write 门控
3. **edit_file** — 精确子串替换，唯一匹配校验，read-before-write 门控
4. **glob** — 文件名模式匹配搜索，支持 `**` 跨目录
5. **grep** — 正则内容搜索，ripgrep 优先 + walker 回退

## 2. 文件位置

```
pkg/tool/basic/read.go     # read_file eino 适配
pkg/tool/basic/write.go    # write_file eino 适配
pkg/tool/basic/edit.go     # edit_file eino 适配
pkg/tool/basic/glob.go     # glob eino 适配
pkg/tool/basic/grep.go     # grep eino 适配
tools.go                   # 核心执行逻辑（runRead, runWrite, runEdit, runGlob, runGrep）
search.go                  # 搜索引擎（searchEngine 接口, ripgrepEngine, walkerEngine）
```

## 3. 依赖

- `os` / `io/fs` — 文件读写与目录遍历
- `path/filepath` — 路径操作
- `strings` — 文本处理
- `unicode/utf8` — UTF-8 校验
- `regexp` — 正则编译（walker 引擎）
- `os/exec` — ripgrep 调用
- `encoding/json` — 参数解析
- `sort` — 结果排序
- `bufio` — 逐行扫描（gitignore/ripgrep JSON）
- `bytes` — 缓冲
- `sync` — 并发读取状态保护

## 4. 关键常量

```go
const maxListResults     = 200             // glob / grep 最大输出条数
const defaultReadLines   = 2000            // read_file 默认分页行数
const maxReadFileBytes   = 256 * 1024      // read_file 读前 stat 文件大小门槛 (256KB)
const maxReadOutputRunes = 25000           // read_file 实际输出字符预算
const maxGrepFileSize    = 5 * 1024 * 1024 // grep 跳过超过该大小的文件 (5MB)

// mtime 去重 stub（约 30 token）
const fileUnchangedStub = "File unchanged since last read. The content from the earlier Read tool_result in this conversation is still current — refer to that instead of re-reading."
```

## 5. 公共数据结构

### 5.1 readState — 读取状态记录

```go
type readState struct {
    mtime    time.Time // 文件修改时间
    start    int       // 返回区间起始行（1-based，含）；空文件为 0
    end      int       // 返回区间结束行（1-based，含）；空文件为 0
    fullView bool      // 本次读取是否覆盖整个文件
}
```

### 5.2 sessionState 中的读取记录

```go
type sessionState struct {
    reads   map[string]readState // absPath → 最近一次成功读取的状态
    readsMu sync.Mutex           // 保护 reads map 的并发访问
    // ...
}
```

### 5.3 flexInt — 兼容数字与字符串

```go
type flexInt int
// UnmarshalJSON 兼容 JSON 数字与数字字符串
// "2000" → 2000, 2000 → 2000, "abc" → error, 2.5 → error
```

### 5.4 safePath — 路径安全校验

```go
func safePath(path string) (string, error) {
    // 1. path 为空 → error "path is required"
    // 2. 非绝对路径 → filepath.Join(workdir, path)
    // 3. filepath.Clean 规范化
    // 4. 计算相对路径: rel := filepath.Rel(workdir, abs)
    // 5. 如果 rel == ".." 或以 "../" 开头 → error "access denied: path escapes workspace"
    // 6. 返回 abs, nil
}
```

**所有文件操作工具的路径参数都必须通过 `safePath()` 校验，禁止 `../` 逃逸出 workdir。**

---

## 6. read_file 工具

### 6.1 参数

```go
type ReadFileArgs struct {
    Path   string `json:"path" jsonschema:"required" jsonschema_description:"File path relative to workspace"`
    Offset int    `json:"offset,omitempty" jsonschema_description:"1-based line number to start reading from"`
    Limit  int    `json:"limit,omitempty" jsonschema_description:"Number of lines to read (default 2000)"`
}
```

handler 层使用指针 + flexInt 区分"未提供"与"显式 0"：

```go
var in struct {
    Path   string   `json:"path"`
    Offset *flexInt `json:"offset"`
    Limit  *flexInt `json:"limit"`
}
```

### 6.2 runRead() 完整流程

```
函数签名: func runRead(st *sessionState, path string, offset int, limit *int) string

流程:
1. 参数校验:
   - offset < 0 → "Error: offset must be >= 1 (1-based starting line), got {offset}"
   - limit != nil && *limit < 0 → "Error: limit must be >= 0, got {*limit}"

2. 路径安全:
   - abs, err := safePath(path)
   - 失败 → "Error: {err}"

3. 读前 stat:
   - info, err := os.Stat(abs)
   - 失败 → "Error: {err}"
   - 如果未显式分页（offset==0 && limit==nil）且 info.Size() > maxReadFileBytes:
     → "Error: {path} is {size} bytes (>{maxReadFileBytes/1024}KB). Read with offset/limit (default page {defaultReadLines} lines)."

4. 默认 offset:
   - offset == 0 → offset = 1

5. 读取文件:
   - data, err := os.ReadFile(abs)
   - 失败 → "Error: {err}"
   - !utf8.Valid(data) → "Error: {path} is not a UTF-8 text file"

6. 计算行区间:
   - 按 \n 分割为 lines（trim 尾部 \n）
   - n = len(lines)
   - 空文件 → start=0, end=0, fullView=true
   - 非空:
     * offset > n → "Error: offset {offset} is beyond end of file ({n} lines)"
     * start = offset
     * end = n
     * pageLines = defaultReadLines (limit==nil) 或 *limit (limit!=nil, 0=不限)
     * 如果 pageLines > 0: end = min(offset-1+pageLines, n)
     * fullView = (start==1 && end==n)

7. 格式化输出:
   - width = lineNumberWidth(n)  // 自适应行号宽度
   - 对 i in [start, end]:
     fmt.Fprintf(&b, "%*d→%s\n", width, i, lines[i-1])
   - output = trimRight(b.String(), "\n")

8. 输出预算校验:
   - len([]rune(output)) > maxReadOutputRunes:
     * msg = "Error: output for lines {start}-{end} is {r} characters, exceeding the {maxReadOutputRunes}-character budget. Retry with a smaller limit."
     * 如果 end < n: msg += " Use offset={end+1} to continue from the next page."
     * → 返回 msg（不返回截断内容）

9. mtime 去重:
   - 如果 st != nil:
     * 加锁查 st.reads[abs]
     * 如果 prev 存在 && prev.mtime == info.ModTime() && prev.start == start && prev.end == end && prev.fullView == fullView:
       → 返回 fileUnchangedStub（约 30 token）
     * 否则更新 st.reads[abs] = readState{mtime, start, end, fullView}

10. 返回 output
```

### 6.3 行号格式

```
   1→first line content
   2→second line content
  10→tenth line content
 100→hundredth line content
```

宽度自适应：`lineNumberWidth(totalLines)` 计算总行数的位数，同一文件翻页时宽度稳定。

### 6.4 输出格式

- 正常：行号 + "→" + 内容，每行一个
- 空文件：空字符串（n==0 时不输出任何行）
- 去重命中：`fileUnchangedStub` 常量文本
- 超限：错误消息（不返回内容）

---

## 7. write_file 工具

### 7.1 参数

```go
type WriteFileArgs struct {
    Path    string `json:"path" jsonschema:"required" jsonschema_description:"File path relative to workspace"`
    Content string `json:"content" jsonschema:"required" jsonschema_description:"Content to write to the file"`
}
```

### 7.2 runWrite() 完整流程

```
函数签名: func runWrite(st *sessionState, path, content string) string

流程:
1. 路径安全:
   - abs, err := safePath(path)
   - 失败 → "Error: {err}"

2. 门控检查:
   - perm, err := checkWritable(st, abs, path)
   - 失败 → "Error: {err}"

3. 行尾归一化:
   - content = strings.ReplaceAll(content, "\r\n", "\n")  // CRLF → LF
   - content = strings.ReplaceAll(content, "\r", "\n")     // 孤立 CR → LF

4. 创建父目录:
   - os.MkdirAll(filepath.Dir(abs), 0o755)
   - 失败 → "Error: {err}"

5. 原子写入:
   - atomicWriteFile(abs, []byte(content), perm)
   - 失败 → "Error: {err}"

6. 更新读取状态:
   - st.markWritten(abs, content)

7. 返回: "Wrote {len(content)} bytes to {path}"
```

### 7.3 checkWritable() — read-before-write 门控

```
函数签名: func checkWritable(st *sessionState, abs, path string) (os.FileMode, error)

流程:
1. os.Stat(abs):
   - 文件不存在 (fs.ErrNotExist) → 返回 (0o644, nil)  // 新建文件，豁免门控
   - 其他错误 → 返回 (0o644, err)

2. 保存原文件权限: perm := fi.Mode().Perm()

3. 门控检查（st != nil 时）:
   a. 加锁查 st.reads[abs]
   b. 如果无记录 或 记录.fullView == false:
      → 返回 (perm, error "{path} has not been read yet. Read it first before writing to it.")
   c. 如果 fi.ModTime().After(last.mtime):
      → 返回 (perm, error "{path} has been modified since read. Read it again before writing to it.")

4. 返回 (perm, nil)  // 允许写入
```

**门控语义**:
- 已存在文件：必须在本会话中全量读过（`fullView == true`），且读取后 mtime 未变
- 新建文件：豁免门控
- 部分读取（翻页）不算全量读取，不允许写入
- 外部修改（mtime 变化）需要重新读取

### 7.4 atomicWriteFile() — 原子写入

```
函数签名: func atomicWriteFile(abs string, data []byte, perm os.FileMode) error

流程:
1. 在同目录创建临时文件: os.CreateTemp(filepath.Dir(abs), ".tmp-*")
2. 注册 defer 清理: 如果 tmp != "" 则 os.Remove(tmp)（失败路径清理）
3. 写入数据: f.Write(data)
4. fsync: f.Sync()  // 确保数据落盘
5. 关闭: f.Close()
6. 设置权限: os.Chmod(tmp, perm)  // 保留原文件权限
7. 原子重命名: os.Rename(tmp, abs)
8. tmp = ""  // rename 成功，取消 defer 清理
9. 返回 nil

保证: 目标路径要么是旧内容完整，要么是新内容完整，永远不会是半截状态。
```

### 7.5 markWritten() — 写入后状态刷新

```
函数签名: func (st *sessionState) markWritten(abs, content string)

流程:
1. st == nil → 直接返回
2. os.Stat(abs) 获取最新 mtime
3. 计算写入行数区间:
   - trim 尾部 \n
   - 非空 → start=1, end=len(split("\n"))
   - 空 → start=0, end=0
4. 加锁更新 st.reads[abs] = readState{mtime, start, end, fullView: true}
```

---

## 8. edit_file 工具

### 8.1 参数

```go
type EditFileArgs struct {
    Path       string `json:"path" jsonschema:"required" jsonschema_description:"File path relative to workspace"`
    OldText    string `json:"old_text" jsonschema:"required" jsonschema_description:"Text to search for"`
    NewText    string `json:"new_text" jsonschema:"required" jsonschema_description:"Text to replace with"`
    ReplaceAll bool   `json:"replace_all,omitempty" jsonschema_description:"Replace all occurrences"`
}
```

### 8.2 runEdit() 完整流程

```
函数签名: func runEdit(st *sessionState, path, oldText, newText string, all bool) string

流程:
1. 前置校验:
   - oldText == "" → "Error: old_text must not be empty"
   - newText == oldText → "Error: new_text must be different from old_text"

2. 路径安全:
   - abs, err := safePath(path)
   - 失败 → "Error: {err}"

3. 门控检查:
   - perm, err := checkWritable(st, abs, path)
   - 失败 → "Error: {err}"

4. 读取文件:
   - data, err := os.ReadFile(abs)
   - 失败 → "Error: {err}"
   - content := string(data)

5. 匹配计数:
   - count := strings.Count(content, oldText)

6. 匹配校验:
   - count == 0:
     → "Error: text not found in {path} — the file may have been modified since it was read; read it again"
   - count > 1 && !all:
     → "Error: old_text is not unique in {path} ({count} matches). Add surrounding context to make it unique, or set replace_all=true to replace every occurrence"

7. 执行替换:
   - all == true: content = strings.ReplaceAll(content, oldText, newText)
   - all == false: content = strings.Replace(content, oldText, newText, 1)  // 只替换第一处

8. 原子写入:
   - atomicWriteFile(abs, []byte(content), perm)
   - 失败 → "Error: {err}"

9. 更新读取状态:
   - st.markWritten(abs, content)

10. 返回: "Edited {path}"
```

### 8.3 唯一匹配语义

| 匹配数 | replace_all | 结果 |
|--------|-------------|------|
| 0 | 任意 | 失败："text not found" |
| 1 | false | 成功：替换唯一匹配 |
| 1 | true | 成功：替换全部（1 处） |
| >1 | false | 失败："not unique, N matches" |
| >1 | true | 成功：替换全部匹配 |

---

## 9. glob 工具

### 9.1 参数

```go
type GlobArgs struct {
    Pattern string `json:"pattern" jsonschema:"required" jsonschema_description:"Glob pattern to match files"`
    Path    string `json:"path,omitempty" jsonschema_description:"Directory to search in (default: workspace root)"`
}
```

### 9.2 runGlob() 完整流程

```
函数签名: func runGlob(pattern string, path string) string

流程:
1. 参数校验:
   - pattern == "" → "Error: pattern is required"

2. 确定搜索根目录:
   - root = workdir
   - path != "" → safePath(path) 校验后作为 root
   - 校验失败 → "Error: {err}"

3. 执行搜索:
   - matches, err := searchEng.Glob(pattern, root)
   - 失败 → "Error: {err}"

4. 限制结果条数:
   - len(matches) > maxListResults (200):
     shown = matches[:200] + "(more matches omitted; {total} total, narrow the pattern)"
   - 否则 shown = matches

5. 空结果:
   - len(shown) == 0 → "(no matches)"

6. 返回: strings.Join(shown, "\n")
```

### 9.3 matchGlob() — 自研 glob 匹配算法

```
函数签名: func matchGlob(pattern, name string) bool

流程:
1. 按 "/" 分割 pattern 和 name 为段数组
2. 调用 matchSegs(patSegs, nameSegs)

matchSegs(pat, name []string) bool:
1. pat 为空:
   - name 也为空 → true
   - name 非空 → false

2. pat[0] == "**":
   - 尝试匹配 pat[1:] 与 name[i:]（i 从 0 到 len(name)）
   - 任一匹配成功 → true
   - 全部失败 → false
   - ** 语义: 匹配零个或多个目录层级

3. pat[0] != "**":
   - name 为空 → false
   - filepath.Match(pat[0], name[0]) 匹配当前段
   - 不匹配 → false
   - 匹配 → 递归 matchSegs(pat[1:], name[1:])
```

**与标准库的区别**: Go `filepath.Glob` 不支持 `**`（跨目录匹配）。自研算法通过递归分段实现 `**` 语义。

### 9.4 Glob 搜索引擎

`searchEng.Glob()` 始终使用 `walkerEngine`（不用 ripgrep），因为 ripgrep 的 glob 默认递归，与 `"*.py"` 只匹配顶层的语义不同。

walker 引擎流程:
1. `filepath.WalkDir(root, scan)` 遍历目录树
2. 跳过 VCS 目录（.git, .svn, .hg, .bzr, .jj, .sl）
3. 加载并缓存 `.gitignore` 规则
4. 跳过被 gitignore 的文件/目录
5. 对每个文件计算相对路径（`/` 分隔）
6. 调用 `matchGlob(pattern, relSlash)` 匹配
7. 匹配成功则追加到结果列表
8. 最终 `sort.Strings(matches)` 排序返回

---

## 10. grep 工具

### 10.1 参数

```go
type GrepArgs struct {
    Pattern string `json:"pattern" jsonschema:"required" jsonschema_description:"Regex pattern to search for"`
    Path    string `json:"path,omitempty" jsonschema_description:"File or directory to search in (default: workspace root)"`
}
```

handler 层使用完整参数集:

```go
type runGrepParams struct {
    Pattern    string `json:"pattern"`
    Path       string `json:"path"`
    Glob       string `json:"glob"`         // glob 过滤（rg 风格）
    OutputMode string `json:"output_mode"`  // content | files_with_matches | count
    HeadLimit  int    `json:"head_limit"`
    Offset     int    `json:"offset"`
    IgnoreCase bool   `json:"ignore_case"`
}
```

### 10.2 runGrep() 完整流程

```
函数签名: func runGrep(params runGrepParams) string

流程:
1. 参数校验:
   - pattern == "" → "Error: pattern is required"

2. 默认值:
   - OutputMode == "" → "files_with_matches"
   - HeadLimit == 0 → maxListResults (200)

3. 路径安全:
   - Path != "" → safePath(Path) 校验
   - 失败 → "Error: {err}"

4. 构造 glob 过滤:
   - Glob != "" → globs = [Glob]

5. 构造 grepRequest:
   req := grepRequest{
       Pattern:     params.Pattern,
       Root:        workdir,
       Path:        params.Path,
       Globs:       globs,
       OutputMode:  params.OutputMode,
       HeadLimit:   params.HeadLimit,
       Offset:      params.Offset,
       IgnoreCase:  params.IgnoreCase,
       LineNumbers: true,
   }

6. 执行搜索:
   result, err := searchEng.Grep(req)
   - 失败:
     * 包含 "ripgrep" → 去掉前缀返回（可能是非法正则）
     * 其他 → "Error: {err}"

7. 格式化输出:
   - files_with_matches: 直接输出 result.Files 列表
   - count: 按文件排序输出 "{file}:{count}"
   - content: 按文件+行号排序输出 "{file}:{line}:{text}"

8. 截断提示:
   - result.Truncated → 追加 "... (more matches omitted; use a more specific pattern or raise head_limit)"

9. 空结果:
   - len(lines) == 0 → "(no matches)"

10. 返回: strings.Join(lines, "\n")
```

### 10.3 搜索引擎选择

```
函数签名: func newSearchEngine() searchEngine

流程:
1. exec.LookPath("rg") 探测 ripgrep
2. 找到 → 返回 &ripgrepEngine{walker: &walkerEngine{}}
3. 未找到 → 返回 &walkerEngine{}
```

### 10.4 ripgrepEngine.Grep()

```
流程:
1. 构造 rg 命令行参数:
   --json --hidden
   --glob !.git --glob !.svn --glob !.hg --glob !.bzr --glob !.jj --glob !.sl
   --max-columns 500
   --max-filesize 5M
   + 所有 .gitignore 文件: --ignore-file {path}
   + 可选: -i（忽略大小写）
   + 可选: -n（行号）
   + 可选: -U --multiline-dotall（多行模式）
   + 可选: --glob {filter}
   + 可选: -C {ctx}（上下文行数）
   + pattern + target_path

2. 执行 rg 命令

3. 处理退出码:
   - 退出 1 = 无匹配（非错误）
   - 其他非零 = 真实错误

4. 解析 JSON 输出: parseRgJSON()
```

### 10.5 parseRgJSON()

```
流程:
1. 初始化 result（Counts map, Matches 数组）
2. 默认 headLimit = 250（如果 req.HeadLimit == 0）
3. 逐行扫描 rg JSON 输出:
   - 解析 {type, data} 结构
   - type == "match":
     a. 提取 path.text, line_number, lines.text
     b. 计算相对路径（filepath.Rel + ToSlash）
     c. 按 OutputMode 处理:
        - count: 累加 fileCounts[rel]++
        - 其他: 应用 offset 跳过后收集到 result.Matches（受 headLimit 限制）
   - type == "summary": 跳过
4. 最终按 OutputMode 聚合:
   - files_with_matches: 从 Matches 去重文件列表，排序，截断到 headLimit
   - count: 从 fileCounts 排序，截断到 headLimit
   - content: 保持 Matches
5. 返回 result
```

### 10.6 walkerEngine.Grep()

```
流程:
1. 编译正则:
   - regexp.Compile(pattern)
   - IgnoreCase → "(?i:" + pattern + ")"
   
2. 确定搜索根目录:
   - Path 非空且是目录 → 用该目录
   - Path 非空且是文件 → 单文件搜索 grepFile()
   - Path 为空 → 用 req.Root

3. 加载 gitignore: loadGitignore(root)

4. filepath.WalkDir(root, scan):
   a. 目录:
      - VCS 目录 → SkipDir
      - 加载并缓存子目录 gitignore
      - 被 gitignore → SkipDir
   b. 文件:
      - 非普通文件 → 跳过
      - fi.Size() > maxGrepFileSize (5MB) → 跳过
      - 非 UTF-8 → 跳过
      - 被 gitignore → 跳过
      - glob 过滤不匹配 → 跳过
      - 逐行正则匹配:
        * 行长度 > 500 → 截断到 500
        * 匹配 → 按 OutputMode 处理

5. 聚合结果（同 ripgrep 的聚合逻辑）
```

### 10.7 gitignore 处理

```
loadGitignore(dir string) []gitignorePattern:
1. 从 dir 向上遍历祖先目录（到 workdir 为止）
2. 每层加载 .gitignore 文件
3. 子目录的规则排在前面（优先级高）
4. 返回合并后的 patterns 列表

parseGitignore(data []byte, dir string) []gitignorePattern:
1. 逐行扫描
2. 空行和 # 注释 → 跳过
3. ! 前缀 → negate = true
4. 去行尾 \#
5. 带 / 的 pattern → 相对于 dir 的绝对路径
6. 不带 / 的 pattern → 匹配任何深度

isGitignored(relPath string, patterns []gitignorePattern) bool:
1. ignored = false
2. 遍历 patterns:
   - 带 / 的 pattern → matchGlob(pattern, relPath)
   - 不带 / → 对 relPath 的每一段做 filepath.Match
   - 匹配且 negate → ignored = false
   - 匹配且非 negate → ignored = true
3. 返回 ignored
```

### 10.8 VCS 目录排除

```go
var vcsDirs = map[string]bool{
    ".git":  true,
    ".svn":  true,
    ".hg":   true,
    ".bzr":  true,
    ".jj":   true,
    ".sl":   true,
}
```

---

## 11. 输出模式详解

### 11.1 read_file 输出

```
   1→package main
   2→
   3→import "fmt"
   4→
   5→func main() {
   6→    fmt.Println("hello")
   7→}
```

行号右对齐，宽度自适应，"→" 分隔。

### 11.2 write_file 输出

```
Wrote 42 bytes to path/to/file.go
```

### 11.3 edit_file 输出

```
Edited path/to/file.go
```

### 11.4 glob 输出

```
path/to/file1.go
path/to/file2.go
path/to/subdir/file3.go
```

每行一个相对路径（相对 workdir，使用 `/` 分隔符）。

### 11.5 grep 输出

**files_with_matches 模式（默认）**:
```
path/to/file1.go
path/to/file2.go
```

**count 模式**:
```
path/to/file1.go:3
path/to/file2.go:1
```

**content 模式**:
```
path/to/file1.go:5:    fmt.Println("hello")
path/to/file1.go:12:    fmt.Println("world")
```

---

## 12. 错误处理

### 12.1 read_file

| 错误场景 | 输出 |
|---------|------|
| 路径逃逸 | "Error: access denied: path escapes workspace: {path}" |
| 文件不存在 | "Error: stat {abs}: no such file or directory" |
| 非 UTF-8 | "Error: {path} is not a UTF-8 text file" |
| 大文件未分页 | "Error: {path} is {N} bytes (>{256}KB). Read with offset/limit (default page 2000 lines)." |
| offset 越界 | "Error: offset {N} is beyond end of file ({M} lines)" |
| 输出超预算 | "Error: output for lines {s}-{e} is {N} characters, exceeding the 25000-character budget. Retry with a smaller limit." |
| mtime 去重 | fileUnchangedStub（约 30 token 的短提示） |

### 12.2 write_file

| 错误场景 | 输出 |
|---------|------|
| 路径逃逸 | "Error: access denied: ..." |
| 未先读取 | "Error: {path} has not been read yet. Read it first before writing to it." |
| 读后修改 | "Error: {path} has been modified since read. Read it again before writing to it." |
| 目录创建失败 | "Error: {err}" |
| 原子写失败 | "Error: {err}" |

### 12.3 edit_file

| 错误场景 | 输出 |
|---------|------|
| old_text 为空 | "Error: old_text must not be empty" |
| new_text == old_text | "Error: new_text must be different from old_text" |
| 未找到匹配 | "Error: text not found in {path} — the file may have been modified since it was read; read it again" |
| 多处匹配 | "Error: old_text is not unique in {path} ({N} matches). Add surrounding context to make it unique, or set replace_all=true to replace every occurrence" |
| 门控失败 | 同 write_file |

### 12.4 glob / grep

| 错误场景 | 输出 |
|---------|------|
| pattern 为空 | "Error: pattern is required" |
| 路径逃逸 | "Error: access denied: ..." |
| 非法正则（grep） | "Error: {ripgrep 错误去掉前缀}" |
| 无匹配 | "(no matches)" |
| 结果超限 | 截断 + "... (more matches omitted; ...)" |

---

## 13. 测试要点

### 13.1 read_file
- 正常读取：行号格式正确，宽度自适应
- 分页读取：offset + limit 正确截取
- 大文件保护：> 256KB 未分页时快速失败
- 输出预算：> 25000 runes 时返回错误而非截断
- mtime 去重：同文件同区间重复读取返回 stub
- mtime 变化：文件修改后去重不命中
- 非 UTF-8 文件：返回错误提示
- 空文件：返回空字符串
- offset 越界：返回错误

### 13.2 write_file
- 新建文件：无需先读取，直接写入
- 覆写已读文件：正常写入
- 覆写未读文件：门控拒绝
- 读后外部修改：门控拒绝（mtime 变化）
- 原子写：临时文件 + rename 正确执行
- 行尾归一化：CRLF → LF
- 父目录自动创建
- 权限保留：原文件权限位不变

### 13.3 edit_file
- 唯一匹配替换：正确替换
- 零匹配：返回 "text not found" 错误
- 多处匹配（replace_all=false）：返回 "not unique" 错误
- 多处匹配（replace_all=true）：全部替换
- old_text 为空：返回错误
- new_text == old_text：返回错误
- 门控检查：同 write_file

### 13.4 glob
- `*.go` 匹配顶层 Go 文件
- `**/*.go` 匹配所有层级 Go 文件
- `src/**/*.ts` 跨目录匹配 TypeScript
- 结果排序正确
- 超过 200 条截断 + 提示
- 空结果返回 "(no matches)"
- gitignore 规则正确排除
- VCS 目录正确跳过

### 13.5 grep
- files_with_matches 模式：返回文件列表
- content 模式：返回文件:行号:内容
- count 模式：返回文件:次数
- 忽略大小写 (-i)
- 跳过 > 5MB 文件
- ripgrep 可用时使用 rg
- ripgrep 不可用时回退 walker
- 正则错误正确报告
- head_limit 截断
- gitignore 尊重
- VCS 目录排除
