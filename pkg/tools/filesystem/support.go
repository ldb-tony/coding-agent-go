// filesystem_support.go — 文件系统模块的支撑函数（safePath + glob 匹配 + 常量）。
package filesystem

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// MaxGrepFileSize grep 跳过超过该大小的文件。
	MaxGrepFileSize = 5 * 1024 * 1024
)

// SafePath 把路径解析为绝对路径，并确保它位于 base 目录内。
func SafePath(path, base string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(base, path)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("access denied: path escapes workspace: %s", path)
	}
	return abs, nil
}

// MatchGlob 是对 filepath.Match 的递归扩展：支持 "**" 跨目录匹配。
func MatchGlob(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

// AtomicWriteFile 先写同目录临时文件再 rename，保证目标路径要么旧内容完整、
// 要么新内容完整（WriteFile 直接 O_TRUNC，写一半崩溃会留下损坏文件）。
func AtomicWriteFile(abs string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(abs), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp) // 失败路径清理临时文件
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, abs); err != nil {
		return err
	}
	tmp = "" // rename 成功，取消清理
	return nil
}

func matchSegs(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if matchSegs(pat[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, _ := filepath.Match(pat[0], name[0])
	if !ok {
		return false
	}
	return matchSegs(pat[1:], name[1:])
}
