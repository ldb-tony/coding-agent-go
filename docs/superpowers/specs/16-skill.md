# 16 - 技能系统规格

## 1. 职责

技能系统负责按需加载和管理 SKILL.md 指令集，包括：

1. 扫描技能目录，发现所有可用技能
2. 解析 SKILL.md 的 frontmatter 元数据和正文内容
3. 生成技能目录摘要，注入 system prompt
4. 按需加载技能全文，供 agent 运行时使用
5. 跟踪已加载技能，支持 compact 时重建附件

## 2. 设计理念

- **技能是按需加载的指令集**，存储在 SKILL.md 文件中
- **system prompt 只放技能目录**（name + description），全文用时才取
- **启动时扫描一次**，之后只查内存注册表，不做磁盘 I/O
- 技能文件通过 frontmatter 声明元数据，正文为 markdown 格式

## 3. 文件位置

```
pkg/agent/skill.go          # skillLoader 实现
skills/                     # 技能目录（项目根目录下）
├── brainstorming/
│   └── SKILL.md
├── tdd/
│   └── SKILL.md
└── debugging/
    └── SKILL.md
```

### 3.1 eino 替代方案

使用 `eino adk/middlewares/skill` 可替代手写实现，配置 `SkillsDir` 后 middleware 自动处理 scan/load/catalog。

## 4. 依赖

- `path/filepath` — 目录遍历和符号链接解析
- `os` — 文件读取和 Stat
- `strings` — 字符串处理和空白折叠
- `gopkg.in/yaml.v3` — frontmatter YAML 解析
- `sort` — 目录排序
- `fmt` — 错误消息格式化

## 5. 数据结构

### 5.1 skill

```go
type skill struct {
    Name        string  // 技能名称，用于目录显示和 load 查找
    Description string  // 单行描述，折叠空白后用于 system prompt
    Content     string  // 完整 SKILL.md 内容（含 frontmatter）
}
```

### 5.2 skillLoader

```go
type skillLoader struct {
    skillsDir string            // 技能根目录路径
    skills    map[string]skill  // name → skill 注册表
    loaded    []string          // 按首次 load 顺序记录（去重）
}
```

### 5.3 SKILL.md 格式

```yaml
---
name: skill-name
description: One-line description
---

Skill content here (markdown body)
```

- `name` 字段可选，缺失时回退为目录名
- `description` 字段可选，缺失时回退为 body 首个非空行
- frontmatter 与 body 之间以 `---` 分隔

## 6. 核心函数

### 6.1 newSkillLoader()

```
函数签名: func newSkillLoader(skillsDir string) *skillLoader

流程:
1. 创建 skillLoader 实例
   - skillsDir = skillsDir
   - skills = make(map[string]skill)
   - loaded = nil（空切片）

2. 返回 &skillLoader{skillsDir, skills, loaded}
```

### 6.2 scan()

```
函数签名: func (sl *skillLoader) scan() error

流程:
1. 解析符号链接
   - resolved, err := filepath.EvalSymlinks(sl.skillsDir)
   - 错误处理:
     * os.IsNotExist(err) → 空注册表，返回 nil（合法状态）
     * 其他错误 → 返回 err

2. 查找所有 SKILL.md
   - pattern := resolved + "/*/SKILL.md"
   - manifests, err := filepath.Glob(pattern)
   - 错误处理: err != nil → 返回 err
   - 无匹配 → 空注册表，返回 nil

3. 遍历每个 manifest 文件:
   for _, manifestPath := range manifests:

   a. 解析真实路径
      - realPath, err := filepath.EvalSymlinks(manifestPath)
      - 错误 → 跳过该文件，记录日志

   b. 逃逸校验
      - rel, err := filepath.Rel(resolved, realPath)
      - 如果 strings.HasPrefix(rel, "..") → 跳过（符号链接指向目录外）

   c. 文件类型检查
      - info, err := os.Stat(realPath)
      - 如果 !info.Mode().IsRegular() → 跳过

   d. 读取文件内容
      - content, err := os.ReadFile(realPath)
      - 如果 err != nil → 跳过
      - 验证 UTF-8: 如果 !utf8.Valid(content) → 跳过

   e. 解析 frontmatter
      - metadata, body := parseFrontmatter(string(content))

   f. 确定技能名称
      - name := metadata["name"]
      - 如果 name 为空 → 回退为目录名:
        * dirName := filepath.Base(filepath.Dir(realPath))
        * name = dirName

   g. 确定技能描述
      - description := metadata["description"]
      - 如果 description 为空 → 回退为 body 首非空行:
        * for _, line := range strings.Split(body, "\n"):
            trimmed := strings.TrimSpace(line)
            if trimmed != "" { description = trimmed; break }

   h. 折叠空白
      - description = strings.Join(strings.Fields(description), " ")

   i. 注册到 map
      - sl.skills[name] = skill{
            Name:        name,
            Description: description,
            Content:     string(content),
        }

4. 返回 nil
```

