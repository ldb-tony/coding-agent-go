# 任务系统使用文档（s10）

任务系统把 s05 的"执行清单"（todo_write）升级为**可恢复的任务图**：每条任务是一条独立记录，落盘在 `.tasks/` 目录，跨会话可恢复；`blockedBy` 记录依赖关系，`owner` 记录认领者。实现位于 [taskstore.go](../taskstore.go)，工具注册位于 [tools.go](../tools.go)。

> 注意：本系统与 s06 的 `task` 工具（子代理委派）无关。`task` 工具用于把子任务交给一个全新上下文的子代理执行；本文档的 6 个工具（`create_task` 等）用于管理持久化任务状态。

---

## 与 todo_write 的区别

| | todo_write（s05） | 任务系统（s10） |
|---|---|---|
| 定位 | 当前任务的执行清单 | 可恢复的任务系统 |
| 存储 | 进程内 / 会话状态 | `.tasks/{id}.json` |
| 依赖 | 无 | `blockedBy` 依赖图 |
| 生命周期 | 当前会话 / 当前任务 | 跨会话 |
| 协作 | 无认领概念 | `owner` / claim 认领 |
| 状态 | pending / in_progress / completed | pending / in_progress / completed |
| 更新契约 | 整体替换清单 | 逐条 create / get / update / list |

---

## 存储布局

每条任务一个 JSON 文件，目录随工作区（workdir）：

```
.tasks/
└── task_1a2b3c4d.json
```

文件内容：

```json
{
  "id": "task_1a2b3c4d",
  "subject": "setup database schema",
  "description": "create users table",
  "status": "pending",
  "owner": null,
  "blockedBy": []
}
```

- **id**：`task_` 前缀 + 8 位随机十六进制。文件以独占方式（`O_EXCL`）创建，撞号时丢弃该 ID 重新生成，绝不覆盖已有任务。
- **status**：`pending` | `in_progress` | `completed`。
- **owner**：认领者名称，未认领时为 `null`。
- **blockedBy**：依赖的任务 ID 列表，创建时为空列表。

`.tasks/` 属于工作区运行时状态，已在 [.gitignore](../.gitignore) 中排除。

---

## 状态机

```
pending ──claim_task──→ in_progress ──complete_task──→ completed
```

只有两个动作、三个状态：

- **claim_task**：`pending` → `in_progress`。记录 owner，开始工作。
- **complete_task**：`in_progress` → `completed`。标记完成并解锁下游任务。

---

## 工具参考

### create_task — 创建任务

| 参数 | 必填 | 说明 |
|---|---|---|
| `subject` | 是 | 任务的简短祈使句摘要（纯空白视为缺失） |
| `description` | 否 | 细节描述，跨会话恢复时靠它继续工作 |

**返回**：新任务的完整 JSON（含运行时生成的 ID）。新任务一律以 `pending`、空 `blockedBy`、`owner: null` 起步。

**失败**：subject 缺失 → `Error: subject is required`。

### update_task — 追加依赖边

| 参数 | 必填 | 说明 |
|---|---|---|
| `task_id` | 是 | create_task 返回的 ID |
| `addBlockedBy` | 否 | 该任务需要等待的任务 ID 列表 |

**返回**：更新后的完整任务 JSON。任务在其全部 `blockedBy` 依赖 completed 之前无法被认领。

**校验（保存前整体校验，任一失败不落盘）**：

- 目标任务与每个依赖都必须存在；
- 目标必须仍是 `pending` 且未被认领（已认领 / 已完成的任务不能再改边）；
- 不允许自依赖；
- 不允许成环（报错并保持文件原样）；
- 重复边安全：与已有边或同批次内重复都不会重复记录；
- 空 `addBlockedBy` 是无害的 no-op，返回任务本身。

**失败示例**：`Error: task task_xxx is in_progress, cannot update dependencies`、`Error: adding task_yyy would create a dependency cycle through task task_xxx`。

### claim_task — 认领任务

| 参数 | 必填 | 说明 |
|---|---|---|
| `task_id` | 是 | 任务 ID |
| `owner` | 否 | 认领者名称，默认 `agent` |

**成功**：`Claimed task_xxx (subject)`，状态变为 `in_progress`，owner 落盘。

**拒绝（返回原因文本，不是错误）**：

