package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// extractOverflowPath 从 bash 截断输出中提取落盘的文件路径。
// 输出格式：Full output saved to "/tmp/agent-bash-output/bash-XXX.log" — read it with read_file.
func extractOverflowPath(output string) string {
	const marker = "Full output saved to "
	idx := strings.Index(output, marker)
	if idx < 0 {
		return ""
	}
	rest := output[idx+len(marker):]
	// 取引号里的路径
	if quoteIdx := strings.Index(rest, "\""); quoteIdx >= 0 {
		rest = rest[quoteIdx+1:]
		if endIdx := strings.Index(rest, "\""); endIdx >= 0 {
			return rest[:endIdx]
		}
	}
	return ""
}

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

	t.Run("long output overflows to file", func(t *testing.T) {
		got := runBash("seq 1 20000")
		if strings.HasPrefix(got, "Error:") {
			t.Fatalf("unexpected error: %q", got)
		}
		// 应以截断内容开头
		if !strings.HasPrefix(got, "1\n2\n3\n") {
			t.Fatalf("output does not start with expected sequence: %.20q", got)
		}
		// 应包含截断提示
		if !strings.Contains(got, "output truncated") {
			t.Fatalf("missing truncation hint in: %.100q", got)
		}
		// 应包含落盘路径
		if !strings.Contains(got, "Full output saved to") {
			t.Fatalf("missing overflow path in result")
		}
		// 验证落盘文件确实存在且内容完整
		// 结果格式：... Full output saved to "/tmp/agent-bash-output/bash-XXX.log" ...
		path := extractOverflowPath(got)
		if path == "" {
			t.Fatalf("could not extract overflow path from: %.200q", got)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read overflow file %s: %v", path, err)
		}
		if len(data) == 0 {
			t.Fatalf("overflow file is empty")
		}
		// 清理
		os.Remove(path)
	})

	t.Run("output under threshold is not truncated", func(t *testing.T) {
		got := runBash("seq 1 100")
		if strings.Contains(got, "output truncated") {
			t.Fatalf("unexpected truncation for small output")
		}
		if strings.Contains(got, "Full output saved") {
			t.Fatalf("unexpected overflow path for small output")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		got := runBashWithTimeout("sleep 3", time.Second)
		if got != "Error: Timeout (1s)" {
			t.Fatalf("got %q, want %q", got, "Error: Timeout (1s)")
		}
	})

	// P0-1: 退出码语义化测试
	t.Run("grep exit 1 = No matches found (not an error)", func(t *testing.T) {
		got := runBash("grep 'xyz_nonexistent' bash.go")
		if got != "No matches found" {
			t.Fatalf("got %q, want %q", got, "No matches found")
		}
	})

	t.Run("grep exit 2 is still an error", func(t *testing.T) {
		got := runBash("grep")
		if !strings.HasPrefix(got, "Error:") {
			t.Fatalf("expected Error: prefix, got %q", got)
		}
	})

	t.Run("diff exit 1 = Files differ (not an error)", func(t *testing.T) {
		got := runBash("echo hello > /tmp/a_test_diff_bash && echo world > /tmp/b_test_diff_bash && diff /tmp/a_test_diff_bash /tmp/b_test_diff_bash")
		// diff 输出被 shell 合并进 stderr，但退出码语义化后应返回 "Files differ" 而非 Error
		// 清理文件（用 || true 防删除已完成的文件时报错）
		runBash("rm -f /tmp/a_test_diff_bash /tmp/b_test_diff_bash")
		if got != "Files differ" {
			t.Fatalf("got %q, want %q", got, "Files differ")
		}
	})

	t.Run("test exit 1 = Condition false (not an error)", func(t *testing.T) {
		got := runBash("test 1 -eq 2")
		if got != "Condition false" {
			t.Fatalf("got %q, want %q", got, "Condition false")
		}
	})

	t.Run("pipeline: cat | grep → 按末段 grep 判定", func(t *testing.T) {
		got := runBash("echo hello | grep world")
		if got != "No matches found" {
			t.Fatalf("got %q, want %q", got, "No matches found")
		}
	})

	t.Run("pipeline: grep with actual match returns the line", func(t *testing.T) {
		got := runBash("echo 'hello world' | grep hello")
		if strings.HasPrefix(got, "Error:") {
			t.Fatalf("unexpected error: %q", got)
		}
		if got != "hello world" {
			t.Fatalf("got %q, want %q", got, "hello world")
		}
	})

	t.Run("non-semantic exit code remains error", func(t *testing.T) {
		got := runBash("false")
		if !strings.HasPrefix(got, "Error: exit status 1") {
			t.Fatalf("got %q, want Error: exit status 1", got)
		}
	})

	// lastCommandName 单测
	t.Run("lastCommandName extracts from pipeline", func(t *testing.T) {
		cases := []struct {
			cmd  string
			want string
		}{
			{"grep foo bar.go", "grep"},
			{"cat a.txt | grep foo", "grep"},
			{"ls; diff a b", "diff"},
			{"FOO=bar grep x file", "grep"},
			{"timeout 5 grep x file", "grep"},
			{"echo hello", "echo"},
			{"", ""},
		}
		for _, c := range cases {
			if got := lastCommandName(c.cmd); got != c.want {
				t.Errorf("lastCommandName(%q) = %q, want %q", c.cmd, got, c.want)
			}
		}
	})
}

