# 12 - 跨会话记忆系统规格

## 1. 职责

跨会话记忆系统负责在多次对话之间保留和召回"以后还用得上"的知识：

1. **存储** — 将记忆以独立 Markdown 文件持久化到 `.memory/` 目录
2. **召回** — 在新轮次开始时，根据用户查询检索相关记忆并注入上下文
3. **抽取** — 在轮次结束时，从对话中提取值得长期保留的信息
4. **合并** — 当记忆数量达到阈值时，利用 LLM 清洗、去重、整合记忆

**设计理念**:
- 记忆不是会话备份，只保留"以后还用得上"的知识
- 当前请求与旧记录冲突时，以当前请求为准
- 没有记忆是合法状态 — 系统不依赖记忆正常运行

## 2. 文件位置

```
pkg/memory/
├── store.go         # memoryStore 结构体、list()、frontmatter 解析
├── recall.go        # recall()、选择 prompt 构建、关键词回退
├── extract.go       # extract()、抽取 prompt 构建、准入校验
└── consolidate.go   # consolidate()、合并 prompt、快照回滚
```

运行时数据目录:
```
.memory/
├── MEMORY.md          # 索引文件
├── user-prefs.md      # 记忆记录（YAML frontmatter + body）
├── feedback-testing.md
└── project-arch.md
```

## 3. 依赖

- `gopkg.in/yaml.v3` — YAML frontmatter 解析
- `pkg/agent/` — LLMClient 接口（用于 recall 选择和 extract 抽取）
- `encoding/json` — LLM 交互的 JSON 序列化
- `os` / `path/filepath` — 文件系统操作
- `sort` — 按文件名排序
- `strings` / `unicode/utf8` — 字符串处理和 UTF-8 校验

## 4. 数据结构

### 4.1 memoryType

```go
type memoryType string

const (
    memoryTypeUser      memoryType = "user"      // 持久用户偏好（"缩进用 tab"）
    memoryTypeFeedback  memoryType = "feedback"   // 长期有效指导（"不要 mock 数据库"）
    memoryTypeProject   memoryType = "project"    // 稳定项目事实（"认证重写是合规驱动"）
    memoryTypeReference memoryType = "reference"  // 外部指针/线索（"pipeline 问题在 Linear"）
)
```

### 4.2 memoryRecord

```go
type memoryRecord struct {
    Name        string     // kebab-case slug，如 "user-prefs"
    Type        memoryType // user | feedback | project | reference
    Description string     // one-line summary
    Body        string     // 记忆正文
    File        string     // 文件名，含 .md 后缀，如 "user-prefs.md"
}
```

### 4.3 memoryStore

```go
type memoryStore struct {
    dir string // 记忆目录路径，如 ".memory"
}
```

### 4.4 frontmatter 结构

每个记忆文件的 YAML frontmatter 格式:

```yaml
---
name: kebab-case-slug
description: one-line summary
metadata:
  type: user | feedback | project | reference
---

body content here
```

## 5. 关键常量

```go
const (
    memoryDirName         = ".memory"       // 记忆目录名
    memoryIndexName       = "MEMORY.md"     // 索引文件名
    maxRecallMemories     = 5               // 单次召回最多记忆数
    maxMemoryBodyRunes    = 4000            // 单条记忆正文最大字符数
    maxRecallBodyRunes    = 20000           // 召回结果总字符预算
    consolidateThreshold  = 10              // 触发合并的记忆数量阈值
    selectionMaxTokens    = 1024            // 选择阶段 LLM 最大输出 token
    extractionMaxTokens   = 2000            // 抽取阶段 LLM 最大输出 token
    maxDigestLineRunes    = 400             // 摘要单行最大字符数
    maxExtractionCtxRunes = 30000           // 抽取时对话上下文预算
)
```

## 6. 核心函数

### 6.1 newMemoryStore()

