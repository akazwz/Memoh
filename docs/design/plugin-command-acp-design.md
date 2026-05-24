# Plugin Command 与 ACP Agent 设计

状态：设计草案
日期：2026-05-23

## 背景

Memoh 已经实现了第一阶段 ACP/Codex 接入：后端有通用 `acpagent.Service`，当前只内置 `codex` profile；Web 输入框的 `/` 提示通过后端 command manifest 动态加载。

下一步需要解决两个问题：

1. Web 的 `/` 快捷指令不应该硬编码在前端。
2. 未来 Codex、Claude、Gemini 这类能力不应该只叫 command，它们应该是由 skills、MCP、ACP agent profile、commands 组合出来的 plugin。

本文档定义 plugin、command manifest、Web `/` 命令提示，以及 `/codex start` 调用 Codex ACP 的整体设计。

## 术语

### Plugin

Plugin 是一个能力包，不是单个命令。它可以声明：

- skills
- MCP templates 或 MCP connections
- ACP agent profiles
- slash commands
- 图标、说明、标签、权限需求

例如 `codex` plugin 可以包含：

- 一个 Codex coding skill
- 一个 `codex` ACP agent profile
- `/codex start`
- `/codex stop`

### Command

Command 是用户触发能力的入口，通常表现为 slash command。

例如：

- `/codex start`
- `/codex stop`

Command 可以来自：

- core：Memoh 内置命令
- plugin：已安装或内置 plugin 贡献的命令
- runtime：后端根据当前状态动态生成的命令

### ACP Agent Profile

ACP agent profile 是 Memoh 启动某个 ACP-compatible CLI 所需的配置：

```go
type Profile struct {
    ID          string
    DisplayName string
    Command     string
    Args        []string
}
```

当前内置 Codex profile：

```text
id: codex
display_name: Codex
command: sh
args: -lc "if command -v codex-acp >/dev/null 2>&1; then exec codex-acp; fi; exec npx -y @zed-industries/codex-acp"
```

## 命名原则

不要把 Codex 叫 command。Codex 是 plugin，`/codex start` 才是 command。

推荐命名：

```text
Plugin ID: codex
Command ID: codex.start
Command text: /codex start
ACP profile ID: codex
```

后续：

```text
Plugin ID: gemini
Command ID: gemini.start
Command text: /gemini start
ACP profile ID: gemini
```

## 总体架构

```text
Supermarket / Built-in Plugin Manifest
    |
    | installs or registers
    v
Plugin Registry
    |
    | contributes
    +--> Skills
    +--> MCP templates/connections
    +--> ACP agent profiles
    +--> Command manifests
             |
             v
        Command Registry
             |
             | GET /api/commands
             v
        Web Command Palette
             |
             | user selects /codex start
             v
        Chat message submit
             |
             v
        Conversation Resolver
             |
             | command resolves to acp_agent:start
             v
        internal/acpagent.Service
             |
             v
        internal/acpclient.Runner
             |
             v
        codex-acp stdio process
```

## Plugin Manifest

Supermarket 当前是 Skill & MCP Registry。建议新增第三类资源：`plugins/`。

目录结构：

```text
supermarket/
  plugins/
    codex/
      plugin.yaml
      README.md
  skills/
    codex-coding/
      SKILL.md
  mcps/
    ...
```

Codex plugin 示例：

```yaml
id: codex
name: Codex
description: Use Codex as an ACP coding agent inside a Memoh workspace.
version: 0.1.0
author:
  name: Memoh
tags:
  - coding
  - acp

skills:
  - codex-coding

mcps: []

acp_agents:
  - id: codex
    display_name: Codex
    command: sh
    args:
      - -lc
      - "if command -v codex-acp >/dev/null 2>&1; then exec codex-acp; fi; exec npx -y @zed-industries/codex-acp"
    workspace_backend:
      - local

commands:
  - id: codex.start
    command: /codex start
    insert_text: "/codex start "
    title: Start Codex
    description: 启动 Codex ACP 子会话，可在后面直接写任务
    source: plugin
    capability: acp_agent
    action: start
    acp_agent_id: codex
    icon: code

  - id: codex.stop
    command: /codex stop
    insert_text: "/codex stop"
    title: Stop Codex
    description: 停止 Codex 子会话，切回 Memoh Agent
    source: plugin
    capability: acp_agent
    action: stop
    acp_agent_id: codex
    icon: code
```

