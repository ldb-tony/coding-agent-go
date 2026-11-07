// Package textutil 提供消息文本处理的共享底层函数。
// 位于依赖图最底层（仅依赖 anthropic SDK），供 compact、memory 等包复用，
// 避免为绕开循环依赖而跨包复制实现。
package textutil

import (
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// ToolResultText 提取 tool_result block 的全部 text 内容（拼接多个 text block）。
func ToolResultText(tr *anthropic.ToolResultBlockParam) string {
	var sb strings.Builder
	for _, c := range tr.Content {
		if c.OfText != nil {
			sb.WriteString(c.OfText.Text)
		}
	}
	return sb.String()
}

// TruncateRunes 按 rune 截断到 n（避免截断多字节字符），超出追加 "..."。
func TruncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}
