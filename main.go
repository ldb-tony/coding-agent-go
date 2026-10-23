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
)

func main() {
	if err := initLogger(); err != nil {
		fmt.Fprintf(os.Stderr, "init logger: %v\n", err)
		os.Exit(1)
	}
	// 异常退出兜底：panic 落日志（含堆栈）再退出，保证 coding-agent-go.log 能定位问题。
	defer func() {
		if r := recover(); r != nil {
			appLog.Error("panic", "err", r, "stack", string(debug.Stack()))
			fmt.Fprintf(os.Stderr, "panic: %v\n", r)
			os.Exit(2)
		}
	}()
	if err := resetLogs(); err != nil {
		appLog.Error("reset logs failed", "err", err)
		fmt.Fprintf(os.Stderr, "reset logs: %v\n", err)
		os.Exit(1)
	}
	appLog.Info("process start", "pid", os.Getpid())

	// rl 提前声明：让 SIGINT handler 闭包能捕获它，在 os.Exit 前 Close
	// 还原终端，避免 raw 模式残留（os.Exit 跳过所有 defer）。
	var rl *readline.Instance

	// Ctrl-C 任何时候安全退出(对齐参考实现的 KeyboardInterrupt 行为)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		appLog.Info("interrupt received, shutting down")
		fmt.Println()
		if rl != nil {
			rl.Close() // 先还原终端，readline 持有 raw 模式
		}
		backgroundTasks.stopAll() // s11: 退出前清理仍在运行的后台任务进程组
		os.Exit(0)
	}()

	fmt.Println("coding-agent-go — teaching coding agent")
	fmt.Printf("Conversation logged to %s\n", messagesLogPath)
	fmt.Println("Enter a question, press Enter to send. Type q to quit.")
	fmt.Println()

	client := newAnthropicClient()
	var history []anthropic.MessageParam

	// s13+: 使用 readline 替代 bufio.Scanner，进入 raw 模式自行处理行编辑。
	// readline 按 rune 计算宽度并重绘，从根本上解决 macOS canonical 模式
	// 下 UTF-8 宽字符退格擦除不彻底的鬼影问题；附带获得命令历史和光标编辑能力。
	var err error
	rl, err = readline.NewEx(&readline.Config{
		Prompt:      "\033[36magent >> \033[0m",
		HistoryFile: readlineHistoryPath,
	})
	if err != nil {
		appLog.Error("readline init failed", "err", err)
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
			appLog.Error("readline error", "err", err)
			break
		}
		q := strings.ToLower(strings.TrimSpace(query))
		if q == "" {
			continue // 空输入仅重新提示，不退出
		}
		if q == "q" || q == "exit" {
			break
		}

		appLog.Info("user query", "query", query)
		// s04：用户输入之后、进入 LLM 之前触发 UserPromptSubmit（上下文注入/输入校验）。
		triggerHooks(EventUserPromptSubmit, &hookContext{Query: query})
		history = append(history, anthropic.NewUserMessage(anthropic.NewTextBlock(query)))
		// s08: activeRequest 显式传给 loop —— tool_result 也是 role=user，
		// compact 时无法从历史里区分"当前用户请求"与"工具返回"
		history, err = agentLoop(context.Background(), client, history, query)
		if err != nil {
			appLog.Error("agent loop failed", "err", err)
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

	appLog.Info("process exit")
	backgroundTasks.stopAll() // s11: 正常退出（q/EOF）前清理仍在运行的后台任务进程组
}
