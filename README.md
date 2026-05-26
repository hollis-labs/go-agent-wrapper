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

## Status (v0.1.0, 2026-05-26)

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
- `agent.delta`, `agent.tool_use`, `agent.tool_result`,
  `agent.subagent_spawn`, and JSON-RPC permission request/resolution
  events flow through the translator;
  if `Config.Policy` is set, tool_use events trigger a
  `policy.Engine.Decide` call and emit a correlated
  `policy.nudge`/`rewrite`/`block`/`approval_requested` event
  (observation half — no rewrite-back to the child yet).
- `plant.started`/`plant.completed`, `sandbox.applied`, and pre-spawn
  `SandboxProfile` plumbing are wired when configured.
- `Config.Filters` can process agent text, tool envelopes/results, and
  command output; `filters.RepairPipeline` adapts concrete
  `go-harness-filters/repair` rules.
- Three concrete adapters: Claude (streaming-stdio), Codex
  (jsonrpc-stdio), OpenCode (http-sse).
- `classifybridge.Engine` lets a `go-harness-filters/classify.Classifier`
  drive policy decisions directly.

Test count: 88 across 11 packages, all `-race` clean.

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
| `wrapper/` | Top-level `Config`, `Wrapper`, and `Run` — the launch boundary itself. Dispatches the adapter's declared runtime (pty / streaming-stdio / jsonrpc-stdio / http-sse / adapter-subprocess-per-turn) to the matching `agentkit/agentsessions` Capabilities. Plumbs `llmtypes.StreamEvent` → `runtimeevents.Event` via `activity.Bridge`. |
| `activity/` | Bridge from wrapper lifecycle to the shared `go-runtime-events` schema. |
| `adapters/` | Provider-integration contract. Base `Adapter` interface is neutral about go-providers; optional `RuntimeAdapter` extension exposes a `provider.CLIAdapter` for adapters that ride on the agentkit runtime. |
| `adapters/claude/` | Claude Code streaming-stdio adapter (`claude -p --input-format stream-json --output-format stream-json --verbose`). |
| `adapters/codex/` | Codex app-server adapter (`codex app-server`) — JSON-RPC 2.0 over stdio. |
| `adapters/opencode/` | OpenCode serve-http adapter (`opencode serve --port 0 --hostname 127.0.0.1`) — HTTP/SSE. |
| `classifybridge/` | Adapts `go-harness-filters/classify.Classifier` into `policy.Engine` so rule-driven classification can drive wrapper policy decisions. |
| `policy/` | Observe / nudge / rewrite / block / approval engine + `Store` interface for app-provided rule backing. |
| `plant/` | Pre-exec planting contract (boot dirs, MCP config, provider settings, hooks/plugins). Called by `Run` before the agentkit runtime is constructed. |
| `sandbox/` | Sandbox profile application contract — composes `go-sandbox`. Called by `Run` after `Start` against the session's PID. |
| `filters/` | Integration point for the `go-harness-filters` pipeline. |

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
