package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"coding-agent-go/pkg/session"
	"coding-agent-go/pkg/tools/memory"
)

// setMemoryEnabled 显式切换记忆总开关并注册恢复。
// 既有 loop 级测试在 setupLogPaths 里统一关闭；需要记忆行为的测试用它打开。
func setMemoryEnabled(t *testing.T, v bool) {
	t.Helper()
	old := memoryEnabled
	memoryEnabled = v
	t.Cleanup(func() { memoryEnabled = old })
}

// failingClient 是永远返回错误的 LLM 客户端，用于验证兜底路径。
type failingClient struct{}

func (failingClient) Send(context.Context, string, []anthropic.MessageParam, []anthropic.ToolUnionParam, int64) (*anthropic.Message, error) {
	return nil, errors.New("llm unavailable")
}

// systemCaptureClient 包装 mockClient，记录每次调用的 system prompt。
type systemCaptureClient struct {
	inner   *mockClient
	systems []string
}

func (c *systemCaptureClient) Send(ctx context.Context, system string, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam, maxTokens int64) (*anthropic.Message, error) {
	c.systems = append(c.systems, system)
	return c.inner.Send(ctx, system, messages, tools, maxTokens)
}

// jsonTextResponse 构造一个只含指定 text 的 mock 响应。
func jsonTextResponse(t *testing.T, text string) *anthropic.Message {
	t.Helper()
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatalf("marshal text: %v", err)
	}
	return mustMessage(t, fmt.Sprintf(`{"id":"msg_json","type":"message","role":"assistant","model":"mock","content":[{"type":"text","text":%s}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, encoded))
}

// newTestStore 建一个指向临时目录的空记忆库。
func newTestStore(t *testing.T) *memory.Store {
	t.Helper()
	return memory.NewStore(t.TempDir())
}

// TestMemorySlug 覆盖 slug 规则：折叠空白为连字符、丢弃符号、小写、空名回退。
func TestMemorySlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"User Preference: Tabs!", "user-preference-tabs"},
		{"  already-slug  ", "already-slug"},
		{"multiple   spaces___and-dashes", "multiple-spaces-and-dashes"},
		{"", "memory"},
		{"   ", "memory"},
		{"!!!", "memory"},
		{"用户偏好", "用户偏好"}, // CJK 字母类字符保留
	}
	for _, c := range cases {
		if got := memory.Slug(c.in); got != c.want {
			t.Errorf("memory.Slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMemoryStoreWriteListIndex 验证写入 → 解析读回 → 索引重建的完整链路。
func TestMemoryStoreWriteListIndex(t *testing.T) {
	s := newTestStore(t)

	path, err := s.Write("User Preference: Tabs!", "user", "User prefers tabs", "User prefers tabs, not spaces.")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if filepath.Base(path) != "user-preference-tabs.md" {
		t.Fatalf("path = %q", path)
	}

	records := s.List()
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	r := records[0]
	if r.Name != "User Preference: Tabs!" || r.Type != memory.TypeUser ||
		r.Description != "User prefers tabs" || r.Body != "User prefers tabs, not spaces." {
		t.Fatalf("parsed record mismatch: %+v", r)
	}

	indexData, err := os.ReadFile(filepath.Join(s.Dir(), memory.IndexName))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if !strings.Contains(string(indexData), "- User Preference: Tabs! (user): User prefers tabs") {
		t.Fatalf("index missing entry: %s", indexData)
	}

	// 同名覆盖：slug 相同 → 仍是一条记录
	if _, err := s.Write("user preference tabs", "user", "Updated description", "Updated body."); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if records := s.List(); len(records) != 1 {
		t.Fatalf("expected 1 record after overwrite, got %d", len(records))
	}
}

// TestMemoryStoreSkipsNonRecords 验证 list 只认记录 .md：索引、其他扩展名、目录都跳过。
func TestMemoryStoreSkipsNonRecords(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Write("real record", "project", "A real record", "Body."); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustWrite(t, s.Dir(), "notes.txt", "not a record")
	mustWrite(t, s.Dir(), memory.IndexName, "# Memory Index")
	if err := os.MkdirAll(filepath.Join(s.Dir(), "dir.md"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	records := s.List()
	if len(records) != 1 || records[0].Name != "real record" {
		t.Fatalf("expected only 'real record', got %+v", records)
	}
}

// TestMemoryStoreWriteValidation 覆盖落盘前的字段校验。
func TestMemoryStoreWriteValidation(t *testing.T) {
	s := newTestStore(t)

	cases := []struct {
		name, memType, desc, body string
		wantErr                   string
	}{
		{"", "user", "d", "b", "name is required"},
		{"n", "diary", "d", "b", "invalid memory type"},
		{"n", "user", "", "b", "description is required"},
		{"n", "user", "d", "  ", "body is required"},
	}
	for _, c := range cases {
		if _, err := s.Write(c.name, c.memType, c.desc, c.body); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("write(%q,%q,%q,%q) err = %v, want contains %q", c.name, c.memType, c.desc, c.body, err, c.wantErr)
		}
	}

	tooLong := strings.Repeat("字", memory.MaxMemoryBodyRunes+1)
	if _, err := s.Write("n", "user", "d", tooLong); err == nil || !strings.Contains(err.Error(), "exceeding") {
		t.Errorf("oversized body err = %v, want exceeding error", err)
	}
	if records := s.List(); len(records) != 0 {
		t.Fatalf("failed writes must not leave records, got %d", len(records))
	}
}

// TestNormalizeMemoryFields 验证 name/description 的换行折叠（frontmatter 单行约束）。
func TestNormalizeMemoryFields(t *testing.T) {
	name, memType, desc, body, err := memory.NormalizeFields("  multi\nline  name ", "user", "desc\nwith\nnewlines", "  body\nkeeps newlines  ")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if name != "multi line name" || desc != "desc with newlines" {
		t.Fatalf("folding broken: name=%q desc=%q", name, desc)
	}
	if memType != memory.TypeUser || body != "body\nkeeps newlines" {
		t.Fatalf("type/body mismatch: %q %q", memType, body)
	}
}

// TestSelectRelevantMemoriesByModel 验证模型选择路径：解析、去重、越界丢弃、截断到 5 条。
func TestSelectRelevantMemoriesByModel(t *testing.T) {
	records := make([]memory.Record, 6)
	for i := range records {
		records[i] = memory.Record{Name: fmt.Sprintf("rec-%d", i), Type: memory.TypeProject,
			Description: fmt.Sprintf("desc %d", i), File: fmt.Sprintf("rec-%d.md", i)}
	}

	t.Run("plain selection", func(t *testing.T) {
		client := &mockClient{responses: []*anthropic.Message{jsonTextResponse(t, "[0, 2]")}}
		got := memory.SelectRelevantMemories(context.Background(), client, "request", records)
		if len(got) != 2 || got[0] != 0 || got[1] != 2 {
			t.Fatalf("got %v", got)
		}
		if client.calls != 1 {
			t.Fatalf("expected 1 LLM call, got %d", client.calls)
		}
	})

	t.Run("dedupe clamp and cap", func(t *testing.T) {
		client := &mockClient{responses: []*anthropic.Message{jsonTextResponse(t, "[9, 0, 0, 1, 2, 3, 4, 5]")}}
		got := memory.SelectRelevantMemories(context.Background(), client, "request", records)
		want := []int{0, 1, 2, 3, 4}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})
}

// TestSelectRelevantMemoriesFallsBack 验证模型输出不可解析时回退关键词匹配。
func TestSelectRelevantMemoriesFallsBack(t *testing.T) {
	records := []memory.Record{
		{Name: "tabs", Type: memory.TypeUser, Description: "User prefers tabs for indentation", File: "tabs.md"},
		{Name: "auth", Type: memory.TypeProject, Description: "Authentication rewrite is compliance-driven", File: "auth.md"},
	}
	client := &mockClient{responses: []*anthropic.Message{jsonTextResponse(t, "I cannot answer that in JSON.")}}
	got := memory.SelectRelevantMemories(context.Background(), client, "what indentation style do I prefer", records)
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("fallback should select [0] via keyword match, got %v", got)
	}
}

// TestKeywordFallback 验证兜底匹配的命中与不命中。
func TestKeywordFallback(t *testing.T) {
	records := []memory.Record{
		{Name: "tabs", Type: memory.TypeUser, Description: "User prefers tabs for indentation", File: "tabs.md"},
		{Name: "auth", Type: memory.TypeProject, Description: "Authentication rewrite is compliance-driven", File: "auth.md"},
	}
	got := memory.KeywordFallback("what indentation style do I prefer", records)
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("got %v, want [0]", got)
	}
	if got := memory.KeywordFallback("completely unrelated request", records); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", got)
	}
}

// TestRecallMemories 验证召回：选中记录的全文进入 st.RecalledMemories。
func TestRecallMemories(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Write("tabs", "user", "User prefers tabs", "User prefers tabs, not spaces."); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.Write("unrelated", "project", "Deploy runs on Fridays", "Deploy runs on Fridays."); err != nil {
		t.Fatalf("seed: %v", err)
	}

	st := session.NewState()
	st.Memory = s
	st.Ctx = context.Background()
	st.Client = &mockClient{responses: []*anthropic.Message{jsonTextResponse(t, "[0]")}}

	recallMemories(st, "what indentation style do I prefer")
	if !strings.Contains(st.RecalledMemories, "tabs, not spaces") {
		t.Fatalf("recalled memories missing selected body: %q", st.RecalledMemories)
	}
	if strings.Contains(st.RecalledMemories, "Fridays") {
		t.Fatalf("unselected record should not be recalled: %q", st.RecalledMemories)
	}
}

// TestRecallMemoriesEmptyStoreSkipsLLM 空库直接返回，不消耗任何模型调用。
func TestRecallMemoriesEmptyStoreSkipsLLM(t *testing.T) {
	st := session.NewState()
	st.Memory = newTestStore(t)
	st.Ctx = context.Background()
	client := &mockClient{} // 零响应：一旦发起调用即 panic，测试即失败
	st.Client = client

	recallMemories(st, "anything")
	if st.RecalledMemories != "" {
		t.Fatalf("expected empty recall, got %q", st.RecalledMemories)
	}
	if client.calls != 0 {
		t.Fatalf("expected 0 LLM calls, got %d", client.calls)
	}
}

// TestRenderRecalledMemoriesBudget 验证召回预算：装不下的整条跳过并注明，单条超预算截断。
func TestRenderRecalledMemoriesBudget(t *testing.T) {
	mk := func(name, body string) memory.Record {
		return memory.Record{Name: name, Type: memory.TypeUser, Description: "d", Body: body}
	}
	records := []memory.Record{mk("a", strings.Repeat("x", 40)), mk("b", strings.Repeat("y", 40))}

	out := memory.RenderRecalledMemories(records, []int{0, 1}, 60)
	if !strings.Contains(out, "(1 more selected records omitted due to recall budget)") {
		t.Fatalf("expected omission note, got %q", out)
	}

	out = memory.RenderRecalledMemories(records, []int{0}, 10)
	if !strings.Contains(out, "…(truncated)") {
		t.Fatalf("expected truncation marker, got %q", out)
	}
}

// TestShouldStoreMemory 覆盖准入校验：完整性、会话措辞、重复。
func TestShouldStoreMemory(t *testing.T) {
	existing := []memory.Record{
		{Name: "Tabs Preference", Type: memory.TypeUser, Description: "User prefers tabs for indentation"},
	}

	cases := []struct {
		name      string
		candidate memory.Candidate
		want      bool
	}{
		{"valid", memory.Candidate{Name: "new fact", Type: "project", Description: "Some fact", Body: "Body."}, true},
		{"missing body", memory.Candidate{Name: "n", Type: "user", Description: "d", Body: ""}, false},
		{"invalid type", memory.Candidate{Name: "n", Type: "diary", Description: "d", Body: "b"}, false},
		{"duplicate slug", memory.Candidate{Name: "tabs preference", Type: "user", Description: "Other", Body: "b"}, false},
		{"duplicate description", memory.Candidate{Name: "other name", Type: "user", Description: "USER   PREFERS TABS FOR INDENTATION", Body: "b"}, false},
		{"session phrase in body", memory.Candidate{Name: "n", Type: "feedback", Description: "d", Body: "Do not create files in this session."}, false},
		{"session phrase in description", memory.Candidate{Name: "n", Type: "feedback", Description: "本次任务的临时规则", Body: "b"}, false},
	}
	for _, c := range cases {
		if got := memory.ShouldStoreMemory(c.candidate, existing); got != c.want {
			t.Errorf("%s: memory.ShouldStoreMemory = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestExtractMemories 验证轮末抽取：persistent + 过准入才落盘，其余候选全部拒绝。
func TestExtractMemories(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Write("tabs preference", "user", "User prefers tabs for indentation", "Tabs body."); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp := jsonTextResponse(t, `[
		{"name":"auth decision","type":"project","description":"Auth rewrite is compliance-driven","body":"The authentication rewrite is compliance-driven.","scope":"persistent"},
		{"name":"temp dir","type":"project","description":"Use build dir","body":"Use the build directory.","scope":"current_task"},
		{"name":"session rule","type":"feedback","description":"Do not create files","body":"Do not create files in this session.","scope":"persistent"},
		{"name":"tabs preference","type":"user","description":"Another tabs note","body":"Duplicate body.","scope":"persistent"},
		{"name":"incomplete","type":"user","description":"Missing body","body":"","scope":"persistent"}
	]`)
	st := session.NewState()
	st.Memory = s
	st.Ctx = context.Background()
	st.Client = &mockClient{responses: []*anthropic.Message{resp}}

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("I prefer tabs. Remember that.")),
	}

	if !extractMemories(st, messages) {
		t.Fatal("extractMemories should report stored=true")
	}
	records := s.List()
	if len(records) != 2 {
		t.Fatalf("expected 2 records (seeded + 1 admitted), got %d", len(records))
	}
	// list 按文件名排序：auth-decision.md < tabs-preference.md
	if records[0].Name != "auth decision" {
		t.Fatalf("expected 'auth decision' stored, got %+v", records[0])
	}
}

// TestExtractMemoriesFailure 模型失败时静默返回 false，不写任何记录。
func TestExtractMemoriesFailure(t *testing.T) {
	s := newTestStore(t)
	st := session.NewState()
	st.Memory = s
	st.Ctx = context.Background()
	st.Client = failingClient{}

	if extractMemories(st, []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}) {
		t.Fatal("expected false on LLM failure")
	}
	if records := s.List(); len(records) != 0 {
		t.Fatalf("expected no records, got %d", len(records))
	}
}

// TestConsolidateBelowThreshold 未达阈值不发起模型调用。
func TestConsolidateBelowThreshold(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < memory.ConsolidateThreshold-1; i++ {
		if _, err := s.Write(fmt.Sprintf("rec-%02d", i), "project", fmt.Sprintf("desc %d", i), fmt.Sprintf("body %d", i)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	st := session.NewState()
	st.Memory = s
	st.Ctx = context.Background()
	st.Client = &mockClient{} // 零响应：一旦发起调用即 panic

	consolidateMemories(st)
	if records := s.List(); len(records) != memory.ConsolidateThreshold-1 {
		t.Fatalf("records should be untouched, got %d", len(records))
	}
}

// TestConsolidateReplaces 达到阈值后模型清洗列表经校验整体替换，索引同步重建。
func TestConsolidateReplaces(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < memory.ConsolidateThreshold; i++ {
		if _, err := s.Write(fmt.Sprintf("rec-%02d", i), "project", fmt.Sprintf("desc %d", i), fmt.Sprintf("body %d", i)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	resp := jsonTextResponse(t, `[{"name":"merged","type":"project","description":"Merged record","body":"Merged body."}]`)
	st := session.NewState()
	st.Memory = s
	st.Ctx = context.Background()
	st.Client = &mockClient{responses: []*anthropic.Message{resp}}

	consolidateMemories(st)

	records := s.List()
	if len(records) != 1 || records[0].Name != "merged" {
		t.Fatalf("expected consolidated store [merged], got %+v", records)
	}
	indexData, err := os.ReadFile(filepath.Join(s.Dir(), memory.IndexName))
	if err != nil || !strings.Contains(string(indexData), "Merged record") {
		t.Fatalf("index not rebuilt: %v %s", err, indexData)
	}
}

// TestConsolidateRejectsBadResults 空结果与非法记录都不落盘，旧记录原样保留。
func TestConsolidateRejectsBadResults(t *testing.T) {
	for _, tc := range []struct {
		name, response string
	}{
		{"empty result", `[]`},
		{"invalid record", `[{"name":"n","type":"user","description":"d","body":""}]`},
		{"no json", `sorry, I cannot help with that`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			for i := 0; i < memory.ConsolidateThreshold; i++ {
				if _, err := s.Write(fmt.Sprintf("rec-%02d", i), "project", fmt.Sprintf("desc %d", i), fmt.Sprintf("body %d", i)); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			st := session.NewState()
			st.Memory = s
			st.Ctx = context.Background()
			st.Client = &mockClient{responses: []*anthropic.Message{jsonTextResponse(t, tc.response)}}

			consolidateMemories(st)
			if records := s.List(); len(records) != memory.ConsolidateThreshold {
				t.Fatalf("old records must survive, got %d", len(records))
			}
		})
	}
}

// TestMemoryStoreReplaceRollback 替换途中出错时快照回滚，旧记录原样恢复。
func TestMemoryStoreReplaceRollback(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Write("alpha", "project", "Alpha record", "Alpha body."); err != nil {
		t.Fatalf("seed alpha: %v", err)
	}
	if _, err := s.Write("beta", "project", "Beta record", "Beta body."); err != nil {
		t.Fatalf("seed beta: %v", err)
	}

	err := s.Replace([]memory.Candidate{
		{Name: "gamma", Type: "project", Description: "Gamma record", Body: "Gamma body."},
		{Name: "bad", Type: "not-a-type", Description: "Invalid", Body: "Invalid body."},
	})
	if err == nil {
		t.Fatal("expected error from invalid candidate")
	}

	records := s.List()
	if len(records) != 2 {
		t.Fatalf("expected rollback to 2 records, got %d", len(records))
	}
	if records[0].Name != "alpha" || records[0].Body != "Alpha body." ||
		records[1].Name != "beta" || records[1].Body != "Beta body." {
		t.Fatalf("rollback lost original content: %+v", records)
	}
}

// TestExtractJSONArray 验证从自由文本里截取 JSON 数组。
func TestExtractJSONArray(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"```json\n[0, 1]\n```", "[0, 1]", true},
		{"Here you go: [0, 2]. Thanks!", "[0, 2]", true},
		{"[]", "[]", true},
		{"no brackets here", "", false},
		{"unbalanced [0, 1", "", false},
	}
	for _, c := range cases {
		got, ok := memory.ExtractJSONArray(c.in)
		if ok != c.wantOK || got != c.want {
			t.Errorf("memory.ExtractJSONArray(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// TestConversationDigest 验证三类 block 的摘要行与截断。
func TestConversationDigest(t *testing.T) {
	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("remember tabs")),
		{Role: anthropic.MessageParamRoleAssistant, Content: []anthropic.ContentBlockParamUnion{
			{OfToolUse: &anthropic.ToolUseBlockParam{ID: "t1", Name: "bash", Input: json.RawMessage(`{"command":"echo hi"}`)}},
		}},
		{Role: anthropic.MessageParamRoleUser, Content: []anthropic.ContentBlockParamUnion{
			{OfToolResult: &anthropic.ToolResultBlockParam{ToolUseID: "t1", Content: []anthropic.ToolResultBlockParamContentUnion{
				{OfText: &anthropic.TextBlockParam{Text: "hi"}},
			}}},
		}},
	}

	digest := memory.ConversationDigest(messages)
	for _, want := range []string{"user: remember tabs", `assistant [tool_use] bash {"command":"echo hi"}`, "user [tool_result] hi"} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest missing %q, got:\n%s", want, digest)
		}
	}

	long := anthropic.MessageParam{Role: anthropic.MessageParamRoleUser, Content: []anthropic.ContentBlockParamUnion{
		{OfText: &anthropic.TextBlockParam{Text: strings.Repeat("长", memory.MaxDigestLineRunes*2)}},
	}}
	line := memory.ConversationDigest([]anthropic.MessageParam{long})
	if len([]rune(strings.Split(line, "\n")[0])) > memory.MaxDigestLineRunes+3 { // textutil.TruncateRunes 追加 "..."
		t.Errorf("digest line not truncated: %d runes", len([]rune(line)))
	}
}

// TestAgentLoopRecallsAndExtracts loop 级集成：召回进 system prompt，轮末抽取照常执行。
func TestAgentLoopRecallsAndExtracts(t *testing.T) {
	setupLogPaths(t)
	setMemoryEnabled(t, true) // 本测试需要记忆行为
	dir := setupWorkdir(t)

	// 预置 .memory：一条缩进偏好
	s := memory.NewStore(filepath.Join(dir, memory.DirName))
	if _, err := s.Write("tabs", "user", "User prefers tabs", "User prefers tabs, not spaces."); err != nil {
		t.Fatalf("seed: %v", err)
	}

	client := &systemCaptureClient{inner: &mockClient{responses: []*anthropic.Message{
		jsonTextResponse(t, "[0]"), // 召回选择
		textResponse(t),            // 最终答复
		jsonTextResponse(t, "[]"),  // 轮末抽取：无候选
	}}}
	initial := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("what indentation style do I prefer?")),
	}

	if _, err := Run(context.Background(), client, initial, "what indentation style do I prefer?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if client.inner.calls != 3 {
		t.Fatalf("expected 3 Send calls (recall, main, extraction), got %d", client.inner.calls)
	}
	if len(client.systems) != 3 {
		t.Fatalf("expected 3 captured systems, got %d", len(client.systems))
	}
	if !strings.Contains(client.systems[0], "select relevant memory records") {
		t.Fatalf("first call should be memory selection, got: %s", client.systems[0])
	}
	if !strings.Contains(client.systems[1], "Recalled memories from previous sessions") ||
		!strings.Contains(client.systems[1], "tabs, not spaces") {
		t.Fatalf("main system prompt should contain recalled memory, got: %s", client.systems[1])
	}
}

// TestAgentLoopExtractsAtEnd loop 级集成：最终答复后自动抽取并落盘 + 建索引。
func TestAgentLoopExtractsAtEnd(t *testing.T) {
	setupLogPaths(t)
	setMemoryEnabled(t, true)
	dir := setupWorkdir(t)

	client := &mockClient{responses: []*anthropic.Message{
		textResponse(t),
		jsonTextResponse(t, `[{"name":"tabs preference","type":"user","description":"User prefers tabs for indentation","body":"User prefers tabs, not spaces.","scope":"persistent"}]`),
	}}
	initial := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("I prefer using tabs for indentation. Remember that.")),
	}

	if _, err := Run(context.Background(), client, initial, "remember tabs"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	records := memory.NewStore(filepath.Join(dir, memory.DirName)).List()
	if len(records) != 1 || records[0].Name != "tabs preference" {
		t.Fatalf("expected extracted record, got %+v", records)
	}
	if _, err := os.Stat(filepath.Join(dir, memory.DirName, memory.IndexName)); err != nil {
		t.Fatalf("index not created: %v", err)
	}
}

// TestAgentLoopMemoryDisabled 记忆开关关闭时空库不发起任何额外模型调用。
func TestAgentLoopMemoryDisabled(t *testing.T) {
	setupLogPaths(t) // 默认关闭记忆
	setupWorkdir(t)

	client := &mockClient{responses: []*anthropic.Message{textResponse(t)}}
	initial := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("hello")),
	}

	if _, err := Run(context.Background(), client, initial, "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("expected exactly 1 Send call with memory disabled, got %d", client.calls)
	}
}
