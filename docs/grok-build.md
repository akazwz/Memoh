# Grok Build runtime

Grok Build is a direct external Agent (`runtime: grok`), separate from the
Grok chat model provider and from generic ACP profiles. Each Bot Agent requires
its bot-owned native workspace and an explicitly installed Grok executable.

## Setup

1. Install Grok Build in the Bot's native workspace. The integration baseline
   is the actual executable `grok 1.0.30 (04b7ffed98c6)`, distributed as
   `@xai-official/grok@1.0.30`. Check the executable version, not only npm metadata.
2. Add a **Grok Build** Agent in the Bot settings or select it during Bot creation.
3. Choose **Grok account** for device-code login, or **API key** for an xAI key.
   The login can be started before creating a Bot; claiming it binds the encrypted
   credential to the selected Bot Agent.
4. Select the Agent in chat, or use `/new grok`. Model and reasoning-effort choices
   come from the installed CLI. Grok supports chat, discuss and scheduled runs.

Executable discovery is read-only. Chat and login never install a dependency.
The workspace image does not bundle Grok, and the host's Grok login is not used.

### Managed dependency publication

The public Supermarket catalog had no `grok` recipe when this integration was
prepared. [The publication payload](grok-build-dependency/grok/dependency.yaml)
is provided separately; it is not embedded or silently installed by the Server.
Publish that recipe and its scripts in Supermarket before advertising one-click
installation. It depends on the catalog's existing `node` recipe, pins 1.0.30,
disables npm lifecycle scripts, verifies the actual binary, and installs the
verified native executable under the dependency manager's version directory.

Until publication, a workspace administrator can install the official pinned
package into the workspace toolkit. The resolver recognizes a preinstalled
`grok` without a warm catalog cache. Never install or log in on the Server host
as a substitute for the Bot workspace.

## Runtime and state ownership

- Each operation starts `grok --no-auto-update agent --no-leader stdio` with a
  clean environment and a private lease `GROK_HOME`. Native file and terminal
  callbacks are not advertised; Grok owns its workspace tools. Memoh tools use
  a fresh HTTP MCP mount carrying the current run's identity and permissions.
- OAuth processes for the same Bot Agent and credential share `GROK_AUTH_PATH`,
  allowing Grok's native file lock to serialize single-use refresh tokens.
  Refreshed tokens return to encrypted storage with credential-version and
  configuration-generation checks. Authentication files are excluded from
  native session snapshots.
- Only native `sessions/` and `workspace/sessions/` trees are captured after the
  writer has exited. Opaque bytes are chunked into bounded JSONL records.
  Immutable storage revisions isolate mutable summaries, tool state and
  compaction results from the previous published checkpoint.
- The application publishes a staged checkpoint in the canonical round
  transaction. Failed staging preserves the old state and reports an error.
  Load replay never becomes a new assistant message. Bot backups retain visible
  history but do not contain runtime checkpoints or fork seeds.
- Fork creation atomically copies the expected published checkpoint with visible
  history. Its target-owned seed survives source deletion and workspace rebuilds.
  The first target lease invokes native fork, using a confirmed native prompt
  boundary. Missing or incompatible anchors are rejected. Copied anchors from a
  different native session are not guessed from visible message positions.
- `/compact` owns the normal run slot, awaits the native RPC, and publishes a
  snapshot without creating a synthetic chat message.

Permissions use Grok's `ask`, `auto` and `always-approve` values. Auto lets Grok
evaluate tool calls and ask when it cannot approve automatically. Plan mode is independent
(`default` or `plan`). Changes apply on the next operation. Native approval,
questions, plan confirmation and MCP elicitation use Memoh's existing decision
cards. Stopping a turn cancels pending decisions and drains the original prompt's
terminal response before releasing its resources.

Follow-up inputs remain in Memoh's queue for a new run. Native interjection is
not advertised because its delivery acknowledgement does not establish durable
consumption. Native goal, rewind and file rollback controls are not exposed.
Images use the CLI's advertised image support; otherwise the existing authorized
file fallback applies.

## Verification boundary

Automated tests cover protocol ordering, replay filtering, approval option IDs,
question answers, cancellation, OAuth polling/redaction, immutable revisions,
failed-stage rollback, and independent fork seeds in PostgreSQL.

Agent-operated UI verification with an authenticated Grok account and CLI 1.0.30
covered model/effort selection, subscription turns, native process and Server
restart recovery, Auto and Ask permissions, tool approval, questions, plan
approval/abandonment, cancellation then continuation, compaction followed by a
new process, and forking at an earlier completed turn after compaction.

This is not human QA. API-key inference, token refresh after expiry, and full
workspace rebuild recovery still require separate credentialed verification.
The published dependency recipe also remains a distribution prerequisite.

Protocol references: [official source](https://github.com/xai-org/grok-build)
and [headless scripting](https://docs.x.ai/build/cli/headless-scripting).