```
函数签名: func newMemoryStore(dir string) *memoryStore

流程:
1. 创建 memoryStore{dir: dir}
2. 返回指针

参数:
- dir: 记忆目录路径，通常为 ".memory"
```

### 6.2 list()

```
函数签名: func (s *memoryStore) list() []memoryRecord

流程:
1. 读取 s.dir 目录下的所有条目
   - 目录不存在 → 返回 nil（无记忆是合法状态，不是错误）
   - 读取失败 → 返回 nil

2. 过滤文件:
   - 跳过 MEMORY.md（索引文件）
   - 只保留 .md 后缀的文件
   - 跳过子目录

3. 逐个解析文件:
   a. 读取文件内容
      - 读取失败 → 跳过该文件
   b. 校验 UTF-8 编码
      - 非 UTF-8 → 跳过该文件
   c. 调用 parseFrontmatter(filename, content) 解析
      - 解析结果填入 memoryRecord

4. 按文件名（File 字段）升序排序

5. 返回 []memoryRecord
```

### 6.3 parseFrontmatter()

```
函数签名: func parseFrontmatter(filename, content string) memoryRecord

流程:
1. 查找第一个 "---" 行
   - 未找到 → 使用降级策略（步骤 6）

2. 查找第二个 "---" 行（在第一个之后）
   - 未找到 → 使用降级策略（步骤 6）

3. 提取两个 "---" 之间的内容作为 YAML

4. 解析 YAML:
   ```yaml
   name: string
   description: string
   metadata:
     type: string
   ```
   - 解析失败 → 使用降级策略（步骤 6）

5. 提取 "---" 之后的剩余内容作为 Body
   - 去除首尾空白

6. 降级策略（frontmatter 解析失败时）:
   - Name: 从文件名去掉 .md 后缀
   - Description: Body 的第一行（截断到 maxDigestLineRunes）
   - Type: memoryTypeProject（默认值）

7. 构建 File 字段: filename（原样使用）

8. 返回 memoryRecord
```

### 6.4 recall()

```
函数签名: func (s *memoryStore) recall(query string) string

流程:
1. 调用 list() 获取所有记忆
   - 无记忆 → 返回空字符串

2. 构建选择 prompt:
   - 列出所有记忆的 name 和 description
   - 格式: "Available memories:\n- {name}: {description}\n..."
   - 要求 LLM 从中选出最多 maxRecallMemories (5) 条与 query 相关的记忆
   - 要求返回 JSON 数组: {"selected": ["name1", "name2"]}

3. 调用 LLM（selectionMaxTokens = 1024）:
   - 成功 → 解析 JSON，获取选中的记忆名称列表
   - 失败 → 回退到关键词匹配（步骤 4）

4. 关键词回退匹配:
   - 将 query 拆分为关键词（按空格和标点）
   - 对每条记忆，检查 name 和 description 是否包含任一关键词（不区分大小写）
   - 按匹配度排序，取前 maxRecallMemories 条

5. 按选中顺序加载记忆全文:
   - 维护总字符预算 maxRecallBodyRunes (20000)
   - 逐条加载，累加 Body 的 rune 数
   - 超出预算时停止加载

6. 格式化输出:
   ```
   ## Memories

   ### {name}
   {body}

   ### {name2}
   {body2}
   ```

7. 返回格式化文本
```

### 6.5 extract()

