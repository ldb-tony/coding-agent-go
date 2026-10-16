# s01 Agent Loop 的 Go 实现 Spec

## Why

learn-claude-code 教程 s01 章给出了最小 Agent harness kernel:一个 `while` 循环 + 一个 bash 工具 = 一个可运行的编码 Agent。本项目(coding-agent-go)用 Go 复现该参考实现([README.md](file:///Users/tony/learn-claude-code/s01_agent_loop/README.md) / [code.py](file:///Users/tony/learn-claude-code/s01_agent_loop/code.py)),作为后续章节机制(多工具、权限、hooks、子代理等)在 Go 侧的地基。

## What Changes

- 初始化 Go module,实现最小 Agent 内核:
  - **Agent Loop**:发送 messages + tools 给 LLM → 响应含 `tool_use` 则执行工具并以 `tool_result` 回喂 → 不含 `tool_use` 则退出循环
  - **唯一工具 bash**:危险命令拦截、120s 超时、输出截断 50000 字符、空输出返回 `(no output)`
  - **交互式 REPL**:多轮对话共享 history,输入 `q` / `exit` / 空行退出;每轮结束后打印模型最终文本
  - **日志**:`input.json` 记录每轮发送给模型的完整 messages,`agent.json` 记录每次原始响应;程序启动时清空两个文件
  - **配置**:加载 `.env`,读取 `ANTHROPIC_API_KEY` / `MODEL_ID` / `ANTHROPIC_BASE_URL`;设置了 BASE_URL 时忽略 `ANTHROPIC_AUTH_TOKEN`
  - **System prompt**:注入当前工作目录与操作系统环境(Windows 走 cmd.exe / 类 Unix 走系统 shell),要求 "Act, don't explain"
- LLM 调用抽象为接口,便于用 mock 做单元测试
- 单元测试覆盖 bash 工具行为与循环终止/继续逻辑

## Impact

- Affected specs: 无(首个 spec)
- Affected code: 全新代码,仓库当前为空(仅 `.git`),无存量影响
- 外部依赖:`github.com/anthropics/anthropic-sdk-go`、`github.com/joho/godotenv`
- 参考实现行为对齐:Python 版 [code.py](file:///Users/tony/learn-claude-code/s01_agent_loop/code.py)(循环语义、拦截清单、超时、截断、日志格式均以此为准)

## ADDED Requirements

### Requirement: Agent Loop 核心循环

系统 SHALL 实现如下循环:每轮将完整 messages 与工具定义发送给 LLM;将 assistant 响应追加到 messages;若响应内容块中不含任何 `tool_use` 块则退出循环;否则执行全部 `tool_use` 块,将结果以 `tool_result` 块(`tool_use_id` 与调用一一对应)作为新的 user 消息追加,继续下一轮。`max_tokens` 固定为 8000。

#### Scenario: 模型不调用工具时退出

- **WHEN** LLM 响应中只有 text / thinking 块、没有 `tool_use` 块
- **THEN** 循环退出,该 assistant 消息已保留在 history 中

#### Scenario: 模型调用工具时循环继续

- **WHEN** LLM 响应包含一个或多个 `tool_use` 块(包括同一响应中的多个并行调用)
- **THEN** 每个调用都被执行,全部 `tool_result` 作为一条 user 消息回喂,循环进入下一轮

#### Scenario: 无空调用不产生空 tool_result

- **WHEN** 某轮响应没有 `tool_use` 块
- **THEN** 不追加任何 tool_result 消息,直接退出

### Requirement: bash 工具

系统 SHALL 提供唯一工具 `bash`(description: "Run a shell command.",input schema 仅含必填字符串 `command`),执行语义与参考实现一致:

- 命令经系统 shell 执行,工作目录为进程当前目录
- 拦截包含以下子串的命令,直接返回 `Error: Dangerous command blocked`:`rm -rf /`、`sudo`、`shutdown`、`reboot`、`> /dev/`
- 超时 120 秒,超时返回 `Error: Timeout (120s)`
- 合并 stdout 与 stderr 并去除首尾空白;为空时返回 `(no output)`
- 输出超过 50000 字符时截断到 50000 字符
- 执行错误(如启动失败)返回 `Error: <err>`,不 panic

#### Scenario: 危险命令被拦截

- **WHEN** 工具输入命令包含 `sudo`
- **THEN** 返回 `Error: Dangerous command blocked`,且不实际执行

#### Scenario: 正常命令返回合并输出

- **WHEN** 执行 `echo hello`
- **THEN** 返回 `hello`

#### Scenario: 空输出占位

- **WHEN** 命令成功但无任何输出(如 `true`)
- **THEN** 返回 `(no output)`

### Requirement: System Prompt

系统 SHALL 构造 system prompt:包含当前工作目录、操作系统环境说明(Windows:"the bash tool runs through cmd.exe; use cmd.exe syntax";类 Unix:"the bash tool runs the system shell"),并以 "Use bash to solve tasks. Act, don't explain." 结尾。

### Requirement: 交互式 REPL

系统 SHALL 提供终端 REPL:启动时打印标题、日志文件路径与退出提示;循环读取用户输入;输入 `q`、`exit` 或空行(均忽略大小写与首尾空白)退出;EOF / Ctrl-C 退出;每次有效输入追加为 user 消息并进入 Agent Loop,期间打印每条被执行的命令及输出前 200 字符,结束后打印该轮模型最终的 text 块;history 跨轮保留。

### Requirement: 调用日志

系统 SHALL 在程序启动时将 `agent.json` 与 `input.json` 重置为空数组;之后每轮调用 LLM 前把完整 messages 追加到 `input.json`(含 `ts` UTC 时间戳、`turn` 序号、`input` 字段),收到响应后把响应追加到 `agent.json`(含 `ts`、`turn`、`response` 字段);日志文件位于程序工作目录,JSON 缩进 2 空格、不转义非 ASCII 字符。

### Requirement: 环境配置

系统 SHALL 启动时加载 `.env`(存在则覆盖已有环境变量);从环境读取 `ANTHROPIC_API_KEY`、`MODEL_ID`(必填)、`ANTHROPIC_BASE_URL`(可选,用于兼容端点);当设置了 `ANTHROPIC_BASE_URL` 时忽略 `ANTHROPIC_AUTH_TOKEN`;提供 `.env.example` 示例文件。

### Requirement: 可测试性

系统 SHALL 将 LLM 调用抽象为接口(输入 system/messages/tools/参数,返回结构化响应),Agent Loop 依赖该接口,使循环逻辑可在无真实 API 的情况下用 mock 测试。

## 非目标(后续章节处理)

- 多工具与并行工具冲突(s02)
- 权限审批(s03)、hooks(s04)、子代理、上下文压缩等 s05+ 机制
- 流式输出、token 统计展示、TUI 美化
