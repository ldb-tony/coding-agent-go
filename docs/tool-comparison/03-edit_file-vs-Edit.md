# 03 · edit_file vs Claude Code Edit / MultiEdit

[返回索引](README.md) ｜ 上一篇：[write_file vs Write](02-write_file-vs-Write.md) ｜ 下一篇：[grep vs Grep](04-grep-vs-Grep.md)

---

## 1. 工具定位与设计哲学

Claude Code 把 Edit 称为三个文件工具中"设计最精巧的一个"。它的模型是**精确替换**：模型只声明"什么变成什么"（`old_string → new_string`），不重发整个文件。两个设计支柱：

1. **增量的 token 经济学**——改 1000 行文件中的 3 行，Edit 只发这 3 行的新旧文本，Write 要发全部 1000 行；
2. **位置的确定性**——`old_string` 在文件中**必须唯一**，有歧义就失败，逼模型给出足够上下文。

我们的 `edit_file` 沿用教程 s02 参考实现的语义：*"Replace text in a file once (first occurrence)"*（[tools.go L372-378](file:///Users/tony/coding-agent-go/tools.go#L372-L378)），`strings.Replace(text, oldText, newText, 1)`（[L191](file:///Users/tony/coding-agent-go/tools.go#L191)）。这是**歧义语义**：模型无法控制改的是多处相似代码中的哪一处，而且静默发生。

另外，CC 还提供配套的 **MultiEdit**（一次调用对同一文件做多处编辑，整体原子），我们没有对应物。

## 2. 功能描述（description）对比

**Claude Code Edit 描述原文**（`FileEditTool/prompt.ts:20-27`）：

```
Performs exact string replacements in files.

- The replacement should be implemented as a old_string/new_string pair ...
- The edit will FAIL if `old_string` is not unique in the file. Either provide
  a larger string with more surrounding context to make it unique or use
  `replace_all` to change every instance of `old_string`.
- Use `replace_all` for replacing and renaming strings across the file. This
  parameter is useful if you want to rename a variable for instance.
```

描述里直接包含：精确匹配（注意缩进/空白）、**唯一性是失败条件**、失败后两条恢复路径（加上下文 / replace_all）、replace_all 的典型用途（变量重命名）。

参数描述：

- `new_string`：*"The text to replace it with (must be different from old_string)"*——新旧相同也是输入错误；
- `replace_all`：*"Replace all occurrences of old_string (default false)"*。

**我们**（[L372-378](file:///Users/tony/coding-agent-go/tools.go#L372-L378)）：

```go
tool("edit_file", "Replace text in a file once (first occurrence).", ...)
```

"first occurrence"把歧义行为当成承诺写给模型；没有任何关于空白精确性、多处匹配、恢复方式的指导。此外还有一个容易被忽视的细节：CC 做 **curly quote 规范化**（模型有时吐出中文弯引号 `‘’“”`，工具自动归一为直引号），降低一类低级失败；我们没有。

## 3. API/Schema 逐项对比

| 项 | Claude Code Edit | 我们的 edit_file | 点评 |
|---|---|---|---|
| 路径 | `file_path` | `path` | —— |
| 旧文本 | `old_string` | `old_text` | 命名差异；语义都是精确子串 |
| 新文本 | `new_string`，必须与 old 不同 | `new_text` | 我们未校验新旧相同（[runEdit](file:///Users/tony/coding-agent-go/tools.go#L175-L195) 直接放行） |
| 全量替换 | `replace_all`（布尔，默认 false） | **无**，固定替换 1 处 | 想改全部只能循环调用 |
| 多处编辑 | **MultiEdit**：`edits: [{old_string, new_string, replace_all?}]` | **无** | N 处编辑 = N 轮 loop |
| 匹配数要求 | 恰好 1（replace_all=false 时）；0 或 >1 均失败 | 恰好替换第 1 处；0 处才报错 | 核心语义分歧 |
| 必填 | file_path/old_string/new_string | path/old_text/new_text | 一致 |

CC schema 原文（`FileEditTool/types.ts:6-19`）：

```ts
z.strictObject({
  file_path: z.string().describe('The absolute path to the file to modify'),
  old_string: z.string().describe('The text to replace'),
  new_string: z.string().describe('The text to replace it with (must be different from old_string)'),
  replace_all: semanticBoolean(z.boolean().default(false).optional())
    .describe('Replace all occurrences of old_string (default false)'),
})
```

## 4. 执行机制对比（源码级）

### Claude Code 的精确替换流程

```
校验输入（新旧非空、不同）
→ read-before-edit：查 readFileState（同 Write 的门控）
→ 读盘取当前内容
→ 计数 old_string 出现次数：
     0 次 → FAIL "string to replace not found"（并提示文件可能被外部修改，建议重新 Read）
     ≥2 次且 !replace_all → FAIL "not unique"，引导加上下文
     恰好 1 次（或 replace_all=true）→ 执行替换
→ 同步临界区落盘（原子写）→ 更新 readFileState → LSP 通知
```

设计要害：

1. **"失败"是正常协议路径**：0 处和多处都是 FAIL，且错误文案本身就是恢复指令。模型一个轮次内就能自我纠正。
2. **replace_all 是显式意图**：变量重命名场景必须由模型明确声明"我要改全部"，默认永远是保守的单次精确替换。
3. **MultiEdit 原子性**：同一文件 N 个替换在一次调用内顺序应用；任何一个 old_string 未命中/有歧义 → 整个调用失败，文件不变。避免"改了 3 处、第 4 处失败、文件处于半改状态"。同时把 N 轮 agent loop 压成 1 轮。
4. 与 Write 共用 read-before 状态门控和 mtime 陈旧检测。

### 我们的流程

[runEdit L175-195](file:///Users/tony/coding-agent-go/tools.go#L175-L195)：

```go
if oldText == "" { return "Error: old_text must not be empty" }
abs := safePath(path)
data, _ := os.ReadFile(abs)
text := string(data)
if !strings.Contains(text, oldText) {
    return fmt.Sprintf("Error: text not found in %s", path)
}
os.WriteFile(abs, []byte(strings.Replace(text, oldText, newText, 1)), 0o644)
```

| 机制 | CC | 我们 |
|---|---|---|
| 匹配 0 处 | 失败 + 建议重读 | 失败（有，[L188-190](file:///Users/tony/coding-agent-go/tools.go#L188-L190)，文案无恢复指引） |
| 匹配多处 | 默认失败 | **静默改第一处**（[L191](file:///Users/tony/coding-agent-go/tools.go#L191)） |
| 全改 | replace_all | 不支持 |
| 多处批量 | MultiEdit 原子 | 不支持，循环 N 次且无整体回滚 |
| 先读后改 | 强制 + mtime | 无 |
| 新旧相同 | 拒绝 | 放行（一次无意义写入） |
| 落盘方式 | 原子 | 直接 WriteFile |
| 弯引号容错 | 有 | 无 |

## 5. 输出契约与边界行为

| 场景 | Claude Code | 我们 |
|---|---|---|
| 唯一匹配 | 替换成功 | 替换成功，回执 `Edited path`（[L194](file:///Users/tony/coding-agent-go/tools.go#L194)） |
| 0 处匹配 | FAIL，提示可能被外部改动、建议重新 Read | `Error: text not found in path` |
| 多处匹配（默认） | FAIL + 两条出路（扩上下文 / replace_all） | 静默成功，改第一处——**错误位置无告警** |
| 批量编辑中途失败 | 整批回滚，文件不变 | 无此能力 |
| 空 old_string | schema 校验失败 | 有拦截（[L176-178](file:///Users/tony/coding-agent-go/tools.go#L176-L178)） |
| old == new | schema 描述约束 | 返回 `Edited`（假成功） |
| 文件不存在 | 门控/读盘失败 | Error 字符串 |

## 6. 状态、安全与权限

- Edit 与 Write 共用同一套 read-before 门控、mtime/内容双检、原子写、权限规则（详见 [02 报告第 6 节](02-write_file-vs-Write.md)）。Edit 权限粒度天然比 Write 小（只动声明的片段），所以在权限策略里 Edit 常被更宽松地授予。
- **唯一性检查本身也是一种安全属性**：`sudo` 配置、CI yaml 这类文件里相同短串可能出现在多个 stanza，静默替换第一处可能改变错误的 stanza；唯一性把"我以为我改的是这里"变成可验证的事实。
- 我们只有 safePath；无状态门控；非原子。

## 7. 效果对比（量化场景推演，估算）

假设一次 agent loop 往返 ≈ 3-10s 延迟 + 当轮全部输入 token 计费。

### 场景 A：文件里有 4 处 `err = nil`，模型想改的是第 3 处所在函数里的那一处

- **CC**：提交短 old_string → FAIL not unique（~30 token 错误，同轮内模型看到结果）；模型加上前后两行上下文使片段唯一 → 成功。**2 次 Edit 调用、~0.1K token 代价，结果位置正确。**
- **我们**：短 old_string → 静默替换**第 1 处**并返回 `Edited`。模型以为改对了，继续后续工作；错误可能在测试失败或用户 review 时才暴露，此时距事故已隔多轮，定位需要重新读文件比对。**表面省 1 次往返，实际把 N 轮后的排查成本和一次错误修改打包欠下。**

### 场景 B：变量重命名，`client` 在一个文件出现 12 处

- **CC**：`replace_all=true` 一次完成；或 MultiEdit 与其他 5 处不同修改合并为**一次**工具调用（1 轮 loop、一份 old/new 文本流量、原子成败）。
- **我们**：要么用 bash `sed`（绕过 edit_file，且 sed 转义又是新错误源），要么 edit_file 调 12 次——每轮文件变化后下一处的上下文也要重新考虑，**12 轮 loop**，且中途某次失败会留下半改名状态（编译错误风暴），没有回滚。
- 粗估：12 轮串行往返在交互体验上是分钟级 vs 秒级；token 上每轮都重发对话历史，成本随轮数累加。

### 场景 C：新旧相同 / 弯引号

- CC：schema/规范化层拦下或自动修复；我们：`old==new` 时报告 Edited（模型误判任务推进）；弯引号直接 text not found，模型未必能自悟原因。

## 8. 差距清单评级

| 维度 | Claude Code | 我们 | 影响 | 优先级 |
|---|---|---|---|---|
| 唯一性强制 | 多处默认 FAIL + 恢复指引 | 静默改第一处 | 改错位置、事故延后暴露 | **P0** |
| replace_all | 显式布尔 | 无 | 重命名只能 sed/循环 | **P0** |
| MultiEdit | 一次多处、整批原子 | 无 | loop 轮次 ×N、半成状态无回滚 | **P1** |
| read-before-edit | 强制 + 陈旧检测 | 无 | 基于旧内容编辑 | **P0**（与 Write 同期共用） |
| 未命中错误文案 | 提示外部改动/重读 | 仅 text not found | 恢复引导弱 | P1 |
| 新旧相同校验 | 拒绝 | 假成功 | 状态误判 | P2 |
| 弯引号规范化 | 有 | 无 | 偶发低级失败 | P2 |
| 原子落盘 | 有 | 无 | 半改文件 | P1（随 Write 的 atomicWrite 共用） |

## 9. 对齐设计草案（仅设计，不实施）

### Schema 变更

```go
// edit_file：新增 replace_all
//   path        string 必填
//   old_text    string 必填
//   new_text    string 必填且 != old_text
//   replace_all bool   可选，默认 false

// 新增工具 multi_edit：
//   path  string 必填
//   edits []struct{ OldText, NewText string; ReplaceAll bool } 必填，len≥1
```

### 函数草案

```go
func runEdit(st *sessionState, path, oldText, newText string, all bool) string {
    // 1. 参数校验：oldText != ""、newText != oldText
    // 2. read-before 门控 + mtime 检查（复用 runWrite 的校验，抽成 checkWritable(st, abs)）
    content, err := readChecked(abs) // 含陈旧检测
    count := strings.Count(content, oldText)
    switch {
    case count == 0:
        return "Error: text not found — the file may have changed; read it again"
    case count > 1 && !all:
        return fmt.Sprintf("Error: old_text is not unique (%d matches). Add surrounding context or set replace_all=true", count)
    }
    if all {
        content = strings.ReplaceAll(content, oldText, newText)
    } else {
        content = strings.Replace(content, oldText, newText, 1)
    }
    atomicWriteFile(abs, []byte(content), 0o644)
    st.touch(abs, true)
    return fmt.Sprintf("Edited %s", path)
}

func runMultiEdit(st *sessionState, path string, edits []editOp) string {
    // 门控 → 读一次 → 在内存中顺序套用全部替换；
    // 每步都做 0/歧义 检查，任一失败立即返回错误且不写盘 → 全部成功才一次原子落盘
}
```

### 测试清单

- 唯一匹配成功（更新现有 `TestRunEdit/replaces_first_occurrence_only`：两处相同文本的旧用例语义反转——**破坏性变更**，改为期望失败）
- 两处匹配 + replace_all=false → 错误且含匹配数；replace_all=true → 全改
- 0 处匹配 → 错误文案含"重读"引导
- old==new → 参数错误；空 old/new → 错误
- MultiEdit：全部命中一次落盘；第 2 个未命中 → 文件内容完全不变
- MultiEdit 各操作共享门控（未读文件拒绝整批）
- 弯引号输入按设计决策决定是否规范化（P2，可先不做但记录）

### 破坏性变更提示

- **"第一处替换"→"唯一替换"是本工具最根本的语义变更**，与教程 s02 参考 Python 实现有意分叉；README 和工具描述需同步说明，现有试玩任务（edit test.py）不受影响（唯一匹配场景行为不变）。
- handler 签名随 Write 一起改为携带 `*sessionState`。

## 10. 参考资料

- [Claude Code Tools reference — Edit tool behavior（含 Non-UTF-8 文件策略）](https://code.claude.com/docs/en/tools-reference#edit-tool-behavior)
- [源码解构 21：FileEditTool 的唯一性约束、replace_all、curly quote 规范化](https://claude-harness.dev/en/articles/21-file-operations)
- 我方实现：[tools.go runEdit L175-195](file:///Users/tony/coding-agent-go/tools.go#L175-L195)、[editHandler L104-114](file:///Users/tony/coding-agent-go/tools.go#L104-L114)
