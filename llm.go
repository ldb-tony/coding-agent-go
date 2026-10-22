package main

import (
	"context"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/joho/godotenv"
)

// LLMClient sends a conversation to an LLM and returns the response message.
type LLMClient interface {
	Send(ctx context.Context, system string, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam, maxTokens int64) (*anthropic.Message, error)
}

type anthropicClient struct {
	client  anthropic.Client
	modelID string
}

func newAnthropicClient() LLMClient {
	_ = godotenv.Overload()

	modelID := os.Getenv("MODEL_ID")
	if modelID == "" {
		appLog.Error("MODEL_ID environment variable is required")
		fmt.Fprintln(os.Stderr, "MODEL_ID environment variable is required")
		os.Exit(1)
	}

	baseURL := os.Getenv("ANTHROPIC_BASE_URL")
	if baseURL != "" {
		os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	}

	opts := []option.RequestOption{}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}

	return &anthropicClient{
		client:  anthropic.NewClient(opts...),
		modelID: modelID,
	}
}

func (c *anthropicClient) Send(ctx context.Context, system string, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam, maxTokens int64) (*anthropic.Message, error) {
	return c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.modelID),
		MaxTokens: maxTokens,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages:  messages,
		Tools:     tools,
	})
}
