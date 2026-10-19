# s03 权限门控(Permission Pipeline)Spec

## Why

当前 Agent 有 6 个工具:文件工具受 `safePath` 保护,但 bash 只靠工具内部简单的子串拦截,且安全逻辑散落在工具内部。安全不能依赖信任模型 —— 需要在每次工具执行前,由代码统一做一道权限检查门。

## What Changes

- 新增 `permission.go`:三闸门权限管线,插入 agent loop 中每个工具调用执行之前
  - **Gate 1 硬性拒绝清单**:bash 命令命中拒绝模式(如 `rm -rf /`、`sudo`、`shutdown`、`reboot`、`mkfs`、`dd if=`、`> /dev/`)→ 立即拒绝,不执行
  - **Gate 2 规则匹配**:bash 破坏性命令(命令位置的 `rm`/`del`,或含 `rm `、`> /etc/`、`chmod 777`)→ 转 Gate 3
  - **Gate 3 用户确认**:打印原因与工具参数,交互询问 `Allow? [y/N]`,默认拒绝
  - 三个门都未命中 → 直接执行(大多数常规操作走此路径)
- 修改 `agent.go`:执行 `runTool` 前调用 `checkPermission()`;被拒绝的调用回喂 `tool_result`(content 为 `Permission denied.`,`is_error=true`),继续处理其余调用
- 修改 `bash.go`:移除内置 `dangerousCommandSubstrings` 拦截(上移为 Gate 1,单一职责);拒绝清单取现有清单与 README 清单的并集(保留更宽的 `> /dev/`,新增 `mkfs`、`dd if=`)
- 修改 `main.go`:banner 与提示符由 `s02` 更新为 `s03`
- 新增 `permission_test.go`;`bash_test.go` 中"危险命令拦截"子测试迁移至权限层测试

## Impact

- Affected specs: 无(新能力,不改变既有文件工具契约)
- Affected code:
  - 新增:`permission.go`、`permission_test.go`
  - 修改:`agent.go`(插入权限检查)、`bash.go`(移除内部拦截)、`bash_test.go`(迁移拦截测试)、`main.go`(banner)
- 行为变化:`runBash` 单独调用不再自带拦截(拦截发生在 loop 层);被拒绝的工具调用现在以 `is_error=true` 的 `tool_result` 回喂模型

## 与 README 的刻意差异(Adaptation Notes)

- **工作区外文件访问**:README 的 Gate 2 含"文件工具访问工作区外路径 → 询问用户"规则。本项目硬约束要求所有文件工具必须通过 `safePath` 校验、禁止工作区路径逃逸,故路径逃逸继续由 `safePath` **硬拒绝**(比 README 的"询问"更强),Gate 2 仅覆盖 bash 破坏性命令
- **正则改写**:README 的 `(?i)(?:^|[;&|()\n])\s*(?:rm|del)(?=\s|$|[;&|()])` 使用 lookahead,Go RE2 不支持;改写为等价的无 lookahead 形式 `(?i)(?:^|[;&|()\n])\s*(?:rm|del)(?:\s|$|[;&|()])`(仅做布尔匹配,消费尾随字符无副作用)
- **拒绝清单并集**:保留现有 `> /dev/`(覆盖 README 的 `> /dev/sda` 且更宽,含 `> /dev/null` 既有行为),并按 README 新增 `mkfs`、`dd if=`

## ADDED Requirements

### Requirement: Gate 1 硬性拒绝清单

系统 SHALL 在执行 bash 工具前检查命令是否命中拒绝清单(简单子串匹配),命中则立即拒绝执行,终端打印 `⛔ Blocked: '<pattern>' is on the deny list`,并向模型回喂 `Permission denied.` 的 tool_result。拒绝清单仅作用于 bash 工具。

#### Scenario: 命中拒绝模式

