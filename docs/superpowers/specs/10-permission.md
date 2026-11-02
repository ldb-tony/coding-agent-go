# 10 - 权限管线规格

## 1. 职责

权限管线是 Agent 的安全守卫，负责在工具执行前进行三层检查：

1. **Gate 1 — 拒绝列表 (deny list)**: 绝对禁止的操作，无条件拦截
2. **Gate 2 — 规则匹配 (rule matching)**: 基于正则/关键词的策略检测
3. **Gate 3 — 用户确认 (user confirmation)**: 交互式询问用户授权

**设计理念**:
- 三闸门模型，任意一关拒绝即拦截本次工具调用
- 通过 eino `callbacks.Handler` 的 `OnToolStart` 回调集成到 Agent 循环
- MCP 外部工具走独立的 host-side 策略，不信任 server 自称的 readOnlyHint
- 支持多种权限模式（default/yolo/plan）

## 2. 文件位置

```
pkg/permission/
├── permission.go       # 三闸门管线核心实现、PermissionChecker
├── rules.go            # 权限规则定义、匹配、破坏性检测
└── mcp_policy.go       # MCP 外部工具 host-side 策略
```

## 3. 依赖

- `encoding/json` — 工具参数解析
- `bufio` / `os` — 用户输入读取
- `regexp` — 破坏性命令正则匹配
- `strings` — 子串匹配
- `fmt` — 终端输出
- `github.com/cloudwego/eino/callbacks` — callbacks.Handler 接口

无外部包依赖，全部使用 Go 标准库 + eino callbacks。

## 4. 数据结构

### 4.1 三闸门模型

```
Gate 1: 拒绝列表 → 直接拦截（仅 bash 工具）
Gate 2: 规则匹配 → 标记需要确认或直接拒绝
Gate 3: 用户确认 → 放行/拦截
全部通过 → 执行工具
```

### 4.2 PermissionAction

```go
type PermissionAction string

const (
    ActionAllow   PermissionAction = "allow"    // 直接放行
    ActionDeny    PermissionAction = "deny"     // 直接拒绝
    ActionConfirm PermissionAction = "confirm"  // 询问用户
)
```

### 4.3 PermissionRule

描述一条权限规则：作用于哪些工具、命中条件、执行动作。

```go
type PermissionRule struct {
    Tools       []string                       // 规则适用的工具名列表
    Check       func(args json.RawMessage) bool // 条件判定函数（接收工具原始 JSON 参数）
    Action      PermissionAction               // 命中时的动作
    Description string                         // 规则描述/告警原因
}
```

**规则匹配优先级**:
1. 精确匹配（工具名完全一致）优先于通配符匹配
2. 通配符匹配（如 `write_*`）按声明顺序
3. 未命中任何规则 → 默认策略（直通放行或询问用户）

### 4.4 PermissionMode

权限模式，控制整体行为。

```go
type PermissionMode string

const (
    ModeDefault PermissionMode = "default" // 标准三闸门
    ModeYolo    PermissionMode = "yolo"    // 跳过确认，自动允许
    ModePlan    PermissionMode = "plan"    // 只读模式，禁止写入
)
```

### 4.5 mcpPermissionLevel

MCP host-side 策略的三档。

```go
type mcpPermissionLevel string

const (
    mcpPermAllow   mcpPermissionLevel = "allow"    // 放行，无需确认
    mcpPermConfirm mcpPermissionLevel = "confirm"  // 询问用户（默认）
    mcpPermDeny    mcpPermissionLevel = "deny"     // 硬拒绝
)
```

### 4.6 PermissionChecker

权限检查器，持有规则和配置。

```go
type PermissionChecker struct {
    mode       PermissionMode                // 当前权限模式
    rules      []PermissionRule              // 权限规则列表
    mcpPolicy  map[string]mcpPermissionLevel // MCP host-side 策略表
    approvalFn func() string                 // 用户确认输入函数（可替换用于测试）
}
```

## 5. 核心常量

### 5.1 硬性拒绝清单 (denyList)

```go
var denyList = []string{
    "rm -rf /",   // 删除根目录
    "sudo",       // 提权操作
    "shutdown",   // 关机
    "reboot",     // 重启
    "mkfs",       // 格式化磁盘
    "dd if=",     // 磁盘直写
    "> /dev/",    // 写入设备文件
}
```

