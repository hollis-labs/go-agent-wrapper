# Changelog

All notable changes to go-agent-wrapper are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Unreleased

### Changed

- **Breaking: the post-hoc policy callback is now explicitly observational.**
  The v0.8.1 API exposed action-shaped names even though `Wrapper.Run`
  consulted it only after emitting `agent.tool_use`, too late to prevent or
  replace the child operation. The replacement API is:

  | v0.8.1 | Unreleased |
  |---|---|
  | `wrapper.Config.Policy` | `wrapper.Config.PolicyObserver` |
  | `policy.Engine.Decide` | `policy.Observer.Observe` |
  | `policy.Request` | `policy.Observation` |
  | `policy.Decision` | `policy.Finding` |
  | `policy.Mode` | `policy.Recommendation` |
  | `ModeObserve` | `RecommendationNone` |
  | `ModeNudge` | `RecommendationNudge` |
  | `ModeRewrite` | `RecommendationRewrite` |
  | `ModeBlock` | `RecommendationBlock` |
  | `ModeApproval` | `RecommendationRequestApproval` |
  | `Decision.Mode` | `Finding.Recommendation` |
  | `Decision.Replacement` | `Finding.SuggestedReplacement` |
  | `Rule.Mode` / `Rule.Replacement` | `Rule.Recommendation` / `Rule.SuggestedReplacement` |
  | `policy.ObserveOnly` | `policy.NoOpObserver` |
  | `classifybridge.Engine` | `classifybridge.Observer` |

  `classifybridge.Engine.NudgeMode` and `.RewriteMode` become the explicitly
  advisory `Observer.NonReversibleRecommendation` and
  `.ReversibleRecommendation` fields. No deprecated aliases are retained: an
  old `Config.Policy` population must fail at compile time rather than silently
  preserve the misleading contract.

- **Preserved the `go-runtime-events` policy wire vocabulary.**
  `RecommendationNudge`, `RecommendationRewrite`, `RecommendationBlock`, and
  `RecommendationRequestApproval` retain the strings `nudge`, `rewrite`,
  `block`, and `approval` and continue to emit `policy.nudge`,
  `policy.rewrite`, `policy.block`, and `policy.approval_requested` with the
  existing `mode` and `replacement` payload keys. These are compatibility
  labels for advisory observations; the wrapper does not perform the named
  actions. Keeping them avoids a coordinated breaking release of
  `go-runtime-events` and its independent consumers.

- **Documented the authoritative host boundary.** Nanite's tool-grant, skill
  capability, and plugin pre-hook gates remain responsible for preventing
  execution. ACP `session/request_permission` remains a separate, narrower
  protocol control point: Claude, Codex, OpenCode, and Pi default it to a
  well-formed cancelled outcome, while Copilot currently returns JSON-RPC
  method-not-handled. Wiring a host response is separate work and can only be
  described as best-effort where a provider actually asks.

### Tests

- Added a native subprocess characterization proving a block recommendation is
  produced only after the child has already created a side-effect marker.
- Added ACP protocol coverage proving Claude's default cancelled response
  unblocks a child waiting on `session/request_permission`, and locking down
  Copilot's distinct method-not-handled response without adding a responder.

## v0.8.0 — 2026-08-21

**New `adapters/piacp` package** (`TASKS/agent-host-acp/15`, Nanite's own
tracker): the first bridge-mediated ACP adapter and Pi's (`earendil-works/pi`)
first appearance as a supported agent anywhere in go-agent-wrapper — no prior
native Pi adapter exists in this repo. Per task 12's operator-approved
decision (Nanite repo, `TASKS/ESCALATIONS.md`, 2026-08-21 "Task 12 resolved"),
the pinned bridge is `svkozak/pi-acp` (npm package, `npx -y pi-acp`, no
separate install required) — the ACP registry's canonical Pi bridge.

### Added

