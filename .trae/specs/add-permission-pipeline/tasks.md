# Tasks

- [x] Task 1: 新增 `permission.go`,实现三闸门权限管线
  - [x] SubTask 1.1: 定义拒绝清单(现有 `> /dev/` 等 5 项 ∪ README 新增 `mkfs`、`dd if=`)与 `checkDenyList(command) (pattern string, ok bool)`,仅对 bash 生效
  - [x] SubTask 1.2: 实现 `containsDestructiveCommand(command) bool`:无 lookahead 改写正则 `(?i)(?:^|[;&|()\n])\s*(?:rm|del)(?:\s|$|[;&|()])`,外加子串 `rm `、`> /etc/`、`chmod 777` 关键词检查
  - [x] SubTask 1.3: 定义规则表 `permissionRules`(结构:tools/check/message,先仅 bash 一条)与 `checkRules(toolName, args) (reason string, matched bool)`
  - [x] SubTask 1.4: 实现 `askUser(toolName, args, reason) bool`:打印 `⚠ 原因`、`Tool: name(args)`、`Allow? [y/N]`,读取一行;`y`/`yes`(大小写不敏感)放行,其余拒绝;输入读取抽为可替换包级变量(如 `readApproval`)便于测试注入
  - [x] SubTask 1.5: 实现 `checkPermission(toolName string, args json.RawMessage) bool`,按 Gate1 → Gate2+3 → 直通 的顺序组合三道门;Gate1 命中时打印 `⛔ Blocked: '<pattern>' is on the deny list`
- [x] Task 2: 重构 `bash.go` 移除内置危险命令拦截,并迁移对应测试
  - [x] SubTask 2.1: 删除 `dangerousCommandSubstrings` 及 `runBashWithTimeout` 内的拦截分支
  - [x] SubTask 2.2: 删除 `bash_test.go` 中 "dangerous commands are blocked" 子测试(语义由 permission 层测试接管)
- [x] Task 3: `agent.go` 集成权限门
  - [x] SubTask 3.1: 在终端回显之后、`runTool` 之前插入 `checkPermission(block.Name, block.Input)`;拒绝时终端打印 `Permission denied.`,回喂 `anthropic.NewToolResultBlock(block.ID, "Permission denied.", true)` 并 `continue`
  - [x] SubTask 3.2: 确认放行路径行为与现状完全一致(既有 agent 测试不受影响)
- [x] Task 4: `main.go` banner 与提示符更新为 `s03: Permission` / `s03 >> `
- [x] Task 5: 新增 `permission_test.go`,遵循 Arrange-Act-Assert 与既有测试风格
  - [x] SubTask 5.1: Gate1 测试:命中各拒绝模式(含 `rm -rf /tmp/x`、`echo hi > /dev/null`、`mkfs`、`dd if=`)返回匹配 pattern;正常命令(`echo hello`)不命中;非 bash 工具不经 Gate1
  - [x] SubTask 5.2: Gate2 测试:`rm test.txt`、`del test.txt`、`DEL test.txt`、`ls && rm -rf build`、`chmod 777 x` 触发;`model`、`delimiter`、`echo del test.txt`、`echo hello` 不触发
  - [x] SubTask 5.3: Gate3 测试:注入 `readApproval` 返回 `y`/`yes`/`Y`/`YES` 放行;`n`、空行、任意文本拒绝
  - [x] SubTask 5.4: `checkPermission` 组合测试:Gate1 短路(命中时不再询问)、Gate2 放行/拒绝、全不命中直通
  - [x] SubTask 5.5: loop 集成测试:mock 一轮返回 `echo one` + `rm -rf /` 两个 bash 调用,断言前者正常执行、后者 tool_result 为 `Permission denied.` 且 `is_error=true`;另验证 Gate2 询问路径(注入 approve/deny)
- [x] Task 6: 全量验证:`go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./...` 全绿;手动 e2e 跑通 README "Try It" 场景(创建文件直通、`rm` 触发询问、工作区外写入被 safePath 硬拒)

# Task Dependencies

- [Task 2] depends on [Task 1](拒绝清单定义在 permission.go,bash 移除内置拦截后由其接管)
- [Task 3] depends on [Task 1]
- [Task 5] depends on [Task 1, Task 2, Task 3]
- [Task 6] depends on [Task 1, Task 2, Task 3, Task 4, Task 5]
- [Task 4] 无依赖,可与 Task 1-3 并行
