package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

func main() {
	if err := resetLogs(); err != nil {
		fmt.Fprintf(os.Stderr, "reset logs: %v\n", err)
		os.Exit(1)
	}

	// Ctrl-C 任何时候安全退出(对齐参考实现的 KeyboardInterrupt 行为)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		fmt.Println()
		backgroundTasks.stopAll() // s11: 退出前清理仍在运行的后台任务进程组
		os.Exit(0)
	}()

	fmt.Println("s04: Hooks")
	fmt.Printf("Conversation logged to %s\n", messagesLogPath)
	fmt.Println("Enter a question, press Enter to send. Type q to quit.")
	fmt.Println()

	client := newAnthropicClient()
	var history []anthropic.MessageParam

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\033[36ms04 >> \033[0m")
		if !scanner.Scan() { // EOF (Ctrl-D)
			break
		}
		query := scanner.Text()
		if q := strings.ToLower(strings.TrimSpace(query)); q == "q" || q == "exit" || q == "" {
			break
		}

		// s04：用户输入之后、进入 LLM 之前触发 UserPromptSubmit（上下文注入/输入校验）。
		triggerHooks(EventUserPromptSubmit, &hookContext{Query: query})
		history = append(history, anthropic.NewUserMessage(anthropic.NewTextBlock(query)))
		var err error
		// s08: activeRequest 显式传给 loop —— tool_result 也是 role=user，
		// compact 时无法从历史里区分"当前用户请求"与"工具返回"
		history, err = agentLoop(context.Background(), client, history, query)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent loop: %v\n", err)
			continue
		}

		// 打印模型本轮最终的 text 块
		if len(history) > 0 {
			for _, block := range history[len(history)-1].Content {
				if block.OfText != nil {
					fmt.Println(block.OfText.Text)
				}
			}
		}
		fmt.Println()
	}

	backgroundTasks.stopAll() // s11: 正常退出（q/EOF）前清理仍在运行的后台任务进程组
}
