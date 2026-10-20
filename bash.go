package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBashTimeout = 120 * time.Second
	maxBashOutputRunes = 50000
	// cwdSyncMarker 用于轻量 cwd 持久化方案：把 pwd 结果拼在命令输出末尾，
	// 用这个独特 marker 前缀，便于从输出尾部精确提取而不污染正常内容。
	cwdSyncMarker = "__CWD_SYNC_7a3f2b__"
)

// exitCodeSemantics 对"退出码 N 不等于错误"的命令进行语义化：
// 映射表 key = 命令 basename，value = isError 判断函数（exitCode → (isError, note)）。
// 只有返回 isError=false 的情况会用 note 替换标准错误信息。
var exitCodeSemantics = map[string]func(int) (bool, string){
	"grep": exitCodeNoMatch1,
	"egrep": exitCodeNoMatch1,
	"fgrep": exitCodeNoMatch1,
	"rg":    exitCodeNoMatch1,
	"diff":  exitCodeDiffer1,
	"cmp":   exitCodeDiffer1,
	"test":  exitCodeFalse1,
	"[":     exitCodeFalse1,
}

// exitCodeNoMatch1: grep/rg 退出 1 = 无匹配（非错误），≥2 = 错误
func exitCodeNoMatch1(code int) (bool, string) {
	if code == 1 {
		return false, "No matches found"
	}
	return true, ""
}

// exitCodeDiffer1: diff/cmp 退出 1 = 文件不同（非错误），≥2 = 错误
func exitCodeDiffer1(code int) (bool, string) {
	if code == 1 {
		return false, "Files differ"
	}
	return true, ""
}

// exitCodeFalse1: test/[ 退出 1 = 条件为假（非错误），≥2 = 错误
func exitCodeFalse1(code int) (bool, string) {
	if code == 1 {
		return false, "Condition false"
	}
	return true, ""
}

// lastCommandName 从 shell 管道/命令串中提取最后一个命令的 basename。
// 管道分段取最后一段："cat a.txt | grep foo" → "grep"；"ls; diff a b" → "diff"。
// 跳过 FOO=bar 形式的环境变量赋值、timeout N 前缀。
func lastCommandName(command string) string {
	segments := strings.FieldsFunc(command, func(r rune) bool {
		return r == '|' || r == ';' || r == '&'
	})
	last := ""
	for i := len(segments) - 1; i >= 0; i-- {
		seg := strings.TrimSpace(segments[i])
		if seg != "" {
			last = seg
			break
		}
	}
	if last == "" {
		return ""
	}
	fields := strings.Fields(last)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		// 跳过 FOO=bar 赋值：包含 = 且不含空白字符或 $
		if strings.Contains(f, "=") && !strings.ContainsAny(f, " \t$/'\"") {
			continue
		}
		// 跳过 timeout 及其数值参数（timeout 5 ... 或 timeout 30s ...）
		if f == "timeout" && i+1 < len(fields) {
			i++ // 额外跳过紧随其后的时间参数
			continue
		}
		return f
	}
	return ""
}

func runBash(command string) string {
	return runBashWithTimeout(command, defaultBashTimeout)
}

func runBashWithTimeout(command string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := shellCommand(ctx, command)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("Error: Timeout (%ds)", int(timeout.Seconds()))
	}
	if err != nil {
		// 尝试 exit code 语义化
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			name := lastCommandName(command)
			if sem, ok := exitCodeSemantics[name]; ok {
				if isError, note := sem(ee.ExitCode()); !isError {
					return note
				}
			}
			// 非零退出：拼接 exit code + 完整 stdout+stderr（cwd marker 可能在末尾）
			combined := strings.TrimSpace(stdout.String() + stderr.String())
			if combined != "" {
				return fmt.Sprintf("Error: exit status %d\n%s", ee.ExitCode(), combined)
			}
			return fmt.Sprintf("Error: exit status %d", ee.ExitCode())
		}
		return "Error: " + err.Error()
	}

	output := strings.TrimSpace(stdout.String() + stderr.String())
	if output == "" {
		return "(no output)"
	}
	runes := []rune(output)
	if len(runes) > maxBashOutputRunes {
		// 超限：把完整输出落盘，返回截断 + 文件路径
		path, err := saveBashOverflow(output)
		if err != nil {
			return fmt.Sprintf("%s\n\n(Output truncated — failed to save full output: %s)",
				string(runes[:maxBashOutputRunes]), err.Error())
		}
		return fmt.Sprintf("%s\n\n... (output truncated at %d characters). Full output saved to %q — read it with read_file.",
			string(runes[:maxBashOutputRunes]), maxBashOutputRunes, path)
	}
	return output
}

