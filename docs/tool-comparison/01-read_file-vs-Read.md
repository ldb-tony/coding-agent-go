# 01 · read_file vs Claude Code Read

[返回索引](README.md) ｜ 下一篇：[write_file vs Write](02-write_file-vs-Write.md)

---

## 1. 工具定位与设计哲学

**Claude Code 的 Read 不是 `cat` 的包装，而是一个"多格式、token 感知的文件读取引擎"。** 它要同时解决四件事：① 把文件内容变成带稳定坐标（行号）的模型输入；② 在有限上下文里控制每次读取的预算；③ 支持代码之外的格式（图片、PDF、Notebook）；④ 为 Write/Edit 的安全门控提供"已读状态"。

我们的 `read_file` 定位是教程 s02 的最小实现——"读文本文件、可选限制行数"，设计重心是**能跑通 agent loop**，不承担预算、坐标、状态、多模态任何一项职责。这不是实现质量问题，是定位差异：当前工具是 s02 的及格线，Read 是产品级工具。

## 2. 功能描述（description）对比

**Claude Code**（schema 与 prompt 中拼装的完整描述，原文摘录）：

- `file_path`：*"The absolute path to the file to read"*——在 schema 层就要求绝对路径；
- `offset`：*"The line number to start reading from. Only provide if the file is too large to read at once"*——同时交代了参数语义和**使用时机**；
- `limit`：*"The number of lines to read. Only provide if the file is too large to read at once."*
- Edit 工具的描述还反向引用读取格式：*"The replacement should be implemented as a `old_string`/`new_string` ... The indentation is important"*——读与改的格式契约互相咬合。

