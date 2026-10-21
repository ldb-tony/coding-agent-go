package main

import (
	"encoding/json"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// s06: 子代理（subagent）——给子任务一份全新的 messages[]，只把最终文本带回父对话。
//
// 与参考实现一致：子代理与父同进程、同 WORKDIR，复用同一套工具、权限与生命周期
// hook；它没有 task 工具（本课只允许一层委派）；父只收到它的最终文本，中间的
// 工具调用与结果不会进入父的 messages[]。

const (
	subagentMaxTurns  = 30   // 子代理循环轮数上限（对齐参考实现的 30）
	subagentMaxTokens = 8000 // 子代理每次生成的 max_tokens
)

// taskToolName 是 task 工具名，也是子代理工具集中被排除的那一个。
const taskToolName = "task"

// subSystemPrompt 是子代理自己的 system prompt（与父的 systemPrompt 不同）。
const subSystemPrompt = "You are a subagent working on a single delegated task. " +
	"Use the provided tools to complete it, then reply with a concise final summary " +
	"of what you found or did. Act, don't explain."

// subTools 返回子代理可用的工具：与父相同的工具池，但不含 task（只允许一层委派）。
// 对齐参考实现的 SUB_TOOLS = BASE_TOOLS（去掉 task）。
// s14: 每轮重算，子代理 connect_mcp 后下一轮也能看到新 MCP 工具（与父 loop 行为一致）。
func subTools(st *sessionState) []anthropic.ToolUnionParam {
	all := assembleToolPool(st)
	out := make([]anthropic.ToolUnionParam, 0, len(all))
	for _, t := range all {
		if t.OfTool != nil && t.OfTool.Name == taskToolName {
			continue
		}
		out = append(out, t)
	}
	return out
}

// runSubagent 运行一个带全新 messages[] 的嵌套 agent loop，返回它的最终文本。
// 子代理的每次工具调用走父相同的分发（runTool）与 hook（PreToolUse/PostToolUse），
// 因此文件系统改动对父子双方都可见。
func runSubagent(st *sessionState, prompt string) string {
	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
	}

	for turn := 0; turn < subagentMaxTurns; turn++ {
		tools := subTools(st)
		resp, err := st.client.Send(st.ctx, subSystemPrompt, messages, tools, subagentMaxTokens)
		if err != nil {
			return "Error: subagent failed: " + err.Error()
		}
		messages = append(messages, resp.ToParam())

		var results []anthropic.ContentBlockParamUnion
		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}
			// 与父相同的权限/hook：PreToolUse 返回非 nil 即拦截本次调用。
			hc := &hookContext{ToolName: block.Name, Input: block.Input}
			if blocked := triggerHooks(EventPreToolUse, hc); blocked != nil {
				results = append(results, anthropic.NewToolResultBlock(block.ID, *blocked, true))
				continue
			}
			// s11: 子代理与父共用同一套工具分发，run_in_background 的 bash 同样转后台。
			var output string
			if shouldRunBackground(block.Name, block.Input) {
				output = startBackgroundTask(block.Input)
			} else {
				output = runTool(st, block.Name, block.Input)
			}
			hc.Output = output
			triggerHooks(EventPostToolUse, hc)
			results = append(results, anthropic.NewToolResultBlock(block.ID, output, false))
		}

		// 没有任何 tool_use：本轮就是最终答复，返回文本。
		if len(results) == 0 {
			return subagentText(resp.Content)
		}
		messages = append(messages, anthropic.NewUserMessage(results...))
	}

	return "Subagent stopped after 30 turns without a final answer."
}

// subagentText 拼接响应里全部 text block 作为最终文本；为空时返回 "(no summary)"。
func subagentText(content []anthropic.ContentBlockUnion) string {
	var b strings.Builder
	for _, block := range content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		return s
	}
	return "(no summary)"
}

// taskHandler 是 task 工具的处理函数：解析 prompt，运行子代理并返回其最终文本。
func taskHandler(st *sessionState, args json.RawMessage) string {
	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "Error: invalid arguments: " + err.Error()
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return "Error: prompt is required"
	}
	if st.client == nil || st.ctx == nil {
		return "Error: subagent is unavailable outside the agent loop"
	}
	return runSubagent(st, in.Prompt)
}
