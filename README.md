# Codex checkpoint verification

Implementation: `287941be4` (`fix/codex-session-checkpoints`).

Agent verification on 2026-09-14. This is not human QA.

Environment: the repository's development Compose services, isolated as
`memoh-codex-checkpoint`, with the current checkout bind-mounted into Server and
Web, the current workspace image, PostgreSQL, and real Codex CLI 0.154.0.
Web: http://localhost:28082/bot/codex-checkpoint-qa.

The model endpoint was a local deterministic Responses API fixture with a fake
API key. It answered from the native request history and returned a compaction
item for compaction requests. This validates the Memoh/Codex persistence path,
not a live provider's inference quality or OAuth behavior.

| Interaction | Database and native-state observation |
|---|---|
| Send a first message containing CHECKPOINT-ORCHID-742 | 13 native records published; the last record was this turn's task_complete. |
| Stop the native process and move the entire CODEX_HOME, including JSONL and SQLite indexes, out of its configured path; continue the same session in the UI | Same native thread ID restored. The model request contained the original user message and the new question as separate native history items. 23 total records, preserving the original prefix. |
| Run /compact in the UI | 30 records published after the terminal compaction record. An initial fixture response lacked the required compaction item; that failed operation retained the previous 23-record publication. The corrected fixture succeeded. |
| Start a delayed turn and click Stop | Publication remained at 30 records. |
| Continue after compaction and stop | Native request contained the compaction item, excluded the stopped turn, and recovered the remembered code. Publication advanced to 43 records. |
| Create a fork and send its first message | Independent native thread and checkpoint, 55 records. The source remained at 43 records. |
| Stop the native process and move the entire CODEX_HOME again; continue the fork in the UI | Same fork native thread ID restored, remembered code retained, fork advanced to 65 records; source remained at 43. |

Source native thread: `01a0a0fa-1f9e-7053-93f3-0787c30dfb79`.
Fork native thread: `01a0a102-3c39-7961-9c01-ecf6405dfb32`.

Screenshots are actual application captures, inspected after capture:

- [First completed turn](01-first-turn.png)
- [Continue after losing native files and indexes](02-cold-restore.png)
- [Continue after compaction and stop](04-after-compact-stop.png)
- [Continue a new fork](05-fork.png)
- [Continue the fork after losing native files and indexes](06-fork-cold-restore.png)

Checks passed: Codex and application/core Go tests; Codex and session-store race
tests; real PostgreSQL session-store tests with MEMOH_TEST_POSTGRES_REQUIRED=1;
affected-package golangci-lint; handler/server/error tests; 75 Web API error
tests. No database schema or SQL query was changed.