这个 manifest 不直接执行代码，只声明能力。执行仍由 Memoh core 完成。

## Command Manifest

后端对 Web 暴露 command manifest。Web 只负责展示、过滤、插入，不硬编码具体命令。

建议 Go 类型：

```go
type CommandManifest struct {
    ID          string   `json:"id"`
    Command     string   `json:"command"`
    InsertText  string   `json:"insert_text"`
    Title       string   `json:"title"`
    Description string   `json:"description,omitempty"`
    Source      string   `json:"source"`      // core | plugin | runtime
    PluginID    string   `json:"plugin_id,omitempty"`
    PluginName  string   `json:"plugin_name,omitempty"`
    Capability  string   `json:"capability"`  // acp_agent | mcp | skill | core
    Action      string   `json:"action"`      // start | stop | ...
    Icon        string   `json:"icon,omitempty"`
    Enabled     bool     `json:"enabled"`
    Scopes      []string `json:"scopes,omitempty"` // web | desktop | im | local_chat
}
```

Codex start 返回示例：

```json
{
  "id": "codex.start",
  "command": "/codex start",
  "insert_text": "/codex start ",
  "title": "Start Codex",
  "description": "启动 Codex ACP 子会话，可在后面直接写任务",
  "source": "plugin",
  "plugin_id": "codex",
  "plugin_name": "Codex",
  "capability": "acp_agent",
  "action": "start",
  "icon": "code",
  "enabled": true,
  "scopes": ["web", "desktop", "local_chat"]
}
```

## Command Registry

后端新增 command registry，统一收集 command manifest。

接口：

```go
type CommandProvider interface {
    Commands(ctx context.Context, req CommandRequest) ([]CommandManifest, error)
}

type CommandRequest struct {
    BotID     string
    SessionID string
    Scope     string
}
```

第一阶段 provider：

```text
ACPAgentCommandProvider
  - 从 acpagent.Service.Profiles() 生成 /<profile> start 和 /<profile> stop
  - 当前只生成 /codex start 和 /codex stop
```

后续 provider：

```text
PluginCommandProvider
  - 从已安装 plugin manifests 读取 commands

CoreCommandProvider
  - 提供 /help、/new 等 Memoh 内置命令

RuntimeCommandProvider
  - 根据当前 active session 状态调整 enabled、description 或优先级
```

## 后端 API

新增接口：

```text
GET /api/commands?bot_id=<bot>&session_id=<session>&scope=web
```

响应：

```json
{
  "commands": [
    {
      "id": "codex.start",
      "command": "/codex start",
      "insert_text": "/codex start ",
      "title": "Start Codex",
      "description": "启动 Codex ACP 子会话，可在后面直接写任务",
      "source": "plugin",
      "plugin_id": "codex",
      "plugin_name": "Codex",
      "capability": "acp_agent",
      "action": "start",
      "icon": "code",
      "enabled": true
    }
  ]
}
```

API 权限：

- 必须校验当前用户对 bot/session 的访问权限。
- 只返回当前 scope 可用的命令。
- 如果命令依赖 workspace/backend 能力，应通过 `enabled=false` 或不返回来表达。

## Web `/` 命令提示

Web 输入框逻辑：

1. 输入以 `/` 开头且没有换行时，显示 command palette。
2. 从后端 command manifest 获取命令。
3. 前端本地按 `command/title/description/plugin_name` 过滤。
4. 选中后插入 `insert_text`。
5. 用户继续补充任务文本并发送。

前端类型：

```ts
interface CommandManifest {
  id: string
  command: string
  insert_text: string
  title: string
  description?: string
  source: 'core' | 'plugin' | 'runtime'
  plugin_id?: string
  plugin_name?: string
  capability: string
  action: string
  icon?: string
  enabled: boolean
}
```

Web 不应该知道 `codex` 是怎么启动的，也不应该维护 `/codex start` 的硬编码列表。

## `/codex start` 执行链路

用户在 Web 输入：

```text
/codex start --project /data/simple-web 新建一个 Go HTTP 服务
```

链路：

