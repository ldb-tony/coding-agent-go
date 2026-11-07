package perm

import (
	"encoding/json"
	"strings"
	"testing"
)

// stubReadApproval 替换包级 ReadApproval 并记录是否被调用，t.Cleanup 恢复原值。
// 涉及包级变量替换，调用方不得使用 t.Parallel()。
func stubReadApproval(t *testing.T, input string) *bool {
	t.Helper()
	called := new(bool)
	old := ReadApproval
	ReadApproval = func() string {
		*called = true
		return input
	}
	t.Cleanup(func() { ReadApproval = old })
	return called
}

func TestCheckDenyList(t *testing.T) {
	// 命中清单的命令：断言 ok=true 且返回对应 pattern
	hit := []struct {
		command string
		pattern string
	}{
		{"rm -rf /", "rm -rf /"},
		{"rm -rf /tmp/x", "rm -rf /"},
		{"sudo ls", "sudo"},
		{"shutdown now", "shutdown"},
		{"reboot", "reboot"},
		{"mkfs.ext4 /dev/sda", "mkfs"},
		{"dd if=/dev/zero of=x", "dd if="},
		{"echo hi > /dev/null", "> /dev/"},
	}
	for _, tc := range hit {
		t.Run("hit: "+tc.command, func(t *testing.T) {
			pattern, ok := CheckDenyList(tc.command)
			if !ok {
				t.Fatalf("CheckDenyList(%q) ok = false, want true", tc.command)
			}
			if pattern != tc.pattern {
				t.Fatalf("CheckDenyList(%q) pattern = %q, want %q", tc.command, pattern, tc.pattern)
			}
		})
	}

	// 未命中清单的安全命令
	for _, command := range []string{"echo hello", "ls -la"} {
		t.Run("miss: "+command, func(t *testing.T) {
			if pattern, ok := CheckDenyList(command); ok {
				t.Fatalf("CheckDenyList(%q) = (%q, true), want ok=false", command, pattern)
			}
		})
	}
}

func TestContainsDestructiveCommand(t *testing.T) {
	trigger := []string{
		"rm test.txt",
		"del test.txt",
		"DEL test.txt",
		"ls && rm -rf build",
		"chmod 777 x",
		"echo hi > /etc/x",
	}
	for _, command := range trigger {
		t.Run("trigger: "+command, func(t *testing.T) {
			if !ContainsDestructiveCommand(command) {
				t.Fatalf("ContainsDestructiveCommand(%q) = false, want true", command)
			}
		})
	}

	// 单词边界与引号场景：含 rm/del 子串但不在命令位置，不应误报
	safe := []string{
		"model",
		"delimiter",
		"echo del test.txt",
		"echo hello",
		"ls -la",
	}
	for _, command := range safe {
		t.Run("safe: "+command, func(t *testing.T) {
			if ContainsDestructiveCommand(command) {
				t.Fatalf("ContainsDestructiveCommand(%q) = true, want false", command)
			}
		})
	}
}

func TestCheckRules(t *testing.T) {
	t.Run("bash destructive command matches", func(t *testing.T) {
		reason, matched := CheckRules("bash", json.RawMessage(`{"command":"rm test.txt"}`))
		if !matched {
			t.Fatalf("matched = false, want true")
		}
		if reason != "Potentially destructive command" {
			t.Fatalf("reason = %q, want %q", reason, "Potentially destructive command")
		}
	})

	t.Run("bash normal command does not match", func(t *testing.T) {
		if reason, matched := CheckRules("bash", json.RawMessage(`{"command":"echo hello"}`)); matched {
			t.Fatalf("matched = true (reason %q), want false", reason)
		}
	})

	t.Run("non-bash tool has no rules", func(t *testing.T) {
		if reason, matched := CheckRules("read_file", json.RawMessage(`{"path":"/etc/passwd"}`)); matched {
			t.Fatalf("matched = true (reason %q), want false", reason)
		}
	})
}

func TestAskUser(t *testing.T) {
	approve := []string{"y\n", "yes\n", "Y\n", "YES\n"}
	for _, input := range approve {
		t.Run("approve: "+strings.TrimSpace(input), func(t *testing.T) {
			stubReadApproval(t, input)
			if !AskUser("bash", json.RawMessage(`{"command":"rm x"}`), "reason") {
				t.Fatalf("AskUser with input %q = false, want true", input)
			}
		})
	}

	deny := []string{"n\n", "\n", "maybe\n"}
	for _, input := range deny {
		name := strings.TrimSpace(input)
		if name == "" {
			name = "empty"
		}
		t.Run("deny: "+name, func(t *testing.T) {
			stubReadApproval(t, input)
			if AskUser("bash", json.RawMessage(`{"command":"rm x"}`), "reason") {
				t.Fatalf("AskUser with input %q = true, want false", input)
			}
		})
	}
}
