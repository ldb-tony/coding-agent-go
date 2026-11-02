# Coding Agent Go - Eino 重构实现指南

> 从零实现一个基于 eino 框架的 Coding Agent

## 如何使用这份指南

这份指南按模块组织，支持你从零开始实现完整的 Coding Agent。

**推荐阅读顺序**:

1. **[00-overview.md](00-overview.md)** — 项目架构总览，理解整体设计
2. **[01-repl.md](01-repl.md)** — 从 REPL 入口开始，搭建应用程序骨架
3. **[02-logger.md](02-logger.md)** — 实现日志系统
4. **[03-llm.md](03-llm.md)** — 集成 LLM 客户端
5. **[04-tools-overview.md](04-tools-overview.md)** — 理解工具层设计
6. **[05-tool-bash.md](05-tool-bash.md)** ~ **[08-tool-mcp.md](08-tool-mcp.md)** — 实现各类工具
7. **[09-agent-core.md](09-agent-core.md)** — 实现 Agent 核心循环
8. **[10-permission.md](10-permission.md)** ~ **[11-hooks.md](11-hooks.md)** — 实现权限和 Hook
9. **[12-memory.md](12-memory.md)** ~ **[15-search.md](15-search.md)** — 实现独立子系统
10. **[16-skill.md](16-skill.md)** ~ **[18-system-prompt.md](18-system-prompt.md)** — 实现 Prompt 相关
11. **[19-integration.md](19-integration.md)** — 端到端集成与测试
12. **[20-agent-teams.md](20-agent-teams.md)** — 多 Agent 协作系统（进阶功能）

## 模块依赖关系

```
                    ┌─────────────┐
                    │ 00-overview │
                    └──────┬──────┘
                           │
              ┌────────────┼────────────┐
              │            │            │
              ▼            ▼            ▼
        ┌──────────┐ ┌──────────┐ ┌──────────┐
        │ 01-repl  │ │ 02-logger│ │ 03-llm   │
        └────┬─────┘ └────┬─────┘ └────┬─────┘
             │            │            │
             └────────────┼────────────┘
                          │
                          ▼
                    ┌──────────┐
                    │ 04-tools │
                    └────┬─────┘
                         │
              ┌──────────┼──────────┐
              │          │          │
              ▼          ▼          ▼
        ┌─────────┐ ┌─────────┐ ┌─────────┐
        │05-bash  │ │06-files │ │07-task  │
        └─────────┘ └─────────┘ └─────────┘
              │          │          │
              └──────────┼──────────┘
                         │
                         ▼
                   ┌──────────┐
                   │09-agent  │
                   └────┬─────┘
                        │
              ┌─────────┼─────────┐
              │         │         │
              ▼         ▼         ▼
        ┌─────────┐ ┌────────┐ ┌─────────┐
        │10-perm  │ │11-hooks│ │12-memory│
        └─────────┘ └────────┘ └─────────┘
                          │
              ┌───────────┼───────────┐
              │           │           │
              ▼           ▼           ▼
        ┌──────────┐ ┌──────────┐ ┌──────────┐
        │16~18     │ │19-integ  │ │20-teams  │
        │prompt    │ │testing   │ │(进阶)    │
        └──────────┘ └──────────┘ └──────────┘
```

## 快速开始

```bash
# 1. 创建项目
mkdir coding-agent-go && cd coding-agent-go

# 2. 初始化 Go 模块
go mod init coding-agent-go

# 3. 添加 eino 依赖
go get github.com/cloudwego/eino@v0.9.22
go get github.com/cloudwego/eino-ext/components/model/claude@v0.1.26

# 4. 按文档顺序实现各模块
# 从 01-repl.md 开始...
```

## 技术栈

| 组件 | 技术选型 |
|------|---------|
| 语言 | Go 1.24+ |
| LLM 框架 | eino v0.9.22 |
| LLM 模型 | Claude (eino-ext/claude) |
| 命令行 | readline |
| 日志 | slog |

## 目录结构

```
coding-agent-go/
├── cmd/agent/
│   └── main.go              # REPL 入口
├── pkg/
│   ├── agent/
│   │   ├── agent.go         # Agent 核心
│   │   ├── config.go        # 配置
│   │   └── llm.go           # LLM 封装
│   ├── tool/
│   │   ├── bash.go          # bash 工具
│   │   ├── task.go          # 子代理工具
│   │   ├── taskstore.go     # 任务图工具
│   │   ├── mcp/             # MCP 工具
│   │   └── registry.go      # 工具注册表
│   ├── memory/              # 跨会话记忆
│   ├── taskstore/           # 持久化任务图
│   ├── background/          # 后台任务
│   ├── permission/          # 权限管线
│   └── search/              # 搜索引擎
├── internal/
│   └── logger/              # 日志
├── skills/                  # 技能目录
├── .memory/                 # 记忆存储
├── .tasks/                  # 任务存储
└── go.mod
```
