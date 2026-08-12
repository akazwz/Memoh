# Live ACP 交互与 Web Agent commands 实现说明

> 状态：Draft PR #1002；评审修复完成，待重新人工 QA，2026-08-12
>
> 范围与问题归类：[ACP-INTERACTIONS-PLAN.md](./ACP-INTERACTIONS-PLAN.md)
>
> Resume 后续：[memohai/Memoh#998](https://github.com/memohai/Memoh/issues/998)

## 1. 文档用途

这份文档描述当前代码如何工作，供代码评审、发布和后续维护使用。为什么只做这些、21 个问题如何归类，以及哪些工作留到以后，放在 Plan 文档中。

本轮实现只有两个主题：

1. Live ACP permission、Form 和 Stop 的语义透传；
2. Web 的 ACP controls 与 Agent-declared slash commands。

这里的 live 指原 ACP 进程、session 和正在等待的 JSON-RPC callback 仍然存在。进程重启后的上下文恢复不在这份实现里。

## 2. 设计约束

### 2.1 Agent 决定语义

permission option、mode id 和 command name 都是 Agent 定义的协议值。Memoh 可以验证结构和权限，但不能改写 ID，也不能把 `allow_once`、session scope 或 Agent 自定义选项压成自己的二元状态。

### 2.2 Server 做最终裁决

Web 缓存用于显示。执行 Agent command、切换 mode 或回答交互时，Server 必须重新检查当前 session、runtime、Agent 身份和授权。过期 UI 不能变成执行权限。

### 2.3 不伪造恢复能力

数据库里的 pending 记录不等于 ACP callback 仍然存在。当前实现允许浏览器刷新后继续操作同一 live runtime，但不承诺 ACP 进程退出后继续原调用。

### 2.4 兼容改动只做增量

新增 HTTP 字段保持 optional，数据库使用带默认值的新列，Turn protobuf 不变。旧 Web、Desktop 和 Channel 不需要与这次功能同时升级。

## 3. 主题一：Live ACP 交互语义透传

### 3.1 Permission 数据链

ACP `session/request_permission` 的选项经过以下路径：

```text
ACP Agent
  -> clientCallbacks.RequestPermission
  -> approval domain / tool_approval_requests
  -> UIMessage 实时与历史投影
  -> Web 用户选择
  -> application.CommitToolApprovalResponse
  -> ACP selected option outcome
```

各层保存的内容如下：

| ACP 数据 | Memoh 表示 | 用途 |
|---|---|---|
| `optionId` | `PermissionOption.ID` | 原样返回 Agent |
| `name` | `PermissionOption.Name` | 用户可见标签 |
| `kind` | allow/reject once/always | 判断选项极性，不重命名 ID |
| 用户选择 | `selected_option_id` | 审计、历史投影和 ACP response |
| 无具体工具的请求 | `operation=permission` | 保留通用授权语义 |

ACP callback 边界会拒绝空 option id、重复 ID 和未知 kind。ID 的比较区分大小写，前后空格不会被悄悄删除。

用户选择后，application 层先核对选项是否属于当前 pending request，再按 option kind 执行 approve 或 reject。`selected_option_id` 与终态在同一次数据库更新中写入，实时卡片和历史卡片读取同一结果。

### 3.2 旧客户端的二元审批

旧 Web/Desktop 只发送 approve 或 reject，没有 `option_id`。兼容规则放在 application 层，所有 HTTP/WS 调用共用：

- 请求没有 options 时，沿用原来的二元审批；
- approve 只可自动对应唯一的 `allow_once`；
- reject 优先对应唯一的 `reject_once`；
- 没有 `reject_once` 时仍可做普通拒绝，ACP 侧返回 `cancelled`；
- 多个同 kind 选项或只有 broader allow scope 时拒绝猜测，让新客户端显式选择。

Memoh 不会为了兼容旧 UI 自动选择 `allow_always`。

### 3.3 通用 permission 与 MCP

权限请求先尝试关联已知 read、write、exec 或 Memoh MCP tool。无法可靠关联的请求不再静默取消，而是以通用 `permission` 进入同一审批链，并保留 Agent 的标题、输入摘要和 options。

安全规则如下：

- consent 形状、非 Memoh MCP 和所有 unmapped 形状都设置 `ForceReview`：即使 Bot 的普通审批策略关闭，这条 generic 通道也绝不替用户回答，「交给用户决定」在所有策略配置下成立；
- 已确认属于 Memoh Tool Gateway 的真实 MCP 调用继续使用 gateway 自己的策略，避免同一次操作弹两次审批；
- Agent 声明了 `execute`/`read`/`edit` kind、仅因 path/command 无法解析而降级成 generic 的请求，仍按对应操作的显式 `deny` 姿态自动拒绝——降级通道不弱于已分类通道；显式 deny 优先于 ForceReview（自动拒绝是诚实的回答，弹卡片反而允许用户批准策略硬禁止的操作）。

纯 thought 请求在不带 consent 语义时可以按既有规则通过。越出 workspace scope 的请求直接拒绝。

Codex runtime 同时固定为 `approval_policy=on-request`、`sandbox_mode=workspace-write`、`network_access=false`。这保证需要升级权限的行为确实会发出 permission，而不是受上游默认值变化影响。

### 3.4 ACP Form

ACP `elicitation/create` 的 form 请求映射到现有 ask_user 表单。v0.13.5 的标准 form 不包含 `sessionId` 或 `toolCallId`，所以归属取自当前 Memoh tool session 和已 attach 的 ACP session；某些 adapter 带来的同名字段只作为可选扩展。

当前支持：

- object 根 schema；
- string 文本输入；
- string 的 `enum`、`oneOf` 或 `anyOf` 单选；
- string enum array 多选；
- required 与 optional 字段；
- Codex/ask_user metadata 表达的 custom answer。

表单保留 ACP property id 和选项值。Web 只看到稳定的 question/option id，提交后 mapping 再把答案还原为 ACP 请求中的 property/value。

不能准确兑现的约束会在协议边界返回 invalid params。当前会拒绝 secret/password、未知 schema keyword、非 string/受支持 array 的类型、冲突的 choice 定义和无法验证的 custom 结构。custom-answer 关系图在入口整体校验：custom 属性不能指向另一个 custom 属性或自身，不能指向不存在或不渲染的属性，required 属性不能携带 custom 伴生。接受一个表单，就意味着返回内容满足已经声明支持的约束；映射层不存在「提交后才失败」的可达分支。

Form 的数据库记录和 UI 投影可以跨浏览器刷新保留，但 field mapping、waiter 和 JSON-RPC callback 是进程内状态。timeout、Stop 或 runtime teardown 会取消请求；进程重启不会假装恢复。

### 3.5 Stop

用户主动 Stop 时：

1. 取消当前 ACP prompt；
2. 取消该 prompt 仍在等待的 permission/Form；
3. 保存已经收到的 partial output；
4. 将本轮标记为 aborted，不追加 `ACP agent failed`；
5. 不对这轮 partial output做 memory extraction；
6. 保留同一进程中的 ACP runtime，供下一轮继续使用。

runtime 复用以静默为前提。ACP SDK 在连接级 goroutine 上分发 permission/Form callback，它们可以在 prompt 取消后存活，所以：

- Stop 不会立即丢弃本地 `session/prompt` JSON-RPC 请求。Client 发出 `session/cancel` 后，在有界宽限内继续等待同一 PromptResponse，并利用 SDK 的 response watermark 排空该响应前所有 `session/update`；
- 被取消轮次的 tool call 会记入 tombstone，晚到的同 ID permission 请求直接得到 cancelled，不会关联到下一轮、也不会以下一轮身份建审批行；Agent 在新一轮 session/update 里重新声明同 ID 时立即解除；
- 只有 PromptResponse、它之前的通知和已进入的 decision callback 都完成后，runtime 才能热复用。任一环在宽限内无法确认，就回收 runtime，不把未知的上轮状态交给下一轮；
- Stop 完成的 permission/Form 以 durable `cancelled`/`canceled` 终态替换 pending 快照，但不额外发送一个迟到的 live frame；最终由唯一的 EventAbort 带出已收口的 partial transcript；
- prompt 完成与 Stop 同瞬发生时，Stop 获胜：完整输出照常保存，但本轮按 aborted 持久化，不做 memory extraction，终态事件是 abort 而不是 end。

prompt 完成与取消同时发生时，清理逻辑会再次检查 context，避免 pending waiter 因 select 竞争残留。

### 3.6 授权

回答 ACP permission/Form 或修改 runtime control 前，Server 检查 Bot、Thread、runtime owner 和当前用户权限。已知 runtime owner 可以操作自己的 runtime；其他成员需要对应 Bot 的 workspace execution 权限。runtime owner 缺失时 fail closed，不能仅凭一条孤儿 pending 记录继续执行。

## 4. 主题二：Web ACP controls 与 Agent commands

### 4.1 Live configuration state

ACP `session/update` 会更新当前 `client.Session` 中的 model、reasoning、mode 和 available commands。`ConfigurationState` 在同一把锁下复制完整快照，避免一次 HTTP status 混入两次通知的值。

SessionPool 返回状态前还会检查 handle 是否已经关闭或替换。关闭中的旧 runtime 不再暴露 ACP session id、modes 或 commands，防止旧能力被当成新 runtime 的授权依据。

mode 切换使用 Agent 声明的 opaque mode id。`session/set_mode` 返回前若 Agent 已发送更晚的 current-mode 通知，通知结果优先；请求侧 fallback 只在 revision 没变化时写入。

### 4.2 Web 状态刷新

Web registry 按 `bot_id + session_id` 保存最近一次 RuntimeStatus。以下时机会调用 Ensure/refresh：

- ACP 会话变为当前可见会话；
- 打开 model、reasoning 或 slash 面板；
- 设置 mode/model/reasoning 后；
- 收到可识别的 stale/config 错误后。

这不是持久化 controls 总线。Server 重启后，Web 通过 HTTP 从新建或仍然 live 的 runtime 获取状态；跨进程 session resume 留给 #998。

### 4.3 Agent command 展示

`available_commands_update` 采用 full replacement 语义。Session 保存 name、description 和 unstructured input hint，slash picker 将它们放进单独的 Agent commands 分组。

用户选好 Codex/Claude Code 但还没有发送第一条消息时，Web 会从 pending warm runtime 显示它已声明的 commands。这只是发现性数据；发送后仍由 Server 对绑定的 live session 做最终裁决。

command name 视为 opaque、区分大小写。名称不能是空字符串、带空白或以 `/` 开头；`review:deep`、`plan/create` 和 Unicode 名称可以使用。选择命令只把 `/<name>` 写进输入框，有 input hint 时保留一个空格，不自动发送。

Memoh 保留 `/help`、`/new`、`/permission`、`/skill`。Agent 声明同名命令也不会覆盖这些控制项。

### 4.4 Agent command 执行

发送时不信任 picker 缓存：

```text
Web 原始输入
  -> LocalChannelHandler 提取 exact selector
  -> SessionPool.RuntimeStatus 查询 live ACP session
  -> exact advertised match: 原文作为普通 ACP prompt
  -> 未声明或 stale: unknown_slash
```

Server 比较当前 session id、live ACP session 和完整 command name。匹配后不剥离 selector，也不把命令解释成 Memoh skill，因此引号、连续空格和参数可以原样到达 Agent。

admission 通过的 selector 随请求带到 SessionPool。每轮 model/reasoning 配置先应用，然后在最终 `session/prompt` 发送边界对**真正接收 prompt**的 Session 和它最新的 command snapshot 复验。runtime 在 admission 与 prompt 之间被 reaper 回收、替换，或 Agent 在配置更新中撤销了命令时，本轮以稳定错误 `acp_agent_command_stale` 失败并回滚已入库的 user message，而不是把过期的 `/name args` 当纯文本投给一个从未声明它的新 Agent。

ACP Agent command 可以带附件。Memoh quick action 拒绝附件由 Server 在 WS 命令分支强制（稳定错误 `slash_attachments_unsupported`），Web 客户端的拦截只是前置体验，避免 `/help`、`/permission` 等本地命令消费文本后静默丢掉文件。

### 4.5 `/permission`

Web `/permission` 是 mode 的快捷入口：

- 无参数时返回当前 mode 和 Agent 声明的可选项；
- 选择列表项时直接调用 typed mode setter，opaque ID 不经过文本 trim/reparse；
- 手输 mode id 时由 Server 验证它是否属于当前 live session；
- 成功后刷新 registry，顶部 mode UI 与命令结果保持一致。

Composer 的 mode dropdown 与 `/permission` 最终都调用 SessionPool `SetMode`。两条 UI 入口不同，Agent 状态写入点只有一个。

## 5. 一致性与错误语义

- slash、permission、Form 和 mode 错误使用稳定 code；Web 根据 code 本地化，不解析英文文本；
- 私有数据库、路径或进程错误只进入日志，不进入 HTTP/SSE/WS 响应；
- stale Agent command 在 admission 返回 `unknown_slash`，在 prompt 时复验失败返回 `acp_agent_command_stale`，都不会降级成 skill 或普通聊天；
- stale approval 返回 not-found/expired，选项冲突返回 request-invalid；
- option id、answer skipped 状态进入现有 command payload hash，重复 control id 不能掩盖不同决定。canonical 形式省略零值（空 option id、skipped=false），因此不携带新字段的 payload 与旧版本二进制计算的哈希逐字节一致——同一 control id 的幂等重试跨升级仍然成立。

## 6. 升级兼容

### 6.1 Proto 与 Channel

Turn protobuf、gRPC transport 和外部 Channel 与基线一致。Web controls 不经过 split Channel RPC，所以没有新 Server 等待旧 Channel ACK，也没有 RPC rename 或 `UNIMPLEMENTED` 的滚动升级问题。

### 6.2 HTTP、Web 与 Desktop

- `option_id`、`control_id` 和 rejection reason 都是 optional；
- 旧客户端的空 body 和二元 approve/reject 继续工作；
- 新响应字段是 additive，旧客户端可以忽略；
- 新增 mode API 的 `mode_id` 是 required；
- OpenAPI 和 TypeScript SDK 已从当前 handler 重新生成。

发布时先升级 Server，再升级 Web/Desktop。这样新 UI 不会把 option id 发给还不认识该字段的旧 Server。

### 6.3 PostgreSQL 0130

0130 增加：

```sql
options JSONB NOT NULL DEFAULT '[]'::jsonb
selected_option_id TEXT NOT NULL DEFAULT ''
```

operation CHECK 同时加入 `permission`。`0001_init.up.sql` 已同步最终 schema，sqlc 使用显式列，因此旧二进制可以忽略新增列。

Create query 在 SQL 边界将 `NULL options` 收口为 `[]`。正常 domain service 本来就会传非空 JSON，这一层保证内部直接 sqlc 调用者也不会绕过 `NOT NULL` 合同。

推荐顺序：

```text
迁移 0130 -> drain 旧 Server -> 新 Server -> 新 Web/Desktop
```

本轮没有 feature gate，不支持新旧 Server 同时写 permission/options。应用回滚时保留 0130 schema，并继续使用包含 0130 的 migrate 镜像。旧 migration bundle 最高只有 0129，无法处理已经记录为 version 130 的数据库。

down migration 会先锁表并检查数据。只要出现 permission、非空 options 或 selected option，它就拒绝删除这些不可重建的审计信息；生产回滚不应把 down 当成普通步骤。

## 7. 主要代码位置

| 层 | 位置 | 职责 |
|---|---|---|
| ACP callbacks | `internal/agent/runtime/acp/client` | permission、Form、modes、commands、Stop 语义 |
| Live runtime | `internal/agent/runtime/acp/session_pool.go` | Session 生命周期与 RuntimeStatus |
| Approval domain | `internal/agent/decision/approval` | options、selected option、policy 和持久化输入 |
| Form domain | `internal/agent/decision/input` | questions、required/custom、答案校验 |
| Application | `internal/agent/application` | 授权、旧客户端兼容、mode 操作 |
| Web admission | `internal/handlers/local_channel.go` | live Agent command 最终裁决 |
| HTTP API | `internal/handlers/acp_runtime.go` | runtime controls 与稳定错误 |
| Projection | `internal/agent/view` | 实时及历史审批/Form 卡片 |
| Web UI | `apps/web/src/pages/home/components` | picker、审批选项、Form 和 mode 入口 |
| Web state | `apps/web/src/store/chat` | runtime registry、slash 发送和结果处理 |
| Schema | `db/postgres/migrations/0130_*` | options 与 selected option 增量迁移 |

## 8. 验证

自动验证已完成：Go 全仓测试、核心 race、相关 Go/TypeScript lint、Web 核心测试、sqlc vet、生成物与 diff 检查。

人工 QA 至少覆盖：

1. Codex 和 Claude Code 各触发一次带多个 scope 的 permission；
2. 检查选择、拒绝和取消返回 Agent 的结果；
3. 提交 text、single-select、multi-select、optional 和 custom Form；
4. 打开 slash picker，执行 Agent command 与 `/permission`；
5. 验证 Agent command 附件和 Memoh quick-action 附件拒绝；
6. prompt 中途 Stop，检查 partial output、终态和下一轮 runtime；
7. 在真实 PostgreSQL 上演练 0129 → 0130。

没有人工确认前，这个实现仍标记为 No human QA。

## 9. 已知边界

- ACP 进程退出后不恢复原 session；
- 不恢复旧 permission/Form callback；
- ACP 会话中以斜杠开头且未被 Agent 声明的首 token 一律 `unknown_slash`（含粘贴的绝对路径）。opaque 命令名使 prose 启发式不可靠，这是有意取舍；未来如需发送此类文本，应设计显式转义而不是暗中回退；
- 冷启动 runtime 的 `available_commands_update` 晚到时，带命令意图的消息可能得到 `acp_agent_command_stale`，回滚干净、重试即可；
- Web controls 采用按需刷新，不是 durable push projection；
- `/permission` 和 Agent commands 只在 Web 提供；
- Form 只接受当前明确支持并能完整验证的 schema 子集；
- 多 Server 部署需要 drain 旧 writer。

这些限制是本轮范围，不是隐藏的恢复承诺。后续实现 #998 时，应在现有 live 语义下面增加原生 session store、ownership 和恢复状态机，而不是改写 permission、Form 或 command 的当前数据模型。
