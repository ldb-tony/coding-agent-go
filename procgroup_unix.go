//go:build unix

// 后台任务的 shell 在自己的进程组里启动，结束时向原始进程组整组发 SIGTERM。
// 这是生命周期清理而不是沙箱：另建会话的进程可以脱离该组。
package main

import (
	"os/exec"
	"syscall"
)

// sysProcAttr 让 shell 进程成为新进程组的组长。
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// stopProcessGroup 向命令所在进程组发送 SIGTERM，用于任务结束/超时/Agent 退出时
// 清理残留的子进程；组不存在（已全部退出）时忽略 ESRCH。
func stopProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}
