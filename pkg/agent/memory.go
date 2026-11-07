// memory.go — 记忆子系统的 agent 层入口（召回 / 抽取 / 合并的门控逻辑）。
// 存储实现位于 pkg/tools/memory/store.go；召回/抽取/合并实现位于
// pkg/tools/memory/recall.go。
//
// 四个子系统（对齐参考实现）：
//
//	存储 Storage       .memory/*.md（YAML frontmatter）+ MEMORY.md 索引；写后重建索引
//	召回 Recall        会话开始时：目录发给模型选相关记录（至多 5 条）→ 失败回退关键词
//	                    匹配 → 按总字符预算加载全文，进 system prompt
//	抽取 Extraction    轮末（无工具调用的最终答复后）检查对话；模型给候选而非结论，
//	                    只有 scope=persistent 且通过准入校验的才落盘
//	合并 Consolidation 记录达到阈值后让模型给出清洗列表，校验后整体替换；
//	                    替换前快照，任何一步失败回滚快照并重建索引
package agent

import (
	"github.com/anthropics/anthropic-sdk-go"

	"coding-agent-go/pkg/tools/memory"
)

// memoryEnabled 是记忆子系统的总开关（召回 + 抽取 + 合并）。默认开启；
// loop 级测试通过 setupLogPaths 统一关闭，需要记忆行为的测试再显式打开
// （见 memory_test.go 的 setMemoryEnabled）。
var memoryEnabled = true

// recallMemories 在用户请求进入 loop 之前召回相关记忆，全文存入 st.RecalledMemories
// （由 systemprompt.SystemPrompt 拼进 system）。空库直接返回，不发起额外模型调用。
func recallMemories(st *sessionState, request string) {
	if !memoryEnabled || st.Memory == nil {
		return
	}
	st.RecalledMemories = memory.RecallMemories(st.Ctx, st.Client, st.Memory, request)
}

// extractMemories 在轮末检查对话，把对以后会话有用的信息写成记忆文件。
// 返回是否写入了新记录。任何失败只打警告日志，绝不影响 agent loop 正常返回。
func extractMemories(st *sessionState, messages []anthropic.MessageParam) bool {
	if !memoryEnabled || st.Client == nil || st.Ctx == nil || st.Memory == nil {
		return false
	}
	return memory.ExtractMemories(st.Ctx, st.Client, st.Memory, messages)
}

// consolidateMemories 在记录数达到 memory.ConsolidateThreshold 时让模型给出清洗后的列表，
// 校验通过后整体替换旧文件。任何失败只打警告日志。
func consolidateMemories(st *sessionState) {
	if !memoryEnabled || st.Client == nil || st.Ctx == nil || st.Memory == nil {
		return
	}
	memory.ConsolidateMemories(st.Ctx, st.Client, st.Memory)
}