- **WHEN** 模型发起 `bash` 调用且命令为 `rm -rf /`(或 `sudo ls`、`shutdown now`、`mkfs.ext4 /dev/sda`、`dd if=/dev/zero of=x`、`echo hi > /dev/null` 等含清单子串的命令)
- **THEN** 该命令不被执行,终端打印拒绝原因,模型收到 `Permission denied.`

#### Scenario: 非 bash 工具跳过 Gate 1

- **WHEN** 模型发起 `read_file`/`write_file` 等非 bash 调用
- **THEN** 不经过拒绝清单检查,直接进入 Gate 2 评估

### Requirement: Gate 2 破坏性命令规则匹配

系统 SHALL 维护规则表(每条规则 = 工具集合 + 检查条件 + 提示消息)。对 bash 工具,当命令包含"命令位置的 `rm` 或 `del`"(正则 `(?i)(?:^|[;&|()\n])\s*(?:rm|del)(?:\s|$|[;&|()])`),或包含子串 `rm `、`> /etc/`、`chmod 777` 时,判定为"潜在破坏性命令",转 Gate 3 询问用户。

#### Scenario: 触发规则

- **WHEN** 模型发起 bash 调用,命令为 `rm test.txt`、`del test.txt`、`DEL test.txt`、`ls && rm -rf build`、`chmod 777 x` 之一
- **THEN** 不直接执行,进入用户确认流程,提示原因为 `Potentially destructive command`

#### Scenario: 不误触发

- **WHEN** 命令为 `model`、`delimiter`、`echo del test.txt`(rm/del 不在命令位置,也不含关键词子串)
- **THEN** 不触发规则,命令直接执行

### Requirement: Gate 3 用户确认

系统 SHALL 在规则命中后暂停,打印告警原因与 `Tool: <name>(<args>)`,提示 `Allow? [y/N]` 并读取一行用户输入;输入 `y`/`yes`(大小写不敏感)则放行执行,其余任何输入(含空行/EOF)一律拒绝。确认输入源须为可替换的包级变量,以便单元测试注入。

#### Scenario: 用户放行

- **WHEN** Gate 2 命中且用户输入 `y`
- **THEN** 工具正常执行,结果回喂模型

#### Scenario: 用户拒绝(含默认)

- **WHEN** Gate 2 命中且用户输入 `n`、空行或其他任意内容
- **THEN** 工具不执行,模型收到 `Permission denied.`

### Requirement: 权限门与 agent loop 集成

系统 SHALL 在 agent loop 中对每个 `tool_use` 块按 LLM 返回顺序串行执行 `checkPermission()` 后再执行工具;被拒绝的调用生成 `tool_result`(content=`Permission denied.`,`is_error=true`)并继续处理同一轮内的其余工具调用;三个门均未命中的调用直接执行。

#### Scenario: 部分调用被拒绝

- **WHEN** 模型在同一轮返回两个 bash 调用:`echo one` 与 `rm -rf /`
- **THEN** `echo one` 正常执行回喂结果;`rm -rf /` 被拒绝并回喂 `Permission denied.`;loop 不中断,继续后续轮次

#### Scenario: 常规操作直通

- **WHEN** 模型发起 `bash: echo hello`、`read_file: main.go` 等不命中任何门的调用
- **THEN** 无告警、无询问,直接执行(与现状行为一致)

## MODIFIED Requirements

### Requirement: bash 工具危险命令拦截

**原状**:`runBash` 内部硬编码 `dangerousCommandSubstrings` 子串拦截,返回 `Error: Dangerous command blocked`。

**新状**:bash 工具不再内置拦截逻辑,拦截职责上移至权限层 Gate 1;拒绝消息统一为 `Blocked: '<pattern>' is on the deny list`(终端)与 `Permission denied.`(tool_result)。`bash_test.go` 中对应子测试迁移到 `permission_test.go`,断言新的拒绝语义。

### Requirement: REPL banner 与提示符

**原状**:启动打印 `s02: Tool Use`,提示符 `s02 >> `。

**新状**:启动打印 `s03: Permission`,提示符 `s03 >> `,其余启动行为(日志路径打印、退出说明)不变。