### 6.3 catalog()

```
函数签名: func (sl *skillLoader) catalog() string

流程:
1. 检查注册表是否为空
   - if len(sl.skills) == 0 → return "none"

2. 收集所有技能名称
   - names := make([]string, 0, len(sl.skills))
   - for name := range sl.skills { names = append(names, name) }

3. 按名称排序
   - sort.Strings(names)

4. 生成目录文本
   - var lines []string
   - for _, name := range names:
       s := sl.skills[name]
       lines = append(lines, fmt.Sprintf("- %s: %s", name, s.Description))

5. 返回拼接结果
   - return strings.Join(lines, "\n")

输出示例:
- brainstorming: Explore ideas before implementation
- debugging: Systematic debugging workflow
- tdd: Test-driven development process
```

### 6.4 load()

```
函数签名: func (sl *skillLoader) load(name string) (string, error)

流程:
1. 查找技能
   - s, ok := sl.skills[name]
   - if !ok → return "", fmt.Errorf("skill %q not found", name)

2. 记录加载顺序（去重）
   - found := false
   - for _, n := range sl.loaded:
       if n == name { found = true; break }
   - if !found → sl.loaded = append(sl.loaded, name)

3. 返回完整内容
   - return s.Content, nil
```

### 6.5 loadedSkills()

```
函数签名: func (sl *skillLoader) loadedSkills() []skill

流程:
1. 按 loaded 顺序收集已加载技能
   - result := make([]skill, 0, len(sl.loaded))
   - for _, name := range sl.loaded:
       if s, ok := sl.skills[name]; ok:
           result = append(result, s)

2. 返回结果
   - return result
```

### 6.6 parseFrontmatter()

```
函数签名: func parseFrontmatter(content string) (map[string]string, string)

流程:
1. 查找第一个 "---\n"
   - idx1 := strings.Index(content, "---\n")
   - if idx1 == -1 → return empty map, entire content as body

2. 查找第二个 "---\n"（从第一个之后开始）
   - searchFrom := idx1 + 4  // len("---\n")
   - idx2 := strings.Index(content[searchFrom:], "---\n")
   - if idx2 == -1 → return empty map, entire content as body
   - idx2 += searchFrom  // 调整偏移

3. 提取 YAML 部分
   - yamlContent := content[idx1+4 : idx2]

4. 提取 body 部分
   - bodyStart := idx2 + 4  // 跳过第二个 "---\n"
   - body := strings.TrimSpace(content[bodyStart:])

5. 解析 YAML
   - var metadata map[string]string
   - err := yaml.Unmarshal([]byte(yamlContent), &metadata)
   - if err != nil → return empty map, entire content as body

6. 返回 (metadata, body)

错误处理:
- 无 frontmatter → 空 map + 全文作为 body
- YAML 解析失败 → 空 map + 全文作为 body
- 不中断扫描流程
```

## 7. 算法细节

### 7.1 符号链接安全

scan() 中的逃逸校验确保技能目录不会被符号链接攻击利用：

```
攻击场景:
  skills/evil -> /etc/passwd/SKILL.md

防御:
  1. EvalSymlinks 解析真实路径
  2. filepath.Rel 计算相对路径
  3. 如果以 ".." 开头 → 跳过（指向目录外）
```

### 7.2 名称回退策略

```
优先级:
1. frontmatter 中的 name 字段
2. SKILL.md 所在目录名

示例:
  skills/my-skill/SKILL.md  →  name: "my-skill"
  skills/tdd/SKILL.md (with name: "test-driven-dev")  →  name: "test-driven-dev"
```

### 7.3 描述回退策略

```
优先级:
1. frontmatter 中的 description 字段
2. body 中首个非空行（去除前后空白）
3. 空字符串（如果 body 全为空）

空白折叠:
  "  Explore   ideas\n  before   implementation  "
  → "Explore ideas before implementation"
```

### 7.4 加载顺序跟踪