**匹配方式**: 子串包含 (`strings.Contains`)，仅对 bash 工具生效。

### 5.2 破坏性命令检测

```go
// 正则: rm/del 出现在命令位置（行首、分号/管道/括号后）
// RE2 不支持 lookahead，用无 lookahead 的等价改写
var destructiveCommandWord = regexp.MustCompile(
    `(?i)(?:^|[;&|()\n])\s*(?:rm|del)(?:\s|$|[;&|()])`,
)

// 关键词列表（子串匹配）
var destructiveKeywords = []string{
    "rm ",         // rm 命令（带空格，避免误匹配 "form" 等）
    "> /etc/",     // 写入系统配置目录
    "chmod 777",   // 开放全部权限
}
```

### 5.3 默认权限规则

```go
var defaultRules = []PermissionRule{
    {
        Tools: []string{"bash"},
        Check: func(args json.RawMessage) bool {
            var in struct {
                Command string `json:"command"`
            }
            _ = json.Unmarshal(args, &in)
            return containsDestructiveCommand(in.Command)
        },
        Action:      ActionConfirm,
        Description: "Potentially destructive command",
    },
}
```

### 5.4 MCP Host 策略表

```go
var mcpHostPolicy = map[string]mcpPermissionLevel{
    // docs server — 全是只读操作
    "docs/search":      mcpPermAllow,
    "docs/get_version": mcpPermAllow,

    // deploy server — status 只读, trigger 破坏性
    "deploy/status":  mcpPermAllow,
    "deploy/trigger": mcpPermConfirm,
}
```

**默认策略**: 未配置的 MCP 工具一律返回 `mcpPermConfirm`（需要用户确认）。

## 6. 核心函数

### 6.1 NewPermissionChecker()

```
函数签名: func NewPermissionChecker(mode PermissionMode) *PermissionChecker

流程:
1. 创建 PermissionChecker
   - checker := &PermissionChecker{
       mode: mode,
       rules: defaultRules,
       mcpPolicy: mcpHostPolicy,
       approvalFn: defaultApprovalFn,
   }

2. defaultApprovalFn 实现:
   - line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
   - return line

3. 返回 checker
```

### 6.2 Check() — 完整权限检查入口

```
函数签名: func (c *PermissionChecker) Check(toolName string, args json.RawMessage) error

流程:
1. 根据权限模式快速判断
   a. c.mode == ModeYolo:
      - 跳过 Gate 2 和 Gate 3
      - 仅检查 Gate 1（denyList），命中 → return error
      - 未命中 → return nil（全部放行）

   b. c.mode == ModePlan:
      - 检查工具是否为写入类:
        * toolName == "bash" → return error("write operations not allowed in plan mode")
        * strings.HasPrefix(toolName, "write_") → return error(...)
        * strings.HasPrefix(toolName, "edit_") → return error(...)
      - 只读工具 → return nil（放行）

   c. c.mode == ModeDefault → 继续完整三闸门检查

2. MCP 外部工具 → 走独立 host-side 策略
   a. isMCPTool(toolName) 检查是否为 MCP 工具（名字以 "mcp__" 开头）
   b. parseMCPName(toolName) 解析 server 和 safeTool 名
   c. 如果名字无法解析 → return nil（交给 runTool 的 fallback 报错，不在这里拦）
   d. 查策略表: level := c.mcpPermission(server, safeTool)
   e. 根据 level:
      - mcpPermAllow → return nil（放行）
      - mcpPermDeny → return error("⛔ Blocked: MCP tool {toolName} denied by host policy")
      - mcpPermConfirm → 调用 c.askUser(toolName, args, "MCP external tool — host policy requires confirmation")
        * 用户同意 → return nil
        * 用户拒绝 → return error("Permission denied.")

3. Gate 1: 硬性拒绝清单（仅 bash）
   a. toolName == "bash" 时:
      - 解析 { "command": "..." } 中的 command 字段
      - pattern, ok := c.checkDenyList(command)
      - 如果 ok → 打印 "⛔ Blocked: '{pattern}' is on the deny list"，return error
      - 注意: Gate 1 直接拦截，不询问用户

4. Gate 2 + 3: 规则匹配 + 用户确认
   a. reason, action := c.checkRules(toolName, args)
   b. 如果 action == ActionDeny → return error("⛔ Blocked: {reason}")
   c. 如果 action == ActionConfirm:
      - approved := c.askUser(toolName, args, reason)
      - 如果 approved → return nil
      - 否则 → return error("Permission denied.")
   d. 如果 action == ActionAllow → return nil

5. 三门均未命中 → return nil（直通放行）
```

