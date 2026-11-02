# 05 - bash 工具规格

## 1. 职责

bash 工具负责在宿主机上执行 shell 命令，是 Agent 与操作系统交互的主要通道：

1. 执行任意 shell 命令并返回 stdout + stderr 合并输出
2. 管理命令超时（默认 120 秒）
3. 跨调用持久化工作目录（cwd）
4. 支持后台执行长耗时命令
5. 进程组管理（确保子进程清理）
6. 退出码语义化处理（grep/diff/test 等特殊退出码）
7. 输出截断与落盘（防止单次输出撑爆上下文窗口）

## 2. 文件位置

```
pkg/tool/basic/bash.go    # eino InvokableTool 适配层（参数定义 + 协议桥接）
bash.go                    # 核心执行逻辑（runBash, runBashWithCwd, shellCommand）
background.go              # 后台任务管理（backgroundManager）
procgroup_unix.go          # Unix 进程组管理（Setpgid + SIGTERM）
procgroup_windows.go       # Windows 进程组管理（no-op）
```

## 3. 依赖

- `os/exec` — 进程创建与管理
- `context` — 超时控制
- `bytes` — stdout/stderr 缓冲
- `runtime` — 跨平台判断
- `syscall` — 进程组属性（Unix）
- `encoding/json` — 参数解析
- `path/filepath` — 临时文件路径
- `os` — 临时文件写入

## 4. 数据结构

### 4.1 BashArgs — eino 适配层参数

```go
type BashArgs struct {
    Command         string `json:"command" jsonschema:"required" jsonschema_description:"Shell command to execute"`
    Timeout         int    `json:"timeout,omitempty" jsonschema_description:"Timeout in seconds (default 120)"`
    RunInBackground bool   `json:"run_in_background,omitempty" jsonschema_description:"Run in background"`
}
```

### 4.2 BashResult — eino 适配层结果

```go
type BashResult struct {
    Output   string `json:"output"`
    ExitCode int    `json:"exit_code"`
}
```

### 4.3 handler 层入参（从 json.RawMessage 解析）

```go
// 同步调用
var in struct {
    Command string `json:"command"`
}

// 后台调用（由 scheduler 层检测）
var in struct {
    RunInBackground bool `json:"run_in_background"`
}
```

### 4.4 bashHandler 闭包

```go
func bashHandler(st *sessionState, args json.RawMessage) string {
    // 1. 解析 command
    // 2. 调用 runBashWithCwd(command, st.shellCwd)
    // 3. 更新 st.shellCwd = newCwd
    // 4. 返回 output
}
```

## 5. 关键常量

```go
const defaultBashTimeout = 120 * time.Second   // 默认超时
const maxBashOutputRunes = 50000               // 输出截断上限（runes）
const cwdSyncMarker = "__CWD_SYNC_7a3f2b__"    // cwd 持久化同步标记
```

**注**: eino 适配层文档中 `maxBashOutputRunes` 标注为 25000，但实际核心逻辑中使用 50000。实现时以 `bash.go` 中的常量为准。

## 6. 核心函数

### 6.1 NewBashTool() — eino 适配

```
函数签名: func NewBashTool(handler func(args json.RawMessage) string) (tool.InvokableTool, error)

流程:
1. 调用 utils.InferTool[BashArgs, BashResult]("bash", description, runFunc)
2. runFunc 内部:
   a. 将 BashArgs 转为 map[string]interface{}
   b. json.Marshal 为 JSON bytes
   c. 调用 handler(jsonBytes) 获取结果字符串
   d. 返回 BashResult{Output: result, ExitCode: 0}
```

### 6.2 runBash() — 基础执行入口

```
函数签名: func runBash(command string) string

流程:
1. 调用 runBashWithTimeout(command, defaultBashTimeout)
2. 返回结果字符串
```

### 6.3 runBashWithTimeout() — 超时控制执行

