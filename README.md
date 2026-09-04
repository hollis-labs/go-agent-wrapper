# go-agent-wrapper

Shared harness for launching, observing, and governing CLI-agent
subprocesses across Hollis Labs apps. Composes the
[`agentkit`](https://github.com/hollis-labs/agentkit) umbrella module
(absorbed `go-agent-sessions` / `go-agent-launch` / `go-agent-runtime` /
`go-agent-context` / `go-agent-broker` in v0.1.0, 2026-05-26) plus the
still-standalone primitives (`go-runner`, `go-providers`, `go-sandbox`)
into a single standardized execution boundary — without prescribing
prompt design, workflow logic, turn semantics, or agent cognition.

This is the "sibling agent in parallel" path identified by the
`agentkit-wrapper-alignment-review-2026-05-26.md` rollout (step 9):
filters / plant / sandbox composition + Tachyon `cmd/agent-wrap`.

## Status

End-to-end launch path is wired:

- `Wrapper.Run` dispatches the adapter's declared runtime to the
  matching agentkit `Capabilities` (PTY / streaming-stdio / jsonrpc-stdio
  / http-sse / adapter-subprocess-per-turn), drives
  `agentsessions.NewFromAdapter` → `Prepare` → `Start` → `Wait`, and
  translates `llmtypes.StreamEvent`s into `runtimeevents.Event`s.
- Lifecycle events emitted: `session.ready`, `process.started`,
  `process.exited`, `session.processing`/`session.idle`, optional
  `session.heartbeat`, `turn.started`/`turn.completed`/`turn.failed`
  (with monotonic per-session TurnIDs), `stdin.write`,
  `stdout.raw`/`stdout.line`, `stderr.raw`/`stderr.line`,
  `interrupt.requested`/`interrupt.acknowledged`.
- `agent.delta`, `agent.tool_use`, `agent.tool_result`, and
  `agent.subagent_spawn` flow through the translator, as do JSON-RPC
  permission request/resolution events where the adapter emits them. If
  `Config.PolicyObserver` is set,
  each translated tool-use event is handed to `policy.Observer.Observe`
  after `agent.tool_use` is emitted. A mapped recommendation emits a correlated
  `policy.nudge`/`rewrite`/`block`/`approval_requested` compatibility event;
  it never changes or prevents child execution.
- `plant.started`/`plant.completed`, `sandbox.applied`, and pre-spawn
  `SandboxProfile` plumbing are wired when configured.
- `Config.Filters` can process agent text, tool envelopes/results, and
  command output; `filters.RepairPipeline` adapts concrete
  `go-harness-filters/repair` rules.
- Native and ACP adapters are available for the providers documented under
  `adapters/`.
- ACP adapters run through a wrapper-owned `acp.Manager`: `Wrapper.Run`
  validates ACP v1 negotiation, performs optional agent authentication,
  capability-gated create-or-resume, deterministic mode/config application,
  prompting, turn cancellation, and close for both stdio and TCP. A failed
  advertised resume is returned instead of silently starting a new session.
  `Wrapper.ACPSnapshot`,
  `Wrapper.ProviderSessionID`, and a shared `Config.ACPManager` replace
  downstream liveness/session registries.
- `classifybridge.Observer` lets a
  `go-harness-filters/classify.Classifier` produce policy findings.

The repository test suite is kept `-race` clean; see Development for the
commands used by CI.

See [ROADMAP.md](./ROADMAP.md) for what's deferred and the next-session
priorities.

Module path: `github.com/hollis-labs/go-agent-wrapper`

## Quickstart

```go
package main

import (
    "context"
    "log"

    "github.com/hollis-labs/go-agent-wrapper/activity"
    "github.com/hollis-labs/go-agent-wrapper/adapters/claude"
    "github.com/hollis-labs/go-agent-wrapper/wrapper"
    runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func main() {
    // Pick any sink — FileSink writes JSONL, MultiSink fans out, or
    // implement runtimeevents.Sink yourself.
    sink, _ := runtimeevents.OpenFileSink("/tmp/wrapper-events.jsonl")
    defer sink.Close()

    w, err := wrapper.New(wrapper.Config{
        App:      "my-app",
        Workdir:  "/path/to/workspace",
        Adapter:  claude.New(),
        Activity: activity.NewBridge(sink),
    })
    if err != nil {
        log.Fatal(err)
    }

    if err := w.Run(context.Background()); err != nil {
        log.Fatal(err)
    }
}
```

## Subpackages

| Path | What it owns |
|---|---|
| `wrapper/` | Top-level `Config`, `Wrapper`, and `Run` — the launch boundary itself. Dispatches native runtimes to `agentkit/agentsessions` and owns ACP stdio/TCP lifecycles through `acp.Manager`. Plumbs all activity into `runtimeevents.Event` via `activity.Bridge`. |
| `acp/` | ACP client contract plus authoritative `Manager`/`Session` registration, liveness, prompt/cancel/close, normalized outcomes, provider session-id readback, and redacted diagnostics. |
| `activity/` | Bridge from wrapper lifecycle to the shared `go-runtime-events` schema. |
| `adapters/` | Provider-integration contract. Base `Adapter` interface is neutral about go-providers; optional `RuntimeAdapter` extension exposes a `provider.CLIAdapter` for adapters that ride on the agentkit runtime. |
| `adapters/claude/` | Claude Code streaming-stdio adapter (`claude -p --input-format stream-json --output-format stream-json --verbose`). |
| `adapters/codex/` | Codex app-server adapter (`codex app-server`) — JSON-RPC 2.0 over stdio. |
| `adapters/opencode/` | OpenCode serve-http adapter (`opencode serve --port 0 --hostname 127.0.0.1`) — HTTP/SSE. |
| `adapters/claudeacp/`, `adapters/codexacp/`, `adapters/piacp/` | Bridge-mediated ACP clients driven end to end by `wrapper.Wrapper`. |
| `adapters/opencodeacp/`, `adapters/copilotacp/` | Native ACP clients; Copilot supports both stdio and TCP. |
| `classifybridge/` | Adapts `go-harness-filters/classify.Classifier` into `policy.Observer` findings. |
| `policy/` | Post-hoc observations, advisory findings, and an optional `Store` interface for app-provided rule backing. |
| `plant/` | Pre-exec planting contract (boot dirs, MCP config, provider settings, hooks/plugins). Called by `Run` before the agentkit runtime is constructed. |
| `sandbox/` | Sandbox profile application contract — composes `go-sandbox`. Called by `Run` after `Start` against the session's PID. |
| `filters/` | Integration point for the `go-harness-filters` pipeline. |

## Policy observation is not enforcement

`Config.PolicyObserver` receives an already-emitted tool-use observation and
returns a `policy.Finding`. Its `Recommendation` is advisory, including
`RecommendationBlock`, `RecommendationRewrite`, and
`RecommendationRequestApproval`. Hosts must enforce grants and permissions at
their own pre-execution call sites. In Nanite, the authoritative boundaries are
the tool-grant check, the skill capability gate, and cancellable plugin
pre-hooks; none may be removed or weakened because a wrapper observer exists.

The stable `go-runtime-events` names and payload values remain unchanged:

| Go recommendation | Existing wire kind | Effect in this wrapper |
|---|---|---|
| `RecommendationNudge` | `policy.nudge` | Emit a correlated advisory event |
| `RecommendationRewrite` | `policy.rewrite` | Emit suggested replacement data; do not substitute it |
| `RecommendationBlock` | `policy.block` | Emit a block recommendation; do not stop execution |
| `RecommendationRequestApproval` | `policy.approval_requested` | Emit an approval recommendation; do not pause execution |

The wire labels are retained to avoid a coordinated breaking release of
`go-runtime-events` and its other consumers. The wrapper's Go API carries the
accurate semantics.

ACP `session/request_permission` is separate: it is a blocking protocol
request, not an observer callback. Direct Claude, Codex, OpenCode, and Pi ACP
clients currently default it to `cancelled`; Copilot currently returns JSON-RPC
method-not-handled. A future host responder can enforce at that point only when
the provider chooses to ask, so it is not a general replacement for host gates.

## ACP lifecycle

Pass any shipped ACP adapter to `wrapper.Config.Adapter`; no direct
`acp.Client` orchestration is required. Use `Config.SessionIDPreset` to resume,
`ACPAuthMethodID` for a non-terminal method advertised by `initialize`, and
`ACPSessionModeID`/`ACPSessionConfig` for post-create configuration. Call
`Wrapper.CancelTurn` for ACP `session/cancel` (the session remains reusable) and
`Wrapper.Stop` to close the whole session. A shared `Config.ACPManager` exposes
lookup, liveness, cancel, close, and shutdown across wrappers.

Manager readiness is published only after the provider session ID and wrapper
control reference are committed. Once `Prompt` accepts and writes a request,
the asynchronous turn belongs to the ACP session rather than the caller's
short-lived context; explicit `CancelTurn`, `Stop`, transport loss, or provider
completion ends it. After `Run` returns, `ACPSnapshot` no longer exposes the
dead session and control calls cannot target it; `ProviderSessionID` remains
available for postmortem correlation.

Unexpected transport EOF, non-zero child-process exit, and malformed protocol
streams are returned as typed `acp.LifecycleError` outcomes. A clean child exit
is recorded as `acp.OutcomeChildExit` without manufacturing an error. Canceled
turns are recorded as `acp.OutcomeCanceled`; cancellation of the whole `Run`
preserves `ctx.Err()`.
`Config.OnACPDiagnostic` receives only bounded, redacted stderr/protocol data;
diagnostics are not mixed into model output.

## Dependencies

All cross-lib deps in this monorepo are wired via local `replace`
directives in `go.mod`. Drop the replaces and bump to tagged versions
before publishing — see [ROADMAP.md](./ROADMAP.md) §"Publish blockers".

- `github.com/hollis-labs/agentkit` (v0.1.0+) — sessions, launch,
  runtime, context, broker.
- `github.com/hollis-labs/go-runtime-events` (v0.1.0+, this monorepo) —
  runtime activity event envelope.
- `github.com/hollis-labs/go-harness-filters` (v0.1.0+, this monorepo) —
  classify + directive + repair (used via `classifybridge/`).
- `github.com/hollis-labs/go-sandbox` (v0.2.1+) — sandbox profiles.
- `github.com/hollis-labs/go-runner` (v0.6.0+) — process supervision.
- `github.com/hollis-labs/go-providers` (v0.23.0+) — provider adapters.

## Architecture notes

- `chrispian/inbox/cli-runner-wrapper-architecture-2026-05-26.md`
- `chrispian/inbox/harness-filters-directives-normalization-2026-05-26.md`
- `chrispian/inbox/agentkit-wrapper-alignment-review-2026-05-26.md`
- `chrispian/inbox/cli-wrapper-implementation-followups-2026-05-26.md`
  (handoff for the next session)

## Development

```sh
go test -race ./...   # tests
go vet ./...          # vet
gofmt -l .            # formatting check (no output = clean)
golangci-lint run     # lint
govulncheck ./...     # vulnerability scan
```

CI (`.github/workflows/check.yml`) runs the same checks on push and pull
request to `main`.

## License

MIT — see [LICENSE](./LICENSE).
