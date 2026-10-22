// mcp.go 实现 s14 的 MCP (Model Context Protocol) harness 层能力:
// 连接外部服务、发现工具、将发现的工具注入 agent loop 供 LLM 调用。
//
// 本实现为教学版: 服务端以进程内 mock server 形式提供 tools/list 与 tools/call 边界，
// 不实现真实 MCP 传输层 (stdio/SSE)。后续接入真实 MCP 服务时只需实现 MCPServer 接口。
//
// 核心组件:
//
//	MCPServer    —— 服务端接口, ListTools 返回工具定义, CallTool 执行调用
//	MCPClient    —— 持有一个 MCPServer 实例, 负责发现 + 缓存 + 按名调用
//	mcpRegistry  —— 会话级注册表, 管理所有已连接的 MCPClient
//
// 工具命名: mcp__{server}__{tool}, 前缀碰撞 (不同 server 规范化后撞名) 在注册期拒绝。
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// MCPServer 是 MCP 服务端抽象。真实 MCP 传输层 (stdio/SSE) 通过实现此接口接入。
type MCPServer interface {
	ListTools() ([]ToolDef, error)
	CallTool(name string, args map[string]any) (string, error)
	// Instructions 返回 server 级使用说明（等价于 MCP 协议的 instructions 字段）。
	// 返回空字符串表示该 server 没有额外说明。
	Instructions() string
}

// ToolDef 描述一个 MCP 工具 (tools/list 返回的条目)。
type ToolDef struct {
	Name        string         // server 内部的原始工具名
	Description string         // 给模型看的用途说明
	InputSchema map[string]any // JSON Schema (type=object)
}

// MCPClient 持有一个已发现的 MCP 服务及其工具列表。
type MCPClient struct {
	server   MCPServer
	serverName string            // 注册时的 server 名 (规范化后, 用于 mcp__{server}__{tool} 前缀)
	tools    []ToolDef          // 发现的工具定义
	origNames map[string]string // safeName → 原始 server-side 名 (CallTool 时回查)
}

// newMCPClient 从一个 MCPServer 发现全部工具并构建客户端。
func newMCPClient(name string, srv MCPServer) (*MCPClient, error) {
	defs, err := srv.ListTools()
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	c := &MCPClient{
		server:     srv,
		serverName: name,
		tools:      defs,
		origNames:  make(map[string]string),
	}
	return c, nil
}

// safeToolName 把 MCP 工具名规范化为模型工具名允许的字符集 (A-Za-z0-9_),
// 撞名后需由上游做碰撞检测 (见 checkCollision)。
var mcpNameSanitizer = regexp.MustCompile(`[^A-Za-z0-9_]`)

func safeToolName(raw string) string {
	return mcpNameSanitizer.ReplaceAllString(raw, "_")
}

// fullName 拼出模型可见的完整工具名: mcp__{server}__{tool}
func fullName(server, tool string) string {
	return "mcp__" + server + "__" + tool
}

// callTool 通过 server 执行一个已发现的工具。
func (c *MCPClient) callTool(safeTool string, argsJSON json.RawMessage) string {
	orig, ok := c.origNames[safeTool]
	if !ok {
		return fmt.Sprintf("MCP error: unknown tool %q on server %q", safeTool, c.serverName)
	}
	args := map[string]any{}
	if len(argsJSON) > 0 {
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return fmt.Sprintf("MCP error: invalid arguments: %v", err)
		}
	}
	result, err := c.server.CallTool(orig, args)
	if err != nil {
		return fmt.Sprintf("MCP error: %T: %v", err, err)
	}
	// 与 read_file/bash 同级的输出预算：MCP server 返回不受信，超长结果必须截断，
	// 避免单次调用吃掉整个上下文窗口。
	if runes := []rune(result); len(runes) > maxMCPOutputRunes {
		result = string(runes[:maxMCPOutputRunes]) +
			fmt.Sprintf("\n... (MCP output truncated at %d chars, total %d)", maxMCPOutputRunes, len(runes))
	}
	return result
}

// maxMCPOutputRunes 是 MCP 工具单次返回的字符预算（对齐 maxReadOutputRunes）。
const maxMCPOutputRunes = 25000

// mcpRegistry 是会话级已连接 MCP 客户端的注册表。
// key = server 名 (规范化后), value = MCPClient。
type mcpRegistry struct {
	clients map[string]*MCPClient
}

func newMCPRegistry() *mcpRegistry {
	return &mcpRegistry{clients: map[string]*MCPClient{}}
}

// connect 连接一个 server 工厂产出的服务, 完成工具发现并检查前缀碰撞。
// 工厂模式: 调用方提供 func() MCPServer, registry 实例化一次后持有。
func (r *mcpRegistry) connect(serverName string, factory func() MCPServer) error {
	if _, exists := r.clients[serverName]; exists {
		return fmt.Errorf("MCP server %q already connected", serverName)
	}
	srv := factory()
	client, err := newMCPClient(serverName, srv)
	if err != nil {
		return err
	}
	// 注册前做前缀碰撞检测: server 内部的工具规范化后不能撞 (即使同一个 server 也不允许)
	// 同时也与已有 client 的 fullName 比
	existing := r.allFullNames()
	for _, t := range client.tools {
		safe := safeToolName(t.Name)
		fn := fullName(serverName, safe)
		if _, dup := existing[fn]; dup {
			return fmt.Errorf("MCP tool name collision: %q from server %q (normalized from %q)",
				fn, serverName, t.Name)
		}
		client.origNames[safe] = t.Name
		existing[fn] = true
	}
	r.clients[serverName] = client
	return nil
}

