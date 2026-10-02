# Changelog

All notable changes to go-agent-wrapper are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## v0.27.2 — 2026-10-02

`ObserveProvider` consumes the provider's final text, and an `opencode run` turn
reduces as exact (CW-20261002-0061). Takes go-providers v0.45.0.

### Fixed

- **`Reducer.ObserveProvider` ignored `events.Done.Text`.** go-providers v0.44.0
  puts the turn's own final message on the typed `Done` (Claude's `result.result`,
  agy's `result.response`; v0.45.0 adds the last step of an `opencode run`), and
  the typed feed built its terminal from the stop reason alone. So the path a host
  taps with agentkit's `TypedEventCallback`, which is how Tether feeds native
  Claude, agy and OpenCode sessions, reported `heuristic` and could pick a
  different string than the runtime's final message. The wrapper path
  (`turn.completed`'s `text`) and `ObserveStream` (`EventDone.Content`) already
  read it; the replay tests ran through the wrapper's Activity sink, which is why
  this was missed.
  - **Now:** the typed feed takes `Done.Text` as the terminal text, `exact`, with
    the same precedence as the other feeds (terminal text, then a final-phase
    delta, then the last block).

### Changed

- **go-providers v0.44.0 → v0.45.0**, whose `opencode run` adapter puts the text
  of the step that ends the turn on its done. An OpenCode turn now reduces as
  `exact` through the wrapper too (it carries a done's text as `turn.completed`'s
  `text` since v0.27.0). No wrapper code changed for this.

### Tests

- **The typed feed against the real adapters**: go-providers' captured fixtures
  go through the adapter that parses them (`ParseLineEvents`, what the typed
  callback delivers) into `ObserveProvider`: Claude print (including a last
  message after tool calls) and streaming (two turns), agy (including after a tool
  and an MCP tool), OpenCode run (including an answer after tool steps) and Codex
  exec. Each turn is `final`, `exact`, with the agent's last message as its text.
  Reverting the fix fails eight of the nine.
- The OpenCode wrapper replays assert `exact`: the captured one-step turn and the
  turn that runs two tool-calling steps and then answers. They fail on
  go-providers v0.44.0.

## v0.27.1 — 2026-10-02

`turnoutput` no longer drops a turn that had no events of its own (review of
agentkit#38, CW-20261002-0061).

### Fixed

- **A turn with no events of its own after an earlier completed turn was
  dropped, so a cancelled or tool-only turn was never reported.** On the
  provider and stream feeds, which carry no turn id, a lone `Done` while the
  reducer was settled was taken for a repeat of the previous turn's `Done`. Codex
  app-server reports nothing when a turn starts, so a turn interrupted before it
  said anything reaches the reducer as exactly that.
  - **Now:** a `Done` with no turn in progress is a repeat only if the last turn
    was completed and this one ends the same way (same stop reason and text), and
    a cancellation is never a repeat. A lone terminal event is never lost.
  - A stop reason that arrives while settled is held until the next event: a
    terminal event takes it (the stream feed reports the stop reason on a usage
    event ahead of the done), any other drops it (it trailed the turn that ended),
    which keeps v0.26.0's fix for a trailing usage leaking into the next turn.

### Tests

- The review's three-turn sequence (an answered turn, an interrupted turn with no
  message, a tool-only turn) on both the typed and the stream feed; two
  interrupted turns in a row; a different ending after a completed turn; a held
  stop reason that must not lead the next turn. Reverting the repeat rule or the
  held stop reason fails them.

## v0.27.0 — 2026-10-02

`turn.completed` carries the turn's own final message when the provider reports
one (CW-20261002-0061, ADR 0049). Takes go-providers v0.44.0.

### Added

- **`turn.completed` `text`**: a native turn's final message, from the provider's
  terminal event (`EventDone.Content`): Claude's `result.result` and agy's
  `result.response` today (go-providers v0.44.0), the last step of an
  `opencode run` once go-providers v0.45.0 is taken. Absent when the provider
  reports none (Codex exec marks its final delta with phase `final` instead), and
  the rest of the payload is unchanged.
  - `turnoutput` reads it as the turn's exact text, so a Claude or agy turn now
    reduces with `confidence: exact` where v0.26.0 reported `heuristic`.
  - The text passes the same `agent_text` filters as the deltas that carried the
    same words, so a repair that applies to the stream applies to it.

### Tests

- The captured Claude streaming transcript (two turns) and the agy print capture,
  through the real wrapper into the reducer, now reduce as `exact`; OpenCode stays
  `heuristic` until it takes go-providers v0.45.0. Unit tests for the payload and
  for the filter. Dropping the payload text or the filter fails them.

## v0.26.0 — 2026-10-02

A host can now ask a session for what each turn said to the user
(CW-20261002-0061, ADR 0049).

### Added

- **`turnoutput`** reduces a session's events to one `Output` per completed
  turn: `{session_id, turn_id, text, kind, stop_reason, runtime, confidence}`.
  - `kind` is `final`, `question`, `approval`, `failure` or `terminal`.
    `confidence` is `exact` when the runtime marked the text and `heuristic`
    when the reducer took the turn's last text block.
  - Reasoning, tool calls, tool results and the turn's earlier blocks are never
    in `text`.
  - A `Reducer` per session takes `Observe` (go-runtime-events envelopes: this
    wrapper's Activity sink and every ACP runtime), `ObserveProvider`
    (go-providers typed events, from agentkit's `TypedEventCallback`) or
    `ObserveStream` (agentkit's `EventFanout`, which drops events when its
    channel is full). Each returns an `Output` exactly when a turn ends.
    `Flush` ends a turn the runtime never finished.
  - Text comes from the terminal event's own text (`turn.completed`'s `text`),
    else the last block a delta marked `final`, else the last text block. The
    package doc says when `text` can be empty and how each kind is decided.
  - Turns are tracked by id: an event that arrives late for a turn already
    reported is dropped, and a turn that starts before the previous one's
    terminal event keeps its own text. The id-less feeds drop a repeated `Done`
    and a trailing stop reason, and never lose a lone `Error`.
  - Tested against what each runtime wrote, by running go-providers'
    captured fixtures through the wrapper into the reducer (Claude streaming,
    OpenCode, Antigravity, Copilot and Pi over ACP). A live-gated test does the
    same against every installed CLI.

### Not covered

- The wrapper does not yet put Claude's `result.result` or any other final
  text on `turn.completed`, so Claude, OpenCode and Antigravity report
  `heuristic` for now; the Output's shape does not change when they gain an
  exact marker.
- Codex's app-server emits no turn events through the wrapper, because the host
  drives its thread protocol, so there is nothing to reduce for it yet.

## v0.25.7 — 2026-10-02

The Copilot ACP client's request writes honor the caller's ctx
(CW-20261001-0238). The shared helper also includes CW-20261001-0261's
hardening: a completed write does not trigger a fallback close, interrupts
finish before deadlines are cleared, and unrelated write errors retain their
cause.

### Fixed

- **A stalled Copilot agent no longer pins `call` and `Prompt` writes.**
  - **Before:** `adapters/copilotacp`'s `Client.call` and `Prompt` wrote the
    request frame with no deadline, the same gap v0.25.4 closed in the NDJSON
    client. An agent that stopped reading stdin, or a TCP peer that stopped
    reading its socket, blocked the write. Canceling ctx did not release it,
    and because it held the writer lock, every later write queued behind it.
  - **Now:** the frame write is bounded by the ctx, over stdio and TCP alike.
    - It writes nothing once ctx has ended, and a caller queued behind a
      stalled write leaves when its own ctx ends.
    - A ctx that ends mid-write interrupts it: through the write deadline when
      the transport has one (a pipe and a socket do), otherwise by closing the
      transport.
    - If the interrupted write had put part of a frame on the wire, the
      transport is closed, because the agent would read a line it cannot
      parse. If nothing was written, it stays open and the request was never
      sent.
    - `Prompt` passes its ctx to the write only, gives the admission gate and
      the turn back when the write is released, and still pairs its
      `turn.started` with a `turn.failed`.
    - `call` already dropped its pending entry when ctx ended first.
  - A ctx that never ends still leaves the write bounded only by the
    transport closing, as before. There is no hidden default deadline.

### Changed

- **`acp.WriteFrameCtx`** is the one implementation of that bounded write,
  extracted from the NDJSON client's `writeLineCtx` (v0.25.4). Both clients
  call it. Canceled or deadline-interrupted writes keep the context error;
  unrelated write failures now retain their original cause, even if the context
  has also ended. NDJSON real-child tests now signal write entry, use 1 MiB
  requests and 4-second deadlines, and run serially to avoid fork/exec ETXTBSY.
- **Correction to v0.25.4's test claim:** restoring the old write path fails
  the blocked-write tests; the pending-entry test is unaffected.
- **Tested** over stdio against a real child that stops reading stdin, and
  over TCP against a peer that stops reading, with the socket buffers shrunk so
  a 1 MiB request blocks. A cancel and a deadline each return promptly with the
  ctx error, leave the writer lock free and nothing pending, and close the
  half-written transport. Fake writers cover a queued caller, a partial frame, a
  transport with no write deadline, an ended ctx, an abandoned call, and
  `Prompt`. The tests end the ctx only after the write has begun, because under
  `-race` on a loaded host building the frame can outlast a short ctx. With the
  unbounded write restored the blocked-write tests fail; the pending-entry
  test independently covers response cancellation. Deterministic helper tests
  cover deadline clearing, callback completion, and unrelated write errors.

### Not covered

- A second `Prompt` or `Cancel` queued behind a stalled `Prompt` still waits
  on `promptCloseMu`, whose admission lock does not honor the queued caller's
  context. This pre-existing gate limitation is separate from request writes
  and tracked as CW-20261002-0051.

## v0.25.6 — 2026-10-01

A host no longer panics when an ACP agent exits during a `Prompt`
(CW-20261001-0262).

### Fixed

- **`Prompt` racing the agent's own exit could panic the host process** with
  `sync: WaitGroup is reused before previous Wait has returned`. Under `-race`
  the same window is a reported data race. It affects the claude, codex,
  opencode and pi clients (the NDJSON client) and the Copilot client alike.
  - **Cause:** `Prompt` registers its turn with `turnWG.Add` while the
    transport's exit path, `closeEvents`, waits on `turnWG`. Only `Close`
    sealed admission before the wait. When the agent exited by itself, a
    `Prompt` admitted after `closeEvents` began waiting added to the
    WaitGroup concurrently with the `Wait`, which `sync.WaitGroup` forbids.
    This predates v0.25.4. A host that retries `Prompt` straight away after a
    failed turn, such as one that reacts to the partial-frame transport close
    v0.25.4 added, is the likeliest to hit it.
  - **Fix:** `closeEvents` now marks the client as closing, under the lock
    `Prompt` adds under, before it waits. A `Prompt` either added before the
    mark, so the wait is ordered after it, or finds the mark and is refused.
    No `Add` can run against the `Wait`.
  - **One visible change:** a `Prompt` after the agent's transport has ended
    now returns `<component>: the agent's transport has ended` and starts no
    turn, as one after `Close` already returned `client is closed`. Before, it
    was admitted, emitted `turn.started`, and failed on the write.
  - **Not changed:** `wrapper.Wrapper`'s `inputWG` and `acp.Session`'s
    `diagnosticWG` already add under their own mutex and close admission
    before they wait.
- **Tested** against a real child that exits on its first prompt, with
  further `Prompt`s spinning through the exit, then a `Prompt` after it.
  The NDJSON client runs it on all four components, and the Copilot client
  over stdio. Run on the unfixed base with `-race -count=50`, they fail on
  the first iteration with the data race and the production panic. With the
  fix, 100 iterations pass in each package.

## v0.25.5 — 2026-10-01

An ACP launch that cannot resume says so (CW-20261001-0223).

### Fixed

- **A preset session on an agent without `loadSession` is reported as lost.**
  - **Before:** `NDJSONBridgeClient.Launch`, given
    `LaunchParams.SessionIDPreset` and an agent that does not advertise
    `agentCapabilities.loadSession`, silently called `session/new`. The
    session started fresh while the host still believed it had resumed
    (Torque's `Session.Resumed` and `used_resume` said true).
  - **Now:** it emits `session.lost` after the session is configured and
    just before `session.ready`, with the same payload as the native
    runtimes' `session.lost`:
    - `requested_id`: the preset;
    - `actual_id`: the session the agent started instead;
    - `reason`: `agent does not support session/load; started a new session`
      (`acp.SessionLoadUnsupportedReason`).
    `Config.OnSessionID` already reported the new id, so a host can also
    compare it with the preset. The wrapper forwards the event unchanged.
  - **Unchanged:**
    - an agent that advertises `loadSession` still gets `session/load`, and a
      failed load is still a launch error, never a fallback to
      `session/new`;
    - a launch with no preset reports nothing;
    - a launch that fails before it is ready reports nothing.
  - New: `acp.NewSessionLostEvent` and `acp.SessionLoadUnsupportedReason`,
    for other ACP clients to report the same thing.
  - **Not covered:** `adapters/copilotacp` has the same silent fallback and
    still does not report it. Its `LaunchParams.SessionIDPreset` doc says so.
- **Tested** on the claude, codex, opencode and pi clients: with and
  without `loadSession`, with and without a preset, and through the wrapper
  with a real subprocess. With the emission removed, the new tests fail.

## v0.25.4 — 2026-10-01

NDJSON request writes honor the caller's ctx (CW-20261001-0211), and the
dependencies move to the latest set so hosts take one consistent pair.

### Changed

- **Dependencies:**

  | Module | From | To |
  |---|---|---|
  | agentkit | v0.20.4 | v0.21.0 |
  | go-providers | v0.41.0 | v0.42.0 |

  The rest of the set was already current: go-sandbox v0.6.0, go-runner
  v0.8.2, go-llm-contracts v0.4.0, go-llm-types v0.5.1. What the new versions
  bring:
  - **agentkit v0.20.5:** an OpenCode serve compaction summary no longer
    reaches the turn as reply text.
  - **agentkit v0.20.6:** OpenCode serve reasoning reaches the turn as
    thought, not reply text.
  - **agentkit v0.21.0:** `StartOptions.ExtraArgs` go at the adapter's
    convention slot. For codex exec, exec-only flags (`-s`, `--cd`,
    `--add-dir`) now land before `resume <id>`, so they work on turn 2.
  - **go-providers v0.42.0:** `provider.ExtraArgsBuilder`, which agentkit
    v0.21.0 uses.

  No wrapper code changed for this. The wrapper's prepared path already
  placed extras at the convention slot through the launch template.

### Fixed

- **A stalled agent no longer pins `Call` and `Prompt` writes.**
  - **Before:** `NDJSONBridgeClient.beginCall` wrote the request frame with
    no deadline, so `Call` and `Prompt` honored their ctx only while
    awaiting the response. An agent that stopped reading stdin and filled
    the pipe blocked the write. Canceling ctx did not release it, and
    because it held the writer lock, every later write queued behind it.
    `Notify` and the permission responses were already bounded; request
    writes were not.
  - **Now:** the frame write is bounded by the ctx. This applies to the
    claude, codex, opencode and pi ACP clients, which share the NDJSON
    client.
    - It writes nothing once ctx has ended, and a caller queued behind a
      stalled write leaves when its own ctx ends.
    - A ctx that ends mid-write interrupts it: through the write deadline
      when stdin has one, otherwise by closing the transport, as `Notify`
      does.
    - If the interrupted write had put part of a frame on the wire, the
      transport is closed, because the agent would read a line it cannot
      parse. If nothing was written, the transport stays open and the
      request was never sent.
    - `Prompt` passes its ctx to the write only. The response is still
      awaited on the client's lifetime.
  - A ctx that never ends still leaves the write bounded only by the
    transport closing, as before. There is no hidden default deadline, so a
    slow-but-alive agent is not cut off.
- **A `Call` that gives up waiting no longer leaves its request registered
  as pending.** A response that arrives later is ignored like one for any
  unknown id.
- **Tested** on all four components: a real child that answers the
  handshake and then stops reading stdin, plus fake stdins for a queued
  caller, a partial frame, a stdin with no write deadline, an ended ctx, and
  `Prompt`. With the old write path they all fail, because the `Call` never
  returns.

## v0.25.3 — 2026-10-01

### Fixed

- **Native turn cancellation is advertised** (CW-20261001-0200).
  `Wrapper.CancelTurn` interrupts a turn and keeps the process on Claude
  streaming-stdio, Codex app-server and OpenCode serve. Until now the native
  descriptors' `Delivery` never listed `adapters.DeliveryCapabilityCancelTurn`
  (only ACP did), so a host reading capabilities would not offer it. They now
  list it for exactly those runtimes. `launch.Select` derives the claim from
  the runtime and the configured adapter, so a host's own adapter that hides
  `provider.TurnInterrupter` or `provider.RPCTurnInterrupter` gets none. The
  per-turn runtimes (Claude print, Codex exec, OpenCode run, agy) still do
  not claim it. The `cancel_turn` evidence names each runtime's own mechanism.
- `Descriptor.Interrupt` is unchanged, because it describes `Stop`. Claude
  streaming-stdio and Codex app-server still stop at the process level
  (`InterruptProcess`), and OpenCode serve aborts natively before signaling
  (`InterruptTurn`). Its docs no longer claim that no interrupt frame is ever
  sent to Claude or Codex.
- `TestCancelTurnCapabilityMatchesCancelTurn` runs every native
  `launch.Supported()` mode on a fake. It checks that the descriptor's
  `cancel_turn` agrees with what `CancelTurn` does, and it names the three
  interruptible runtimes. `TestCancelTurnCapabilityACP` covers the ACP
  launches, and `TestSelectCancelTurnFollowsTheAdapter` covers a host
  adapter without the interface.

## v0.25.2 — 2026-10-01

A coherent dependency refresh, so hosts take one consistent latest set
(CW-20261001-0194). Dependencies and tests only; no wrapper code changes.

### Changed

- **Dependencies:**

  | Module | From | To |
  |---|---|---|
  | agentkit | v0.20.1 | v0.20.4 |
  | go-providers | v0.40.0 | v0.41.0 |
  | go-sandbox | v0.5.1 | v0.6.0 |
  | go-runner (indirect) | v0.7.0 | v0.8.2 |
  | go-llm-contracts (indirect) | v0.3.0 | v0.4.0 |

  What the new versions bring:
  - **agentkit v0.20.2–v0.20.4:** lost-session events on per-turn
    resumes; serve-http turns end only on their own session's errors; and
    the fix that pairs agentkit with go-providers v0.41.0 (below).
  - **go-providers v0.41.0:** codex exec resumes its thread, and
    `CodexAdapter.IsSessionLost`.
  - **go-sandbox v0.6.0:** `DenyUserServiceManager`.
  - **go-runner v0.8.2:** resource-limit and long-line fixes.

  go-llm-types (v0.5.1), go-runtime-events, go-harness-filters,
  go-materialize, go-permission and agent-contracts-leaf are already at
  their latest.
- **Codex exec sessions now resume their thread from turn 2.** Before, each
  turn started a new thread. This comes from go-providers v0.41.0 and
  agentkit v0.20.4.
- **Do not pair go-providers v0.41.0 with agentkit v0.20.3 or earlier.**
  That combination breaks codex exec turn 2 under agentkit's
  `AutoPlantBootDir`. This release pins the pair that works. The wrapper
  itself does not set `AutoPlantBootDir`.

## v0.25.1 — 2026-10-01

Lint findings, part 1 (CW-20261001-0066): nilerr and errorlint.

### Changed

- **Errors from `snapshot` and from the child environment now wrap their
  cause**, as well as their sentinel (`ErrShadowStoreUnavailable`,
  `ErrInvalidEnvironment`), so `errors.Is` and `errors.As` reach it. The
  message text is unchanged.

### Internal

- The `ParseLine` skips of unparseable lines in `adapters/copilotacp` and
  three tests are deliberate (the `provider.CLIAdapter` contract), and are
  marked `//nolint:nilerr` with that reason.
- Tests compare sentinel errors with `errors.Is`.

## v0.25.0 — 2026-10-01

An ACP launch honours `Config.ProtectedPaths` without a resolved
`SandboxPolicy` (CW-20261001-0162). Takes go-sandbox v0.5.1 (was v0.5.0).

### Security

- **`acp.LaunchParams.ProtectedPaths`** and **`acp.ProtectOnlyProfileID`**.
  `acp.PrepareLaunchSandbox` applies them to the spawned agent:
  - With a required `SandboxPolicy`, they are merged into it.
  - Without one, or with an explicitly disabled one, the child runs under
    `protect-control-plane`, agentkit's minimal profile: the host
    filesystem, writable, with these directories read-only. It is reported
    through `SandboxOutcomeCallback` like a resolved policy.
  - A backend that cannot write-protect paths refuses the launch.
  - Both launchers that call `PrepareLaunchSandbox` get it: the NDJSON
    bridge clients, and copilotacp over stdio and TCP. The profile shares
    the host network, so the parent still reaches a local TCP agent.
- **`acp.CheckRemoteSandbox`** refuses `ProtectedPaths`
  (`ErrRemoteSandboxUnsupported`). A remote or pre-existing endpoint cannot
  be kept from writing them, whatever its policy says.
- **Wrapper ACP path:**
  - `Config.ProtectedPaths` now reaches the launcher. An ACP launch with no
    resolved policy is no longer refused.
  - A required policy is still merged up front, so a conflict is refused
    before anything starts.
  - `ErrProtectedPathsUnsupported` now means only that the platform's
    backend cannot write-protect paths, or that the paths conflict with
    the policy.
- **go-sandbox v0.5.1:** a host-filesystem profile now runs a symlinked
  command at its own path. An ACP agent installed as a symlink, such as an
  npm shim, keeps its argv[0].
- **Tested:**
  - A real ACP agent (`wrapper/testdata/acpfixture`) launched with
    `ProtectedPaths` and no policy tries, at startup, to rewrite a file in a
    protected directory. The write is refused, its own workdir write lands,
    the session comes up, and `sandbox.applied` reports
    `protect-control-plane` applied.
  - This runs for opencode (NDJSON) and copilot over stdio and TCP.
  - With the paths not forwarded, the same test fails because the write
    lands.
  - Unit tests cover the remote refusal and the bwrap argv.

## v0.24.0 — 2026-10-01

`CancelTurn` interrupts Codex app-server and OpenCode serve turns
(CW-20261001-0160). Requires agentkit v0.20.1 and go-providers v0.40.0.

### Changed

- **`Wrapper.CancelTurn` keeps the process on two more native runtimes.**
  It still emits `interrupt.requested` and `interrupt.acknowledged`, and the
  next `SendInput` starts a turn on the same process.
  - **Codex app-server:** it sends `turn/interrupt` for the open turn,
    through agentkit's jsonrpc-stdio session. The wrapper doesn't model Codex
    turns, because the host drives the thread protocol. The host sees the
    turn end as Codex reports it: `turn/completed` with
    `status: "interrupted"`.
  - **OpenCode serve:** it calls `POST /session/{id}/abort`, through agentkit's
    serve-http session. The turn ends with `turn.failed`, reason
    `interrupted`.
  - agentkit v0.20.1 keeps the session's own JSON-RPC request ids clear of
    the ids a host writes through `SendInput`.
- **The prepared-execution adapter forwards `provider.RPCTurnInterrupter`,**
  but only when the adapter it wraps has one.

## v0.23.0 — 2026-10-01

Hosts can write-protect their control-plane directories from the agents the
wrapper launches (CW-20260930-0237). Takes agentkit v0.19.0 (was v0.18.0) and
go-sandbox v0.5.0 (was v0.4.1).

### Security

- **`Config.ProtectedPaths`** lists absolute directories the agent must
  never write: the host's state, database, config, catalog or allow-lists.
  `New` rejects a relative entry.
- **Native and prepared runtime paths:** forwarded to agentkit's
  `StartOptions.ProtectedPaths`.
  - The paths fold into the one sandbox that wraps the child:
    `SandboxPolicy`, `SandboxProfile`, a prepared access policy, or
    otherwise a minimal host-filesystem profile whose only effect is the
    protection.
  - A backend that cannot enforce it, or a provider-native runtime, refuses
    the launch.
- **ACP path:**
  - The paths are merged into the resolved `SandboxPolicy`, or the prepared
    access policy, with go-sandbox `WithProtected`.
  - With no resolved policy, the ACP launch is refused with
    `ErrProtectedPathsUnsupported`, because the ACP launcher has no
    protect-only sandbox yet. The error names CW-20261001-0162, which adds
    one.
- **go-sandbox's rules apply:**
  - directories only, given by their real path, existing before launch;
  - no write grant inside one;
  - protection stops direct writes. Under the minimal host-filesystem
    profile it is not a boundary against writes delegated to same-uid
    services; go-sandbox's README has the boundary statement.
- **Tested:**
  - A wrapper `Run` whose fake agent tries to rewrite an allow-list in a
    registered directory leaves it unchanged, while the agent's own write
    lands. Without the forwarding, the same test shows the allow-list
    rewritten.
  - ACP merge and ACP refusal are tested too.

## v0.22.0 — 2026-10-01

`CancelTurn` interrupts a streaming Claude turn and keeps the process
(CW-20261001-0103). Requires agentkit v0.18.0 and go-providers v0.39.0.

### Changed

- **`Wrapper.CancelTurn` works on a native session whose runtime can
  interrupt a turn.** Today that is streaming-stdio Claude, through
  agentkit's `agentsessions.TurnInterrupter` and Claude's stream-json
  `control_request` interrupt. It was `ErrTurnCancelUnsupported` for every
  native runtime.
  - It emits `interrupt.requested` (reason `turn_cancel`), waits for Claude's
    acknowledgement, then emits `interrupt.acknowledged` with the `turn_id`.
  - The open turn ends with `turn.failed`, `reason: "interrupted"`.
  - The process stays up, and the next `SendInput` runs a turn on it, with
    in-process state intact and no `--resume` cold start.
  - With no turn open, Claude just acknowledges.
  - ACP still sends `session/cancel`. Any other session still returns
    `ErrTurnCancelUnsupported`, and `Stop` ends it.
- **The prepared-execution adapter forwards `provider.TurnInterrupter`**,
  but only when the adapter it wraps has one.

## v0.21.1 — 2026-10-01

### Fixed

- **An ACP agent that exits during launch could panic the host** with "send
  on closed channel" (CW-20261001-0129). When `Client.Launch` or the host
  `Commit` failed, `Manager.Launch` finished the session from its own
  goroutine and closed `Session.Events()` while `drain` could still be
  forwarding a client event to it. With an immediately-exiting copilot
  binary it panicked 1 run in 4. Closing `done` first would not have fixed
  it: once both channels are closed, the send case in drain's select is
  still ready, and select picks between ready cases at random. Now every
  send on `Events()` holds the mutex that its close takes, and skips the
  send once the channel is closed (the pattern the NDJSON and Copilot
  clients already use). A finished session also stops observing: a late
  `turn.completed` can no longer move a closed session back to `ready`.
  `drain` keeps reading and discarding the client's events until the client
  closes them, so a client is never left blocked. `done` still closes last,
  so when `Wait` or `Close` returns, `Events()` and `Diagnostics()` are
  closed and no `OnDiagnostic` callback is running.

## v0.21.0 — 2026-10-01

`Config.PermissionPosture` is the launch's posture, not only the Codex
responder's (CW-20260930-0138, D-72). Requires agentkit v0.17.0 and
go-providers v0.37.0.

### Changed

- **A native launch that is not prepared carries an explicit
  `PermissionPosture`.** The go-providers registry maps the Mode onto the
  runtime's own flags or environment:

  | posture | claude | codex | opencode | agy |
  |---|---|---|---|---|
  | `default` | `--permission-mode default` | `-c sandbox_mode="read-only" -c approval_policy="on-request"` | `OPENCODE_PERMISSION={"edit":"ask","bash":"ask"}` | none |
  | `accept-edits` | `acceptEdits` | `workspace-write`, `on-request` | `{"edit":"allow","bash":"ask"}` | `--mode accept-edits` |
  | `plan` | `plan` | `read-only`, `never` | `{"edit":"deny","bash":"ask"}` | `--mode plan` |
  | `yolo` | `bypassPermissions` | `danger-full-access`, `never` | every permission allowed | `--dangerously-skip-permissions` |

  The flags lead the adapter's `ExtraArgs`, on a copy of the adapter, so they
  sit at the launch convention's extra-argument slot, before `--` and before
  the caller's own `Selection.ExtraArgs`. That needs a go-providers adapter:
  `Run` returns an error for another adapter type, as `launch.Select` does for
  `Binary` and `ExtraArgs`.
- **Prepared launch.** The plan owns the flags (agentkit's
  `Provider.Permission`). The Codex approval responder now answers from
  `PreparedExecution.Posture` when `PermissionPosture` is empty. When both are
  set and differ, `Run` returns `ErrPostureConflict`.

### Behaviour change to check

- **An explicit `PermissionPosture: default` now makes a native Codex launch
  read-only, asking on every write.** Headless, the responder declines those
  requests, so the agent cannot write. This is go-permission's `default`, the
  same thing Claude does headless. To keep the old behaviour (the planted
  `never` / `workspace-write` with the responder in default), leave
  `PermissionPosture` empty, or use `accept-edits` for writes in the
  workspace.

### Unchanged

- **An empty `PermissionPosture` sets no flags and no environment.**
  `TestNativeEmptyPostureMatchesV0_20_0` compares the empty-posture launch
  for claude, codex, opencode and agy against a golden generated from
  v0.20.0. The responder still answers as `default`.
- ACP sessions keep `ACPBestEffortPermissionRequestResponder`.

## v0.20.0 — 2026-10-01

### Added

- **`Config.MCPAllow`**, next to `PermissionPosture` (CW-20261001-0084). It
  narrows which MCP tool calls the `default` and `accept-edits` postures
  approve on the native Codex app-server runtime, through agentkit
  `turn.CodexApprovalResponder.MCPAllow`.
  - **Entries:** `server` or `server/tool`, each half a `path.Match`
    pattern, for example `mux/torque_*`.
  - **Behaviour:** with entries, a call that matches none is declined.
    `plan` and `yolo` ignore the list. With no entries, behaviour is as
    before: every planted server's tool calls are approved.
  - **Validation:** `New` rejects a malformed entry.
- **`agent.permission.resolved` for an MCP tool call** now carries
  `mcp_server` and `mcp_tool`, plus `mcp_allow_entry` when an entry granted
  the call.
- Requires agentkit v0.16.0, which carries the allow-list.

## v0.19.0 — 2026-10-01

### Added

- **ACP sessions carry MCP servers** (CW-20260930-0136). `Config.ACPMCPServers`
  (`[]acp.MCPServer`, also `acp.LaunchParams.MCPServers`) is sent as
  `mcpServers` on `session/new` and `session/load`, where every ACP client
  used to send `[]`. That covers the NDJSON bridge and Copilot's own client.
  An `acp.MCPServer` sets exactly one transport: `URL` with optional
  `Headers` (streamable HTTP), or `Command` with `Args` and `Env` (stdio).
  `acp.SessionMCPServers` renders the ACP SDK's wire shapes: http is
  `{type:"http", name, url, headers:[{name,value}]}` and stdio is
  `{name, command, args, env:[{name,value}]}`. Arrays are always present and
  name/value entries are sorted. An empty or duplicate name, or not exactly
  one transport, fails the launch.
- `acp.InitializeResult.MCPHTTP` reports
  `agentCapabilities.mcpCapabilities.http`. HTTP servers go only to agents
  that advertise it (opencode and Copilot do). For any other agent they are
  dropped, and `OnACPDiagnostic` names them; it never includes header values.
- Known limit: pi-acp advertises `http: false` and does not pass session MCP
  servers to Pi at all, so Pi sessions get none from here, stdio included.
  Native runtimes take MCP servers from the launch plan through agentkit's
  prepared plant (agentkit v0.15.0), not from this field.

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

## v0.17.1 — 2026-10-01

Takes agentkit v0.14.2 (was v0.14.0). It brings two fixes to the wrapper's
native runtimes. In agentkit v0.14.1 (CW-20261001-0086), a child output line
over 1 MiB no longer stops the session reader. In v0.14.2 (CW-20261001-0102),
`ExtraArgs` and Claude's `--add-dir` go before `--` on non-template launches,
not after it, where the agent read them as prompt text.

### Fixed

- **ACP transports no longer lose the child's last frame at exit**
  (CW-20261001-0039).
  - **Symptom:** a session/close reply written just before the child exited
    could be discarded. The pending call then failed with "protocol stream
    closed before response" (`acp.NDJSONBridgeClient`: claudeacp, codexacp,
    opencodeacp, piacp) or "connection closed waiting for session/close
    response" (copilotacp stdio).
  - **Cause:** both read the child's stdout from `cmd.StdoutPipe()` while
    `cmd.Wait()` ran concurrently, and Wait closes that pipe as soon as the
    child exits. It is the same race agentkit fixed in CW-20261001-0046.
  - **Fix:** the transports own the stdout pipe, and the stderr pipe when it
    is read, via `os.Pipe`. After Wait, the reader drains to EOF, bounded by
    one second, before the process exit is reported. The new
    `internal/childoutput` package holds this logic.
  - **Evidence:** the formerly flaky
    `TestBestEffortPermissionResponderMayCallPromptAndCloseAllACPSubprocesses`
    passes at `-race -count=100`. On main it failed 11 of 50 runs: 12
    Close subtests, spread across all five clients.

## v0.17.0 — 2026-10-01

One event vocabulary at the wrapper (CW-20260930-0137 event half; the wrapper
side of CW-20260930-0228 and CW-20260930-0222). Additive payload fields; three
new event kinds.

### Added

- **Block boundaries on `agent.delta`.** `block_id` (stable within one content
  block, different for the next) and `phase` come from go-llm-types'
  `StreamEvent.BlockID` / `Phase`, which go-providers v0.35.0 sets: Claude's
  event uuid, codex exec's `item.id`, opencode's part id. Native thinking
  deltas now carry `phase: "thought"`, the value ACP already emits. Apps
  separate blocks without per-provider guessing.
- **ACP `messageId` becomes `block_id`** in every ACP translator (claude,
  codex, opencode, pi, copilot; copilot's thought chunks use `thoughtId`).
  Copilot deltas gain `phase` (`message` / `thought`) like the others.
- **A normalised `stop_reason` on terminal events**, from the turn's usage
  through `llmtypes.NormalizeStopReason`: `end_turn`, `max_tokens`,
  `tool_use`, `turn_limit`, `refusal`, `cancelled`, `error`, or the
  provider's own word. `turn.failed` carries `error`. ACP's top-level
  `stop_reason` is normalised the same way (`max_turn_requests` →
  `turn_limit`).
- **Cost.** `mergeTurnUsage` sums `Usage.CostUSD` (a per-event delta), so
  each turn's terminal event carries the turn's cost under `usage`.
- **Three new kinds, for every runtime that reports them** (go-runtime-events
  v0.2.0):
  - `session.lost` from `events.SessionLost`
  - `agent.permission_denied` from `events.PermissionDenied`
  - `session.auth_failed` from `events.AuthFailed` (agentkit v0.14.0 emits it
    when the auth classifier matches)

### Changed

- Requires agentkit v0.14.0, go-providers v0.36.0, go-llm-types v0.5.1 and
  go-runtime-events v0.2.1 (was v0.13.0 / v0.34.1 / v0.3.0 / v0.1.2).

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
