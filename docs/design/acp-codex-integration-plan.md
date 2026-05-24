# ACP Codex 接入计划

状态：第一阶段已实现，后续阶段待设计
日期：2026-05-23

更新说明：第一阶段落地时，底层 runtime 已从 Codex 专用服务调整为通用 ACP agent profile 机制；当前只内置 `codex` profile。具体代码路径和最新实现以 `docs/design/acp-codex-integration-implementation.md` 为准。

## 背景

Memoh 现在已经有内置的 Go Agent、workspace 容器、基于 bridge 的文件和命令执行、工具审批、Web/Desktop 聊天流式输出，以及 IM 平台适配器。接入 ACP 的目标不是替换 Memoh 自己的 Agent，而是让 Memoh Agent 可以把完整的代码任务委派给外部 coding agent。第一阶段目标是通过 `codex-acp` 调用 Codex。

长期形态中，Memoh 作为 ACP client，`codex-acp` 作为 ACP agent。`codex-acp` 运行在 bot 的 workspace 内，所以 Codex 的文件访问和命令执行仍然受 Memoh workspace 容器或可信本地 workspace 的边界约束。

当前第一阶段只做 Desktop/local workspace 的最小实现：先用 `npx -y @zed-industries/codex-acp` 启动 Codex ACP adapter，Memoh 不打包二进制、不构建 workspace 镜像、不做 DB 级 task 持久化。目标是先验证 Memoh 能通过 ACP 启动 Codex 子会话、把 Codex 输出流式展示在同一个聊天里，并在该会话活跃时把后续用户消息路由给 Codex。所有文件和 terminal 操作都发生在 trusted local workspace 中。

## 外部依赖

- ACP 协议：https://agentclientprotocol.com/
- ACP 概览：https://agentclientprotocol.com/protocol/overview
- ACP 文件系统回调：https://agentclientprotocol.com/protocol/file-system
- ACP terminal 回调：https://agentclientprotocol.com/protocol/terminals
- ACP session modes：https://agentclientprotocol.com/protocol/session-modes
- Codex ACP adapter：https://github.com/zed-industries/codex-acp
- Go ACP SDK：https://pkg.go.dev/github.com/coder/acp-go-sdk

SDK 选择：

- 第一版使用 `github.com/coder/acp-go-sdk@v0.13.0`。
- 这是社区 Go SDK，不是 ACP spec 组织官方维护的 Go SDK。
- SDK 类型必须隔离在 `internal/acpclient` 内，不能泄露到 agent、conversation、handlers、channel 等业务包。
- 如果实际接 `codex-acp` 时发现 SDK 不兼容，就在同一个 `internal/acpclient` 接口下切换为内部最小 JSON-RPC 实现。

## 目标

1. 增加最小可用的 ACP client 能力，用来启动和驱动 Codex。
2. 保持聊天作为主交互：用户让 Memoh 用 Codex，Memoh 委派任务，Codex 输出显示在同一个聊天里。
3. Codex 的文件访问、文件写入和 terminal 执行都通过 Memoh workspace 边界。
4. 复用现有工具审批策略和审批交互。
5. 优先支持 Web/Desktop，再支持 IM 平台的过滤和节流输出。
6. MVP 保持简单：每个 `bot_session` 同时只允许一个活跃 Codex task。

## MVP 不做的事

- 不做通用 ACP marketplace UI。
- 不做 Codex 之外的多 agent 编排。
- 不做 ACP session mode 或 plan mode 选择 UI。
- 不持久化每一条 Codex stream delta，只持久化最终 assistant 文本。
- 不支持同一个 bot session 里并发多个 Codex task。
- 不把独立的 Workspace Codex 面板作为主交互。
- 第一阶段不打包 `codex` / `codex-acp`，也不构建 Codex workspace 镜像；先通过用户本机 PATH 中的 `npx` 运行 `@zed-industries/codex-acp`。
- 第一阶段用 `npx` 启动 adapter，因此 Desktop/local server 进程必须能找到 `npx`。后续预装二进制或 workspace 镜像形态再取消这个要求。

## 第一阶段最小实现范围

第一阶段的目标是一个可验证的 local proof of concept，不是完整产品化能力。

范围：

