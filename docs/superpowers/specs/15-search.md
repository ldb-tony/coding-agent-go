# 15 - 搜索引擎规格

## 1. 职责

搜索引擎系统为代码搜索提供高性能的统一接口:

1. **内容搜索 (Grep)** — 基于正则表达式的文件内容搜索，支持上下文、大小写忽略、多行模式、glob 过滤
2. **文件名匹配 (Glob)** — 按 glob 模式查找匹配的文件路径，支持 `**` 递归匹配
3. **双引擎架构** — ripgrep 优先（高性能外部进程），walker 回退（纯 Go 标准库实现）
4. **共享过滤** — 两种引擎共享 gitignore 匹配器和 VCS 目录排除

**与 eino filesystem middleware 的关系**:
- search 模块是 filesystem middleware 中 glob 和 grep 工具的底层实现
- middleware 负责工具注册（eino tool.InvokableTool 接口）和参数解析（JSON → grepRequest）
- search 模块负责实际执行搜索，返回结构化结果（grepResult）
- middleware 将 grepResult 格式化为 LLM 可读的文本

## 2. 文件位置

```
pkg/search/
├── search.go        # searchEngine 接口、工厂函数 newSearchEngine()
├── ripgrep.go       # ripgrepEngine 实现（Grep 委托 rg，Glob 委托 walker）
├── walker.go        # walkerEngine 实现（纯 Go WalkDir + 正则匹配）
└── gitignore.go     # gitignore 匹配器（加载、解析、判断）
```

## 3. 依赖

- `os/exec` — 调用 ripgrep 外部命令（`exec.LookPath` 探测 + `exec.Command` 执行）
- `encoding/json` — 解析 rg `--json` 输出（逐行 JSON 事件）
- `path/filepath` — 文件遍历（WalkDir）和路径匹配
- `regexp` — 正则表达式编译和匹配（walker 引擎）
- `strings` — 字符串处理
- `bufio` — 逐行扫描（rg JSON 输出 + gitignore 文件解析）
- `bytes` — 缓冲区（rg stdout/Stderr）
- `os` — 文件状态检查（Stat、ReadFile）
- `io/fs` — DirEntry 遍历
- `unicode/utf8` — UTF-8 校验（walker 跳过二进制文件）
- `sort` — 结果排序
- `strconv` — 数字转换（rg 参数构建）

## 4. 数据结构

### 4.1 searchEngine 接口

```go
type searchEngine interface {
    Grep(req grepRequest) (*grepResult, error)
    Glob(pattern, root string) ([]string, error)
}
```

### 4.2 grepRequest

```go
type grepRequest struct {
    Pattern     string   // 搜索模式（正则表达式）
    Root        string   // 搜索根目录（绝对路径）
    Path        string   // 可选：限定文件或子目录（相对 Root 或绝对）
    Globs       []string // glob 过滤（rg 风格），如 ["*.go", "!*_test.go"]
    OutputMode  string   // "content" | "files_with_matches" | "count"（默认 "files_with_matches"）
    BeforeCtx   int      // 匹配行前的上下文行数（-B）
    AfterCtx    int      // 匹配行后的上下文行数（-A）
    IgnoreCase  bool     // 是否忽略大小写（-i）
    LineNumbers bool     // content 模式是否带行号（-n）
    Multiline   bool     // 是否启用多行匹配模式（-U）
    HeadLimit   int      // 最大结果数（默认 250）
    Offset      int      // 跳过前 N 条结果
}
```

### 4.3 grepResult

```go
type grepResult struct {
    Files     []string       // files_with_matches 模式下的命中文件列表（相对 Root，正斜杠）
    Counts    map[string]int // count 模式下的 "文件路径: 匹配次数"
    Matches   []grepMatch    // content 模式下的匹配行
    NumFiles  int            // 实际命中的文件数（截断前也保留真实值）
    Truncated bool           // 结果是否被截断（达到 HeadLimit）
}
```

### 4.4 grepMatch

```go
type grepMatch struct {
    File string // 文件路径（相对于 Root，正斜杠分隔）
    Line int    // 行号（1-based）
    Text string // 匹配行文本（已 TrimRight 去除行尾换行）
}
```

