# OpenCode Go integration

Memoh supports OpenCode Go as the `opencode-go` client type. Select **OpenCode Go**
under Settings → Providers, enter a Go API key, enable the provider, and import
and enable the desired models. The default base URL is
`https://opencode.ai/zen/go/v1`.

## Dependency and ownership

Twilight ([PR #49](https://github.com/felinics/twilight/pull/49)) owns the
model-to-protocol routing, request header support, and HTTP wire formats. Like
OpenCode itself, it sends every model to Chat Completions except a small table
of models documented on Responses or Anthropic Messages, so a newly published
model works without an SDK update. Its Go provider also adapts requests to how
Go's routes behave, such as padding `reasoning_content` on replayed tool calls,
so Memoh sends every request through it. Memoh reads the routed protocol from
`ProtocolForModel` to choose reasoning options and prompt caching. Go models take
only the request's reasoning effort; Claude-specific thinking configuration is
not applied.

Memoh owns `x-opencode-session`: native turns use the owning Thread ID, including
subagent turns; title generation and compaction use that same conversation ID.
Standalone jobs and model probes receive independent job IDs. Request contexts
carry the session without changing shared provider headers. Requests identify
the application with Memoh's normal User-Agent.

## Catalog and migration

`conf/providers/opencode-go.yaml` contains the models in the
[Go endpoint table](https://opencode.ai/docs/go/#endpoints) except time-limited
free models, with context windows and input capabilities from
[models.dev](https://models.dev/api.json), checked on 2026-09-29. Most entries use
provider-managed reasoning defaults; the Luna entry exposes its supported
off/low/medium/high/xhigh controls. The initial integration does not advertise
unverified reasoning controls for other Go models. Manual custom providers have
conservative text/tool/reasoning discovery defaults; the preset supplies richer
catalog metadata.

Migration `0160_opencode_go` extends the provider type constraint. The canonical
initial schema includes the same type. Rollback refuses to proceed while Go
providers exist, because converting a provider with mixed protocols to a single
legacy type would break its models. Remove those configurations before downgrading.

## Follow-up

- When Go documents a new Responses or Messages model, add it to Twilight's
  exception table and to the preset together. Do not infer protocols from model
  name prefixes.
- Expand reasoning controls after verifying each model's Go wire contract.

Automated coverage includes all three wire paths for generation and streaming,
reasoning/cache decoration, tool continuation session stability, concurrent
conversation isolation, native parent/child session ownership, standalone probes,
and the Completions default for unlisted models. A public model-list connectivity check
does not verify credentials; use **Test Model** and a real chat for that purpose.
