package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

func agentLoop(ctx context.Context, client LLMClient, messages []anthropic.MessageParam) ([]anthropic.MessageParam, error) {
	for turn := 0; ; turn++ {
		logInput(turn, messages)

		resp, err := client.Send(ctx, systemPrompt(), messages, bashTool(), 8000)
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
			var in struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(block.Input, &in)

			fmt.Printf("\033[33m$ %s\033[0m\n", in.Command)
			output := runBash(in.Command)
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