### 4.5 ripgrepEngine

```go
type ripgrepEngine struct {
    walker *walkerEngine // 用于 Glob 委托（rg glob 语义与我们不同）
}
```

### 4.6 walkerEngine

```go
type walkerEngine struct{}
```

### 4.7 gitignorePattern

```go
type gitignorePattern struct {
    pattern string // glob pattern（可能含路径分隔符，已 ToSlash 转换）
    negate  bool   // true 表示 ! 前缀（否定规则）
}
```

## 5. 关键常量

```go
const (
    maxListResults  = 200              // Glob / list 类操作的最大输出条数
    maxGrepFileSize = 5 * 1024 * 1024  // Grep 跳过超过该大小的文件 (5MB)
)

// 默认 HeadLimit: 250（在 grepRequest.HeadLimit == 0 时使用）

// VCS 目录排除列表 — 全平台排除，避免在 SVN/Bazaar/Jujutsu/Sapling 里返回噪音
var vcsDirs = map[string]bool{
    ".git":  true,
    ".svn":  true,
    ".hg":   true,
    ".bzr":  true,
    ".jj":   true,
    ".sl":   true,
}
```

## 6. 工厂函数

### 6.1 newSearchEngine()

```
函数签名: func newSearchEngine() searchEngine

流程:
1. 探测 ripgrep 是否可用:
   _, err := exec.LookPath("rg")

2. 如果可用:
   return &ripgrepEngine{walker: &walkerEngine{}}
   // Glob 操作始终委托给 walkerEngine（rg 的 glob 默认递归，语义不同）

3. 如果不可用:
   return &walkerEngine{}
```

## 7. Grep 实现 — ripgrepEngine

### 7.1 Grep()

```
函数签名: func (e *ripgrepEngine) Grep(req grepRequest) (*grepResult, error)

流程:

1. 构建 rg 基础参数:
   args := []string{
     "--json",              // JSON 输出模式（逐行 JSON 事件）
     "--hidden",            // 包含隐藏文件
     "--glob", "!.git",     // 排除 VCS 目录
     "--glob", "!.svn",
     "--glob", "!.hg",
     "--glob", "!.bzr",
     "--glob", "!.jj",
     "--glob", "!.sl",
     "--max-columns", "500", // 单行最大字符数（防止超长行）
     "--max-filesize", "5M", // 文件大小上限（与 maxGrepFileSize 一致）
   }

2. 收集并传递所有 .gitignore 文件:
   for _, gi := range findAllGitignoreFiles(req.Root):
     args = append(args, "--ignore-file", gi)
   // rg 默认只在 git 仓库里自动读 .gitignore
   // 我们显式收集所有层级的 .gitignore 并传给 rg

3. 添加可选参数:
   if req.IgnoreCase:
     args = append(args, "-i")
   if req.LineNumbers:
     args = append(args, "-n")
   if req.Multiline:
     args = append(args, "-U", "--multiline-dotall")
   for _, g := range req.Globs:
     args = append(args, "--glob", g)
   if req.BeforeCtx > 0 || req.AfterCtx > 0:
     // 使用 -C 统一上下文行数（取 AfterCtx 值）
     args = append(args, "-C", strconv.Itoa(req.AfterCtx))

4. 确定搜索目标路径:
   target := req.Root
   if req.Path != "":
     abs, err := safePath(req.Path)
     if err != nil: return nil, err
     target = abs

5. 追加 pattern 和 target（rg 参数顺序: [options] pattern [path]）:
   args = append(args, req.Pattern, target)

6. 执行 rg:
   cmd := exec.Command("rg", args...)
   var stdout, stderr bytes.Buffer
   cmd.Stdout = &stdout
   cmd.Stderr = &stderr
   err := cmd.Run()

7. 处理退出码:
   - 退出码 0: 有匹配，正常处理
   - 退出码 1: 无匹配，不是错误 → 返回空结果
   - 退出码 2: rg 错误 → 返回 error("ripgrep: {stderr}")
   - 其他非零: → 返回 error

   实现:
   if err != nil:
     var ee *exec.ExitError
     if !errors.As(err, &ee) || ee.ExitCode() != 1:
       return nil, fmt.Errorf("ripgrep: %s", strings.TrimSpace(stderr.String()))

8. 解析 JSON 输出:
   return parseRgJSON(stdout.Bytes(), req)
```

