// hooks.go 实现 s04 的 hook 机制：把扩展逻辑挂在 agent loop 的关键节点上，
// 而不是写进 loop 里。四个事件覆盖一次完整的 agent 周期：
//
//	UserPromptSubmit — 用户输入之后、进入 LLM 之前（输入校验、上下文注入）
//	PreToolUse       — 工具执行之前（权限校验、日志）；返回非 nil 则拦截本次调用
//	PostToolUse      — 工具执行之后（副作用、输出检查）；返回值不影响控制流
//	Stop             — loop 即将退出时（清理、决定是否继续）；返回非 nil 则注入消息并继续
//
// 扩展通过 registerHook() 挂载，loop 只调用 triggerHooks()，不直接依赖任何具体检查。
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// HookEvent 是 hook 挂载的生命周期事件名。
type HookEvent string

const (
	EventUserPromptSubmit HookEvent = "UserPromptSubmit"
	EventPreToolUse       HookEvent = "PreToolUse"
	EventPostToolUse      HookEvent = "PostToolUse"
	EventStop             HookEvent = "Stop"
)

// hookContext 在一次触发中携带该事件可用的上下文。hook 只读取自己关心的事件字段，
// 无关事件下对应字段保持零值。
type hookContext struct {
	Query    string                   // EventUserPromptSubmit：用户原始输入
	ToolName string                   // EventPreToolUse / EventPostToolUse：工具名
	Input    json.RawMessage          // EventPreToolUse / EventPostToolUse：工具原始 JSON 参数
	Output   string                   // EventPostToolUse：工具结果文本
	Messages []anthropic.MessageParam // EventStop：即将退出时的完整对话
}

// hookFunc 是 hook 回调的统一签名。返回非 nil 表示"接管本次事件"：
// PreToolUse 返回非 nil 会拦截该次工具调用（返回值作为 tool_result）；
// Stop 返回非 nil 会强制 loop 继续。其余事件的返回值不影响控制流。
type hookFunc func(*hookContext) *string

// hooks 是"事件 → 回调列表"的注册表。
var hooks = map[HookEvent][]hookFunc{
	EventUserPromptSubmit: {},
	EventPreToolUse:       {},
	EventPostToolUse:      {},
	EventStop:             {},
}

// registerHook 把一个回调追加到指定事件的回调列表末尾。
func registerHook(event HookEvent, cb hookFunc) {
	hooks[event] = append(hooks[event], cb)
}

// triggerHooks 按注册顺序触发某事件的所有回调，返回第一个非 nil 的结果并短路；
// 全部返回 nil 时返回 nil。
func triggerHooks(event HookEvent, hc *hookContext) *string {
	for _, cb := range hooks[event] {
		if result := cb(hc); result != nil {
			return result
		}
	}
	return nil
}

func init() { registerBuiltinHooks() }

// registerBuiltinHooks 挂载 s04 参考实现里的五个示例 hook：权限校验（s03 逻辑迁移至此）、
// 调用日志、大输出提醒、上下文注入、退出汇总。抽成函数是为了让测试能重建一套干净注册表。
func registerBuiltinHooks() {
	registerHook(EventUserPromptSubmit, contextInjectHook)
	registerHook(EventPreToolUse, permissionHook)
	registerHook(EventPreToolUse, logHook)
	registerHook(EventPostToolUse, largeOutputHook)
	registerHook(EventStop, summaryHook)
}

// contextInjectHook 是 UserPromptSubmit hook：进入 LLM 前记录当前工作目录。
func contextInjectHook(*hookContext) *string {
	fmt.Printf("\033[90m[HOOK] UserPromptSubmit: working in %s\033[0m\n", workdir)
	return nil
}

// permissionHook 是 PreToolUse hook：s03 的 checkPermission 逻辑迁移到这里。
// 三闸门任意一关拒绝时返回拒绝信息，从而拦截本次工具调用。
func permissionHook(hc *hookContext) *string {
	if checkPermission(hc.ToolName, hc.Input) {
		return nil
	}
	msg := "Permission denied."
	return &msg
}

// logHook 是 PreToolUse hook：记录每一次工具调用（原先写在 loop 里的回显迁移到这里）。
func logHook(hc *hookContext) *string {
	fmt.Printf("\033[90m[HOOK] %s(%s)\033[0m\n", hc.ToolName, previewArgs(hc.Input))
	return nil
}

// largeOutputRunes 是大输出提醒阈值。参考实现用 100000，但本项目的工具本身已把结果
// 截断（bash 50K、read 25K 字符），100000 永远触发不到；这里下调到 40000 让提醒真正可用。
const largeOutputRunes = 40000

// largeOutputHook 是 PostToolUse hook：输出超过阈值时给出提醒，返回值不影响控制流。
func largeOutputHook(hc *hookContext) *string {
	if n := len([]rune(hc.Output)); n > largeOutputRunes {
		fmt.Printf("\033[33m[HOOK] Large output from %s: %d chars\033[0m\n", hc.ToolName, n)
	}
	return nil
}

// summaryHook 是 Stop hook：loop 即将退出时打印本次会话的工具调用次数。
func summaryHook(hc *hookContext) *string {
	fmt.Printf("\033[90m[HOOK] Stop: session used %d tool calls\033[0m\n", countToolResults(hc.Messages))
	return nil
}

// countToolResults 统计对话里 tool_result 块的数量，即已执行的工具调用次数。
func countToolResults(messages []anthropic.MessageParam) int {
	count := 0
	for _, m := range messages {
		for _, b := range m.Content {
			if b.OfToolResult != nil {
				count++
			}
		}
	}
	return count
}

// previewArgs 把工具参数压成单行、最多 60 字符的预览（对齐参考实现的 60 字符截断）。
func previewArgs(input json.RawMessage) string {
	s := strings.TrimSpace(string(input))
	runes := []rune(s)
	if len(runes) > 60 {
		return string(runes[:60]) + "..."
	}
	return s
}
