package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

var messagesLogPath = "messages.json"

// readlineHistoryPath readline 命令历史文件（↑↓ 翻页、跨进程保留）。
var readlineHistoryPath = ".coding-agent-history"

// appLogPath 运行诊断日志（append 模式，进程退出后保留，用于定位异常退出）。
var appLogPath = "coding-agent-go.log"

// appLog 默认丢弃输出：测试不调用 initLogger 也安全。每条记录直接写文件、
// 无缓冲，进程崩溃前写入的日志不会丢。
var appLog = slog.New(&bracketHandler{w: io.Discard, mu: &sync.Mutex{}, level: slog.LevelInfo})

// initLogger 以 append 模式打开日志文件并替换全局 logger；文件句柄跟随进程生命周期，
// 不主动 close。
func initLogger() error {
	f, err := os.OpenFile(appLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	appLog = slog.New(&bracketHandler{w: f, mu: &sync.Mutex{}, level: slog.LevelInfo})
	return nil
}

// bracketHandler 以 "[时间戳][级别] 消息 key=value" 格式输出日志，时间戳为本地时间。
type bracketHandler struct {
	w     io.Writer
	mu    *sync.Mutex
	level slog.Level
	attrs []slog.Attr
}

func (h *bracketHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

func (h *bracketHandler) Handle(_ context.Context, r slog.Record) error {
	var buf bytes.Buffer
	buf.WriteByte('[')
	buf.WriteString(r.Time.Format("2006-01-02 15:04:05.000"))
	buf.WriteString("][")
	buf.WriteString(levelName(r.Level))
	buf.WriteString("] ")
	buf.WriteString(r.Message)
	writeAttr := func(a slog.Attr) {
		buf.WriteByte(' ')
		buf.WriteString(a.Key)
		buf.WriteByte('=')
		buf.WriteString(a.Value.String())
	}
	for _, a := range h.attrs {
		writeAttr(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(a)
		return true
	})
	buf.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(buf.Bytes())
	return err
}

func (h *bracketHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &nh
}

func (h *bracketHandler) WithGroup(string) slog.Handler { return h }

// levelName 输出首字母大写的级别名：Info / Warn / Error / Debug。
func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "Error"
	case l >= slog.LevelWarn:
		return "Warn"
	case l >= slog.LevelInfo:
		return "Info"
	default:
		return "Debug"
	}
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

	// SDK 的 tool_use input 是 json.RawMessage（API 响应原文，服务端含 &<>
	// 的 \uXXXX 转义），直接 Encode 会原样透传。先序列化再反序列化为 any 做
	// 归一化，之后重新编码即为可读原文。
	raw, err := json.Marshal(entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: log messages: %v\n", err)
		return
	}
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		fmt.Fprintf(os.Stderr, "warning: log messages: %v\n", err)
		return
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(normalized); err != nil {
		fmt.Fprintf(os.Stderr, "warning: log messages: %v\n", err)
		return
	}
	if err := os.WriteFile(messagesLogPath, buf.Bytes(), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: log messages: %v\n", err)
	}
}