1. 只支持 Desktop/local server 中的 trusted local workspace。
2. 只支持 bot 的 workspace backend 为 `local`。
3. 依赖用户本机已有可用的 Node/npm `npx`，并且 `memoh-server` 进程的 PATH 能找到 `npx`。
4. 依赖用户已经完成 Codex 登录或配置好本机环境变量；第一阶段不从 Memoh provider/OAuth 系统注入 Codex 凭证。
5. 使用现有 workspace bridge `ExecStream` 启动 `npx -y @zed-industries/codex-acp`，暂时不改 bridge proto，也不新增 `ExecStreamWithEnv`。
6. 新增 `codex_delegate` 工具，主 Agent 调用后启动一个 Codex ACP 子会话并快速返回 task 信息。
7. 每个 `bot_session` 维护一个内存态活跃 Codex task；task 活跃期间，同一会话后续用户消息自动路由给 Codex。
8. 不新增 DB table，不做 `codex_tasks` 持久化。
9. 不新增前端面板；Web/Desktop 通过现有 `agent_stream` 消息事件看到 Codex 流式输出。
10. IM 侧第一阶段先不做细粒度流式体验；Codex 完成后可通过现有 outbound 通道发送最终文本。

第一阶段保留的安全约束：

1. 只允许 local workspace root 内的文件路径。
2. ACP file read/write 回调必须做路径 normalize 和 workspace-root 校验。
3. ACP terminal 回调的 cwd 必须落在 local workspace root 内。
4. `codex_delegate` 应作为敏感工具纳入审批策略；审批通过表示用户允许本次 Codex session 在该 workspace 内执行代码任务。
5. 第一阶段不做每个 ACP permission request 对应一次 Memoh 审批；这是后续 approval broker 阶段处理的内容。

## 产品交互

主流程：

1. 用户发送普通聊天消息，例如：“用 Codex 在 /data/app 修复测试失败”。
2. Memoh 主 Agent 判断这是一个代码委派任务，调用 `codex_delegate`。
3. Memoh 在同一个聊天里回复任务已经交给 Codex。
4. Codex 的流式输出显示在同一个 conversation 里，消息 metadata 标记 `source=codex`。
5. Codex task 活跃期间，同一个 bot session 里的后续用户消息默认转发给 Codex。
6. Codex 完成一轮 prompt 后，Memoh 持久化一条最终 assistant message；ACP session 继续保持活跃，直到用户用 `/codex stop` / `停止 codex` 停止或后续策略清理。

第一阶段简化：

- 不新增独立 Codex 面板。
- 不做 DB 级 task 表和断线恢复。
- 不持久化每一条 Codex delta。
- 不做 ACP session mode / plan mode 的 UI 选择。
- 只允许同一个 `bot_session` 同时存在一个活跃 Codex 子会话。
- 提供最小退出口：`/codex stop`、`停止 codex`、`退出 codex` 会关闭当前 Codex 子会话，后续消息回到 Memoh Agent。

Web/Desktop 展示：

- Codex assistant 文本作为普通 assistant stream 显示，但带一个小的 “Codex” 来源标识。
- 工具调用尽量复用现有 stream/tool-call 组件。
- 权限申请复用现有审批 UI。
- Workspace 的文件、terminal、display 面板只作为辅助查看和调试入口。

IM 展示：

- Codex 输出走现有 channel stream 过滤逻辑。
- reasoning/thought delta 不发送到 IM。
- 文本输出需要节流或批量发送，避免触发平台编辑频率限制。
- 审批提示复用现有平台的审批命令或回复流程。

## 运行模型

第一阶段运行模型：

- `codex-acp` 通过 trusted local workspace 的 in-process bridge 启动，第一阶段启动命令为 `npx -y @zed-industries/codex-acp`。
- `npx` 命令从 `memoh-server` 进程 PATH 查找。
- ACP `session/new.cwd` 使用 local workspace 的真实宿主机路径，例如 `~/.memoh/workspaces/<bot>` 或用户创建 bot 时指定的路径。
- 如果用户传入 `project_path`，必须是 local workspace root 下的绝对路径或相对路径。
- 不支持 container workspace；遇到 container-backed bot 时返回明确错误。
- 不支持 VNC/display；Display 是 workspace 能力，不是 ACP 第一阶段要求。

后续容器镜像：