- loaded 切片按首次 load 调用顺序记录
- 重复 load 同一技能不追加（去重）
- loadedSkills() 按此顺序返回，用于 compact 时重建附件

## 8. 关键常量

```go
// 无外部可配置常量，所有行为由文件结构决定
// SKILL.md 文件名硬编码
const skillManifestName = "SKILL.md"

// frontmatter 分隔符
const frontmatterDelimiter = "---\n"

// 目录匹配模式
const skillGlobPattern = "/*/SKILL.md"
```

## 9. eino middleware 集成

### 9.1 使用 eino adk/middlewares/skill

```go
import "github.com/cloudwego/eino/adk/middlewares/skill"

// 配置 middleware
skillMiddleware := skill.NewMiddleware(&skill.Config{
    SkillsDir: "skills/",
})

// middleware 自动处理:
// - 启动时调用 scan()
// - system prompt 注入 catalog()
// - 按需 load() 技能全文
// - compact 时通过 loadedSkills() 重建附件
```

### 9.2 与手写实现的对应关系

| eino middleware 行为 | 手写实现对应方法 |
|---------------------|----------------|
| 启动扫描 | `scan()` |
| 目录注入 | `catalog()` |
| 按需加载 | `load(name)` |
| 已加载跟踪 | `loadedSkills()` |

## 10. 错误处理

| 错误场景 | 处理方式 | 是否中断 |
|---------|---------|---------|
| skillsDir 不存在 | 空注册表，catalog() 返回 "none" | 否 |
| 符号链接解析失败 | 跳过该文件，记录日志 | 否 |
| 符号链接逃逸 | 跳过该文件 | 否 |
| 非常规文件 | 跳过该文件 | 否 |
| 文件读取失败 | 跳过该文件，记录日志 | 否 |
| 非 UTF-8 内容 | 跳过该文件，记录日志 | 否 |
| YAML 解析失败 | 空 metadata，全文作为 body | 否 |
| load 不存在的技能 | 返回 error | 是（调用方处理）|

## 11. 测试要点

### 11.1 单元测试

1. **scan 正常流程**:
   - 创建临时目录，写入多个 SKILL.md
   - 验证所有技能正确注册
   - 验证 name/description 回退逻辑

2. **scan 符号链接**:
   - 创建指向目录外的符号链接
   - 验证逃逸文件被跳过
   - 验证目录内符号链接正常解析

3. **scan 目录不存在**:
   - 传入不存在的路径
   - 验证返回空注册表，无错误

4. **scan 空目录**:
   - 创建无 SKILL.md 的目录
   - 验证返回空注册表

5. **catalog 排序**:
   - 注册多个技能（乱序）
   - 验证输出按 name 字母排序

6. **catalog 空注册表**:
   - 验证返回 "none"

7. **load 正常流程**:
   - 加载已注册技能
   - 验证返回完整 Content
   - 验证 loaded 列表更新

8. **load 重复加载**:
   - 同一技能 load 两次
   - 验证 loaded 列表无重复

9. **load 不存在的技能**:
   - 验证返回 error

10. **loadedSkills 顺序**:
    - 按 A, B, C 顺序 load
    - 验证返回顺序为 A, B, C

11. **parseFrontmatter 完整**:
    - 标准 YAML + body
    - 验证 metadata 和 body 正确分离

12. **parseFrontmatter 无 frontmatter**:
    - 纯 markdown 内容
    - 验证返回空 map + 全文 body

13. **parseFrontmatter YAML 错误**:
    - 非法 YAML 语法
    - 验证返回空 map + 全文 body

14. **description 空白折叠**:
    - 多空格、换行、前后空白
    - 验证折叠为单空格分隔

### 11.2 集成测试

1. **端到端扫描**:
   - 创建完整 skills/ 目录结构
   - 调用 scan → catalog → load 全流程
   - 验证输出一致性

2. **compact 附件重建**:
   - load 多个技能后触发 compact
   - 验证 loadedSkills() 返回正确的附件列表

## 12. 注意事项

1. **扫描时机**: 只在启动时调用一次 scan()，后续操作全在内存中完成
2. **并发安全**: 当前设计为单线程使用（启动时 scan，运行时 load），无需加锁
3. **内容缓存**: Content 字段保存完整文件内容，包括 frontmatter，避免重复读取
4. **技能命名冲突**: 如果多个目录使用相同 name，后者覆盖前者（map 语义）
5. **文件大小限制**: 当前无单文件大小限制，大技能文件会占用较多内存
