//go:build windows

// s11: Windows 没有 POSIX 进程组语义，走默认行为（Context 取消时只杀直接子进程）。
package main

import (
	"os/exec"
	"syscall"
)

// sysProcAttr 在 Windows 上无进程组可用，返回 nil 走默认启动属性。
func sysProcAttr() *syscall.SysProcAttr {
	return nil
}

// stopProcessGroup 在 Windows 上是 no-op。
func stopProcessGroup(*exec.Cmd) {}
