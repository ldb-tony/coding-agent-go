# 04 · grep vs Claude Code Grep

[返回索引](README.md) ｜ 上一篇：[edit_file vs Edit](03-edit_file-vs-Edit.md) ｜ 下一篇：[glob vs Glob](05-glob-vs-Glob.md)

---

## 1. 为什么需要专用搜索工具（设计哲学）

claude-harness.dev 在搜索系统篇开篇给了直接答案：`find`/`grep` 走 bash 有三个不可接受——① **权限失控**（shell 绕过权限系统）；② **输出失控**（`grep -r x .` 可能返回几 MB，灌爆上下文）；③ **格式不友好**（命令行格式对模型不友好）。

CC 的 Grep 是 **ripgrep 的薄封装**，哲学是"把 ripgrep 的核心能力直接暴露给模型，而不是过度抽象"——13 个参数几乎是 rg 命令行的 1:1 映射。它要解决的是 AI 面对陌生代码库的第一个问题："代码在哪儿"，并且用**两阶段收窄**（先 `files_with_matches` 定位文件，再 Read 细读）控制 token。

我们的 grep 是教学实现：用 Go `regexp` + `filepath.WalkDir` 自己造了一个搜索引擎（[runGrep L235-313](file:///Users/tony/coding-agent-go/tools.go#L235-L313)），参数 3 个、输出一种、不尊重忽略规则。这是六个工具中**工程代差最大**的一个。

## 2. 功能描述（description）对比

**Claude Code**：每个参数都有一句话描述，模型看到的是 rg 专家说明书：

- `pattern`：*"The regular expression pattern to search for"*
- `output_mode`：`content | files_with_matches | count` 枚举——模型从 schema 就知道有三种粒度
- `-A/-B/-C`：上下文行数；`-i` 大小写；`-n` 行号（content 模式默认 true）
- `head_limit`：*"Limit output to first N lines/entries, equivalent to | head -N. Works across all output modes ... When unspecified, shows all results from ripgrep."*（v2 Agent SDK 描述）
- `type`：语言类型过滤（js/py/rust…）；`multiline`：跨行匹配

**我们**（[L382-388](file:///Users/tony/coding-agent-go/tools.go#L382-L388)）：

```go
tool("grep", "Search file contents with a regex. Optional path to search in,
     optional glob filter for filenames.", ...)
```

模型不知道：输出上限是多少、被截断后怎么办、能否忽略大小写、默认搜哪些目录（会不会包含 node_modules）。其中"会不会尊重 gitignore"是模型对 rg/grep 类工具的**既有心智模型**，我们的实际行为（不尊重，只跳 .git，[L267-269](file:///Users/tony/coding-agent-go/tools.go#L267-L269)）与该预期错位，错误更隐蔽。

## 3. API/Schema 逐项对比

### CC Grep 的 13 个参数（`GrepTool.ts:33-89`）

| 参数 | 类型/取值 | 我们是否有 |
|---|---|---|
| `pattern` | 正则 | 有（Go RE2 正则，[L239](file:///Users/tony/coding-agent-go/tools.go#L239)） |
| `path` | 文件或目录 | 有（[L243-250](file:///Users/tony/coding-agent-go/tools.go#L243-L250)） |
| `glob` | rg glob 过滤（可重复） | **自造 `include`**（[L283](file:///Users/tony/coding-agent-go/tools.go#L283)：basename **或** 完整 rel 命中即可，语义与 rg 不同） |
| `output_mode` | `content` / `files_with_matches` / `count` | 无（永远 content 风格） |
| `-B` / `-A` / `-C` / `context` | 上下文行数 | 无 |
| `-n` | 行号 | 无开关（输出固定带行号） |
| `-i` | 忽略大小写 | 无 |
| `type` | rg 内置语言类型表 | 无 |
| `head_limit` | 条目上限（默认 250） | 无（固定 200，[常量表 L31](file:///Users/tony/coding-agent-go/tools.go#L30-L34)） |
| `offset` | 跳过前 N 条（翻页） | 无 |
| `multiline` | `-U --multiline-dotall` 跨行 | 无 |

我们的 schema 见 [L382-388](file:///Users/tony/coding-agent-go/tools.go#L382-L388)，handler struct 见 [L127-131](file:///Users/tony/coding-agent-go/tools.go#L127-L131)。

### 输出模式：三种粒度 vs 一种

| mode | 形态 | 用途 | 排序 |
|---|---|---|---|
| `files_with_matches`（**默认**） | 文件路径列表 | 先定位"哪些文件" | **按 mtime 降序**，最近改动的在前；测试环境按文件名保证确定性（`GrepTool.ts:529-553`） |
| `content` | 匹配行（+上下文） | 定位到行 | rg 顺序 |
| `count` | `文件:次数` | 评估改动范围/命中规模 | —— |

我们永远输出 `relpath:行号:行内容`（[L258](file:///Users/tony/coding-agent-go/tools.go#L258)），相当于只有 content，且无法先回答"大概涉及几个文件"这种廉价问题——count 模式本可以用极短输出替代一次全量 content 搜索。

## 4. 执行机制对比（源码级）

### CC：参数 → rg argv → 结构化分页

```
pattern/path/glob/type/context/-i/-n/multiline
→ 构造 rg 命令行（GrepTool.ts:329-441）：
    rg --hidden
       --glob '!.git' --glob '!.svn' --glob '!.hg' --glob '!.bzr' --glob '!.jj' --glob '!.sl'
       --max-columns 500
       [-U --multiline-dotall]
       --json（按 mode 决定字段）
→ 默认尊重 .gitignore / .ignore（rg 内建，含编码检测、二进制跳过、并行遍历）
→ 权限 deny 路径转换为 --glob '!**/pattern' 排除（normalizePatternsToPath, :412-427）
→ files_with_matches：对结果批量 stat（Promise.allSettled），按 mtimeMs 降序排序
→ applyHeadLimit：默认 250 条，offset 翻页；limit=0 为不限逃生舱；
                  仅真实截断时回传 appliedLimit 信号（:106-128）
```

细节逐条：

1. **`--hidden` 但不搜 VCS 目录**：隐藏文件默认搜（dotfile 配置常是目标），6 种 VCS 目录（Git/SVN/Mercurial/Bazaar/Jujutsu/Sapling）一律排除——搜 `.git` 几乎永远只有噪音。
2. **`--max-columns 500`**：单行超过 500 列的匹配不灌进结果——base64、压缩产物、source map 一行几万字符，一次命中就可以废掉一轮预算。
3. **忽略规则是权限系统的一部分**：deny 规则在搜索时同样生效，模型无法用 grep "侧读"被 deny 的文件路径内容；非绝对路径要补 `**/` 前缀（rg 只在工作目录下应用 gitignore 语义）。
4. **mtime 排序的产品假设**：用户最关心最近活跃的文件，命中文件按新鲜度排。
5. **分页协议完整**：默认 250 上限 + offset；只有真的截断时结果里才出现 appliedLimit，模型收到信号就知道可以 offset 翻页；显式 0 表示"我知道风险，给我全部"。
6. **平台策略（v2.1.x 官方表格）**：Grep/Glob 在 macOS/Linux/WSL 上**默认不挂载**（这些平台可直接在 Bash 用 rg/fd），Windows 默认提供。这体现"专用工具与 shell 互补、按环境裁剪"。

### 我们：WalkDir + 逐文件正则

[runGrep L235-313](file:///Users/tony/coding-agent-go/tools.go#L235-L313) 的实际行为：

```
编译 Go 正则（非法正则报错，有，L239-242）
→ 确定 root（path 可指向文件或目录，safePath 校验，L243-254）
→ filepath.WalkDir(root, scan)：
    目录：只跳 .git（L266-269）——不看 .gitignore/.ignore
    非普通文件跳过；>5MB 文件跳过（L275）
    include 过滤：matchGlob(include, basename) || matchGlob(include, rel)（L283）
    读整文件 → utf8.Valid → 逐行 regexp 匹配 → "rel:lineno:line"
    集满 200 条 → append "... (more matches omitted)" 后 SkipAll（L291-294）
```

| 机制 | CC（rg） | 我们 |
|---|---|---|
| 忽略规则 | gitignore/ignore/exclude + VCS 6 种 + 权限 deny | 只跳 `.git` 一个目录 |
| 隐藏文件 | `--hidden` 搜 | WalkDir 默认就搜 dotfile（碰巧接近） |
| 二进制 | rg 自动编码/二进制检测 | UTF-8 校验后跳过（[L287-289](file:///Users/tony/coding-agent-go/tools.go#L287-L289)），合理但粗糙 |
| 大文件 | 无硬性 5MB 概念（rg 流式读） | >5MB 直接不搜（[L275](file:///Users/tony/coding-agent-go/tools.go#L275)），大源码/生成文件漏报 |
| 长行防护 | max-columns 500 | 无 |
| 遍历性能 | rg：内存映射/并行/字面量优化 | 每文件整读串行，node_modules 场景慢 1-2 个数量级 |
| 正则方言 | Rust regex | Go RE2（大体兼容，零宽/回溯特性有差异） |
| 输出粒度 | 三模式 | 一种 |
| 分页 | offset + 250 + 截断信号 | 200 硬顶，无法看第 201 条之后 |
| 上下文行 | -A/-B/-C | 无 |

## 5. 输出契约与边界行为

| 场景 | CC | 我们 |
|---|---|---|
| 无命中 | 空集（rg 退出码 1 被语义化为"无匹配"，见 [bash 报告](06-bash-vs-Bash.md)） | `(no matches)`（[L309-311](file:///Users/tony/coding-agent-go/tools.go#L309-L311)） |
| 非法正则 | rg 报错透传 | `Error: invalid regex`（[L240-242](file:///Users/tony/coding-agent-go/tools.go#L240-L242)） |
| 超上限 | 250 条 + appliedLimit 信号 + offset 可翻页 | 200 条 + `... (more matches omitted)`（[L292-293](file:///Users/tony/coding-agent-go/tools.go#L292-L293)），**无法继续** |
| 长行 | 500 列截断 | 整行进结果 |
| path 越界 | 权限规则拒绝 | safePath 拒绝（有测试） |
| path 是文件 | 单文件搜索 | 支持（[L304-308](file:///Users/tony/coding-agent-go/tools.go#L304-L308)） |
| 结果数量级预览 | count 模式（短输出） | 不支持，只能全量搜完自己数 |

## 6. 状态、安全与权限

- **权限联动是 Grep 最重要的安全属性**：CC 的文件 deny 规则会转成 rg 排除参数，搜索无法成为绕过读权限的侧信道。我们的 grep 与 safePath 只检查 `path` 起点，include 也只是正向过滤——workdir 内任何文件都可被搜索到内容（包括未来若引入 secrets 目录概念时）。
- Grep 在 CC 中声明为 read-only、工作目录内免确认；工具按风险分级治理。
- 我们无状态、无权限分级。

## 7. 效果对比（量化场景推演，估算）

### 场景 A：在典型前端/Node 仓库（含 node_modules ~5 万文件）搜 `API_BASE_URL`

- **CC**：rg 尊重 .gitignore，真实源码可能 3 个文件命中，files_with_matches 输出 3 行路径（~60 token，毫秒级）；模型再 Read 目标文件。count/content 按需切换。
- **我们**：WalkDir 遍历 5 万文件、串行读盘 + 逐行正则；node_modules 里打包产物的注释/README 可能命中几十上百条，结果瞬间撞 200 上限——**真实源码的命中可能被排在依赖目录之后直接挤出窗口**（我们按 walk 顺序收集，无 mtime/相关性排序）。输出几百行（粗估 3-8K token）全是噪音；耗时秒到十几秒，且无法翻页验证"是不是漏了"。
- 这不是边角场景，而是搜索工具的主战场。

### 场景 B：评估一个标识符在仓库中的影响面（重构前问"多少处用到"）

- **CC**：`output_mode=count`，输出 `file:N` 列表，可能 10 行 ~200 token，模型立刻评估规模。
- **我们**：只能全量 content 搜索，200 处上限后被截断（[TestRunGrepTruncation](file:///Users/tony/coding-agent-go/tools_test.go) 已验证该行为），既数不准总数也读不全位置；模型可能被迫再用 bash `grep -c`——专用工具反而被 bash 取代。

### 场景 C：需要上下文判断（匹配行本身看不懂，要看前后 3 行）

- **CC**：`-C 3` 一次拿到带上下文的结果，1 次调用。
- **我们**：拿到行号后再对每个文件发带 offset 的 read_file……但我们**连 offset 都没有**（[01 报告](01-read_file-vs-Read.md)），只能整文件读后自己找。一次本可 ~1K token 的操作膨胀为多文件全量读取。

### 场景 D：token 持续性

工具结果常驻历史。files_with_matches 的 3 行路径在后续 10 轮里成本 ~600 token；我们的噪音结果 5K token 在 10 轮里是 50K。搜索输出的信噪比会在多轮任务中复利放大。

## 8. 差距清单评级

| 维度 | Claude Code | 我们 | 影响 | 优先级 |
|---|---|---|---|---|
| gitignore/ignore 尊重 | 默认 | 无 | 主战场（大仓库）结果不可用 | **P0** |
| output_mode 三态 | files/content/count | 仅 content | 定位/估规模成本高 | **P0** |
| 分页 | offset + 250 + 截断信号 | 200 硬顶不可翻 | 漏结果且无感知 | **P0** |
| 上下文行 | -A/-B/-C | 无 | 命中可读性差，连锁多花读取 | P1 |
| -i / type / multiline | 有 | 无 | 表达力 | P1 |
| 长行防护 | 500 列 | 无 | 单行灌爆预算 | P1 |
| 大文件策略 | 流式可搜 | >5MB 跳过 | 漏报 | P2 |
| 结果排序 | files 模式按 mtime | walk 顺序 | 新鲜结果被噪音淹没 | P1 |
| 权限排除联动 | deny→rg glob | 无 | 侧信道 | P1（随 s03） |
| include 语义 | rg glob（心智一致） | basename/rel 自造或匹配 | 结果反直觉 | P1 |
| 引擎性能 | rg 并行/mmap | 串行整读 | 大仓库延迟 | P0（与 gitignore 同期被 rg 方案一并解决） |

## 9. 对齐设计草案（仅设计，不实施）

### 引擎策略：运行时探测 rg + 自研回退

```go
type searchEngine interface {
    Grep(req grepRequest) (*grepResult, error)
}
// grepRequest: Pattern, Root, Globs []string, Type string, OutputMode(content|files|count),
//              Before, After, IgnoreCase, LineNumbers bool, Multiline bool,
//              HeadLimit int (默认250), Offset int

func newSearchEngine() searchEngine {
    if p, err := exec.LookPath("rg"); err == nil { return ripgrepEngine{bin: p} }
    return walkerEngine{} // 回退：Go 实现，必须补 gitignore 解析
}
```

- **ripgrepEngine**：按第 4 节的 argv 映射拼装；`--json` 解析输出；files 模式自己 stat 按 mtime 排；VCS 六目录排除；max-columns 500。
- **walkerEngine（回退）**：保留现有 WalkDir 骨架，但引入最小 gitignore 支持（选项：纯 Go 库如 `go-gitignore`，或自实现约 100 行的目录级 `.gitignore` 匹配）；补齐 output_mode、head_limit/offset、-i、-C、长行截断。语义必须与 rg 路径一致，夹具测试同时对两个引擎跑同一套用例。

### Schema 变更（对齐命名）

`grep` 入参改为：`pattern`(必填), `path`, `glob`（替代 `include`，**破坏性更名**，保留一版兼容或直接随里程碑更新）, `output_mode`(枚举), `-i/-n/-A/-B/-C`（JSON 属性名建议用 `ignore_case`/`context_before` 等，Go 侧不必照搬横杠）、`head_limit`、`offset`。

### 输出契约

- files 模式：路径行 + mtime 排序；count：`path:n`；content：`path:line:col:text` + 可选上下文块
- 截断时末尾行统一为 `... (N more matches; use offset to page)`，让翻页成为协议

### 测试清单

- 夹具仓库：`node_modules/`、`.gitignore` 嵌套规则、hidden 文件、`.git/`、二进制、超长单行文件、>5MB 文件
- 三模式各自输出与排序（files 模式用可控 mtime 夹具）
- head_limit/offset 翻页、limit=0、截断信号只在真截断时出现
- -i、-A/-B/-C、glob 过滤、非法正则、path 文件/目录
- **双引擎参数化测试**：同一组用例分别对 rg 引擎和 walker 回退断言
- 更新现有 [TestRunGrep](file:///Users/tony/coding-agent-go/tools_test.go) 全部用例的输出格式预期

### 风险与破坏性变更

- 外部依赖：rg 不在环境里时回退路径必须保持一等公民（CI/教学环境零依赖可跑）。
- `include` → `glob` 更名、默认输出从 content 变 files_with_matches 都是模型可感知变更，工具描述需重写并真实试玩校准。
- 正则方言：Go RE2 与 Rust regex 极少数语法差异在回退引擎文档中注明。

## 10. 参考资料

- [Claude Code Tools reference — Grep tool behavior（含平台默认挂载策略）](https://code.claude.com/docs/en/tools-reference#grep-tool-behavior)
- [Agent SDK 参考：Grep 参数（output_mode/-A/-B/-C/head_limit）](https://platform.claude.com/docs/en/agents-and-tools)
- [源码解构 23：GrepTool 完整 schema、argv 构造、250 分页、mtime 排序、deny 联动](https://claude-harness.dev/en/articles/23-search-system)
- 我方实现：[runGrep L235-313](file:///Users/tony/coding-agent-go/tools.go#L235-L313)、[grepHandler L126-136](file:///Users/tony/coding-agent-go/tools.go#L126-L136)、[现有 grep 测试](file:///Users/tony/coding-agent-go/tools_test.go)