### 7.2 parseRgJSON()

```
函数签名: func parseRgJSON(data []byte, req grepRequest) (*grepResult, error)

流程:

1. 初始化:
   result := &grepResult{Counts: map[string]int{}}
   headLimit := req.HeadLimit
   if headLimit == 0: headLimit = 250
   offset := req.Offset
   skipped := 0
   fileCounts := map[string]int{}

2. 逐行解析 rg JSON（每行是一个 JSON 事件）:
   scanner := bufio.NewScanner(bytes.NewReader(data))
   for scanner.Scan():
     line := scanner.Text()
     if line == "": continue

     var evt struct {
       Type string          `json:"type"`
       Data json.RawMessage `json:"data"`
     }
     json.Unmarshal([]byte(line), &evt)

3. 处理 "match" 事件:
   case "match":
     解析匹配数据:
       var m struct {
         Path       struct{ Text string } `json:"path"`
         LineNumber int                   `json:"line_number"`
         Lines      struct{ Text string } `json:"lines"`
       }
       json.Unmarshal(evt.Data, &m)

     计算相对路径:
       rel, _ := filepath.Rel(root, m.Path.Text)
       rel = filepath.ToSlash(rel)

     文件计数:
       fileCounts[rel]++

     按 OutputMode 处理:
       case "count":
         // 只计数，不收集匹配行
       default (content / files_with_matches):
         // 应用 offset
         if skipped < offset:
           skipped++
           continue
         // 应用 headLimit
         if len(result.Matches) >= headLimit:
           result.Truncated = true
           continue
         // 收集匹配行
         result.Matches = append(result.Matches, grepMatch{
           File: rel,
           Line: m.LineNumber,
           Text: strings.TrimRight(m.Lines.Text, "\r\n"),
         })

4. 处理 "summary" 事件:
   // 可用于获取更多元信息（当前实现中跳过）

5. 按 OutputMode 聚合最终结果:

   case "files_with_matches" (默认):
     // 从 Matches 中提取去重的文件列表
     seen := map[string]bool{}
     for _, m := range result.Matches:
       seen[m.File] = true
     result.NumFiles = len(seen)
     for f := range seen:
       result.Files = append(result.Files, f)
     sort.Strings(result.Files)
     // 应用 headLimit 截断文件列表
     if len(result.Files) > headLimit:
       result.Files = result.Files[:headLimit]
       result.Truncated = true
     result.Matches = nil  // 清空匹配行

   case "count":
     result.Counts = fileCounts
     result.NumFiles = len(fileCounts)
     // 按文件名排序，截断到 headLimit
     keys := sorted keys of fileCounts
     if len(keys) > headLimit:
       删除超出部分
       result.Truncated = true
     result.Matches = nil

   case "content":
     result.NumFiles = len(fileCounts)
     // 保持 Matches 不变

6. 返回 result, scanner.Err()
```

### 7.3 rg 退出码映射

| 退出码 | 含义 | 处理方式 |
|--------|------|---------|
| 0 | 有匹配 | 正常解析 JSON 输出 |
| 1 | 无匹配 | 返回空结果，不是错误 |
| 2 | 错误（如正则语法错误） | 返回 error，包含 stderr 内容 |

## 8. Grep 实现 — walkerEngine

### 8.1 Grep()

