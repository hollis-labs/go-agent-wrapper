# Changelog

All notable changes to go-agent-wrapper are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## v0.18.0 — 2026-10-01

### Changed

- **`plant` takes provider settings paths from the go-providers layout**
  (CW-20261001-0074, D-73: one list). `Spec.ProviderSettings[name]` is
  planted at the native-config path of the layout's every-mode row for
  runtime `name`. `name` may be an id or an alias. The hard-coded
  claude/codex/opencode switch is gone. Paths, relative to the boot dir:
  - **Claude:** `.claude/settings.json` (unchanged).
  - **Codex:** `config.toml`, was `.codex/config.toml`. Codex reads it
    under `CODEX_HOME=boot`.
  - **OpenCode:** `opencode.json`, was `.config/opencode/opencode.json`.
    OpenCode reads it under `OPENCODE_CONFIG_DIR=boot`.
  - **Antigravity:** `.agents/plugins/tether/plugin.json`, was a guessed
    `.config/antigravity/settings`.
- **Breaking:** a name the registry doesn't know, or a runtime launched
  only over ACP (copilot, pi), is now an error. Before, plant fell back to
  `.config/<name>/settings`.

## v0.16.0 — 2026-10-01

Prepared launches run each turn with its own argv (CW-20260930-0135, the
wrapper half of "one argv owner"). Pairs with agentkit v0.13.0 (was
v0.12.2); go-providers v0.34.1 and go-sandbox v0.4.1 are unchanged.

### Fixed

- **A prepared launch no longer reruns the first turn every turn.** The
  wrapper hands `Config.PreparedExecution` straight to agentkit's Start.
  agentkit v0.13.0 resolves each turn's argv from the prepared execution's
  launch template (`Bindings.Launch`): the turn's own prompt, `--resume` /
  `--session` / `--conversation` with the session the previous turn reported,
  and the launch's own flags. Before, every turn reused the frozen first-turn
  argv: the boot prompt again, no resume, and the provider argv appended a
  second time. A streaming-stdio Claude launch gets its boot prompt as the
  first stdin turn.
- **The prepared adapter no longer hides capabilities.** It forwarded only
  `EventParser`. It now forwards `SessionLostClassifier`,
  `AuthFailureClassifier`, `SessionResumeVerifier` and `Preflighter` as well,
  answering as an adapter without the interface would when the inner one
  lacks it. A prepared agy launch, for example, now detects a lost
  conversation and an auth failure. `BootDirProvider` is deliberately not
  forwarded: the prepared boot dir is already planted, and forwarding it would
  let the session plant again whenever `AutoPlantBootDir` is set.
- The prepared adapter's `BuildArgs` resolves the launch template for the turn
  instead of returning nil. A prepared execution without a template keeps its
  frozen argv.

## v0.15.0 — 2026-10-01

Apps pick any agent runtime by id and mode through one call, native or ACP
(CW-20260930-0134, EP-20260930-0001). Pairs with agentkit v0.12.2 and
go-providers v0.34.1, takes go-sandbox v0.4.1, and adds a dependency on
agent-contracts-leaf v0.3.0. go-providers v0.34.1 is a security fix
(CW-20261001-0069): an untrusted turn is never parsed as a CLI flag, because
claude print, codex exec and opencode run take the prompt last, after `--`.
So their argv now ends in `-- <prompt>`.

### Added

