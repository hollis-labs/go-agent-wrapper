# Changelog

All notable changes to go-agent-wrapper are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
