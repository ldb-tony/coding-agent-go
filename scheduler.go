package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// maxToolConcurrency 是并发执行工具的上限，防止一次几十个 read 打开过多 fd、
// 内存暴涨、把上下文瞬间打爆。
const maxToolConcurrency = 8

// toolUseWithIndex 包装 tool_use block 与其原始下标，用于分组后仍能按原顺序回填结果。
type toolUseWithIndex struct {
	index int
	block anthropic.ContentBlockUnion
}

// toolGroup 是一组可以并发执行的工具调用。
type toolGroup struct {
	tools      []toolUseWithIndex
	hasUnsafe  bool
	unsafeSpec *toolSpec // 组内唯一的 unsafe 工具（如果有）
}

// executeToolBatch 执行一批 tool_use，支持路径感知并发调度。
// 函数签名设计为可注入 hook 函数，便于父 loop 与子代理复用。
//
// 调度语义：
//   - PreToolUse hook 在派发并发组之前按原顺序串行执行；被拦截的调用不执行，
//     不参与资源冲突判断，但仍产出 tool_result（is_error=true）。
//   - 工具执行在组内并发，组间串行；结果按原始下标写入预分配数组，按原顺序返回。
//   - PostToolUse hook 等组内结果收集完后，按原顺序在主 goroutine 串行执行。
//   - 单个工具报错不取消同组兄弟调用；worker panic 转成 error result。
//   - ctx 取消时：已启动的调用等待完成，未启动的调用产出 "canceled" 结果。
func executeToolBatch(
	ctx context.Context,
	st *sessionState,
	blocks []anthropic.ContentBlockUnion,
	triggerPreHook func(*hookContext) *string,
	triggerPostHook func(*hookContext),
) []anthropic.ContentBlockParamUnion {
	// 提取所有 tool_use，保留原始下标
	var toolUses []toolUseWithIndex
	for i, block := range blocks {
		if block.Type == "tool_use" {
			toolUses = append(toolUses, toolUseWithIndex{index: i, block: block})
		}
	}

	if len(toolUses) == 0 {
		return nil
	}

	// 预分配结果数组，按原始下标写入
	results := make([]anthropic.ContentBlockParamUnion, len(toolUses))
	// 记录每个工具的输出，用于 PostToolUse hook 和终端打印
	outputs := make([]string, len(toolUses))
	// 记录被 hook 拦截的调用，不参与调度
	blocked := make([]bool, len(toolUses))
	// 建立原始 blocks 下标到 toolUses 位置索引的映射
	indexMap := make(map[int]int, len(toolUses))

	// 阶段 1：在主 goroutine 串行执行 PreToolUse hook，拦截的调用标记为 blocked
	for i, tu := range toolUses {
		indexMap[tu.index] = i
		hc := &hookContext{ToolName: tu.block.Name, Input: tu.block.Input}
		if blockedMsg := triggerPreHook(hc); blockedMsg != nil {
			blocked[i] = true
			results[i] = anthropic.NewToolResultBlock(tu.block.ID, *blockedMsg, true)
			outputs[i] = *blockedMsg
		}
	}

	// 阶段 2：贪心分组，将未被拦截的 tool_use 按资源冲突关系分组
	groups := greedyGroup(toolUses, blocked)

	var hasUnsafe bool
	for _, group := range groups {
		if group.hasUnsafe {
			hasUnsafe = true
			break
		}
	}
	appLog.Info("tool batch start",
		"total_tools", len(toolUses),
		"groups", len(groups),
		"has_unsafe", hasUnsafe)

	// 阶段 3：按组执行，组间串行，组内并发
	sem := make(chan struct{}, maxToolConcurrency) // 信号量限制并发上限
	for i, group := range groups {
		_ = i // 保留索引以备将来需要
		executeGroup(ctx, st, group, sem, toolUses, blocked, results, outputs, indexMap)
	}

	// 阶段 4：在主 goroutine 串行执行 PostToolUse hook 和终端打印
	for i, tu := range toolUses {
		if blocked[i] {
			// 被拦截的调用已经产出结果，直接打印
			printToolOutput(outputs[i])
			continue
		}
		// 打印工具输出
		printToolOutput(outputs[i])
		// 执行 PostToolUse hook
		hc := &hookContext{ToolName: tu.block.Name, Input: tu.block.Input, Output: outputs[i]}
		triggerPostHook(hc)
	}

	return results
}