// allFullNames 返回所有已注册 client 的 fullName 集合, 用于碰撞检测。
func (r *mcpRegistry) allFullNames() map[string]bool {
	out := map[string]bool{}
	for _, c := range r.clients {
		for _, t := range c.tools {
			out[fullName(c.serverName, safeToolName(t.Name))] = true
		}
	}
	return out
}

// assembleToolDefs 把所有已连接 client 的工具定义展开为 anthropic.ToolUnionParam 列表,
// 同时返回一个 fullName → MCPClient 的映射供 runTool dispatch 使用。
func (r *mcpRegistry) assembleToolDefs() ([]anthropic.ToolUnionParam, map[string]*MCPClient) {
	var defs []anthropic.ToolUnionParam
	dispatch := map[string]*MCPClient{}
	for _, c := range r.clients {
		for _, t := range c.tools {
			safe := safeToolName(t.Name)
			fn := fullName(c.serverName, safe)
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			var required []string
			switch r := schema["required"].(type) {
			case []string:
				required = r
			case []any:
				for _, v := range r {
					if s, ok := v.(string); ok {
						required = append(required, s)
					}
				}
			}
			tp := anthropic.ToolParam{
				Name:        fn,
				Description: anthropic.String(t.Description),
				InputSchema: anthropic.ToolInputSchemaParam{
					Type:       "object",
					Properties: schema["properties"],
					Required:   required,
				},
			}
			defs = append(defs, anthropic.ToolUnionParam{OfTool: &tp})
			dispatch[fn] = c
		}
	}
	return defs, dispatch
}

// hasClient 返回指定 server 是否已连接。
func (r *mcpRegistry) hasClient(name string) bool {
	_, ok := r.clients[name]
	return ok
}

// serverNames 返回所有已连接 server 的名字列表 (排序后, 便于日志/调试)。
func (r *mcpRegistry) serverNames() []string {
	names := make([]string, 0, len(r.clients))
	for n := range r.clients {
		names = append(names, n)
	}
	return names
}

// instructionsSection 渲染已连接 server 的 instructions。没有非空说明时返回空串，
// systemPrompt 据此跳过整个 Block。按 server 名排序保证输出稳定（利于测试与缓存）。
func (r *mcpRegistry) instructionsSection() string {
	names := make([]string, 0, len(r.clients))
	for name, c := range r.clients {
		if strings.TrimSpace(c.server.Instructions()) != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# MCP Server Instructions\n\n")
	b.WriteString("The following MCP servers have provided instructions for how to use their tools and resources:\n")
	for _, name := range names {
		fmt.Fprintf(&b, "\n## %s\n%s\n", name, strings.TrimSpace(r.clients[name].server.Instructions()))
	}
	return strings.TrimRight(b.String(), "\n")
}

// connect_mcp 的 mock server 注册表 —— 教学版用进程内工厂函数代表真实服务。
// 真实接入时换成从配置文件/命令行读 (name → stdio command / SSE URL)。
var mockMCPFactories = map[string]func() MCPServer{
	"docs":   newDocsServer,
	"deploy": newDeployServer,
}

// connectMCPHandler 是 connect_mcp 工具的 handler: 把指定 server 连进来。
// server 名来自 mockMCPFactories (教学版)。
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
	if st.mcp == nil {
		st.mcp = newMCPRegistry()
	}
	if st.mcp.hasClient(in.Name) {
		return fmt.Sprintf("MCP server %q already connected", in.Name)
	}
	factory, ok := mockMCPFactories[in.Name]
	if !ok {
		available := make([]string, 0, len(mockMCPFactories))
		for k := range mockMCPFactories {
			available = append(available, k)
		}
		return fmt.Sprintf("Unknown MCP server %q. Available: %s", in.Name, strings.Join(available, ", "))
	}
	if err := st.mcp.connect(in.Name, factory); err != nil {
		return "Error connecting MCP server: " + err.Error()
	}
	// 构建成功提示: 列出刚连接上的 server 暴露了哪些工具
	client := st.mcp.clients[in.Name]
	var toolList strings.Builder
	for _, t := range client.tools {
		fmt.Fprintf(&toolList, "  - %s  (%s)\n", fullName(in.Name, safeToolName(t.Name)), t.Description)
	}
	return fmt.Sprintf("Connected MCP server %q. New tools available:\n%sYou can now invoke these tools directly.",
		in.Name, toolList.String())
}

// isMCPTool 判断一个工具名是否来自 MCP (前缀 mcp__)。
func isMCPTool(name string) bool {
	return strings.HasPrefix(name, "mcp__")
}

// parseMCPName 从 mcp__{server}__{tool} 提取 server 名和 safeTool 名。
// 失败时返回 ok=false。
func parseMCPName(name string) (server, safeTool string, ok bool) {
	if !strings.HasPrefix(name, "mcp__") {
		return "", "", false
	}
	trimmed := strings.TrimPrefix(name, "mcp__")
	parts := strings.SplitN(trimmed, "__", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
