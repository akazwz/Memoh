# ACP notification queue overflow: verification evidence

Screenshots for the PR fixing ACP disconnects when acp-go-sdk's notification queue overflows.

- `before-main.png`: main. A mock ACP Agent writes 5000 thought chunks and 5000 message chunks in one write; the turn fails.
- `after-fix.png`: this branch. The same turn completes; the content is still there after a reload.