- `memohai/workspace-codex:debian`：Debian workspace 基础镜像，加 `codex`、`codex-acp`、`git`、`bash`、`ca-certificates`、`curl` 或 `wget`、`ripgrep`。
- `memohai/workspace-codex-node:debian`：在上面基础上增加 Node.js/pnpm，给 JS/TS 项目使用。
- `memohai/workspace-codex-vnc:debian`：基于现有 VNC 镜像，加 `codex` 和 `codex-acp`。

第一阶段因为选择 `npx` 启动 adapter，Node/npm 是 adapter 启动器需求；长期预装 Rust 二进制后，Node.js 仍只属于具体项目运行时需求。

凭证：

- 第一阶段不从 Memoh 注入 `CODEX_API_KEY` 或 `OPENAI_API_KEY`。
- `codex-acp` 使用用户本机已有的 Codex 登录状态、配置文件或环境变量。
- 后续容器/服务端形态再通过 bridge exec env 传入 `CODEX_API_KEY` 或 `OPENAI_API_KEY`。
- 不能把 secret 拼进 shell 命令字符串。
- 后续建议给 bridge client 增加 `ExecStreamWithEnv`，因为 proto 已经支持 `env`，但当前 Go wrapper 的 streaming exec 没暴露这个字段。

## Session 和目录模型

第一阶段映射：

- 第一次 `codex_delegate` 调用创建一个新的 ACP session。
- Runtime 内存中记录 `bot_id + session_id -> active Codex task`。
- 活跃 task 存在时，后续同一 `bot_session` 的用户消息默认进入同一个 Codex ACP session。
- `project_path` 默认 local workspace root。
- 第一阶段不在 UI 中支持目录切换；需要切换项目时应停止当前 task 后重新委派。

产品化映射：

- 每个 `bot_session` 只允许一个活跃 Codex task。
- Task 记录 `bot_id`、`session_id`、`project_path`、provider、ACP session id、status、source platform 和 reply target。
- 如果已有活跃 task，再用不同项目路径启动新 task，应返回明确错误，或提示用户先停止当前 task。

目录切换：

- 不在同一个 Codex ACP session 内模拟 `cd`。
- 新项目 cwd 创建新的 ACP session。
- 未来版本可以支持按 `bot_id + project_path` 切换 session。

## 后端架构

### 包结构

新增：

```text
internal/acpclient/
  client.go        # Memoh 自有接口，封装选定的 ACP SDK
  process.go       # 第一阶段通过 local workspace bridge stdio 启动 npx codex-acp
  fs.go            # ACP fs 回调 -> workspace bridge 文件 API
  terminal.go      # ACP terminal 回调 -> bridge ExecStream
  approval.go      # 后续：ACP permission 回调 -> Memoh tool approval
  events.go        # ACP session updates -> 文本收集；后续再转 Memoh stream events

internal/codex/
  service.go       # 后续：TaskService: Start, SendInput, Stop, Status
  task.go          # runtime 状态、task id、生命周期
  store.go         # 持久化接口
  events.go        # 发布 Web/Desktop/IM stream events
  fx.go            # FX wiring

internal/agent/tools/codex.go
  # codex_delegate ToolProvider
```

`internal/acpclient` 必须是唯一 import `github.com/coder/acp-go-sdk` 的包。

第一阶段落地：

- `internal/acpclient`
- `internal/codex`
- `internal/agent/tools/codex.go`
- agent tool wiring
- conversation flow active task routing

`internal/codex.TaskService`、DB store、长期 session routing 和前端 task indicator 都放到后续阶段。

### ACP Client Wrapper

先定义 Memoh 自有接口：

```go
type AgentClient interface {
    Initialize(ctx context.Context) error
    NewSession(ctx context.Context, req NewSessionRequest) (Session, error)
    Prompt(ctx context.Context, sessionID string, input PromptInput) error
    Cancel(ctx context.Context, sessionID string) error
    Close(ctx context.Context) error
}
```

实现层可以使用 `acp.NewClientSideConnection` 或 SDK 对应的 helper。编码前需要以实际安装的 SDK 版本为准确认准确 API 名称。

Memoh 的 ACP client 实现需要处理这些 client-side 回调：