```
函数签名: func runBashWithTimeout(command string, timeout time.Duration) string

流程:
1. 创建带超时的 context:
   ctx, cancel := context.WithTimeout(context.Background(), timeout)
   defer cancel()

2. 构造 shell 命令:
   cmd := shellCommand(ctx, command)

3. 设置 stdout/stderr 缓冲:
   var stdout, stderr bytes.Buffer
   cmd.Stdout = &stdout
   cmd.Stderr = &stderr

4. 执行命令:
   err := cmd.Run()

5. 错误处理:
   a. 超时（ctx.Err() == context.DeadlineExceeded）:
      → 返回 "Error: Timeout ({N}s)"
   
   b. 非零退出码（exec.ExitError）:
      i.   提取最后命令名: name := lastCommandName(command)
      ii.  查 exitCodeSemantics 表:
           - 如果语义化判定为非错误（如 grep 退出 1 = 无匹配）:
             → 返回语义化 note（"No matches found" / "Files differ" / "Condition false"）
           - 否则: 拼接 stdout + stderr:
             * 有内容 → "Error: exit status {N}\n{combined}"
             * 无内容 → "Error: exit status {N}"
      
      iii. 非 ExitError → "Error: {err.Error()}"
   
   c. 成功（err == nil）:
      i.   合并 stdout + stderr，trim 空白
      ii.  空输出 → "(no output)"
      iii. 超过 maxBashOutputRunes → 截断 + 落盘:
           * 调用 saveBashOverflow(output) 写入临时文件
           * 落盘成功 → "{截断内容}\n\n... (output truncated at {N} characters). Full output saved to \"{path}\" — read it with read_file."
           * 落盘失败 → "{截断内容}\n\n(Output truncated — failed to save full output: {err})"
      iv.  未超限 → 返回完整 output
```

### 6.4 runBashWithCwd() — cwd 持久化执行

```
函数签名: func runBashWithCwd(command, shellCwd string) (output string, newCwd string)

流程:
1. 保存原始命令名（用于退出码语义化）:
   origCmdName := lastCommandName(command)

2. 构造包装命令:
   - shellCwd 非空:
     "cd {shellCwd} && {command}; __orig_exit=$?; echo {cwdSyncMarker}$(pwd) $__orig_exit; exit $__orig_exit"
   - shellCwd 为空:
     "{command}; __orig_exit=$?; echo {cwdSyncMarker}$(pwd) $__orig_exit; exit $__orig_exit"

3. 调用 runBashWithTimeout(wrapped, defaultBashTimeout) 获取 raw 输出

4. 从 raw 输出尾部提取 cwdSyncMarker:
   idx := strings.LastIndex(raw, cwdSyncMarker)
   
   a. 找到 marker:
      i.   beforeMarker = raw[:idx] trim 空白 → 作为实际输出
      ii.  afterMarker = raw[idx+len(marker):] trim 空白
      iii. 解析 afterMarker:
           - 按空格 split → parts
           - len >= 2: newCwd = parts[0], exitCode = atoi(parts[1])
           - len == 1: newCwd = parts[0]
      iv.  用 origCmdName 做退出码语义化覆盖:
           - 查 exitCodeSemantics[origCmdName]
           - 如果非错误 → beforeMarker = note
      v.   beforeMarker 为空 → output = "(no output)"
      vi.  返回 (output, newCwd)
   
   b. 未找到 marker:
      → 返回 (raw, shellCwd)（保持原 cwd 不变）
```

### 6.5 shellCommand() — 跨平台 shell 构造

```
函数签名: func shellCommand(ctx context.Context, command string) *exec.Cmd

流程:
- Windows (runtime.GOOS == "windows"):
  → exec.CommandContext(ctx, "cmd", "/C", command)
- Unix (其他):
  → exec.CommandContext(ctx, "sh", "-c", command)
```

### 6.6 lastCommandName() — 提取最后命令名

```
函数签名: func lastCommandName(command string) string

流程:
1. 按 | ; & 分割命令为 segments
2. 从后向前找第一个非空 segment
3. 按空格 split 为 fields
4. 跳过 FOO=bar 形式的赋值（包含 = 且不含空白/$/'/"）
5. 跳过 timeout 前缀及其数值参数
6. 返回第一个非跳过字段

示例:
- "cat a.txt | grep foo" → "grep"
- "ls; diff a b" → "diff"
- "FOO=bar grep pattern" → "grep"
- "timeout 5 sleep 10" → "sleep"
```

### 6.7 saveBashOverflow() — 超限输出落盘

```
函数签名: func saveBashOverflow(content string) (string, error)

流程:
1. 构造临时目录: filepath.Join(os.TempDir(), "agent-bash-output")
2. 创建目录（0o755 权限）
3. 创建临时文件: os.CreateTemp(dir, "bash-*.log")
4. 写入完整内容
5. 关闭文件
6. 返回临时文件绝对路径

设计决策:
- 落盘到 os.TempDir() 而非 workdir，避免 grep/glob 搜索时产生噪音
```

