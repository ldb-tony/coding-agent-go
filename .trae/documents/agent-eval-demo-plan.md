# coding-agent-go 评测框架 + demo 测试集 实施计划

## Summary

给 coding-agent-go 建一套**黑盒评测框架**：在临时工作区里跑编译出的真实二进制，用隐藏测试校验结果，自动产出「摘要报告 + JSON 明细 + 每轮交互日志」。

本期范围（用户已确认）：
- **只落地 `demo` 场景的 3 个常用测试集**，并支持用 `-task <id>` 指定跑具体用例。
- 框架本身支持 `demo / small / full` 三个场景的选取能力，`small`/`full` 的任务注册表本期为空，后续直接往里加任务即可，不改代码。
- **不用 Docker**：全部任务走语言原生环境（Go 的 `go test`，无外部依赖），日志路径与今天的 debug 工作流完全一致。
- 报告形式：**Markdown 摘要 + JSON/JSONL 明细**。
- 任务定义格式预留 SWE-bench 兼容字段（`repo` 起点 + `verify` 的 `fail_to_pass`/`pass_to_pass`），后续可无缝灌入开源基准。

产出物两类别：
1. **生产代码的最小改动**（让 agent 能被无头驱动 + 日志可定向 + 可计时），见「Proposed Changes A」。
2. **`cmd/eval` 评测 runner + `evals/` 任务集与产物目录**，见「Proposed Changes B / C」。

## Current State Analysis

基于对仓库的实际阅读（行号为准）：