- `session/update`：转换为 Memoh `agent_stream` 事件，Web/Desktop 复用现有聊天流式 UI 展示 Codex 文本、tool activity 和 plan 摘要；最终文本再作为 assistant message 持久化。
- `fs/read_text_file`：校验路径，调用 bridge `ReadFile`。
- `fs/write_text_file`：校验路径，需要时走审批，然后调用 bridge `WriteFile`。
- `session/request_permission`：第一阶段在外层 `codex_delegate` 已审批的前提下选择一次性允许；没有外层授权或路径/cwd 校验失败时拒绝。后续改成创建 Memoh 工具审批请求并等待 approve/reject。
- `terminal/create`：通过 bridge `ExecStream` 运行命令。
- `terminal/output`：把 terminal 输出转发回 Codex ACP connection。
- `terminal/wait_for_exit`、`terminal/kill`、`terminal/release`：映射到 bridge stream 生命周期。

### Codex Task Service

第一阶段实现内存态 `CodexTaskService`，但不新增 DB table，不做进程重启后的 task 恢复。

职责：

- 解析 bot session 和 workspace。
- 校验项目路径。
- 通过 bridge stdio 启动 `npx -y @zed-industries/codex-acp`。
- 用 `cwd=project_path` 创建 ACP session。
- 发送初始 prompt。
- 维护活跃 task 的 runtime map。
- 将后续用户消息转发给活跃 task。
- 把 Codex stream updates 发布到现有 message event hub。
- 在完成、取消、进程退出、session close 时停止 task。
- 持久化 task 状态变化。

建议 API：

```go
type TaskService interface {
    Start(ctx context.Context, input StartInput) (Task, error)
    SendInput(ctx context.Context, taskID string, text string, attachments []Attachment) error
    Stop(ctx context.Context, taskID string, reason string) error
    ActiveBySession(ctx context.Context, botID string, sessionID string) (Task, bool, error)
}
```

### Tool Provider

在 `internal/agent/tools` 下新增普通 Twilight tool provider：`codex_delegate`。

输入：

- `task`：交给 Codex 的自然语言任务。
- `project_path`：可选。第一阶段默认 local workspace root；后续 container workspace 默认 `/data`。
- `mode`：可选，初始只支持 `normal`。
- `attachments`：可选，workspace 路径或 message asset 引用。

第一阶段行为：

- 校验当前 bot 是否为 local workspace backend。
- 通过 `internal/codex.TaskService` 异步启动 Codex task。
- 通过 `internal/acpclient` 启动 `codex-acp`、创建 ACP session、发送初始 prompt。
- 返回一个简短结果给主 Agent：task id、project path、active status。
- 不阻塞等待 Codex 完成。
- Codex stream update 通过 `agent_stream` 进入 Web/Desktop 聊天。
- 后续同一 `bot_session` 用户消息由 conversation routing 直接转发给活跃 Codex task。

### Conversation Routing

在普通 inbound message flow 里增加一个 pre-agent routing hook：

1. 如果当前消息所属 bot session 有活跃 Codex task，把消息路由到 `TaskService.SendInput`。
2. 用户消息仍然按现有流程持久化。
3. 这一轮不调用主 Memoh Agent，除非用户明确要求停止 Codex 或切回主 Agent。

停止和切换控制：

- 尽量支持自然语言：“停止 Codex”、“先别让 Codex 做了”。
- Slash command 可以以后作为 fallback 增加，但不是 MVP 必需。

## 权限映射

ACP 操作到 Memoh 审批策略的映射：

| ACP 操作 | Memoh approval tool name | 说明 |
| --- | --- | --- |
| 外层 Codex 委派 | `codex_delegate` | 第一阶段的主要审批点。用户批准本次 Codex session 在 local workspace 内工作。 |
| 读文件 | `read` 或 bypass | 读操作通常默认允许，除非未来扩展读审批策略。 |
| 写文件 | `write` 或 `edit` | ACP 提供 diff/edit 类 tool call 时用 `edit`，否则用 `write`。 |
| terminal command | `exec` | tool input 里包含 command、cwd、env key、timeout、task id。 |
| permission request | 按 tool kind 映射 | 保留 Codex tool call title 和 locations 到审批 metadata。 |

第一阶段：

- `codex_delegate` 作为敏感工具纳入审批策略。
- 外层审批通过后，本次 ACP permission request 可以选择 `allow_once`。
- 外层未审批、路径不合法、cwd 不合法、非 local workspace 时拒绝。
- 不实现 per-operation approval broker。