- 任务不是 `pending` → `Task task_xxx is in_progress, cannot claim`；
- 存在未完成依赖 → `Blocked by: task_aaa, task_bbb`（状态不变）。

### complete_task — 完成并解锁

| 参数 | 必填 | 说明 |
|---|---|---|
| `task_id` | 是 | 任务 ID |
| `owner` | 否 | 必须与认领者一致，默认 `agent` |

**成功**：`Completed task_xxx (subject)`；若因此有下游任务**刚被解锁**，追加一行 `Unblocked: <subject>, <subject>`。

"刚解锁"以完成前的快照为准：完成前就已可开始的 pending 任务不会重复上报；没有新解锁时不含 `Unblocked` 行。

**拒绝**：

- 任务不是 `in_progress` → `Task task_xxx is pending, cannot complete`；
- owner 不匹配 → `Task task_xxx is owned by alice, not bob`（只有认领者能完成任务）。

### get_task — 查看完整详情

| 参数 | 必填 | 说明 |
|---|---|---|
| `task_id` | 是 | 任务 ID |

**返回**：任务完整 JSON，包括 `description` 与 `blockedBy` 细节。跨会话恢复时用读取完整描述来继续工作。

### list_tasks — 列出全部任务

无参数。

**返回**：每条任务一行摘要，按 ID 排序：

```
task_1a2b3c4d [completed] setup database schema (owner: agent)
task_9f8e7d6c [pending] create API endpoints (owner: none, blocked by: task_1a2b3c4d)
```

空库返回 `(no tasks)`。

---

## 依赖检查规则（can_start）

任务可认领的条件：**全部** `blockedBy` 依赖均为 `completed`。特别注意：依赖任务**文件丢失**（被手工删除等）视为未完成，不会被当作已完成放行——宁可阻塞也不误放行。

---

## 典型工作流：两阶段建图

建图分两个阶段：**先创建全部节点拿到 ID，再连线**。这一点在模型于同一响应里发出多个工具调用时尤为重要——兄弟调用在结果产生前就已定型，一个 `create_task` 无法引用另一个调用的新生成 ID。

```
schema ────→ endpoints ────→ tests
   └─────────→ docs
```

```text
# 阶段 1：创建全部节点，各自拿到运行时 ID
create_task(subject="setup database schema")   → task_a1b2c3d4
create_task(subject="create API endpoints")    → task_e5f6a7b8
create_task(subject="write tests")             → task_c9d0e1f2
create_task(subject="write docs")              → task_b3c4d5e6

# 阶段 2：用返回的 ID 加边
update_task(task_id="task_e5f6a7b8", addBlockedBy=["task_a1b2c3d4"])
update_task(task_id="task_c9d0e1f2", addBlockedBy=["task_e5f6a7b8"])
update_task(task_id="task_b3c4d5e6", addBlockedBy=["task_a1b2c3d4"])

# 认领并完成第一个无依赖任务
claim_task(task_id="task_a1b2c3d4")
# → Claimed task_a1b2c3d4 (setup database schema)
complete_task(task_id="task_a1b2c3d4")
# → Completed task_a1b2c3d4 (setup database schema)
#   Unblocked: create API endpoints, write docs

claim_task(task_id="task_e5f6a7b8")   # 依赖已满足，可认领
complete_task(task_id="task_e5f6a7b8")
# → Completed ... Unblocked: write tests
```

---

## 跨会话恢复

全部状态都在 `.tasks/` 文件里，store 自身无内存状态。会话重启后：

1. `list_tasks` 查看全局进度（哪些完成、哪些待认领、哪些被阻塞）；
2. `get_task` 读取完整 `description`，恢复"接下来具体做什么"的上下文；
3. 对无未完成依赖的任务 `claim_task` 继续。

---

## 测试

单元测试位于 [taskstore_test.go](../taskstore_test.go)，覆盖：

- ID 格式与唯一性、非法 ID（含路径逃逸）拒绝；
- 创建落盘 / 跨会话恢复 / subject 校验 / 损坏文件报错；
- 两阶段建图、重复边去重、空依赖 no-op；
- 保存前整体校验（存在性、pending、无 owner、自依赖、环）且拒绝不落盘；
- 依赖完成解锁、依赖文件丢失视为未完成；
- claim / complete 全状态机与"刚解锁"快照语义；
- 6 个工具经 `runTool` 的参数校验与错误前缀。

运行：

```sh
go test -run 'Task' -v ./...
```
