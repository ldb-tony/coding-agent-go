package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// skill 是扫描结果中的一条技能：目录条目 + 全文。
type skill struct {
	Name        string
	Description string
	Content     string
}

// skillLoader 在会话启动时扫描 skillsDir/*/SKILL.md，建立 name → skill 注册表。
// 对齐参考实现的 SkillLoader：只扫一次，之后 load 只查内存注册表。
type skillLoader struct {
	skillsDir string
	skills    map[string]skill
	loaded    []string // 按首次 load 顺序记录已加载技能（去重），压缩后重建附件用
}

func newSkillLoader(skillsDir string) *skillLoader {
	return &skillLoader{skillsDir: skillsDir, skills: map[string]skill{}}
}

// scan 扫描 `<skillsDir>/*/SKILL.md` 并重建注册表。
//
// 边界处理（对齐参考实现的 scan）：
//   - skillsDir 不存在 → 空注册表，不算错误（无技能是合法状态）
//   - manifest 解析（EvalSymlinks）后必须仍位于 skillsDir 内，防符号链接逃逸
//   - 只接受常规文件；读不了或非 UTF-8 的 manifest 跳过
//   - name 回退目录名，description 回退正文首个非空行（去掉前导 "# " 并折叠空白）
func (l *skillLoader) scan() {
	l.skills = map[string]skill{}

	root, err := filepath.EvalSymlinks(l.skillsDir)
	if err != nil {
		return // 目录不存在：没有技能可用
	}
	matches, err := filepath.Glob(filepath.Join(l.skillsDir, "*", "SKILL.md"))
	if err != nil {
		return
	}
	for _, manifest := range matches {
		resolved, err := filepath.EvalSymlinks(manifest)
		if err != nil {
			continue
		}
		// 逃逸校验：解析后的真实路径必须落在 skills 目录内。
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || filepath.IsAbs(rel) ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(resolved)
		if err != nil || !utf8.Valid(data) {
			continue
		}

		content := string(data)
		metadata, body := parseFrontmatter(content)

		name := strings.TrimSpace(metadata["name"])
		if name == "" {
			name = filepath.Base(filepath.Dir(resolved))
		}

		description := strings.TrimSpace(metadata["description"])
		if description == "" {
			for _, line := range strings.Split(body, "\n") {
				if trimmed := strings.TrimSpace(line); trimmed != "" {
					description = trimmed
					break
				}
			}
		}
		// 折叠空白为单个空格，并去掉前导 "# "（对齐参考实现的 " ".join(...lstrip("# ").split())）。
		description = strings.Join(strings.Fields(strings.TrimLeft(description, "# ")), " ")

		l.skills[name] = skill{Name: name, Description: description, Content: content}
	}
}

// catalog 只输出 name + description 的清单，供 system prompt 使用。
// 空注册表返回 "none"。按 name 排序，保证输出稳定（利于 prompt 缓存与测试）。
func (l *skillLoader) catalog() string {
	if len(l.skills) == 0 {
		return "none"
	}
	names := l.sortNames()
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "- %s: %s\n", name, l.skills[name].Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

// load 按 name 查注册表（不解释为文件路径），命中返回 SKILL.md 全文；
// 未命中返回带可用清单的短错误，供模型自我纠正。
func (l *skillLoader) load(name string) string {
	if s, ok := l.skills[name]; ok {
		l.recordLoaded(name)
		return s.Content
	}
	available := "none"
	if names := l.sortNames(); len(names) > 0 {
		available = strings.Join(names, ", ")
	}
	return fmt.Sprintf("Error: Unknown skill '%s'. Available: %s", name, available)
}

// recordLoaded 记录首次加载的技能名（去重、保持顺序）。
func (l *skillLoader) recordLoaded(name string) {
	for _, n := range l.loaded {
		if n == name {
			return
		}
	}
	l.loaded = append(l.loaded, name)
}

// loadedSkill 是已加载技能的正文条目（压缩附件用）。
type loadedSkill struct {
	Name    string
	Content string
}

// loadedContents 按首次 load 顺序返回已加载技能的正文（顺序稳定）。
func (l *skillLoader) loadedContents() []loadedSkill {
	out := make([]loadedSkill, 0, len(l.loaded))
	for _, name := range l.loaded {
		if s, ok := l.skills[name]; ok {
			out = append(out, loadedSkill{Name: name, Content: s.Content})
		}
	}
	return out
}

func (l *skillLoader) sortNames() []string {
	names := make([]string, 0, len(l.skills))
	for name := range l.skills {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// parseFrontmatter 把 SKILL.md 切成 YAML frontmatter（简化为 key: value 行）与正文。
//
// 只支持参考实现用到的子集：frontmatter 必须由文件首行的 "---" 开始、由下一个
// "---" 行结束；值为标量并去掉包裹引号。首行不是 "---" 或缺少结束分隔符时，
// 整份内容按正文处理（无元数据）。行内的重复键按"后者覆盖前者"取值。
func parseFrontmatter(content string) (map[string]string, string) {
	metadata := map[string]string{}
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t\r") != "---" {
		return metadata, content
	}

	closing := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t\r") == "---" {
			closing = i
			break
		}
		key, value, ok := strings.Cut(lines[i], ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		metadata[key] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	if closing < 0 {
		return map[string]string{}, content // 未闭合：视为无 frontmatter
	}
	return metadata, strings.Join(lines[closing+1:], "\n")
}

// loadSkillHandler 是 load_skill 工具的处理函数。
func loadSkillHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	if st == nil || st.skills == nil {
		return "Error: skill loading is unavailable outside the agent loop"
	}
	return st.skills.load(strings.TrimSpace(in.Name))
}