后续需要的实现改造：

- 当前审批流程和 Twilight tool execution 绑定较深。
- 增加 decision broker/waiter，让外部系统可以创建 pending approval 并等待审批结果。
- `Approve` 和 `Reject` 更新 DB 后，需要把 decision 发布给 broker。
- `flow.RespondToolApproval` 需要识别外部 approval target，并把 decision 返回给外部 waiter，而不是执行 Twilight tool。

最小识别方式：

- 使用 `tool_call_id` 前缀，例如 `acp:<task_id>:<request_id>`。
- 长期建议在 tool approval 表增加显式 `source` 和 `source_ref` 字段。

审批安全规则：

- 创建审批前，所有路径都必须 normalize，并确认没有逃逸 workspace root。
- 命令必须以结构化 input 表示，方便策略检查和 UI 展示。
- env value 要脱敏，只展示 env key。
- 如果 task 被取消，所有 pending ACP approval 都返回 cancelled outcome。

## Streaming 和持久化

Stream events：

- 第一阶段把 ACP `agent_message_chunk` 转为 `EventTypeAgentStream`，Web/Desktop 作为普通 assistant stream 展示。
- `codex_delegate` tool result 只返回 task id、project path、active status，不等待 Codex 完成。
- Codex prompt 完成时必须先同步持久化最终 assistant 文本，再发布 `end` stream 事件，避免前端刷新早于落库。
- `codex_delegate` 成功后需要明确告知用户：任务已交给 Codex，接下来由 Codex 在当前会话中沟通，可发送 `/codex stop` 切回 Memoh。
- ACP `agent_thought_chunk` 不展示、不发送到 IM。
- ACP tool call start/update 和 plan update 转为 tool/progress UI message，帮助用户理解 Codex 做了什么。
- MVP 不暴露 plan-mode 选择。

持久化：

- 第一阶段不新增持久化表。
- 用户消息按现有流程持久化。
- Codex 结果作为主 Agent 的工具结果进入本轮上下文，最终由主 Agent 的 assistant message 持久化。
- 后续阶段再持久化 Codex 最终 assistant message、runtime delta 和 task 状态。

推荐表：

```sql
codex_tasks (
  id uuid primary key,
  bot_id uuid not null,
  session_id uuid not null,
  provider text not null,
  project_path text not null,
  status text not null,
  acp_session_id text,
  source_platform text,
  reply_target text,
  requested_message_id uuid,
  final_message_id uuid,
  error text,
  metadata json/jsonb not null default '{}',
  created_at timestamptz/datetime not null,
  updated_at timestamptz/datetime not null,
  ended_at timestamptz/datetime
)
```

Migration 要求：

- 更新 PostgreSQL canonical `0001_init.up.sql`。
- 增加下一组 PostgreSQL incremental up/down migration。
- 更新 SQLite canonical `0001_init.up.sql` 和对应 down migration。
- 两个后端都增加等价 sqlc queries。
- 运行 `mise run sqlc-generate`。

MVP shortcut：

- 如果只做更小的 spike，可以先把 task 状态放内存，跳过 DB 改动。
- 这只适合本地 proof of concept。产品 MVP 至少应该持久化 task status。

## Workspace Bridge 改造

第一阶段：

- 不改 bridge proto。
- 使用现有 `ExecStream(ctx, command, workDir, timeout)` 启动 `npx -y @zed-industries/codex-acp`。
- local bridge 会继承 `memoh-server` 进程环境；因此用户安装的 `npx` 必须对该进程 PATH 可见。

后续新增：

- `ExecStreamWithEnv(ctx, command, workDir string, timeout int32, env []string)`。
- 如果 `codex-acp` terminal callback 需要 PTY 语义，再补 PTY 和 resize 支持。

复用：

- `internal/handlers/mcp_stdio.go` 已经有把 workspace process stdio stream 接到协议 transport 的成熟模式。
- ACP process management 应该复用这个思路，但不要耦合到 handler 层代码。

## 前端范围

Web/Desktop MVP：

- 第一阶段不新增前端能力。
- Codex 输出先作为主 Agent 最终回复的一部分展示。
- 后续再让 Codex 输出显示在现有 chat stream 里，并给 `metadata.source == "codex"` 的 assistant message 加一个小来源标签。
- 复用现有审批 UI。
- 复用现有 workspace file/terminal/display 面板。

