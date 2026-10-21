// mcp_permission.go 为 MCP 外部工具提供 host-side 权限策略。
//
// MCP 协议允许 server 提供 readOnlyHint / destructiveHint, 但这些提示来自 server 本身,
// 不可作为授权依据。宿主 (harness) 侧必须独立决定每个外部工具的放行策略。
//
// 默认: 未配置的 MCP 工具一律 require confirmation —— 最坏情况下用户会被 prompt。
//
// 策略级别:
//
//	"allow"     —— 放行, 无需确认
//	"confirm"   —— 询问用户 (默认)
//	"deny"      —— 硬拒绝
package main

// mcpPermissionLevel 是 MCP host-side 策略的三档。
type mcpPermissionLevel string

const (
	mcpPermAllow   mcpPermissionLevel = "allow"
	mcpPermConfirm mcpPermissionLevel = "confirm"
	mcpPermDeny    mcpPermissionLevel = "deny"
)

// mcpHostPolicy 按 (server, safeTool) 精确匹配策略。
// 教学版直接硬编码; 生产版应从配置文件加载。
var mcpHostPolicy = map[string]mcpPermissionLevel{
	// docs server —— 全是只读操作
	"docs/search":     mcpPermAllow,
	"docs/get_version": mcpPermAllow,

	// deploy server —— status 只读, trigger 破坏性
	"deploy/status":  mcpPermAllow,
	"deploy/trigger": mcpPermConfirm,
}

// mcpPermission 查策略表, 未命中返回默认值 confirm。
func mcpPermission(server, safeTool string) mcpPermissionLevel {
	key := server + "/" + safeTool
	if level, ok := mcpHostPolicy[key]; ok {
		return level
	}
	// 兜底: 未配置的外部工具一律确认
	return mcpPermConfirm
}
