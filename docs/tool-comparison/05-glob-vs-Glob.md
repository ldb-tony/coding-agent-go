# 05 · glob vs Claude Code Glob

[返回索引](README.md) ｜ 上一篇：[grep vs Grep](04-grep-vs-Grep.md) ｜ 下一篇：[bash vs Bash](06-bash-vs-Bash.md)

---

## 1. 工具定位与设计哲学

Glob 回答的问题是"**哪些文件存在**"——按文件名模式定位文件，与 Grep（文件**内容**里有什么）正交。CC 的 Glob 极简：2 个入参，但输出是**带元信息的结构化结果**，并且从匹配引擎、路径呈现、并发声明三个方向做 token 与安全优化。它在工具谱里是"最轻量的入口工具"——模型通常先 Glob 定位、再 Read/Grep 收窄。

我们的 `glob` 是六个工具里与 CC **差距最小**的一个：自研的跨目录 `**` 匹配器（[matchGlob/matchSegs L316-340](file:///Users/tony/coding-agent-go/tools.go#L316-L340)）补上了 Go 标准库不支持 `**` 的缺口，有排序、有截断、跳过 .git、单测齐全。差距主要在忽略规则、path 参数和输出契约，而非核心匹配能力。

## 2. 功能描述（description）对比

**Claude Code**（`GlobTool.ts:26-35`）：

- `pattern`：*"The glob pattern to match files against"*
- `path`：*"The directory to search in. If not specified, the current working directory will be used ..."*——把"根目录可指定、默认 cwd"写清楚。

**我们**（[L379-381](file:///Users/tony/coding-agent-go/tools.go#L379-L381)）：

```go
tool("glob", "Find files by pattern relative to the workspace (supports **).",
    map[string]any{"pattern": map[string]string{"type": "string"}},
    []string{"pattern"})
```

质量尚可（点明了相对 workdir 和 `**`），但模型不知道：根目录能否换（不能——没有 path 参数）、会不会尊重 gitignore（实际不会）、结果上限多少（200）。

## 3. API/Schema 逐项对比

| 项 | Claude Code Glob | 我们的 glob | 点评 |
|---|---|---|---|
| 模式 | `pattern` | `pattern` | 引擎不同：CC 用与 rg 一致的 glob 库；我们用自研段匹配 |
| 搜索根 | `path`（可选，默认 cwd） | **无**，固定 workdir 变量（[L202](file:///Users/tony/coding-agent-go/tools.go#L202)） | 无法在子树/外部授权目录中检索 |
| 忽略规则 | 尊重 .gitignore；权限 deny 路径不出现 | 只跳 `.git`（[L207-209](file:///Users/tony/coding-agent-go/tools.go#L207-L209)） | 与 grep 同源问题 |
| 结果上限 | **100** 条（v0.2.x 源码 `globLimits?.maxResults ?? 100`；可被配置覆盖） | **200** 条（[L31](file:///Users/tony/coding-agent-go/tools.go#L30-L34)） | 口径不同，见第 5 节版本说明 |
| 输出 | 结构化：`{filenames, durationMs, numFiles, truncated}` | 纯文本多行 + 截断提示行 | 元信息差异 |
| 路径呈现 | cwd 相对路径（根前缀只出现 0 次而非 N 次） | 也是相对 workdir（[L212-218](file:///Users/tony/coding-agent-go/tools.go#L212-L218)） | **这一点我们天然对齐** |
| 空结果 | `"No files found"` | `(no matches)` | 语义等价 |

CC 输出 schema 原文（`GlobTool.ts:38-45`）：

```ts
z.object({
  durationMs: z.number().describe('Time taken to execute the search'),
  numFiles:   z.number().describe('Total number of files found'),
  filenames:  z.array(z.string()).describe('Array of file paths'),
  truncated:  z.boolean().describe('Whether results were truncated (limited to 100 files)'),
})
```

## 4. 执行机制对比（源码级）

### Claude Code

```
glob(pattern, root, {limit:100, offset:0}, signal, permissionContext)
  → glob 库遍历（与 rg 共享忽略语义：.gitignore/.ignore）
  → 权限上下文过滤（deny 路径不进入结果，搜索不能绕过读权限）
  → files.map(toRelativePath)   // 根路径前缀去重，省 token
  → { filenames, durationMs: Date.now()-start, numFiles, truncated }
截断时在结果末尾追加（GlobTool.ts:186-196）：
  "(Results are truncated. Consider using a more specific path or pattern.)"
并发声明（:76-81）：
  isConcurrencySafe() → true
  isReadOnly()        → true   // 多个 Glob 可并行，模型可同时发 **/*.ts 与 **/*.tsx
```

工程要点：

1. **相对路径是显式 token 优化**：`/Users/noah/project/src/index.ts` → `src/index.ts`，根前缀（可能 30-50 字符）在 100 个结果里重复 100 次。
2. **`truncated` 布尔 + 行动建议**：不只告诉模型"被截了"，还告诉它该怎么做（更具体的 path/pattern）。`durationMs` 让模型（和 harness）能感知慢查询。
3. **并发安全是显式契约**：`isConcurrencySafe/isReadOnly` 是工具基类方法，StreamingToolExecutor 据此决定多个同类调用能否并行调度。只读工具天然安全。

### 我们

[runGlob L197-233](file:///Users/tony/coding-agent-go/tools.go#L197-L233)：

```go
filepath.WalkDir(workdir, func(p, d, err) {
    if d.IsDir() { if d.Name()==".git" { return SkipDir }; return nil }
    rel, _ := filepath.Rel(workdir, p)
    if matchGlob(pattern, filepath.ToSlash(rel)) { matches = append(matches, rel) }
})
sort.Strings(matches)
if len(matches) > 200 {
    matches = append(matches[:200], "... (more matches omitted; narrow the pattern)")
}
```

匹配器本体（[matchSegs L320-340](file:///Users/tony/coding-agent-go/tools.go#L320-L340)）是手写的路径段递归匹配，`**` 可匹配零到多个段，`filepath.Match` 处理段内通配——正确性由 [TestRunGlob](file:///Users/tony/coding-agent-go/tools_test.go)（`*` 单层、`**/*.py` 递归）覆盖，是合格实现。

差异清单：

| 机制 | CC | 我们 |
|---|---|---|
| 搜索根 | 参数化 | 写死 workdir |
| gitignore | 尊重 | 忽略（只跳 .git） |
| deny 过滤 | 有 | 无（safePath 管起点不管结果内容） |
| 上限/翻页 | 100（可配置）；offset 入参存在于内部调用 | 200 硬顶无翻页 |
| 截断表达 | 结构化 truncated + 建议文案 | 文本提示行（我们的建议文案"narrow the pattern"方向其实一致） |
| 耗时/计数 | durationMs/numFiles | 无（模型只能数行数） |
| 并发声明 | 显式 isConcurrencySafe | 事实上安全（纯读），但调度层无契约 |

## 5. 输出契约与边界行为

| 场景 | CC | 我们 |
|---|---|---|
| 空模式 | schema required 拦截 | `Error: pattern is required`（[L198-200](file:///Users/tony/coding-agent-go/tools.go#L198-L200)） |
| 无命中 | `No files found` | `(no matches)` |
| 超上限 | 100 条 + truncated=true + 建议 | 200 条 + 提示行，无法翻页（[L226-228](file:///Users/tony/coding-agent-go/tools.go#L226-L228)） |
| 结果计数 | numFiles 字段（截断时也知道真实意图边界） | 无 |
| 慢查询 | durationMs | 无 |
| `.git` 内容 | 排除 | 排除（[测试 TestRunGlobSkipsGit](file:///Users/tony/coding-agent-go/tools_test.go)） |
| node_modules | gitignore 命中则排除 | **照常返回** |

**版本说明**：100（v0.2.x GlobTool 源码默认）与 200（我们的 maxListResults）不是谁对谁错；v2.1.x 官方文档未在工具页给出固定数字，限制可经配置调整。报告比较的是"结构化截断 + 可配置/翻页"这套契约，而非具体数值。

## 6. 状态、安全与权限

- Glob 是 CC 权限矩阵中工作目录内**免确认**的只读工具，但 deny 的路径模式直接从结果中过滤——与 Grep 共享同一套 `getFileReadIgnorePatterns`，文件名也不能泄露秘密目录的存在。
- 工具显式声明 read-only/concurrency-safe，调度器可安全并行（模型一次响应里发多个 Glob 时不需要串行化）。
- 我们：safePath 在 Glob 中甚至没有用武之地（根固定），结果集无权限过滤；[agent.go](file:///Users/tony/coding-agent-go/agent.go#L24-L49) 对多个工具调用本就按顺序逐个执行，当前没有并行调度，所以并发契约暂时不紧迫，但未来并行化时会需要。

## 7. 效果对比（量化场景推演，估算）

### 场景 A：在 Go 后端仓库找配置文件 `**/*config*.go`，同时忽略规则里有 `vendor/`

- **CC**：gitignore 生效，返回 4 个真实项目文件（~80 token），numFiles=4，无截断。
- **我们**：vendor 里可能带几十个 `*config*`（依赖库的配置模块），结果被依赖刷屏，真实文件混在中间（我们按文件名字典序，[L224](file:///Users/tony/coding-agent-go/tools.go#L224)，与"相关性/新鲜度"无关）；超过 200 个就截断且无翻页。模型还要逐个路径用常识剔除 vendor——把忽略规则的判断成本转嫁给了模型的推理 token。
- 量级：200 行路径 ≈ 3-5K token vs 4 行 ≈ 80 token，差距约 40-60 倍，且常驻历史每轮重复计费。

### 场景 B：只想在 `web/src/` 子树找文件

- **CC**：`path="web/src"` 直接收窄，遍历范围小、输出短。
- **我们**：没有 path 参数，只能用模式前缀模拟（`web/src/**/*.tsx`）——能做，但描述里没有这个约定，模型未必稳定使用；且 WalkDir 仍全量遍历 workdir（模式只在收结果时过滤），**IO 成本没有收窄**。

### 场景 C：结果被截断后的决策

- **CC**：truncated=true + numFiles + "用更具体的 path/pattern"建议，模型可以换更窄模式（这是协议化的一次自纠正）。
- **我们**：看到提示行也能自纠正（文案有引导），但不知道总命中数、不知道换了模式后是否仍漏，只能试错。

## 8. 差距清单评级

| 维度 | Claude Code | 我们 | 影响 | 优先级 |
|---|---|---|---|---|
| gitignore 尊重 | 有 | 无 | 依赖目录噪音，主场景质量 | **P0**（与 grep 共用引擎解决） |
| path 参数 | 有 | 无 | 子树检索只能模式模拟，IO 不省 | **P1** |
| 结构化输出 | truncated/numFiles/durationMs | 纯文本 | 截断后决策信息不足 | P1 |
| 翻页 | offset（内部能力） | 无 | 硬顶不可达 | P2（与 grep 分页统一） |
| 并发安全契约 | isConcurrencySafe/isReadOnly | 事实安全无声明 | 并行调度化时补 | P2 |
| 路径相对化 | 有 | 有 | 已对齐 | —— |
| `**` 匹配 | 库实现 | 自研递归（正确、有测试） | 已基本对齐 | —— |

## 9. 对齐设计草案（仅设计，不实施）

与 Grep 报告第 9 节的搜索引擎方案共用基础设施（忽略规则解析、VCS 排除、权限过滤）。

### Schema 变更

```go
// glob
//   pattern string 必填
//   path    string 可选（safePath 校验，默认 workdir）
```

### 输出结构

```go
type listResult struct {
    Items     []string `json:"items"`
    NumFiles  int      `json:"numFiles"`
    Truncated bool     `json:"truncated"`
    DurationMs int64   `json:"durationMs"`
}
```

工具结果文本建议保持人类/模型双可读：

```
<items joined by \n>
... (truncated: matched %d, showing %d; narrow the pattern or set a path)
```

numFiles 用真实命中数（遍历计数），让模型判断"换窄模式够不够"。

### 引擎复用

- rg 可用时：Glob 也可以直接用 `rg --files -g '<pattern>'` 或 `fd` 思路实现（CC 本身在类 Unix 默认不挂载 Glob，鼓励 shell）；回退引擎用现有 matchGlob 但 WalkDir 根改为参数化 path，并接入 gitignore matcher。
- VCS 排除扩到 6 种目录（当前只有 .git）。

### 测试清单

- 新增 path：子树搜索、path 为文件、path 越界拒绝
- gitignore 夹具（vendor/node_modules/嵌套 .gitignore）下不返回被忽略文件
- 截断输出含真实 numFiles 与提示；现有 `TestRunGlobTruncation` 更新为新契约
- VCS 六种目录排除
- 相对路径输出保持不变（现有 doublestar/star 用例不改预期）

### 破坏性变更

- 结果文本格式变化会影响现有断言（[tools_test.go](file:///Users/tony/coding-agent-go/tools_test.go) 多处 `runGlob(...) == "..."` 精确匹配需调整）。
- 上限数值若从 200 改为 100 需同步改 `TestRunGlobTruncation`；建议保留 200（数值不是差距核心）。

## 10. 参考资料

- [Claude Code Tools reference — Glob tool behavior（平台默认挂载策略）](https://code.claude.com/docs/en/tools-reference#glob-tool-behavior)
- [源码解构 23：GlobTool schema、100 文件截断、路径相对化、并发安全声明](https://claude-harness.dev/en/articles/23-search-system)
- 我方实现：[runGlob L197-233](file:///Users/tony/coding-agent-go/tools.go#L197-L233)、[matchGlob/matchSegs L316-340](file:///Users/tony/coding-agent-go/tools.go#L316-L340)、[globHandler L116-124](file:///Users/tony/coding-agent-go/tools.go#L116-L124)
