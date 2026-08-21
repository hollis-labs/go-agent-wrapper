# Changelog

All notable changes to go-agent-wrapper are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## v0.2.0 — 2026-08-21

Dependency-currency + core-strengthening release. No breaking change to
this repo's own exported surface; `Config` gains two new optional fields
(additive, keyed-literal compatible).

### Changed

- **Bumped `agentkit` require from `v0.1.0` to `v0.3.0`** in `go.mod` to
  match what the local `replace github.com/hollis-labs/agentkit =>
  ../agentkit` block was already resolving to on disk (`agentkit`'s HEAD
  at the time, `v0.3.0-1-g5b8aaad`, one docs-only commit past the
  `v0.3.0` tag). Zero source changes required: this repo only imports
  `agentkit/agentsessions`, and `git diff v0.1.0 v0.3.0 --
  agentsessions/` inside `libs/agentkit` is byte-for-byte empty. The
  renamed `agentlaunch` symbols from `agentkit/CHANGELOG.md` v0.2.0/v0.3.0
  (`RenderFrontEnd`→`MissingPolicy`, `FrontEndAutonomous`→`PolicyError`,
  `FrontEndInteractive`→`PolicyCollect`, `RenderRequest.FrontEnd`→
  `RenderRequest.OnMissing`, the `PreparedPlantContext` field renames)
  don't apply — `agentlaunch` is a package this repo never touches.
- Confirmed `go-runner v0.5.0` (indirect) is already current; no version
  change needed there.
- `policyModeToEventKind`: `policy.ModeApproval` now maps to its own
  `policy.approval_requested` event kind instead of collapsing into
  `policy.block` — the "no dedicated approval-request event yet" gap
  noted in the v0.1.0 skeleton comment is closed.
- Per-turn bookkeeping (`currentTurnID`, `turn.started`/`session.processing`
  bracket, `turn.completed`/`turn.failed` → `session.idle`) is now
  centralized in shared `emitObserved`/`emitProviderObserved` closures
  inside `Wrapper.Run`, so both the `EventFanout` (`llmtypes.StreamEvent`)
  path and the new `TypedEventCallback` path share one turn-state
  machine instead of each tracking it separately.

### Added

- **`Config.Filters` is now actually invoked**, closing the "skeleton
  scope — Filters is not invoked in this pass" gap from v0.1.0:
  - `wrapper/filter_payload.go` — `filterStreamEvent` runs agent-text
    deltas and tool-use envelopes through the pipeline before
    translation; `filterPayload`/`filterToolResultPayload` repair the
    `tool_result` content-preview on the already-translated
    `runtimeevents` payload.
  - `wrapper/io_streams.go` — `streamWriter` gained a `filter
    filters.Pipeline` field; raw stdout/stderr bytes and each line are
    run through `filterBytes` before being emitted as
    `stdout.raw`/`stdout.line`/`stderr.raw`/`stderr.line` (the child's
    actual stdin/stdout/stderr are untouched — only the emitted event
    payload is repaired).
  - `filters/repair_pipeline.go` (new) — `RepairPipeline` /
    `NewRepairPipeline` adapt a `go-harness-filters/repair.Repairer`
    chain to the wrapper's `Pipeline` interface; syntactic repairs
    replace content, semantic-changing repairs surface as `Notes` only
    and leave content untouched.
- **`wrapper/event_translator.go`'s `translateProviderEvent`** — bridges
  richer `go-providers/provider/events.Event` frames (`ToolResult`,
  `SubagentSpawn`, `Heartbeat`) that the legacy `llmtypes.StreamEvent`
  surface can't represent, wired through a new
  `agentsessions.StartOptions.TypedEventCallback` in `Wrapper.Run`.
- **`Config.SandboxProfile`** (`go-sandbox/sandbox.Profile`) — forwarded
  to `agentsessions.StartOptions.Profile` so runtimes that support
  pre-spawn `go-sandbox` wrapping can constrain the child before exec.
  Zero-value (empty ID) disables this path.
- **`Config.HeartbeatInterval`** — when positive, `Wrapper.Run` emits
  wrapper-synthesized `session.heartbeat` events at this cadence for the
  life of the run; adapter-provided heartbeats still surface separately
  via `translateProviderEvent`.
- **`agentsessions.StartOptions.JsonRpcRequestHook`** wiring — server-
  initiated JSON-RPC requests (e.g. a provider-side permission prompt)
  now emit a correlated `agent.permission_requested` /
  `agent.permission_resolved` pair. No approval handler is configured
  yet, so the resolved event always carries `allowed: false` and the
  hook returns a JSON-RPC error — an actual approval-handling path is
  still open work.

### Notes

