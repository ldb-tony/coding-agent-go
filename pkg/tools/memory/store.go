// store.go 实现跨会话记忆的存储与合并（Storage / Consolidation）。
//
//	存储       .memory/*.md（YAML frontmatter）+ MEMORY.md 索引；写后重建索引
//	合并       记录达到阈值后让模型给出清洗列表，校验后整体替换；
//	           替换前快照，任何一步失败回滚快照并重建索引
//
// 召回（Recall）与抽取（Extraction）依赖 LLM 客户端，留在主包编排层。
package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"coding-agent-go/pkg/tools/filesystem"
	pkgskill "coding-agent-go/pkg/tools/skill"
)

const (
	// DirName 是记忆目录名。
	DirName = ".memory"
	// IndexName 是索引文件名。
	IndexName = "MEMORY.md"
	// MaxRecallMemories 是单次召回最多选中的记录数。
	MaxRecallMemories = 5

	// MaxMemoryBodyRunes 是单条记忆正文的落盘上限（防单条记录失控）。
	MaxMemoryBodyRunes = 4000
	// MaxRecallBodyRunes 是召回正文总字符预算（= 5 条 × 4000，正常不会触发）。
	MaxRecallBodyRunes = 20000
	// ConsolidateThreshold 是记录数达到该值时触发合并。
	ConsolidateThreshold = 10

	// SelectionMaxTokens 是召回选择的模型调用 max_tokens。
	SelectionMaxTokens = 1024
	// ExtractionMaxTokens 是抽取/合并的模型调用 max_tokens。
	ExtractionMaxTokens = 2000
	// MaxDigestLineRunes 是抽取摘要的单行截断长度。
	MaxDigestLineRunes = 400
	// MaxExtractionCtxRunes 是抽取时发给模型的对话摘要总预算。
	MaxExtractionCtxRunes = 30000
)

// Type 是记忆的四种类型。
type Type string

const (
	TypeUser      Type = "user"      // 持久的用户偏好："缩进用 tab"
	TypeFeedback  Type = "feedback"  // 长期有效的指导："不要 mock 数据库"
	TypeProject   Type = "project"   // 稳定的项目事实："认证重写是合规驱动"
	TypeReference Type = "reference" // 外部指针/线索："pipeline 问题在 Linear INGEST"
)

// ValidType 报告 t 是否是合法记忆类型。
func ValidType(t Type) bool {
	switch t {
	case TypeUser, TypeFeedback, TypeProject, TypeReference:
		return true
	}
	return false
}

// Record 是一条已从磁盘解析出来的记忆。
type Record struct {
	Name        string
	Type        Type
	Description string
	Body        string
	File        string // 文件名（含 .md），List 按它排序保证顺序稳定
}

// Candidate 是模型给出的记忆候选；Scope 只有 persistent 才允许持久化。
type Candidate struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Scope       string `json:"scope"`
}

// Store 管理 .memory/ 目录：记录文件 + MEMORY.md 索引。
type Store struct {
	dir string
}

func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// Dir 返回记忆目录。
func (s *Store) Dir() string { return s.dir }

// List 扫描目录下除索引外的全部 .md 记录，按文件名排序。
// 目录不存在 → nil（无记忆是合法状态）；读不了或非 UTF-8 的文件跳过。
func (s *Store) List() []Record {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var records []Record
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || name == IndexName {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil || !utf8.Valid(data) {
			continue
		}
		meta, body := pkgskill.ParseFrontmatter(string(data))
		rec := Record{
			Name:        strings.TrimSpace(meta["name"]),
			Type:        Type(strings.TrimSpace(meta["type"])),
			Description: strings.TrimSpace(meta["description"]),
			Body:        strings.TrimSpace(body),
			File:        name,
		}
		if rec.Name == "" {
			rec.Name = strings.TrimSuffix(name, ".md")
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].File < records[j].File })
	return records
}

