package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

var messagesLogPath = "messages.json"

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
