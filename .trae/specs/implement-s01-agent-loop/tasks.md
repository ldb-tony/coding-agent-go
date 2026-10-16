# Tasks

- [x] Task 1: 初始化 Go module 与外部依赖
  - [x] SubTask 1.1: `go mod init coding-agent-go`
  - [x] SubTask 1.2: 引入 `github.com/anthropics/anthropic-sdk-go`、`github.com/joho/godotenv`
  - [x] SubTask 1.3: 创建 `.env.example`(含 `ANTHROPIC_API_KEY`、`MODEL_ID`、`ANTHROPIC_BASE_URL` 占位)
  - [x] SubTask 1.4: `go mod tidy` 并确认可编译

- [x] Task 2: 实现 bash 工具与单元测试
  - [x] SubTask 2.1: 实现 `runBash(command string) string` 函数(危险拦截、超时 120s、输出截断 50000、合并 stdout/stderr、空输出占位、错误容错)
  - [x] SubTask 2.2: 编写 `bash_test.go` 覆盖:危险命令拦截、正常输出、空输出、超时、stderr 合并、截断

- [x] Task 3: 实现 LLM 客户端封装与配置加载
  - [x] SubTask 3.1: 实现 `.env` 加载逻辑及环境变量读取(`ANTHROPIC_API_KEY`、`MODEL_ID`、`ANTHROPIC_BASE_URL`,并处理 `ANTHROPIC_AUTH_TOKEN` 逻辑)
  - [x] SubTask 3.2: 定义 LLM 接口(支持 system/messages/tools/max_tokens 输入,返回 assistant content blocks)
  - [x] SubTask 3.3: 使用 `anthropics/anthropic-sdk-go` 实现接口,支持自定义 base_url;构造 system prompt(含 cwd、环境说明、"Act, don't explain.")

- [x] Task 4: 实现 Agent Loop 与单元测试
  - [x] SubTask 4.1: 实现 `agentLoop(messages []MessageBlock, client LLMClient)` 函数,严格匹配 spec 中的循环语义
  - [x] SubTask 4.2: 提供 mock LLM client(模拟含 tool_use 与不含 tool_use 的响应)
  - [x] SubTask 4.3: 编写 `agent_test.go` 覆盖:无 tool_use 时循环退出、有 tool_use 时循环继续并回喂 tool_result、多个并行 tool_use 全部执行

- [x] Task 5: 实现日志系统
  - [x] SubTask 5.1: 启动时重置 `agent.json` 与 `input.json` 为空 JSON 数组
  - [x] SubTask 5.2: 每轮调用前记录完整 input messages 到 `input.json`(含 `ts`、`turn`、`input`)
  - [x] SubTask 5.3: 每轮收到响应后记录完整 response 到 `agent.json`(含 `ts`、`turn`、`response`)
  - [x] SubTask 5.4: 确保 JSON 格式:缩进 2、不转义 Unicode、时间戳 UTC ISO-8601

- [x] Task 6: 实现 REPL 入口
  - [x] SubTask 6.1: 启动时打印标题、`input.json`/`agent.json` 路径、退出提示
  - [x] SubTask 6.2: 实现输入循环:读取 stdin、`q`/`exit`/空行退出、EOF/Ctrl-C 安全退出
  - [x] SubTask 6.3: 将用户输入追加为 user message,调用 agentLoop,循环结束后打印模型最终 text 块
  - [x] SubTask 6.4: history 跨轮保留
  - [x] SubTask 6.5: 执行命令时打印彩色命令与输出前 200 字符(兼容常见终端)

- [x] Task 7: 验证与清理
  - [x] SubTask 7.1: 运行 `go test ./...` 全部通过
  - [x] SubTask 7.2: 手动端到端验证(填写 `.env` 后用示例 prompt 跑通一个完整回合)
  - [x] SubTask 7.3: 清理所有非必要调试代码,确保无未使用的 import/变量

# Task Dependencies

- Task 2 与 Task 3 可并行,均依赖 Task 1
- Task 4 依赖 Task 3(LLM 接口)与 Task 2(bash 工具)
- Task 5 与 Task 6 均依赖 Task 4(Agent Loop 与日志接口),可并行
- Task 7 依赖 Task 5 与 Task 6
