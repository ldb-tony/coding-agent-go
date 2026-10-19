# 02 · write_file vs Claude Code Write

[返回索引](README.md) ｜ 上一篇：[read_file vs Read](01-read_file-vs-Read.md) ｜ 下一篇：[edit_file vs Edit](03-edit_file-vs-Edit.md)

---

## 1. 工具定位与设计哲学

Write 是"全量替换"型工具：把模型给出的完整内容整体落盘。正因为它**破坏性最强**（一次调用抹掉整个文件），Claude Code 的设计重心不是写本身，而是**写之前的确认机制**——核心原则一句话：

> **You cannot write to a file you haven't read.**（你不能写一个你没读过的文件。）

再加上"读过之后文件若已被改动也要拒写"，以及落盘的原子性和写后与 LSP 的联动。Write 在 CC 里是一个被状态门控保护的危险操作。

我们的 `write_file` 延续教程 s02 的定位："让模型能创建文件"，[runWrite](file:///Users/tony/coding-agent-go/tools.go#L161-L173) 只有 13 行，核心是 `MkdirAll + WriteFile`。方便、够用，但"模型基于错误想象整文件覆盖代码"这一类事故在架构上完全敞开。

## 2. 功能描述（description）对比

**Claude Code** 的 Write 描述把前置条件直接写进契约（官方 Tools reference 的 Write tool behavior 与源码校验信息）：

- *"File has not been read yet. Read it first before writing to it."*（errorCode 2 拒绝文案）
- *"File has been modified since read, either by the user or by a linter. Read it again before attempting to write it."*（errorCode 3 拒绝文案）
- 工具描述层要求：覆盖已存在文件前必须先 Read；新建文件无需先读。

**我们**（[tools.go L366-371](file:///Users/tony/coding-agent-go/tools.go#L366-L371)）：

```go
tool("write_file", "Write content to a file, overwriting it.",
    map[string]any{
        "path":    map[string]string{"type": "string"},
        "content": map[string]string{"type": "string"},
    }, ...)
```

"overwriting it"只陈述了能力，没有陈述任何前置义务。模型收到这条描述得到的信号是"随时可写"，这正是 CC 明确要阻止的行为模式。

## 3. API/Schema 逐项对比

| 项 | Claude Code Write | 我们的 write_file | 点评 |
|---|---|---|---|
| 路径 | `file_path`（绝对路径） | `path`（safePath 收口） | 同 Read |
| 内容 | `content`（完整新内容，强制 LF 行尾写出） | `content` | CC 落盘统一行尾，消除 CRLF 噪音 |
| 前置状态 | 已存在文件必须有"全量读取"记录且未陈旧 | 无 | **核心差距** |
| 必填 | file_path, content | path, content | 一致 |
| 返回 | 成功确认 | `Wrote N bytes to path`（[L172](file:///Users/tony/coding-agent-go/tools.go#L172)） | 我们的字节数回执不错，CC 无此细节 |

## 4. 执行机制对比（源码级）

### Claude Code：validateInput → 同步临界区 → 原子落盘 → LSP 通知

**① Read-before-Write 校验**（`FileWriteTool.ts:196-222`）：

```ts
const readTimestamp = toolUseContext.readFileState.get(fullFilePath)
if (!readTimestamp || readTimestamp.isPartialView) {
  return { result: false,
    message: 'File has not been read yet. Read it first before writing to it.',
    errorCode: 2 }
}
const lastWriteTime = Math.floor(fileMtimeMs)
if (lastWriteTime > readTimestamp.timestamp) {
  return { result: false,
    message: 'File has been modified since read ... Read it again ...',
    errorCode: 3 }
}
```

| errorCode | 触发场景 | 防御的事故 |
|---|---|---|
| 2 | 没读过，或只用 offset/limit 读过**部分视图** | 模型不知道文件全貌，整文件写入会摧毁既有代码 |
| 3 | 读取之后文件被用户、linter、其他工具改过 | 基于陈旧认知覆盖，丢失并发改动 |
| 0 | 团队记忆文件含密钥 | 防敏感信息写入共享位置 |

注意"部分读不算读"（`isPartialView`）：只看过第 100-200 行不代表有权覆盖全文。

**② 写入前二次状态确认 + 原子性**（`FileWriteTool.ts:267-305`）：

```ts
// Load current state and confirm no changes since last read.
// Please avoid async operations between here and writing to disk to preserve atomicity.
let meta = readFileSyncWithMetadata(fullFilePath)  // ENOENT → null（新建）
if (meta !== null) {
  if (lastWriteTime > lastRead.timestamp) {
    const isFullRead = lastRead.offset === undefined && lastRead.limit === undefined
    if (!isFullRead || meta.content !== lastRead.content) {
      throw new Error(FILE_UNEXPECTEDLY_MODIFIED_ERROR)  // Windows mtime 误报时用内容兜底
    }
  }
}
writeTextContent(fullFilePath, content, enc, 'LF')
```

两个工程细节：

- **读状态→写盘之间禁异步**：同步临界区消除并发写竞态。
- **Windows 兜底**：云盘同步/杀毒会让 mtime 变化但内容没变；对"全量读过"的文件额外做内容比对，避免误拒。这是真实跨平台踩坑后的补丁。

**③ 写后通知 LSP**（`FileWriteTool.ts:308-326`）：向语言服务器发 `didChange` + `didSave`，TypeScript 等服务器立刻重新生成诊断——模型**下一轮**就可能看到新引入的类型错误。工具结果不只是文本确认，还驱动了反馈环。

### 我们：13 行直写

[runWrite L161-173](file:///Users/tony/coding-agent-go/tools.go#L161-L173)：

```go
abs, err := safePath(path)
os.MkdirAll(filepath.Dir(abs), 0o755)
os.WriteFile(abs, []byte(content), 0o644)
return fmt.Sprintf("Wrote %d bytes to %s", len(content), path)
```

| CC 机制 | 我们有无 | 后果 |
|---|---|---|
| 未读拒写（errorCode 2） | 无 | 模型可凭想象覆盖未读过的文件 |
| 陈旧拒写（errorCode 3） | 无 | 与用户/linter/并行工具的改动互相丢失 |
| 部分读无全权 | 无（也没有部分读概念） | —— |
| 同步临界区 | 无 | 多工具并行时有写竞态窗口 |
| 原子写（临时文件+rename） | 无，`WriteFile` 直接截断目标 | 写到一半进程崩溃 → 目标文件是半截坏文件 |
| 行尾归一 | 无 | Windows 上可能混入 CRLF |
| LSP 联动 | 无 | 写完没有结构化反馈通道 |

我们做得好的一点：`MkdirAll` 自动建父目录（试玩任务中被真实验证过），CC 对父目录不存在也会创建，这点对齐。

## 5. 输出契约与边界行为

| 场景 | Claude Code Write | 我们 |
|---|---|---|
| 新建文件 | 直接写，无需先读（ENOENT → meta=null 豁免） | 直接写，一致 |
| 覆盖已读未变文件 | 成功，刷新 readFileState | 成功 |
| 覆盖**未读**文件 | errorCode 2 拒绝，文案指引 Read | 静默成功 |
| 覆盖**读后被改**文件 | errorCode 3 拒绝，指引重新 Read | 静默成功，旧改动永久丢失 |
| 部分读后覆盖 | 视为未读，拒绝 | 无此概念 |
| 写中途失败 | 临时文件+rename，目标不被破坏 | `WriteFile` 已截断目标，留下损坏文件 |
| 写后反馈 | LSP didChange/didSave，下一轮有诊断 | 只有字节数字符串 |
| 越界路径 | 权限规则 + 工作目录策略 | safePath 拒绝（[L162-165](file:///Users/tony/coding-agent-go/tools.go#L162-L165)，有测试） |

## 6. 状态、安全与权限

CC 的 Write 门控完全建立在 harness 状态上：

```
Read 成功 → readFileState[path] = {timestamp, isPartialView, content?}
                │
Write/Edit 校验 → 查 state：不存在/部分 → errorCode 2
                → stat mtime > timestamp → errorCode 3（全读再做内容兜底）
外部修改者：用户、linter、另一个并行 agent
```

权限规则侧（v2.1.x 官方）：

- `Edit(/src/**)` 的 allow 规则**隐含授予**同路径 Read 权限；
- 反过来 `Read(~/secrets/**)` 的 deny 规则**同时禁止** Edit/Write 在该路径创建/修改文件（v2.1.208 起对 edit 生效、v2.1.228 起对 write 生效），因为"改完总要能读回来"。读和写在规则层不是独立开关。

我们没有会话状态（[toolHandler](file:///Users/tony/coding-agent-go/tools.go#L54) 签名只有 `json.RawMessage`，拿不到任何上下文），所以这类门控**在当前架构里无处挂载**——这是路线图里程碑 A 要先做 sessionState 的原因。

## 7. 效果对比（量化场景推演，估算）

### 场景 A：模型记错文件名，把 `config.go` 当成要新建的 `config_new.go` 整文件覆盖

- **CC**：`config.go` 存在但从未读过 → errorCode 2，约 20 token 的拒绝；模型 Read 后发现文件不是自己想的，零损失。**1 次失败往返（~20 token）vs 一次事故**。
- **我们**：直接成功，原 config.go 全部内容被替换。发现途径只有用户事后发现/g 测试挂掉，恢复靠 git，对话本身无任何告警。事故成本（排查 + 从 git 找回 + 上下文解释）远超一次工具往返。

### 场景 B：Read → 模型思考两轮 → 期间 gofmt/IDE 自动格式化了文件 → Write

- **CC**：mtime 变化 → errorCode 3 拒绝，模型重新 Read（增量约 8K token，1 次往返），在最新内容上重做修改。
- **我们**：Write 成功，**gofmt 的格式化结果连同模型未感知的任何外部改动一起被覆盖**。这类问题在日志里无痕，调试成本极高。
- 概率注：单 agent 串行场景 B 概率低，但多工具并行（我们已支持一次响应多个 tool_use 顺序执行）、引入 linter hook 后概率显著上升——门控是为并发与自动化环境准备的。

### 场景 C：写大文件时进程被 Ctrl-C / 崩溃

- **CC**：临时文件 rename 模式下，要么旧文件完整、要么新文件完整。
- **我们**：`WriteFile` 先 `O_TRUNC`，写到一半被杀 → 文件存在但是半截；若此时 go build 触发，得到的是语法错误风暴，模型可能误判为代码问题继续改，错误放大。

### token 视角

门控的"成本"是拒绝时多一次 Read（几千 token）；收益是避免一次整文件丢失事故。Read 还是 Write 的显式前置，还带来一个隐性好处：**模型写文件前被强制做一次现状确认**，凭幻觉生成完整文件的行为模式在流程上被打断。

## 8. 差距清单评级

| 维度 | Claude Code | 我们 | 影响 | 优先级 |
|---|---|---|---|---|
| read-before-write | 强制，部分读无全权（errorCode 2） | 无 | 幻觉覆盖/误覆盖事故 | **P0** |
| 陈旧检测 | mtime + 全读内容兜底（errorCode 3） | 无 | 并发/linter 改动丢失 | **P0** |
| 原子写 | 同步临界区 + 临时文件 rename | 直接 WriteFile | 崩溃留半截文件 | **P0** |
| 新建豁免 | ENOENT 不需先读 | 天然满足 | 对齐时别误拦新文件 | 设计约束 |
| 行尾归一 | 强制 LF | 原样写 | 跨平台噪音 diff | P2 |
| LSP 联动 | didChange/didSave 驱动诊断环 | 无 | 反馈滞后 | P2（远期 LSP 主题） |
| 权限联动 | Read deny 连带禁写 | 无 | 安全纵深 | P1（随 s03 权限） |

## 9. 对齐设计草案（仅设计，不实施）

依赖 Read 报告第 9 节的 `sessionState.reads`。

```go
var errFileNotRead = errors.New("File has not been read yet. Read it first before writing to it.")
var errFileStale    = errors.New("File has been modified since read. Read it again before writing.")

func runWrite(st *sessionState, path, content string) string {
    abs := safePath(path)
    if fi, err := os.Stat(abs); err == nil {
        // 已存在文件：门控
        last, ok := st.reads[abs]
        if !ok || !last.fullView {
            return "Error: " + errFileNotRead.Error()
        }
        if fi.ModTime().After(last.mtime) {
            // 平台兜底：全读时比对内容哈希
            if cur, _ := os.ReadFile(abs); sha256String(cur) != last.contentHash {
                return "Error: " + errFileStale.Error()
            }
        }
    } // ENOENT → 新建，豁免

    if err := atomicWriteFile(abs, []byte(content), 0o644); err != nil { ... }
    st.touch(abs, true) // 写后刷新已读状态
    return fmt.Sprintf("Wrote %d bytes to %s", len(content), path)
}

// 同目录临时文件 + rename，保证目标路径永不会是半截状态
func atomicWriteFile(abs string, data []byte, perm os.FileMode) error {
    f, err := os.CreateTemp(filepath.Dir(abs), ".tmp-*")
    // 写 data → f.Sync() → os.Chmod → os.Rename(f.Name(), abs)
}
```

### 测试清单

- 覆盖未读已存在文件 → errorCode 2 式拒绝，文件内容不变
- 新建（不存在）路径无需先读，成功
- Read 后直接 Write 成功；Read 后被外部改写（测试里手动改 mtime/内容）→ 拒绝
- 部分读（带 offset/limit 的 Read 状态）后 Write → 拒绝
- 原子写：模拟写失败（如目录不可写）时目标旧文件完好
- Write 成功后 readState 被刷新（紧接着再 Write 不被误拦）
- 更新现有 `TestRunWrite` / `TestRunWriteEdgeCases`：用例需先在 sessionState 注入读取记录

### 破坏性变更提示

- 真实试玩中模型创建**新文件**不受影响；但"让模型直接覆盖一个没读过的文件"这类请求会被拒，错误文案必须明确引导（先 Read），否则模型会困惑重试。
- handler 签名从 `func(json.RawMessage) string` 改为携带 `*sessionState`，[toolHandlers](file:///Users/tony/coding-agent-go/tools.go#L57-L64) 分发表要整体调整（建议闭包绑定或在 runTool 入参增加 st）。

## 10. 参考资料

- [Claude Code Tools reference — Write tool behavior / 权限规则联动](https://code.claude.com/docs/en/tools-reference#write-tool-behavior)
- [源码解构 21：FileWriteTool 的 validateInput、原子写与 LSP 通知](https://claude-harness.dev/en/articles/21-file-operations)
- 我方实现：[tools.go runWrite L161-173](file:///Users/tony/coding-agent-go/tools.go#L161-L173)、[writeHandler L93-102](file:///Users/tony/coding-agent-go/tools.go#L93-L102)
