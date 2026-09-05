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

## Status (v0.9.0, 2026-09-05)

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
- `Config.Environment` materializes an explicit child environment for every
  native or ACP spawn. Its typed inherit/merge/replace modes, inherited-key
  allowlist, ordered overrides, and final unset list make secret egress and
  precedence inspectable without an `env -i` shell wrapper.
- `adapters.Select` chooses a native adapter from provider, Runtime kind, and
  launch mode. It covers Claude streaming-stdio (including the explicit
  developer-mode variant) and Codex/OpenCode subprocess-per-turn while
  preserving the shipped packages' existing app-server/serve-http defaults.
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

## Install

```sh
go get github.com/hollis-labs/go-agent-wrapper@v0.9.0
```

The module requires Go 1.26.6. Its dependency graph contains no local
`replace` directives.

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
    sink, err := runtimeevents.OpenFileSink("/tmp/wrapper-events.jsonl")
    if err != nil {
        log.Fatal(err)
    }
    defer func() {
        if err := sink.Close(); err != nil {
            log.Printf("close event sink: %v", err)
        }
    }()

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

`Run` owns the complete session lifetime and blocks until the provider exits or
the context is canceled. For a command you can run against an installed Claude
CLI, including first-turn delivery, structured JSONL events, and signal-aware
shutdown, see [`examples/claude-stream`](./examples/claude-stream/).

## Child environment and native adapter selection

The zero-value `wrapper.Config.Environment` inherits the host process
environment for compatibility. Security-sensitive hosts should make the choice
explicit. A strict replacement environment is the usual migration from a
generated `env -i` script:

```go
adapter, err := adapters.Select(adapters.Selection{
    Provider:    adapters.ProviderCodex,
    RuntimeKind: adapters.RuntimeKindCLI,
    LaunchMode:  adapters.LaunchSubprocessPerTurn,
    Binary:      "/absolute/path/to/codex",
})
if err != nil {
    return err
}

w, err := wrapper.New(wrapper.Config{
    App:      "my-app",
    Workdir:  workdir,
    Adapter:  adapter,
    Activity: bridge,
    Environment: wrapper.ChildEnvironment{
        Mode: wrapper.EnvironmentReplace,
        Set: []string{
            "HOME=" + home,
            "PATH=" + path,
            "CODEX_HOME=" + plantedConfigDir,
        },
    },
})
```

`EnvironmentMerge` starts from the ambient process environment;
`EnvironmentInherit` accepts only narrowing (`Allowlist`/`Unset`), while
`EnvironmentReplace` starts empty. A nil allowlist means all inherited keys; a
non-nil empty allowlist means none. `Set` is ordered (the last duplicate wins),
then `Unset` wins over everything. Invalid names/assignments and NUL bytes fail
before spawn. Names compare case-insensitively on Windows and case-sensitively
on other hosts. Values and `Selection.ExtraArgs` are passed as direct environment
and argv entries, never shell-interpolated. When a strict configuration
materializes to zero entries, long-lived and ACP launches contain only the
reserved `GO_AGENT_WRAPPER_EMPTY_ENVIRONMENT=1` marker; this prevents the
current runtime dependencies' empty-slice fallback from restoring the ambient
environment. Subprocess-per-turn launches preserve a genuinely empty slice.

`adapters.LaunchDefault` intentionally retains existing behavior: Claude uses
streaming stdio, Codex uses app-server, and OpenCode uses serve-http. Hosts that
need the Nanite-compatible native shapes request `LaunchStreamingStdio` for
Claude and `LaunchSubprocessPerTurn` for Codex/OpenCode. `DeveloperMode` is
defined only for factory-created Claude adapters. A host with a previously
configured `provider.CLIAdapter` can pass it in `Selection.CLIAdapter`; that
adapter remains authoritative for provider-specific settings, while the
Selection still supplies the wrapper descriptor/lifecycle shape. Known
go-providers adapter types are rejected when their configured shape contradicts
the selected launch mode.