// Write 校验并写入一条记忆（frontmatter + 正文），随后重建索引。返回落盘路径。
func (s *Store) Write(name string, memType, description, body string) (string, error) {
	n, mt, d, b, err := NormalizeFields(name, memType, description, body)
	if err != nil {
		return "", err
	}
	if runes := len([]rune(b)); runes > MaxMemoryBodyRunes {
		return "", fmt.Errorf("memory body is %d runes, exceeding the %d-rune limit", runes, MaxMemoryBodyRunes)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(s.dir, Slug(n)+".md")
	if err := filesystem.AtomicWriteFile(path, []byte(Document(n, mt, d, b)), 0o644); err != nil {
		return "", err
	}
	if err := s.rebuildIndex(); err != nil {
		return "", err
	}
	return path, nil
}

// NormalizeFields 校验并规整记忆四元组：name/description 折叠连续空白为单个
// 空格（frontmatter 只支持单行标量，换行会破坏解析），body 仅去首尾空白。
// 任一字段缺失或 type 非法即报错——不完整的候选不允许落盘。
func NormalizeFields(name string, memType, description, body string) (string, Type, string, string, error) {
	name = strings.Join(strings.Fields(strings.TrimSpace(name)), " ")
	description = strings.Join(strings.Fields(strings.TrimSpace(description)), " ")
	body = strings.TrimSpace(body)
	if name == "" {
		return "", "", "", "", errors.New("memory name is required")
	}
	if !ValidType(Type(memType)) {
		return "", "", "", "", fmt.Errorf("invalid memory type %q (must be user, feedback, project, or reference)", memType)
	}
	if description == "" {
		return "", "", "", "", errors.New("memory description is required")
	}
	if body == "" {
		return "", "", "", "", errors.New("memory body is required")
	}
	return name, Type(memType), description, body, nil
}

// Document 渲染一条记忆文件的完整内容（YAML frontmatter + 正文）。
func Document(name string, memType Type, description, body string) string {
	return fmt.Sprintf("---\nname: %s\ntype: %s\ndescription: %s\n---\n\n%s\n",
		name, memType, description, body)
}

// Slug 把记忆名转成文件名安全的 slug：小写，空白/下划线/既有连字符折叠为
// 单个 '-'，丢弃其余符号；全部被丢弃时回退 "memory"，保证永远能落一个文件。
func Slug(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := true // 顺带抑制前导连字符
	for _, r := range name {
		switch {
		case unicode.IsSpace(r) || r == '-' || r == '_':
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "memory"
	}
	return slug
}

// rebuildIndex 从全部记录文件重新生成 MEMORY.md（一行一条：name (type): description）。
func (s *Store) rebuildIndex() error {
	records := s.List()
	var b strings.Builder
	b.WriteString("# Memory Index\n\n")
	if len(records) == 0 {
		b.WriteString("(empty)\n")
	} else {
		for _, r := range records {
			fmt.Fprintf(&b, "- %s (%s): %s\n", r.Name, r.Type, r.Description)
		}
	}
	return filesystem.AtomicWriteFile(filepath.Join(s.dir, IndexName), []byte(b.String()), 0o644)
}

// Replace 用清洗后的记录整体替换目录内容（合并的落盘路径）。替换前快照全部
// 记录文件；删除或写入途中出错则恢复快照并重建索引，绝不留下半截状态。
func (s *Store) Replace(cands []Candidate) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	isRecord := func(name string, isDir bool) bool {
		return !isDir && strings.HasSuffix(name, ".md") && name != IndexName
	}
	snapshot := map[string][]byte{}
	for _, e := range entries {
		if isRecord(e.Name(), e.IsDir()) {
			data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
			if err != nil {
				return err
			}
			snapshot[e.Name()] = data
		}
	}
	removeRecords := func() {
		list, _ := os.ReadDir(s.dir)
		for _, e := range list {
			if isRecord(e.Name(), e.IsDir()) {
				os.Remove(filepath.Join(s.dir, e.Name()))
			}
		}
	}
	restore := func() error {
		removeRecords()
		for name, data := range snapshot {
			if err := os.WriteFile(filepath.Join(s.dir, name), data, 0o644); err != nil {
				return err
			}
		}
		return s.rebuildIndex()
	}

	removeRecords()
	for _, c := range cands {
		name, memType, description, body, err := NormalizeFields(c.Name, c.Type, c.Description, c.Body)
		if err == nil {
			doc := Document(name, memType, description, body)
			err = filesystem.AtomicWriteFile(filepath.Join(s.dir, Slug(name)+".md"), []byte(doc), 0o644)
		}
		if err != nil {
			if rerr := restore(); rerr != nil {
				return fmt.Errorf("%v (restore failed: %v)", err, rerr)
			}
			return err
		}
	}
	return s.rebuildIndex()
}