| 事实 | 位置 | 对本次设计的影响 |
|---|---|---|
| 唯一入口是 stdin 交互 REPL，无 CLI 参数、无一次执行模式 | [main.go L14-L73](file:///Users/tony/coding-agent-go/main.go#L14-L73) | 黑盒评测必须新增 `-p` 无头模式 |
| 日志写死在包级变量 `agent.json` / `input.json`（cwd 相对路径），每次启动 `resetLogs()` 清空 | [logging.go L13-L25](file:///Users/tony/coding-agent-go/logging.go#L13-L25) | 需支持 `EVAL_LOG_DIR` 定向到 per-task 目录 |
| 每次 `Send` 前后各记一条带 RFC3339Nano `ts` 的日志（`logInput` 在 Send 前、`logResponse` 在 Send 后） | [agent.go L31-L47](file:///Users/tony/coding-agent-go/agent.go#L31-L47) | **每轮 LLM 耗时 = `agent.json.ts - input.json.ts`**，无需额外插桩就能拿到 |
| 响应体自带 `usage`（input/output/cache tokens） | [llm.go L49-L57](file:///Users/tony/coding-agent-go/llm.go#L49-L57) | token 维度直接从 `agent.json` 解析 |
| loop 为 `for turn := 0; ; turn++`，**无轮数上限** | [agent.go L25](file:///Users/tony/coding-agent-go/agent.go#L25) | runner 必须自带超时 + 杀整个进程组 |
| `workdir` 取进程 cwd，`safePath` 以此为根 | [tools.go L22-L31](file:///Users/tony/coding-agent-go/tools.go#L22-L31) | **把 agent 的 cwd 设为任务工作区副本**，即可天然隔离且不改代码 |
| 工具集：bash(+`run_in_background`)、read/write/edit_file、glob、grep、todo_write、load_skill、compact、task(子代理)、create/update/claim/complete/get/list_task | [tools.go L662-L756](file:///Users/tony/coding-agent-go/tools.go#L662-L756) | demo 用例据此选型，覆盖核心工具链路 |
| 记忆子系统在轮末发起**额外 LLM 调用**（召回/抽取/合并），开关是包级 `memoryEnabled = true` | [memory.go L374-L380](file:///Users/tony/coding-agent-go/memory.go#L374-L380)、[memory.go L642-L645](file:///Users/tony/coding-agent-go/memory.go#L642-L645) | 评测需确定性 + 避免污染耗时/token → 需可关闭 |
| 权限 Gate 3 在破坏性命令时阻塞读 stdin 确认 | [permission.go L84-L112](file:///Users/tony/coding-agent-go/permission.go#L84-L112) | 无头模式 stdin 为 EOF → `choice=""` → 直接拒绝（**不阻塞、不 hang**），行为可预期 |
| hook 在 `init()` 注册一次（含打印型 hook） | [hooks.go L69-L79](file:///Users/tony/coding-agent-go/hooks.go#L69-L79) | 无头模式下 stdout 会混入 `[HOOK]` 噪音；runner 把 stdout 原样存档，供 debug |
| 已有测试用 `setupLogPaths` 覆盖日志路径、覆盖 `workdir` | [agent_test.go L38-L51](file:///Users/tony/coding-agent-go/agent_test.go#L38-L51) | 与无头模式的 env 覆盖机制一致，可复用同思路 |
| 模块名 `coding-agent-go`，Go 1.24.4，无 YAML 直接依赖 | [go.mod](file:///Users/tony/coding-agent-go/go.mod) | runner 放 `cmd/eval`（同模块新包），任务 spec 用 **JSON** 避免新增依赖 |
| `.env` 由 `godotenv.Overload()` 从 **cwd** 加载 | [llm.go L26](file:///Users/tony/coding-agent-go/llm.go#L26) | **关键坑**：agent 的 cwd 是任务工作区，那里没有 `.env` → `MODEL_ID` 为空会 `log.Fatal`。runner 必须把 `.env` 解析后注入 `cmd.Env` |
| 仓库根存在已编译二进制 `coding-agent-go`（已 gitignore） | [.gitignore L9](file:///Users/tony/coding-agent-go/.gitignore#L9) | runner 每次 `go build` 现编，保证测的是当前代码 |

## 决策与假设

**决策**
1. **黑盒驱动**：runner 编译并启动真实二进制，`cwd = 任务工作区`，通过 `-p "<prompt>"` 一次执行。不改 `agentLoop` 语义。
2. **日志定向而非新增日志系统**：新增 env `EVAL_LOG_DIR`，把 `agentLogPath`/`inputLogPath` 指到 `runs/<run_id>/<task_id>/logs/`。per-turn 耗时/token 全部由既有日志推导 → 今天怎么读日志 debug，评测就怎么读。
3. **可选工具耗时追踪**：新增 env `EVAL_TRACE_TOOLS=1` 时，在 `agent.go` / `task.go` 的工具执行前后各记一条到 `logs/tools.json`。这是唯一为评测新增的插桩，默认关闭，用于回答「哪一次工具调用把时间吃掉了」。
4. **评测确定性**：新增 env `EVAL_DISABLE_MEMORY=1` 关闭记忆召回/抽取/合并（省 LLM 调用、去随机性）。
5. **隐藏测试注入**：agent 只看到 `repo/` 快照；校验前把 `tests/` 覆盖进工作区再跑 `verify`（SWE-bench 的 hidden-test 语义）。
6. **防作弊**：spec 支持 `protected_files`（path + sha256），agent 改动了被保护文件（例如测试文件）即判失败。
7. **超时即失败**：进程超时/崩溃/API 报错一律记 `status=timeout|agent_error`，`completion=0`，不算「完成」。
8. **同一个二进制、不同 scenario**：`-scenario` 决定任务选择；demo/small/full 共用一套 runner 与报告格式。
9. **报告语言**：`report.md` 中文，任务 id / 字段名保持英文（便于和开源基准对齐）。

**假设**
- demo 任务不依赖网络（除模型 API）与外部包；Go 工具链在本机可用。
- 工作区 fixture 中不能有 `.env` 文件（否则 `godotenv.Overload()` 会覆盖注入的 env）。
- demo 任务的解法不需要触发权限 Gate 3 的破坏性命令（无头下会被直接拒绝，属已知行为，不作为失败原因归因给 agent）。
- 子代理与压缩 summary 的 `client.Send` 不经过 `logInput/logResponse`（[compact.go L365-L370](file:///Users/tony/coding-agent-go/compact.go#L365-L370) 直接调用），故 `llm_total_ms` 在触发 compaction 的长任务里会低估。demo 任务足够的短，不触发该路径；此限制记入报告附录。

## Metrics 定义（写入报告附录，避免面试时歧义）

- **完成率 completion** `= resolved_tasks / executed_tasks`。某任务 `resolved` ⟺ 所有 `fail_to_pass` 校验通过 **且** 所有 `pass_to_pass` 校验仍通过 **且** 无 `protected_files` 违规 **且** 进程正常退出未超时。
- **准确率 accuracy** `= 通过的校验项数 / 总校验项数`（校验项级、含部分分）。报告同时给「总体加权准确率」与「每任务准确率」。
- **耗时**
  - `agent_wall_ms`：agent 进程墙钟（不含校验）
  - `verify_wall_ms`：校验命令总耗时
  - `llm_total_ms` / `llm_p50_ms` / `llm_p95_ms` / `llm_max_ms`：每轮 `agent.json.ts - input.json.ts` 的统计
  - `llm_share = llm_total_ms / agent_wall_ms`：模型占比（越低说明工具/开销占比越高）
  - `tool_total_ms` / 每工具明细（`EVAL_TRACE_TOOLS=1` 时）
- **tokens**：`input / output / cache_read / cache_write` 求和（来自 `usage`）；**不做金额估算**（该模型无公开价表，避免编造）。
- **turns**：一次任务的模型交互轮数。

## Proposed Changes

### A. 生产代码最小改动（3 个文件）

**A1. [main.go](file:///Users/tony/coding-agent-go/main.go) — 新增无头一次执行模式**

- What：在 REPL 之前解析 `-p`/`--prompt`；给出 prompt 时执行一次后退出。
- How：
  - `parsePromptArg(args []string) (string, bool)`：支持 `-p <prompt>`、`--prompt <prompt>`、`--prompt=<prompt>`；同时解析可选 `--print-logs`（默认 false，忽略）。
  - 命中则走与 REPL **同一路径**：`triggerHooks(EventUserPromptSubmit, ...)` → `append(user message)` → `agentLoop(context.Background(), client, history, query)` → 打印最后一条 assistant 的 text 块 → `backgroundTasks.stopAll()` → 成功 `os.Exit(0)`，出错 `os.Exit(1)`。
  - 保持 `resetLogs()` 在最前面调用（`EVAL_LOG_DIR` 下即为空目录，天然干净）。
- Why：黑盒评测需要一个「一次执行、可脚本调用、退出码可判」的入口；复用 REPL 同一代码路径，避免「测的不是线上路径」。

**A2. [logging.go](file:///Users/tony/coding-agent-go/logging.go) — 日志路径可定向 + 可选工具追踪**

- What：
  - `agentLogPath`/`inputLogPath` 改为由 `initLogPaths()` 从 env 计算：`EVAL_LOG_DIR` 非空时 = `<dir>/agent.json`、`<dir>/input.json`，否则保持 `agent.json`/`input.json`（行为不变）。
  - 新增 `toolLogPath`（`<EVAL_LOG_DIR or cwd>/tools.json`）与 `traceTool(name string, args json.RawMessage, start time.Time, out string, ok bool)`；仅当 `EVAL_TRACE_TOOLS == "1"` 时写入，其余场景零开销。
  - 复用现有 `appendLog` 的 JSON 数组写法，条目含 `ts`、`tool`、`args_preview`（≤200 rune）、`duration_ms`、`ok`、`output_preview`（≤200 rune）。
- Why：满足「每轮交互日志留存 + 每次模型交互耗时」的硬要求；工具级耗时用于定位「哪里卡住」。

**A3. [memory.go](file:///Users/tony/coding-agent-go/memory.go) / [agent.go](file:///Users/tony/coding-agent-go/agent.go) / [task.go](file:///Users/tony/coding-agent-go/task.go) — 关闭开关 + 工具追踪调用点**

- What：
  - `memory.go` L645 改为 `var memoryEnabled = os.Getenv("EVAL_DISABLE_MEMORY") != "1"`（正常运行行为不变）。
  - `agent.go` 主循环里 `runTool`/`startBackgroundTask` 分支前后各取 `time.Now()`，包一个 `traceTool(...)` 调用；`task.go` 子代理中同样处理（保持父子对称）。
- Why：A3 前半保证评测确定性；后半是唯一插桩点，改动面 2 处、默认关。

### B. 评测 runner：`cmd/eval`（新包，同模块）

新文件：`cmd/eval/main.go`、`cmd/eval/run.go`、`cmd/eval/metrics.go`、`cmd/eval/report.go`、`cmd/eval/proc_unix.go`、`cmd/eval/metrics_test.go`。
仅用标准库（`flag`、`os/exec`、`encoding/json`、`syscall`），不引入新依赖。

**CLI 契约（decision-complete）**

```
go run ./cmd/eval [flags]
  -scenario  demo|small|full    默认 demo
  -task      id[,id...]         demo 下指定具体用例（覆盖 -scenario 的全量选择）
  -tags      a,b                按 tag 过滤
  -list                         只列出被选中的任务，不执行
  -verify-only                  跳过 agent，用原始 repo 直接跑校验（校验管线自检，确定性）
  -repeat    N                  每任务重复次数（默认 1）
  -model     <id>               覆盖 .env 的 MODEL_ID
  -env-file  .env                注入给 agent 的环境变量来源
  -timeout   <seconds>          单任务 agent 超时上限（覆盖 spec.timeout_sec）
  -runs-dir  evals/runs
  -report-lang zh|en            默认 zh
```

**每个任务的执行流程**

1. `run_id = 20060102-150405`；建立 `evals/runs/<run_id>/`。
2. 一次性 `go build -o evals/runs/<run_id>/.bin/coding-agent-go .`（保证测当前代码）。
3. 任务工作区：`cp -R evals/tasks/<scenario>/<id>/repo → runs/<run_id>/<id>/workspace`（保留脚本，不删除，便于 debug）。
4. 启动 agent：
   - `cmd.Dir = workspace`
   - `cmd.Env = 解析 .env 得到的环境 + MODEL_ID + ANTHROPIC_API_KEY + ANTHROPIC_BASE_URL + EVAL_LOG_DIR=<id>/logs + EVAL_TRACE_TOOLS=1 + EVAL_DISABLE_MEMORY=1`
   - 参数：`-p "<prompt>"`
   - `cmd.Stdin = nil`（EOF，不阻塞）；`stdout/stderr → <id>/agent.stdout.log`
   - `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}`；超时用 `syscall.Kill(-pgid, SIGKILL)` 杀整组（`proc_unix.go` 实现，Windows 走 `taskkill` 的 build-tag 分支；本机 macOS 为主路径）
   - 记录 `agent_wall_ms`、退出码
5. 注入隐藏测试：`cp -R evals/tasks/<scenario>/<id>/tests/* → workspace`（覆盖）。
6. `protected_files` 校验：逐个算 sha256 对比，违规记入 `protected_violations`。
7. 按 `spec.verify[]` 顺序执行校验命令（`sh -c`，各自 timeout），记录 `exit_code / ms / output_tail`；`kind ∈ {fail_to_pass, pass_to_pass}`。
8. 解析日志算指标（见 Metrics），组装 `result.json`、追加 `results.jsonl`。

**并发**：demo 默认**串行**（任务少、日志好读）；提供 `-jobs 1`（默认 1，预留 >1 用 worker pool，因 agent 进程互相独立，工作区隔离后可安全并发）。

### C. `evals/` 目录结构

```
evals/
  README.md                       # 任务格式说明 + 指标定义 + 如何加 small/full 任务（中文）
  schema/task.schema.json         # 任务 spec 的 JSON Schema（含 SWE-bench 兼容字段）
  tasks/
    demo/
      demo-01-fix-failing-test/
        task.json
        repo/                     # 交给 agent 的起点仓库（含可见失败测试）
        tests/                    # 隐藏测试（此处留空，测试本就可见）
      demo-02-precise-edit/
        task.json
        repo/                     # 含一个"重复行"诱饵，逼迫 unique-match 处理
        tests/                    # 隐藏测试，校验前注入
      demo-03-multi-file-rename/
        task.json
        repo/                     # 跨 3 文件/3 目录使用旧符号名
        tests/                    # 隐藏测试，校验前注入
  runs/                           # 产物（加入 .gitignore）
    <run_id>/
      report.md                   # 摘要报告
      results.jsonl               # 一行一任务
      <task_id>/
        workspace/                # 任务工作区副本（保留）
        logs/{input.json,agent.json,tools.json}   # 每轮交互日志
        agent.stdout.log
        result.json               # 完整明细
```

**task.json 字段（本期实现的完整集合）**

```json
{
  "id": "demo-01-fix-failing-test",
  "scenarios": ["demo", "small"],
  "tags": ["bugfix", "bash", "edit_file"],
  "language": "go",
  "prompt": "`go test ./...` fails. Fix the bug so all tests pass. Do not modify or weaken any test file.",
  "timeout_sec": 180,
  "protected_files": [{"path": "calc_test.go", "sha256": "<hex>"}],
  "verify": [
    {"name": "F2P: TestAdd", "kind": "fail_to_pass", "cmd": "go test ./calc -run TestAdd -count=1", "timeout_sec": 60},
    {"name": "P2P: existing suite", "kind": "pass_to_pass", "cmd": "go test ./calc -run 'TestSub|TestMul' -count=1", "timeout_sec": 60}
  ],
  "notes": "覆盖 loop+bash+read+edit 的基本链路"
}
```

预留（本期不实现逻辑，仅落在 schema 里并写注释）：`repo_url`/`base_commit`（远程起点）、`docker_image`（Phase 2 容器后端）、`install_cmd`。

**3 个 demo 用例（覆盖核心工具链路，均为无外部依赖的 Go 小仓库）**

| id | 场景 | 起点 | 期望解法要点 | 校验 | 覆盖的 agent 能力 |
|---|---|---|---|---|---|
| `demo-01-fix-failing-test` | `go test ./...` 报错 | module `calc`：`calc.go` 里 `Add` 错写成减法，`calc_test.go` 可见失败 | 跑测试 → 读文件 → 精确 edit → 复跑 | F2P `TestAdd`；P2P 其余用例；protect `calc_test.go` | loop、bash、read_file、edit_file、错误迭代 |
| `demo-02-precise-edit` | issue 风格描述 | module `parser`：`ParseDuration` 处理 `1h30m` 有误；**另一处文件有结构相似的诱饵行**，直接替换会命中 2 次 | 必须先全量 read，再用唯一上下文 edit | F2P `go test ./parser -run TestParseDuration`；P2P `go test ./...` | read-before-write 门控、edit unique-match 失败后的恢复、grep 定位 |
| `demo-03-multi-file-rename` | 跨包重命名 | module `svc`：`GetUserName` 在 `api/`、`store/`、`internal/util/` 共 4 处被引用 | grep 全仓 → glob 确认范围 → `edit_file replace_all` 或逐处 edit → `go build/test` 收口 | F2P `go test ./...`（隐藏测试调新名）；P2P `sh -c '! grep -rn "GetUserName" --include="*.go" .'` | grep、glob(`**`)、edit_file(`replace_all`)、多文件一致性、build/test 收口 |

三者共同点：耗时短（预期 < 60s）、无网络依赖、任何一环坏掉都会**立刻红**——正好满足「1-3 个用例快速验证 agent 是否有明显 bug」。三者都是「以隐藏测试为判据」的真实修复任务，而非演示脚本，符合面试说服力要求。

### D. 报告产物

**`report.md`（摘要，中文）** 章节固定：
1. Run 头：run_id / 时间 / model / git rev / scenario / 任务数 / 是否 verify-only
2. **总体指标表**：完成率、准确率、agent_wall p50/p95、llm p50/p95、llm_share、总 tokens、turns
3. **分任务表**：id、status、completion、accuracy(x/y)、agent_wall、llm 合计、turns、tools 次数、日志相对路径
4. **失败明细**：每个未完成任务列出失败的校验项 + 退出码 + 输出尾部 + agent 最后一段 text + 日志路径（一键定位）
5. **附录：指标定义**（上文 Metrics 全量）+ 已知限制（compaction 期间 summary 调用不计数等）

**`results.jsonl`**：一行一任务，字段与 `result.json` 同构（便于 `jq`/脚本消费与回归对比）。
**`<task_id>/result.json`**：完整明细，含 `turns_detail[]`（每轮 llm_ms/tokens）与 `tool_detail[]`，即「完整的报告」。

### E. 其它

- [.gitignore](file:///Users/tony/coding-agent-go/.gitignore)：新增 `evals/runs/`。
- `evals/README.md`：写清任务格式、指标定义、跑法、以及**如何扩展 small / full**（把 `scenarios` 改成含 `small`/`full` 即可被选中；开源基准导入路径：把 SWE-bench-style JSONL 转成同构 `task.json` + `repo/` + `tests/`，不需要改 runner）。

## 实施顺序

1. A1–A3 生产改动 → `go build ./...` 通过 → 手动 `./coding-agent-go -p "read README.md and summarize"` 冒烟。
2. C：建 `evals/schema/task.schema.json` + 3 个 demo 任务（fixture 仓库、隐藏测试、task.json）+ `evals/README.md`。
3. B：`cmd/eval` runner（选任务 → 建工作区 → 无头跑 agent → 注入测试 → 校验 → 算指标 → 出报告）。
4. 自检：`-verify-only` 跑 3 个任务（确定性），确认 F2P 全挂、P2P 全过、protect 生效 → 证明校验管线正确。
5. 真实跑：`-scenario demo` 一次，人工核对 report.md 指标与 logs 里的 per-turn 耗时是否对得上。
6. `.gitignore` 更新。

## Verification

- **编译**：`go build ./... && go vet ./...` 全绿；既有 `go test ./...` 全部保持通过（A 系列改动不破坏原有测试）。
- **runner 单测**：`cmd/eval/metrics_test.go` 用构造的 `input.json`/`agent.json`/`tools.json` 断言 llm p50/p95、llm_share、tokens 求和、F2P/P2P 判定、protected sha256 违规判定、`status=timeout` 归类。
- **校验管线自检（确定性）**：`go run ./cmd/eval -scenario demo -verify-only` → 3 个任务 `completion=0`（F2P 未通过）、`pass_to_pass` 全通过、`protected_violations=[]`；证明「未修复 → 未完成」的判据成立，排除假阳性。
- **指定用例**：`go run ./cmd/eval -list` 列出 3 个 demo 用例；`-task demo-02-precise-edit` 只跑该例（断言只产出 1 个任务目录）。
- **端到端**：`go run ./cmd/eval -scenario demo` 产出 `runs/<run_id>/report.md`、`results.jsonl`、每个任务的 `logs/{input.json,agent.json,tools.json}` 与 `agent.stdout.log`；抽查 `result.json.turns_detail` 的 `llm_ms` 与 `agent.json`/`input.json` 的 ts 差值一致；`report.md` 中完成率 = resolved/3、准确率 = 通过项/总项 可手工复算。
- **无头不阻塞**：确认 agent 在有破坏性命令意图时输出 `Permission denied.` 并在超时内退出（不挂住）。