**我们**（[tools.go L360-365](file:///Users/tony/coding-agent-go/tools.go#L360-L365)）：

```go
tool("read_file", "Read file contents. Optionally limit the number of lines.", ...)
```

一句话。模型不知道：输出是否带行号、超限会怎样、limit 从哪开始计数、大文件该先读多少。行为契约只能靠模型从输出中"猜"，猜错的代价是一整轮工具往返。

**描述对行为的影响**：CC 的描述把"大文件 → 用 offset/limit 分页"写成显式规范，模型第一次调用就会分页；我们的模型面对 5000 行文件只会全量读，直到撞上 50K rune 截断，拿到一份**没有任何"我被截断了"语义保证**的内容（我们虽有截断标记，见第 5 节，但描述里从未承诺分页协议）。

## 3. API/Schema 逐项对比

| 项 | Claude Code Read | 我们的 read_file | 点评 |
|---|---|---|---|
| 路径参数 | `file_path`，要求**绝对路径** | `path`，相对 workdir（[safePath L37-51](file:///Users/tony/coding-agent-go/tools.go#L37-L51)） | CC 消除 cwd 歧义；我们靠 safePath 收口，可接受但语义更弱 |
| 起始位置 | `offset`（非负整数，行号起点） | **无** | 我们只能"从头读 N 行"，无法翻到文件中段 |
| 行数 | `limit`（正整数），默认前 2000 行 | `limit`，0 表示不限 | CC 默认值本身就是策略；我们的 0=全量把责任推给模型 |
| PDF | `pages`（如 `"1-5"`、`"10-20"`，每次≤20 页） | 无 | 多模态能力，见第 4 节 |
| 必填 | `file_path` | `path` | 一致 |

CC schema 原文（v0.2.x 源码 `FileReadTool.ts:227-243`）：

```ts
z.strictObject({
  file_path: z.string().describe('The absolute path to the file to read'),
  offset: semanticNumber(z.number().int().nonnegative().optional())
    .describe('The line number to start reading from. Only provide if the file is too large to read at once'),
  limit: semanticNumber(z.number().int().positive().optional())
    .describe('The number of lines to read. Only provide if the file is too large to read at once.'),
  pages: z.string().optional() /* PDF page range, max 20 pages */,
})
```

注意 `semanticNumber`：模型偶尔把数字发成字符串（`"2000"`），schema 层直接容错。我们 handler struct 的 `Limit int`（[L83-86](file:///Users/tony/coding-agent-go/tools.go#L83-L86)）遇到字符串数字会静默归零（变成"不限行数"），语义反而危险。

## 4. 执行机制对比（源码级）

### Claude Code 的读取管线

```
file_path 校验 → 设备路径黑名单 → stat（大小/类型分支）
  ├─ 文本：读切片 → 行号格式化 → 双层预算检查 → 记录 readFileState(mtime, 是否完整读)
  ├─ 图片：Sharp/napi 压缩到 token 预算 → Base64 多模态块
  └─ PDF：按 pages 抽取（≤20 页）
后续重复读：mtime 未变 → 返回 ~30 token 的 FILE_UNCHANGED_STUB
```

关键机制：

1. **双层 token 预算**（`FileReadTool/limits.ts` 注释原文，#21841 实验）：

   | 限额 | 默认 | 检查对象 | 检查成本 | 超限动作 |
   |---|---|---|---|---|
   | maxSizeBytes | **256 KB** | 文件总大小（不是切片大小，注释明确标注这是已知 mismatch） | 1 次 stat | 读前抛错 |
   | maxTokens | **25 000 token** | 实际输出 token | API tokenizer 往返 | 读后抛错 |

   注释记录了一次真实 A/B：显式 limit 读取超字节上限时，曾改为截断返回（issue #21841，2026-03），随后回退——**截断让工具错误率下降，但平均 token 上升**：截断仍返回约 25K token 内容，而错误路径只有约 100 字节。让 AI 快速失败、带着 offset 重试，总账单更便宜。

2. **行号是输出本体的一部分**（cat -n 风格），模型后续引用、Edit 定位、与用户讨论都有稳定坐标。

3. **mtime 去重**（`FILE_UNCHANGED_STUB`）：*"File unchanged since last read. The content from the earlier Read tool_result in this conversation is still current — refer to that instead of re-reading."* 约 30 token；检测依赖 `readFileState` 中记录的 mtime。大文件在多轮对话里被重复读取是常见浪费，stub 直接消掉。

4. **设备路径黑名单**：`/dev/zero`、`/dev/random`、`/dev/urandom`、`/dev/full`（无限输出）、`/dev/stdin`、`/dev/tty`（阻塞等待）、stdout/stderr/fd 别名——在读之前就拒绝，防止 agent 被一条工具调用挂死。

5. **多格式输出是判别联合**：文本/图片/PDF/Notebook 各走不同渲染路径，图片压缩后 Base64 进多模态消息，让模型真正"看懂"截图而非报错。

### 我们的读取管线

[runRead L138-159](file:///Users/tony/coding-agent-go/tools.go#L138-L159)：

```go
data, _ := os.ReadFile(abs)            // 整文件一次性读入内存
if !utf8.Valid(data) { return "Error: ... not a UTF-8 text file" }
lines := strings.Split(TrimRight(data,"\n"), "\n")
if limit > 0 && limit < len(lines) { lines = lines[:limit] }  // 只能从头截
output := Join(lines, "\n")
if runes := []rune(output); len(runes) > maxReadRunes {       // 50000，L32
    output = string(runes[:maxReadRunes]) + "\n... (output truncated)"
}
```

差异点：无行号；无 offset；按**字符数**而非 token 做预算（中文/代码密度差异下口径不准）；整文件读进内存后才截断（大文件的 IO 与内存成本照付）；无状态所以无去重；UTF-8 之外一律报错（图片/PDF 无路径）；无设备文件防护（读 `/dev/zero` 会先撑爆内存/字符数——虽然 safePath 允许绝对路径只要在 workdir 内，workdir 内一般没有 /dev，但相对路径构造不到它，实际风险低，属纵深防御缺口）。

## 5. 输出契约与边界行为

| 场景 | Claude Code Read | 我们的 read_file |
|---|---|---|
| 正常读取 | 带行号文本（默认≤2000 行） | 无行号纯文本（[L150-154](file:///Users/tony/coding-agent-go/tools.go#L150-L154)） |
| 文件过大 | 读前/读后抛**短错误**，文案直接指导 offset/limit 分页 | 截到 50K rune + `... (output truncated)`（[L155-157](file:///Users/tony/coding-agent-go/tools.go#L155-L157)） |
| 超限后怎么办 | 错误是协议信号，模型立刻发 offset 重试 | 有截断标记但**无分页能力**（没 offset），模型除了"盲改/盲总结"没有正式恢复路径 |
| 重复读同一文件 | mtime 未变 → ~30 token stub | 每次全量重发 |
| 文件不存在 | 错误 | `"Error: " + err`（[L144-146](file:///Users/tony/coding-agent-go/tools.go#L144-L146)） |
| 二进制/非文本 | 图片/PDF 正常支持，其他二进制按类型处理 | 一律 `not a UTF-8 text file`（[L147-149](file:///Users/tony/coding-agent-go/tools.go#L147-L149)） |
| 空文件 | 空结果 | `TrimRight` 后 `Split` 得到一个 `""`，输出为空字符串，无明确契约 |
| 字符串数字参数 | semanticNumber 容错 | `json.Unmarshal` 失败 → limit 归零 = 全量读 |

## 6. 状态、安全与权限

- **harness 状态**：CC 的 Read 在 `readFileState` 写入 `{路径: mtime, isPartialView, content}`，这份状态是 Write/Edit 门控的前提（见 [02 报告](02-write_file-vs-Write.md)、[03 报告](03-edit_file-vs-Edit.md)）。Read 不是只读操作——它在系统里还承担"颁发写权限凭证"的角色。
- **权限**：Read 在工作目录内默认免确认；但 `Read(~/secrets/**)` 路径 deny 规则存在，且 deny 规则同时作用于 Grep/Glob/LSP；工作目录外路径仍会弹窗。
- **我们**：safePath（[L37-51](file:///Users/tony/coding-agent-go/tools.go#L37-L51)）是唯一防线，做到了"不出 workdir"，但没有任何读取状态沉淀，读不读与后续能否写完全无关。

## 7. 效果对比（量化场景推演，估算）

**假设**：代码文本约 4 字符/token；一个 agent loop 往返 ≈ 一次 LLM 调用的延迟与全部输入 token 计费；工具结果进入历史后每轮重复计费。

### 场景 A：阅读一个 5000 行（约 200 KB，~50K token）的源文件，找第 4000 行附近的函数

- **CC**：首次读 → 撞 256KB/2000 行限制，返回约 100 字节错误（1 次往返，~输入 0.5K token）；模型发 `offset=3900, limit=300`，拿到带行号的 300 行（~3K token）。**累计 2 次往返、约 3.5K token 新增上下文**，且坐标精确。
- **我们**：一次全量读 → 50K rune 截断（假设保留约前 12000 个 token 的内容，**目标函数在第 4000 行，根本不在返回里**）。模型若聪明，会再用 grep 曲线定位（多 1 次工具往返）；若不聪明，直接基于前 12K token 的错误切片回答或编辑。**累计 1-2 次往返、~12K token 常驻上下文，且前 50K rune 的内容在之后每轮重复计费**。若后续还有 5 轮对话，这 12K token 实际成本放大到 ~60K+ token 量级。
- 差距本质：CC 把"读多少"做成带预算的协议；我们把整份沉没成本放进上下文。

### 场景 B：多轮任务中同一个 800 行文件被读 3 次（读→改别处→再确认→再改）

- **CC**：第 1 次 ~8K token；第 2、3 次 mtime 未变各 ~30 token stub。新增约 8.1K token。
- **我们**：3 × 8K = 24K token 全量进历史。
- 附加：若文件在第 2 次读后被 linter/用户改动，CC 第 3 次读会返回新内容（mtime 变了），后续 Write 还会被陈旧检测拦下；我们毫无感知。

### 场景 C：无匹配语义之外的失败成本

模型误把截图当代码读：CC 返回压缩后的图像内容，模型能直接分析 UI；我们返回一行 Error，模型需要另想办法（通常做不到，任务卡死）。

## 8. 差距清单评级

| 维度 | Claude Code | 我们 | 影响 | 优先级 |
|---|---|---|---|---|
| 行号输出 | cat -n 风格 | 无 | 坐标缺失，Edit/讨论不精确 | **P0** |
| offset 分页 | 有 | 无 | 大文件中段不可达，无正式恢复路径 | **P0** |
| 超限策略 | 短错误 + 分页指引 | 50K rune 静默截断 | 错误切片污染上下文、误导后续推理 | **P0** |
| 预算口径 | 256KB + 25K token 双层 | 50K rune 单层 | 中文/代码密度下口径失真 | P1 |
| 重复读去重 | mtime stub ~30 token | 全量重发 | 多轮任务线性浪费 | P1 |
| 读取状态 | readFileState 供写门控 | 无 | 无法实现 read-before-write | P1（与 Write 同期） |
| 数字参数容错 | semanticNumber | 字符串→静默归零 | 偶发参数序列化错误变成"全量读" | P2 |
| 多模态 | 图片/PDF/Notebook | 非 UTF-8 即报错 | 任务类型受限 | P2 |
| 设备路径防护 | /dev 黑名单 | 无（workdir 约束下风险低） | 纵深防御 | P2 |

## 9. 对齐设计草案（仅设计，不实施）

### Schema 变更

```go
// read_file
//   path     string  必填（沿用 workdir 相对语义）
//   offset   int     可选，1 起始行号；<0 视为参数错误
//   limit    int     可选，默认 defaultReadLines = 2000；显式 0 保留为"逃生舱"需谨慎
```

### 函数/状态草案

```go
const (
    defaultReadLines   = 2000
    maxReadFileBytes   = 256 * 1024   // 读前 stat 总文件大小门槛
    maxReadOutputRunes = 25000        // 简化口径：先按 rune，后续可换 tokenizer
)

type readState struct {
    mtime        time.Time
    fullView     bool
    contentHash  string // 全读时记录，供陈旧检测内容兜底
}

// 会话级状态，挂在 agentLoop 而不是全局
type sessionState struct {
    reads map[string]readState // absPath -> 最近读取
}

func runRead(st *sessionState, path string, offset, limit int) string
```

要点：① 输出改为 `     1→content` 行号格式；② stat 发现文件 >256KB 且未显式给 limit 时返回短错误：`Error: file is N bytes (>256KB). Read with offset/limit (default page 2000 lines).`；③ 每次成功读取后写 `st.reads[abs]`，mtime 未变的重复读返回 stub；④ 数字入参做字符串/数字双容错（自定义 UnmarshalJSON 或先用 `json.Number`）。

### 测试清单（在现有 [tools_test.go](file:///Users/tony/coding-agent-go/tools_test.go) 基础上新增）

- offset/limit 组合的分页正确性、页码越界（offset 超行数返回空+提示）
- 输出每行带行号且行号连续
- >256KB 文件无显式 limit → 短错误，不返回内容
- 截断场景返回错误而非内容（更新现有 `TestRunReadTruncation` 的预期，**破坏性变更**）
- 重复读 mtime 未变 → stub；touch 更新 mtime → 重新返回内容
- 字符串数字 `{"limit":"100"}` 正确解析
- 现有 `TestRunRead` 的"limit 从头截断"用例改为带行号预期

### 破坏性变更提示

- 输出带行号后，所有断言纯文本内容的测试需更新；需在工具描述中明确"行号不是文件内容，Edit 的 old_text 不要包含行号"。
- 超限从"截断返回"改为"抛错"，模型行为会变化——描述中必须写清恢复方式，这与 CC 实测的省 token 结论一致。

## 10. 参考资料

- [Claude Code Tools reference — Read tool behavior](https://code.claude.com/docs/en/tools-reference#read-tool-behavior)
- [源码解构 21：The File Operations Trio（Read 章节，含 limits.ts 原文与 #21841 实验）](https://claude-harness.dev/en/articles/21-file-operations)
- 我方实现：[tools.go runRead L138-159](file:///Users/tony/coding-agent-go/tools.go#L138-L159)、[safePath L37-51](file:///Users/tony/coding-agent-go/tools.go#L37-L51)、[readHandler L82-91](file:///Users/tony/coding-agent-go/tools.go#L82-L91)