```text
Web submit message
    -> conversation StreamChat/StreamChatWS
    -> routeACPAgentMessage
    -> parse slash command: resource=codex, action=start
    -> resolve profile: acpagent.Profile{ID: codex}
    -> acpagent.Service.Start
    -> acpclient.Runner.StartSession
    -> workspace bridge ExecStream
    -> Codex profile command: codex-acp or npx fallback
    -> ACP session/new + prompt
```

启动成功后：

1. Web 先展示 handoff 文案：接下来由 Codex 沟通。
2. Codex 的 ACP `session/update` 被转成 UIMessage。
3. UIMessage metadata：

```json
{
  "source": "acp_agent",
  "agent_id": "codex",
  "agent": "Codex"
}
```

4. Web 根据 metadata 显示 `Codex` badge。
5. 同一 session 后续用户消息会自动路由给 active Codex task。
6. `/codex stop` 停止 task 并切回 Memoh Agent。

## Plugin 安装对命令的影响

安装 plugin 时，Memoh 需要处理：

```text
plugin.yaml
  -> install skills
  -> create MCP draft/connections
  -> register ACP profiles
  -> register command manifests
```

卸载或禁用 plugin 时：

```text
disable plugin
  -> hide command manifests
  -> disable ACP profiles
  -> optionally disable skills/MCP connections
```

当前 Codex 可以先作为 built-in plugin 处理：

- 不从 supermarket 安装。
- 在服务启动时注册 built-in `codex` profile 和 commands。
- Web 仍然通过 `/api/commands` 获取，而不是硬编码。

## Supermarket 演进

当前 supermarket 只有：

- `/api/skills`
- `/api/mcps`
- `/api/tags`

建议新增：

```text
GET /api/plugins
GET /api/plugins/:id
GET /api/plugins/:id/download
```

Memoh 本体新增：

```text
GET /supermarket/plugins
GET /supermarket/plugins/:id
POST /bots/:bot_id/supermarket/install-plugin
```

安装 plugin 时不一定立刻创建所有 MCP connection。对于需要 secret 的 MCP，建议先创建 draft 或引导用户进入配置页。

## 与现有代码关系

当前已具备：

- `internal/acpclient`：通用 ACP client，不内置 Codex 命令。
- `internal/acpagent`：profile 化 ACP agent runtime。
- `internal/conversation/flow/resolver_acpagent.go`：`/codex start`/`stop` 路由。
- `internal/command/manifest.go`：UI command manifest registry。
- `internal/command/acp_manifest.go`：内置 ACP agent command provider，目前只暴露 Codex。
- `internal/handlers/commands.go`：`GET /api/commands`。
- `apps/web/src/pages/home/components/chat-pane.vue`：`/` 提示从 `/api/commands` 动态加载。

后续需要新增：

- 后续 plugin manifest parser/installer。

## 风险与取舍

### 不要让 command 执行逻辑只存在前端

前端只能展示和插入 command。真正解析和执行必须在后端，因为 IM、Desktop、Web 都会发送同样的文本命令。

### Plugin 不是任意代码执行

Plugin manifest 是声明式的。它可以声明 command、skill、MCP、ACP profile，但执行仍走 Memoh core 的受控路径。

### ACP runtime 保持 core 能力

ACP 本身不要做成 plugin。Codex/Gemini/Claude 是 plugin；ACP client/runtime 是 Memoh core。

### Command manifest 只解决提示，不代替权限

命令是否显示不等于有权限执行。执行时仍要走 bot/session access check、workspace backend check、tool approval 或后续 permission broker。

## 分阶段实现建议

### 阶段 1：Command manifest API

- 新增 command registry。已实现。
- 新增 `/api/commands`。已实现。
- 注册 built-in Codex commands。已实现。
- Web `/` 提示改为读取后端。已实现。

不改 supermarket，不做 plugin install。

### 阶段 2：Built-in plugin 模型

- 定义 Go 侧 `PluginManifest`。
- 将 Codex profile 和 command 改成 built-in plugin manifest 注册。
- Web command manifest 中带 `plugin_id/plugin_name`。

### 阶段 3：Supermarket plugins

- supermarket 新增 `plugins/`。
- Memoh 支持 list/install plugin。
- 安装 plugin 后注册 skills、MCP、ACP profiles、commands。

### 阶段 4：权限与状态

- command manifest 增加 disabled reason。
- ACP command 根据 active task 状态动态调整描述。
- plugin 权限、workspace backend、secret 配置状态进入 command availability。
