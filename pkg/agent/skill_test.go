package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"coding-agent-go/pkg/session"
	"coding-agent-go/pkg/systemprompt"
)

// TestLoadSkillHandler 通过 runTool 分发验证 handler：命中返回全文，未命中/坏参数返回 Error。
func TestLoadSkillHandler(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "skills/pdf/SKILL.md", "---\ndescription: Process PDF files.\n---\nPDF instructions\n")

	st := session.NewState()
	if st.Skills == nil || len(st.Skills.Skills()) != 1 {
		t.Fatalf("session should scan skills from workdir(), got %+v", st.Skills)
	}

	t.Run("hit returns full content", func(t *testing.T) {
		got := runTool(st, "load_skill", json.RawMessage(`{"name":"pdf"}`))
		if got != "---\ndescription: Process PDF files.\n---\nPDF instructions\n" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("unknown name returns available list", func(t *testing.T) {
		got := runTool(st, "load_skill", json.RawMessage(`{"name":"ghost"}`))
		if got != "Error: Unknown skill 'ghost'. Available: pdf" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("invalid json args", func(t *testing.T) {
		got := runTool(st, "load_skill", json.RawMessage(`{bad`))
		if !strings.HasPrefix(got, "Error:") {
			t.Fatalf("got %q, want Error: prefix", got)
		}
	})
}

// TestSystemPromptContainsCatalogOnly 验证 system prompt 只带目录、不带 SKILL.md 全文。
func TestSystemPromptContainsCatalogOnly(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "skills/code-review/SKILL.md",
		"---\ndescription: Review code changes.\n---\nSECRET-FULL-BODY\n")

	prompt := systemprompt.SystemPrompt(session.NewState())
	if !strings.Contains(prompt, "Skills available:") {
		t.Fatalf("prompt should announce the catalog:\n%s", prompt)
	}
	if !strings.Contains(prompt, "- code-review: Review code changes.") {
		t.Fatalf("prompt should list name + description:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Use load_skill to read the full instructions when a skill applies.") {
		t.Fatalf("prompt should tell the model how to load a skill:\n%s", prompt)
	}
	if strings.Contains(prompt, "SECRET-FULL-BODY") {
		t.Fatalf("full SKILL.md must not be inlined into the system prompt:\n%s", prompt)
	}
}