MVP 不新增主 Codex panel。

后续可选 UI：

- Bot settings tab：启用/禁用 Codex delegation、选择默认 project path、选择 image profile。
- Workspace task indicator：显示活跃 Codex task、停止按钮、project path。
- Session history：展示历史 Codex tasks 和最终消息。

## IM Channel 范围

第一阶段：

- 不做 IM 侧 Codex 流式输出。
- IM 只接收主 Agent 最终回复。

后续 MVP 行为：

- 使用现有 outbound stream filtering。
- 发送可见 assistant text 和审批提示。
- 抑制 thought/reasoning。
- 按 adapter 对文本编辑或发送做节流。

需要手动验证的 adapter：

- Telegram
- DingTalk
- Discord
- Feishu/Lark

## 实施阶段

### Phase 0：Desktop Local 异步 MVP

1. 增加 `github.com/coder/acp-go-sdk@v0.13.0`。
2. 在 `internal/acpclient` 中封装 SDK。
3. 写一个 fake ACP agent test process，用本地 pipe 验证 initialize、new session、prompt、session/update。
4. 用现有 bridge `ExecStream` 启动 `npx -y @zed-industries/codex-acp`，`npx` 从 PATH 查找。
5. 只允许 local workspace backend；container workspace 直接返回 unsupported。
6. ACP `session/new.cwd` 使用 local workspace 真实路径。
7. 实现 `fs/read_text_file`、`fs/write_text_file` 的 workspace-root 校验和 bridge 文件访问。
8. 实现最小 terminal callback：create/output/wait/kill/release 映射到 bridge exec 生命周期。
9. 新增 `internal/codex.Service`，维护内存态 active task，转发 ACP stream 到 `agent_stream`。
10. 新增 `codex_delegate` ToolProvider，启动 task 后快速返回 task 信息。
11. conversation flow 在 active task 存在时把后续用户消息转发给 Codex。
12. Codex prompt 完成后持久化最终 assistant 文本。
13. 注册 `codex_delegate` 到 agent tool wiring。

退出标准：

- Go test 证明 wrapper 可以驱动 fake ACP agent，不依赖真实 Codex。
- local workspace bot 可以通过 `codex_delegate` 调用 `npx -y @zed-industries/codex-acp`。
- 缺少 `npx` 或 adapter 启动失败时返回明确错误，提示用户安装 Node/npm 或确认 Desktop/local server PATH。
- 非 local workspace bot 调用时返回明确 unsupported。
- 不产生 DB migration、前端变更、workspace 镜像变更。

### Phase 1：权限和安全补强

1. 把 `codex_delegate` 纳入工具审批策略。
2. 记录本次 Codex session 的外层审批状态。
3. ACP permission request 在外层审批通过后才允许 `allow_once`。
4. 加强 path/cwd 校验，覆盖相对路径、`..`、symlink escape。
5. terminal command 的 cwd 必须在 local workspace root 内。
6. terminal output 做大小限制，避免内存无限增长。

退出标准：

- 未审批时 Codex 不能执行写文件和 terminal 操作。
- 审批通过后，Codex 仍不能逃逸 local workspace root。
- 大量 terminal 输出不会拖垮 server 进程。

### Phase 2：Chat Stream Integration

1. 把 ACP `agent_message_chunk` 转成 `EventTypeAgentStream`。
2. 给 Codex stream/message 增加 `metadata.source = "codex"`。
3. Web/Desktop 显示 Codex 来源标签。
4. ACP tool call start/update 转成现有 tool-call stream event shape。
5. ACP plan update 转成紧凑状态事件，不做 plan mode 选择。

退出标准：

- Web chat 能在同一 conversation 中看到 Codex 流式文本。
- 刷新后至少能看到主 Agent 最终回复。
- IM 仍只接收过滤后的主 Agent 输出。

### Phase 3：Codex Task Service

1. 增加 `internal/codex.TaskService`。
2. 实现每个 bot session 一个 active task。
3. 将后续用户消息路由到 active Codex task。
4. 支持 stop/cancel task。
5. 进程退出、session close、context cancel 时清理 runtime state。

退出标准：

- 后端测试可以启动 fake task，并路由 follow-up input。
- 正常完成和取消时都能清理进程资源。
- 同一 session 不允许并发多个 Codex task。

