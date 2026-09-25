# QA evidence: native sessions stay in /data

Local dev environment (http://localhost:18082), after migration 0156 removed the database snapshots and the server was restarted.

- `01-codex-cold-resume.png`: an existing Codex session resumes from its `/data` rollout after a server restart and still recalls the earlier code word.
- `02-codex-native-history-lost.png`: with the rollout file removed, Codex starts a new thread and the `native_history_lost` notice is shown and persisted.
- `04-claude-native-history-lost.png`: with the transcript removed, Claude Code starts a new session and shows the same notice.