```
函数签名: func (e *walkerEngine) Grep(req grepRequest) (*grepResult, error)

流程:

1. 编译正则表达式:
   re, err := regexp.Compile(req.Pattern)
   if req.IgnoreCase:
     re = regexp.MustCompile("(?i:" + req.Pattern + ")")
   - 编译失败 → 返回 nil, error("invalid regex: ...")

2. 确定搜索范围:
   root := req.Root
   if req.Path != "":
     abs, err := safePath(req.Path)
     if err != nil: return nil, err
     info, statErr := os.Stat(abs)
     if statErr == nil && info.IsDir():
       root = abs          // 路径是目录 → 缩小搜索范围
     else:
       return e.grepFile(abs, req, re)  // 路径是文件 → 单文件搜索

3. 初始化 gitignore:
   giPatterns := loadGitignore(root)
   giCache := map[string][]gitignorePattern{root: giPatterns}
   // 子目录 .gitignore 缓存（目录绝对路径 → 合并后的 patterns）

4. 初始化结果:
   result := &grepResult{Counts: map[string]int{}}
   headLimit := req.HeadLimit
   if headLimit == 0: headLimit = 250
   seenFiles := map[string]bool{}  // files_with_matches 用
   var skipOffset int

5. 遍历文件树:
   filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error:

     a. 错误处理:
        if err != nil: return nil  // 跳过无法访问的路径

     b. 目录处理:
        if d.IsDir():
          name := d.Name()
          if vcsDirs[name]: return filepath.SkipDir
          // 进入目录时预计算其 gitignore，缓存起来
          if _, ok := giCache[p]; !ok:
            giCache[p] = loadGitignore(p)
          dirPatterns := giCache[p]
          rel, relErr := filepath.Rel(req.Root, p)
          if relErr == nil && isGitignored(rel, dirPatterns):
            return filepath.SkipDir
          return nil

     c. 非普通文件跳过:
        if !d.Type().IsRegular(): return nil

     d. 大文件跳过:
        fi, infoErr := d.Info()
        if infoErr != nil || fi.Size() > maxGrepFileSize:
          return nil

     e. 路径转换与 gitignore 检查:
        rel, relErr := filepath.Rel(req.Root, p)
        relSlash := filepath.ToSlash(rel)
        fileDir := filepath.Dir(p)
        dirPatterns := giCache[fileDir]  // 使用缓存
        if !ok:
          dirPatterns = loadGitignore(fileDir)
          giCache[fileDir] = dirPatterns
        if isGitignored(rel, dirPatterns):
          return nil

     f. glob 过滤:
        if len(req.Globs) > 0:
          matched := false
          for _, g := range req.Globs:
            if matchGlob(g, relSlash) || matchGlob(g, filepath.Base(relSlash)):
              matched = true; break
          if !matched: return nil

     g. 读取文件内容
        data, readErr := os.ReadFile(p)
        if readErr != nil || !isUTF8(data):
          return nil  // 跳过不可读或非 UTF-8 文件

     h. 逐行匹配:
        lines := strings.Split(string(data), "\n")
        fileHit := 0
        for i, line := range lines:
          if len(line) > 500:
            line = line[:500]  // 单行截断到 500 字符
          if re.MatchString(line):
            fileHit++
            switch req.OutputMode:
              case "files_with_matches", "":
                seenFiles[relSlash] = true
              case "count":
                // 在文件级别计数（不收集行）
              case "content":
                if skipOffset < req.Offset:
                  skipOffset++
                  continue
                if len(result.Matches) >= headLimit:
                  result.Truncated = true
                  continue
                result.Matches = append(result.Matches, grepMatch{
                  File: relSlash,
                  Line: i + 1,
                  Text: strings.TrimRight(line, "\r"),
                })

        if fileHit > 0:
          result.Counts[relSlash] = fileHit

        return nil
   )

6. 按 OutputMode 聚合最终结果（与 ripgrepEngine 相同逻辑）:
   - files_with_matches: 从 seenFiles 提取排序文件列表，应用 headLimit
   - count: 从 result.Counts 排序，应用 headLimit
   - content: 保持 Matches 不变

7. 返回 result, nil
```

### 8.2 grepFile() — 单文件搜索

