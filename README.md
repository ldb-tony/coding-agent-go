# s02: Tool Use

一个最小可用的 Go 版 coding agent：6 个工具（bash + 文件读写编辑 + 搜索）+ agent loop + Claude。

> *"加一个工具，只加一行映射"* —— agent loop 与 s01 完全一致，唯一的变化是工具执行从硬编码 `runBash` 变成 `toolHandlers` 分发查找。

## 运行

```bash
go run .
```

## 工具分发

s01 只有一个 bash 工具：读文件要 `cat`，写文件要 `echo >`，编辑要 `sed`。模型得先把意图翻译成 shell 命令，浪费 token 还容易出错。

s02 把工具扩展到 6 个，每个工具独立定义、独立实现：

1. **定义工具**：在 `allTools()` 返回的工具列表里加一个 JSON schema 条目
2. **注册处理器**：在 `toolHandlers` map 里加一行 `工具名 → 处理函数` 的映射

```go
var toolHandlers = map[string]toolHandler{
    "bash":       bashHandler,
    "read_file":  readHandler,
    "write_file": writeHandler,
    "edit_file":  editHandler,
    "glob":       globHandler,
    "grep":       grepHandler,
}

// agent loop 中唯一的改动点：
output := runTool(block.Name, block.Input) // 查表分发
```

模型一次返回多个 tool_use 时，按 `response.content` 中的原始顺序逐个执行。

## 工具一览

| 工具 | 参数 | 行为 |
|------|------|------|
| `bash` | `command` | 执行 shell 命令（120s 超时、50K 字符截断、危险命令拦截） |
| `read_file` | `path`，可选 `offset`、`limit` | 读取 UTF-8 文本，输出 `cat -n` 风格行号；默认每页 2000 行；>256KB 未分页或单页超 25K 字符时返回带 offset/limit 指引的短错误（不截断）；重复读取未变化区间返回短提示；非 UTF-8 文件报错 |
| `write_file` | `path`、`content` | 覆盖写入文件（父目录自动创建），返回写入字节数 |
| `edit_file` | `path`、`old_text`、`new_text` | 替换文件中第一处匹配文本；未命中返回错误 |
| `glob` | `pattern` | 按 glob 模式查找文件，支持跨目录 `**`；排序后最多输出 200 条 |
| `grep` | `pattern`，可选 `path`、`include` | 正则搜索文件内容，输出 `file:line:content`；跳过二进制/大于 5MB 的文件，最多 200 条 |

## 路径安全

文件工具（read/write/edit 及 grep 的 path）都经过 `safePath` 校验：路径解析为绝对路径后必须位于工作目录内，`../` 逃逸和工作区外的绝对路径一律拒绝。

bash 工具不受此限制 —— `rm -rf /` 之类的操作目前只有简单的危险命令子串拦截，真正的权限审批是 s03 的主题。

## 试玩任务

1. `Read the file README.md and tell me what this project is about`
2. `Create a file called test.py that prints "hello", then read it back`
3. `Find all Go files in this directory`（观察 glob 的 `**` 递归匹配）
4. `找出代码里所有包含 "Error" 的行，整理写进 errors.txt`（grep + write 多工具协作）

观察点：模型什么时候只调一个工具，什么时候一次调多个？多个工具调用的执行顺序是否正确？

## 项目结构

- [agent.go](agent.go) — agent loop 主逻辑（与 s01 相同，仅工具执行改为分发查找）
- [tools.go](tools.go) — 工具 schema 定义、`toolHandlers` 分发表，以及 read/write/edit/glob/grep 实现与 `safePath`
- [bash.go](bash.go) — bash 工具执行（120s 超时、50K 字符截断、危险命令拦截）
- [llm.go](llm.go) — Anthropic 客户端封装
- [main.go](main.go) — REPL 入口
- [logging.go](logging.go) — 输入/响应日志
- [tools_test.go](tools_test.go) — 文件工具、路径安全与分发逻辑测试
