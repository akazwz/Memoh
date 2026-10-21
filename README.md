# Codex 恢复 thread 撞上写锁：验证截图

dev 环境，用外部进程占住会话 thread 的写锁后发消息。

- `released-recovered.png`：等待期间锁被释放，接回原 thread，上下文完整。
- `held-busy.png`：锁一直被占用，等满 90 秒返回 busy，没有新开 thread。
