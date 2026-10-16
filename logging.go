package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

var (
	agentLogPath = "agent.json"
	inputLogPath = "input.json"
)

func resetLogs() error {
	for _, path := range []string{agentLogPath, inputLogPath} {
		if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func logInput(turn int, messages []anthropic.MessageParam) {
	entry := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"turn":  turn,
		"input": messages,
	}
	if err := appendLog(inputLogPath, entry); err != nil {
		fmt.Fprintf(os.Stderr, "warning: log input: %v\n", err)
	}
}

func logResponse(turn int, resp *anthropic.Message) {
	entry := map[string]any{
		"ts":       time.Now().UTC().Format(time.RFC3339Nano),
		"turn":     turn,
		"response": resp,
	}
	if err := appendLog(agentLogPath, entry); err != nil {
		fmt.Fprintf(os.Stderr, "warning: log response: %v\n", err)
	}
}

func appendLog(path string, entry any) error {
	var entries []json.RawMessage
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &entries)
	}

	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	entries = append(entries, raw)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(entries); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
