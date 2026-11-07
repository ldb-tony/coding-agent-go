// mcp.go — handler 留在主包（依赖 sessionState）。
// MCPClient / MCPServer / ToolDef / MCPRegistry 等纯逻辑已迁移至 pkg/tools/mcp/。
package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"coding-agent-go/pkg/tools/mcp"
)

// connectMCPHandler 是 connect_mcp 工具的 handler: 把指定 server 连进来。
func connectMCPHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	if in.Name == "" {
		return "Error: name is required"
	}
	if st.Mcp == nil {
		st.Mcp = mcp.NewMCPRegistry()
	}
	if st.Mcp.HasClient(in.Name) {
		return fmt.Sprintf("MCP server %q already connected", in.Name)
	}
	factory, ok := mcp.MockMCPFactories[in.Name]
	if !ok {
		available := make([]string, 0, len(mcp.MockMCPFactories))
		for k := range mcp.MockMCPFactories {
			available = append(available, k)
		}
		return fmt.Sprintf("Unknown MCP server %q. Available: %s", in.Name, strings.Join(available, ", "))
	}
	if err := st.Mcp.Connect(in.Name, factory); err != nil {
		return "Error connecting MCP server: " + err.Error()
	}
	client := st.Mcp.Clients()[in.Name]
	var toolList strings.Builder
	for _, t := range client.Tools {
		fmt.Fprintf(&toolList, "  - %s  (%s)\n", mcp.FullName(in.Name, mcp.SafeToolName(t.Name)), t.Description)
	}
	return fmt.Sprintf("Connected MCP server %q. New tools available:\n%sYou can now invoke these tools directly.",
		in.Name, toolList.String())
}

func isMCPTool(name string) bool                            { return mcp.IsMCPTool(name) }
func parseMCPName(name string) (string, string, bool)       { return mcp.ParseMCPName(name) }
func mcpPermission(server, tool string) mcp.PermissionLevel { return mcp.McpPermission(server, tool) }

var (
	mcpPermAllow   = mcp.McpPermAllow
	mcpPermDeny    = mcp.McpPermDeny
	mcpPermConfirm = mcp.McpPermConfirm
)
