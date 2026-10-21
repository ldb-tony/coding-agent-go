// mcp_test.go 覆盖 s14 MCP harness 层能力：
// 工具名规范化 / 注册表连接与碰撞 / 工具池动态合并 / runTool 分发 / host-side 权限策略 / loop 级端到端。
package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// newMCPTestState 构建带已初始化 MCP 注册表的会话状态（等价于 connect_mcp 首次调用后的状态）。
func newMCPTestState() *sessionState {
	st := newSessionState()
	st.mcp = newMCPRegistry()
	return st
}

// ——————— 单元: 命名规范化 ———————

func TestSafeToolName(t *testing.T) {
	cases := map[string]string{
		"search":         "search",
		"get.version":    "get_version",
		"one/two":        "one_two",
		"with-dash":      "with_dash",
		"already_ok_123": "already_ok_123",
	}
	for in, want := range cases {
		if got := safeToolName(in); got != want {
			t.Errorf("safeToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMCPName(t *testing.T) {
	server, tool, ok := parseMCPName("mcp__docs__search")
	if !ok || server != "docs" || tool != "search" {
		t.Fatalf("parseMCPName valid = (%q, %q, %v)", server, tool, ok)
	}
	for _, bad := range []string{"mcp__docs", "mcp____search", "docs__search", "mcp__a__", ""} {
		if _, _, ok := parseMCPName(bad); ok {
			t.Errorf("parseMCPName(%q) should fail", bad)
		}
	}
}

// ——————— 单元: 注册表 ———————

func TestMCPRegistryConnect(t *testing.T) {
	r := newMCPRegistry()
	if err := r.connect("docs", newDocsServer); err != nil {
		t.Fatalf("connect docs: %v", err)
	}
	if !r.hasClient("docs") {
		t.Fatal("docs should be connected")
	}
	if err := r.connect("docs", newDocsServer); err == nil {
		t.Fatal("duplicate connect should fail")
	}
	if err := r.connect("deploy", newDeployServer); err != nil {
		t.Fatalf("connect deploy: %v", err)
	}
	defs, dispatch := r.assembleToolDefs()
	if len(defs) != 4 { // docs: search+get_version, deploy: status+trigger
		t.Fatalf("assembleToolDefs = %d tools, want 4", len(defs))
	}
	for _, want := range []string{"mcp__docs__search", "mcp__docs__get_version", "mcp__deploy__status", "mcp__deploy__trigger"} {
		if _, ok := dispatch[want]; !ok {
			t.Errorf("dispatch missing %q", want)
		}
	}
}

// ——————— 单元: 工具池合并 ———————

func TestAssembleToolPoolMergesMCP(t *testing.T) {
	st := newMCPTestState()
	base := assembleToolPool(st)
	if len(base) != len(allTools()) {
		t.Fatalf("no-connection pool = %d, want %d (base only)", len(base), len(allTools()))
	}
	if err := st.mcp.connect("docs", newDocsServer); err != nil {
		t.Fatalf("connect: %v", err)
	}
	merged := assembleToolPool(st)
	if len(merged) != len(base)+2 {
		t.Fatalf("merged pool = %d, want %d", len(merged), len(base)+2)
	}
	found := false
	for _, tool := range merged {
		if tool.OfTool != nil && tool.OfTool.Name == "mcp__docs__search" {
			found = true
		}
	}
	if !found {
		t.Fatal("merged pool should contain mcp__docs__search")
	}
}

// ——————— 单元: runTool 分发 ———————

func TestRunToolMCPDispatch(t *testing.T) {
	st := newMCPTestState()
	out := runTool(st, "mcp__docs__search", json.RawMessage(`{"query":"hooks"}`))
	if !strings.Contains(out, "not connected") {
		t.Fatalf("unconnected dispatch = %q", out)
	}
	if err := st.mcp.connect("docs", newDocsServer); err != nil {
		t.Fatalf("connect: %v", err)
	}
	out = runTool(st, "mcp__docs__search", json.RawMessage(`{"query":"hooks"}`))
	if !strings.Contains(out, "PreToolUse") {
		t.Fatalf("search result missing expected snippet: %q", out)
	}
	// 缺必填参数 → 错误回到 tool boundary, 不 panic
	out = runTool(st, "mcp__docs__search", json.RawMessage(`{}`))
	if !strings.Contains(out, "MCP error") {
		t.Fatalf("missing-arg dispatch should return MCP error, got %q", out)
	}
	out = runTool(st, "mcp__ghost__x", json.RawMessage(`{}`))
	if !strings.Contains(out, "not connected") {
		t.Fatalf("unknown server = %q", out)
	}
	out = runTool(st, "mcp__docs__nope", json.RawMessage(`{}`))
	if !strings.Contains(out, "unknown tool") {
		t.Fatalf("unknown tool = %q", out)
	}
}

// ——————— 单元: connect_mcp handler ———————

func TestConnectMCPHandler(t *testing.T) {
	st := newSessionState() // mcp 为 nil, handler 应惰性初始化
	out := connectMCPHandler(st, json.RawMessage(`{"name":"docs"}`))
	if !strings.Contains(out, "Connected MCP server") || !strings.Contains(out, "mcp__docs__search") {
		t.Fatalf("connect output = %q", out)
	}
	out = connectMCPHandler(st, json.RawMessage(`{"name":"docs"}`))
	if !strings.Contains(out, "already connected") {
		t.Fatalf("reconnect output = %q", out)
	}
	out = connectMCPHandler(st, json.RawMessage(`{"name":"ghost"}`))
	if !strings.Contains(out, "Unknown MCP server") {
		t.Fatalf("unknown server output = %q", out)
	}
}

// ——————— 单元: host-side 权限策略 ———————

func TestMCPPermissionPolicy(t *testing.T) {
	cases := []struct {
		server, tool string
		want         mcpPermissionLevel
	}{
		{"docs", "search", mcpPermAllow},
		{"docs", "get_version", mcpPermAllow},
		{"deploy", "status", mcpPermAllow},
		{"deploy", "trigger", mcpPermConfirm},
		{"unknown", "whatever", mcpPermConfirm}, // 未配置默认 confirm
	}
	for _, c := range cases {
		if got := mcpPermission(c.server, c.tool); got != c.want {
			t.Errorf("mcpPermission(%q, %q) = %q, want %q", c.server, c.tool, got, c.want)
		}
	}
}

func TestCheckPermissionMCP(t *testing.T) {
	if !checkPermission("mcp__docs__search", json.RawMessage(`{"query":"x"}`)) {
		t.Fatal("allow-level MCP tool should pass without prompt")
	}
	// deny 级别硬拒 (临时注入一条策略)
	mcpHostPolicy["docs/search"] = mcpPermDeny
	defer func() { mcpHostPolicy["docs/search"] = mcpPermAllow }()
	if checkPermission("mcp__docs__search", json.RawMessage(`{"query":"x"}`)) {
		t.Fatal("deny-level MCP tool should be blocked")
	}
	// 非 MCP 工具不受影响
	if !checkPermission("read_file", json.RawMessage(`{"path":"x"}`)) {
		t.Fatal("base tool should be unaffected by MCP policy")
	}
}

func TestCheckPermissionMCPConfirm(t *testing.T) {
	// confirm 级别 (deploy/trigger): 用户答 y 放行, 答 n 拦截 —— stub stdin 防悬挂
	called := stubReadApproval(t, "y\n")
	if !checkPermission("mcp__deploy__trigger", json.RawMessage(`{"service":"web-api"}`)) {
		t.Fatal("confirm + y should pass")
	}
	if !*called {
		t.Fatal("confirm level must ask the user")
	}
	stubReadApproval(t, "n\n")
	if checkPermission("mcp__deploy__trigger", json.RawMessage(`{"service":"web-api"}`)) {
		t.Fatal("confirm + n should block")
	}
	// 未配置的 MCP 工具默认也走 confirm
	stubReadApproval(t, "n\n")
	if checkPermission("mcp__docs__unknown_tool", json.RawMessage(`{}`)) {
		t.Fatal("unconfigured MCP tool should default to confirm and block on n")
	}
}

// ——————— loop 级: connect 后下一轮工具池含 MCP 工具 ———————

// captureToolsClient 记录每次 Send 收到的工具名列表。
type captureToolsClient struct {
	responses []*anthropic.Message
	calls     int
	toolNames [][]string
}

func (m *captureToolsClient) Send(ctx context.Context, system string, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam, maxTokens int64) (*anthropic.Message, error) {
	var names []string
	for _, tool := range tools {
		if tool.OfTool != nil {
			names = append(names, tool.OfTool.Name)
		}
	}
	m.toolNames = append(m.toolNames, names)
	resp := m.responses[m.calls]
	m.calls++
	return resp, nil
}

func TestAgentLoopMCPConnectThenCall(t *testing.T) {
	setupLogPaths(t)

	connectResp := mustMessage(t, `{"id":"m1","type":"message","role":"assistant","model":"mock",
		"content":[{"type":"tool_use","id":"tu1","name":"connect_mcp","input":{"name":"docs"}}],
		"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	callResp := mustMessage(t, `{"id":"m2","type":"message","role":"assistant","model":"mock",
		"content":[{"type":"tool_use","id":"tu2","name":"mcp__docs__get_version","input":{}}],
		"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)

	client := &captureToolsClient{responses: []*anthropic.Message{connectResp, callResp, textResponse(t)}}
	initial := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("connect docs and get version")),
	}

	messages, err := agentLoop(context.Background(), client, initial, "test mcp")
	if err != nil {
		t.Fatalf("agentLoop: %v", err)
	}
	if client.calls != 3 {
		t.Fatalf("expected 3 Send calls, got %d", client.calls)
	}
	hasMCP := func(names []string) bool {
		for _, n := range names {
			if n == "mcp__docs__get_version" {
				return true
			}
		}
		return false
	}
	if hasMCP(client.toolNames[0]) {
		t.Fatal("turn 1 should not have MCP tools before connect")
	}
	if !hasMCP(client.toolNames[1]) {
		t.Fatal("turn 2 should include mcp__docs__get_version after connect")
	}
	var allResults []string
	for _, m := range messages {
		for _, b := range m.Content {
			if b.OfToolResult != nil {
				for _, c := range b.OfToolResult.Content {
					if c.OfText != nil {
						allResults = append(allResults, c.OfText.Text)
					}
				}
			}
		}
	}
	joined := strings.Join(allResults, "\n")
	if !strings.Contains(joined, "Connected MCP server") {
		t.Fatalf("missing connect result in messages: %q", joined)
	}
	if !strings.Contains(joined, "Documentation API") {
		t.Fatalf("missing get_version result in messages: %q", joined)
	}
}
