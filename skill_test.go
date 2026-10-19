package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestSkillLoader 在临时目录里造一个 skills/ 并扫描，返回 loader。
// 传入 skillFiles 的键形如 "code-review/SKILL.md"。
func newTestSkillLoader(t *testing.T, skillFiles map[string]string) *skillLoader {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range skillFiles {
		mustWrite(t, dir, filepath.Join("skills", rel), content)
	}
	loader := newSkillLoader(filepath.Join(dir, "skills"))
	loader.scan()
	return loader
}

// TestSkillScanFrontmatter 验证标准路径：frontmatter 的 name/description 被采用。
func TestSkillScanFrontmatter(t *testing.T) {
	loader := newTestSkillLoader(t, map[string]string{
		"code-review/SKILL.md": "---\nname: code-review\ndescription: Review code for correctness.\n---\n\n# Code Review\n\nSteps...\n",
	})

	s, ok := loader.skills["code-review"]
	if !ok {
		t.Fatalf("skill not registered: %v", loader.sortNames())
	}
	if s.Description != "Review code for correctness." {
		t.Fatalf("description = %q", s.Description)
	}
	if !strings.Contains(s.Content, "# Code Review") {
		t.Fatalf("content should keep the full file, got %q", s.Content)
	}
}

// TestSkillScanFallbacks 验证 name 回退目录名、description 回退正文首个非空行
// （去前导 "# " 并折叠空白），对齐参考实现的回退链。
func TestSkillScanFallbacks(t *testing.T) {
	loader := newTestSkillLoader(t, map[string]string{
		// 无 frontmatter：name 取目录名，description 取正文首个非空行
		"pdf/SKILL.md": "\n#   Extract   PDF text\n\nMore detail.\n",
		// 有 frontmatter 但缺 description：同样回退正文
		"sql/SKILL.md": "---\nname: sql-style\n---\n\n## SQL Style Guide\n",
		// name 显式给值但带引号：去掉包裹引号
		"api/SKILL.md": "---\nname: \"api-design\"\ndescription: 'API design conventions.'\n---\n\nbody\n",
	})

	if _, ok := loader.skills["pdf"]; !ok {
		t.Fatalf("name should fall back to directory name, got %v", loader.sortNames())
	}
	if got := loader.skills["pdf"].Description; got != "Extract PDF text" {
		t.Fatalf("pdf description = %q, want %q", got, "Extract PDF text")
	}
	if _, ok := loader.skills["sql-style"]; !ok {
		t.Fatalf("frontmatter name should win over directory name, got %v", loader.sortNames())
	}
	if got := loader.skills["sql-style"].Description; got != "SQL Style Guide" {
		t.Fatalf("sql description = %q", got)
	}
	if got := loader.skills["api-design"].Description; got != "API design conventions." {
		t.Fatalf("quoted description = %q", got)
	}
}

// TestSkillScanMissingDir 验证没有 skills/ 目录时是空注册表，而不是错误。
func TestSkillScanMissingDir(t *testing.T) {
	dir := t.TempDir()
	loader := newSkillLoader(filepath.Join(dir, "skills"))
	loader.scan()

	if len(loader.skills) != 0 {
		t.Fatalf("expected empty registry, got %v", loader.sortNames())
	}
	if got := loader.catalog(); got != "none" {
		t.Fatalf("catalog = %q, want none", got)
	}
	if got := loader.load("anything"); got != "Error: Unknown skill 'anything'. Available: none" {
		t.Fatalf("load = %q", got)
	}
}

// TestSkillScanSkipsSymlinkEscape 验证 SKILL.md 指向 skills 目录之外的符号链接被跳过。
func TestSkillScanSkipsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.md")
	if err := os.WriteFile(outside, []byte("---\nname: evil\ndescription: escaped\n---\nsecret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills", "evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "skills", "evil", "SKILL.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	loader := newSkillLoader(filepath.Join(dir, "skills"))
	loader.scan()

	if len(loader.skills) != 0 {
		t.Fatalf("escaping symlink should be skipped, got %v", loader.sortNames())
	}
}

// TestSkillCatalogAndLoad 验证目录只含 name + description、按名排序，load 命中返回全文。
func TestSkillCatalogAndLoad(t *testing.T) {
	loader := newTestSkillLoader(t, map[string]string{
		"zeta/SKILL.md":  "---\ndescription: Last skill.\n---\nZ body\n",
		"alpha/SKILL.md": "---\ndescription: First skill.\n---\nA body\n",
	})

	want := "- alpha: First skill.\n- zeta: Last skill."
	if got := loader.catalog(); got != want {
		t.Fatalf("catalog =\n%q\nwant\n%q", got, want)
	}
	if got := loader.load("zeta"); got != "---\ndescription: Last skill.\n---\nZ body\n" {
		t.Fatalf("load = %q", got)
	}
	if got := loader.load("nope"); got != "Error: Unknown skill 'nope'. Available: alpha, zeta" {
		t.Fatalf("load miss = %q", got)
	}
}

// TestLoadSkillHandler 通过 runTool 分发验证 handler：命中返回全文，未命中/坏参数返回 Error。
func TestLoadSkillHandler(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "skills/pdf/SKILL.md", "---\ndescription: Process PDF files.\n---\nPDF instructions\n")

	st := newSessionState()
	if st.skills == nil || len(st.skills.skills) != 1 {
		t.Fatalf("session should scan skills from workdir, got %+v", st.skills)
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

// TestSystemPromptContainsCatalogOnly 验证 system prompt 只带目录、不带 SKILL.md 全文
// （对齐参考实现的验收点：调用 load_skill 之后全文才进入消息列表）。
func TestSystemPromptContainsCatalogOnly(t *testing.T) {
	dir := setupWorkdir(t)
	mustWrite(t, dir, "skills/code-review/SKILL.md",
		"---\ndescription: Review code changes.\n---\nSECRET-FULL-BODY\n")

	prompt := systemPrompt(newSessionState())
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

// TestParseFrontmatterEdgeCases 覆盖解析边界：无 frontmatter、未闭合、CRLF、重复键。
func TestParseFrontmatterEdgeCases(t *testing.T) {
	t.Run("no frontmatter", func(t *testing.T) {
		meta, body := parseFrontmatter("# Just a body\n")
		if len(meta) != 0 || body != "# Just a body\n" {
			t.Fatalf("meta=%v body=%q", meta, body)
		}
	})

	t.Run("unclosed frontmatter is treated as body", func(t *testing.T) {
		content := "---\nname: broken\n"
		meta, body := parseFrontmatter(content)
		if len(meta) != 0 || body != content {
			t.Fatalf("meta=%v body=%q", meta, body)
		}
	})

	t.Run("crlf frontmatter", func(t *testing.T) {
		meta, body := parseFrontmatter("---\r\nname: win\r\n---\r\nbody\r\n")
		if meta["name"] != "win" {
			t.Fatalf("meta=%v", meta)
		}
		if strings.TrimSpace(body) != "body" {
			t.Fatalf("body=%q", body)
		}
	})

	t.Run("last duplicate key wins", func(t *testing.T) {
		meta, _ := parseFrontmatter("---\nname: first\nname: second\n---\nbody\n")
		if meta["name"] != "second" {
			t.Fatalf("meta=%v", meta)
		}
	})
}
