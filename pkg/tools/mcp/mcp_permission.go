// mcp_permission.go 为 MCP 外部工具提供 host-side 权限策略。
//
// MCP 协议允许 server 提供 readOnlyHint / destructiveHint, 但这些提示来自 server 本身,
// 不可作为授权依据。宿主 (harness) 侧必须独立决定每个外部工具的放行策略。
//
// 默认: 未配置的 MCP 工具一律 require confirmation —— 最坏情况下用户会被 prompt。
//
// 策略级别:
//
//	0 McpPermAllow   —— 放行, 无需确认
//	1 McpPermConfirm —— 询问用户 (默认)
//	2 McpPermDeny    —— 硬拒绝
package mcp

// PermissionLevel 是 MCP host-side 策略的三档 (int 枚举)。
type PermissionLevel int

const (
	McpPermAllow   PermissionLevel = 0
	McpPermConfirm PermissionLevel = 1
	McpPermDeny    PermissionLevel = 2
)

// McpHostPolicy 按 (server, safeTool) 精确匹配策略。
// 教学版直接硬编码; 生产版应从配置文件加载。
var McpHostPolicy = map[string]PermissionLevel{
	// docs server —— 全是只读操作
	"docs/search":      McpPermAllow,
	"docs/get_version": McpPermAllow,

	// deploy server —— status 只读, trigger 破坏性
	"deploy/status":  McpPermAllow,
	"deploy/trigger": McpPermConfirm,
}

// McpPermission 查策略表, 未命中返回默认值 McpPermConfirm。
func McpPermission(server, safeTool string) PermissionLevel {
	key := server + "/" + safeTool
	if level, ok := McpHostPolicy[key]; ok {
		return level
	}
	return McpPermConfirm
}