### 6.3 checkDenyList()

```
函数签名: func (c *PermissionChecker) checkDenyList(command string) (pattern string, ok bool)

流程:
1. 遍历 denyList 中的每个模式
2. 如果 strings.Contains(command, pattern) → 返回 (pattern, true)
3. 全部未命中 → 返回 ("", false)

注意: 简单子串匹配，不做词法分析。
```

### 6.4 containsDestructiveCommand()

```
函数签名: func containsDestructiveCommand(command string) bool

流程:
1. 正则检查: destructiveCommandWord.MatchString(command)
   - 正则: (?i)(?:^|[;&|()\n])\s*(?:rm|del)(?:\s|$|[;&|()])
   - 匹配 rm/del 出现在命令位置（行首或分隔符后）
   - 大小写不敏感

2. 关键词检查: 遍历 destructiveKeywords
   - 如果 strings.Contains(command, keyword) → return true

3. 均未命中 → return false
```

### 6.5 checkRules()

```
函数签名: func (c *PermissionChecker) checkRules(toolName string, args json.RawMessage) (reason string, action PermissionAction)

流程:
1. 遍历 c.rules
2. 对每条规则:
   a. 遍历 rule.Tools，检查 toolName 是否匹配
      - 精确匹配: rule.Tool == toolName
      - 通配符匹配: strings.HasPrefix(toolName, "write_") 匹配 "write_*"
   b. 如果工具名匹配 → 调用 rule.Check(args)
   c. 如果 Check 返回 true → 返回 (rule.Description, rule.Action)
3. 全部未命中 → 返回 ("", ActionAllow)

匹配优先级:
- 精确匹配 > 通配符匹配 > 默认策略
- 同级别匹配按规则声明顺序，首个命中即返回
```

### 6.6 askUser()

```
函数签名: func (c *PermissionChecker) askUser(toolName string, args json.RawMessage, reason string) bool

流程:
1. 打印告警信息:
   ⚠  {reason}
      Tool: {toolName}({args trimmed})
      Allow? [y/N]

2. 读取一行用户输入 (c.approvalFn())
   - approvalFn 是可替换的函数，便于测试
   - 默认实现: bufio.NewReader(os.Stdin).ReadString('\n')

3. 处理输入:
   - 转小写 + trim 空格
   - "y" 或 "yes" → return true（放行）
   - 其他（包括空行、"n"、"no" 等） → return false（拦截）
```

### 6.7 mcpPermission()

```
函数签名: func (c *PermissionChecker) mcpPermission(server, safeTool string) mcpPermissionLevel

流程:
1. 构建 key = server + "/" + safeTool
2. 查 c.mcpPolicy 策略表
3. 命中 → 返回对应策略级别
4. 未命中 → 返回 mcpPermConfirm（兜底：未配置的外部工具一律确认）
```

### 6.8 isMCPTool()

```
函数签名: func isMCPTool(name string) bool

流程:
1. return strings.HasPrefix(name, "mcp__")
```

### 6.9 parseMCPName()

```
函数签名: func parseMCPName(name string) (server, safeTool string, ok bool)

MCP 工具名格式: mcp__{server}__{tool}

流程:
1. 去掉 "mcp__" 前缀
2. 按 "__" 分割剩余部分
3. 如果分割结果不是恰好 2 部分 → return ("", "", false)
4. return (parts[0], parts[1], true)
```

## 7. 与 eino callbacks 的集成

### 7.1 OnToolStart 回调中执行权限检查

```go
type agentCallbacks struct {
    session    *sessionState
    permission *PermissionChecker
}

func (c *agentCallbacks) OnToolStart(ctx context.Context, info *callbacks.ToolInfo) error {
    if err := c.permission.Check(info.Name, info.Input); err != nil {
        return err // 返回 error 阻止工具执行
    }
    return nil
}
```

**关键点**:
- `OnToolStart` 在工具执行前触发
- 返回 error 会阻止工具执行
- error 消息作为 tool_result（isError=true）返回给模型
- 模型可据此调整策略（例如换一种方式完成任务）