// printToolOutput 打印工具输出（短输出全打，长输出截断到 200 字符）。
func printToolOutput(output string) {
	if runes := []rune(output); len(runes) > 200 {
		fmt.Println(string(runes[:200]))
	} else {
		fmt.Println(output)
	}
}

// greedyGroup 实现贪心分组算法：遍历 tool_use，按资源冲突关系分组。
// 返回的分组列表保证：组内工具可并发，组间必须串行。
func greedyGroup(toolUses []toolUseWithIndex, blocked []bool) []toolGroup {
	var groups []toolGroup
	var current *toolGroup

	for i, tu := range toolUses {
		if blocked[i] {
			continue // 被拦截的调用不参与分组
		}

		spec := getToolSpec(tu.block.Name)
		resources := spec.resources(tu.block.Input)

		if current == nil {
			// 第一组，直接加入
			current = &toolGroup{
				tools:      []toolUseWithIndex{tu},
				hasUnsafe:  !spec.concurrencySafe,
				unsafeSpec: nil,
			}
			if !spec.concurrencySafe {
				current.unsafeSpec = spec
			}
			continue
		}

		// 判断是否与当前组冲突
		if spec.concurrencySafe {
			// safe 工具：若当前组已有 unsafe 且冲突，结束当前组，新开一组
			if current.hasUnsafe && conflictsWithGroup(resources, current, toolUses, blocked) {
				groups = append(groups, *current)
				current = &toolGroup{
					tools:      []toolUseWithIndex{tu},
					hasUnsafe:  false,
					unsafeSpec: nil,
				}
			} else {
				// 无冲突，加入当前组
				current.tools = append(current.tools, tu)
			}
		} else {
			// unsafe 工具：若当前组已有 unsafe，结束当前组
			if current.hasUnsafe {
				groups = append(groups, *current)
				current = &toolGroup{
					tools:      []toolUseWithIndex{tu},
					hasUnsafe:  true,
					unsafeSpec: spec,
				}
			} else {
				// 当前组都是 safe 工具，把冲突的读切出来先作为独立组执行
				conflicting, nonConflicting := splitByConflict(tu, spec, current, toolUses, blocked)
				if len(conflicting) > 0 {
					// 有冲突的读，先 flush 当前组（只保留非冲突的）
					if len(nonConflicting) > 0 {
						current.tools = nonConflicting
						groups = append(groups, *current)
					}
					// 冲突的读作为独立组先执行
					groups = append(groups, toolGroup{
						tools:      conflicting,
						hasUnsafe:  false,
						unsafeSpec: nil,
					})
				}
				// unsafe 工具与剩余无冲突的读合并为新组
				current = &toolGroup{
					tools:      append(nonConflicting, tu),
					hasUnsafe:  true,
					unsafeSpec: spec,
				}
			}
		}
	}

	// flush 最后一组
	if current != nil && len(current.tools) > 0 {
		groups = append(groups, *current)
	}

	return groups
}

// getToolSpec 返回工具的元数据，MCP 工具和未知工具返回 global spec。
func getToolSpec(name string) *toolSpec {
	if spec, ok := toolRegistry[name]; ok {
		return spec
	}
	// MCP 工具和未知工具都是 global
	return &toolSpec{
		handler:         nil,
		concurrencySafe: false,
		resources:       func(_ json.RawMessage) resourceSet { return resourceSet{global: true} },
	}
}

// conflictsWithGroup 判断新工具的资源是否与组内已有工具冲突。
func conflictsWithGroup(resources resourceSet, group *toolGroup, toolUses []toolUseWithIndex, blocked []bool) bool {
	for _, tu := range group.tools {
		spec := getToolSpec(tu.block.Name)
		otherRes := spec.resources(tu.block.Input)
		if conflicts(resources, otherRes) {
			return true
		}
	}
	return false
}

