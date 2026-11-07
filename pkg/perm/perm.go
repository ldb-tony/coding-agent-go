// Package perm 实现三闸门权限管线：硬拒清单 → 规则匹配 → 用户确认。
package perm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Gate 1: 硬性拒绝清单（仅对 bash 生效，简单子串匹配）。
var denyList = []string{"rm -rf /", "sudo", "shutdown", "reboot", "mkfs", "dd if=", "> /dev/"}

// CheckDenyList 返回命中的拒绝模式；未命中 ok=false。
func CheckDenyList(command string) (pattern string, ok bool) {
	for _, p := range denyList {
		if strings.Contains(command, p) {
			return p, true
		}
	}
	return "", false
}

// Gate 2: 破坏性命令检测。RE2 不支持 lookahead，用无 lookahead 的等价改写。
var destructiveCommandWord = regexp.MustCompile(`(?i)(?:^|[;&|()\n])\s*(?:rm|del)(?:\s|$|[;&|()])`)

var destructiveKeywords = []string{"rm ", "> /etc/", "chmod 777"}

// ContainsDestructiveCommand 判断 bash 命令是否潜在破坏性：
// 正则命中（rm/del 出现在命令位置）或含任一关键词子串。
func ContainsDestructiveCommand(command string) bool {
	if destructiveCommandWord.MatchString(command) {
		return true
	}
	for _, kw := range destructiveKeywords {
		if strings.Contains(command, kw) {
			return true
		}
	}
	return false
}

// permissionRule 描述一条权限规则：作用于哪些工具、命中条件、询问原因。
type permissionRule struct {
	tools   []string
	check   func(args json.RawMessage) bool
	message string
}

var permissionRules = []permissionRule{
	{
		tools: []string{"bash"},
		check: func(args json.RawMessage) bool {
			var in struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(args, &in)
			return ContainsDestructiveCommand(in.Command)
		},
		message: "Potentially destructive command",
	},
}

// CheckRules 返回第一条命中规则的 message；未命中 matched=false。
func CheckRules(toolName string, args json.RawMessage) (reason string, matched bool) {
	for _, rule := range permissionRules {
		for _, t := range rule.tools {
			if t == toolName && rule.check(args) {
				return rule.message, true
			}
		}
	}
	return "", false
}

// ReadApproval 读取一行用户确认输入，抽为包级变量便于测试替换。
var ReadApproval = func() string {
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return line
}

// AskUser 打印告警并询问，返回是否放行（Gate 3）。
func AskUser(toolName string, args json.RawMessage, reason string) bool {
	fmt.Printf("\n⚠  %s\n", reason)
	fmt.Printf("   Tool: %s(%s)\n", toolName, strings.TrimSpace(string(args)))
	fmt.Print("   Allow? [y/N] ")
	choice := strings.ToLower(strings.TrimSpace(ReadApproval()))
	return choice == "y" || choice == "yes"
}
