# ACP Codex 集成实现说明

状态：第一阶段已实现
日期：2026-05-23

## 一句话概括

这次实现把 Memoh 接成了一个最小可用的 ACP client，并把上层运行时抽成了通用 ACP agent profile。当前只注册内置 `codex` profile，Memoh 会在 bot 的 trusted local workspace 里启动 `codex-acp`，把 Codex 当成一个子会话接入同一个聊天线程：用户可以让主 Agent 通过 `codex_delegate` 委派任务，也可以在 Web 输入框显式发送 `/codex start`。Codex 的流式回复、内部工具活动和最终结果都会进入原聊天流；Web 会根据 ACP metadata 标记当前回复方。

当前实现只覆盖第一阶段：local workspace、内存态 active task、内置 `codex` profile 默认启动预装的 `codex-acp` adapter，并在本地环境缺少该命令时回退到 `npx -y @zed-industries/codex-acp`；不新增数据库表、不做完整 approval broker。`internal/acpclient` 保持通用 ACP client，只负责协议和 stdio 桥接；Codex 只是 `acpagent` 中的一个具体 profile。

## 设计目标

1. Memoh 自己的 Agent 仍然是主 Agent，ACP 只是用来启动和驱动外部 coding agent。
2. Codex 在 Memoh workspace 边界内工作，文件读写和命令执行都走现有 workspace bridge。
3. 用户交互仍然是聊天，而不是单独打开一个 Codex terminal。
4. Codex task 启动后，同一个 bot session 的后续消息自动交给 Codex，直到用户停止。
5. Web 上需要看得出来当前是谁在回复，Codex 的内部工具活动不能显示成“调用工具 codex”。

## 运行前提

第一阶段依赖：

- bot workspace backend 必须是 `local`。
- Codex profile 默认启动预装的 `codex-acp`。生产 server 镜像会把 `codex-acp` 预装进 workspace toolkit；本地 trusted workspace 如果 PATH 中没有 `codex-acp`，会回退到 `npx -y @zed-industries/codex-acp`。
- 用户需要自己完成 Codex 登录或本机环境配置；Memoh 当前不注入 Codex 凭证。
- Codex 操作的 `project_path` 必须落在 local workspace root 内。

不支持：

- container workspace 内启动 Codex ACP。
- DB 级 `codex_tasks` 恢复。
- 每个 ACP permission request 都弹 Memoh 审批。
- ACP plan mode/session mode UI。
- 多个 Codex task 在同一个 bot session 并发运行。

## 总体架构

```text
User / Web Chat
    |
    | 1. 普通消息或 /codex start
    v
conversation flow Resolver
    |
    | 2a. 已有 ACP agent task -> 直接转发给当前 active agent
    | 2b. /codex start -> 显式启动 codex profile
    | 2c. 普通主 Agent -> 可调用 codex_delegate
    v
internal/acpagent.Service
    |
    | 3. StartSession / Prompt / Stop
    v
internal/acpclient.Runner
    |
    | 4. workspace bridge ExecStream 启动 adapter stdio
    v
codex-acp
    |
    | 5. ACP callbacks: fs / terminal / permission / session update
    v
workspace bridge
    |
    | 6. ReadFile / WriteFile / ExecStream
    v
local workspace
```

Codex 输出回流：

```text
ACP agent session/update
    -> internal/acpclient.StreamEvent
    -> internal/acpagent.Service.convertACPEventLocked
    -> conversation.UIMessageStreamConverter
    -> message event hub: agent_stream
    -> WebSocket / SSE
    -> Web chat message blocks
```

最终文本持久化：

```text
internal/acpagent.Service.runPrompt
    -> Completion callback
    -> Resolver.persistCodexCompletion
    -> storeRound(..., modelID="")
    -> bot_history_messages
```

`modelID` 为空是有意的：Codex ACP 不是 `models` 表中的 Memoh chat model，避免消息持久化把 `codex-acp` 当 UUID 解析导致失败。

## 主要代码变更