- The local `replace github.com/hollis-labs/agentkit => ../agentkit`
  block is intentionally still present in this tagged release — see the
  comment above the `replace (...)` block in `go.mod`. Nanite's own
  `go.mod` needs a matching local `replace` pointing at this repo (task
  `03` in Nanite's `agent-host-acp` batch) until this repo has enough
  tagged history to be a normal module-proxy dependency; both replaces
  stay until that stabilizes. `go-harness-filters` and
  `go-runtime-events` keep the original v0.1.0 "drop before tagging"
  discipline — nothing depends on those two staying.
- Everything under "Added" above (except the agentkit pin bump under
  "Changed") landed in the untagged commit immediately preceding this
  tag (`a248ab4`, "Strengthen headless wrapper core") and had not
  previously been changelogged; this entry is the first release note
  for that work.

## v0.1.0 — 2026-05-26

Initial cut. Full launch path wired against agentkit v0.1.0 +
go-runtime-events v0.1.0 + go-harness-filters v0.1.0. 88 tests across 11
packages, all `-race` clean.

### Added

- **`wrapper/` — top-level launch path.**
  - `Config` (App, Adapter, Activity, Workdir, BootDir, SessionID,
    Planter+PlantSpec, Sandbox, Policy, Filters).
  - `Wrapper.Run(ctx)` dispatches the adapter's declared runtime to the
    matching `agentkit/agentsessions.Capabilities` lifecycle flag
    (PTY / StreamingStdio / JsonRpcStdio / ServeHTTP / adapter-default),
    drives `NewFromAdapter` → `Prepare` → `Start` → `Wait`, translates
    `llmtypes.StreamEvent` through the activity bridge into
    `runtimeevents.Event`, and emits the full lifecycle vocabulary
    (session.ready, process.started/exited, turn.started/completed/failed,
    interrupt.requested/acknowledged).
  - `Wrapper.SendInput` emits `stdin.write` before forwarding to the
    session.
  - `Wrapper.Stop` and the ctx-watcher goroutine both emit
    `interrupt.requested` → `session.Stop` → `interrupt.acknowledged`
    correlated by `ParentID`.
  - `Wrapper.SessionID` exposes the auto-generated wrapper-session ID.
  - Per-session monotonic TurnID tracking: `turn.started` fires before
    the first turn-internal event (delta, tool_use, tool_result,
    permission_*, subagent_spawn); turn-scoped events carry the TurnID;
    `turn.completed`/`turn.failed` reset state for the next turn.

- **`activity/` — bridge to `go-runtime-events`.**
  - `NewBridge(sink)` wraps any `runtimeevents.Sink` (nil sink → no-op).
  - `Bind(app, sessionID, process)` binds per-session identity.
  - `Emit(ctx, kind, source, payload, opts...)` forwards to the embedded
    Emitter with all the convenience options.

- **`adapters/` — provider-integration contract.**
  - Base `Adapter` interface (Name / Describe / Resolve) stays neutral
    of go-providers — apps that declare their own runtime path don't
    have to depend on agentkit.
  - Optional `RuntimeAdapter` interface adds `CLIAdapter()
    provider.CLIAdapter` for adapters that ride on the agentkit runtime.
  - `Descriptor` (Provider, Runtime, Channels) advertises capabilities;
    `ResolveContext` (BootDir, Cwd, Env, PTY, AppHints) and `Spec`
    (Binary, Args, Env, Cwd) form the exec-shape contract.

- **`adapters/claude/` — Claude streaming-stdio adapter.**
  - `New(WithBinary, WithExtraArgs)`; declares streaming-stdio runtime +
    `claude-stream-json` channel; rejects PTY.
  - `CLIAdapter()` returns `provider.NewClaudeAdapterStreamingStdio()`.

- **`adapters/codex/` — Codex JSON-RPC stdio adapter.**
  - `New(WithBinary, WithExtraArgs)`; declares jsonrpc-stdio runtime +
    `jsonrpc` channel; rejects PTY.
  - `CLIAdapter()` returns `provider.NewCodexAdapterAppServer()`.

- **`adapters/opencode/` — OpenCode HTTP/SSE adapter.**
  - `New(WithBinary, WithExtraArgs)`; declares http-sse runtime +
    `opencode-plugin` channel; rejects PTY.
  - `CLIAdapter()` returns `provider.NewOpencodeAdapterServeHTTP()`.

- **`policy/` — wrapper policy engine contract.**
  - `Engine.Decide(ctx, Request) (Decision, error)`.
  - `Store` interface for app-provided rule backing.
  - `Mode` constants (observe / nudge / rewrite / block / approval).
  - `ObserveOnly` no-op engine, `ErrNoRule` sentinel.

- **`classifybridge/` — classify → policy adapter.**
  - `Engine` wraps any `go-harness-filters/classify.Classifier`,
    translating `Match.Reversible=false` → `ModeNudge`,
    `Reversible=true` → `ModeRewrite` (both overridable).
  - Synthesizes `Message` from `Recommended` and `Replacement` from the
    first recommended command on rewrite.

- **`plant/` — pre-exec planting contract.**
  - `Planter.Plant(ctx, bootDir, Spec) (Result, error)`.
  - `Spec` carries Files, MCPConfig, ProviderSettings, Hooks,
    RecoveryPrompt.
  - `NoOpPlanter` reference impl.
  - Wrapper.Run calls Planter before the agentkit runtime is
    constructed and emits `plant.started`/`plant.completed` around the
    call.

- **`sandbox/` — sandbox application contract.**
  - `Applier.Apply(ctx, pid) (Result, error)`.
  - `NoOpApplier` reference impl.
  - Wrapper.Run calls Applier after `Start` (PID may be 0 on
    subprocess-per-turn runtimes); emits `sandbox.applied` with the
    Result.

- **`filters/` — pipeline integration point.**
  - `Pipeline.Process(ctx, Input) (Output, error)`.
  - `Passthrough` no-op reference impl. Wrapper.Run does not invoke
    Filters yet — integration with `go-harness-filters` pipeline lands
    in a follow-up.

- **Initial module scaffold from folio's `go-lib` preset** — CI
  workflow, MIT license.

### Notes

- Local `replace` directives in `go.mod` for `agentkit`,
  `go-harness-filters`, `go-runtime-events`, `go-sandbox`, `go-runner`,
  `go-providers`, `go-llm-types`, `go-llm-contracts`. Drop on publish
  once each dep has a tagged release.
- `Wrapper.Run` is the only execution path; PTY runtime is wired in
  dispatch but no concrete PTY adapter ships in v0.1.0.
- See [ROADMAP.md](./ROADMAP.md) for deferred scope.
