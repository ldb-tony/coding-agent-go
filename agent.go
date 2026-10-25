package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// commaTokens 将整数格式化为千分位形式，如 12345 -> "12,345"，用于 token 用量展示。
func commaTokens(v int64) string {
	s := strconv.FormatInt(v, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	return sign + b.String()
}

func agentLoop(ctx context.Context, client LLMClient, messages []anthropic.MessageParam, activeRequest string) ([]anthropic.MessageParam, error) {
	// 会话级状态：read_file 的 mtime 去重（及后续 read-before-write 门控）挂在这里。
	st := newSessionState()
	st.client = client // 子代理的嵌套 loop 复用父的 LLM 客户端
	st.ctx = ctx       // 子代理继承父 loop 的 context（超时/取消）
	cpt := newCompactor(workdir, client)
	st.recallMemories(activeRequest)
	roundsSinceTodo := 0
	reactiveRetries := 0
	var totalIn, totalOut int64
	for turn := 0; ; turn++ {
		messages = injectBackgroundResults(messages)
		messages = cpt.prepare(ctx, st, messages, activeRequest)
		// 覆盖写入完整快照：先落用户输入（及历史），收到响应后再加上模型回答。
		logMessages(turn, systemPrompt(st), messages)

		// 打印本轮对话摘要（system prompt + 最后一条 user message）
		logTurnSummary(turn, "request", systemPrompt(st), messages)

		sendStart := time.Now()
		resp, err := client.Send(ctx, systemPrompt(st), messages, assembleToolPool(st), 8000)
		if err != nil {
			appLog.Error("llm send failed", "turn", turn, "ms", time.Since(sendStart).Milliseconds(), "err", err)
			errMsg := strings.ToLower(err.Error())
			tooLong := strings.Contains(errMsg, "prompt_too_long") || strings.Contains(errMsg, "too many tokens")
			if tooLong && reactiveRetries < maxReactiveRetries {
				appLog.Warn("prompt_too_long, reactive compact", "turn", turn, "retry", reactiveRetries+1)
				fmt.Println("[reactive compact]")
				messages = cpt.reactiveCompact(ctx, messages, activeRequest)
				reactiveRetries++
				continue
			}
			return messages, err
		}
		reactiveRetries = 0 // 成功一次后重置，下一次 prompt_too_long 仍能兜底
		toolUses := 0
		for _, block := range resp.Content {
			if block.Type == "tool_use" {
				toolUses++
			}
			// 打印服务端工具调用状态（web_search / web_fetch 由百炼服务端执行）
			if block.Type == "server_tool_use" {
				stu := block.AsServerToolUse()
				var inputDesc string
				if m, ok := stu.Input.(map[string]any); ok {
					if q, ok := m["query"].(string); ok {
						inputDesc = q
					} else if u, ok := m["url"].(string); ok {
						inputDesc = u
					}
				}
				if inputDesc != "" {
					fmt.Printf("[server_tool] %s: %s\n", stu.Name, inputDesc)
				} else {
					fmt.Printf("[server_tool] %s\n", stu.Name)
				}
			}
		}
		appLog.Info("llm response", "turn", turn, "ms", time.Since(sendStart).Milliseconds(),
			"stop_reason", resp.StopReason, "input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens, "tool_uses", toolUses)

		cpt.observeUsage(resp.Usage, len(messages))
		messages = append(messages, resp.ToParam())
		// 模型回答（含 tool_use）写入同一份快照。
		logMessages(turn, systemPrompt(st), messages)

		// 打印模型回答摘要
		logModelResponse(turn, resp)

		// 每轮模型返回后在 stdout 打印 token 用量（当轮 + 会话累计），与 slog 记录一致。
		totalIn += resp.Usage.InputTokens
		totalOut += resp.Usage.OutputTokens
		fmt.Printf("[tokens] in=%s out=%s cache_create=%s cache_read=%s | Σ in=%s out=%s\n",
			commaTokens(resp.Usage.InputTokens), commaTokens(resp.Usage.OutputTokens),
			commaTokens(resp.Usage.CacheCreationInputTokens), commaTokens(resp.Usage.CacheReadInputTokens),
			commaTokens(totalIn), commaTokens(totalOut))

		var results []anthropic.ContentBlockParamUnion
		usedTodoThisRound := false
		compactRequested := false
		// 统计 usedTodoThisRound 和 compactRequested（在调度前完成）
		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}
			if block.Name == "todo_write" {
				usedTodoThisRound = true
			}
			if block.Name == "compact" {
				compactRequested = true
			}
		}

		// 使用 executeToolBatch 执行工具批次，支持路径感知并发调度
		results = executeToolBatch(
			ctx,
			st,
			resp.Content,
			func(hc *hookContext) *string {
				// PreToolUse hook：权限校验、调用日志等
				if blocked := triggerHooks(EventPreToolUse, hc); blocked != nil {
					appLog.Warn("tool blocked by hook", "turn", turn, "tool", hc.ToolName, "reason", *blocked)
					return blocked
				}
				return nil
			},
			func(hc *hookContext) {
				// PostToolUse hook：执行之后的副作用/输出检查
				triggerHooks(EventPostToolUse, hc)
			},
		)

		if len(results) == 0 {
			if force := triggerHooks(EventStop, &hookContext{Messages: messages}); force != nil {
				appLog.Info("stop hook injected message, loop continues", "turn", turn)
				messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(*force)))
				continue
			}
			appLog.Info("agent loop done", "turns", turn+1)
			if stored := st.extractMemories(messages); stored {
				st.consolidateMemories()
			}
			return messages, nil
		}

		if usedTodoThisRound {
			roundsSinceTodo = 0
		} else {
			roundsSinceTodo++
			if roundsSinceTodo >= 3 {
				results = append(results, anthropic.NewTextBlock(
					"<reminder>Consider using todo_write to update your task plan.</reminder>"))
				roundsSinceTodo = 0
			}
		}

		messages = append(messages, anthropic.NewUserMessage(results...))
		// tool_result 写入后再次覆盖，保证快照始终是最完整的当前对话。
		logMessages(turn, systemPrompt(st), messages)

		if compactRequested {
			out, err := cpt.compactHistory(ctx, st, messages, activeRequest)
			if err != nil {
				appLog.Error("compact failed", "err", err)
				fmt.Printf("[compact failed: %v]\n", err)
			} else {
				messages = out
			}
		}
	}
}