| 文件/目录 | 作用 |
|---|---|
| `go.mod`, `go.sum` | 增加 `github.com/coder/acp-go-sdk` 依赖。 |
| `internal/acpclient/` | ACP client 封装层。只在这个包内直接使用 ACP SDK。 |
| `internal/acpagent/service.go` | 通用 ACP agent 子会话运行时服务，维护 profile、active task、发送 prompt、转换流式事件、回调持久化；当前只内置 `codex` profile。 |
| `internal/agent/tools/codex.go` | 新增 `codex_delegate` tool，让主 Agent 可以把代码任务交给 Codex。 |
| `internal/conversation/flow/resolver_acpagent.go` | ACP agent 会话路由、profile slash command 解析、`/codex start`/`/codex stop`、完成消息持久化。 |
| `internal/conversation/flow/resolver.go` | 注入 ACP agent service，连接 stream publisher 和 completion callback。 |
| `internal/conversation/flow/resolver_stream.go` | WebSocket/streaming 入口调用 Codex 路由。 |
| `cmd/agent/app.go`, `cmd/agent/module.go` | FX wiring：创建 ACP runner、ACP agent service、注册 Codex tool provider。 |
| `internal/toolapproval/policy.go` | 把 `codex_delegate` 纳入敏感工具策略。 |
| `internal/conversation/uimessage*.go` | UI message 增加 `metadata`，stream/persisted 两条路径都能保留 ACP agent metadata。 |
| `apps/web/src/store/chat-list.ts` | 读取 UIMessage metadata，给 assistant turn 标记 responder display name，例如 `Codex`。 |
| `apps/web/src/composables/api/useChat.types.ts` | Web UI message 类型增加 `metadata` 字段。 |
| `apps/web/src/pages/home/components/message-item.vue` | Codex 回复显示 `Codex` 小标识，流式时显示“正在回复”。 |
| `apps/web/src/pages/home/components/tool-call-registry.ts` | ACP agent 内部工具事件按 metadata 动态显示，例如“Codex 调用工具”，plan 显示为“Codex 更新计划”。 |
| `apps/web/src/pages/home/components/chat-pane.vue` | 输入框增加 `/` 命令提示，支持 `/codex start` 和 `/codex stop`。 |
| `apps/web/src/i18n/locales/*.json` | 增加命令提示、Codex 工具文案、正在回复文案。 |

## ACP client 层

### 包边界

`internal/acpclient` 是 ACP SDK 的隔离层。业务包不直接 import `github.com/coder/acp-go-sdk`，这样后续如果 Go SDK 不兼容，或者要换成内部 JSON-RPC 实现，只需要替换这个包。

### Runner

核心类型：

- `Runner`
- `StartSession(ctx, StartRequest, EventSink)`
- `Session`
- `Session.Prompt(ctx, prompt)`
- `Session.Close()`

`internal/acpclient` 不内置 Codex 命令，调用方必须传入 ACP agent 的 `command` / `args`。内置 Codex profile 的启动配置在 `internal/acpagent`：

```go
Command = "sh"
Args = []string{
    "-lc",
    "if command -v codex-acp >/dev/null 2>&1; then exec codex-acp; fi; exec npx -y @zed-industries/codex-acp",
}
```

启动流程：

1. 通过 `WorkspaceInfo(botID)` 获取 workspace backend 和默认目录。
2. 只允许 `bridge.WorkspaceBackendLocal`。
3. 用 `ResolvePathUnderRoot(root, project_path)` 校验项目路径。
4. 通过 `MCPClient(botID)` 获取 workspace bridge client。
5. 用 `bridge.ExecStream` 启动调用方传入的 ACP agent 命令。Codex profile 会优先执行 `codex-acp`，缺失时回退到 `npx -y @zed-industries/codex-acp`。
6. 把 `ExecStream` 包装成 stdio reader/writer，交给 ACP SDK 的 `NewClientSideConnection`。
7. 调用 ACP `initialize`，声明 Memoh client 支持：
   - `fs.readTextFile`
   - `fs.writeTextFile`
   - `terminal`
8. 调用 ACP `newSession`，`cwd` 使用解析后的 project path。
9. 后续 prompt 通过 ACP `prompt` 发送。

### stdio 进程桥接

`internal/acpclient/process.go` 负责把 bridge `ExecStream` 包装成 ACP SDK 需要的 stdio process。

关键点：

