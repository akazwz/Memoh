# ACP 交互改造计划与范围

> 状态：Draft PR #1002；评审修复完成，待重新人工 QA，2026-08-12
>
> 实现说明：[ACP-INTERACTIONS-IMPLEMENTATION.md](./ACP-INTERACTIONS-IMPLEMENTATION.md)
>
> 相关 issue：[#971](https://github.com/memohai/Memoh/issues/971)、[#972](https://github.com/memohai/Memoh/issues/972)、[#974](https://github.com/memohai/Memoh/issues/974)、[#975](https://github.com/memohai/Memoh/issues/975)、[#998](https://github.com/memohai/Memoh/issues/998)

## 1. 这轮解决什么

最初的调查列出了 21 个问题。它们不是 21 个独立功能，真正落到代码里只有两个主题。

### 1.1 Live ACP 交互语义透传

ACP Agent 发出 permission 或 Form 时，Memoh 保留 Agent 给出的选项、字段和结果语义，并把用户选择送回原 callback。这条链只承诺当前 ACP 进程仍在运行。

本轮包括：

- permission options 与 selected option 的端到端透传；
- 无法映射成 read、write、exec 的通用 permission；
- consent 与非 Memoh MCP 的人工确认；
- ACP Form elicitation；
- 用户 Stop 后保留热 runtime，不把主动停止记录成 Agent 失败。

### 1.2 Web ACP controls 与 Agent commands

Web 显示当前 ACP session 实际声明的 modes 和 commands。命令是否可执行由 Server 查询 live SessionPool 后决定，前端缓存只负责显示和补全。

本轮包括：

- Web `/permission` 查看和切换 mode；
- slash picker 展示 `available_commands_update`；
- Agent command 原文发送、opaque 名称、附件和冲突处理；
- stale 或未声明的命令稳定拒绝。

数据流、权威边界、兼容策略和主要文件见[实现说明](./ACP-INTERACTIONS-IMPLEMENTATION.md)。

## 2. 明确不做什么

以下内容不属于这两个主题，或必须等 session resume 一起设计：

- ACP 进程退出或 Server 重启后的 session resume；
- 恢复已经丢失的 permission/Form JSON-RPC callback；
- Native deferred continuation、RunOutput 和跨阶段 asset owner；
- Channel delivery ACK，以及 `requires_ack` / `event_ack`；
- 0131 `user_input_interaction_commands` ledger；
- session recovery、reaper、runtimefence 的重写；
- 全渠道纯文本决策和 Channel control lane；
- 删除 Telegram 现有 ask_user wizard 或审批回调；
- 跨会话审批收件箱；
- ACP schedule/heartbeat、Hermes timeout 等独立问题；
- 把全局 exec 默认策略改成 ask。

因此，这轮没有 Turn protobuf、gRPC、外部 Channel、Desktop 或 0131 migration 的改动。现有 Telegram、Channel、Native ask_user 和旧 recovery 行为保持不变。

## 3. Resume 单独处理

Resume 统一放在 [#998](https://github.com/memohai/Memoh/issues/998)，不在当前分支留下半套恢复状态机。

当前 Memoh 只调用 `session/new`。Codex 和 Claude Code 虽然支持 `session/resume` / `session/load`，但原生 transcript 仍放在进程临时目录。Memoh 的 message 表只保存产品可见历史，缺少 Agent 私有上下文、tool call/result identity、compaction 状态和未完成调用栈，不能替代原生 session store。

#998 需要一起解决：

1. Memoh thread 与原生 ACP session id 的持久映射；
2. Agent 原生 transcript/store 的受保护存储；
3. 单写者 lease、版本和配置指纹；
4. 按 capability 调用 `session/resume`；
5. 进程退出后将旧 callback 标记为 lost/expired；
6. 恢复上下文后让 Agent 重新评估，不盲目重放旧选择；
7. 对副作用记录 intent、started、completed 和 in_doubt。

恢复历史上下文与复活一次未完成的 callback 是两件事。前者可以依靠原生 session store，后者需要 Agent 或协议提供可重绑的执行检查点。

## 4. 21 个问题的归类

| # | 问题 | 处理结果 |
|---|---|---|
| P1 | permission options 没有端到端透传 | 本轮完成，主题 1 |
| P2 | ACP Form elicitation 未实现 | 完成 live 版本；恢复进 #998 |
| P3 | Stop 杀掉 runtime，并显示 Agent failed | 本轮完成 |
| P4 | ACP schedule/heartbeat 静默失败 | 不做，保留 #975 |
| P5 | 决策后的 run/continuation 投影不完整 | 进 #998 |
| P6 | 重启后留下孤儿 pending 行 | 进 #998 |
| P7 | ACP 审批授权只认 runtime owner | 完成 live 授权边界 |
| P8 | 决策投递结果不真实 | durable delivery 进 #998 |
| P9 | 非 MCP 请求存在关联竞态 | 本轮保守兜底；恢复进 #998 |
| P10 | 审批默认姿态不清楚 | 只固定 Codex 姿态，不改全局策略 |
| P11 | workspace-target ACP 策略未接通 | 不做 |
| P12 | Codex approval/sandbox/network 姿态不固定 | 本轮完成 |
| P13 | modes 和 available commands 不可见、不可控 | 本轮完成，主题 2 |
| P14 | 非 Memoh MCP permission 一律取消 | 本轮完成，主题 1 |
| P15 | Channel 审批 UX 不一致 | 不做，不修改 Channel |
| P16 | Web 拒绝不能填写理由 | 本轮完成 |
| P17 | 没有跨会话审批收件箱 | 不做 |
| P18 | Channel 审批 prompt 死代码 | 不做纯清理 |
| P19 | Hermes 自动 deny 与等待时间不一致 | 不做 profile 特判 |
| P20 | consent elicitation 被自动接受 | 本轮完成，强制 review |
| P21 | read 策略可能被输入形状旁路 | 本轮完成：保守分类 + 声明 kind 的显式 deny 映射，generic 通道强制人工决定 |

## 5. 验收状态

已经完成：

- Go 全仓测试、相关 race 测试和定向 lint；
- Web 核心测试与相关 ESLint；
- sqlc、OpenAPI、TypeScript SDK 重新生成；
- Proto、Channel、0131、Resume 等禁止范围核对；
- 旧 HTTP 请求和 0130 additive migration 的静态兼容检查。

仍需人工完成：

- Codex 与 Claude Code 的 Web happy path；
- permission options、Form、Stop、`/permission` 和 Agent commands 的真实交互；
- 有 `TEST_POSTGRES_DSN` 的环境中执行 0129 → 0130 升级演练。

自动化结果不能代替人工 QA。人工验证前，PR 描述必须保留仓库规定的 No human QA 提示。

## 6. 修改纪律

- 与两个主题直接相关的问题可以在本轮修复；
- 依赖 resume 才能成立的工作进入 #998；
- 与 ACP 无关的问题只记录，不顺手修改；
- 每个权威边界只保留一到两个核心测试；
- 生成文件只能通过 sqlc、Swagger 和 SDK 生成任务更新。

这个边界是评审依据。发现新问题时，先判断它属于哪一类，再决定改代码还是记入后续。
