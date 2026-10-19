package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"

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
		log.Fatal("MODEL_ID environment variable is required")
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

func systemPrompt() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	var env string
	if runtime.GOOS == "windows" {
		env = "Windows: the bash tool runs through cmd.exe; use cmd.exe syntax, not Unix Bash or PowerShell syntax"
	} else {
		env = "Unix-like: the bash tool runs the system shell"
	}

	return fmt.Sprintf(
		"You are a coding agent at %s. Environment: %s.\n"+
			"For complex multi-step tasks, use todo_write first to plan the steps before executing.\n"+
			"Use the provided tools to solve tasks. Act, don't explain.",
		cwd, env)
}