// P0-3: cwd 持久化测试 —— 用 runBashWithCwd 模拟 bashHandler 的会话语义
func TestRunBashWithCwd(t *testing.T) {
	// 准备子目录
	tmpDir := t.TempDir()
	subDir := tmpDir + "/sub"
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 在 sub 里创建一个标记文件
	if err := os.WriteFile(subDir+"/marker.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("cwd persists across calls", func(t *testing.T) {
		// 第一次调用：cd 进 sub 目录，记住新 cwd
		out1, cwd1 := runBashWithCwd("cd "+subDir, "")
		if out1 != "(no output)" {
			t.Fatalf("first call output = %q, want (no output)", out1)
		}
		if cwd1 != subDir {
			t.Fatalf("cwd after cd = %q, want %q", cwd1, subDir)
		}

		// 第二次调用：在已持久化的 cwd 下直接 pwd
		out2, cwd2 := runBashWithCwd("pwd", cwd1)
		if out2 != subDir {
			t.Fatalf("second call pwd output = %q, want %q", out2, subDir)
		}
		if cwd2 != subDir {
			t.Fatalf("cwd unchanged = %q, want %q", cwd2, subDir)
		}

		// 第三次调用：不需要绝对路径就能读 marker 文件
		out3, _ := runBashWithCwd("cat marker.txt", cwd1)
		if out3 != "hello" {
			t.Fatalf("read relative file = %q, want %q", out3, "hello")
		}
	})

	t.Run("empty cwd = process workdir", func(t *testing.T) {
		out, cwd := runBashWithCwd("pwd", "")
		if cwd == "" {
			t.Fatalf("cwd not captured")
		}
		if out != workdir {
			t.Fatalf("initial pwd = %q, want workdir %q", out, workdir)
		}
	})

	t.Run("command failure still syncs cwd", func(t *testing.T) {
		// 命令有 exit 1，但 marker 仍会执行（$? 捕获原始退出码）
		out, cwd := runBashWithCwd("cd "+subDir+" && false", "")
		if cwd != subDir {
			t.Fatalf("cwd after failed command = %q, want %q (should still sync)", cwd, subDir)
		}
		if !strings.HasPrefix(out, "Error:") {
			t.Fatalf("expected Error: prefix from false, got %q", out)
		}
	})

	t.Run("exit code semantics work through cwd wrapper (regression)", func(t *testing.T) {
		// Advisor 指出的回归：runBashWithCwd 包装后 lastCommandName 拿到 "exit" 而非 "grep"
		// 应在 wrapper 内部用原始命令名做语义化
		out, _ := runBashWithCwd("grep 'xyz_nonexistent' bash.go", "")
		if out != "No matches found" {
			t.Fatalf("grep no-match through cwd wrapper = %q, want %q (regression: exit code semantics broken)", out, "No matches found")
		}
	})
}
