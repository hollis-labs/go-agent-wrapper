# go-agent-wrapper Roadmap

Status as of v0.9.0 (2026-09-04). See
[CHANGELOG.md](./CHANGELOG.md) for what landed.

## Publish blockers

None. Every dependency is available at the version required by `go.mod`, and
the module contains no local `replace` directive. Release publication remains
a deliberate manual operation after independent review.

## Deferred this pass

### Concrete adapters

- **PTY adapter** — `Wrapper.Run` dispatches the `pty` runtime token to
  `Capabilities.PTY=true`, but no PTY-shaped concrete adapter ships in
  v0.9.0. Claude has `provider.NewClaudeAdapterPTY()`, Codex has a PTY
  shape too — both are mechanical follow-ons to the existing
  streaming-stdio / jsonrpc-stdio adapters.

### Event taxonomy

The full kind set from
`cli-runner-wrapper-architecture-2026-05-26.md` is defined in
`go-runtime-events`. Headless semantic coverage now includes:

- `agent.tool_result` from agentkit's provider typed-event callback.
- `agent.subagent_spawn` from provider typed events (e.g. Claude `Task`).
- `agent.permission_requested` / `agent.permission_resolved` for
  server-initiated JSON-RPC requests where the direct client maps them.
  A configured best-effort responder now produces both events in every client,
  validates the exact provider-offered option ID, and answers with ACP selected
  or cancelled. With no responder, Claude, Codex, OpenCode, and Pi retain their
  cancelled/event behavior; Copilot retains method-not-handled and no events.
- `session.processing` / `session.idle` around observed turn boundaries.
- `session.heartbeat` from provider typed heartbeats, plus optional
  wrapper-synthesized heartbeats via `Config.HeartbeatInterval`.

Remaining validation: exercise the provider typed-event paths against
live Claude/Codex/OpenCode binaries, especially JSON-RPC approval shapes.

### Policy observation — settled boundary

`Config.PolicyObserver` is intentionally post-hoc. It turns an already-emitted
tool-use observation into an advisory `policy.Finding` and a correlated legacy
`policy.*` event. The wrapper cannot generally intercept native CLI tools before
their side effects, so this seam will not grow rewrite-back or block semantics.
Hosts retain their own authoritative pre-execution gates.

ACP `session/request_permission` is the narrower exception: the protocol lets a
child block waiting for an answer. `ACPBestEffortPermissionRequestResponder`
now supplies that answer, but only for providers and operation classes that
actually issue the request. It remains separate from `PolicyObserver`; the
README coverage table records the measured bypasses and limitations.

### Filters

`Config.Filters` now invokes `Pipeline.Process` for `agent.delta` text,
tool-use envelopes, `agent.tool_result` previews, and stdout/stderr
command-output observations. Repairs replace wrapper-emitted event
payloads only; they do not rewrite child execution. The wrapper also
ships `filters.RepairPipeline`, which adapts concrete
`go-harness-filters/repair` repairers such as
`MissingClosingDelimiterJSON` into `Config.Filters`.

### Tachyon `cmd/agent-wrap` reference CLI

The architecture doc proposes a standalone binary:

```bash
tachyon-engine wrap --pty -- claude
agent-wrap --pty -- claude
```

A `cmd/agent-wrap` in this module would dogfood the library end-to-end
and give us a PTY-test surface independent of any one app. Per the
alignment doc, the userland distribution lives in Tachyon — this
module's `cmd/agent-wrap` is the reference shape.

### Sandbox enforcement on adapter runtime

`Sandbox.Apply` runs against `session.Health().PID` after Start. For
the subprocess-per-turn adapter runtime, PID is 0 between turns — the
applier has no live child to constrain. Pre-spawn enforcement on that
runtime belongs in `agentsessions.StartOptions.Profile`; the wrapper now
exposes `Config.SandboxProfile sandbox.Profile` and forwards it. Keep
`Config.Sandbox` for long-lived PID post-start appliers.

### Stdio fidelity per runtime

`streamWriter` documents this: agentkit's `Fanout` writer is "session
output" (post-parse, formatted) for non-PTY runtimes — the raw
child-stdout bytes flow through `runner.Run` and don't reach the wrapper
unchanged. PTY runtime is the path for byte-exact stdout. A future
revision could either:

- read directly from the underlying `*os.File` stdout pipe (requires
  agentkit changes);
- accept the existing fidelity and rename the events to reflect what
  they actually carry ("session_output.*" rather than "stdout.*").

Lean toward the agentkit change since the architecture doc clearly
calls for byte-exact raw events.

## Open design questions

The former event-correlation question is resolved by
`runtimeevents.Emitter.EmitReturning` in go-runtime-events v0.1.2; callers no
longer need to pre-generate an ID through `WithID` merely to establish a parent.

1. **Filter policy-observation boundary.** `classifybridge` lives in this module
   and depends on `go-harness-filters/classify`. If filter consumers
   (Nanite/Torque/Tether) want filter-driven policy without the wrapper,
   they'd need this bridge in a neutral location. Extract to a
   third-party `go-policy-decisions` module? Or accept the wrapper
   dependency and document it? Revisit after first real app integration.
2. **TurnID semantics on adapter runtime.** The subprocess-per-turn
   adapter runtime is genuinely turn-shaped (each `SendInput` spawns a
   fresh child). Our `turn.*` events align well there. For long-lived
   runtimes (streaming-stdio, jsonrpc-stdio, http-sse), turns are
   adapter-defined and depend on observing turn-terminal stream events
   correctly — currently `EventDone`/`EventUsage`/`EventError` close a
   turn. Validate this matches Claude/Codex/OpenCode reality once we
   exercise them with live binaries.

## Release readiness

- Per-package `doc.go` files exist for every subpackage; spot-check
  they read well as godoc.
- `examples/claude-stream` is a runnable native streaming example with
  first-turn delivery, JSONL events, and signal-aware shutdown.
- CI performs format, vet, compatible pinned lint, full race tests, and
  vulnerability checks with the exact Go 1.26.6 version declared in `go.mod`.

## Deferred release automation

There is no workflow that tags or publishes a release. Keep publication manual
until the project has an explicit provenance and approval design; do not infer
release authority from a green validation workflow.

## Related docs

- Original architecture: `chrispian/inbox/cli-runner-wrapper-architecture-2026-05-26.md`
- Filters companion: `chrispian/inbox/harness-filters-directives-normalization-2026-05-26.md`
- Agentkit alignment: `chrispian/inbox/agentkit-wrapper-alignment-review-2026-05-26.md`
- Next-session handoff: `chrispian/inbox/cli-wrapper-implementation-followups-2026-05-26.md`
