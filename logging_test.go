package main

import (
	"bytes"
	"log/slog"
	"regexp"
	"sync"
	"testing"
)

func newBracketLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(&bracketHandler{w: buf, mu: &sync.Mutex{}, level: slog.LevelInfo})
}

func TestBracketHandlerFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := newBracketLogger(&buf)

	logger.Info("process start", "pid", 1234)

	want := regexp.MustCompile(`^\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3}\]\[Info\] process start pid=1234\n$`)
	if got := buf.String(); !want.MatchString(got) {
		t.Fatalf("unexpected log format: %q", got)
	}
}

func TestBracketHandlerLevels(t *testing.T) {
	var buf bytes.Buffer
	logger := newBracketLogger(&buf)

	logger.Warn("auto compact", "chars", 100)
	logger.Error("llm send failed", "err", "boom")
	logger.Debug("should be dropped") // 低于 Info，被过滤

	out := buf.String()
	if !regexp.MustCompile(`\]\[Warn\] auto compact chars=100\n`).MatchString(out) {
		t.Fatalf("warn line missing or malformed: %q", out)
	}
	if !regexp.MustCompile(`\]\[Error\] llm send failed err=boom\n`).MatchString(out) {
		t.Fatalf("error line missing or malformed: %q", out)
	}
	if bytes.Contains(buf.Bytes(), []byte("should be dropped")) {
		t.Fatalf("debug record should be filtered: %q", out)
	}
}
