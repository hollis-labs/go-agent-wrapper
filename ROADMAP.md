# go-agent-wrapper Roadmap

Status as of v0.1.0 (2026-05-26). See
[CHANGELOG.md](./CHANGELOG.md) for what landed.

## Publish blockers

1. **Drop local `replace` directives.** Each cross-lib dependency
   (`agentkit`, `go-runtime-events`, `go-harness-filters`, `go-sandbox`,
   `go-runner`, `go-providers`, `go-llm-types`, `go-llm-contracts`) is
   wired via a local path replace in `go.mod`. Before tagging v0.1.0,
   each dep needs a real tag and the require lines need to bump to it.
   Dependency order for publishing:
   1. `go-runtime-events` (no internal deps).
   2. `go-harness-filters` (no internal deps in this module).
   3. `go-agent-wrapper` (depends on both above + agentkit).
2. **No CI publishing pipeline yet.** Folio scaffolded the standard
   `.github/workflows/check.yml` (test + vet + lint + vulncheck); a
   release workflow is the next step.

## Deferred this pass

### Concrete adapters

- **PTY adapter** — `Wrapper.Run` dispatches the `pty` runtime token to
  `Capabilities.PTY=true`, but no PTY-shaped concrete adapter ships in
  v0.1.0. Claude has `provider.NewClaudeAdapterPTY()`, Codex has a PTY
  shape too — both are mechanical follow-ons to the existing
  streaming-stdio / jsonrpc-stdio adapters.

### Event taxonomy

The full kind set from
`cli-runner-wrapper-architecture-2026-05-26.md` is defined in
`go-runtime-events`, but several aren't yet emitted by `Wrapper.Run`:

- `agent.tool_result` — currently no translator mapping. The fake CLI
  in integration tests doesn't emit tool_results either.
- `agent.subagent_spawn` — same.
- `agent.permission_requested` / `agent.permission_resolved` — same.
- `session.idle` / `session.processing` / `session.heartbeat` — no
  state-machine tracking in the translator. Idle / processing would
  derive from agent activity windows; heartbeat from a periodic timer.

### Policy enforcement (the rewrite-back half)

`Config.Policy` currently runs in OBSERVATION mode only: the verdict
surfaces as a `policy.*` event but the child's input/output is
unmodified. Real enforcement (intercepting tool_use before the child
runs it, substituting per `policy.rewrite`, blocking per `policy.block`,
pausing for `policy.approval`) is a bigger session-input-path change.
The architecture doc spells out the disclosure requirements
("Original command / Executed / Reason"); apply those when wiring
enforcement.

### Filters

`Config.Filters` is a Config field but `Wrapper.Run` never calls
`Pipeline.Process`. Add invocation points on `agent.delta` text,
`agent.tool_result` output, and incoming tool_call envelopes once the
pipeline is concrete (`go-harness-filters` repair/normalize rule sets
need to land first).

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
runtime belongs in `agentsessions.StartOptions.Profile` (which the
wrapper doesn't currently plumb from Config). Add a
`Config.SandboxProfile sandbox.Profile` field and forward it.

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

1. **Approval mode event kind.** `policy.ModeApproval` currently maps
   to `runtimeevents.KindPolicyBlock`. A real approval flow (where the
   wrapper pauses the session, asks the operator, and resumes on
   accept) needs:
   - A `KindPolicyApprovalRequested` event kind in `go-runtime-events`.
   - A pause/resume mechanism in `Wrapper.Run` (currently Run blocks on
     `session.Wait` with no pause primitive).
2. **`WithID` option misuse.** `runtimeevents.WithID` lets callers
   pre-generate an event ID for `ParentID` correlation. Duplicate IDs
   in the same session would break correlation. Document the contract
   harder, or expose a safer `EmitReturning(ctx, ...) (id, err)` shape.
3. **Filter policy boundary.** `classifybridge` lives in this module
   and depends on `go-harness-filters/classify`. If filter consumers
   (Nanite/Torque/Tether) want filter-driven policy without the wrapper,
   they'd need this bridge in a neutral location. Extract to a
   third-party `go-policy-decisions` module? Or accept the wrapper
   dependency and document it? Revisit after first real app integration.
4. **TurnID semantics on adapter runtime.** The subprocess-per-turn
   adapter runtime is genuinely turn-shaped (each `SendInput` spawns a
   fresh child). Our `turn.*` events align well there. For long-lived
   runtimes (streaming-stdio, jsonrpc-stdio, http-sse), turns are
   adapter-defined and depend on observing turn-terminal stream events
   correctly — currently `EventDone`/`EventUsage`/`EventError` close a
   turn. Validate this matches Claude/Codex/OpenCode reality once we
   exercise them with live binaries.

## Pre-publish polish

- Per-package `doc.go` files exist for every subpackage; spot-check
  they read well as godoc.
- The `Config.Filters` field still documents "no invocation" in v0.1.0
  — update once filters are wired.
- `examples/README.md` is empty. Drop runnable usage examples in
  before the public tag.

## Related docs

- Original architecture: `chrispian/inbox/cli-runner-wrapper-architecture-2026-05-26.md`
- Filters companion: `chrispian/inbox/harness-filters-directives-normalization-2026-05-26.md`
- Agentkit alignment: `chrispian/inbox/agentkit-wrapper-alignment-review-2026-05-26.md`
- Next-session handoff: `chrispian/inbox/cli-wrapper-implementation-followups-2026-05-26.md`