### 7.2 权限检查在 executeToolBatch 中的处理

```
在 executeToolBatch 的阶段 1（串行 PreToolUse hook）中:

遍历 toolUses:
  对每个 tool_use 构建 hookContext{ToolName, Input}
  调用 triggerPreHook(hc)
    → 内部调用 permissionChecker.Check(toolName, input)
    → Check 返回 error → 转换为 *string（error 消息）
  如果返回非 nil（被拦截）:
    blocked[i] = true
    results[i] = NewToolResultBlock(id, *blockedMsg, true)  // is_error=true
    outputs[i] = *blockedMsg
```

### 7.3 拦截消息作为 tool_result

被拦截的工具调用会返回 "Permission denied." 给模型:
- 模型可据此调整策略
- 例如: 收到 "Permission denied." 后，模型可能选择不执行该操作或换一种方式

## 8. 算法细节

### 8.1 破坏性命令正则解析

正则 `(?i)(?:^|[;&|()\n])\s*(?:rm|del)(?:\s|$|[;&|()])` 的设计：

- `(?i)` — 大小写不敏感
- `(?:^|[;&|()\n])` — 命令必须出现在合法位置：行首，或分号/管道/括号/换行后
- `\s*` — 允许前导空白
- `(?:rm|del)` — 匹配 rm 或 del 命令
- `(?:\s|$|[;&|()])` — 命令后必须跟空白、行尾或分隔符（避免误匹配 "format" 中的 "rm"）

**为什么不用 lookahead**: RE2（Go 正则引擎）不支持 lookahead，所以用等价的后缀字符集匹配。

### 8.2 策略优先级

```
MCP 工具:
  host-side 策略 (mcpHostPolicy) 优先于 server 自称的 readOnlyHint
  理由: MCP 协议的 readOnlyHint 来自 server 本身，不可作为授权依据

非 MCP 工具:
  权限模式检查 → Gate 1 (denyList) → Gate 2 (rules) → Gate 3 (askUser)
  按顺序短路，任一关拒绝即拦截
```

### 8.3 通配符匹配算法

```
func matchToolPattern(pattern, toolName string) bool:
  如果 pattern 不含 "*":
    return pattern == toolName  // 精确匹配

  如果 pattern 以 "*" 结尾:
    prefix = pattern[:len(pattern)-1]
    return strings.HasPrefix(toolName, prefix)  // 前缀匹配

  如果 pattern 以 "*" 开头:
    suffix = pattern[1:]
    return strings.HasSuffix(toolName, suffix)  // 后缀匹配

  // 通用通配符匹配
  return globMatch(pattern, toolName)
```

## 9. 错误处理

| 场景 | 处理方式 | 返回值 |
|------|---------|--------|
| MCP 工具名无法解析 | 放行（交给 runTool 报错） | nil |
| bash 参数 JSON 解析失败 | 忽略错误，command 为空串 | 由 denyList 判定 |
| 规则 Check 函数内部错误 | 忽略，视为未命中 | — |
| 用户输入读取失败 | approvalFn 返回空串 | false（拦截） |
| 未知工具名 | 三门均未命中 | nil（直通） |
| plan 模式下写入操作 | 直接拒绝 | error("write operations not allowed in plan mode") |

## 10. 输出格式

### 10.1 硬性拒绝

```
⛔ Blocked: 'sudo' is on the deny list
```

### 10.2 MCP 拒绝

```
⛔ Blocked: MCP tool "mcp__deploy__trigger" denied by host policy
```

### 10.3 用户确认

```
⚠  Potentially destructive command
   Tool: bash({"command":"rm -rf build/"})
   Allow? [y/N] 
```

### 10.4 plan 模式拒绝

```
⛔ Blocked: write operations not allowed in plan mode
```

## 11. 用户确认交互

### 11.1 终端提示格式

```
⚠  {reason}
   Tool: {toolName}({args trimmed})
   Allow? [y/N] 
```

- reason: 规则描述/告警原因
- toolName: 工具名
- args trimmed: 工具参数，截断到 60 字符
- 用户输入 "y" 或 "yes"（大小写不敏感）→ 放行
- 其他输入 → 拦截

### 11.2 超时处理

当前实现不支持超时。用户必须明确输入 y/yes 或按回车拒绝。