- 启动前用 `command -v <command>` 检查调用方传入的 ACP command 是否可用。Codex 的 `codex-acp` / `npx` fallback 属于 Codex profile，不放在通用 acpclient 中。
- stdout 作为 ACP JSON-RPC 输入读回。
- stdin 通过 `ExecStream.SendStdin` 写给 adapter。
- stderr 不参与 JSON-RPC，保留最后 8 KiB 作为错误上下文。
- 关闭时关闭 stdin/stdout/exec stream，并等待最多 2 秒。

如果启动失败，错误会包含 adapter stderr tail，方便定位缺少 `codex-acp` / `npx`、Codex 未登录、adapter 崩溃等问题。

### 路径安全

`internal/acpclient/path.go` 实现 workspace root 校验。

规则：

- 空路径表示 workspace root。
- `/data` 作为 local workspace root 的别名。
- `/data/foo` 映射到 `<workspace-root>/foo`。
- 相对路径基于 workspace root。
- 绝对路径必须仍在 workspace root 内。
- 会解析已存在父目录的 symlink，防止通过符号链接逃逸 workspace root。

这层同时用于：

- ACP `newSession.cwd`
- ACP file read/write callback
- ACP terminal cwd
- ACP permission request 中 tool location 和 raw input 路径字段校验

### ACP callback 映射

`clientCallbacks` 实现 ACP client-side callbacks：

| ACP callback | 当前实现 |
|---|---|
| `ReadTextFile` | 校验路径后调用 bridge `ReadFile`，只允许文本文件。 |
| `WriteTextFile` | 校验路径后调用 bridge `WriteFile`。 |
| `RequestPermission` | 先校验 tool locations/raw input 不逃逸 workspace；通过后优先选择 `allow_once`，否则 `allow_always`，否则 cancel。 |
| `SessionUpdate` | 收集 text/tool/plan，并通过 `EventSink` 发给 `internal/acpagent.Service`。 |
| `CreateTerminal` | 通过 bridge `ExecStream` 启动命令。 |
| `TerminalOutput` | 返回已收集 stdout/stderr，默认保留 128 KiB，最多 1 MiB。 |
| `WaitForTerminalExit` | 等待 ExecStream exit。 |
| `KillTerminal` | 关闭 ExecStream。 |
| `ReleaseTerminal` | 关闭并移除 terminal。 |

当前 terminal callback 不支持 `env`，如果 ACP 请求带 env 会拒绝。这是第一阶段限制，因为 bridge streaming exec wrapper 还没暴露 env。

## ACP agent runtime service

`internal/acpagent.Service` 是 ACP 子会话的通用运行时管理层。

### profile 机制

`acpagent.Profile` 描述一个可启动的 ACP agent：

```go
type Profile struct {
    ID          string
    DisplayName string
    Command     string
    Args        []string
}
```

当前只内置一个 profile：

```text
id: codex
display_name: Codex
command: sh
args: -lc "if command -v codex-acp >/dev/null 2>&1; then exec codex-acp; fi; exec npx -y @zed-industries/codex-acp"
```

后续要支持 Claude/Gemini，不需要新增一套 runtime；增加对应 profile，再把 slash command resource 映射到 profile 即可。例如 `/gemini start` 可以映射到 `id=gemini, command=gemini, args=["--acp"]`。

核心职责：

- 每个 `bot_id + session_id` 维护一个 active task。
- 启动 ACP session。
- 把用户 prompt 发送给当前 active ACP agent。
- 把 ACP stream event 转成 Memoh UI message。
- 在一轮 prompt 完成后回调 Resolver 持久化最终文本。
- 停止子会话时关闭 ACP session。

### active task key

```text
task key = bot_id + ":" + session_id
```

这意味着同一个 bot session 同时最多一个 ACP agent 子会话。重复 start 时如果 task 已存在，会复用同 profile 的 session 并发送新 prompt；如果旧 task 已 failed/closed，会先清理再启动新 session。

### prompt 串行化

每个 task 有 `promptMu`，保证同一个 ACP session 内 prompt 串行执行，避免用户连续发消息造成 ACP prompt 并发。

### stream 转换

ACP event 到 UI block 的映射：

