// permission.go — checkPermission 留在根目录，因依赖 mcp 包。
// 纯函数已拆至 pkg/perm/perm.go，checkPermission 通过 perm 包调用。
package agent

import (
	"encoding/json"
	"fmt"

	"coding-agent-go/pkg/perm"
)

// checkPermission 按固定顺序过三道门：硬拒 → 规则+询问 → 直通。
// MCP 外部工具走独立的 host-side 策略。
func checkPermission(toolName string, args json.RawMessage) bool {
	// MCP 外部工具走独立的 host-side 策略（server 自称的 readOnlyHint 不可信）
	if isMCPTool(toolName) {
		server, safeTool, ok := parseMCPName(toolName)
		if !ok {
			return true // 名字无法解析时交给 runTool 的 fallback 报错，不在这里拦
		}
		switch mcpPermission(server, safeTool) {
		case mcpPermAllow:
			return true
		case mcpPermDeny:
			fmt.Printf("\n⛔ Blocked: MCP tool %q denied by host policy\n", toolName)
			return false
		default: // mcpPermConfirm —— 未配置的外部工具默认也落这里
			return perm.AskUser(toolName, args, "MCP external tool — host policy requires confirmation")
		}
	}
	// Gate 1: 硬性拒绝清单（仅 bash）
	if toolName == "bash" {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(args, &in)
		if pattern, ok := perm.CheckDenyList(in.Command); ok {
			fmt.Printf("\n⛔ Blocked: '%s' is on the deny list\n", pattern)
			return false
		}
	}
	// Gate 2 + 3: 规则命中则询问用户
	if reason, matched := perm.CheckRules(toolName, args); matched {
		return perm.AskUser(toolName, args, reason)
	}
	// 三门均未命中：直通
	return true
}