**未来扩展**:
- 可添加超时机制（如 30 秒无输入自动拒绝）
- 可添加 `--auto-deny` 命令行参数（非交互模式）

### 11.3 记住选择（会话内）

当前实现不记住用户选择。每次工具调用都独立检查。

**未来扩展**:
- 可添加会话内缓存: `map[string]bool`（toolName+args → 用户选择）
- 缓存有效期: 当前会话
- 缓存 key: toolName + args 的 SHA256 哈希

## 12. 权限模式详解

### 12.1 ModeDefault — 标准三闸门

```
流程: MCP 检查 → Gate 1 → Gate 2 → Gate 3 → 放行
```

**适用场景**: 日常开发，需要用户确认危险操作

### 12.2 ModeYolo — 自动允许

```
流程: Gate 1 (denyList) → 放行
```

**适用场景**: 
- 自动化测试
- CI/CD 环境
- 信任环境（如沙箱容器）

**注意**: 即使 yolo 模式，denyList 中的命令仍然被拦截（如 `rm -rf /`, `sudo`）

### 12.3 ModePlan — 只读模式

```
流程: 检查工具类型 → 写入类工具拒绝 → 只读工具放行
```

**写入类工具列表**:
- `bash` — Shell 执行
- `write_*` — 文件写入
- `edit_*` — 文件编辑

**适用场景**:
- 代码审查
- 安全分析
- 只读查询

## 13. 测试要点

### 13.1 Gate 1: denyList 测试

1. **精确命中**: `sudo apt install` → 拦截
2. **子串命中**: `echo rm -rf / tmp` → 拦截（包含 "rm -rf /"）
3. **未命中**: `echo hello` → 放行
4. **大小写**: denyList 是精确子串匹配，不做大小写转换
5. **所有模式**: 逐一验证 "rm -rf /", "sudo", "shutdown", "reboot", "mkfs", "dd if=", "> /dev/"

### 13.2 Gate 2: 破坏性命令检测测试

1. **正则命中 — 行首**: `rm -rf build/` → true
2. **正则命中 — 分号后**: `cd /tmp; rm -rf *` → true
3. **正则命中 — 管道后**: `find . | rm file` → true
4. **正则命中 — del 命令**: `del /q file.txt` → true
5. **正则不命中 — 词中间**: `format` → false（rm 不在命令位置）
6. **关键词命中**: `> /etc/passwd` → true
7. **关键词命中**: `chmod 777 file` → true
8. **全部不命中**: `ls -la` → false

### 13.3 Gate 3: 用户确认测试

1. **输入 "y"**: → true
2. **输入 "yes"**: → true
3. **输入 "Y"**: → true（大小写不敏感）
4. **输入 "YES"**: → true
5. **输入 "n"**: → false
6. **输入空行**: → false
7. **输入其他**: → false

### 13.4 MCP 权限测试

1. **allow 策略**: `docs/search` → mcpPermAllow → 放行
2. **deny 策略**: 配置 deny 的工具 → 拦截
3. **confirm 策略**: `deploy/trigger` → mcpPermConfirm → 询问用户
4. **未配置**: 任意未列出的 MCP 工具 → mcpPermConfirm（兜底）
5. **名字解析失败**: 无法解析的 MCP 名 → 放行（交给 runTool 报错）

### 13.5 Check 集成测试

1. **MCP 工具优先**: MCP 工具不走 denyList 和 rules
2. **bash 走完整管线**: denyList → rules → askUser
3. **非 bash 非 MCP**: 直接走 rules → askUser
4. **直通**: 普通 read_file 等工具 → 三门均未命中 → 放行

### 13.6 权限模式测试

1. **ModeYolo**: denyList 命中 → 拦截；其他 → 放行
2. **ModePlan**: write_file → 拦截；read_file → 放行
3. **ModeDefault**: 完整三闸门流程

### 13.7 approvalFn 可替换性

- `approvalFn` 是 PermissionChecker 的字段，测试时可替换为 mock 函数
- 验证测试中不依赖真实 stdin 输入

### 13.8 通配符匹配测试

1. **精确匹配**: "bash" 匹配 "bash"
2. **前缀匹配**: "write_*" 匹配 "write_file"
3. **不匹配**: "write_*" 不匹配 "read_file"
4. **空模式**: "" 不匹配任何工具名