### Phase 4：Approval Broker

1. 增加 approval decision broker/waiter。
2. 把每个 ACP permission request 转成 `toolapproval.CreatePending`。
3. `Approve` / `Reject` 更新 DB 后，把 decision 返回给 ACP request。
4. 支持 timeout/cancel。

退出标准：

- Codex write/exec 权限可以从 Web/Desktop 单独审批。
- 至少一个 IM adapter 可以审批或拒绝同一个请求。
- 外层 `codex_delegate` 审批不再等价于内部所有操作全放行。

### Phase 5：Persistence

1. 为 PostgreSQL 和 SQLite 增加 `codex_tasks` migration。
2. 增加 sqlc queries。
3. 存储 task 生命周期状态变化。
4. Codex 最终结果持久化为独立 assistant message。
5. server restart 时，把之前 running 的 task 标记为 failed 或 interrupted。

退出标准：

- 刷新页面后能看到 active task status。
- 重启后不会留下假的 active task。
- Codex 最终消息刷新后仍可见。

### Phase 6：Container Workspace

1. 构建 `workspace-codex` 镜像。
2. 增加 `ExecStreamWithEnv`。
3. 在 container workspace 内启动预装的 `codex-acp`。
4. 通过 env 传入凭证。
5. 支持 VNC 镜像变体，但不把 display 当作 ACP 依赖。

退出标准：

- container-backed bot 可以通过 ACP 调用 Codex。
- local 和 container 两条路径共用 `internal/acpclient`。
- 凭证不会出现在 shell command、日志或审批 UI value 中。

### Phase 7：IM 和产品化 UI

1. IM 侧展示 Codex 可见进度。
2. 加输出节流，避免平台刷屏或 rate limit。
3. Bot settings 增加 Codex delegation 开关、默认 project path、image profile。
4. Workspace 增加 active task indicator 和 stop control。
5. Session history 展示历史 Codex tasks。

退出标准：

- IM 能收到合理的过滤后进度。
- Web/Desktop 有可见 task 状态和停止入口。
- 普通用户无需理解 ACP 或命令行即可使用 Codex delegation。

## 验证计划

第一阶段单元测试：

- `internal/acpclient`：fake ACP process、SDK wrapper、session/update 文本收集。
- `internal/acpclient`：fs read/write callback 通过 fake bridge client。
- `internal/acpclient`：terminal create/output/wait/kill/release 的生命周期和输出截断。
- `internal/agent/tools`：`codex_delegate` 参数校验、非 local workspace 拒绝、缺少 binary 错误文案。
- 路径校验：拒绝 `..`、symlink escape、非 local workspace 绝对路径。

第一阶段集成测试：

- fake ACP agent over bridge `ExecStream`。
- 文件 read/write callback 通过 bridge。
- terminal callback 通过 bridge。
- `codex_delegate` 能启动 task 并快速返回 task 信息；Codex 文本通过 `agent_stream` 到达聊天。
- 同一 `bot_session` 后续消息会复用已有 ACP session。

第一阶段手动测试：

- Desktop/local：创建 local workspace bot。
- 确认 `memoh-server` 进程 PATH 能找到 `npx`。
- 让主 Agent 调用 `codex_delegate` 修改 local workspace 里的小文件。
- 验证文件变更发生在 local workspace root 内。
- 失败场景：缺少 `npx`、用户未登录 Codex、非法 project path、`codex-acp` 进程退出、非 local workspace bot。

后续阶段测试：

- `internal/codex`：task 生命周期、active-task routing、cancellation。
- `internal/toolapproval`：broker decision、timeout、external approval target。
- `internal/conversation/flow`：follow-up message 路由到 Codex，而不是主 Agent。
- Web/Desktop：启动 Codex task、审批命令、观察 stream output、刷新页面。
- Telegram/DingTalk/Discord/Feishu：确认可见输出可读，且不会刷屏。

第一阶段建议命令：

```bash
go test ./internal/acpclient ./internal/agent/tools
mise run lint
```

后续只有改到 SQL、API 或前端 SDK surface 时，才需要运行 SQLC/Swagger/SDK 生成：

```bash
mise run sqlc-generate
mise run swagger-generate
mise run sdk-generate
```

## 风险和缓解

