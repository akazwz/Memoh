# OpenCode native runtime implementation plan

## Decision and scope

Add OpenCode as a built-in external runtime alongside Codex and Claude Code.
Run the official `opencode serve` executable inside the bot-owned workspace;
use HTTP requests and SSE over the existing bridge tunnel. Do not introduce
an ACP adapter or a JavaScript sidecar.

The first release covers agent configuration, launcher discovery, live model
selection, text/image prompts, streamed text/reasoning/tool activity, tool
approval, user questions, cancellation, native session continuation, and the
Memoh MCP tool gateway. Advanced native branching, undo, and in-flight
steering are outside this initial release and must not be advertised.

Research baseline: OpenCode v1.18.31 (commit
`014614d35b397775e5d397a490fc72368c894ec2`). Use one coherent API surface from
that release and exercise it against the real executable. The public Server
and SDK documentation describe the same native server, not separate engines:

- https://opencode.ai/docs/server/
- https://opencode.ai/docs/sdk/
- https://github.com/anomalyco/opencode/tree/v1.18.31

## Ownership and transport

- Implement `external.Driver` in `internal/agent/runtime/opencode`; keep
  application turn orchestration and decision persistence runtime-neutral.
- Start a private loopback HTTP listener with a random password. Connect
  through `bridge.Client.DialContext`, retaining the host/workspace gRPC/UDS
  boundary. Never publish a container port or expose the native API to Web.
- Use a separate process and MCP mount for each active turn. Persist the
  native session ID with the Memoh thread and reuse the native conversation
  on the next turn. This bounds process lifetime and prevents directory-wide
  MCP configuration from routing another thread's tool calls.
- Keep native state in persistent workspace storage scoped to BotAgent and
  Thread. Temporary launch state is process-owned and removed on teardown.
  Keep credentials out of metadata, logs, and public errors.
- Configure models/providers through the existing credential and agent
  configuration boundaries. Workspace configuration remains available for
  providers not managed by the initial credential UI.
- Check workspace dependency availability without installing on chat or
  model-catalog requests. Installation remains a Manage-authorized action.
- Respect runtime configuration guards, reset/fresh-runtime requests, and
  run ownership. Stop must cancel pending decisions, abort native execution,
  retain partial output, and close the process before releasing the run.
- Memoh owns its canonical conversation and execution ledger. OpenCode owns
  native history. Pass the current composed Memoh context as the per-turn system overlay;
  retain the native message history on resumed sessions instead of replaying it.

## Implementation sequence

1. Add the runtime vocabulary, configuration validation, schema migration,
   credential compatibility, dependency discovery, and backend registration.
2. Add an HTTP/SSE client and process lifecycle, model catalog, persistent
   session mapping, prompt execution, event projection, and cancellation.
3. Connect existing approval/user-input flows and the per-turn MCP mount.
4. Add OpenCode to existing agent setup and chat surfaces, including English,
   Chinese, and Japanese labels. Reuse shared components and generated SDK.
5. Run generation, focused protocol/lifecycle tests, repository checks, and
   the development environment. Exercise the real UI and inspect screenshots.

## Acceptance and verification

- Agent creation and configuration validate without leaking secrets.
- Missing CLI blocks a turn with actionable dependency feedback.
- Model and variant choices come from the configured OpenCode instance.
- A real prompt streams and persists one coherent transcript; images reach
  the selected model when supported.
- Native tools and Memoh MCP tools produce correctly attributed events.
- Approve, reject, answer, and stop actions settle the current run; late
  events cannot attach to another run.
- A follow-up after process restart retains native conversation continuity.
- Two simultaneous threads cannot exchange MCP identities or credentials.
- Dev services run the modified code; real UI interactions and current
  screenshots are required before declaring the implementation complete.

## Progress

- [x] Investigate repository boundaries and upstream native interfaces.
- [x] Record the implementation plan.
- [x] Implement backend and configuration integration.
- [x] Implement Web integration and regenerate contracts.
- [x] Complete implementation and focused automated checks; runtime/UI acceptance waived by the user on 2026-09-16.

## Capability alignment checkpoint — 2026-09-17

The working branch `feat/opencode-native-runtime` is based on upstream main
`1aaef83ff55a9432da4ac7dc631fff36f5e254ab`. This checkpoint preserves ongoing
implementation; the earlier acceptance record does not cover the additions below.

- Keep the reasoning menu's inherited `default` selection separate from its
  resolved display value. Read native model/agent options and inherited agent
  variants; use documented defaults only for explicitly recognized providers.
  Unknown defaults remain unknown rather than being inferred from variant order.