```
函数签名: func (e *walkerEngine) grepFile(abs string, req grepRequest, re *regexp.Regexp) (*grepResult, error)

流程:
1. 读取文件:
   data, err := os.ReadFile(abs)
   if err != nil || !isUTF8(data): return nil, nil

2. 路径转换:
   rel, _ := filepath.Rel(req.Root, abs)
   relSlash := filepath.ToSlash(rel)

3. 逐行匹配（与 WalkDir 中的单文件逻辑相同）:
   lines := strings.Split(string(data), "\n")
   for i, line := range lines:
     if len(line) > 500: line = line[:500]
     if re.MatchString(line):
       按 OutputMode 收集结果

4. 返回 result, nil
```

## 9. Glob 实现

### 9.1 ripgrepEngine.Glob()

```
函数签名: func (e *ripgrepEngine) Glob(pattern, root string) ([]string, error)

流程:
1. 委托给 walkerEngine:
   return e.walker.Glob(pattern, root)

原因: rg 的 glob 默认递归列出所有文件，语义与我们的 "*.py" 只匹配顶层不同。
     因此 Glob 始终使用 walkerEngine 实现。
```

### 9.2 walkerEngine.Glob()

```
函数签名: func (e *walkerEngine) Glob(pattern, root string) ([]string, error)

流程:

1. 初始化:
   var matches []string
   giCache := map[string][]gitignorePattern{root: loadGitignore(root)}

2. 遍历文件树:
   filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error:

     a. 错误处理:
        if err != nil: return nil

     b. 目录处理:
        if d.IsDir():
          name := d.Name()
          if vcsDirs[name]: return filepath.SkipDir
          // 预加载 gitignore
          if _, ok := giCache[p]; !ok:
            giCache[p] = loadGitignore(p)
          dirPatterns := giCache[p]
          rel, relErr := filepath.Rel(root, p)
          if relErr == nil && isGitignored(rel, dirPatterns):
            return filepath.SkipDir
          return nil

     c. gitignore 检查:
        rel, relErr := filepath.Rel(root, p)
        if relErr != nil: return nil
        relSlash := filepath.ToSlash(rel)
        fileDir := filepath.Dir(p)
        dirPatterns := giCache[fileDir]
        if !ok:
          dirPatterns = loadGitignore(fileDir)
          giCache[fileDir] = dirPatterns
        if isGitignored(rel, dirPatterns):
          return nil

     d. glob 匹配:
        if matchGlob(pattern, relSlash):
          matches = append(matches, relSlash)

        return nil
   )

3. 排序:
   sort.Strings(matches)

4. 返回 matches, nil
```

## 10. gitignore 匹配器

### 10.1 loadGitignore()

```
函数签名: func loadGitignore(dir string) []gitignorePattern

用途: 加载 dir 及其祖先目录下的 .gitignore 文件，合并为统一的 pattern 列表。

流程:
1. 初始化空 patterns 列表

2. 从 dir 开始，逐级向上遍历祖先目录:
   current := dir
   for:
     a. 读取 current/.gitignore:
        data, err := os.ReadFile(filepath.Join(current, ".gitignore"))
        if err == nil:
          // 解析并前置到 patterns 列表（子目录规则优先）
          patterns = append(parseGitignore(data, current), patterns...)

     b. 检查是否到达 workdir 祖先:
        parent := filepath.Dir(current)
        if parent == current: break  // 到达根目录
        rel, err := filepath.Rel(workdir, parent)
        if err != nil || strings.HasPrefix(rel, ".."):
          break  // 超出 workdir 范围

     c. current = parent

3. 返回 patterns

注意: 子目录的 .gitignore 规则排在前面（先匹配），
     祖先目录的规则排在后面。isGitignored() 中后匹配的规则会覆盖先匹配。
```

### 10.2 findAllGitignoreFiles()

```
函数签名: func findAllGitignoreFiles(root string) []string

用途: 递归收集 root 目录树下所有 .gitignore 文件的绝对路径。
     用于 ripgrepEngine 显式 --ignore-file 传递。

流程:
1. 初始化 result 切片

2. filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error:
     if err != nil: return nil
     if d.IsDir():
       if vcsDirs[d.Name()]: return filepath.SkipDir
       return nil
     if d.Name() == ".gitignore":
       result = append(result, p)
     return nil
   )

3. 返回 result
```

### 10.3 parseGitignore()

