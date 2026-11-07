package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/chzyer/readline"

	"coding-agent-go/pkg/agent"
	"coding-agent-go/pkg/llm"
	"coding-agent-go/pkg/log"
	"coding-agent-go/pkg/tools/bash"
)

func main() {
	if err := log.InitLogger(); err != nil {
		fmt.Fprintf(os.Stderr, "init logger: %v\n", err)
		os.Exit(1)
	}
	// 异常退出兜底：panic 落日志（含堆栈）再退出，保证 coding-agent-go.log 能定位问题。
	defer func() {
		if r := recover(); r != nil {
			log.AppLog.Error("panic", "err", r, "stack", string(debug.Stack()))
			fmt.Fprintf(os.Stderr, "panic: %v\n", r)
			os.Exit(2)
		}
	}()
	if err := log.ResetLogs(); err != nil {
		log.AppLog.Error("reset logs failed", "err", err)
		fmt.Fprintf(os.Stderr, "reset logs: %v\n", err)
		os.Exit(1)
	}
	log.AppLog.Info("process start", "pid", os.Getpid())

	// rl 提前声明：让 SIGINT handler 闭包能捕获它，在 os.Exit 前 Close
	// 还原终端，避免 raw 模式残留（os.Exit 跳过所有 defer）。
	var rl *readline.Instance

	// Ctrl-C 任何时候安全退出(对齐参考实现的 KeyboardInterrupt 行为)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		log.AppLog.Info("interrupt received, shutting down")
		fmt.Println()
		if rl != nil {
			rl.Close() // 先还原终端，readline 持有 raw 模式
		}
		bash.BackgroundTasks.StopAll() // 退出前清理仍在运行的后台任务进程组
		os.Exit(0)
	}()

	fmt.Println("coding-agent-go — teaching coding agent")
	fmt.Printf("Conversation logged to %s\n", log.MessagesLogPath)
	fmt.Printf("Sub-agent conversations logged to %s\n", log.SubMessagesLogPath)
	fmt.Println("Enter a question, press Enter to send. Type q to quit.")
	fmt.Println()

	client := llm.NewAnthropicClient()
	var history []anthropic.MessageParam

	var err error
	rl, err = readline.NewEx(&readline.Config{
		Prompt:      "\033[36magent >> \033[0m",
		HistoryFile: log.ReadlineHistoryPath,
	})
	if err != nil {
		log.AppLog.Error("readline init failed", "err", err)
		fmt.Fprintf(os.Stderr, "readline init: %v\n", err)
		os.Exit(1)
	}
	defer rl.Close()

	for {
		query, err := rl.Readline()
		if err != nil {
			if err == readline.ErrInterrupt {
				break // Ctrl-C
			}
			if err == io.EOF {
				break // Ctrl-D
			}
			log.AppLog.Error("readline error", "err", err)
			break
		}
		q := strings.ToLower(strings.TrimSpace(query))
		if q == "" {
			continue // 空输入仅重新提示，不退出
		}
		if q == "q" || q == "exit" {
			break
		}

		log.AppLog.Info("user query", "query", query)
		agent.TriggerHooks(agent.EventUserPromptSubmit, &agent.HookContext{Query: query})
		history = append(history, anthropic.NewUserMessage(anthropic.NewTextBlock(query)))
		history, err = agent.Run(context.Background(), client, history, query)
		if err != nil {
			log.AppLog.Error("agent loop failed", "err", err)
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

	log.AppLog.Info("process exit")
	bash.BackgroundTasks.StopAll() // 正常退出（q/EOF）前清理仍在运行的后台任务进程组
}
