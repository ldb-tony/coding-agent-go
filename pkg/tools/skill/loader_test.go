package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestLoader 在临时目录里造一个 skills/ 并扫描，返回 loader。
// 传入 skillFiles 的键形如 "code-review/SKILL.md"。
func newTestLoader(t *testing.T, skillFiles map[string]string) *Loader {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range skillFiles {
		mustWriteFile(t, dir, filepath.Join("skills", rel), content)
	}
	loader := NewLoader(filepath.Join(dir, "skills"))
	loader.Scan()
	return loader
}

func mustWriteFile(t *testing.T, base, rel, content string) {
	t.Helper()
	p := filepath.Join(base, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestScanFrontmatter 验证标准路径：frontmatter 的 name/description 被采用。
func TestScanFrontmatter(t *testing.T) {
	loader := newTestLoader(t, map[string]string{
		"code-review/SKILL.md": "---\nname: code-review\ndescription: Review code for correctness.\n---\n\n# Code Review\n\nSteps...\n",
	})

	s, ok := loader.Skills()["code-review"]
	if !ok {
		t.Fatalf("skill not registered: %v", loader.SortedNames())
	}
	if s.Description != "Review code for correctness." {
		t.Fatalf("description = %q", s.Description)
	}
	if !strings.Contains(s.Content, "# Code Review") {
		t.Fatalf("content should keep the full file, got %q", s.Content)
	}
}

// TestScanFallbacks 验证 name 回退目录名、description 回退正文首个非空行。
func TestScanFallbacks(t *testing.T) {
	loader := newTestLoader(t, map[string]string{
		"pdf/SKILL.md": "\n#   Extract   PDF text\n\nMore detail.\n",
		"sql/SKILL.md": "---\nname: sql-style\n---\n\n## SQL Style Guide\n",
		"api/SKILL.md": "---\nname: \"api-design\"\ndescription: 'API design conventions.'\n---\n\nbody\n",
	})

	if _, ok := loader.Skills()["pdf"]; !ok {
		t.Fatalf("name should fall back to directory name, got %v", loader.SortedNames())
	}
	if got := loader.Skills()["pdf"].Description; got != "Extract PDF text" {
		t.Fatalf("pdf description = %q, want %q", got, "Extract PDF text")
	}
	if _, ok := loader.Skills()["sql-style"]; !ok {
		t.Fatalf("frontmatter name should win over directory name, got %v", loader.SortedNames())
	}
	if got := loader.Skills()["sql-style"].Description; got != "SQL Style Guide" {
		t.Fatalf("sql description = %q", got)
	}
	if got := loader.Skills()["api-design"].Description; got != "API design conventions." {
		t.Fatalf("quoted description = %q", got)
	}
}

// TestScanMissingDir 验证没有 skills/ 目录时是空注册表，而不是错误。
func TestScanMissingDir(t *testing.T) {
	dir := t.TempDir()
	loader := NewLoader(filepath.Join(dir, "skills"))
	loader.Scan()

	if len(loader.Skills()) != 0 {
		t.Fatalf("expected empty registry, got %v", loader.SortedNames())
	}
	if got := loader.Catalog(); got != "none" {
		t.Fatalf("catalog = %q, want none", got)
	}
	if got := loader.Load("anything"); got != "Error: Unknown skill 'anything'. Available: none" {
		t.Fatalf("load = %q", got)
	}
}

// TestScanSkipsSymlinkEscape 验证 SKILL.md 指向 skills 目录之外的符号链接被跳过。
func TestScanSkipsSymlinkEscape(t *testing.T) {
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

	loader := NewLoader(filepath.Join(dir, "skills"))
	loader.Scan()

	if len(loader.Skills()) != 0 {
		t.Fatalf("escaping symlink should be skipped, got %v", loader.SortedNames())
	}
}

// TestCatalogAndLoad 验证目录只含 name + description、按名排序，load 命中返回全文。
func TestCatalogAndLoad(t *testing.T) {
	loader := newTestLoader(t, map[string]string{
		"zeta/SKILL.md":  "---\ndescription: Last skill.\n---\nZ body\n",
		"alpha/SKILL.md": "---\ndescription: First skill.\n---\nA body\n",
	})

	want := "- alpha: First skill.\n- zeta: Last skill."
	if got := loader.Catalog(); got != want {
		t.Fatalf("catalog =\n%q\nwant\n%q", got, want)
	}
	if got := loader.Load("zeta"); got != "---\ndescription: Last skill.\n---\nZ body\n" {
		t.Fatalf("load = %q", got)
	}
	if got := loader.Load("nope"); got != "Error: Unknown skill 'nope'. Available: alpha, zeta" {
		t.Fatalf("load miss = %q", got)
	}
}

// TestParseFrontmatterEdgeCases 覆盖解析边界：无 frontmatter、未闭合、CRLF、重复键。
func TestParseFrontmatterEdgeCases(t *testing.T) {
	t.Run("no frontmatter", func(t *testing.T) {
		meta, body := ParseFrontmatter("# Just a body\n")
		if len(meta) != 0 || body != "# Just a body\n" {
			t.Fatalf("meta=%v body=%q", meta, body)
		}
	})

	t.Run("unclosed frontmatter is treated as body", func(t *testing.T) {
		content := "---\nname: broken\n"
		meta, body := ParseFrontmatter(content)
		if len(meta) != 0 || body != content {
			t.Fatalf("meta=%v body=%q", meta, body)
		}
	})

	t.Run("crlf frontmatter", func(t *testing.T) {
		meta, body := ParseFrontmatter("---\r\nname: win\r\n---\r\nbody\r\n")
		if meta["name"] != "win" {
			t.Fatalf("meta=%v", meta)
		}
		if strings.TrimSpace(body) != "body" {
			t.Fatalf("body=%q", body)
		}
	})

	t.Run("last duplicate key wins", func(t *testing.T) {
		meta, _ := ParseFrontmatter("---\nname: first\nname: second\n---\nbody\n")
		if meta["name"] != "second" {
			t.Fatalf("meta=%v", meta)
		}
	})
}