| ACP event | UI stream event | Web 展示 |
|---|---|---|
| text delta | `text_delta` + `metadata.source=acp_agent` + `metadata.agent_id` | 当前 ACP agent 文本块 |
| tool start/update/end | `tool_call_*`，tool name 为 `acp_agent_tool` | 例如“Codex 调用工具” |
| plan update | `tool_call_progress`，tool name 为 `acp_agent_plan` | 例如“Codex 更新计划” |

当前 Codex profile 的文本和工具 UIMessage 都带：

```json
{
  "source": "acp_agent",
  "agent_id": "codex",
  "agent": "Codex"
}
```

### handoff 文案

显式 `/codex start` 会传入 `HandoffText`。`Service.runPrompt` 会在发送 Codex prompt 前先输出一条 Memoh 交接说明：

```text
已启动 Codex ACP 子会话。接下来这个会话将由 Codex 和你沟通；发送 /codex stop 可以切回 Memoh。
```

这条说明不标记为 Codex，避免用户误以为它是 Codex 说的。

## 两种启动入口

### 入口一：主 Agent 调用 tool

`internal/agent/tools/codex.go` 注册 `codex_delegate`。

输入参数：

```json
{
  "task": "自然语言代码任务",
  "project_path": "可选，workspace root 内路径",
  "mode": "normal",
  "attachments": ["可选 workspace 路径或引用"]
}
```

行为：

1. 校验 bot id。
2. 通过 `WorkspaceInfo` 校验 workspace backend 必须是 local。
3. 校验 `task` 必填。
4. 只允许 `mode=normal`。
5. attachments 会拼进 task 文本。
6. 调用 `acpagent.Service.Start`，并传入 `AgentID=codex`。
7. 工具快速返回 task id、project path、ACP session id 和提示文案。

这个入口适用于用户自然语言说“用 Codex 做 xxx”，主 Agent 判断后主动委派。

### 入口二：Web 输入框 slash command

`internal/conversation/flow/resolver_codex.go` 支持：

```text
/codex start [--project <path>] [任务文本]
/codex stop
```

例子：

```text
/codex start --project /data/simple-web 新建一个 Go HTTP server
/codex stop
```

`/codex start` 的行为：

1. Resolver 在进入普通 Agent resolve 之前解析命令。
2. 如果不是 `/codex`，继续普通 Agent 流程。
3. 如果是 `/codex start`，直接调用 `acpagent.Service.Start`，不让主 Agent 再思考。
4. 如果没有任务文本，会给 Codex 一个默认任务：接管会话并等待用户下一条指令。
5. 启动后发布 handoff message。
6. 持久化用户命令和 handoff message。

`/codex stop` 的行为：

1. 调用 `acpagent.Service.Stop(botID, sessionID)`。
2. 发布“已停止 Codex 子会话，后续消息会回到 Memoh Agent。”
3. 持久化控制消息。

仍保留自然语言停止：

```text
停止 codex
退出 codex
stop codex
```

这些只在当前 session 已有 active Codex task 时生效。

## Conversation flow 接入点

Resolver 在普通 Agent resolve 前检查 ACP agent 路由。

同步 Chat：

```go
if routed, err := r.routeACPAgentMessage(ctx, req); routed {
    return conversation.ChatResponse{}, err
}
```

Streaming/WebSocket 也走同一逻辑。

路由优先级：

1. `/codex start` / `/codex stop`
2. 当前 session 是否有 active Codex task
3. 如果有，后续用户消息转发给 Codex
4. 如果没有，进入普通 Memoh Agent

这样实现了“Codex ACP 一启动，当前会话后续消息由 Codex 接管”的交互。

## 消息流和持久化

### 流式展示

ACP agent service 调用 `SetStreamPublisher` 注入的回调，把 stream 发送给 Resolver：

```go
r.publishBackgroundAgentStream(out.BotID, out.SessionID, out.Stream)
```

Web 端收到的是已有 `agent_stream` 事件：

```json
{
  "type": "message",
  "data": {
    "type": "text",
    "content": "...",
    "metadata": {
      "source": "acp_agent",
      "agent_id": "codex",
      "agent": "Codex"
    }
  }
}
```

这个设计复用了现有聊天流式通道，没有新增独立 Codex WebSocket。

### 最终持久化

`runPrompt` 会累积本轮 Codex text delta。prompt 返回后，如果最终文本非空，先调用 completion callback，再发布 stream end。

顺序是：

