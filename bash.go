package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	defaultBashTimeout = 120 * time.Second
	maxBashOutputRunes = 50000
)

var dangerousCommandSubstrings = []string{
	"rm -rf /",
	"sudo",
	"shutdown",
	"reboot",
	"> /dev/",
}

func runBash(command string) string {
	return runBashWithTimeout(command, defaultBashTimeout)
}

func runBashWithTimeout(command string, timeout time.Duration) string {
	for _, bad := range dangerousCommandSubstrings {
		if strings.Contains(command, bad) {
			return "Error: Dangerous command blocked"
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("Error: Timeout (%ds)", int(timeout.Seconds()))
	}
	if err != nil {
		return "Error: " + err.Error()
	}

	output := strings.TrimSpace(stdout.String() + stderr.String())
	if output == "" {
		return "(no output)"
	}
	if runes := []rune(output); len(runes) > maxBashOutputRunes {
		output = string(runes[:maxBashOutputRunes])
	}
	return output
}