## 7. 进程组管理

### 7.1 Unix（procgroup_unix.go）

```go
// sysProcAttr 让 shell 进程成为新进程组的组长
func sysProcAttr() *syscall.SysProcAttr {
    return &syscall.SysProcAttr{Setpgid: true}
}

// stopProcessGroup 向进程组发送 SIGTERM
func stopProcessGroup(cmd *exec.Cmd) {
    if cmd != nil && cmd.Process != nil {
        _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
    }
}
```

**说明**: `-cmd.Process.Pid` 表示向进程组而非单个进程发信号。`Setpgid: true` 确保子进程成为新组组长，这样 `Kill(-pid, SIGTERM)` 能清理整棵子进程树。

### 7.2 Windows（procgroup_windows.go）

```go
// sysProcAttr 在 Windows 上返回 nil（默认行为）
func sysProcAttr() *syscall.SysProcAttr {
    return nil
}

// stopProcessGroup 在 Windows 上是 no-op
func stopProcessGroup(*exec.Cmd) {}
```

**说明**: Windows 没有 POSIX 进程组语义。context 取消时 `exec.CommandContext` 只杀直接子进程。

### 7.3 后台任务使用进程组

`backgroundManager.start()` 中：
```
1. cmd.SysProcAttr = sysProcAttr()   // 设置进程组属性
2. go m.run(t, ctx, &stdout, &stderr) // 后台 goroutine 执行
3. run() 结束后调用 stopProcessGroup(t.cmd) // 清理残留子进程
```

## 8. 退出码语义化

### 8.1 语义映射表

```go
var exitCodeSemantics = map[string]func(int) (bool, string){
    "grep":  exitCodeNoMatch1,    // 退出 1 = 无匹配（非错误）
    "egrep": exitCodeNoMatch1,
    "fgrep": exitCodeNoMatch1,
    "rg":    exitCodeNoMatch1,
    "diff":  exitCodeDiffer1,     // 退出 1 = 文件不同（非错误）
    "cmp":   exitCodeDiffer1,
    "test":  exitCodeFalse1,      // 退出 1 = 条件为假（非错误）
    "[":     exitCodeFalse1,
}
```

### 8.2 语义函数

| 函数 | isError 判定 | 非错误时的 note |
|------|-------------|----------------|
| `exitCodeNoMatch1` | code == 1 → false, code >= 2 → true | "No matches found" |
| `exitCodeDiffer1` | code == 1 → false, code >= 2 → true | "Files differ" |
| `exitCodeFalse1` | code == 1 → false, code >= 2 → true | "Condition false" |

### 8.3 退出码总结

| 退出码 | 含义 | 输出 |
|--------|------|------|
| 0 | 成功 | stdout + stderr 合并 |
| 非 0（有语义化） | 取决于命令 | 语义化 note 或 "Error: exit status N" |
| 非 0（无语义化） | 失败 | "Error: exit status N\n{output}" |
| 超时 | DeadlineExceeded | "Error: Timeout (120s)" |

## 9. 后台执行

### 9.1 触发条件

```go
func shouldRunBackground(toolName string, input json.RawMessage) bool {
    // 仅当 toolName == "bash" 且 run_in_background == true 时返回 true
    // 不再从命令内容（install/build/test 等关键词）猜测
}
```

### 9.2 后台执行流程

```
1. executeToolWithRecover() 检测 shouldRunBackground()
2. 调用 startBackgroundTask(input):
   a. 解析 command 参数
   b. backgroundTasks.start(command) → 返回 bg_id（格式 "bg_{NNNN}"）
   c. 返回占位结果: "[Background task {bg_id} started] The result will be delivered as a <task_notification> in a later turn. Continue with other work in the meantime."
3. Agent Loop 下一轮开头调用 injectBackgroundResults():
   a. backgroundTasks.collect() 弹出所有已完成任务
   b. 格式化为 <task_notification> 块
   c. 作为独立 user message 追加到对话历史
```

### 9.3 bg_id 格式

```
bg_{4位零填充序号}
示例: bg_0001, bg_0002, bg_0010
```

序号在 `backgroundManager.next` 中自增，进程级生命周期。

## 10. cwd 持久化

### 10.1 状态存储

