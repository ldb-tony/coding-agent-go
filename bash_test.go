package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRunBash(t *testing.T) {
	t.Run("normal command output", func(t *testing.T) {
		if got := runBash("echo hello"); got != "hello" {
			t.Fatalf("runBash(echo hello) = %q, want %q", got, "hello")
		}
	})

	t.Run("empty output", func(t *testing.T) {
		if got := runBash("true"); got != "(no output)" {
			t.Fatalf("runBash(true) = %q, want %q", got, "(no output)")
		}
	})

	t.Run("stderr is merged into output", func(t *testing.T) {
		got := runBash("echo out; echo err >&2")
		if got != "out\nerr" {
			t.Fatalf("got %q, want %q", got, "out\nerr")
		}
	})

	t.Run("long output is truncated to 50000 runes", func(t *testing.T) {
		got := runBash("seq 1 20000")
		if strings.HasPrefix(got, "Error:") {
			t.Fatalf("unexpected error: %q", got)
		}
		if n := utf8.RuneCountInString(got); n != maxBashOutputRunes {
			t.Fatalf("output length = %d runes, want %d", n, maxBashOutputRunes)
		}
		if !strings.HasPrefix(got, "1\n2\n3\n") {
			t.Fatalf("output does not start with expected sequence: %.20q", got)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		got := runBashWithTimeout("sleep 3", time.Second)
		if got != "Error: Timeout (1s)" {
			t.Fatalf("got %q, want %q", got, "Error: Timeout (1s)")
		}
	})
}
