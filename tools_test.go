package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// setupWorkdir 把文件工具的 workdir 切到临时目录，返回该目录。
func setupWorkdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := workdir
	workdir = dir
	t.Cleanup(func() { workdir = old })
	return dir
}

func mustWrite(t *testing.T, dir, rel, content string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// intPtr 构造显式 limit 参数（nil 表示未提供，指针表示模型显式给值）。
func intPtr(i int) *int { return &i }

func TestSafePath(t *testing.T) {
	dir := setupWorkdir(t)

	t.Run("relative path resolves inside workdir", func(t *testing.T) {
		got, err := safePath("a/b.txt")
		if err != nil {
			t.Fatalf("safePath: %v", err)
		}
		if got != filepath.Join(dir, "a", "b.txt") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("absolute path inside workdir is allowed", func(t *testing.T) {
		got, err := safePath(filepath.Join(dir, "x.txt"))
		if err != nil {
			t.Fatalf("safePath: %v", err)
		}
		if got != filepath.Join(dir, "x.txt") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("traversal escaping workdir is denied", func(t *testing.T) {
		for _, p := range []string{"../secret.txt", "a/../../secret.txt", "/etc/passwd"} {
			if _, err := safePath(p); err == nil {
				t.Errorf("safePath(%q) expected error, got nil", p)
			}
		}
	})

	t.Run("empty path is denied", func(t *testing.T) {
		if _, err := safePath(""); err == nil {
			t.Error("safePath(\"\") expected error, got nil")
		}
	})
}

func TestRunRead(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "hello.txt", "line1\nline2\nline3\n")

	t.Run("reads whole file with line numbers", func(t *testing.T) {
		got := runRead(newSessionState(), "hello.txt", 0, nil)
		want := "1→line1\n2→line2\n3→line3"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("limit truncates lines with line numbers", func(t *testing.T) {
		got := runRead(newSessionState(), "hello.txt", 0, intPtr(2))
		want := "1→line1\n2→line2"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("empty file returns empty output", func(t *testing.T) {
		mustWrite(t, dir, "empty.txt", "")
		if got := runRead(newSessionState(), "empty.txt", 0, nil); got != "" {
			t.Fatalf("got %q, want empty string", got)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if got := runRead(newSessionState(), "nope.txt", 0, nil); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("binary file", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0xff, 0xfe, 0x00}, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := runRead(newSessionState(), "bin.dat", 0, nil); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("path escaping workdir", func(t *testing.T) {
		if got := runRead(newSessionState(), "../main.go", 0, nil); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q", got)
		}
	})
}

func TestRunWrite(t *testing.T) {
	dir := setupWorkdir(t)

	got := runWrite(newSessionState(), "out/test.txt", "hello")
	if got != "Wrote 5 bytes to out/test.txt" {
		t.Fatalf("got %q", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out", "test.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("read back: %q, err %v", data, err)
	}
}

func TestRunEdit(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "code.py", "print('a')\nprint('a')\n")

	// 破坏性变更：旧实现静默替换第一处，对齐 CC 后多处匹配默认失败。
	t.Run("two identical matches fail by default and file is unchanged", func(t *testing.T) {
		st := newSessionState()
		runRead(st, "code.py", 0, nil)
		got := runEdit(st, "code.py", "print('a')", "print('b')", false)
		if !strings.Contains(got, "not unique") || !strings.Contains(got, "2 matches") {
			t.Fatalf("got %q", got)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "code.py"))
		if string(data) != "print('a')\nprint('a')\n" {
			t.Fatalf("file was modified: %q", data)
		}
	})

	t.Run("unique match succeeds", func(t *testing.T) {
		st := newSessionState()
		mustWrite(t, dir, "unique.py", "print('a')\nprint('c')\n")
		runRead(st, "unique.py", 0, nil)
		if got := runEdit(st, "unique.py", "print('a')", "print('b')", false); got != "Edited unique.py" {
			t.Fatalf("got %q", got)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "unique.py"))
		if string(data) != "print('b')\nprint('c')\n" {
			t.Fatalf("file content: %q", data)
		}
	})

	t.Run("replace_all replaces every occurrence", func(t *testing.T) {
		st := newSessionState()
		runRead(st, "code.py", 0, nil)
		if got := runEdit(st, "code.py", "print('a')", "print('b')", true); got != "Edited code.py" {
			t.Fatalf("got %q", got)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "code.py"))
		if string(data) != "print('b')\nprint('b')\n" {
			t.Fatalf("file content: %q", data)
		}
	})

	t.Run("text not found suggests re-reading", func(t *testing.T) {
		st := newSessionState()
		runRead(st, "code.py", 0, nil)
		got := runEdit(st, "code.py", "no-such-text", "x", false)
		if !strings.Contains(got, "text not found in code.py") || !strings.Contains(got, "read it again") {
			t.Fatalf("got %q", got)
		}
	})
}

// TestRunEditGate 覆盖 read-before-edit 门控（与 Write 共用 checkWritable）：
// 未读拒绝、部分读拒绝、读后过期拒绝、编辑后状态刷新允许连续编辑。
func TestRunEditGate(t *testing.T) {
	dir := setupWorkdir(t)

	t.Run("editing unread file is refused and file unchanged", func(t *testing.T) {
		mustWrite(t, dir, "unread.txt", "original")
		got := runEdit(newSessionState(), "unread.txt", "original", "evil", false)
		if !strings.Contains(got, "has not been read yet") {
			t.Fatalf("got %q", got)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "unread.txt"))
		if string(data) != "original" {
			t.Fatalf("file was modified: %q", data)
		}
	})

	t.Run("partial read does not grant edit access", func(t *testing.T) {
		st := newSessionState()
		mustWrite(t, dir, "p.txt", "l1\nl2\nl3\n")
		got := runRead(st, "p.txt", 2, nil) // 只读第 2 行起，非全量
		if strings.HasPrefix(got, "Error:") {
			t.Fatalf("read: %q", got)
		}
		if got := runEdit(st, "p.txt", "l2", "x", false); !strings.Contains(got, "has not been read yet") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("stale read (file modified since) is refused", func(t *testing.T) {
		st := newSessionState()
		abs := mustWrite(t, dir, "stale.txt", "v1\n")
		runRead(st, "stale.txt", 0, nil)
		// 模拟外部改动：内容 + mtime 一起变
		mustWrite(t, dir, "stale.txt", "v1 changed\n")
		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(abs, future, future); err != nil {
			t.Fatal(err)
		}
		got := runEdit(st, "stale.txt", "v1", "v2", false)
		if !strings.Contains(got, "has been modified since read") {
			t.Fatalf("got %q", got)
		}
		data, _ := os.ReadFile(abs)
		if string(data) != "v1 changed\n" {
			t.Fatalf("file was modified: %q", data)
		}
	})

	t.Run("successful edit refreshes read state for consecutive edits", func(t *testing.T) {
		st := newSessionState()
		mustWrite(t, dir, "chain.txt", "a\nb\nc\n")
		runRead(st, "chain.txt", 0, nil)
		if got := runEdit(st, "chain.txt", "a", "A", false); strings.HasPrefix(got, "Error:") {
			t.Fatalf("first edit: %q", got)
		}
		// 不再 Read，直接第二次编辑也应成功（编辑后状态已刷新）
		if got := runEdit(st, "chain.txt", "c", "C", false); strings.HasPrefix(got, "Error:") {
			t.Fatalf("second edit: %q", got)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "chain.txt"))
		if string(data) != "A\nb\nC\n" {
			t.Fatalf("file content: %q", data)
		}
	})
}

func TestRunGlob(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "a.py", "")
	mustWrite(t, dir, "sub/b.py", "")
	mustWrite(t, dir, "sub/deep/c.py", "")
	mustWrite(t, dir, "sub/notes.txt", "")

	t.Run("star matches one level", func(t *testing.T) {
		if got := runGlob("*.py", ""); got != "a.py" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("doublestar matches recursively", func(t *testing.T) {
		got := strings.Split(runGlob("**/*.py", ""), "\n")
		want := []string{"a.py", "sub/b.py", "sub/deep/c.py"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("no matches", func(t *testing.T) {
		if got := runGlob("*.zzz", ""); got != "(no matches)" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("empty pattern", func(t *testing.T) {
		if got := runGlob("", ""); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q", got)
		}
	})
}

func TestRunGrep(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "a.txt", "foo\nbar foo\n")
	mustWrite(t, dir, "sub/b.log", "foo here\n")
	mustWrite(t, dir, "c.md", "nothing here\n")

	t.Run("regex search with file:line:format", func(t *testing.T) {
		got := strings.Split(runGrep(runGrepParams{Pattern: "foo", OutputMode: "content"}), "\n")
		want := []string{"a.txt:1:foo", "a.txt:2:bar foo", "sub/b.log:1:foo here"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("glob filters filenames", func(t *testing.T) {
		got := runGrep(runGrepParams{Pattern: "foo", Glob: "*.log", OutputMode: "content"})
		if got != "sub/b.log:1:foo here" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("path narrows search to a file", func(t *testing.T) {
		got := runGrep(runGrepParams{Pattern: "bar", Path: "a.txt", OutputMode: "content"})
		if got != "a.txt:2:bar foo" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("no matches", func(t *testing.T) {
		if got := runGrep(runGrepParams{Pattern: "zzz-not-there"}); got != "(no matches)" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("invalid regex", func(t *testing.T) {
		if got := runGrep(runGrepParams{Pattern: "[unclosed"}); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("files_with_matches default mode lists files", func(t *testing.T) {
		got := runGrep(runGrepParams{Pattern: "foo"}) // 默认 files_with_matches
		lines := strings.Split(got, "\n")
		want := []string{"a.txt", "sub/b.log"}
		if len(lines) != len(want) {
			t.Fatalf("got %v, want %v", lines, want)
		}
		for i := range want {
			if lines[i] != want[i] {
				t.Fatalf("got %v, want %v", lines, want)
			}
		}
	})

	t.Run("count mode shows file:hit_count", func(t *testing.T) {
		got := runGrep(runGrepParams{Pattern: "foo", OutputMode: "count"})
		if !strings.Contains(got, "a.txt:2") {
			t.Fatalf("expected a.txt:2 in count output, got %q", got)
		}
		if !strings.Contains(got, "sub/b.log:1") {
			t.Fatalf("expected sub/b.log:1 in count output, got %q", got)
		}
	})
}

func TestRunToolUnknown(t *testing.T) {
	if got := runTool(newSessionState(), "no_such_tool", json.RawMessage(`{}`)); !strings.HasPrefix(got, "Error:") {
		t.Fatalf("got %q", got)
	}
}

func TestAgentLoopDispatchesFileTool(t *testing.T) {
	setupLogPaths(t)
	dir := setupWorkdir(t)
	mustWrite(t, dir, "hello.txt", "hello from file")

	toolUseResp := mustMessage(t, `{"id":"msg_read","type":"message","role":"assistant","model":"mock","content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"hello.txt"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	client := &mockClient{responses: []*anthropic.Message{toolUseResp, textResponse(t)}}
	initial := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("read it")),
	}

	messages, err := agentLoop(context.Background(), client, initial, "test request")
	if err != nil {
		t.Fatalf("agentLoop: %v", err)
	}

	results := toolResultTexts(t, messages[2])
	if len(results) != 1 {
		t.Fatalf("expected 1 tool_result, got %d", len(results))
	}
	found := false
	for _, c := range results[0].Content {
		if c.OfText != nil && strings.Contains(c.OfText.Text, "hello from file") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected tool_result content to contain %q", "hello from file")
	}
}

// TestAllToolsRegistered 防止新增工具时漏注册 schema（allTools 与 toolHandlers 必须一一对应）。
func TestAllToolsRegistered(t *testing.T) {
	tools := allTools()
	want := []string{"bash", "read_file", "write_file", "edit_file", "glob", "grep", "todo_write", "load_skill", "task", "compact",
		"create_task", "update_task", "claim_task", "complete_task", "get_task", "list_tasks", "connect_mcp"}
	if len(tools) != len(want) {
		t.Fatalf("allTools returned %d tools, want %d", len(tools), len(want))
	}
	for i, name := range want {
		if tools[i].OfTool == nil {
			t.Fatalf("tools[%d] OfTool is nil", i)
		}
		if tools[i].OfTool.Name != name {
			t.Errorf("tools[%d] = %q, want %q", i, tools[i].OfTool.Name, name)
		}
		if _, ok := toolHandlers[name]; !ok {
			t.Errorf("tool %q defined in allTools but missing in toolHandlers", name)
		}
		// s14: 静态工具名禁止占用 mcp__ 前缀（会遮蔽 MCP fallback 分发）
		if strings.HasPrefix(name, "mcp__") {
			t.Errorf("static tool %q must not use reserved mcp__ prefix", name)
		}
	}
}

// TestToolHandlersDispatch 覆盖 handler 层的 JSON 参数解析：合法参数经 runTool 分发到具体实现，非法 JSON 返回 Error。
func TestToolHandlersDispatch(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "hello.txt", "hello")
	mustWrite(t, dir, "code.txt", "aaa")
	mustWrite(t, dir, "dup.txt", "xx\nxx\n")

	cases := []struct {
		name      string
		tool      string
		args      string
		wantSub   string
		wantError bool
	}{
		{name: "read_file valid", tool: "read_file", args: `{"path":"hello.txt"}`, wantSub: "1→hello"},
		{name: "read_file string number limit", tool: "read_file", args: `{"path":"hello.txt","limit":"1"}`, wantSub: "1→hello"},
		{name: "write_file valid", tool: "write_file", args: `{"path":"out.txt","content":"data"}`, wantSub: "Wrote 4 bytes to out.txt"},
		{name: "edit_file valid", tool: "edit_file", args: `{"path":"code.txt","old_text":"aaa","new_text":"bbb"}`, wantSub: "Edited code.txt"},
		{name: "edit_file replace_all", tool: "edit_file", args: `{"path":"dup.txt","old_text":"xx","new_text":"yy","replace_all":true}`, wantSub: "Edited dup.txt"},
		{name: "glob valid", tool: "glob", args: `{"pattern":"*.txt"}`, wantSub: "hello.txt"},
		{name: "grep valid", tool: "grep", args: `{"pattern":"hello","output_mode":"content"}`, wantSub: "hello.txt:1:hello"},
		{name: "read_file invalid json", tool: "read_file", args: `{bad`, wantError: true},
		{name: "write_file invalid json", tool: "write_file", args: `{bad`, wantError: true},
		{name: "edit_file invalid json", tool: "edit_file", args: `{bad`, wantError: true},
		{name: "glob invalid json", tool: "glob", args: `{bad`, wantError: true},
		{name: "grep invalid json", tool: "grep", args: `{bad`, wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newSessionState()
			// read-before-edit 门控：edit_file 有效用例需先在同一会话内全量读取目标文件
			if tc.tool == "edit_file" && !tc.wantError {
				runRead(st, "code.txt", 0, nil)
				runRead(st, "dup.txt", 0, nil)
			}
			got := runTool(st, tc.tool, json.RawMessage(tc.args))
			if tc.wantError {
				if !strings.HasPrefix(got, "Error:") {
					t.Fatalf("got %q, want Error: prefix", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("got %q, want substring %q", got, tc.wantSub)
			}
		})
	}

	// edit_file 落盘结果也要验证（handler 确实改了文件；replace_all 全部替换）
	data, err := os.ReadFile(filepath.Join(dir, "code.txt"))
	if err != nil || string(data) != "bbb" {
		t.Fatalf("after edit dispatch: %q, err %v", data, err)
	}
	data, err = os.ReadFile(filepath.Join(dir, "dup.txt"))
	if err != nil || string(data) != "yy\nyy\n" {
		t.Fatalf("after replace_all dispatch: %q, err %v", data, err)
	}
}

// TestRunWriteEdgeCases 覆盖覆盖写、路径逃逸拒绝。
func TestRunWriteEdgeCases(t *testing.T) {
	dir := setupWorkdir(t)

	t.Run("overwrite existing file", func(t *testing.T) {
		st := newSessionState()
		if got := runWrite(st, "f.txt", "first"); !strings.Contains(got, "Wrote 5 bytes") {
			t.Fatalf("first write: %q", got)
		}
		// 写后状态已刷新，允许连续写；这里再显式 Read 一次模拟真实流程
		runRead(st, "f.txt", 0, nil)
		if got := runWrite(st, "f.txt", "second"); !strings.Contains(got, "Wrote 6 bytes") {
			t.Fatalf("second write: %q", got)
		}
		data, err := os.ReadFile(filepath.Join(dir, "f.txt"))
		if err != nil || string(data) != "second" {
			t.Fatalf("read back: %q, err %v", data, err)
		}
	})

	t.Run("path escaping workdir is denied", func(t *testing.T) {
		if got := runWrite(newSessionState(), "../escape.txt", "x"); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q", got)
		}
		if _, err := os.Stat(filepath.Join(dir, "..", "escape.txt")); !os.IsNotExist(err) {
			t.Fatalf("file should not be created outside workdir")
		}
	})
}

// TestRunWriteGate 覆盖 read-before-write 门控与 LF 归一：
// 未读拒写、部分读拒写、读后过期拒写、写后状态刷新、CRLF 落盘为 LF。
func TestRunWriteGate(t *testing.T) {
	dir := setupWorkdir(t)

	t.Run("overwrite unread file is refused and file unchanged", func(t *testing.T) {
		st := newSessionState()
		mustWrite(t, dir, "unread.txt", "original")
		got := runWrite(st, "unread.txt", "evil")
		if !strings.Contains(got, "has not been read yet") {
			t.Fatalf("got %q", got)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "unread.txt"))
		if string(data) != "original" {
			t.Fatalf("file was modified: %q", data)
		}
	})

	t.Run("partial read does not grant write access", func(t *testing.T) {
		st := newSessionState()
		mustWrite(t, dir, "p.txt", "l1\nl2\nl3\n")
		got := runRead(st, "p.txt", 2, nil) // 只读第 2 行起，非全量
		if strings.HasPrefix(got, "Error:") {
			t.Fatalf("read: %q", got)
		}
		if got := runWrite(st, "p.txt", "x"); !strings.Contains(got, "has not been read yet") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("full read then write succeeds", func(t *testing.T) {
		st := newSessionState()
		mustWrite(t, dir, "ok.txt", "old\n")
		runRead(st, "ok.txt", 0, nil)
		if got := runWrite(st, "ok.txt", "new\n"); !strings.Contains(got, "Wrote 4 bytes") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("stale read (file modified since) is refused", func(t *testing.T) {
		st := newSessionState()
		abs := mustWrite(t, dir, "stale.txt", "v1\n")
		runRead(st, "stale.txt", 0, nil)
		// 模拟外部改动：内容 + mtime 一起变
		mustWrite(t, dir, "stale.txt", "v2\n")
		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(abs, future, future); err != nil {
			t.Fatal(err)
		}
		got := runWrite(st, "stale.txt", "v3\n")
		if !strings.Contains(got, "has been modified since read") {
			t.Fatalf("got %q", got)
		}
		data, _ := os.ReadFile(abs)
		if string(data) != "v2\n" {
			t.Fatalf("file was modified: %q", data)
		}
	})

	t.Run("successful write refreshes read state for consecutive writes", func(t *testing.T) {
		st := newSessionState()
		mustWrite(t, dir, "chain.txt", "a\n")
		runRead(st, "chain.txt", 0, nil)
		if got := runWrite(st, "chain.txt", "b\n"); strings.HasPrefix(got, "Error:") {
			t.Fatalf("first write: %q", got)
		}
		// 不再 Read，直接第二次写也应成功（写后状态已刷新）
		if got := runWrite(st, "chain.txt", "c\n"); strings.HasPrefix(got, "Error:") {
			t.Fatalf("second write: %q", got)
		}
	})

	t.Run("CRLF and lone CR are normalized to LF on disk", func(t *testing.T) {
		st := newSessionState()
		got := runWrite(st, "crlf.txt", "a\r\nb\rc\n")
		if strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q", got)
		}
		data, err := os.ReadFile(filepath.Join(dir, "crlf.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "a\nb\nc\n" {
			t.Fatalf("on-disk content: %q", data)
		}
		if !strings.Contains(got, "Wrote 6 bytes") {
			t.Fatalf("byte count should reflect normalized content: %q", got)
		}
	})

	t.Run("new file needs no prior read", func(t *testing.T) {
		st := newSessionState()
		if got := runWrite(st, "fresh/new.txt", "x"); !strings.Contains(got, "Wrote 1 bytes") {
			t.Fatalf("got %q", got)
		}
	})
}

// TestRunEditEdgeCases 覆盖空 old_text、old==new、文件不存在、路径逃逸、多行替换。
func TestRunEditEdgeCases(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "multi.txt", "line1\nline2\n")

	t.Run("empty old_text is rejected", func(t *testing.T) {
		if got := runEdit(newSessionState(), "multi.txt", "", "x", false); got != "Error: old_text must not be empty" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("old_text equal to new_text is rejected", func(t *testing.T) {
		if got := runEdit(newSessionState(), "multi.txt", "line1", "line1", false); got != "Error: new_text must be different from old_text" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if got := runEdit(newSessionState(), "nope.txt", "a", "b", false); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("path escaping workdir is denied", func(t *testing.T) {
		if got := runEdit(newSessionState(), "../main.go", "a", "b", false); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("multiline replacement", func(t *testing.T) {
		st := newSessionState()
		runRead(st, "multi.txt", 0, nil)
		got := runEdit(st, "multi.txt", "line1\nline2", "a\n\nb", false)
		if got != "Edited multi.txt" {
			t.Fatalf("got %q", got)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "multi.txt"))
		if string(data) != "a\n\nb\n" {
			t.Fatalf("file content: %q", data)
		}
	})
}

// TestRunReadOffsetLimit 覆盖 offset/limit 分页组合、越界与参数校验。
func TestRunReadOffsetLimit(t *testing.T) {
	dir := setupWorkdir(t)
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "line%d\n", i)
	}
	mustWrite(t, dir, "ten.txt", b.String())

	t.Run("middle page", func(t *testing.T) {
		got := runRead(newSessionState(), "ten.txt", 4, intPtr(2))
		want := " 4→line4\n 5→line5"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("limit clamps to end of file", func(t *testing.T) {
		got := runRead(newSessionState(), "ten.txt", 9, intPtr(10))
		want := " 9→line9\n10→line10"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("offset alone pages with default limit", func(t *testing.T) {
		got := runRead(newSessionState(), "ten.txt", 8, nil)
		want := " 8→line8\n 9→line9\n10→line10"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("explicit zero limit reads everything", func(t *testing.T) {
		got := runRead(newSessionState(), "ten.txt", 1, intPtr(0))
		rows := strings.Split(got, "\n")
		if len(rows) != 10 || rows[0] != " 1→line1" || rows[9] != "10→line10" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("offset beyond end returns short error with hint", func(t *testing.T) {
		got := runRead(newSessionState(), "ten.txt", 11, nil)
		if !strings.HasPrefix(got, "Error:") || !strings.Contains(got, "beyond end") || !strings.Contains(got, "10 lines") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("negative offset and limit are rejected", func(t *testing.T) {
		if got := runRead(newSessionState(), "ten.txt", -1, nil); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("negative offset got %q", got)
		}
		if got := runRead(newSessionState(), "ten.txt", 1, intPtr(-2)); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("negative limit got %q", got)
		}
	})
}

// TestRunReadLineNumbers 验证输出每行带行号、行号连续且按文件总行数对齐。
func TestRunReadLineNumbers(t *testing.T) {
	dir := setupWorkdir(t)
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "L%d\n", i)
	}
	mustWrite(t, dir, "numbered.txt", b.String())

	got := runRead(newSessionState(), "numbered.txt", 0, nil)
	rows := strings.Split(got, "\n")
	if len(rows) != 10 {
		t.Fatalf("got %d rows, want 10: %q", len(rows), got)
	}
	for i, row := range rows {
		want := fmt.Sprintf("%2d→L%d", i+1, i+1) // 10 行 → 宽度 2，等宽对齐
		if row != want {
			t.Fatalf("row %d = %q, want %q", i, row, want)
		}
	}

	// 翻到中段行号仍连续，且宽度按整个文件（而非切片）稳定
	page := runRead(newSessionState(), "numbered.txt", 9, intPtr(2))
	if page != " 9→L9\n10→L10" {
		t.Fatalf("got %q", page)
	}
}

// TestRunReadLargeFileGate 覆盖第一层预算：>256KB 且未显式 limit 时读前短错误，
// 显式分页可绕过字节门槛，逃生舱 limit=0 仍受输出字符预算约束。
func TestRunReadLargeFileGate(t *testing.T) {
	dir := setupWorkdir(t)
	var b strings.Builder
	for i := 0; i < 6000; i++ {
		b.WriteString("UNIQUEMARKER padding padding padding padding padding\n") // 53 bytes × 6000 ≈ 311KB
	}
	rel := "big.txt"
	abs := mustWrite(t, dir, rel, b.String())
	fi, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() <= maxReadFileBytes {
		t.Fatalf("setup: size %d should exceed %d", fi.Size(), maxReadFileBytes)
	}

	t.Run("no explicit limit returns short byte-gate error without content", func(t *testing.T) {
		got := runRead(newSessionState(), rel, 0, nil)
		if !strings.HasPrefix(got, "Error:") || !strings.Contains(got, "256KB") || !strings.Contains(got, "offset/limit") {
			t.Fatalf("got %q", got)
		}
		if strings.Contains(got, "UNIQUEMARKER") {
			t.Fatalf("byte-gate error must not include file content, got %q", got)
		}
		if n := len([]rune(got)); n > 300 {
			t.Fatalf("error should be short, got %d runes", n)
		}
		// 失败读取不写状态：再次读取仍然是短错误，而不是 unchanged stub
		if got := runRead(newSessionState(), rel, 0, nil); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("repeated gated read got %q", got)
		}
	})

	t.Run("explicit limit bypasses byte gate and pages content", func(t *testing.T) {
		got := runRead(newSessionState(), rel, 1, intPtr(100))
		rows := strings.Split(got, "\n")
		if len(rows) != 100 {
			t.Fatalf("got %d rows, want 100", len(rows))
		}
		body := "UNIQUEMARKER padding padding padding padding padding"
		if rows[0] != fmt.Sprintf("%4d→%s", 1, body) || rows[99] != fmt.Sprintf("%4d→%s", 100, body) {
			t.Fatalf("page head/tail mismatch:\n%s", got)
		}
	})

	t.Run("escape-hatch limit zero still hits output budget", func(t *testing.T) {
		got := runRead(newSessionState(), rel, 1, intPtr(0))
		if !strings.HasPrefix(got, "Error:") || !strings.Contains(got, "budget") {
			t.Fatalf("got %q", got)
		}
		if strings.Contains(got, "UNIQUEMARKER") {
			t.Fatalf("budget error must not include file content, got %q", got)
		}
	})

	// 协议闭环：短错误提示 "Read with offset/limit"，模型只带 offset（不带 limit）
	// 重试时必须能拿到内容，不能二次撞上字节门槛。
	// 夹具：70000 行 ×4 字节 = 280KB（>256KB）；默认 2000 行页 ≈ 20K runes（<25K 预算）。
	t.Run("offset-only retry after byte-gate error succeeds", func(t *testing.T) {
		var e strings.Builder
		for i := 0; i < 70000; i++ {
			e.WriteString("abc\n")
		}
		oref := "offsetonly.txt"
		oabs := mustWrite(t, dir, oref, e.String())
		if fi, err := os.Stat(oabs); err != nil || fi.Size() <= maxReadFileBytes {
			t.Fatalf("setup: size should exceed %d", maxReadFileBytes)
		}

		first := runRead(newSessionState(), oref, 0, nil)
		if !strings.HasPrefix(first, "Error:") || !strings.Contains(first, "offset/limit") {
			t.Fatalf("first read should hit byte gate, got %q", first)
		}

		// 严格按提示只发 offset：默认页大小，应成功返回中段内容
		mid := runRead(newSessionState(), oref, 50001, nil)
		rows := strings.Split(mid, "\n")
		if len(rows) != defaultReadLines {
			t.Fatalf("got %d rows, want %d", len(rows), defaultReadLines)
		}
		if rows[0] != "50001→abc" || rows[len(rows)-1] != "52000→abc" {
			t.Fatalf("page head/tail mismatch: %q ... %q", rows[0], rows[len(rows)-1])
		}

		// 字面重试 offset=1（仍无 limit）也必须放行
		head := runRead(newSessionState(), oref, 1, nil)
		if headRows := strings.Split(head, "\n"); len(headRows) != defaultReadLines || headRows[0] != "    1→abc" {
			t.Fatalf("offset=1 retry should return the first page, got %d rows", len(headRows))
		}
	})
}

// TestRunReadBudgetErrors 覆盖第二层预算（破坏性变更）：超 25000 rune 返回短错误，
// 不再静默截断；缩小分页可成功恢复。
func TestRunReadBudgetErrors(t *testing.T) {
	dir := setupWorkdir(t)
	long := strings.Repeat("a", maxReadOutputRunes+50)
	mustWrite(t, dir, "long.txt", long)

	got := runRead(newSessionState(), "long.txt", 0, nil)
	if !strings.HasPrefix(got, "Error:") || !strings.Contains(got, "budget") {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "truncated") {
		t.Fatalf("silent truncation marker must be gone, got %q", got)
	}
	if n := len([]rune(got)); n > 300 {
		t.Fatalf("error should be short, got %d runes", n)
	}
	if c := strings.Count(got, "a"); c > 300 {
		t.Fatalf("error must not carry file content, found %d 'a' runes", c)
	}

	// 100 行 ×1000 字符 ≈ 100K runes：默认页超预算 → 错误；小页读取成功（恢复路径）
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString(strings.Repeat("x", 1000) + "\n")
	}
	mustWrite(t, dir, "wide.txt", b.String())
	if got := runRead(newSessionState(), "wide.txt", 0, nil); !strings.HasPrefix(got, "Error:") {
		t.Fatalf("default page over budget: got %q", got)
	}
	small := runRead(newSessionState(), "wide.txt", 1, intPtr(10))
	rows := strings.Split(small, "\n")
	if len(rows) != 10 || !strings.HasPrefix(rows[0], "  1→xxx") || !strings.HasPrefix(rows[9], " 10→xxx") {
		t.Fatalf("small page should succeed, got %d rows", len(rows))
	}

	// 5000 行 ×49 字符 = 245KB（< 256KB，过字节门槛）：默认 2000 行页在中段超预算，
	// 错误必须给出 offset=2001 的续读指引
	var d strings.Builder
	for i := 0; i < 5000; i++ {
		d.WriteString(strings.Repeat("z", 49) + "\n")
	}
	mustWrite(t, dir, "deep.txt", d.String())
	mid := runRead(newSessionState(), "deep.txt", 0, nil)
	if !strings.HasPrefix(mid, "Error:") || !strings.Contains(mid, "offset=2001") {
		t.Fatalf("mid-file budget error should point to next page, got %q", mid)
	}
	if strings.Contains(mid, "zzz") {
		t.Fatalf("budget error must not carry file content, got %q", mid)
	}
	next := runRead(newSessionState(), "deep.txt", 2001, intPtr(100))
	if nextRows := strings.Split(next, "\n"); len(nextRows) != 100 {
		t.Fatalf("page at offset 2001 should return 100 rows, got %d", len(nextRows))
	}
}

// TestReadHandlerNumericArgs 在 handler 层验证数字参数容错：数字字符串正确解析，
// 非法字符串/小数报错而不是静默归零。
func TestReadHandlerNumericArgs(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "nums.txt", "line1\nline2\nline3\n")
	read := func(args string) string {
		return runTool(newSessionState(), "read_file", json.RawMessage(args))
	}

	t.Run("string digits parse as integer", func(t *testing.T) {
		if got := read(`{"path":"nums.txt","limit":"2"}`); got != "1→line1\n2→line2" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("string offset with numeric limit", func(t *testing.T) {
		if got := read(`{"path":"nums.txt","offset":"2","limit":1}`); got != "2→line2" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("whitespace around string digits is tolerated", func(t *testing.T) {
		if got := read(`{"path":"nums.txt","limit":" 2 "}`); got != "1→line1\n2→line2" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("plain integer still works", func(t *testing.T) {
		if got := read(`{"path":"nums.txt","limit":2}`); got != "1→line1\n2→line2" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("non-numeric string is rejected", func(t *testing.T) {
		if got := read(`{"path":"nums.txt","limit":"abc"}`); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("float is rejected", func(t *testing.T) {
		if got := read(`{"path":"nums.txt","limit":2.5}`); !strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q", got)
		}
	})
}

// TestRunReadUnchangedStub 覆盖 mtime 去重：同区间重复读 → stub；
// touch 更新 mtime → 重新返回内容；不同区间（翻页）不命中 stub。
func TestRunReadUnchangedStub(t *testing.T) {
	dir := setupWorkdir(t)
	abs := mustWrite(t, dir, "f.txt", "line1\nline2\nline3\n")
	st := newSessionState()

	first := runRead(st, "f.txt", 0, nil)
	if !strings.Contains(first, "line1") {
		t.Fatalf("first read should return content, got %q", first)
	}
	if second := runRead(st, "f.txt", 0, nil); second != fileUnchangedStub {
		t.Fatalf("unchanged re-read should return stub, got %q", second)
	}

	newTime := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(abs, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	if third := runRead(st, "f.txt", 0, nil); third != first {
		t.Fatalf("after mtime change should return content again, got %q", third)
	}

	// mtime 未变但读取不同区间：必须返回该页内容而非 stub
	page := runRead(st, "f.txt", 2, intPtr(1))
	if page != "2→line2" {
		t.Fatalf("different range should page content, got %q", page)
	}
	// 同一页再读一次 → stub
	if again := runRead(st, "f.txt", 2, intPtr(1)); again != fileUnchangedStub {
		t.Fatalf("same page re-read should return stub, got %q", again)
	}
}

// TestRunGlobSkipsGit 验证 .git 目录被跳过。
func TestRunGlobSkipsGit(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, ".git/config", "[core]\n")
	mustWrite(t, dir, "app.go", "")

	got := runGlob("**/*", "")
	if strings.Contains(got, ".git/config") {
		t.Fatalf(".git contents should be skipped, got:\n%s", got)
	}
	if !strings.Contains(got, "app.go") {
		t.Fatalf("expected app.go in results, got:\n%s", got)
	}
}

// TestRunGlobTruncation 验证超过 maxListResults 条结果时截断并给出提示。
func TestRunGlobTruncation(t *testing.T) {
	dir := setupWorkdir(t)
	for i := 0; i < maxListResults+1; i++ {
		mustWrite(t, dir, fmt.Sprintf("many/f%03d.txt", i), "")
	}

	got := runGlob("many/*.txt", "")
	lines := strings.Split(got, "\n")
	if len(lines) != maxListResults+1 {
		t.Fatalf("got %d output lines, want %d (matches + marker)", len(lines), maxListResults+1)
	}
	if !strings.Contains(lines[len(lines)-1], "more matches omitted") {
		t.Fatalf("last line = %q, want truncation marker", lines[len(lines)-1])
	}
	if strings.Contains(got, fmt.Sprintf("many/f%03d.txt", maxListResults)) {
		t.Fatalf("the %dth match should have been truncated away", maxListResults+1)
	}
}

// TestRunGrepDirectoryPath 验证 path 传入目录时只搜该目录。
func TestRunGrepDirectoryPath(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "a.txt", "foo\n")
	mustWrite(t, dir, "sub/b.txt", "foo in sub\n")

	got := runGrep(runGrepParams{Pattern: "foo", Path: "sub", OutputMode: "content"})
	if !strings.Contains(got, "sub/b.txt:1:foo in sub") {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "a.txt") {
		t.Fatalf("results should be restricted to sub/, got:\n%s", got)
	}
}

// TestRunGrepSkipsBinaryAndOversize 验证二进制文件与超过 maxGrepFileSize 的文件被跳过。
func TestRunGrepSkipsBinaryAndOversize(t *testing.T) {
	dir := setupWorkdir(t)
	// 二进制（非 UTF-8）文件，内含可匹配 ASCII
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0xff, 0xfe, 'f', 'o', 'o'}, 0o644); err != nil {
		t.Fatal(err)
	}
	// 超过大小上限的文本文件
	mustWrite(t, dir, "big.txt", strings.Repeat("foo\n", (maxGrepFileSize/4)+10))
	mustWrite(t, dir, "small.txt", "foo here\n")

	got := runGrep(runGrepParams{Pattern: "foo", OutputMode: "content"})
	if strings.Contains(got, "bin.dat") || strings.Contains(got, "big.txt") {
		t.Fatalf("binary/oversize files should be skipped, got:\n%s", got)
	}
	if !strings.Contains(got, "small.txt:1:foo here") {
		t.Fatalf("expected small.txt match, got:\n%s", got)
	}
}

// TestRunGrepTruncation 验证匹配数超过 maxListResults 时截断。
func TestRunGrepTruncation(t *testing.T) {
	dir := setupWorkdir(t)
	var b strings.Builder
	for i := 0; i < maxListResults+1; i++ {
		b.WriteString("foo\n")
	}
	mustWrite(t, dir, "many.txt", b.String())

	got := runGrep(runGrepParams{Pattern: "foo", OutputMode: "content"})
	lines := strings.Split(got, "\n")
	if len(lines) != maxListResults+1 {
		t.Fatalf("got %d output lines, want %d (matches + marker)", len(lines), maxListResults+1)
	}
	if !strings.Contains(lines[len(lines)-1], "more matches omitted") {
		t.Fatalf("last line = %q, want truncation marker", lines[len(lines)-1])
	}
}

// TestRunGrepPathEscape 验证 grep 的 path 参数同样受 safePath 约束。
func TestRunGrepPathEscape(t *testing.T) {
	setupWorkdir(t)
	if got := runGrep(runGrepParams{Pattern: "foo", Path: "../outside"}); !strings.HasPrefix(got, "Error:") {
		t.Fatalf("got %q, want Error: prefix", got)
	}
}

// TestGitignoreRespected 验证根目录 .gitignore 的规则被 glob 和 grep 同时尊重。
func TestGitignoreRespected(t *testing.T) {
	dir := setupWorkdir(t)
	// 根目录 .gitignore 排除 *.log
	mustWrite(t, dir, ".gitignore", "*.log\n")
	// 创建 .gitignore 规则范围内外的文件
	mustWrite(t, dir, "app.go", "hello from app\n")
	mustWrite(t, dir, "debug.log", "hello from log\n") // 被忽略
	mustWrite(t, dir, "notes.txt", "hello too\n")

	// glob：应排除 *.log
	got := runGlob("*.*", "")
	if strings.Contains(got, "debug.log") {
		t.Fatalf("debug.log should be gitignored, got:\n%s", got)
	}
	if !strings.Contains(got, "app.go") || !strings.Contains(got, "notes.txt") {
		t.Fatalf("non-ignored files missing, got:\n%s", got)
	}

	// grep：应排除 debug.log 里的匹配
	grepOut := runGrep(runGrepParams{Pattern: "hello", OutputMode: "content"})
	if strings.Contains(grepOut, "debug.log") {
		t.Fatalf("debug.log should be gitignored from grep, got:\n%s", grepOut)
	}
	if !strings.Contains(grepOut, "app.go:1:hello from app") {
		t.Fatalf("expected app.go match, got:\n%s", grepOut)
	}
}

// TestNestedGitignore 验证子目录 .gitignore 被 walkerEngine 加载。
func TestNestedGitignore(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "root.go", "hello\n")
	mustWrite(t, dir, "pkg/internal/build.go", "hello internal\n")
	// pkg/internal/.gitignore 排除 build.go
	mustWrite(t, dir, "pkg/internal/.gitignore", "build.go\n")

	// glob 不应返回 build.go
	got := runGlob("**/*.go", "")
	if strings.Contains(got, "pkg/internal/build.go") {
		t.Fatalf("pkg/internal/build.go should be ignored by nested .gitignore, got:\n%s", got)
	}
	if !strings.Contains(got, "root.go") {
		t.Fatalf("root.go missing, got:\n%s", got)
	}
}

// TestRunGlobWithPath 验证 glob 新增的 path 参数。
func TestRunGlobWithPath(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "top.py", "")
	mustWrite(t, dir, "sub/deep/bottom.py", "")
	mustWrite(t, dir, "sub/mid.py", "")

	// 不指定 path：搜到全部（相对于 workdir）
	all := runGlob("**/*.py", "")
	if !strings.Contains(all, "top.py") || !strings.Contains(all, "sub/mid.py") || !strings.Contains(all, "sub/deep/bottom.py") {
		t.Fatalf("path='': expected all 3 .py files, got:\n%s", all)
	}

	// 指定 path="sub"：搜到 sub 目录下的（相对于 sub 的路径）
	sub := runGlob("**/*.py", "sub")
	// 返回的是相对于 sub 的路径：mid.py 和 deep/bottom.py
	if !strings.Contains(sub, "mid.py") || !strings.Contains(sub, "deep/bottom.py") {
		t.Fatalf("path='sub' missing subdir files, got:\n%s", sub)
	}
}