ACP protocol selection remains in the provider ACP packages. The same
`Config.Environment` contract is forwarded to their `acp.LaunchParams`.

## Subpackages

| Path | What it owns |
|---|---|
| `wrapper/` | Top-level `Config`, `Wrapper`, and `Run` — the launch boundary itself. Dispatches native runtimes to `agentkit/agentsessions` and owns ACP stdio/TCP lifecycles through `acp.Manager`. Plumbs all activity into `runtimeevents.Event` via `activity.Bridge`. |
| `acp/` | ACP client contract plus authoritative `Manager`/`Session` registration, liveness, prompt/cancel/close, normalized outcomes, provider session-id readback, and redacted diagnostics. |
| `activity/` | Bridge from wrapper lifecycle to the shared `go-runtime-events` schema. |
| `adapters/` | Provider-integration contract. Base `Adapter` interface is neutral about go-providers; optional `RuntimeAdapter` exposes a `provider.CLIAdapter`; `Select` provides typed native provider/runtime/launch-mode selection. |
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
request, not an observer callback. Set
`Config.ACPBestEffortPermissionRequestResponder` to receive the validated
provider request and return either `acp.SelectPermissionOption(optionID)` or a
zero `acp.PermissionSelection` to cancel. The selected ID must exactly match an
option the provider offered; errors, panics, mismatched sessions, malformed
requests, and unoffered IDs fail closed to ACP `cancelled`. Responder callbacks
run away from the protocol reader and lifecycle locks, and their contexts are
cancelled by turn cancellation, turn completion, transport loss, or close.
At most 64 requests may be dispatched asynchronously per client, and at most 64
responder callbacks may remain active; callbacks that ignore cancellation
retain a slot until they return. Saturation backpressures the single protocol
reader with a bounded cancelled response instead of allocating another
goroutine or raw-input copy. Permission response and cancellation writes have
bounded deadlines, so transport backpressure cannot pin cancellation or close
coordination. If a decision cannot be delivered, the client emits a fixed,
redacted fail-closed resolution and tears down the transport so an agent cannot
remain blocked waiting for a response that never arrived.
Each permission frame is bound to an immutable turn generation when the
protocol reader admits it. The reader closes that admission before publishing
the corresponding `session/prompt` response, and turn completion waits for all
requests already admitted to that generation. A frame received after the
barrier is answered `cancelled` without invoking a later turn's responder or
emitting turn-scoped permission events.

The name “best effort” is load-bearing. This callback is a real pre-execution
decision point only when the provider sends `session/request_permission`; it
does not cause providers to ask and is not a replacement for host tool grants,
sandboxing, or other authoritative gates. The responder receives raw tool input
and provider extensions because an approval UI needs that context; those values
may be sensitive and are never echoed into diagnostics. Diagnostics use fixed,
bounded, redacted text.

Nil preserves each adapter's established safe, non-blocking behavior:

| ACP client | Nil responder | Permission events | Measured provider coverage |
|---|---|---|---|
| Claude bridge | ACP `cancelled` | requested + resolved | A real ordinary Bash call executed internally without asking; other operation classes are not exhaustively measured. |
| Codex bridge | ACP `cancelled` | requested + resolved | A real ordinary shell call executed internally without asking; other classes are not exhaustively measured. |
| OpenCode native ACP | ACP `cancelled` | requested + resolved | One real shell shape executed internally without asking; not an exhaustive guarantee. |
| Pi bridge | ACP `cancelled` | requested + resolved | No permission request observed; `pi-acp` documents that Pi executes filesystem and terminal work locally. |
| Copilot native ACP | JSON-RPC `-32601` | none | A real Copilot CLI 1.0.12 stdio turn asked to run the non-mutating `pwd` shell command emitted one `session/request_permission` with tool kind `execute`; a zero responder selection cancelled it. This measures only that shell shape. Configured response transport is additionally covered synthetically over stdio and TCP. |

