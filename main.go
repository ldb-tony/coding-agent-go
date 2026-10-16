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
		os.Exit(0)
	}()

	fmt.Println("s01: Agent Loop")
	fmt.Printf("Inputs logged to %s\n", inputLogPath)
	fmt.Printf("Responses logged to %s\n", agentLogPath)
	fmt.Println("Enter a question, press Enter to send. Type q to quit.")
	fmt.Println()

	client := newAnthropicClient()
	var history []anthropic.MessageParam

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\033[36ms01 >> \033[0m")
		if !scanner.Scan() { // EOF (Ctrl-D)
			break
		}
		query := scanner.Text()
		if q := strings.ToLower(strings.TrimSpace(query)); q == "q" || q == "exit" || q == "" {
			break
		}

		history = append(history, anthropic.NewUserMessage(anthropic.NewTextBlock(query)))
		var err error
		history, err = agentLoop(context.Background(), client, history)
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
}
