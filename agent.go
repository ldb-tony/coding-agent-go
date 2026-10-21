package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

func agentLoop(ctx context.Context, client LLMClient, messages []anthropic.MessageParam, activeRequest string) ([]anthropic.MessageParam, error) {
	// 会话级状态：read_file 的 mtime 去重（及后续 read-before-write 门控）挂在这里。
	st := newSessionState()
	st.client = client // s06: 子代理的嵌套 loop 复用父的 LLM 客户端
	st.ctx = ctx       // s06: 子代理继承父 loop 的 context（超时/取消）
	// s08: 上下文压缩器；每个 agentLoop 一个实例（summarize 要复用 client）
	cpt := newCompactor(workdir, client)
	// s09: 会话开始时召回相关记忆（空库时跳过，不发起额外模型调用）；
	// 召回结果挂在 st 上，systemPrompt 每轮拼进 system。
	st.recallMemories(activeRequest)
	// s05: 连续未使用 todo_write 的轮次数；>=3 时注入 reminder。
	roundsSinceTodo := 0
	// s08: API 返回 prompt_too_long 时的兜底重试次数
	reactiveRetries := 0
	for turn := 0; ; turn++ {
		// s11: 每轮模型调用前收集已完成的后台任务，以 <task_notification> 注入对话。
		// 已完成的任务不会主动唤醒 Agent，只在 loop 转起来时被收集。
		messages = injectBackgroundResults(messages)
		// s08: 每轮模型调用前先跑压缩流水线；只在超预算时才真正动手
		messages = cpt.prepare(ctx, messages, activeRequest)
		// 覆盖写入完整快照：先落用户输入（及历史），收到响应后再加上模型回答。
		logMessages(turn, messages)

		resp, err := client.Send(ctx, systemPrompt(st), messages, allTools(), 8000)
		if err != nil {
			// s08: prompt_too_long 时归档 + 总结 + 保留最近 5 条，最多重试 1 次
			errMsg := strings.ToLower(err.Error())
			tooLong := strings.Contains(errMsg, "prompt_too_long") || strings.Contains(errMsg, "too many tokens")
			if tooLong && reactiveRetries < maxReactiveRetries {
				fmt.Println("[reactive compact]")
				messages = cpt.reactiveCompact(ctx, messages, activeRequest)
				reactiveRetries++
				continue
			}
			return messages, err
		}
		reactiveRetries = 0 // 成功一次后重置，下一次 prompt_too_long 仍能兜底

		messages = append(messages, resp.ToParam())
		// 模型回答（含 tool_use）写入同一份快照。
		logMessages(turn, messages)

		var results []anthropic.ContentBlockParamUnion
		usedTodoThisRound := false
		compactRequested := false
		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}
			if block.Name == "todo_write" {
				usedTodoThisRound = true
			}
			// s08: compact 工具本身不压缩，只打个标记；本批其他工具照常执行，
			// 等整个 batch 的 tool_result 都落进 messages 后再统一 compact_history，
			// 避免产生孤儿 tool_result（没有对应 tool_use 的 API 400）
			if block.Name == "compact" {
				compactRequested = true
			}

			hc := &hookContext{ToolName: block.Name, Input: block.Input}

			// s04：权限校验、调用日志等扩展逻辑从 loop 里移出，改由 PreToolUse hook 决定。
			// hook 返回非 nil 表示拦截本次工具调用，返回值作为 tool_result 回喂模型。
			if blocked := triggerHooks(EventPreToolUse, hc); blocked != nil {
				fmt.Println(*blocked)
				results = append(results, anthropic.NewToolResultBlock(block.ID, *blocked, true))
				continue
			}

			// 唯一的改动点：从硬编码 runBash 变成分发查找。
			// s11: run_in_background 的 bash 调用交给后台 goroutine，本轮先返回带
			// bg_id 的占位结果，让 loop 继续处理后续工具调用与下一轮。
			var output string
			if shouldRunBackground(block.Name, block.Input) {
				output = startBackgroundTask(block.Input)
			} else {
				output = runTool(st, block.Name, block.Input)
			}
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
			// s09: 轮末抽取可复用知识（失败只告警，不影响正常返回）；
			// 有新记录且库存达到阈值时顺带触发合并。
			if stored := st.extractMemories(messages); stored {
				st.consolidateMemories()
			}
			return messages, nil
		}

		// s05: reminder —— 连续 3 轮没有 todo_write 时提醒模型更新计划。
		// 用 text block 注入（不是 tool_result），因为没有对应的 tool_use；
		// Anthropic API 不接受无 tool_use 的 tool_result。
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
		logMessages(turn, messages)

		// s08: 模型请求 compact 时，在整个 batch 的 tool_result 都进 messages 之后再压缩
		if compactRequested {
			messages = cpt.compactHistory(ctx, messages, activeRequest)
		}
	}
}