```
函数签名: func parseGitignore(data []byte, dir string) []gitignorePattern

用途: 解析 .gitignore 文件内容为 gitignorePattern 列表。
     支持: # 注释、! 否定、/ 根锚定、标准 glob。

流程:
1. 初始化空 patterns 列表

2. 逐行扫描:
   scanner := bufio.NewScanner(bytes.NewReader(data))
   for scanner.Scan():
     line := strings.TrimSpace(scanner.Text())

     a. 跳过空行和注释:
        if line == "" || strings.HasPrefix(line, "#"): continue

     b. 解析否定前缀:
        negate := false
        if strings.HasPrefix(line, "!"):
          negate = true
          line = line[1:]

     c. 去除行尾转义 #:
        line = strings.TrimRight(line, "\\#")
        if line == "": continue

     d. 判断路径锚定:
        if strings.Contains(line, "/") && !strings.HasSuffix(line, "/"):
          // 带 / 的 pattern → 相对于 .gitignore 所在目录
          patterns = append(patterns, gitignorePattern{
            pattern: filepath.ToSlash(filepath.Join(dir, line)),
            negate:  negate,
          })
        else:
          // 不带 / 的 pattern → 匹配任何深度
          patterns = append(patterns, gitignorePattern{
            pattern: filepath.ToSlash(line),
            negate:  negate,
          })

3. 返回 patterns
```

### 10.4 isGitignored()

```
函数签名: func isGitignored(relPath string, patterns []gitignorePattern) bool

用途: 检查给定相对路径（相对 workdir）是否被 gitignore 忽略。

流程:
1. ignored := false
   relSlash := filepath.ToSlash(relPath)

2. 遍历所有 pattern（按顺序，后出现的覆盖先出现的）:
   for _, p := range patterns:
     matched := false

     a. 绝对路径 pattern（含 /）→ 精确路径匹配:
        if strings.Contains(p.pattern, "/"):
          matched = matchGlob(p.pattern, relSlash)

     b. 无 / 的 pattern → 匹配 basename 或路径的任何一段:
        else:
          parts := strings.Split(relSlash, "/")
          for _, part := range parts:
            if ok, _ := filepath.Match(p.pattern, part): ok:
              matched = true
              break

     c. 应用匹配结果:
        if matched:
          if p.negate:
            ignored = false   // 否定规则 → 取消忽略
          else:
            ignored = true    // 肯定规则 → 设置忽略

3. 返回 ignored
```

## 11. matchGlob 算法

### 11.1 入口

```
函数签名: func matchGlob(pattern, name string) bool

用途: 支持跨目录的 ** 通配（Go 标准库 filepath.Glob 不支持 **）。

流程:
1. 分段:
   patSegs := strings.Split(pattern, "/")
   nameSegs := strings.Split(name, "/")
2. 递归匹配:
   return matchSegs(patSegs, nameSegs)
```

### 11.2 递归匹配

```
函数签名: func matchSegs(pat, name []string) bool

流程:
1. 基本情况:
   if len(pat) == 0:
     return len(name) == 0

2. ** 处理（匹配零个或多个目录段）:
   if pat[0] == "**":
     for i := 0; i <= len(name); i++:
       if matchSegs(pat[1:], name[i:]):
         return true
     return false

3. 普通段匹配:
   if len(name) == 0:
     return false
   ok, _ := filepath.Match(pat[0], name[0])
   if !ok:
     return false
   return matchSegs(pat[1:], name[1:])
```

### 11.3 匹配示例

| 模式 | 路径 | 结果 | 说明 |
|------|------|------|------|
| `*.go` | `main.go` | true | 单段通配 |
| `*.go` | `pkg/main.go` | false | `*` 不跨目录 |
| `**/*.go` | `a/b/c.go` | true | `**` 匹配任意深度 |
| `pkg/*.go` | `pkg/main.go` | true | 精确目录 |
| `pkg/*.go` | `cmd/main.go` | false | 目录不匹配 |
| `src/**` | `src/a/b/c` | true | `**` 匹配剩余目录 |

## 12. 与 eino filesystem middleware 的关系

### 12.1 职责分层