```
函数签名: func (s *memoryStore) extract(messages []Message) bool

流程:
1. 构建对话摘要:
   - 遍历 messages，提取每条消息的角色和文本内容
   - 控制总字符数在 maxExtractionCtxRunes (30000) 以内
   - 超出时从最早消息开始截断

2. 构建抽取 prompt:
   - 指示 LLM 从对话中提取值得长期记忆的信息
   - 要求返回 JSON 数组:
     ```json
     [
       {
         "name": "kebab-case-slug",
         "type": "user|feedback|project|reference",
         "description": "one-line summary",
         "body": "memory content"
       }
     ]
     ```
   - 强调: 只提取 scope=persistent 的信息（跨会话仍有价值的）
   - 强调: 当前请求与旧记录冲突时，以当前请求为准

3. 调用 LLM（extractionMaxTokens = 2000）:
   - 失败 → 返回 false（无记忆被抽取）
   - 返回空数组 → 返回 false

4. 解析 LLM 返回的 JSON 数组

5. 逐条准入校验:
   a. scope 校验: 必须是 persistent（跨会话有价值的）
      - 临时调试信息、一次性操作 → 跳过
   b. 格式校验:
      - name 非空且为合法 kebab-case
      - type 必须是四种之一
      - body 长度不超过 maxMemoryBodyRunes (4000)
      - description 非空

6. 写入文件:
   - 确保 s.dir 目录存在（os.MkdirAll）
   - 对每条通过校验的记忆:
     a. 构建完整文件内容（frontmatter + body）
     b. 写入 s.dir/{name}.md
     c. 写入失败 → 记录日志，继续处理下一条

7. 更新 MEMORY.md 索引:
   - 调用 rebuildIndex()

8. 返回 true（至少有一条记忆被成功抽取）
```

### 6.6 consolidate()

```
函数签名: func (s *memoryStore) consolidate()

流程:
1. 调用 list() 获取所有记忆
   - 记录数 < consolidateThreshold (10) → 直接返回，不执行合并

2. 构建合并 prompt:
   - 列出所有记忆的完整内容
   - 指示 LLM 执行以下操作:
     * 删除过时或重复的记忆
     * 合并内容相似的记录
     * 修正格式不规范的记录
     * 保留仍有价值的记忆
   - 要求返回 JSON:
     ```json
     {
       "keep": [{"name": "...", "type": "...", "description": "...", "body": "..."}],
       "delete": ["name1", "name2"]
     }
     ```

3. 调用 LLM:
   - 失败 → 直接返回（不执行任何变更）

4. 解析 LLM 返回的 JSON

5. 创建快照（用于回滚）:
   - 深拷贝当前所有 memoryRecord
   - 记录每个文件的原始内容

6. 逐条应用变更:
   a. 删除操作:
      - 删除 s.dir/{name}.md
      - 失败 → 执行回滚（步骤 7）
   b. 保留/修改操作:
      - 重新生成 frontmatter + body
      - 写入 s.dir/{name}.md
      - 失败 → 执行回滚（步骤 7）

7. 回滚（任一步骤失败时）:
   - 从快照恢复所有文件
   - 记录错误日志
   - 返回

8. 重建 MEMORY.md 索引:
   - 调用 rebuildIndex()
```

### 6.7 rebuildIndex()

```
函数签名: func (s *memoryStore) rebuildIndex()

流程:
1. 调用 list() 获取所有记忆

2. 构建索引内容:
   - 每条记忆一行:
     ```
     - [Title](file.md) — one-line hook
     ```
   - Title: 记忆的名称或描述
   - file.md: 记忆文件名
   - hook: description 字段
   - 每行不超过 150 字符
   - 总行数不超过 200 行

3. 写入 s.dir/MEMORY.md（覆盖）
   - 失败 → 记录日志
```

## 7. MEMORY.md 索引格式

```markdown
- [User Preferences](user-prefs.md) — Prefers tab indentation, Go formatting
- [Testing Feedback](feedback-testing.md) — Do not mock database layer
- [Project Architecture](project-arch.md) — Auth rewrite is compliance-driven
```

规则:
- 每条格式: `- [Title](file.md) — one-line hook`
- 每条不超过 150 字符
- 总行数不超过 200 行
- 按文件名排序

## 8. 记忆文件格式

每个记忆文件是标准 Markdown + YAML frontmatter:

