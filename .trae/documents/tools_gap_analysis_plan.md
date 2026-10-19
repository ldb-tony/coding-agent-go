# 逐工具深度对比报告 — 实施计划（修订版）

## 用户需求（修订后）

不做代码实施。为当前工程的**每一个工具**单独生成一份与 Claude Code 对应工具的深度对比报告（每个工具一个 md 文件），要求：基于每个 tool 专门讨论，深度显著高于上一版总览文档；暂不进入实施阶段。

## 调研基础（已完成，可直接落报告）

已掌握 Claude Code v2.1.x 官方文档 + v0.2.x 源码级资料（claude-harness.dev 三篇源码解构）：

- **Read**：schema（file_path/offset/limit/pages）；双层限额（maxSizeBytes=256KB 读前抛错、maxTokens=25000 读后抛错）；默认 2000 行；官方实验结论"抛错分页比截断省 token（#21841）"；图片 Sharp 压缩 Base64、PDF≤20 页；mtime unchanged stub（~30 token）；/dev 设备路径黑名单
- **Write**：read-before-write 门控（errorCode 2 未读/部分读、3 读后被改；mtime + 全读内容比对兜底）；同步临界区原子写；写后 LSP didChange/didSave 通知
- **Edit**：old_string 唯一性强制（多处 FAIL 引导加上下文或 replace_all）；只传增量的 token 经济性；curly quote 规范化；描述中精确空白约束
- **Grep**：ripgrep 薄封装 13 参数；三输出模式（默认 files_with_matches 按 mtime 降序）；默认 250 条 + offset 分页 + limit=0 逃生舱；--hidden、排除 6 种 VCS 目录、--max-columns 500；gitignore 与权限 deny 规则联动
- **Glob**：pattern+path；结构化输出（durationMs/numFiles/filenames/truncated）；100 文件上限；cwd 相对路径省 token；isConcurrencySafe/isReadOnly
- **Bash**：schema（command/description/timeout≤600000/run_in_background）；命令分类器（search/read/list/neutral/silent 集合，管道全段判定）；退出码语义表（grep/rg 退出 1=无匹配不是错，diff/test 同理）；23 项 bashSecurity 检查（ID 编号，含 IFS 注入/Unicode 空白/命令替换/zsh 危险内建）；信息性破坏命令警告；可选 OS 沙箱；默认 120s/最大 600s、阻塞自动转后台（15s budget）；输出 30000 字符（BASH_MAX_OUTPUT_LENGTH，最大 150000）超限落盘；cwd 持久
- **我方实现**：已逐行核对 [tools.go](file:///Users/tony/coding-agent-go/tools.go)、[bash.go](file:///Users/tony/coding-agent-go/bash.go) 全部行为与常量

## 交付物（7 个新文件，不改任何代码）

目录：`docs/tool-comparison/`

| 文件 | 对比对象 |
|---|---|
| `README.md` | 索引 + 方法论 + 跨工具总差距矩阵 + token 经济学/确定性/安全三层共性结论 + 路线图摘要（从总览文档迁入） |
| `01-read_file-vs-Read.md` | read_file ↔ Claude Code Read |
| `02-write_file-vs-Write.md` | write_file ↔ Write |
| `03-edit_file-vs-Edit.md` | edit_file ↔ Edit（含 MultiEdit） |
| `04-grep-vs-Grep.md` | grep ↔ Grep（含与 ripgrep 的关系） |
| `05-glob-vs-Glob.md` | glob ↔ Glob |
| `06-bash-vs-Bash.md` | bash ↔ Bash |

## 每份工具报告的固定结构（保证深度一致）

1. **工具定位与设计哲学** — CC 的设计意图 vs 我们沿用教程语义的定位
2. **功能描述（description）对比** — CC 描述原文摘录（英文原文+中文解读）vs 我们的一句话描述；分析描述如何影响模型调用行为
3. **API/Schema 逐项对比表** — 参数名、类型、必填、默认值、语义；CC schema 原文片段；我们的 JSON schema 与 handler struct；差异逐条点评
4. **执行机制对比（源码级）** — CC 的处理流程（带源文件路径/函数名证据）vs 我们的实现（引用具体代码行链接）；时序或状态流差异
5. **输出契约与边界行为** — 正常/空结果/超限/错误路径；错误文案是否可恢复；截断语义；退出码/错误码
6. **状态、安全与权限** — harness 状态依赖、read-before 门控、路径规则、sandbox/分类器/gitignore；我们的现状
7. **效果对比（量化场景推演）** — 至少 2 个具体场景，给出 token 量、loop 往返次数、错误风险的推演数字（如：读 5000 行文件、grep node_modules 仓库、歧义 edit、cd 不持久）
8. **差距清单评级表** — 维度 / CC 行为 / 我们的行为 / 对效果的影响 / P0-P2 优先级
9. **对齐设计草案**（仅设计，不实施）— Go 侧建议的 schema 变更、函数签名/struct 草案、新增状态依赖、需要新增/修改的测试用例清单、破坏性变更提示
10. **参考资料** — 官方文档与源码解构链接

## 编写标准

- 中文撰写；所有 CC 行为必须有来源（官方文档或源码文章），不杜撰参数与数值
- 我方行为引用到具体代码行（clickable 绝对路径链接）
- 量化推演标注"估算"及其假设，不伪装成实测
- 报告间通过 README 索引互链；跨工具共性内容只放 README，单报告用链接引用避免重复

## 不做的事

- 不修改任何 .go 生产代码与测试
- 不做真实 API 联调
- 旧的 `.trae/documents/tools_gap_analysis_plan.md` 保留为计划过程文档（报告落盘后可在 README 中说明它已被系列报告取代）

## 验证方式

- 6 份报告覆盖 6 个工具，每份包含固定 10 节
- 事实抽查：参数名/默认值/限额数字与来源一致；我方代码引用行号准确（交付前重新核对 tools.go/bash.go）
- 链接有效性自检（本地文件链接 + 外部 URL）

## 风险

- 资料版本差异（源码解构基于 v0.2.x，官方参考为 v2.1.x）：凡两者可能漂移的数值（如 15s 自动后台阈值、100/250 上限）同时标注两个来源与版本
- 报告较长：用固定目录与小标题保证可跳读；每报告控制在单一主题内
