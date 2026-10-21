package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

var messagesLogPath = "messages.json"

// appLogPath 运行诊断日志（append 模式，进程退出后保留，用于定位异常退出）。
var appLogPath = "coding-agent-go.log"

// appLog 默认丢弃输出：测试不调用 initLogger 也安全。slog 每条记录直接写文件、
// 无缓冲，进程崩溃前写入的日志不会丢。
var appLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// initLogger 以 append 模式打开日志文件并替换全局 logger；文件句柄跟随进程生命周期，
// 不主动 close。
func initLogger() error {
	f, err := os.OpenFile(appLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	appLog = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return nil
}

func resetLogs() error {
	// 快照日志：每次调用覆盖写入，启动时置空对象即可。
	return os.WriteFile(messagesLogPath, []byte("{}"), 0o644)
}

// logMessages 覆盖写入当前完整会话快照：用户输入、模型回答、tool_use/tool_result。
// react 模式下 messages 本身已包含全部历史，覆盖而非追加可避免历史重复。
func logMessages(turn int, messages []anthropic.MessageParam) {
	entry := map[string]any{
		"ts":       time.Now().UTC().Format(time.RFC3339Nano),
		"turn":     turn,
		"messages": messages,
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(entry); err != nil {
		fmt.Fprintf(os.Stderr, "warning: log messages: %v\n", err)
		return
	}
	if err := os.WriteFile(messagesLogPath, buf.Bytes(), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: log messages: %v\n", err)
	}
}