```go
type sessionState struct {
    shellCwd string // bash cwd 持久化（跨调用保持工作目录）
    // ...
}
```

### 10.2 同步机制

使用 `cwdSyncMarker`（`"__CWD_SYNC_7a3f2b__"`）作为独特前缀，在命令末尾附加 `echo {marker}$(pwd) {exitCode}`，从输出尾部精确提取当前工作目录。

### 10.3 生命周期

1. `bashHandler` 调用 `runBashWithCwd(command, st.shellCwd)`
2. `runBashWithCwd` 包装命令并执行
3. 从输出解析 newCwd
4. `bashHandler` 更新 `st.shellCwd = newCwd`
5. 下次调用时使用更新后的 cwd 作为起始目录

## 11. 输出格式

### 11.1 正常输出

```
{stdout + stderr 合并，trim 空白}
```

### 11.2 无输出

```
(no output)
```

### 11.3 截断输出

```
{前 maxBashOutputRunes 个字符}

... (output truncated at {maxBashOutputRunes} characters). Full output saved to "{临时文件路径}" — read it with read_file.
```

### 11.4 超时

```
Error: Timeout (120s)
```

### 11.5 非零退出（有内容）

```
Error: exit status {N}
{stdout + stderr 合并}
```

### 11.6 非零退出（无内容）

```
Error: exit status {N}
```

### 11.7 后台启动

```
[Background task bg_NNNN started] The result will be delivered as a <task_notification> in a later turn. Continue with other work in the meantime.
```

### 11.8 后台完成通知

```
<task_notification>
Background task bg_NNNN ({command}) completed:
{output}
</task_notification>
```

## 12. safePath 与 bash

**bash 工具不使用 `safePath()`**。路径安全由 bash 自身的权限模型保证（用户在自己的 shell 中执行命令）。bash 有自己的危险命令检测机制（通过权限系统的 PreToolUse hook 实现），而非路径逃逸检测。

## 13. 错误处理

| 错误场景 | 处理方式 | 输出格式 |
|---------|---------|---------|
| 命令超时 | context.DeadlineExceeded 检测 | "Error: Timeout (120s)" |
| 非零退出码 | exec.ExitError 类型断言 | "Error: exit status N" + 输出 |
| 语义化非错误 | exitCodeSemantics 查表 | 语义 note（如 "No matches found"） |
| 命令不存在 | exec.Command 报错 | "Error: {err}" |
| 输出落盘失败 | saveBashOverflow 返回 error | 截断内容 + 落盘失败提示 |
| cwd marker 未找到 | 回退到原始输出 | 原始输出 + 保持旧 cwd |

## 14. 测试要点

### 14.1 基础执行
- 简单命令（`echo hello`）返回正确输出
- stdout + stderr 合并
- 无输出时返回 "(no output)"

### 14.2 超时控制
- 超过 defaultBashTimeout 的命令返回超时消息
- 自定义 timeout 参数生效
- 超时后子进程被正确清理

### 14.3 退出码语义化
- `grep` 无匹配（退出 1）返回 "No matches found" 而非错误
- `diff` 文件不同（退出 1）返回 "Files differ"
- `test` 条件假（退出 1）返回 "Condition false"
- 以上命令退出码 >= 2 仍报错误
- 管道命令正确提取最后命令名

### 14.4 输出截断
- 超过 maxBashOutputRunes 的输出被截断
- 截断后完整内容落盘到临时文件
- 返回消息包含文件路径和 read_file 提示
- 落盘失败时返回降级消息

### 14.5 cwd 持久化
- `cd /tmp` 后下次调用起始目录为 /tmp
- cwdSyncMarker 正确解析
- marker 未找到时保持旧 cwd
- 退出码语义化使用原始命令名（非包装后的 "exit"）

### 14.6 后台执行
- `run_in_background=true` 立即返回 bg_id
- bg_id 格式正确（bg_NNNN）
- 后台任务完成后 collect() 返回通知
- stopAll() 清理运行中的后台任务

### 14.7 进程组管理
- Unix: Setpgid=true 设置正确
- Unix: stopProcessGroup 向整组发 SIGTERM
- Windows: sysProcAttr 返回 nil
- 子进程树在超时/退出后被完整清理

### 14.8 lastCommandName
- 管道命令取最后一段
- 分号分隔取最后一段
- 跳过环境变量赋值
- 跳过 timeout 前缀及参数