1. stream start
2. stream message...
3. completion callback 持久化最终文本
4. stream end

这个顺序是为了避免前端收到 end 后立刻刷新历史，但最终 Codex 文本还没写进 DB，导致消息短暂消失。

持久化内容：

- 如果用户消息已经由 Web/channel 层持久化，则 `UserMessagePersisted=true`，completion 只写 assistant。
- 如果是后续 Codex routed message 且用户消息未提前持久化，则 completion 会补写 user + assistant。
- assistant content 使用 structured content part，并带 `metadata.source=acp_agent`、`metadata.agent_id=codex`、`metadata.agent=Codex`。
- `modelID` 传空字符串，不绑定 `models` 表。

### control/handoff message

`/codex start` 和 `/codex stop` 的控制说明是 Memoh 发出的普通 assistant text，不带 Codex metadata。这样 Web 不会把它标成 Codex 本人回复。

## Web 交互实现

### slash command 提示

`apps/web/src/pages/home/components/chat-pane.vue` 中新增输入框命令建议。

当前命令：

- `/codex start`
- `/codex stop`

交互：

- 输入 `/` 时显示建议。
- 输入 `/c`、`/codex` 时过滤建议。
- 上下键切换选项。
- Enter/Tab 选中命令并填入输入框。
- Esc 关闭建议。
- 鼠标点击也可以选择。

第一阶段只暴露 Codex 命令，没有把现有所有 slash commands 都放进 Web 命令面板，因为 WebSocket chat 当前没有统一接入通用 command handler。

### Codex 回复标识

Web 类型层给 UI message 增加：

```ts
metadata?: Record<string, unknown>
```

`chat-list.ts` 读取 block metadata：

- 如果 `metadata.source === "acp_agent"`，assistant turn 使用 `metadata.agent` 作为 responder，例如 `Codex`。
- `message-item.vue` 根据 responder 显示小标签。
- streaming 状态下显示“正在回复”。

### ACP agent 工具文案

之前 ACP tool activity 统一叫 tool name `codex`，前端走通用 fallback，显示成“调用工具 codex”。现在改为：

- 后端 tool name：`acp_agent_tool`
- 中文文案：`Codex 调用工具`
- 英文文案：`Codex tool`

Plan update：

- 后端 tool name：`acp_agent_plan`
- 中文文案：`Codex 更新计划`
- 英文文案：`Codex plan update`

## 权限和安全边界

### 当前已做

1. `codex_delegate` 被纳入工具审批敏感工具。
2. 只支持 local workspace，container backend 会被拒绝。
3. 所有文件路径和 terminal cwd 都必须落在 workspace root 内。
4. `/data` 只作为 local workspace root alias，不是任意宿主机路径。
5. ACP permission request 会检查 tool locations 和 raw input 中的路径，不合法则 cancel。
6. adapter 启动命令使用参数转义，避免直接拼接未转义参数。

### 当前没有做

1. ACP 内部每次写文件/执行命令没有单独弹出 Memoh 审批。
2. `RequestPermission` 在范围校验通过后会自动选择 `allow_once`。
3. 不支持 per-command policy，例如某条 Codex terminal 命令必须审批。
4. 不支持向 adapter 注入 secret env。

这是第一阶段的取舍：外层 `codex_delegate` 或显式 `/codex start` 表示用户启动了一次 Codex 子会话；更细粒度 approval broker 后续再补。

## IM 平台行为

当前主要验证 Web/Desktop 本地聊天。

IM 侧的基础路径已经存在：

- Codex completion 会走 `outboundFn` 发送最终文本。
- reasoning/thought 不会作为 IM 消息展示。

当前没有做 IM 平台的 Codex delta 逐条编辑或节流发送。Telegram/Discord/Lark 等平台的消息频率、编辑限制和审批交互需要单独设计。

## 验证覆盖

已增加或更新的测试覆盖：

