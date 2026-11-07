// Package llm 提供 LLM 客户端接口和 Anthropic 实现。
package llm

import (
	"context"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/joho/godotenv"

	"coding-agent-go/pkg/log"
)

// LLMClient sends a conversation to an LLM and returns the response message.
type LLMClient interface {
	Send(ctx context.Context, system string, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam, maxTokens int64) (*anthropic.Message, error)
}

type anthropicClient struct {
	client  anthropic.Client
	modelID string
}

// NewAnthropicClient 从环境变量加载配置并构建 Anthropic 客户端。
func NewAnthropicClient() LLMClient {
	_ = godotenv.Overload()

	modelID := os.Getenv("MODEL_ID")
	if modelID == "" {
		log.AppLog.Error("MODEL_ID environment variable is required")
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
	// system 包含两个 TextBlock：原始 prompt + 百炼客户端标识（激活 web_search/web_fetch 服务端工具）
	systemBlocks := []anthropic.TextBlockParam{
		{Text: system},
		{Text: "x-anthropic-billing-header: cc_entrypoint=cli;"},
	}
	return c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.modelID),
		MaxTokens: maxTokens,
		System:    systemBlocks,
		Messages:  messages,
		Tools:     tools,
	})
}
