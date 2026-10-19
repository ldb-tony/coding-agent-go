# s02 工具能力（read_file / write_file / edit_file / grep / glob）实施计划

## 仓库调研结论

对照用户提供的 s02 参考 README（Python 版：bash + read/write/edit/glob，dispatch map 模式）检查当前 Go 工程，发现：

**五个文件工具的代码在工作区中已经存在（未提交），且构建、vet、测试全部通过。**

- [tools.go](file:///Users/tony/coding-agent-go/tools.go)（untracked）已包含：
  - `safePath`：路径越界（`../` 逃逸、绝对路径逃逸）拒绝，对齐参考的 `safe_path`
  - `toolHandlers` 分发表：`bash` / `read_file` / `write_file` / `edit_file` / `glob` / `grep`
  - `runTool` 未知工具返回 `Error: unknown tool`
  - 五个工具的完整实现，语义与参考一致并有工程化增强：
    - `read_file`：limit 限制行数、UTF-8 校验、50K 字符截断
    - `write_file`：自动 `MkdirAll`，返回 `Wrote N bytes to ...`
    - `edit_file`：仅替换首次出现（`Replace(..., 1)`），未命中返回 `Error: text not found`
    - `glob`：自研 `**` 跨目录匹配（Go 标准库不支持 `**`）、排序去重、上限 200 条、跳过 `.git`
    - `grep`（参考 README 中没有，但用户明确要求）：正则搜索、可选 `path`（文件或目录）、`include` 文件名过滤、`file:line:content` 输出、跳过二进制/大于 5MB 的文件、上限 200 条
  - `allTools()`：六个工具的 JSON schema 定义
- [agent.go](file:///Users/tony/coding-agent-go/agent.go#L41)：循环唯一改动点已就位 —— `runTool(block.Name, block.Input)` 分发查找；多个 tool_use 按原始顺序逐个执行
- [llm.go](file:///Users/tony/coding-agent-go/llm.go#L49-L57)：`Send` 已透传 tools 参数
- [main.go](file:///Users/tony/coding-agent-go/main.go)：REPL 已更新为 `s02: Tool Use` 标识
- [tools_test.go](file:///Users/tony/coding-agent-go/tools_test.go)（untracked）：safePath / read / write / edit / glob / grep / 未知工具 / agent loop 分发 read_file 的完整测试
- 验证结果：`go build ./...` ✓、`go vet ./...` ✓、`go test ./...` 全部通过 ✓

**唯一缺口：** [README.md](file:///Users/tony/coding-agent-go/README.md) 仍是 s01 内容（标题 "s01: Agent Loop"、只描述 bash、项目结构未列 tools.go），与 s02 代码不符。

## 变更文件与模块

- `README.md`：从 s01 文档更新为 s02 文档（唯一需要修改的文件）
- 其余 `.go` 文件：不改动，仅做最终回归验证

## 实施步骤

1. **更新 README.md 为 s02 内容**，参照用户提供的参考 README 的结构：
   - 标题与简介改为 s02: Tool Use（Go 版，6 个工具：bash + 5 个文件/搜索工具）
   - 说明"加一个工具 = TOOLS 定义加一条 + toolHandlers 加一行映射，agent loop 不变"的 dispatch 模式（对应 Go 的 `allTools()` 与 `toolHandlers`）
   - 工具列表表格：read_file / write_file / edit_file / glob / grep 的参数与行为说明
   - 路径安全说明：`safePath` 限定 workdir，bash 不受限（为 s03 permission 埋伏笔）
   - 更新"项目结构"：补充 `tools.go`（工具定义、分发与文件工具实现）、`tools_test.go`
   - 更新试玩提示（对齐参考 README 的 4 条 prompt，适配本项目，如"找出所有 .go 文件"）
2. **最终验证**：`gofmt -l .`（无输出）、`go build ./...`、`go vet ./...`、`go test ./...`
3. 汇总结果。不做 git commit（除非用户另行要求）

## 依赖与注意事项

- 无新增第三方依赖：grep 用标准库 `regexp`，glob 的 `**` 为自研递归匹配，walk 用 `path/filepath`
- 不做真实 API 联调（需要 `MODEL_ID` / API Key 且产生费用）；以单测和构建验证为准
- 现有代码为未提交状态，本计划不涉及提交，只补齐文档缺口

## 验证标准

- `go test ./...` 全部通过
- `go vet ./...` 无告警，`gofmt -l .` 无输出
- README 描述与实际工具集（6 个）、实际文件结构一致

## 风险

- **工作区已有未提交实现，重复实现会覆盖可工作的代码**：处理方式为不重写 `.go` 文件，仅审计 + 补文档；如用户希望推倒重写，需在审批时明确
- 若用户期望的 grep 语义与当前实现不同（例如纯字面量而非正则）：审批时可指出，当前实现采用正则并在 schema 描述中已注明
