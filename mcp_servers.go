// mcp_servers.go 提供进程内 mock MCP server, 作为真实 MCP 传输层的替身。
// 教学版不做真实网络通信 —— 真实接入时换成 stdio/SSE transport 即可。
package main

import (
	"fmt"
	"strings"
)

// ——————— docs server: 文档搜索 + 版本查询 ———————

type docsServer struct{}

func newDocsServer() MCPServer { return &docsServer{} }

func (s *docsServer) ListTools() ([]ToolDef, error) {
	return []ToolDef{
		{
			Name:        "search",
			Description: "Search the documentation knowledge base by keyword. Returns a list of matching entries with snippets.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":    map[string]string{"type": "string", "description": "Search keyword or phrase"},
					"max_hits": map[string]any{"type": "integer", "description": "Maximum results to return (default 5)"},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "get_version",
			Description: "Return the current documentation API version and last-updated timestamp.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}, nil
}

func (s *docsServer) CallTool(name string, args map[string]any) (string, error) {
	switch name {
	case "search":
		q, _ := args["query"].(string)
		if q == "" {
			return "", fmt.Errorf("query is required")
		}
		maxHits := 5
		if mh, ok := args["max_hits"].(float64); ok && mh > 0 {
			maxHits = int(mh)
		}
		results := mockDocSearch(q, maxHits)
		if len(results) == 0 {
			return fmt.Sprintf("No documentation found for %q", q), nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Found %d results for %q:\n\n", len(results), q)
		for i, r := range results {
			fmt.Fprintf(&b, "%d. [%s] %s\n   %s\n\n", i+1, r.section, r.title, r.snippet)
		}
		return b.String(), nil
	case "get_version":
		return `Documentation API: v2.3.1
Last updated: 2026-10-15
Docs endpoint: https://docs.example.com/api/v2
Supported SDKs: Go, Python, TypeScript`, nil
	default:
		return "", fmt.Errorf("unknown docs tool %q", name)
	}
}

type docResult struct {
	section, title, snippet string
}

func mockDocSearch(query string, maxHits int) []docResult {
	q := strings.ToLower(query)
	all := []docResult{
		{"Hooks", "UserPromptSubmit hook", "Fires after user input, before LLM call. Use for context injection or input validation."},
		{"Hooks", "PreToolUse hook", "Fires before each tool execution. Return non-nil to intercept."},
		{"Hooks", "Stop hook", "Fires when the loop is about to exit. Return non-nil to inject a message and continue."},
		{"Permission", "Three-gate permission model", "Gate 1: deny list. Gate 2: regex/keyword rules. Gate 3: user confirmation."},
		{"Permission", "MCP host-side policy", "MCP servers may provide hints, but the host decides. Unconfigured external tools default to confirm."},
		{"MCP", "Tool name prefixing", "Tools from external servers are exposed as mcp__{server}__{tool} to avoid collisions."},
		{"MCP", "connect_mcp tool", "Use connect_mcp(name=\"docs\") to bring external tools into the model's tool pool."},
		{"MCP", "Tool discovery boundary", "ListTools defines the boundary. Server provides defs + handler, harness handles naming/permissions."},
		{"Architecture", "Assemble tool pool", "Base tools + MCP-discovered tools are merged each turn before sending to the model."},
		{"Architecture", "Session state", "Each agentLoop has independent sessionState: reads, todo, tasks, memory, MCP clients."},
	}
	var hits []docResult
	for _, r := range all {
		if len(hits) >= maxHits {
			break
		}
		combined := strings.ToLower(r.section + " " + r.title + " " + r.snippet)
		if strings.Contains(combined, q) {
			hits = append(hits, r)
		}
	}
	return hits
}

// ——————— deploy server: 服务状态查询 + 部署触发 ———————

type deployServer struct {
	triggered bool
}

func newDeployServer() MCPServer { return &deployServer{} }

func (s *deployServer) ListTools() ([]ToolDef, error) {
	return []ToolDef{
		{
			Name:        "status",
			Description: "Check the current deployment status of a service. Returns version, health, and uptime.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"service": map[string]any{"type": "string", "description": "Service name (default: 'web-api')"},
				},
			},
		},
		{
			Name:        "trigger",
			Description: "Trigger a deployment for the specified service. This is a destructive operation — it will restart the service.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"service": map[string]any{"type": "string", "description": "Service to deploy (default: 'web-api')"},
					"image":   map[string]any{"type": "string", "description": "Docker image tag (default: 'latest')"},
				},
			},
		},
	}, nil
}

func (s *deployServer) CallTool(name string, args map[string]any) (string, error) {
	switch name {
	case "status":
		svc := "web-api"
		if v, ok := args["service"].(string); ok && v != "" {
			svc = v
		}
		if s.triggered {
			return fmt.Sprintf(`Service: %s
Status:  DEPLOYING (rolling restart in progress)
Version: 2.4.0 (pending)
Health:  transitioning
Uptime:  0m`, svc), nil
		}
		return fmt.Sprintf(`Service: %s
Status:  RUNNING
Version: 2.3.1
Health:  OK
Uptime:  14d 6h
Instances: 3/3 healthy`, svc), nil
	case "trigger":
		svc := "web-api"
		if v, ok := args["service"].(string); ok && v != "" {
			svc = v
		}
		image := "latest"
		if v, ok := args["image"].(string); ok && v != "" {
			image = v
		}
		s.triggered = true
		return fmt.Sprintf(`Deployment triggered for %s
Image:    %s
Strategy: rolling
Expected: ~90s
Call status again to poll progress.`, svc, image), nil
	default:
		return "", fmt.Errorf("unknown deploy tool %q", name)
	}
}
