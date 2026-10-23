package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSystemPromptBlocksComplete 验证分块结构齐全，且旧的一段式开头已移除。
func TestSystemPromptBlocksComplete(t *testing.T) {
	setupWorkdir(t)
	prompt := systemPrompt(newSessionState())

	for _, header := range []string{
		"# Identity",
		"# System",
		"# Doing tasks",
		"# Executing actions with care",
		"# Using your tools",
		"# Tone and style",
		"# Output efficiency",
		"# Session-specific guidance",
		"# Environment",
	} {
		if !strings.Contains(prompt, header) {
			t.Errorf("prompt missing block header %q:\n%s", header, prompt)
		}
	}
	if strings.Contains(prompt, "You are a coding agent at") {
		t.Errorf("prompt still uses the old one-liner opening:\n%s", prompt)
	}
}

// TestSystemPromptUsesRealToolNames 防止把参考实现独有的工具/命令写进 prompt。
func TestSystemPromptUsesRealToolNames(t *testing.T) {
	setupWorkdir(t)
	prompt := systemPrompt(newSessionState())

	for _, name := range []string{
		"read_file", "write_file", "edit_file", "glob", "grep",
		"bash", "todo_write", "task", "load_skill", "compact", "connect_mcp",
		"create_task", "update_task", "get_task", "list_tasks",
	} {
		if !strings.Contains(prompt, name) {
			t.Errorf("prompt should mention real tool %q", name)
		}
	}
	for _, forbidden := range []string{
		"AskUserQuestion", "TodoWrite", "TaskCreate", "Skill tool", "! <command>",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("prompt must not reference unavailable capability %q:\n%s", forbidden, prompt)
		}
	}
}

// TestSystemPromptEnvironment 验证环境块字段来自实际运行环境。
func TestSystemPromptEnvironment(t *testing.T) {
	dir := setupWorkdir(t)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODEL_ID", "test-model-id")
	t.Setenv("SHELL", "/bin/zsh")

	prompt := systemPrompt(newSessionState())
	for _, want := range []string{
		"# Environment",
		"Primary working directory: " + dir,
		"Is a git repository: Yes",
		"Platform: " + runtime.GOOS,
		"Shell: /bin/zsh",
		"You are powered by the model named test-model-id.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("environment block missing %q:\n%s", want, prompt)
		}
	}
	if !strings.Contains(prompt, "OS Version: ") {
		t.Errorf("environment block missing OS version:\n%s", prompt)
	}
}

// TestSystemPromptMemoryOnlyWhenRecalled 验证记忆块只在有召回内容时出现。
func TestSystemPromptMemoryOnlyWhenRecalled(t *testing.T) {
	setupWorkdir(t)

	empty := systemPrompt(newSessionState())
	if strings.Contains(empty, "# Memory") || strings.Contains(empty, "Recalled memories") {
		t.Fatalf("memory block should be absent without recalled memories:\n%s", empty)
	}

	st := newSessionState()
	st.recalledMemories = "User prefers tabs, not spaces."
	prompt := systemPrompt(st)
	for _, want := range []string{
		"# Memory",
		"Recalled memories from previous sessions",
		"User prefers tabs, not spaces.",
		"You have no tool to write memory directly.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("memory block missing %q:\n%s", want, prompt)
		}
	}
}

// TestSystemPromptMCPInstructions 验证 server 级 instructions 只随连接状态出现。
func TestSystemPromptMCPInstructions(t *testing.T) {
	setupWorkdir(t)

	st := newSessionState()
	if p := systemPrompt(st); strings.Contains(p, "# MCP Server Instructions") {
		t.Fatalf("MCP instructions must be absent before connecting:\n%s", p)
	}

	st.mcp = newMCPRegistry()
	if err := st.mcp.connect("docs", newDocsServer); err != nil {
		t.Fatal(err)
	}
	if err := st.mcp.connect("deploy", newDeployServer); err != nil {
		t.Fatal(err)
	}
	prompt := systemPrompt(st)
	if !strings.Contains(prompt, "# MCP Server Instructions") {
		t.Fatalf("missing MCP instructions block:\n%s", prompt)
	}
	for _, want := range []string{"## docs", "## deploy", "mcp__docs__search", "mcp__deploy__trigger"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("MCP block missing %q:\n%s", want, prompt)
		}
	}
	// 按 server 名排序：deploy 在 docs 之前。
	if strings.Index(prompt, "## deploy") > strings.Index(prompt, "## docs") {
		t.Errorf("MCP servers should be sorted by name:\n%s", prompt)
	}
}

// TestSystemPromptConcurrencySemantics 验证并发语义描述已更新，旧的串行表述已移除。
func TestSystemPromptConcurrencySemantics(t *testing.T) {
	setupWorkdir(t)
	prompt := systemPrompt(newSessionState())

	// 旧的串行表述应该已移除
	for _, forbidden := range []string{
		"executed in order, one after another",
		"Batched calls are executed in order",
		"do not rely on them running concurrently",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("prompt still contains old serial execution description %q:\n%s", forbidden, prompt)
		}
	}

	// 新的并发/保序表述应该存在
	for _, want := range []string{
		"read_file / glob / grep",
		"concurrently",
		"write_file / edit_file / bash",
		"do not overlap",
		"in the order they were issued",
		"Results are always returned in the order",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing new concurrency semantics %q:\n%s", want, prompt)
		}
	}
}