// saveBashOverflow 把超限的完整 bash 输出写到系统临时目录的 agent-bash-output/ 下，
// 返回落盘的绝对路径。目录在 os.TempDir() 下，避免与 workdir 内的 grep/glob 产生噪音。
func saveBashOverflow(content string) (string, error) {
	dir := filepath.Join(os.TempDir(), "agent-bash-output")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "bash-*.log")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// runBashWithCwd 是面向 handler 的 cwd 持久化入口：
//   1) 用 shellCwd 包壳（cd <shellCwd> && ...）
//   2) 在命令末尾附加 pwd marker（同时携带原始退出码）用于同步
//   3) 执行后从输出解析 marker，用原始 command 名做退出码语义化
//   4) 返回 (干净输出, 新 cwd)
//
// shellCwd 为空时：跳过包壳。command 和 echo marker 必须在同一 shell 层。
func runBashWithCwd(command, shellCwd string) (output string, newCwd string) {
	// 原始命令名 —— 用于退出码语义化（包装后 lastCommandName 会拿到 "exit"）
	origCmdName := lastCommandName(command)

	// 包装命令：保存原始退出码，marker 同时携带 pwd 和退出码（空格分隔）
	var wrapped string
	if shellCwd != "" {
		wrapped = fmt.Sprintf("cd %s && %s; __orig_exit=$?; echo %s$(pwd) $__orig_exit; exit $__orig_exit", shellCwd, command, cwdSyncMarker)
	} else {
		wrapped = fmt.Sprintf("%s; __orig_exit=$?; echo %s$(pwd) $__orig_exit; exit $__orig_exit", command, cwdSyncMarker)
	}

	raw := runBashWithTimeout(wrapped, defaultBashTimeout)

	idx := strings.LastIndex(raw, cwdSyncMarker)
	if idx >= 0 {
		beforeMarker := strings.TrimSpace(raw[:idx])
		// afterMarker 格式："<pwd> <exitCode>"（空格分隔）
		afterMarker := strings.TrimSpace(raw[idx+len(cwdSyncMarker):])
		var exitCode int
		if afterMarker != "" {
			parts := strings.Fields(afterMarker)
			if len(parts) >= 2 {
				newCwd = parts[0]
				exitCode, _ = strconv.Atoi(parts[1])
			} else if len(parts) == 1 {
				newCwd = parts[0]
			}
		}

		// 用原始命令名做退出码语义化（runBashWithTimeout 里用的是包装后的名字）
		// 但注意：如果 runBashWithTimeout 因为 timeout 返回错误，beforeMarker 里会有 Timeout 信息
		if exitCode > 0 {
			if sem, ok := exitCodeSemantics[origCmdName]; ok {
				if isError, note := sem(exitCode); !isError {
					// 语义化覆盖 —— 原始命令是 grep 且退出码 1 → "No matches found"
					beforeMarker = note
				}
			}
		}

		if beforeMarker == "" {
			output = "(no output)"
		} else {
			output = beforeMarker
		}
		return output, newCwd
	}

	return raw, shellCwd
}

// shellCommand 构造跨平台的 shell 命令（同步 bash 与 s11 后台任务共用）。
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command)
	}
	return exec.CommandContext(ctx, "sh", "-c", command)
}