- Connect the existing Plan control to native agents, preserving Plan deny
  rules independently of the selected permission preset.
- Fork through the native HTTP endpoint at the selected completed round.
  Track remapped message IDs so copied history supports nested forks. Branches
  are independent native sessions in an explicit, BotAgent-owned family store;
  unrelated threads retain separate stores.
- Add native command discovery, read commands and manual compaction. Native
  commands receive the current Memoh system context through a temporary native
  instructions file. Compaction must finish successfully before publishing its
  new continuation anchor.

Focused tests passed for inherited effort resolution, model/agent precedence,
unknown/custom-provider defaults, and Plan permission preservation. The actual
OpenCode 1.18.31 executable passed a race-enabled test covering restart/resume,
tool approval, questions, nested forks, a separate branch process sharing the
family store, manual compaction, and continuation after compaction.

Still pending: complete real-UI verification of these added controls, command
execution and cancellation coverage, and the remaining capability comparison
(including native goal and in-flight steering semantics). Full repository lint
currently stops at an unchanged loader-contract violation in
`apps/web/src/pages/home/components/tool-call-diff-panel.vue`. This checkpoint
is not a completed acceptance or a release submission.

## Implemented integration

The Go driver lives in `internal/agent/runtime/opencode`. It launches
`opencode serve`, subscribes before sending a prompt, projects native events
onto the existing transcript, and reconciles the final result with persisted
native messages. Each turn refreshes session permission rules and the Memoh
system context. A resumed session must match its last committed user-message
anchor; a divergent native tail starts a fresh session instead.

The credential flow supports an encrypted API key plus its OpenCode provider
ID, or explicit workspace authentication. Only the provider ID is exposed in
public credential metadata. Workspace login is read from
`/data/.local/share/opencode/auth.json`; native session databases remain under
`/data/.memoh/opencode/<agent UUID>/<thread UUID>`. Missing executables produce
the existing dependency feedback without installing anything.

Web setup, agent settings, chat model selection, authorization drafts,
onboarding, and schedules recognize `opencode`. Permission choices apply on
the next turn: ask for native tool approval, inherit workspace rules, or allow
native tools. Memoh MCP tools continue through Memoh's own permission flow.
Migration `0153_opencode_runtime` and the canonical schema admit the new
runtime; the down migration refuses to discard existing OpenCode records.
Swagger and the TypeScript SDK have been regenerated.

Native questions use the existing Memoh question UI. Unsupported question
shapes, including a single predefined choice, are rejected rather than
silently changing the allowed answers. In-flight steering, native undo, and
native branching remain outside this implementation.

## OpenCode Go setup