| 风险 | 缓解 |
| --- | --- |
| ACP Go SDK 是社区维护，变化可能较快。 | 隔离在 `internal/acpclient`；固定版本；增加 fake-agent contract tests。 |
| 用户本机没有安装 Node/npm `npx`，或 Desktop local server PATH 找不到。 | 第一阶段返回明确错误；文档提示用户安装 Node/npm 并重启 Memoh Desktop。 |
| 用户未登录 Codex 或本机凭证不可用。 | 第一阶段不托管凭证；把 `codex-acp` stderr tail 放进错误信息。 |
| local workspace 没有容器隔离，命令以本机 server 进程权限执行。 | 只在 Desktop/local trusted workspace 开启；默认拒绝非 local workspace；外层 `codex_delegate` 需要审批。 |
| `codex-acp` 实际行为可能和协议假设有差异。 | Phase 0 用 fake agent 和真实 `codex-acp` 各做一次验证，再做产品 UI。 |
| 当前审批流程和 Twilight tool 耦合较深。 | 第一阶段先用外层 delegation approval；后续增加 broker 和 external approval target。 |
| IM channel 可能刷屏或被 rate limit。 | 第一阶段不做 IM Codex 流；后续复用 adapter filtering 并增加 Codex 专用节流。 |
| 长进程生命周期可能泄漏资源。 | 第一阶段每 session 一个内存态 task，受 context timeout/cancel 控制，并在 stop/进程退出时 close stdio 和 terminal。 |
| server restart 会丢失运行中进程。 | 第一阶段没有 DB 持久 active task；产品 MVP 持久化 task，并在启动时把 running task 标记 interrupted。 |
| workspace path escape。 | normalize path，并在 fs/terminal callback 前强制 workspace-root 校验。 |
| secret 泄露到日志或审批 UI。 | 第一阶段不注入 Memoh secret；后续 env 结构化传递，显示时只展示 key，不展示 value。 |
| VNC/display 和 ACP 概念混淆。 | display 是 workspace capability，不是 ACP requirement。 |

## 待决策问题

已定：

1. 第一阶段只做 Desktop/local workspace。
2. 第一阶段依赖用户本机安装 Node/npm，并通过 `npx -y @zed-industries/codex-acp` 启动 adapter。
3. 第一阶段不打包二进制、不构建 workspace 镜像。
4. 第一阶段不做 `codex_tasks` 持久化。
5. 第一阶段不做 plan mode 选择。

待后续决策：

1. 产品 MVP 是否必须包含 `codex_tasks` 持久化，还是可以在 stream/task service 后再加？
2. 产品化凭证来源用哪个：provider API key、user OAuth，还是 bot-level Codex account？
3. 默认 workspace image 是否全局切换，还是 Codex 需要显式选择 image profile？
4. 读文件审批是否要配置化，还是留给后续更完整的审批策略？
5. 用户在聊天里如何明确把控制权从 Codex 切回 Memoh 主 Agent：只用自然语言，还是也给可见 stop control？

## 验收标准

第一阶段完成时应满足：

1. 用户使用 Desktop/local workspace bot。
2. 用户本机 PATH 中安装了 `npx`，且 Codex 已登录或可用。
3. 主 Agent 可以调用 `codex_delegate`。
4. Memoh 通过 ACP initialize、new session、prompt 向 `codex-acp` 发送一次任务。
5. `codex_delegate` 快速返回 task 信息，Codex assistant text 通过同一聊天的 `agent_stream` 展示。
6. ACP 文件读写和 terminal cwd 被限制在 local workspace root 内。
7. 非 local workspace bot、缺少 `npx`、非法 project path 都有明确错误。
8. 不需要 DB migration、前端改动、workspace 镜像改动。

产品 MVP 完成时应满足：

1. 用户能在普通聊天里要求 Codex 处理 bot workspace 里的 repo。
2. Memoh 在 workspace 内启动 `npx -y @zed-industries/codex-acp`，并通过 ACP 发送任务。
3. Codex 流式输出能显示在同一个 Web/Desktop chat 中。
4. 后续用户消息会发送给活跃 Codex task。
5. Codex 文件写入和 terminal 命令被限制在 workspace 内，并遵守 Memoh 审批。
6. Codex 最终回复被持久化为 chat message。
7. task 在完成、取消、审批拒绝、进程退出、server restart 时都能正确清理生命周期。