```
┌─────────────────────────────────────────┐
│  eino filesystem middleware             │
│  (pkg/tool/filesystem.go)               │
│                                         │
│  - 注册 tool.InvokableTool 接口         │
│  - 解析 LLM 的 JSON 参数               │
│  - 调用 search 模块执行搜索             │
│  - 将 grepResult 格式化为 LLM 文本     │
└──────────────┬──────────────────────────┘
               │ 调用
               ▼
┌─────────────────────────────────────────┐
│  search 模块                             │
│  (pkg/search/)                           │
│                                         │
│  - searchEngine 接口                     │
│  - ripgrepEngine / walkerEngine 实现     │
│  - gitignore 匹配器                      │
│  - matchGlob 算法                        │
└─────────────────────────────────────────┘
```

### 12.2 工具映射

| filesystem 工具 | search 模块方法 | OutputMode |
|----------------|----------------|------------|
| grep | searchEngine.Grep() | content / files_with_matches / count |
| glob | searchEngine.Glob() | — |

### 12.3 参数转换

middleware 将 LLM 的 JSON 参数转换为 grepRequest:

| LLM 参数 | grepRequest 字段 | 说明 |
|----------|-----------------|------|
| pattern | Pattern | 正则表达式 |
| path | Path | 限定搜索路径 |
| type | Globs | 转为 `*.{ext}` 格式的 glob |
| output_mode | OutputMode | content / files_with_matches / count |
| -A / -B / -C | AfterCtx / BeforeCtx | 上下文行数 |
| head_limit | HeadLimit | 结果上限（默认 250） |
| offset | Offset | 跳过前 N 条 |
| glob | Globs | 直接传递 glob 模式 |
| -i | IgnoreCase | 忽略大小写 |
| multiline | Multiline | 多行模式 |

## 13. 性能优化

### 13.1 ripgrep 引擎

- **流式 JSON 解析**: 使用 `bufio.Scanner` 逐行解析 rg 输出，不需要等待全部输出再解析
- **进程级并行**: ripgrep 本身是多进程安全的，多个 Grep 调用可并发执行
- **文件大小过滤**: `--max-filesize 5M` 在 rg 内部跳过大文件，减少 I/O
- **行长度限制**: `--max-columns 500` 防止超长行占用内存
- **VCS 排除**: 通过 `--glob !.git` 等在 rg 内部排除，不进入 VCS 目录
- **gitignore 传递**: 显式收集所有 `.gitignore` 文件并通过 `--ignore-file` 传递，确保非 git 仓库也能正确过滤

### 13.2 walker 引擎

- **WalkDir 高效遍历**: Go 标准库的 `filepath.WalkDir` 使用 `os.ReadDir` 批量读取目录项
- **gitignore 缓存**: 每个目录的 gitignore patterns 只加载一次，缓存在 `giCache` map 中
- **大文件跳过**: `fi.Size() > maxGrepFileSize` 在读取文件内容前检查，避免加载大文件到内存
- **UTF-8 校验**: `isUTF8(data)` 跳过二进制文件，避免正则匹配乱码
- **单行截断**: `len(line) > 500` 截断超长行，防止内存膨胀

### 13.3 共享优化

- **VCS 目录排除**: 在引擎层面实现（`vcsDirs` map），两种引擎共享
- **结果路径归一化**: 所有返回路径使用 `filepath.ToSlash` 转为正斜杠，相对 Root 路径
- **HeadLimit 默认值**: 250，防止返回过多结果
- **Offset 支持**: 允许分页查询，跳过前 N 条结果

## 14. 错误处理

| 错误场景 | 引擎 | 处理方式 |
|---------|------|---------|
| rg 不可用 | ripgrepEngine | 工厂函数返回 walkerEngine（透明降级） |
| rg 无匹配（退出码 1） | ripgrepEngine | 返回空 grepResult，不是错误 |
| rg 执行错误（退出码 2） | ripgrepEngine | 返回 error，包含 stderr 内容 |
| 正则编译失败 | walkerEngine | 返回 error("invalid regex: ...") |
| 文件无法打开 | 两者 | 跳过该文件，继续遍历 |
| 文件非 UTF-8 | walkerEngine | 跳过该文件（isUTF8 检查） |
| 文件超过 5MB | 两者 | 跳过该文件 |
| 路径转换失败 | 两者 | 使用绝对路径作为降级 |
| .gitignore 不存在 | 两者 | 使用空 pattern 列表（不忽略任何文件） |
| rg JSON 解析失败 | ripgrepEngine | 跳过该行，继续解析下一行 |
| safePath 路径越界 | 两者 | 返回 error("access denied: path escapes workspace") |