All configured clients preserve numeric, string, and schema-present `null`
JSON-RPC request IDs and reject object, array, or boolean IDs without exposing
the request in diagnostics. Copilot's own outbound calls remain numeric and
accept only the corresponding numeric responses.

For the later Nanite integration, Nanite's existing approval vocabulary is
decision `allow`/`deny` plus scope `once`/`session`. The mechanical ACP mapping
is to an actually offered option whose kind is respectively `allow_once`,
`reject_once`, `allow_always`, or `reject_always`; the responder must return
that option's opaque `optionId`, not synthesize one from the kind. A timeout or
canceled Nanite wait maps to the zero selection. This module deliberately does
not import or duplicate Nanite's approval engine.

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

## Upgrade from v0.8.1

v0.9.0 intentionally replaces the action-shaped policy API with names that
describe its real post-execution, observational behavior. There are no
deprecated aliases: stale integrations fail at compile time instead of keeping
the misleading enforcement contract.

| v0.8.1 | v0.9.0 |
|---|---|
| `wrapper.Config.Policy` | `wrapper.Config.PolicyObserver` |
| `policy.Engine.Decide` | `policy.Observer.Observe` |
| `policy.Request` / `policy.Decision` | `policy.Observation` / `policy.Finding` |
| `policy.Mode` | `policy.Recommendation` |
| `Decision.Mode` | `Finding.Recommendation` |
| `Decision.Replacement` | `Finding.SuggestedReplacement` |
| `policy.ObserveOnly` | `policy.NoOpObserver` |
| `classifybridge.Engine` | `classifybridge.Observer` |

The complete symbol and constant mapping is in [CHANGELOG.md](./CHANGELOG.md).
Hosts may also adopt the new wrapper-owned `acp.Manager`, explicit
`Config.Environment`, typed `adapters.Select`, and best-effort ACP permission
responder. Remove any consumer-side replacements for `go-harness-filters` and
`go-runtime-events`: this release uses their published v0.1.1 and v0.1.2 tags.

## Dependencies

- `github.com/hollis-labs/agentkit` (v0.5.0) — sessions, launch,
  runtime, context, broker.
- `github.com/hollis-labs/go-runtime-events` (v0.1.2) —
  runtime activity event envelope.
- `github.com/hollis-labs/go-harness-filters` (v0.1.1) —
  classify + directive + repair (used via `classifybridge/`).
- `github.com/hollis-labs/go-sandbox` (v0.2.1) — sandbox profiles.
- `github.com/hollis-labs/go-runner` (v0.5.0, indirect) — process supervision.
- `github.com/hollis-labs/go-providers` (v0.23.0) — provider adapters.

All requirements are released versions fetched through the public Go module
proxy; `go.mod` contains no `replace` directive.

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

The default suite is deterministic and never launches an installed Claude,
Codex, Copilot, OpenCode, or Pi process merely because its CLI (or `npx`) is on
`PATH`. Real-provider tests are a separate, explicit operator action:

```sh
GO_AGENT_WRAPPER_LIVE_PROVIDER_TESTS=1 \
  go test -race ./adapters/claudeacp ./adapters/codexacp \
    ./adapters/copilotacp ./adapters/opencodeacp ./adapters/piacp \
    ./sidebyside -run '^(TestLive|TestReal)'
```

Those tests additionally require the named provider binaries, bridge runtimes,
credentials, account quota, and model configuration. Opting in keeps genuine
provider failures visible; it does not convert an installed but unauthenticated
or incompatible provider into deterministic test coverage. In particular, the
ambient Copilot configuration may still reject `reasoning_effort="medium"`
when it routes to `claude-haiku-4.5`; that is external provider/account state,
not synthetic adapter coverage.

CI (`.github/workflows/check.yml`) runs the same checks on push and pull
request to `main`. It reads the exact Go 1.26.6 toolchain declaration from
`go.mod`, avoiding drift from a moving `stable` alias.

## License

MIT — see [LICENSE](./LICENSE).