- **`piacp.Client`** — a real, self-contained `acp.Client` implementation.
  Spawns and owns `npx -y pi-acp` (configurable via `WithClientBinary`/the
  `PIACP_CLI_PATH` env var, in which case the default `-y pi-acp` npx
  arguments are omitted rather than nonsensically prepended to an
  already-resolved binary) directly via os/exec; real request/response
  correlation (an id-keyed pending map), real `session/update` notification
  dispatch. Wire behavior — newline-delimited JSON-RPC 2.0;
  `initialize`/`session/new`/`session/load`/`session/prompt`/
  `session/cancel`/`session/update` shapes — was verified directly against a
  real `pi-acp` 0.0.33 bridge driving a real `pi` 0.84.2 process, not assumed
  from documentation. One real, load-bearing shape difference from
  opencodeacp's `session/load` found by testing: pi-acp's `session/load`
  result carries no `sessionId` field (unlike `session/new`'s) — the caller
  must keep using the id it requested resume with; confirmed via a genuine
  cross-process resume (kill the original `pi-acp` process, resume from a
  fresh one).
- **Verified live `InterruptCapability`: `adapters.InterruptTurn`.**
  `session/cancel` was tested against a definitely-still-running `bash` tool
  subprocess (a real `sleep`-based counting loop, confirmed in-flight via
  several real terminal-output ticks over multiple wall-clock seconds before
  cancellation, deliberately not relying on model-generation speed for the
  timing claim): the in-flight tool call transitioned to `status: "failed"`
  and the `session/prompt` response arrived with `stopReason: "cancelled"`
  within single-digit milliseconds — a genuine mid-turn abort, not
  acknowledge-and-let-finish. A follow-up prompt on the same session
  completed normally afterward, confirming the session itself survives
  cancellation (turn-scoped, matching ACP's own semantics for the
  misleadingly-named `session/cancel` method).
- **`session/update` → `runtimeevents` mapping**: `agent_message_chunk` →
  `agent.delta` (verified live); `agent_thought_chunk` → `agent.delta`
  (mapped defensively but unverified — pi-acp's own README documents "no
  separate thought stream" as a current limitation, so this is dead code
  today); `tool_call`/`tool_call_update` → `agent.tool_use`/
  `agent.tool_result`, including pi-acp-specific real incremental
  `terminal_output`/`terminal_exit` metadata for `execute`-kind (bash) tool
  calls, surfaced in the event payload rather than dropped.
  `session_info_update`/`available_commands_update`/`user_message_chunk`
  (the last observed only as a `session/load` resume-replay artifact) are
  deliberately left unmapped (no current runtimeevents analog). No
  `fs/*`/`terminal/*`/`session/request_permission` server-initiated request
  was ever observed — per pi-acp's own README this is a documented,
  permanent design limitation ("pi reads/writes and executes locally"), not
  an untested unknown; any such request is still declined defensively rather
  than left to hang.
- **`piacp.Adapter`** — `adapters.Adapter` + `adapters.RuntimeAdapter`
  (`Name() == "pi-acp"`, `Descriptor.Provider == "pi"`). `CLIAdapter()`
  returns a `provider.CLIAdapter` shim mirroring opencodeacp's own precedent
  exactly: real Detect/BuildArgs (so a `wrapper.Wrapper.Run()` caller spawns
  the one real bridge process, not a duplicate) and a pass-through ParseLine
  — the same confirmed seam-gap finding tasks 08/09/10 already documented
  (go-providers' `CLIAdapter` interface has no hook for a bidirectionally-real
  JSON-RPC session once agentkit owns the spawned process's stdin). Real,
  live-verified ACP driving in this package goes through `Client` directly.
- **Explicit Node.js/npm/npx runtime requirement documented** in the package
  doc comment, per the operator's own framing at task 12's decision: a real,
  deliberate, and reversible choice — `acp.Client` already isolates every
  caller from the concrete implementation, so replacing this package with a
  pure-Go Pi bridge later (if one matures) is a new implementation, not a
  rearchitecture. Same requirement already applies to this repo's Claude/
  Codex bridge-mediated ACP adapters (tasks 13/14).

### Verified live (not mocked), against a real local backend

No cloud provider (`anthropic`/`openai`/`google`) had usable credentials on
the machine this package was implemented and verified against — `pi auth
check` returned real `credentials_not_configured` for all three. Rather than
skip live verification, `pi` was wired to a real, locally-running Ollama
model (`llama3.1:8b`) via its own documented Custom Providers mechanism
(`~/.pi/agent/models.json`) — a real LLM backend, not a mock, just a free
local one instead of a paid cloud one; the ACP wire behavior this package
depends on is a property of pi-acp/pi's own implementation, independent of
model choice. `TestLiveClientCompletesOneRealTurn` and
`TestLiveClientCancelAbortsMidGeneration` (both skip, not fail, when
`npx`/`pi` aren't on PATH or Launch fails for an environment reason) passed
for real, including under `-race`.

## v0.7.0 — 2026-08-21

**New `adapters/copilotacp` package** (`TASKS/agent-host-acp/10`, Nanite's
own tracker): the second native ACP adapter — GitHub Copilot CLI, driven
via its own `--acp` flag, over **both** stdio and TCP transports — the
first shipped adapter to genuinely exercise `adapters.Transport` as a
real, functioning per-adapter choice rather than a single hardcoded
value.

### Added

- **`copilotacp.Client`** — a real, self-contained `acp.Client`
  implementation supporting `adapters.TransportStdio` and
  `adapters.TransportTCP`. For stdio it spawns and owns `copilot --acp`
  directly via os/exec; for TCP it spawns `copilot --acp --port <N>` (or,
  via `WithDialOnly`, connects to an already-running daemon) and owns the
  TCP connection. Either way: real request/response correlation (an
  id-keyed pending map), real `session/update` notification dispatch, no
  bridge library, no dependency on agentkit/jsonrpc-stdio machinery. Wire
  behavior — newline-delimited JSON-RPC 2.0; `initialize`/`session/new`/
  `session/prompt`/`session/cancel`/`session/update` shapes — was
  verified directly against a real Copilot CLI 1.0.12 binary on both
  transports, not assumed from documentation. `--port` genuinely binds
  and LISTENs (confirmed via `lsof`; a second instance on the same port
  gets a real `EADDRINUSE`) — no `--host`/`--acp --help` exists; not
  documented anywhere found, only confirmed by testing the flag directly.
- **Verified live `InterruptCapability`: `adapters.InterruptTurn`.**
  `session/cancel` (a notification, not a request — confirmed against
  the spec) was tested mid-generation against a real ~2000-word-essay
  prompt: generation was cut off within ~3 seconds of Cancel, with an
  agent-emitted "Info: Operation cancelled by user" message chunk,
  rather than running to natural completion — a genuine abort, not
  acknowledge-and-let-finish.
- **`session/update` → `runtimeevents` mapping**: `agent_message_chunk`/
  `agent_thought_chunk` → `agent.delta`; `tool_call`/`tool_call_update` →
  `agent.tool_use`/`agent.tool_result`. `plan`/`available_commands_update`/
  `usage_update` are deliberately left unmapped (no current
  runtimeevents analog). Any server-initiated request (`fs/*`,
  `terminal/*`, `session/request_permission`) is declined with a
  JSON-RPC error rather than left to hang — real fs/terminal proxying is
  out of scope (matches docs/engineering/architecture/17-acp.md's own
  flagged unknown).
- **`copilotacp.Adapter`** — `adapters.Adapter` + `adapters.RuntimeAdapter`
  (`Name() == "copilot"`). `Describe()` reflects whichever transport the
  Adapter was configured with (`WithAdapterTransport`), proving
  `Descriptor.Transport` is a real per-adapter choice, not a hardcoded
  value. `CLIAdapter()` returns a `provider.CLIAdapter` bridge with real
  Detect/BuildArgs (so a `wrapper.Wrapper.Run()` caller spawns the one
  real `copilot --acp` process, not a duplicate) and — a deliberate,
  small divergence from task 09's sibling adapter's pure pass-through —
  a ParseLine that does real `session/update` translation, reusing the
  same logic `Client` itself needs regardless. Neither variant drives the
  handshake/turn-sending automatically through that composition; see the
  package doc's "Wrapper.Run composition" section for the confirmed
  seam-gap finding this is built around (independently re-confirmed here;
  first found by task 09 for OpenCode's own ACP adapter).
- **Confirmed, real gap: no agentkit TCP-session runtime kind exists.**
  `ProtocolACP`+`TransportTCP` has no `wrapper/runtime_dispatch.go` entry
  (task 08 left it deliberately unmapped) and cannot get one without
  `agentkit/agentsessions` growing an actual TCP-socket-based Runtime/
  Session kind first — agentkit ships exactly four kinds today (PTY,
  streaming-stdio, jsonrpc-stdio, serve-http), none TCP-based. Out of
  this repo's scope; `Client`'s TCP transport is fully real and tested
  standalone (see below), independent of that gap.

### Verified live (not mocked)

Against a real, authenticated `copilot` (1.0.12) binary: `Test
RealCopilotACP_Stdio_EndToEnd` and `TestRealCopilotACP_TCP_EndToEnd`
(one real completed turn each, skip — not fail — when `copilot` isn't on
PATH), `TestRealCopilotACP_CancelInterruptsTurn` (real mid-generation
cancel), and `TestRealCopilotACP_EventsMapToActivityBridge` (drives
`Client.Events()` through the same `activity.Bridge` every other
adapter's turn activity flows through in production). All four
gracefully skip rather than fail if the live account hits a real
"exceeded your monthly quota" condition mid-test (hit during this task's
own implementation) — a live account-state fact, not an adapter defect;
structural assertions (turn lifecycle, TurnID correlation, Bridge
binding) still run regardless of quota state.

### Notes

- No new dependency: `adapters/copilotacp` imports only `acp`,
  `activity`, `adapters` (this module), `go-providers/provider`,
  `go-llm-types`, and `go-runtime-events` — all already required.
  `go.mod` is unchanged.
- A genuine `sync.WaitGroup` Add-before-Wait race in `Client`'s own
  turn-completion/events-close sequencing was caught live by
  `go test -race` during implementation (a fast responder's terminal
  event could be silently dropped if the underlying connection closed in
  the same instant) and fixed — see `Client.closeEvents`'/`Client.Prompt`'s
  doc comments. `go test ./... -race` is clean across the whole module.

## v0.6.0 — 2026-08-21

**New `adapters/opencodeacp` package** (`TASKS/agent-host-acp/09`, Nanite's
own tracker): the first native ACP adapter — OpenCode, driven via its
own documented `opencode acp` subprocess mode. Additive alongside the
existing `adapters/opencode` (native HTTP/SSE protocol, untouched) —
both stay independently selectable.

### Added

- **`opencodeacp.Client`** — a real, self-contained `acp.Client`
  implementation. Spawns and owns `opencode acp` directly via os/exec
  (real request/response correlation, real notification dispatch — no
  bridge library, no dependency on go-agent-wrapper's own agentkit/
  jsonrpc-stdio machinery). Wire behavior (newline-delimited JSON-RPC
  2.0; `initialize`/`session/new`/`session/load`/`session/prompt`/
  `session/cancel`/`session/update` shapes, including the
  `update.sessionUpdate` discriminator field name a scraped spec
  summary got wrong) was verified directly against a real opencode
  1.15.6 binary, not assumed from documentation — see the package doc
  for the full empirical findings.
- **Verified live `InterruptCapability`: `adapters.InterruptTurn`.**
  `session/cancel` was tested mid-generation against a real long-form
  prompt: the turn's terminal response arrived ~35ms after Cancel, with
  generation only a few chunks in — a genuine abort, not
  acknowledge-and-let-finish. Matches the native (non-ACP) OpenCode
  adapter's own already-verified `Stop()` capability tier.
- **`session/update` → `runtimeevents` mapping**, per
  docs/engineering/architecture/17-acp.md's mapping: `agent_message_chunk`/
  `agent_thought_chunk` → `agent.delta`; `tool_call`/`tool_call_update` →
  `agent.tool_use`/`agent.tool_result`; the server-initiated
  `session/request_permission` request → `agent.permission_requested`/
  `resolved` (answered with a well-formed ACP "cancelled" outcome — no
  interactive approval mechanism is wired into this Client; that is
  Nanite's own policy layer, upstream of this package).
  `available_commands_update`/`usage_update`/`plan`-style informational
  variants are deliberately left unmapped.
- **`opencodeacp.Adapter`** — `adapters.Adapter` + `adapters.RuntimeAdapter`
  (`Name() == "opencode-acp"`, distinct from the native adapter's
  `"opencode"`; `Descriptor.Provider == "opencode"`, same upstream
  identity). `CLIAdapter()` returns a `provider.CLIAdapter` bridge that
  deliberately mirrors `adapters/codex`'s own app-server shape (real
  Detect/BuildArgs, pass-through ParseLine) rather than trying to drive
  a real session through it — see the package doc's "Client ownership
  of the subprocess" section for the underlying seam-gap finding this
  is built around (go-providers' CLIAdapter interface has no hook for a
  bidirectionally-real, response-correlated session once agentkit spawns
  the process, and no shipped adapter in this repo — including the
  existing Codex app-server one — has that wired through
  `wrapper.Wrapper.Run()` today).

### Verified live (not mocked)

Both against a real, authenticated `opencode acp` (1.15.6) subprocess,
via this package's own `TestLiveClientCompletesOneRealTurn` and
`TestLiveClientCancelAbortsMidGeneration` (skip, not fail, when
`opencode` isn't on PATH or Launch fails for an environment reason):
one full real turn (`session/prompt` → streamed `agent.delta` → real
`turn.completed`) and one real mid-generation cancel (turn.completed
arriving in tens of milliseconds, not after natural completion).

### Notes

- No new dependency: `adapters/opencodeacp` imports only `acp`,
  `adapters` (this module), `go-providers/provider`, `go-llm-types`, and
  `go-runtime-events` — all already required. `go.mod` is unchanged.

## v0.5.0 — 2026-08-21

**New `acp` package** (`TASKS/agent-host-acp/08`, Nanite's own tracker):
the ACP (Agent Client Protocol, agentclientprotocol.com) *client*
abstraction — a Hollis host driving an underlying CLI agent over ACP,
not the (separate, prior-art, untouched-here) server role. Interface and
wiring only; no concrete ACP wire-connection logic ships in this
release — that's follow-on work (native OpenCode/Copilot CLI adapters,
then a Claude/Codex/Pi bridge).

### Added

- **`acp.Client`** — the single Go interface (`Launch`, `Prompt`,
  `Cancel`, `Events`, `InterruptCapability`, `Close`) a concrete ACP
  implementation (native direct-wire or third-party-bridge-mediated)
  satisfies. `Events()` yields `runtimeevents.Event` values directly —
  no parallel event vocabulary — so an ACP-driving implementation feeds
  the same activity-bridge translation path
  (`wrapper/event_translator.go`) every other adapter already uses.
  `InterruptCapability()` returns the same `adapters.InterruptCapability`
  vocabulary (`none`/`process`/`turn`/`steer`) task 02's `Descriptor`
  split introduced, since a given ACP implementation's real
  `session/cancel` behavior is a per-implementation fact, not a
  protocol-level guarantee.
- **`acp.DescriptorFor(client, providerName, transport)`** — builds the
  `adapters.Descriptor` an ACP-backed `Adapter.Describe()` should
  return (`Protocol: adapters.ProtocolACP` always; `Transport` threaded
  through for stdio vs. TCP daemon modes; `Interrupt` mirrored from the
  Client). The one place the Client's real capability reaches the
  Descriptor seam.
- **`wrapper/runtime_dispatch.go`: `ProtocolACP`/`TransportStdio`
  dispatch entry.** ACP is JSON-RPC 2.0 over stdio — the same framing
  agentkit's `JsonRpcStdio` runtime already speaks generically for
  Codex's app-server — so this is a framing-level-only table addition
  (`agentsessions.Capabilities{JsonRpcStdio: true}`,
  `runtimeevents.ChannelJSONRPC`, new `RuntimeACPStdio` legacy token). It
  says nothing about ACP's own method vocabulary
  (`initialize`/`session/new`/`session/prompt`/`session/cancel`/
  `session/update`), which a concrete ACP adapter's `CLIAdapter`
  implementation supplies in follow-on work. ACP over TCP (Copilot
  CLI's `--acp` daemon mode) is deliberately left unmapped — agentkit
  has no TCP-session runtime kind yet; `TestRuntimeCapsACPTCPUnmapped`
  pins the current "not yet supported" state on purpose.
- **`wrapper/wrapper_acp_test.go`** — end-to-end proof that a
  fake/no-op `acp.Client`, composed into a fake `adapters.RuntimeAdapter`
  via `acp.DescriptorFor` + a minimal `provider.CLIAdapter` shim, drives
  through `Wrapper.Run`'s real agentkit jsonrpc-stdio path: the Client's
  `Launch`/`Prompt` are genuinely invoked from the spawn/parse-line
  path (not just declared side by side), and its `Events()` output
  reaches `runtimeevents.Event` values in the sink via the existing
  translation path. No real ACP wire-format knowledge appears in the
  fake — it's a JSON-echo script, not an ACP implementation.

### Notes

- **Pre-existing flaky test found, not fixed, during this task's
  verification pass**: `TestRunEndToEndAdapterRuntime`
  (`wrapper/wrapper_integration_test.go`) intermittently reports
  "sequence not monotonic" under `-count=20`+ repeats — reproduced on a
  clean `v0.4.0` checkout (commit `4eed6c7`) in an isolated worktree,
  unrelated to any change in this release. Looks like a real race in
  concurrent `Emit` ordering across `Wrapper.Run`'s several
  emitter-calling goroutines (fanout translator, provider typed-event
  callback, stdout/stderr stream writers) racing the shared
  `runtimeevents.Sequencer`, not a test-harness artifact — worth a
  dedicated follow-up, out of scope for this release.
- No new dependency: `acp` imports only `adapters` (this module) and
  `go-runtime-events` (already required). `go.mod` is unchanged.

## v0.4.0 — 2026-08-21

Two changes, landed together as this release:

- **Adds the `snapshot` package**: `FilesystemSnapshotProvider` interface + a
  `ShadowGit` implementation (`TASKS/filesystem-snapshots/01`, Nanite's own
  tracker) — capture/diff/preview/selective-restore of an agent's granted
  filesystem paths via a separate internal git object database, isolated
  from any real repo's own `.git`. Additive; nothing else in this repo
  changes shape.
- **Bumps the `agentkit` pin to v0.5.0** (from v0.3.0), picking up two real
  correctness fixes to session-waiter/completion-signaling code found by
  Nanite's own live dogfeed (`TASKS/agent-host-acp/07`, `20`, `22`):
  the unsupervised waiter now surfaces a real `*agentsessions.ExitError` on
  abnormal exit (previously silently swallowed for the overwhelming
  majority of real-world kills/crashes — see agentkit's own v0.4.0
  CHANGELOG entry for the full detail, since it's a real behavioral change
  for any direct `agentkit` consumer too), and adapter-runtime sessions now
  synthesize a terminal event when the driven CLI adapter's own `ParseLine`
  never emits one (true for OpenCode's default mode). **The agentkit local
  `replace` this repo carried since v0.1.0 is dropped as of this release** —
  v0.5.0 is pushed and tagged on origin, so the plain `require` line
  resolves directly; no local checkout needed anymore.

## v0.3.0 — 2026-08-21

Real-adapter viability release. Closes the gap between what
`Wrapper.Run` actually forwarded to `agentsessions.StartOptions` and
what its own three shipped adapters (`adapters/claude`/`codex`/
`opencode`) need to run at all — every one of them selects an agentkit
runtime kind (streaming-stdio / jsonrpc-stdio / serve-http) that
hard-errors before spawning anything when `StartOptions.WorkspaceDir`
and `LogPath` are both empty, and until this release `Wrapper.Run`'s
hardcoded `StartOptions{}` literal never set either — confirmed
non-functional for all three real adapters, not just read (Nanite
`TASKS/agent-host-acp/06`, logged in Nanite's `TASKS/ESCALATIONS.md`,
2026-08-21). Also backfills the changelog entry for the
`Descriptor.Protocol`/`Transport` split (commit `371c9d0`), which
landed on `main` after `v0.2.0` was tagged and had not previously been
changelogged.

### Added

- **`Config.WorkspaceDir` / `Config.LogPath`** — forwarded to
  `agentsessions.StartOptions.WorkspaceDir`/`LogPath`. When both are
  left empty, `Wrapper.Run` synthesizes
  `<Workdir>/.wrapper-workspace/<SessionID>` rather than returning a
  required-field error — the same "zero values degrade cleanly"
  contract `Config.BootDir` already gives callers. Closes the hard,
  unconditional blocker described above.
- **`Config.SessionIDPreset`** — forwarded to
  `agentsessions.StartOptions.SessionIDPreset`. Needed by Claude's
  post-restart `--resume <id>` resume flow; a new integration test
  confirms it reaches the real `ClaudeAdapter.BuildArgs` argv shape end
  to end (against the real adapter, not a fake).
- **`Config.OnSessionID`** — forwarded to
  `agentsessions.StartOptions.OnSessionID`. `Wrapper.Run` now also
  unconditionally rebinds `Process.ProviderSessionID` from inside that
  same callback (previously this rebind only happened from the
  `EventFanout` consumer goroutine). Needed because not every
  runtime's session-id delivery reaches `EventFanout`: agentkit's
  `serveHTTPSession.createSession` (OpenCode's primary, first-session
  delivery point) calls `OnSessionID` directly and never pushes a
  matching `EventFanout` frame, so a caller observing only the
  `activity.Bridge`'s `Sink` would never have seen that session id at
  all. Claude's streaming-stdio path and OpenCode's own secondary SSE
  `session.created` path already fire `OnSessionID` and `EventFanout`
  together, so for those two the pre-existing rebind alone would have
  sufficed — this field specifically closes the `createSession` gap.
  See the doc comment on `Config.OnSessionID` for the full
  investigation this finding is based on.
- **`Config.AutoFireFirstTurn` / `Config.FirstTurnPayload`** —
  forwarded to `agentsessions.StartOptions.AutoFireFirstTurn`/
  `FirstTurnPayload`. Needed by every `ModeOneShot`/`ModeSubagent`/
  `ModeBackground` boot, which relies on the runtime auto-delivering
  the kickoff payload as the first turn rather than the caller racing
  its own `SendInput` against `Start`'s return.
- **New integration tests** (`wrapper/wrapper_real_adapters_test.go`)
  drive `Wrapper.Run` against the real, non-empty `Descriptor` of all
  three shipped adapters (`ProtocolClaudeStreamJSON`/`TransportStdio`,
  `ProtocolCodexAppServer`/`TransportStdio`,
  `ProtocolOpenCodeNative`/`TransportHTTPSSE`) via each adapter's real
  env-var `Detect()` override (`CLAUDE_CLI_PATH`/`CODEX_CLI_PATH`/
  `OPENCODE_CLI_PATH`) pointed at a fake binary — not the
  empty-Protocol/Transport fallback pair every prior integration test
  in this repo used, which is exactly why the `WorkspaceDir`/`LogPath`
  gap went undetected in the first place.

### Changed (backfilled from commit `371c9d0`, unreleased since `v0.2.0`)

- **`adapters.Descriptor.Runtime` (a bare string) removed, replaced by
  typed `Protocol` + `Transport` fields.** The single string conflated
  wire-format shape (streaming NDJSON vs. JSON-RPC vs. plain PTY
  bytes) with transport medium (stdio vs. HTTP+SSE) — a collapse that
  was harmless while every runtime implied a unique transport but
  breaks once ACP (same protocol over stdio or TCP) lands. This is a
  breaking change to `Descriptor`'s literal shape for any external
  caller (`go-agent-wrapper` has zero adopters to date, so nothing in
  the portfolio is broken by it in practice) — acceptable pre-1.0 per
  this repo's own SemVer discipline. `wrapper/runtime_dispatch.go`'s
  dispatch/mapping functions now key off `Protocol`+`Transport` pairs;
  a `legacyRuntimeToken` helper preserves the exact pre-split token
  mirrored into `runtimeevents.Process.Runtime` so downstream
  consumers of that (separate, go-runtime-events) field see no
  behavior change. No behavior change for the three shipped adapters
  themselves — same `agentsessions.Capabilities` flags, same source
  channels, same legacy `Process.Runtime` token values as before the
  split.
- **`adapters.Descriptor.InterruptCapability`** (`none`/`process`/
  `turn`/`steer`) — new capability-discovery field, set to the
  verified real value per shipped adapter: Claude and Codex are
  `InterruptProcess` (their agentkit sessions close stdin and escalate
  straight to SIGTERM/SIGKILL, no wire-level cancel frame); OpenCode
  is `InterruptTurn` (calls its native `/global/dispose` +
  `/session/{id}/abort` endpoints before the same escalation).

### Notes

- `Config`'s six new fields above (`WorkspaceDir`, `LogPath`,
  `SessionIDPreset`, `OnSessionID`, `AutoFireFirstTurn`,
  `FirstTurnPayload`) are purely additive and keyed-literal compatible
  — no existing `Config{...}` construction needs to change. The one
  breaking change in this release is `Descriptor.Runtime`'s removal,
  covered above.
- The local `replace github.com/hollis-labs/agentkit => ../agentkit`
  block (see the comment above the `replace (...)` block in `go.mod`)
  stays in place for the same reason `v0.2.0`'s entry documented —
  unchanged by this release.

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