```markdown
---
name: user-prefs
description: User prefers tab indentation and Go formatting conventions
metadata:
  type: user
---

- Indentation: tabs (not spaces)
- Go code: gofmt standard
- Commit messages: conventional commits format
```

## 9. 算法细节

### 9.1 关键词回退匹配

当 LLM 选择失败时，使用简单的关键词匹配:

```
func keywordFallback(query string, memories []memoryRecord) []memoryRecord:
  1. keywords = splitAndLowercase(query)
  2. 对每条记忆计算匹配分数:
     score = 0
     for kw in keywords:
       if contains(name, kw):    score += 2
       if contains(description, kw): score += 1
       if contains(body, kw):    score += 0.5
  3. 按 score 降序排序
  4. 取前 maxRecallMemories (5) 条
  5. 返回
```

### 9.2 对话摘要构建

```
func buildDigest(messages []Message, budget int) string:
  1. 从最新消息开始，逐条累加字符数
  2. 每条消息格式化为: "{role}: {content}"
  3. 单条消息超过 maxDigestLineRunes (400) → 截断
  4. 累加超过 budget → 停止
  5. 反转顺序（保持时间正序）
  6. 拼接返回
```

### 9.3 kebab-case 校验

```
func isValidKebabCase(s string) bool:
  - 非空
  - 只包含 [a-z0-9-]
  - 不以 - 开头或结尾
  - 不包含连续 --
```

## 10. 错误处理

| 错误场景 | 处理方式 |
|---------|---------|
| .memory 目录不存在 | list() 返回 nil，不报错 |
| 文件读取失败 | 跳过该文件，继续处理其他文件 |
| 非 UTF-8 文件 | 跳过该文件 |
| YAML 解析失败 | 降级: name 用文件名，description 用首行 |
| LLM 选择失败 | 回退到关键词匹配 |
| LLM 抽取失败 | 返回 false，不写入任何文件 |
| LLM 合并失败 | 不执行任何变更，保持原状 |
| 文件写入失败 | 合并时回滚快照；抽取时记录日志继续 |
| 索引重建失败 | 记录日志，不影响记忆文件本身 |

## 11. 并发安全

- memoryStore 本身无状态（只有 dir 路径），list/read/write 操作是文件级的
- consolidate() 的快照-应用-回滚流程不是原子操作，不应并发调用
- recall() 和 extract() 可在不同 goroutine 调用（只读 + 写入不同文件）
- 建议: 由 Agent 核心串行化调用，避免并发冲突

## 12. 测试要点

### 12.1 单元测试

1. **parseFrontmatter**:
   - 正常 frontmatter 解析
   - 缺少 frontmatter → 降级
   - YAML 格式错误 → 降级
   - 非 UTF-8 内容 → 跳过

2. **list**:
   - 空目录 → nil
   - 目录不存在 → nil
   - 混合 .md 和非 .md 文件 → 只返回 .md
   - 跳过 MEMORY.md
   - 结果按文件名排序

3. **recall**:
   - 无记忆 → 空字符串
   - LLM 选择成功 → 返回格式化文本
   - LLM 选择失败 → 关键词回退
   - 字符预算截断 → 不超过 maxRecallBodyRunes

4. **extract**:
   - LLM 返回空数组 → false
   - LLM 失败 → false
   - 准入校验过滤临时信息
   - 文件写入和索引更新

5. **consolidate**:
   - 记录数 < 10 → 不执行
   - LLM 失败 → 不变更
   - 写入失败 → 回滚快照
   - 成功后索引重建

### 12.2 集成测试

1. **端到端流程**:
   - 抽取 → 召回 → 验证记忆被正确注入
   - 多次抽取 → 合并 → 验证去重效果

2. **边界条件**:
   - 4000 字符正文上限
   - 20000 字符召回预算
   - 10 条合并阈值

3. **文件兼容性**:
   - 手动创建 .memory/ 文件 → list 正确解析
   - 损坏的 YAML → 降级处理正确
