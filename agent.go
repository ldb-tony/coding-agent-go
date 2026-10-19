package main

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

func agentLoop(ctx context.Context, client LLMClient, messages []anthropic.MessageParam) ([]anthropic.MessageParam, error) {
	// 会话级状态：read_file 的 mtime 去重（及后续 read-before-write 门控）挂在这里。
	st := newSessionState()
	for turn := 0; ; turn++ {
		logInput(turn, messages)

		resp, err := client.Send(ctx, systemPrompt(), messages, allTools(), 8000)
		if err != nil {
			return messages, err
		}
		logResponse(turn, resp)

		messages = append(messages, resp.ToParam())

		var results []anthropic.ContentBlockParamUnion
		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}

			hc := &hookContext{ToolName: block.Name, Input: block.Input}

			// s04：权限校验、调用日志等扩展逻辑从 loop 里移出，改由 PreToolUse hook 决定。
			// hook 返回非 nil 表示拦截本次工具调用，返回值作为 tool_result 回喂模型。
			if blocked := triggerHooks(EventPreToolUse, hc); blocked != nil {
				fmt.Println(*blocked)
				results = append(results, anthropic.NewToolResultBlock(block.ID, *blocked, true))
				continue
			}

			// 唯一的改动点：从硬编码 runBash 变成分发查找
			output := runTool(st, block.Name, block.Input)
			if runes := []rune(output); len(runes) > 200 {
				fmt.Println(string(runes[:200]))
			} else {
				fmt.Println(output)
			}

			// s04：执行之后的副作用/输出检查走 PostToolUse（返回值不影响控制流）。
			hc.Output = output
			triggerHooks(EventPostToolUse, hc)

			results = append(results, anthropic.NewToolResultBlock(block.ID, output, false))
		}

		if len(results) == 0 {
			// s04：loop 即将退出时触发 Stop；返回非 nil 则注入消息并继续循环。
			if force := triggerHooks(EventStop, &hookContext{Messages: messages}); force != nil {
				messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(*force)))
				continue
			}
			return messages, nil
		}
		messages = append(messages, anthropic.NewUserMessage(results...))
	}
}
