// skill.go — handler 留在主包（依赖 sessionState）。
// 纯逻辑已迁移至 pkg/tools/skill/loader.go。
package agent

import (
	"encoding/json"
	"strings"

	pkgskill "coding-agent-go/pkg/tools/skill"
)

// re-export for sessionState struct field compatibility
type (
	skillLoader = pkgskill.Loader
)

func newSkillLoader(skillsDir string) *skillLoader { return pkgskill.NewLoader(skillsDir) }

var parseFrontmatter = pkgskill.ParseFrontmatter

// loadSkillHandler 是 load_skill 工具的处理函数。
func loadSkillHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	if st == nil || st.Skills == nil {
		return "Error: skill loading is unavailable outside the agent loop"
	}
	return st.Skills.Load(strings.TrimSpace(in.Name))
}
