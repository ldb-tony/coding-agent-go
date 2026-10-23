// 后台任务——慢操作放进后台 goroutine，Agent Loop 继续跑，后续轮次收集完成结果。
//
// 执行模型与同步 bash 的区别：
//
//	同步：当前 tool_call 阻塞直到命令返回
//	后台：start() 注册任务、启动 goroutine（对齐参考实现的 daemon thread）并立即
//	      返回 bg_id 占位 tool_result；命令结束后任务进入完成队列，loop 在下一轮
//	      开头用 collect() 把结果以 <task_notification> 独立事件注入对话。
//
// 通知不复用原 tool_use_id：原 tool_call 已经用占位 tool_result 应答过了，一次
// tool_use 仍然恰好对应一次 tool_result。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// backgroundTask 是单个后台任务的状态。status: running / completed / failed。
type backgroundTask struct {
	id      string
	command string
	status  string
	detail  string // 通知里 status 后的补充说明，如 " (exit 1)" / " (timeout)"
	result  string
	cmd     *exec.Cmd          // running 期间非 nil，stopAll 用它清理进程组
	cancel  context.CancelFunc // 尚未 Start 时 stopAll 的兜底：取消 ctx 让 Run 立即失败
}

// backgroundManager 持有任务状态与完成队列，所有方法并发安全。
type backgroundManager struct {
	mu      sync.Mutex
	next    int           // bg_id 自增计数
	timeout time.Duration // 单条命令超时，与同步 bash 相同的默认值
	tasks   map[string]*backgroundTask
	ready   []string // 已完成待收集的任务 id，FIFO
}

// backgroundTasks 是进程级管理器：REPL 每轮用户输入都新建 agentLoop/sessionState，
// 而后台任务的生命周期跨越多轮对话，因此挂在包级（对齐参考实现的 BACKGROUND 全局）。
var backgroundTasks = newBackgroundManager()

func newBackgroundManager() *backgroundManager {
	return &backgroundManager{
		timeout: defaultBashTimeout,
		tasks:   map[string]*backgroundTask{},
	}
}

// start 注册任务并立即启动后台 goroutine，返回 bg_id。非零退出码或 worker 异常
// 会使任务进入 failed；命令在独立进程组里执行，结束/超时后整组清理。
// cmd 在持锁阶段同步登记到任务上，保证 stopAll 从 start 返回起的任何时刻
// 都能看到运行中的进程组，不会错过清理窗口。
func (m *backgroundManager) start(command string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	id := fmt.Sprintf("bg_%04d", m.next)
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	cmd := shellCommand(ctx, command)
	cmd.SysProcAttr = sysProcAttr()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	t := &backgroundTask{id: id, command: command, status: "running", cmd: cmd, cancel: cancel}
	m.tasks[id] = t
	appLog.Info("background task started", "id", id, "command", command)
	go m.run(t, ctx, &stdout, &stderr)
	return id
}

// run 在后台 goroutine 里执行命令并落盘结果，完成后追加进 ready 队列。
func (m *backgroundManager) run(t *backgroundTask, ctx context.Context, stdout, stderr *bytes.Buffer) {
	defer t.cancel()
	err := t.cmd.Run()
	stopProcessGroup(t.cmd) // 生命周期清理：结束时停掉原始进程组里的残留子进程

	status, detail, result := "completed", "", combineBashOutput(*stdout, *stderr)
	if ctx.Err() == context.DeadlineExceeded {
		status, detail = "failed", " (timeout)"
		result = fmt.Sprintf("Error: Timeout (%ds)", int(m.timeout.Seconds()))
	} else if err != nil {
		status = "failed"
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			detail = fmt.Sprintf(" (exit %d)", ee.ExitCode())
		} else {
			detail = " (error: " + err.Error() + ")"
		}
	}

	m.mu.Lock()
	t.status, t.detail, t.result = status, detail, result
	t.cmd = nil
	m.ready = append(m.ready, t.id)
	m.mu.Unlock()

	if status == "completed" {
		appLog.Info("background task finished", "id", t.id, "status", status)
	} else {
		appLog.Warn("background task failed", "id", t.id, "status", status, "detail", detail)
	}
}

// combineBashOutput 合并 stdout/stderr 并按同步 bash 的规则截断。
func combineBashOutput(stdout, stderr bytes.Buffer) string {
	output := strings.TrimSpace(stdout.String() + stderr.String())
	if runes := []rune(output); len(runes) > maxBashOutputRunes {
		output = string(runes[:maxBashOutputRunes])
	}
	return output
}

// collect 弹出完成队列里的全部结果并格式化为 <task_notification> 通知（FIFO）。
func (m *backgroundManager) collect() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.ready) == 0 {
		return nil
	}
	var notes []string
	for _, id := range m.ready {
		t := m.tasks[id]
		body := t.result
		if body == "" {
			body = "(no output)"
		}
		notes = append(notes, fmt.Sprintf(
			"<task_notification>\nBackground task %s (%s) %s%s:\n%s\n</task_notification>",
			t.id, t.command, t.status, t.detail, body))
	}
	m.ready = nil
	return notes
}

// taskStatus 返回任务当前状态；id 不存在时返回 ""。供测试与调试使用。
func (m *backgroundManager) taskStatus(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tasks[id]; ok {
		return t.status
	}
	return ""
}

// stopAll 清理仍在运行的任务：进程已启动则向其进程组发 SIGTERM；尚未 Start
// （Process 还是 nil）则取消 ctx，让随后的 Run 立即以失败落盘。
// 在 Agent 退出路径上调用（正常退出与 SIGINT/SIGTERM 均覆盖）。
func (m *backgroundManager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	stopped := 0
	for _, t := range m.tasks {
		if t.status != "running" {
			continue
		}
		stopped++
		if t.cmd != nil && t.cmd.Process != nil {
			stopProcessGroup(t.cmd)
		} else if t.cancel != nil {
			t.cancel()
		}
	}
	if stopped > 0 {
		appLog.Info("background tasks stopped", "count", stopped)
	}
}

// shouldRunBackground 判断本次工具调用是否走后台：仅当 bash 调用显式把
// run_in_background 设为 true（Harness 不再从 install/build/test 等关键词猜测）。
func shouldRunBackground(toolName string, input json.RawMessage) bool {
	if toolName != "bash" {
		return false
	}
	var in struct {
		RunInBackground bool `json:"run_in_background"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return false
	}
	return in.RunInBackground
}

// startBackgroundTask 解析 bash 参数并启动后台任务，返回给模型的占位结果带 bg_id。
func startBackgroundTask(input json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &in)
	id := backgroundTasks.start(in.Command)
	return fmt.Sprintf("[Background task %s started] The result will be delivered as a "+
		"<task_notification> in a later turn. Continue with other work in the meantime.", id)
}

// injectBackgroundResults 在每轮模型调用前收集已完成的后台任务，把通知作为独立的
// user 事件追加进对话。没有就绪结果时原样返回，不产生额外消息。
func injectBackgroundResults(messages []anthropic.MessageParam) []anthropic.MessageParam {
	notes := backgroundTasks.collect()
	if len(notes) == 0 {
		return messages
	}
	return append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(strings.Join(notes, "\n\n"))))
}
