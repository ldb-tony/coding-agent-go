package main

import (
	"context"
	"encoding/json"
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

			// 终端回显：bash 只显示命令，其他工具显示参数 JSON
			var in struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(block.Input, &in)
			label := in.Command
			if label == "" {
				label = string(block.Input)
			}
			fmt.Printf("\033[33m$ %s %s\033[0m\n", block.Name, label)

			if !checkPermission(block.Name, block.Input) {
				fmt.Println("Permission denied.")
				results = append(results, anthropic.NewToolResultBlock(block.ID, "Permission denied.", true))
				continue
			}

			// 唯一的改动点：从硬编码 runBash 变成分发查找
			output := runTool(st, block.Name, block.Input)
			if runes := []rune(output); len(runes) > 200 {
				fmt.Println(string(runes[:200]))
			} else {
				fmt.Println(output)
			}

			results = append(results, anthropic.NewToolResultBlock(block.ID, output, false))
		}

		if len(results) == 0 {
			return messages, nil
		}
		messages = append(messages, anthropic.NewUserMessage(results...))
	}
}