// splitByConflict 将组内工具分为冲突和非冲突两组。
func splitByConflict(unsafeTU toolUseWithIndex, unsafeSpec *toolSpec, group *toolGroup, toolUses []toolUseWithIndex, blocked []bool) (conflicting, nonConflicting []toolUseWithIndex) {
	unsafeRes := unsafeSpec.resources(unsafeTU.block.Input)
	for _, tu := range group.tools {
		spec := getToolSpec(tu.block.Name)
		otherRes := spec.resources(tu.block.Input)
		if conflicts(unsafeRes, otherRes) {
			conflicting = append(conflicting, tu)
		} else {
			nonConflicting = append(nonConflicting, tu)
		}
	}
	return
}

// conflicts 判定两个资源集合是否冲突。
// 冲突规则：
//   - 任一方 global → 冲突
//   - writes ∩ writes ≠ ∅ → 冲突（总是冲突，即使路径不同，避免两个写互相覆盖）
//   - reads ∩ writes ≠ ∅ 或 writes ∩ reads ≠ ∅ → 冲突
//   - glob/grep 的读根目录包含写路径 → 冲突
func conflicts(a, b resourceSet) bool {
	// 任一方 global，总是冲突
	if a.global || b.global {
		return true
	}

	// writes ∩ writes ≠ ∅（总是冲突，即使路径不同）
	if len(a.writes) > 0 && len(b.writes) > 0 {
		return true
	}

	// reads ∩ writes ≠ ∅
	for _, r := range a.reads {
		for _, w := range b.writes {
			if r == w || strings.HasPrefix(r, w+"/") {
				return true
			}
		}
	}

	// writes ∩ reads ≠ ∅
	for _, w := range a.writes {
		for _, r := range b.reads {
			if w == r || strings.HasPrefix(w, r+"/") {
				return true
			}
		}
	}

	return false
}

// executeGroup 执行一组工具，组内并发，使用信号量限制并发上限。
func executeGroup(
	ctx context.Context,
	st *sessionState,
	group toolGroup,
	sem chan struct{},
	toolUses []toolUseWithIndex,
	blocked []bool,
	results []anthropic.ContentBlockParamUnion,
	outputs []string,
	indexMap map[int]int,
) {
	var wg sync.WaitGroup
	for _, tu := range group.tools {
		wg.Add(1)
		go func(tu toolUseWithIndex) {
			defer wg.Done()

			// 获取信号量
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				// ctx 已取消，未启动的调用产出 "canceled" 结果
				pos := indexMap[tu.index]
				results[pos] = anthropic.NewToolResultBlock(tu.block.ID, "Error: canceled", true)
				outputs[pos] = "Error: canceled"
				return
			}

			// 执行工具，recover panic
			output := executeToolWithRecover(ctx, st, tu.block.Name, tu.block.Input)
			pos := indexMap[tu.index]
			outputs[pos] = output
			results[pos] = anthropic.NewToolResultBlock(tu.block.ID, output, false)
		}(tu)
	}
	wg.Wait()
}

// executeToolWithRecover 执行工具调用，recover panic 转成 error result。
func executeToolWithRecover(ctx context.Context, st *sessionState, name string, input json.RawMessage) (output string) {
	defer func() {
		if r := recover(); r != nil {
			output = fmt.Sprintf("Error: tool %s panicked: %v", name, r)
			// 把堆栈写 appLog（如果有的话）
			appLog.Error("tool panicked", "tool", name, "panic", r)
		}
	}()

	// 检查 ctx 是否已取消
	select {
	case <-ctx.Done():
		return "Error: canceled"
	default:
	}

	// 处理 run_in_background 的 bash 调用
	start := time.Now()
	if shouldRunBackground(name, input) {
		output = startBackgroundTask(input)
		appLog.Info("tool exec", "tool", name, "background", true, "ms", time.Since(start).Milliseconds(),
			"input", truncateForLog(string(input)), "output", truncateForLog(output))
	} else {
		// 调用工具
		output = runTool(st, name, input)
		appLog.Info("tool exec", "tool", name, "background", false, "ms", time.Since(start).Milliseconds(),
			"input", truncateForLog(string(input)), "output", truncateForLog(output))
	}
	return output
}