OpenCode is the agent runtime. OpenCode Go and OpenCode Zen are separate model
services supported by that runtime. Following the [official Go setup](https://opencode.ai/docs/go/#how-it-works),
new OpenCode Agents default to **OpenCode Go** and API-key authentication.
Users paste their subscription key, save it, and choose a model from the
native catalog. They do not need to enter a provider ID or a Base URL.

The named service selector maps Go to `opencode-go` and Zen to `opencode`.
OpenCode itself supplies the service endpoints and per-model protocol; Memoh
does not force Go through a generic OpenAI-compatible endpoint. Anthropic,
OpenAI, Google, and OpenRouter also have named choices. Custom provider IDs
remain available for providers configured in the workspace. Workspace login
and endpoint overrides are under Advanced. Existing custom settings remain
readable there.

Changing the service clears the pending key, default model, and endpoint.
The cleared configuration must be saved successfully before a new service's
key is stored. An unchanged query refresh no longer replaces unsaved form
fields: UI verification found this could restore the previous service's
model and endpoint during a switch.

On the running `memoh-opencode-qa` environment at `http://localhost:48082`,
the Agent settings flow was exercised with a disposable test key: create an
Agent, save Go authorization, load the native Go catalog, select a model,
reload and reopen the settings, switch to the custom fixture provider, and
switch back to Go. Database inspection confirmed `provider_id: opencode-go`,
an `opencode-go/...` model, and no old endpoint. Custom model discovery also
worked. This verifies configuration and catalog discovery, **not authenticated
Go inference**; no real Go subscription key was used.

Screenshots in `/tmp/memoh-opencode-qa/`: `25-go-key-setup.png` shows the new
default form; `26-go-model-catalog.png` shows native Go choices;
`27-go-saved-refreshed.png` shows the persisted choice after reloading;
`28-custom-provider-catalog.png` shows the custom provider and endpoint.
`31-new-bot-go-default.png` shows the matching Go default in the new-Bot flow
in dark mode; the form was inspected without creating another Bot.

Focused ESLint and the Agent dependency test passed after these UI changes.
The full Web typecheck still fails (438 diagnostics in this run, none reported
in the changed authorization/detail components or new OpenCode components).
The UI contract check remains blocked by the existing `animate-spin` violation
in `tool-call-diff-panel.vue`. These checks are not reported as passing.

## Dependency catalog rollout

The managed installation recipe belongs to
[`felinics/supermarket`](https://github.com/felinics/supermarket), not this
repository. A companion local checkout at `../Memoh-opencode-supermarket`
contains `registries/memoh/dependencies/opencode/`, the matching
`registries/memoh/apps/opencode/` entry, refreshed dependency and registry
locks, and recipe/catalog tests. The App entry is required by Web onboarding
and Manage installation; the dependency recipe alone is not sufficient. The recipe installs the official npm package with
lifecycle scripts disabled, selects its native platform binary, verifies
execution, and uses the existing staged install/update/rollback mechanism.
Linux x64 uses the baseline build so AVX2 is not required.

The public catalog still needs that companion change published before a
normal Manage installation can discover OpenCode. No remote publication or
PR has been performed. Existing workspace installations can already be
discovered through the read-only launcher resolver.

## Verification record — 2026-09-16

Completed checks:

- `mise run sqlc-generate` and `mise run sdk-generate` succeeded.
- Go tests passed for agent, chat, handlers, settings, schedule, bot agents,
  credentials, workspace dependencies, and core registration.
- Focused race tests passed for the new runtime, configuration, and
  credentials. Existing model preference, schedule, and inline question
  response tests now include OpenCode.
- An opt-in integration test ran the real OpenCode 1.18.31 executable with
  a deterministic local model endpoint. It exercised native HTTP/SSE,
  execution of `printf`, approval, question replies, and session continuation
  after terminating and restarting the native server. This verifies the
  native protocol; it does not substitute for the Memoh UI or a live provider.
- Five focused Web test files passed (16 tests). ESLint passed for the
  modified Web files. English, Chinese, and Japanese JSON passed duplicate-key
  checks.
- The companion recipe suite passed 91 tests, including installation failure
  recovery, update, and removal. A real npm download and managed installation
  passed on macOS arm64 and executed OpenCode 1.18.31. Linux workspace execution also passed during the UI verification below.

The native executable test can be repeated without provider credentials:

```sh
OPENCODE_INTEGRATION_BINARY="$(command -v opencode)" \
  go test -race -v ./internal/agent/runtime/opencode -run TestLiveServer -count=1
```

Repository-wide checks have existing failures: the UI contract guard reports
`animate-spin` in `tool-call-diff-panel.vue`; the normal ESLint invocation
encounters ambiguous TypeScript roots from `.claude/worktrees`; full Go lint
reports the unused native `mergeMessages` method. Web type checking fails in
the unchanged HEAD checkout as well (356 normalized diagnostics, chiefly
shared UI aliases and SDK rootDir). Comparing against that checkout found no
new diagnostic content from this change.

## Real application verification — 2026-09-16 (America/Los_Angeles)

The user subsequently requested UI acceptance verification. OrbStack was
restarted with their authorization, and Docker recovered. The existing
`memoh-dev` stack belonged to another checkout, so acceptance used a separate
`memoh-opencode-qa` Compose project derived from `devenv/docker-compose.yml`.
Server and Web mount this checkout at `/workspace`; PostgreSQL, pgvector,
ConnectIt, Server, and Channel passed health checks. Web served the current
Vite source at `http://localhost:48082`; Server listens at
`http://localhost:48080`. Migration 153 applied with `dirty = false`.

QA used the real OpenCode 1.18.31 Linux executable inside the bot's managed
workspace, native HTTP/SSE over the workspace bridge, and a deterministic local
OpenAI-compatible model endpoint. The model fixture drives predictable text,
native tools, and questions; it does not establish live-provider compatibility
or model quality. The local Supermarket catalog served the companion changes.
No remote catalog publication was performed.

The following flows were exercised in the real Web UI:

- Onboarding created **OpenCode UI QA**, saved an encrypted test API key with
  provider ID, installed OpenCode into its native workspace, and enabled the
  Agent. Settings retained the provider ID without returning the key; changing
  Base URL and default model took effect on subsequent requests.
- Chat loaded the native model catalog. Model and reasoning-variant selection
  worked, including selecting the high variant at a 390 px viewport.
- Text streamed and persisted. Native command approval executed `printf`;
  rejection produced an error tool result without executing the command.
  Native question selection returned the chosen answer to OpenCode.
- Refreshing the page and sending a follow-up retained native conversation
  history. Stopping a long response disconnected the model request, preserved
  partial text, and allowed the next turn. No native `serve` process remained
  after the completed turns.
- Two Memoh threads acquired distinct native session IDs. A fresh thread did
  not receive the first thread's conversation. Both executed Memoh's
  `list_sessions` MCP tool; persisted tool messages belonged to the matching
  run and thread. The run ledger had no active runs after completion.
- A disabled schedule was created with OpenCode selected, then reopened in the
  editor. PostgreSQL retained `runtime_type = opencode` and the expected Agent
  ID. Scheduled firing was not exercised.
- Screenshots were captured and inspected at 1280 × 900 in light/dark modes
  and 390 × 844 in dark mode. Chat, model controls, approval, and question UI
  remained usable at the inspected sizes.

Acceptance and the subsequent icon correction exposed the following defects,
which were fixed:

1. The dependency existed without its matching App descriptor, so onboarding
   failed with `App "memoh/opencode" not found`. Adding the App and rebuilding
   both catalog locks allowed the existing onboarding retry to finish without
   creating another bot. A regression assertion requires each official direct
   Agent runtime to have an App that declares its dependency.
2. The standalone SVG used `currentColor`, which renders black when loaded as
   an image and became invisible on dark backgrounds. The catalog icons now
   use an explicit light backing and dark mark. The inline Web icon continues
   to inherit the UI text color. Updating the installed App through its real
   management UI completed successfully; fresh screenshots confirmed both
   App and dependency icons are visible in dark mode.

3. The initial mark came from OpenCode's model-provider icon asset, not its
   product logo. After checking [the official brand assets](https://opencode.ai/brand),
   Web, the App descriptor, and the dependency descriptor now use the official
   square product mark from `packages/console/app/src/asset/brand/` at v1.18.31.
   The Web SVG retains the exact two paths with theme-inherited color; catalog
   images retain the official light colors on a light backing. The original
   icon screenshots above establish flow behavior, not correct branding.

   The installed App was updated again through Manage using the final padded
   catalog asset. `29-product-app-light.png` and `30-product-app-dark.png`
   show the current App and dependency marks in both themes.
   `23-product-icon-dark.png` and `33-product-icon-light.png` show the inline
   mark in the real sidebar, composer, and Agent menu.
   `32-icon-size-calibration.png` is a separate diagnostic comparison at
   12/14/16/32 px, inspected for clipping, weight, and alignment; it is not an
   application screenshot. The focused catalog test passed after the final
   icon revision: 3 tests, 832 assertions.

The companion catalog checks passed again after the fixes: 14 tests and 1,064
assertions across committed registries, dependency registry, and dependency
HTTP tests. Existing repository-wide check limitations are recorded above.

Reviewable local evidence is in `/tmp/memoh-opencode-qa/`:

| Evidence | What it shows |
| --- | --- |
| `01-create-authorized.png`, `05-agent-settings.png` | OpenCode selection, saved authorization, provider ID and runtime settings |
| `02-install-before-fix.png`, `03-install-complete.png` | Missing-App failure and successful onboarding retry |
| `04-model-picker.png`, `06-chat-connected.png` | Native model catalog and completed HTTP-backed reply |
| `07-native-approval.png`, `08-approved-result.png` | Pending approval and actual command output |
| `10-native-question.png`, `11-question-answered.png` | Native question and submitted answer |
| `12-resumed-conversation.png` | Conversation retained after browser reload and native process restart |
| `13-streaming-before-stop.png`, `14-stopped.png` | Streaming response and persisted partial reply after stop |
| `15-mcp-result.png`, `16-isolated-thread.png` | MCP result and independent conversation in the second thread |
| `17-narrow-dark-model.png`, `18-narrow-dark-chat.png` | Usable model/reasoning picker and chat at 390 px in dark mode |
| `19-installed-app.png`, `21-installed-dark-fixed.png` | Installed 1.18.31 and icon visibility before/after the dark-mode fix |
| `20-schedule-saved.png` | Reopened, disabled OpenCode schedule |
| `runtime-evidence.txt`, `model-requests.jsonl`, `services.json` | Thread/run attribution, model disconnect, and service health evidence |

These are agent-operated checks, not human QA. Workspace-auth login, a live
provider, scheduled firing, and every existing ACP/Codex/Claude Code flow were
not exercised through this UI run. Changes remain local and uncommitted in
both repositories. The public catalog still requires the companion App and
dependency changes to be published before normal public-catalog installation.