| 测试 | 覆盖点 |
|---|---|
| `internal/acpclient` tests | fake ACP agent、initialize/new session/prompt、fs、terminal、permission、通用 client 缺少 command 时的错误、显式缺失命令错误。 |
| `internal/acpagent/service_test.go` | Start/Send active session、profile command/args、Codex profile 启动命令、stream 顺序、completion 顺序、ACP agent tool event label、handoff 先于 agent 输出。 |
| `internal/agent/tools/codex_test.go` | `codex_delegate` 参数校验、local workspace 限制、unsupported mode、starter error 透传。 |
| `internal/conversation/flow/resolver_codex_test.go` | `/codex start`/`/codex stop` 解析，stop phrase，model id 为空。 |
| `internal/conversation/uimessage_test.go` | UIMessage metadata 在 streaming 和 persisted 两条路径保留。 |
| `apps/web/src/store/chat-list.test.ts` | Web store 能根据 `metadata.source=acp_agent` 和 `metadata.agent` 标记 assistant turn。 |

本次验证命令：

```bash
go test ./...
mise run lint
pnpm --filter @memohai/web exec vue-tsc --noEmit
pnpm --filter @memohai/web exec vitest run src/store/chat-list.test.ts
```

## 用户使用方式

自然语言委派：

```text
用 Codex 在 /data/simple-web 新建一个 Go HTTP 服务
```

主 Agent 判断后调用 `codex_delegate`。

显式命令启动：

```text
/codex start --project /data/simple-web 新建一个 Go HTTP 服务，监听 8080，提供 / 和 /health
```

只启动并等待后续指令：

```text
/codex start
```

停止 Codex：

```text
/codex stop
```

或者在 Codex active 时：

```text
停止 codex
```

## 常见错误和定位

### `peer disconnected before response`

通常表示 `codex-acp` adapter 进程在 initialize 或 new session 前退出。

排查：

1. `memoh-server` 或 workspace bridge 的 PATH 是否能找到 `codex-acp`；本地 trusted workspace 可检查 `npx` fallback。
2. 用户是否能在同一环境手动运行 `codex-acp`，或运行 `npx -y @zed-industries/codex-acp`。
3. Codex 是否已登录或配置好凭证。
4. stderr tail 是否有更明确的 adapter 错误。

### `codex ACP requires a local workspace`

当前 bot 使用 container workspace。第一阶段只支持 trusted local workspace。

### `invalid project_path`

`project_path` 不在 workspace root 内，或者通过 symlink 逃逸了 root。使用 `/data/<relative-path>` 或相对路径。

### 启动成功但 Web 没有 Codex 标识

检查 Codex stream/persisted UIMessage 是否带：

```json
{
  "metadata": {
    "source": "acp_agent",
    "agent_id": "codex",
    "agent": "Codex"
  }
}
```

Web store 根据 metadata 中的 `source` 和 `agent` 判断 responder。

## 当前限制

1. active task 只存在内存里，server 重启会丢失。
2. Codex session 没有 DB task 记录，不支持状态页和恢复。
3. 不支持 container workspace。
4. 预装的 `codex-acp` 只解决 adapter 可执行文件，不解决 Codex 凭证注入。
5. 不支持 bridge streaming exec env。
6. ACP permission request 还没有映射成 Memoh 的逐项审批。
7. Web 命令面板只支持 Codex 命令，不是通用 slash command palette。
8. 同一 session 只允许一个 Codex task。
9. 不持久化每条 stream delta，只持久化最终 assistant 文本。

## 后续建议

优先级建议：

1. 增加 `codex_tasks` 表，持久化 active task 状态、project path、ACP session id、last error。
2. 做 approval broker，把 ACP `request_permission` 映射到 Memoh tool approval。
3. 为 bridge streaming exec 增加 env 支持，安全注入 Codex/OpenAI 凭证。
4. 做 container workspace 支持，并补齐 Codex 凭证注入。
5. Web 命令面板从硬编码 Codex 扩展为后端命令 manifest。
6. IM 平台做 Codex 输出节流、最终文本发送和审批回复体验。
7. 增加 Codex task 状态 UI，例如 session header 中显示 active project 和停止按钮。
8. 支持按 project path 管理多个 Codex session，但同一聊天仍需要明确选择当前 active session。

## 和原计划文档的关系

`docs/design/acp-codex-integration-plan.md` 是路线和阶段规划。

本文档记录当前已经落地的第一阶段实现，包括具体代码路径、运行链路、交互入口、消息持久化和已知限制。后续如果进入 container、approval broker 或 DB task 阶段，应继续更新本文档，而不是只更新计划。