- **Package `launch`.** `launch.Select(launch.Selection{Runtime, Mode, ...})`
  returns the adapter for any runtime in the go-providers registry, by id or
  alias:
  - Native factories wrap the go-providers adapter: Claude streaming-stdio
    and subprocess-per-turn; Codex app-server and exec; OpenCode run and
    http-sse; Antigravity per-turn.
  - ACP factories wrap claudeacp, codexacp, opencodeacp, copilotacp (stdio
    and TCP, with `Selection.Port`) and piacp.
  - An unset mode is the registry's default: Claude streaming-stdio, Codex
    jsonrpc-stdio (D-74), OpenCode and Antigravity subprocess-per-turn,
    Copilot and Pi acp-stdio.
  - The factory set is closed. `launch.Supported` lists it, and a registry
    mode it does not drive (Claude's PTY TUI) is `ErrUnsupportedSelection`.
  - Native adapters come from go-providers' `provider.NewAdapter`, the one
    constructor table shared with agentkit's planting path. The wrapper keeps
    only its dispatch facts (protocol, transport, channel) per pair.
  - All six runtimes launch through `launch.Select` + `wrapper.New` + `Run` in
    `TestLaunchEveryRegistryRuntimeThroughSelect`, against go-providers
    `providertest` fakes replaying captured CLI output. 5 of 6 complete a
    turn. For Codex app-server, only the spawn, its argv and the first
    payload are verified: the wrapper does not drive the Codex thread
    protocol, the host does through agentkit's `turn`.
    `TestLiveLaunchEveryInstalledRuntimeThroughSelect` does the same against
    installed CLIs behind the shared live-provider gate.

### Changed

- **Selected native adapters are no longer wrapped.** `Selection.Binary` and
  `Selection.ExtraArgs` are set on the go-providers adapter's own `Binary` and
  `ExtraArgs` fields (go-providers v0.34.0), copied so a host's adapter is
  never mutated. As a result:
  - every optional interface survives selection (`EventParser`,
    `SessionLostClassifier`, `AuthFailureClassifier`, `Preflighter`,
    `SessionResumeVerifier`, `BootDirProvider`), pinned by
    `TestNativeAdaptersKeepTheirOptionalInterfaces`;
  - a pinned binary reaches `Detect`.

  **Argv change:** `ExtraArgs` now land at each convention's extra slot
  instead of after everything:
  - before Claude's and agy's variadic `--add-dir`;
  - before codex exec's `--json`;
  - before opencode run's trailing message.

  A host-supplied *custom* `CLIAdapter` cannot take `Binary`/`ExtraArgs`
  without being wrapped, so that combination is now `ErrInvalidSelection`.
- **More events from Select-built sessions** (additive). Because the adapter
  reaches agentkit unwrapped:
  - sessions now emit `agent.tool_result`, `agent.subagent_spawn` and
    provider heartbeats;
  - agentkit's session-lost, auth-failure and resume-verify handling
    switches on.

  Consumers that switch exhaustively on event kinds will see new ones.
- `codexacp.WithDirectBinary` / `WithClientDirectBinary` run an installed
  `codex-acp` directly, with no npx, `-y` or package spec, as claudeacp and
  piacp already could. `launch.Select` uses it for `Selection.Binary` on
  Codex ACP. `WithBinary` still replaces only `npx`.
- The `wrapper.Runtime*` `Process.Runtime` tokens take their values from
  agent-contracts-leaf `runtimes.Mode`. The values are unchanged, and
  `adapter` keeps its spelling.

### Removed

- **Breaking:** `adapters.Select`, `adapters.Selection`, `adapters.Provider`
  and its constants, `adapters.RuntimeKind` (`cli`/`api`) and
  `adapters.LaunchMode` with its constants (D-73, no aliases per D-22). Use
  `launch.Select` with `Runtime` (a registry id such as
  `string(runtimes.Codex)`) and `Mode` (a `runtimes.Mode`). The old launch
  modes map like this:

  | Old | New |
  |---|---|
  | `LaunchAppServer` | `runtimes.ModeJSONRPCStdio` |
  | `LaunchServeHTTP` | `runtimes.ModeHTTPSSE` |
  | `LaunchStreamingStdio` | `runtimes.ModeStreamingStdio` |
  | `LaunchSubprocessPerTurn` | `runtimes.ModeSubprocessPerTurn` |
  | `LaunchDefault` | an empty `Mode` |

  `RuntimeKindAPI` has no replacement: an API provider is not a CLI runtime
  the wrapper launches. Callers are Nanite `internal/runtime/agent/factory.go`
  and Torque `internal/runtime/agent/boot.go` (Sprint 4).
- **Breaking:** `ErrUnsupportedSelection` and `ErrInvalidSelection` moved to
  package `launch`. `Select` returns `adapters.Adapter`, not
  `adapters.RuntimeAdapter`: an ACP adapter is not a RuntimeAdapter in the
  native sense. A native result still implements `RuntimeAdapter`; type-assert
  for it.

### Migration notes

- **Data migration:** stored mode strings `app-server` and `serve-http`, and
  any other old launch-mode spelling, now fail at runtime with
  `ErrUnsupportedSelection`. They are not translated. Nanite's and Torque's
  persisted profiles need a data migration to the `runtimes.Mode` spellings
  (`jsonrpc-stdio`, `http-sse`, `subprocess-per-turn`, `streaming-stdio`)
  before they adopt this release.

## v0.14.0 — 2026-10-01

Codex app-server approvals are answered from a permission posture instead of
refused (CW-20260930-0139).

### Added

- **`Config.PermissionPosture`** (go-permission `Mode`: `default`,
  `accept-edits`, `plan`, `yolo`; zero value `default`). On the native Codex
  app-server runtime the `JsonRpcRequestHook` now answers server-initiated
  approval requests through agentkit's `turn.CodexApprovalResponder`
  instead of refusing every one with -32601. `default` approves the MCP tool
  calls the launch planted and declines sandbox escalations (commands, file
  changes outside the writable roots); `accept-edits` also approves file
  changes; `plan` declines all three; `yolo` approves all three. Requests the
  responder cannot decide for a human (user-input questions, permission
  profiles, dynamic tool calls, other elicitations) are still refused with a
  JSON-RPC error in every posture. `New` rejects an unknown mode.
- `agent.permission.resolved` now carries `posture`, `reason` and, for an
  approval, `kind` (`mcp_tool_call`, `file_change`, `command_execution`),
  alongside `method`, `allowed` and `error`.

### Changed

- **Behavior change:** with the zero-value posture, a Codex app-server
  session's MCP tool-call approvals are granted where they were refused.
  Set `PermissionPosture: permission.ModePlan` to keep refusing them.
- Requires `agentkit` v0.11.0 (was v0.9.0), which brings `go-sandbox` v0.4.0
  (was v0.3.0); adds `go-permission` v0.1.0. `go-providers` stays at
  v0.30.0; v0.31.0 waits for the paired agentkit release
  (CW-20260930-0133).
## v0.13.1 — 2026-10-01

Fix: one terminal event per native turn (CW-20260930-0137 slice a; root cause
of CW-20261001-0019, Nanite's lost reply).

### Fixed

- **Usage no longer closes a turn.** `llmtypes.EventUsage` was translated to
  its own `turn.completed`, which closed the turn and emitted `session.idle`,
  so the adapter's real terminal (`EventDone`) went out afterwards as a second
  `turn.completed` with no TurnID. The feed read `turn.completed(turn_id,
  usage)` → `session.idle` → `turn.completed(no turn_id)` for Claude
  (streaming stdio) and Codex alike. Usage is now accumulated over the turn
  (token counts summed, latest stop reason kept) and attached under `usage`
  to the turn's one terminal event, `turn.completed` or `turn.failed`, which
  carries the TurnID and is followed by `session.idle`.
- A terminal event that arrives with no open turn opens one first, so it is
  still tagged and followed by `session.idle`.
- A turn still open when the child exits is closed as `turn.failed`
  (`reason: "process_exited"`, `exit_code`, any `wait_error` and accumulated
  `usage`), followed by `session.idle`, before `process.exited`. Previously
  such a turn was left open.

### Changed

- Consumers that read usage from a usage-only `turn.completed` now find it
  on the turn's terminal event under the same `usage` key, and see one
  `turn.completed` per turn instead of two.

## v0.13.0 — 2026-09-30

Dependency convergence: go-agent-wrapper now builds against released tags instead
of an agentkit pseudo-version.

### Changed

- Requires `agentkit` v0.9.0 (was the `04514ae` pseudo-version), `go-providers` v0.30.0 (was v0.26.0) and `go-materialize` v0.1.0 (was a pseudo-version). The agentkit and go-providers pair moves together.
- OpenCode's per-turn run now goes through `opencode run --format json`, so `Select` returns argv `run --format json --agent <name> <prompt>` and the wrapper normalizes OpenCode's typed JSON output into events. A host that pinned the old plain-text argv must update.
- Tests: the OpenCode argv pin and the fake `opencode` in the subprocess-per-turn integration test now use the JSON output shape.

## v0.12.0 — 2026-09-30

### Changed

- **Behavior change: `adapters.Select` now resolves OpenCode's unset launch mode to `LaunchSubprocessPerTurn`** (`opencode run`), not `LaunchServeHTTP`. OpenCode's serve-http runtime is deferred until its SSE and permission behavior is probed, so it should not have been the default. `LaunchServeHTTP` still works when requested explicitly. A host that relied on the unset mode getting serve-http must now pass `LaunchMode: LaunchServeHTTP`. Claude (streaming stdio) and Codex (app-server) defaults are unchanged.
- README: the `LaunchDefault` description matches; the Codex difference from agentkit's `runtimebind` default remains documented as open.

## v0.11.1 — 2026-09-30

Docs and comments only; no code change.

### Changed

- README and ROADMAP status now read v0.11.0, `go get` names the current tag, and the dependency list matches `go.mod`.
- The `ProtocolACP` and `TransportTCP` comments no longer say no shipped adapter uses them: five ACP adapters exist and `copilotacp` uses TCP. The `RuntimeACPTCP` dispatch comment now describes the wrapper-owned `acp.Manager` path instead of saying TCP has no case.
- `adapters/opencode` docs point at `LaunchSubprocessPerTurn` for run mode.
- The README states that `LaunchDefault` (Codex app-server, OpenCode serve-http) is this library's default and differs from agentkit's `runtimebind` default (subprocess-per-turn), so hosts should request a mode explicitly.

## v0.11.0 — 2026-09-30

### Added

- `acp.NDJSONBridgeClient`, `acp.NDJSONBridgeConfig` and `acp.RPCError`: one
  shared newline-delimited JSON-RPC ACP client (handshake, turn lifecycle,
  termination coordination, permission dispatch) parameterized by command
  resolution, notification translation and an optional launch-environment hook.
  `CurrentTurnID` and `Emit` let a translator stamp and emit events.

### Fixed

- `claudeacp`, `codexacp`, `opencodeacp` and `piacp` now report a JSON-RPC
  response with a non-numeric id as a protocol diagnostic and count it as
  malformed input for the termination coordinator, as `copilotacp` already did.
  They previously dropped it silently.

### Changed

- `claudeacp`, `codexacp`, `opencodeacp` and `piacp` delegate their client
  implementation to `acp.NDJSONBridgeClient`; each keeps its exported `Client`
  type, options and behavior, and shrinks to command resolution plus its own
  notification translator.
- A JSON-RPC error returned by `codexacp` and `opencodeacp` now reads
  `codexacp: jsonrpc error …` / `opencodeacp: jsonrpc error …` instead of the
  unprefixed `acp: jsonrpc error …`, matching `claudeacp` and `piacp`.

- Update agentkit to pick up its cutover to
  [`go-materialize`](https://github.com/hollis-labs/go-materialize)
  (CW-20260918-0036): `artifact`/`materialize` moved out of agentkit into
  their own module. `plant/plant.go`, `wrapper/prepared.go`,
  `wrapper/wrapper.go` and the shared-conformance example now import
  `go-materialize/artifact` and `go-materialize/materialize` directly
  instead of `agentkit/artifact`/`agentkit/materialize`.

## v0.10.1 — 2026-09-06

- Update agentkit to v0.6.1 to preserve streaming terminal events when a
  prepared child exits before its stdout reader runs.
- Install and probe bubblewrap in Linux CI so required prepared-execution
  sandbox enforcement is exercised rather than failing on a missing backend.

## v0.10.0 — 2026-09-06

### Added

- Prepared-execution and shared-planter entry points preserve exact launch
  bindings and artifact ownership without planting a second time.
- Local native/ACP launches enforce resolved sandbox policies or reject
  unsupported required confinement before spawn. Remote/pre-existing ACP
  endpoints report their enforcement limitations explicitly.
- Typed delivery capabilities, content-free correlation fields, route
  generation checks and honest receipt stages; `PlanDelivery` reports
  unsupported, busy, offline and stale-route outcomes without sending.
- Cross-library conformance examples and fixtures cover tree materialization,
  prepared execution, runtime delivery capabilities and OS denial behavior.

### Changed

- Update dependencies to agentkit v0.6.0, go-providers v0.26.0,
  go-sandbox v0.3.0 and transitive go-runner v0.7.0.

## v0.9.1 — 2026-09-05

### Fixed

- ACP `Close` now preserves graceful prompt/session-close wire ordering while
  bounding admission and terminal-drain waits, then preempts a blocked Prompt
  transport write across Claude, Codex, Copilot, OpenCode, and Pi. Regression
  coverage includes Copilot's TCP transport as well as stdio.
- Real-provider integration tests now require the explicit
  `GO_AGENT_WRAPPER_LIVE_PROVIDER_TESTS=1` opt-in before checking installed
  CLIs or launching provider/bridge processes. The ordinary deterministic
  suite no longer mistakes a hosted runner's bundled `npx` for configured
  Claude credentials.
- Executable test fixtures are now published at unique, immutable paths only
  after their writers are closed, preventing Linux `ETXTBSY` races in the
  concurrent and race-enabled suites.

## v0.9.0 — 2026-09-05

This is a minor release under the module's pre-1.0 compatibility policy. It
contains substantial additive ACP lifecycle, environment, adapter-selection,
and permission-response APIs, plus one intentional breaking correction to the
misleading policy API described below.

### Added

- **Best-effort ACP permission responder.** Hosts can set
  `wrapper.Config.ACPBestEffortPermissionRequestResponder` (or the matching
  `acp.LaunchParams` field) to answer `session/request_permission` across
  Claude, Codex, Copilot, OpenCode, and Pi. The shared option-ID contract
  validates provider offers, preserves raw request extensions for the approval
  UI, cancels with turn/session lifecycle, and fails closed on malformed input,
  callback errors/panics, session mismatch, or invalid selections. It is
  explicitly not a general execution gate where a provider does not ask.
  Request and callback admission are bounded per client, response/cancel writes
  have teardown-safe deadlines, and all clients preserve numeric, string, and
  schema-present null request IDs while rejecting invalid ID shapes. An
  undeliverable permission response emits a fixed, redacted fail-closed event
  and terminates the transport instead of leaving the child blocked. Reader
  admission binds every request to one immutable turn generation; prompt
  completion closes that generation and waits its admitted responses, while a
  later frame is cancelled quietly rather than reclassified into the next turn.

- **Explicit child-process environment contract.** `wrapper.Config.Environment`
  now accepts a typed `ChildEnvironment` with inherit/merge/replace modes, an
  inherited-key allowlist, ordered `Set` entries (last duplicate wins), and a
  final `Unset` list. Native and ACP subprocesses receive the materialized
  environment directly through their spawn APIs; no shell command or generated
  `env -i` wrapper is required. The zero value retains ambient inheritance.
- **First-class native adapter selection.** `adapters.Select` resolves a
  `Selection` keyed by provider, Runtime kind, and launch mode. It includes
  Claude streaming-stdio and subprocess-per-turn (with explicit developer-mode
  constructors), Codex app-server and subprocess-per-turn, and OpenCode
  serve-http and subprocess-per-turn. Absolute binary overrides and extra args
  remain direct os/exec path/argv values. Existing per-provider default launch
  shapes are unchanged.

- **Wrapper-owned ACP session lifecycle.** `acp.Manager` and its managed
  `Session` now own registration, initialize/authenticate, create-or-resume,
  deterministic mode/config application, prompts, turn-scoped cancellation,
  close, liveness snapshots, provider session-id readback, automatic
  unregister, and exactly-once client cleanup. `Wrapper.Run` uses this path for
  every shipped ACP adapter (Claude, Codex, Copilot, OpenCode, and Pi), including
  Copilot's TCP transport; hosts no longer need a parallel direct `acp.Client`
  or liveness registry.
- Added typed ACP terminal outcomes for unexpected disconnect, child-process
  exit, malformed streams, and canceled operations, plus bounded redacted
  stderr/protocol diagnostics through `Config.OnACPDiagnostic`.
- Added `Wrapper.CancelTurn`, `ProviderSessionID`, `ACPSnapshot`, and
  `ACPManager`, with `Config` controls for ACP authentication, session mode,
  and session configuration.

### Changed

- `Wrapper.Run` now passes the materialized child environment into both
  `agentsessions.StartOptions.Env` and ACP `LaunchParams.Env`, and honors a
  non-nil `Adapter.Resolve` `Spec.Env` as the adapter's final replacement.
- Native wrapper teardown now closes new input admission and drains accepted
  `SendInput` calls before closing the event fanout, preventing a canceled
  subprocess-per-turn call from racing a terminal event into a closed channel;
  the same ordering applies when post-start sandbox setup fails.
- Codex ACP now resolves its optional `CODEX_PATH` only from the environment
  supplied to the launch (or its explicit client option), so a sanitized child
  environment cannot re-import an excluded ambient CLI path.

- Serialized each `activity.Bridge` sequence assignment with its sink write so
  concurrent lifecycle, heartbeat, and stream producers cannot deliver event
  sequence N+1 before N.
- ACP launch now uses a two-phase host commit: provider identity and wrapper
  control authority are installed before Manager readiness becomes observable.
  `Wrapper.Run` clears that live authority at teardown while retaining the
  provider ID for postmortem correlation.
- All shipped ACP clients validate the negotiated protocol version before any
  auth/session request and gate `session/load` on the advertised capability.
  ACP v1 load responses retain the requested session ID (including the pinned
  `codex-acp@1.6.2` response, which has no `sessionId`), and an advertised load
  failure no longer silently falls back to `session/new`.
- Accepted asynchronous prompts in Claude, Codex, OpenCode, and Pi are now
  owned by the client/session lifetime instead of the accepting caller's
  context. Explicit cancel/close and transport teardown remain authoritative.
- One transport-termination coordinator now joins protocol-reader and child
  completion before closing events. Recorded malformed input deterministically
  outranks a consequent child exit.

- **Breaking: the post-hoc policy callback is now explicitly observational.**
  The v0.8.1 API exposed action-shaped names even though `Wrapper.Run`
  consulted it only after emitting `agent.tool_use`, too late to prevent or
  replace the child operation. The replacement API is:

  | v0.8.1 | v0.9.0 |
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
  execution. ACP `session/request_permission` is now a separate, narrower
  best-effort responder point. Nil preserves Claude/Codex/OpenCode/Pi's
  well-formed cancelled outcomes and Copilot's distinct JSON-RPC
  method-not-handled behavior. The measured coverage table calls out providers
  that execute ordinary tool classes without asking.

- **Release-ready dependency graph.** Removed the local
  `go-harness-filters` and `go-runtime-events` replacements and now require the
  published v0.1.1 and v0.1.2 tags, respectively. The old v0.8.1 requirements
  were stale behind those replacements: a clean consumer otherwise failed to
  compile first on `repair.Chain`, then on
  `runtimeevents.KindPolicyApprovalRequested`. The module now resolves all
  dependencies from the public Go proxy with no sibling checkout.
- Raised the declared Go patch level from 1.26.1 to 1.26.6, which contains the
  standard-library security fixes reported by `govulncheck`. CI reads that
  exact version from `go.mod` instead of following the moving `stable` alias
  and source-builds pinned golangci-lint v2.11.4 with the same toolchain.

### Tests

- Added adversarial environment coverage for inherited allowlists, empty
  allowlists, unsets, duplicate assignments, spaces, metacharacters, NULs, and
  ambient-secret exclusion, plus Windows case-insensitive key behavior,
  including real Claude/Codex/OpenCode child processes.
- Added provider/runtime/launch-mode selection coverage plus real cancellation,
  process-reaping, direct argv, and normalized-event tests for Claude streaming
  and Codex/OpenCode subprocess-per-turn paths.

- Added real subprocess ACP fixtures covering fresh/resumed handshake,
  authentication/configuration, provider session IDs, prompt/cancel/re-prompt,
  disconnect, malformed stream, child exit, and exactly-once cleanup across all
  five shipped adapters, plus Copilot TCP coverage and lifecycle race/stress
  coverage.
- Added deterministic regressions for pre-commit readiness, ACP version/load
  capability negotiation, spec-valid Codex resume, accepted prompt ownership,
  malformed/exit precedence, and truthful post-`Run` control state.
- Added a native subprocess characterization proving a block recommendation is
  produced only after the child has already created a side-effect marker.
- Added ACP protocol coverage proving Claude's default cancelled response
  unblocks a child waiting on `session/request_permission`, and locking down
  Copilot's distinct method-not-handled default.
- Added a real subprocess conformance matrix across all five ACP clients for
  default, allow, reject, explicit cancel, turn cancel, callback error, invalid
  selection, concurrent requests, string/null/invalid request IDs, and responder
  re-entry into Prompt/Close, plus shared race/stress coverage, exact
  legacy-default assertions, real non-reading-child floods,
  backpressured response/cancel teardown, and redaction assertions.
- Safely measured provider-side Copilot CLI 1.0.12 behavior with a real stdio
  turn: a non-mutating `pwd` shell request emitted one ACP permission request
  (`kind: execute`) and the zero responder selection cancelled it. This is a
  one-shape measurement; synthetic fixtures separately cover stdio/TCP response
  handling and do not imply wider provider invocation coverage.

## v0.8.1 — 2026-08-21

The published tag added the Claude and Codex ACP adapter packages and the
side-by-side live comparison harness. The tag's tree did not include a
corresponding changelog section, so this historical entry records that shipped
surface. Its stale `go-harness-filters` v0.1.0 and `go-runtime-events` v0.1.0
requirements were masked during repository development by local replacements;
v0.9.0 corrects the published dependency graph.

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
