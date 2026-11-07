// Package session 提供会话级状态管理。
// session.go 管理 workdir（工作目录）配置，state.go 定义会话状态容器 State。
package session

import (
	"log"
	"os"
	"sync"
)

var (
	workdirMu sync.RWMutex
	workdir   = MustWorkdir()
)

// Workdir 返回当前工作目录。
func Workdir() string {
	workdirMu.RLock()
	defer workdirMu.RUnlock()
	return workdir
}

// SetWorkdir 动态设置工作目录（测试/运行时用）。
func SetWorkdir(dir string) {
	workdirMu.Lock()
	defer workdirMu.Unlock()
	workdir = dir
}

// MustWorkdir 返回当前工作目录，失败则 log.Fatal 退出。
func MustWorkdir() string {
	wd, err := os.Getwd()
	if err != nil {
		log.Fatal("get working directory: ", err)
	}
	return wd
}
