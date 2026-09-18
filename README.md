# #1316 通用 ACP PATH 修复 — 验证截图

环境：`mise run dev`（Web http://localhost:18082，containerd 后端），Bot 工作区内已按 Devin 官方方式安装 CLI（`/data/.local/bin/devin`，PATH 只写在 `~/.bashrc`）。
「修复后」截图均来自 PR 最终代码构建的运行实例。

| 文件 | 内容 |
|---|---|
| `before-01-select-devin.png` | 修复前：聊天里选中通用 ACP（命令 `devin`）→ "The Agent runtime operation failed. Please try again."，接口 500 |
| `after-01-select-devin.png` | 修复后：同样操作，运行实例启动，出现 Devin 上报的模型（SWE-2 High）与会话模式（Code），接口 200 |
| `after-02-devin-reply.png` | 修复后：发送消息，Devin 正常回复 |
| `after-03-command-not-found-en.png` | 命令临时改成不存在的 `devin-missing`：新提示（英文），接口 409 `acp.command_not_found` |
| `after-04-command-not-found-zh.png` | 同上，中文界面 |
| `after-05-terminal.png` | 工作区终端仍为交互式 `/bin/bash`，能找到 `/data/.local/bin/devin` |