## 15. 测试要点

### 15.1 单元测试

1. **newSearchEngine**:
   - rg 可用 → 返回 ripgrepEngine（含 walker 委托）
   - rg 不可用 → 返回 walkerEngine

2. **matchGlob**:
   - `*.go` 匹配 `main.go` → true
   - `*.go` 匹配 `pkg/main.go` → false（`*` 不跨目录）
   - `**/*.go` 匹配 `a/b/c.go` → true
   - `pkg/*.go` 匹配 `pkg/main.go` → true
   - `pkg/*.go` 匹配 `cmd/main.go` → false
   - `src/**` 匹配 `src/a/b/c` → true
   - 空模式 → 行为验证

3. **parseGitignore**:
   - `#` 注释行 → 跳过
   - 空行 → 跳过
   - `!important.log` → negate=true
   - `build/` → 带路径分隔符 → 相对锚定
   - `*.log` → 无路径分隔符 → 任意深度

4. **isGitignored**:
   - 空 patterns → 不忽略任何文件
   - `*.log` → 忽略所有 .log 文件
   - `!important.log` → 取消忽略 important.log
   - 后出现的规则覆盖先出现的

5. **walkerEngine.Grep**:
   - 基本正则匹配
   - 忽略大小写
   - HeadLimit 截断（默认 250）
   - Offset 跳过
   - 大文件跳过（> 5MB）
   - VCS 目录跳过
   - gitignore 过滤
   - 非 UTF-8 文件跳过
   - 单行截断（> 500 字符）

6. **walkerEngine.Glob**:
   - 基本 glob 匹配
   - `**` 递归匹配
   - VCS 目录排除
   - gitignore 过滤
   - 结果排序

7. **ripgrepEngine.Grep**:
   - rg JSON 输出解析（match / summary 事件）
   - 三种 OutputMode 聚合
   - rg 退出码 1（无匹配）→ 空结果
   - rg 退出码 2（错误）→ error
   - HeadLimit / Offset 应用

8. **parseRgJSON**:
   - match 事件解析
   - 相对路径转换
   - files_with_matches 去重
   - count 模式统计
   - content 模式保留行信息
   - Truncated 标记

### 15.2 集成测试

1. **端到端搜索**:
   - 创建测试目录结构（含子目录、.gitignore、大文件、二进制文件）
   - 使用 Grep 搜索 → 验证结果正确性
   - 使用 Glob 搜索 → 验证结果正确性

2. **引擎一致性**:
   - 相同输入 → ripgrepEngine 和 walkerEngine 结果一致
   - 可通过 mock exec.LookPath 强制切换引擎

3. **gitignore 集成**:
   - 创建多层 .gitignore
   - 验证被忽略的文件不出现在结果中
   - 验证否定规则生效

4. **大文件处理**:
   - 创建 > 5MB 文件
   - 验证被跳过（两种引擎均跳过）

5. **路径处理**:
   - 所有结果路径相对于 root
   - 使用正斜杠分隔
   - filepath.Rel 正确

### 15.3 边界条件

1. 空目录 → 空结果（不报错）
2. 空搜索模式 → rg 可能报错，walker 返回 error
3. 二进制文件 → rg 自动跳过；walker 通过 isUTF8 跳过
4. 符号链接 → rg 默认跟随；walker 需要处理
5. 特殊字符文件名 → 路径正确转义
6. 并发搜索 → 两种引擎都是无状态的，可并发调用
7. 超长行（> 500 字符）→ 截断处理
8. 搜索路径越界（../ 逃逸）→ safePath 拒绝
