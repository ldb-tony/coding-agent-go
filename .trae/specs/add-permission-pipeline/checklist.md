# Checklist

- [x] `permission.go` 实现三闸门管线:Gate1 拒绝清单 / Gate2 规则表 / Gate3 用户确认 / `checkPermission` 组合,顺序为 硬拒 → 规则 → 询问 → 直通
- [x] Gate1 拒绝清单为并集:`rm -rf /`、`sudo`、`shutdown`、`reboot`、`mkfs`、`dd if=`、`> /dev/`,仅作用于 bash
- [x] Gate1 命中时终端打印 `⛔ Blocked: '<pattern>' is on the deny list`,命令不执行
- [x] Gate2 正则无 lookahead 改写正确:`rm test.txt`、`del test.txt`、`DEL test.txt`、`ls && rm -rf build` 触发;`model`、`delimiter`、`echo del test.txt` 不误触发
- [x] Gate2 关键词 `rm `、`> /etc/`、`chmod 777` 触发询问,提示消息为 `Potentially destructive command`
- [x] Gate3 交互格式:打印 `⚠ 原因` 与 `Tool: name(args)`,提示 `Allow? [y/N]`;`y`/`yes`(大小写不敏感)放行,其余输入(含空行)默认拒绝
- [x] Gate3 输入源为可替换包级变量,单元测试可注入
- [x] agent loop 中每个 tool_use 执行前经过 `checkPermission`;拒绝时回喂 `tool_result`(content=`Permission denied.`,`is_error=true`),同轮其余调用继续执行,loop 不中断
- [x] 三个门均未命中的常规调用(如 `echo hello`、`read_file`)直通执行,无告警无询问
- [x] `bash.go` 内置 `dangerousCommandSubstrings` 拦截已移除,`runBash` 不再自带拦截
- [x] `bash_test.go` 危险命令子测试已迁移至 `permission_test.go`,其余 bash 测试(输出/超时/截断/stderr 合并)保持通过
- [x] 文件工具路径逃逸仍由 `safePath` 硬拒绝(工作区外写入返回 access denied,与项目硬约束一致)
- [x] `main.go` banner 为 `s03: Permission`,提示符为 `s03 >> `
- [x] `go build ./...`、`go vet ./...`、`gofmt -l .` 无告警,`go test ./...` 全部通过
- [x] 手动 e2e:创建当前目录文件直通;`rm` 删除触发询问且 y/n 行为正确;写 `/etc/` 下文件被 safePath 硬拒
