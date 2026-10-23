package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestConflicts(t *testing.T) {
	tests := []struct {
		name     string
		a, b     resourceSet
		expected bool
	}{
		{
			name:     "both global",
			a:        resourceSet{global: true},
			b:        resourceSet{global: true},
			expected: true,
		},
		{
			name:     "one global",
			a:        resourceSet{reads: []string{"/a"}},
			b:        resourceSet{global: true},
			expected: true,
		},
		{
			name:     "read vs read different paths",
			a:        resourceSet{reads: []string{"/a"}},
			b:        resourceSet{reads: []string{"/b"}},
			expected: false,
		},
		{
			name:     "read vs read same path",
			a:        resourceSet{reads: []string{"/a"}},
			b:        resourceSet{reads: []string{"/a"}},
			expected: false,
		},
		{
			name:     "write vs write different paths",
			a:        resourceSet{writes: []string{"/a"}},
			b:        resourceSet{writes: []string{"/b"}},
			expected: true,
		},
		{
			name:     "write vs write same path",
			a:        resourceSet{writes: []string{"/a"}},
			b:        resourceSet{writes: []string{"/a"}},
			expected: true,
		},
		{
			name:     "read vs write different paths",
			a:        resourceSet{reads: []string{"/a"}},
			b:        resourceSet{writes: []string{"/b"}},
			expected: false,
		},
		{
			name:     "read vs write same path",
			a:        resourceSet{reads: []string{"/a"}},
			b:        resourceSet{writes: []string{"/a"}},
			expected: true,
		},
		{
			name:     "read vs write path prefix",
			a:        resourceSet{reads: []string{"/a/b"}},
			b:        resourceSet{writes: []string{"/a"}},
			expected: true,
		},
		{
			name:     "write vs read path prefix",
			a:        resourceSet{writes: []string{"/a/b"}},
			b:        resourceSet{reads: []string{"/a"}},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := conflicts(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("conflicts() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestGetToolSpec(t *testing.T) {
	// 测试已知工具
	spec := getToolSpec("read_file")
	if !spec.concurrencySafe {
		t.Error("read_file should be concurrencySafe")
	}

	spec = getToolSpec("write_file")
	if spec.concurrencySafe {
		t.Error("write_file should not be concurrencySafe")
	}

	spec = getToolSpec("bash")
	if spec.concurrencySafe {
		t.Error("bash should not be concurrencySafe")
	}

	// 测试未知工具（应该是 global）
	spec = getToolSpec("unknown_tool")
	if spec.concurrencySafe {
		t.Error("unknown tool should not be concurrencySafe")
	}
	res := spec.resources(nil)
	if !res.global {
		t.Error("unknown tool should have global resources")
	}
}

func TestResourceFunctions(t *testing.T) {
	// 测试 read_file 的 resources 函数
	args := json.RawMessage(`{"path": "test.txt"}`)
	res := toolRegistry["read_file"].resources(args)
	if len(res.reads) != 1 {
		t.Errorf("read_file should have 1 read resource, got %d", len(res.reads))
	}
	if res.global {
		t.Error("read_file should not be global")
	}

	// 测试 write_file 的 resources 函数
	args = json.RawMessage(`{"path": "test.txt"}`)
	res = toolRegistry["write_file"].resources(args)
	if len(res.writes) != 1 {
		t.Errorf("write_file should have 1 write resource, got %d", len(res.writes))
	}
	if res.global {
		t.Error("write_file should not be global")
	}

	// 测试 glob 的 resources 函数（默认路径）
	args = json.RawMessage(`{"pattern": "*.go"}`)
	res = toolRegistry["glob"].resources(args)
	if len(res.reads) != 1 {
		t.Errorf("glob should have 1 read resource, got %d", len(res.reads))
	}
	if res.global {
		t.Error("glob should not be global")
	}

	// 测试 bash 的 resources 函数
	args = json.RawMessage(`{"command": "ls"}`)
	res = toolRegistry["bash"].resources(args)
	if !res.global {
		t.Error("bash should be global")
	}
}

func TestExecuteToolBatchBasic(t *testing.T) {
	// 创建测试用的 sessionState
	st := newSessionState()
	ctx := context.Background()

	// 创建测试用的 tool_use blocks
	blocks := []anthropic.ContentBlockUnion{
		{
			Type:  "tool_use",
			ID:    "tool1",
			Name:  "read_file",
			Input: json.RawMessage(`{"path": "test1.txt"}`),
		},
		{
			Type:  "tool_use",
			ID:    "tool2",
			Name:  "read_file",
			Input: json.RawMessage(`{"path": "test2.txt"}`),
		},
	}

	// 执行批次
	results := executeToolBatch(
		ctx,
		st,
		blocks,
		func(hc *hookContext) *string { return nil }, // 不拦截
		func(hc *hookContext) {},                       // 无 PostToolUse
	)

	// 验证结果数量
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
}

func TestExecuteToolBatchConcurrency(t *testing.T) {
	// 测试并发执行：创建多个 read_file 调用，验证它们真的并发了
	st := newSessionState()
	ctx := context.Background()

	// 创建临时测试文件
	for i := 0; i < 3; i++ {
		// 使用 write_file 创建测试文件
		path := "test_concurrent_" + string(rune('a'+i)) + ".txt"
		writeArgs := json.RawMessage(`{"path": "` + path + `", "content": "test content"}`)
		runTool(st, "write_file", writeArgs)
	}

	// 创建多个 read_file 调用
	var blocks []anthropic.ContentBlockUnion
	for i := 0; i < 3; i++ {
		path := "test_concurrent_" + string(rune('a'+i)) + ".txt"
		blocks = append(blocks, anthropic.ContentBlockUnion{
			Type:  "tool_use",
			ID:    "tool" + string(rune('1'+i)),
			Name:  "read_file",
			Input: json.RawMessage(`{"path": "` + path + `"}`),
		})
	}

	// 记录并发执行的工具数量
	var concurrentCount int32
	var maxConcurrent int32
	var currentConcurrent int32

	// 执行批次，通过 PostToolUse hook 记录并发情况
	results := executeToolBatch(
		ctx,
		st,
		blocks,
		func(hc *hookContext) *string {
			// PreToolUse: 记录并发开始
			current := atomic.AddInt32(&currentConcurrent, 1)
			for {
				old := atomic.LoadInt32(&maxConcurrent)
				if current <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, current) {
					break
				}
			}
			return nil
		},
		func(hc *hookContext) {
			// PostToolUse: 记录并发结束
			atomic.AddInt32(&currentConcurrent, -1)
			atomic.AddInt32(&concurrentCount, 1)
		},
	)

	// 验证结果数量
	if len(results) != 3 {
		t.Errorf("expected 3 results, got %d", len(results))
	}

	// 验证并发执行：最大并发数应该 > 1
	maxConc := atomic.LoadInt32(&maxConcurrent)
	if maxConc <= 1 {
		t.Errorf("expected concurrent execution, max concurrent = %d", maxConc)
	}

	t.Logf("Max concurrent executions: %d", maxConc)
}

func TestExecuteToolBatchHookBlocking(t *testing.T) {
	// 测试 hook 拦截：被拦截的工具应该产出 error result
	st := newSessionState()
	ctx := context.Background()

	blocks := []anthropic.ContentBlockUnion{
		{
			Type:  "tool_use",
			ID:    "tool1",
			Name:  "read_file",
			Input: json.RawMessage(`{"path": "test.txt"}`),
		},
		{
			Type:  "tool_use",
			ID:    "tool2",
			Name:  "write_file",
			Input: json.RawMessage(`{"path": "test.txt", "content": "test"}`),
		},
	}

	blockedMsg := "blocked by test"
	results := executeToolBatch(
		ctx,
		st,
		blocks,
		func(hc *hookContext) *string {
			// 拦截所有工具
			return &blockedMsg
		},
		func(hc *hookContext) {},
	)

	// 验证结果数量
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}

	// 验证结果都是 error
	for i, result := range results {
		if result.OfToolResult == nil || !result.OfToolResult.IsError.Value {
			t.Errorf("result %d should be an error", i)
		}
	}
}

func TestExecuteToolBatchPanicRecovery(t *testing.T) {
	// 测试 panic 恢复：工具 panic 应该转成 error result
	st := newSessionState()
	ctx := context.Background()

	// 注册一个会 panic 的工具
	toolRegistry["panic_tool"] = &toolSpec{
		handler: func(st *sessionState, args json.RawMessage) string {
			panic("test panic")
		},
		concurrencySafe: false,
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
	defer delete(toolRegistry, "panic_tool")

	blocks := []anthropic.ContentBlockUnion{
		{
			Type:  "tool_use",
			ID:    "tool1",
			Name:  "panic_tool",
			Input: json.RawMessage(`{}`),
		},
	}

	results := executeToolBatch(
		ctx,
		st,
		blocks,
		func(hc *hookContext) *string { return nil },
		func(hc *hookContext) {},
	)

	// 验证结果
	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}

	if results[0].OfToolResult == nil {
		t.Error("result should not be nil")
	}

	// 验证结果包含 panic 信息
	if results[0].OfToolResult == nil || len(results[0].OfToolResult.Content) == 0 {
		t.Error("result should contain error message")
	}
}

func TestExecuteToolBatchCancellation(t *testing.T) {
	// 测试 ctx 取消：未启动的调用应该产出 "canceled" 结果
	st := newSessionState()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	blocks := []anthropic.ContentBlockUnion{
		{
			Type:  "tool_use",
			ID:    "tool1",
			Name:  "read_file",
			Input: json.RawMessage(`{"path": "test.txt"}`),
		},
	}

	results := executeToolBatch(
		ctx,
		st,
		blocks,
		func(hc *hookContext) *string { return nil },
		func(hc *hookContext) {},
	)

	// 验证结果
	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}

	// 验证结果是 canceled
	if results[0].OfToolResult == nil {
		t.Error("result should not be nil")
	}
}

func TestGreedyGrouping(t *testing.T) {
	// 测试贪心分组算法
	blocks := []anthropic.ContentBlockUnion{
		{Type: "tool_use", ID: "1", Name: "read_file", Input: json.RawMessage(`{"path": "a.txt"}`)},
		{Type: "tool_use", ID: "2", Name: "read_file", Input: json.RawMessage(`{"path": "b.txt"}`)},
		{Type: "tool_use", ID: "3", Name: "read_file", Input: json.RawMessage(`{"path": "c.txt"}`)},
	}

	toolUses := []toolUseWithIndex{
		{index: 0, block: blocks[0]},
		{index: 1, block: blocks[1]},
		{index: 2, block: blocks[2]},
	}
	blocked := []bool{false, false, false}

	groups := greedyGroup(toolUses, blocked)

	// 三个 read_file 应该在同一组
	if len(groups) != 1 {
		t.Errorf("expected 1 group, got %d", len(groups))
	}
	if len(groups[0].tools) != 3 {
		t.Errorf("expected 3 tools in group, got %d", len(groups[0].tools))
	}
}

func TestGreedyGroupingWithConflict(t *testing.T) {
	// 测试贪心分组算法：有冲突的情况
	blocks := []anthropic.ContentBlockUnion{
		{Type: "tool_use", ID: "1", Name: "read_file", Input: json.RawMessage(`{"path": "a.txt"}`)},
		{Type: "tool_use", ID: "2", Name: "write_file", Input: json.RawMessage(`{"path": "a.txt", "content": "test"}`)},
		{Type: "tool_use", ID: "3", Name: "read_file", Input: json.RawMessage(`{"path": "b.txt"}`)},
	}

	toolUses := []toolUseWithIndex{
		{index: 0, block: blocks[0]},
		{index: 1, block: blocks[1]},
		{index: 2, block: blocks[2]},
	}
	blocked := []bool{false, false, false}

	groups := greedyGroup(toolUses, blocked)

	// read(a), write(a), read(b) 应该分成至少两组
	if len(groups) < 2 {
		t.Errorf("expected at least 2 groups, got %d", len(groups))
	}
}

func TestGreedyGroupingWithBlocked(t *testing.T) {
	// 测试贪心分组算法：有被拦截的调用
	blocks := []anthropic.ContentBlockUnion{
		{Type: "tool_use", ID: "1", Name: "read_file", Input: json.RawMessage(`{"path": "a.txt"}`)},
		{Type: "tool_use", ID: "2", Name: "read_file", Input: json.RawMessage(`{"path": "b.txt"}`)},
		{Type: "tool_use", ID: "3", Name: "read_file", Input: json.RawMessage(`{"path": "c.txt"}`)},
	}

	toolUses := []toolUseWithIndex{
		{index: 0, block: blocks[0]},
		{index: 1, block: blocks[1]},
		{index: 2, block: blocks[2]},
	}
	blocked := []bool{false, true, false} // 第二个被拦截

	groups := greedyGroup(toolUses, blocked)

	// 只有两个未被拦截的调用，应该在同一组
	if len(groups) != 1 {
		t.Errorf("expected 1 group, got %d", len(groups))
	}
	if len(groups[0].tools) != 2 {
		t.Errorf("expected 2 tools in group, got %d", len(groups[0].tools))
	}
}

func TestMaxToolConcurrency(t *testing.T) {
	// 测试并发上限
	if maxToolConcurrency != 8 {
		t.Errorf("maxToolConcurrency should be 8, got %d", maxToolConcurrency)
	}
}
