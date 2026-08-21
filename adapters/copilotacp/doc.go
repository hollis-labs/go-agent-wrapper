// Package copilotacp implements a native ACP (Agent Client Protocol,
// agentclientprotocol.com) [adapters.Adapter] for GitHub Copilot CLI's
// `--acp` mode — TASKS/agent-host-acp/10 in the sibling Nanite repo
// (hollis-labs/apps/nanite). It is a thin passthrough, not a bridge:
// Copilot CLI speaks real ACP natively, so this package's job is wiring,
// not translation between two different wire protocols.
//
// # Confirmed live, against the real binary (2026-08-21)
//
// `/opt/homebrew/bin/copilot` 1.0.12's `--acp` flag ("Start as Agent
// Client Protocol server") is real and was driven end to end during
// this task's implementation, not assumed from documentation:
//
//   - Stdio transport: `copilot --acp` speaks newline-delimited JSON-RPC
//     2.0 over its own stdin/stdout — confirmed by piping a real
//     `initialize` request in and reading a real, spec-shaped response
//     back (agentCapabilities, agentInfo, authMethods).
//   - TCP transport: `copilot --acp --port <N>` is ALSO real — the
//     process genuinely binds and LISTENs on the given TCP port
//     (confirmed via `lsof`; a second instance on the same port fails
//     with a real `EADDRINUSE`), and a plain TCP socket connection
//     speaks the identical NDJSON JSON-RPC 2.0 protocol. No `--host`
//     flag was found; the port binds on all interfaces (wildcard) by
//     default. There is no `--acp --help` subcommand — `--acp` is a
//     top-level option, not its own command; TCP support is otherwise
//     undocumented in `copilot --help`/GitHub's own docs and was found
//     only by testing the flag directly against the real binary.
//   - Full turn lifecycle over both transports: `initialize` →
//     `session/new` (returns a real `sessionId`) → `session/prompt`
//     (blocks until the turn resolves) with `session/update`
//     notifications (`agent_message_chunk`, `agent_thought_chunk`, ...)
//     streamed in between, ending in the `session/prompt` response
//     carrying `stopReason`.
//   - `session/cancel` (a JSON-RPC *notification*, not a request —
//     confirmed against the spec, not just Copilot's behavior) genuinely
//     interrupts an in-flight turn: a prompt asking for a ~2000-word
//     essay was cut off ~3 seconds after `session/cancel`, with an
//     agent-emitted "Info: Operation cancelled by user" message chunk,
//     rather than running to completion. This is a real mid-turn
//     interrupt, not process-kill-only — see [adapters.InterruptTurn]
//     below.
//
// # Design: the Client owns its own process/connection directly
//
// [Client] (this package) implements [acp.Client] as a genuinely
// self-contained ACP wire client: for [adapters.TransportStdio] it
// spawns `copilot --acp` itself and owns its stdin/stdout pipes; for
// [adapters.TransportTCP] it spawns `copilot --acp --port <N>` (or, via
// [WithDialOnly], simply dials an already-running daemon) and owns the
// TCP connection. Either way it does the full `initialize`/`session/new`
// handshake, request/response correlation, and `session/update`
// notification translation itself, and emits [runtimeevents.Event]
// values directly on [Client.Events] — no [llmtypes.StreamEvent]
// round-trip, no bridge.
//
// This is the composition [Client] is built for, and the one this
// task's required real, non-mocked end-to-end tests exercise for both
// transports (see the package's _test.go files).
//
// # Known limitation: Wrapper.Run's jsonrpc-stdio composition is real but capture-only
//
// [Adapter] also implements [adapters.RuntimeAdapter] — [Adapter.CLIAdapter]
// returns a [provider.CLIAdapter] shim, mirroring the composition shape
// task 08's own fake-test (wrapper/wrapper_acp_test.go, TestRunFakeACPAdapter_JsonRpcStdio)
// demonstrated, so this Adapter is structurally consistent with
// adapters/claude, adapters/codex, and adapters/opencode, and so
// [wrapper.Wrapper.Run] can dispatch it through the [adapters.ProtocolACP]
// + [adapters.TransportStdio] entry task 08 wired into
// wrapper/runtime_dispatch.go.
//
// That composition has one confirmed, real limit, found while
// implementing this task rather than assumed going in:
// [provider.CLIAdapter.BuildArgs] runs before the child process exists
// (agentkit's jsonRpcStdioSession.spawnAttempt calls it to build argv,
// then spawns) and [provider.CLIAdapter.ParseLine] is read-only (agentkit
// owns the child's stdin exclusively once spawned) — neither method gives
// an adapter a writer it can use to drive a real, multi-step JSON-RPC
// handshake (initialize → session/new → session/prompt) the way
// [Client], driven directly, does. [wrapper.Wrapper] itself exposes no
// [agentsessions.JsonRpcCaller]-forwarding surface to a caller either
// (only the raw-bytes [wrapper.Wrapper.SendInput] escape hatch), so there
// is no clean, non-double-spawning way today for an Adapter's own
// Launch/Prompt orchestration to run automatically inside this
// composition.
//
// Given that, [Adapter.CLIAdapter]'s glue is honest about what it does
// and does not do:
//
//   - Detect/BuildArgs return the real `copilot --acp` binary+args, so a
//     caller that does route this through [wrapper.Wrapper.Run] gets the
//     real agent process, not a placeholder — and Wrapper.Run's own
//     process/session lifecycle (PID, Wait, Stop-signal escalation)
//     genuinely applies to it.
//   - ParseLine does real, useful work: it recognizes `session/update`
//     notifications on whatever the child writes back and translates
//     them into [llmtypes.StreamEvent] (the same translation logic
//     [Client] itself uses, shared via translate.go) — so a caller
//     willing to hand-write pre-framed `initialize`/`session/new`/
//     `session/prompt` JSON-RPC request bytes through
//     [wrapper.Wrapper.SendInput] (the documented "raw-bytes escape
//     hatch" every jsonrpc-stdio Session already supports) gets correct
//     event translation back.
//   - What this composition does NOT do is automatically perform the
//     handshake or turn-sending for the caller the way [Client.Launch]/
//     [Client.Prompt] do when driven directly — BuildArgs has no writer
//     to send `initialize` with, so it does not try.
//
// Task 09's sibling adapter (adapters/opencodeacp, landed first, tag
// v0.6.0) independently found the identical seam gap and made a
// narrower choice: its own ParseLine is a pure (nil, nil) pass-through,
// matching adapters/codex's own precedent exactly. This package's
// ParseLine does real translation instead — a deliberate, small
// divergence, not a disagreement: [Client]'s notification-parsing logic
// (translate.go) already has to exist regardless (it's what makes
// [Client.Events] work at all), so exposing it through ParseLine too
// costs nothing extra and gives a [wrapper.Wrapper.SendInput]-driven
// caller real event translation instead of none. Both are honest about
// the same underlying limit: neither ParseLine variant drives the
// handshake/turn-sending automatically.
//
// This is symmetric with the confirmed TCP dispatch-table gap task 08
// itself documented (agentkit has no TCP-session runtime kind at all —
// confirmed directly against github.com/hollis-labs/agentkit's
// agentsessions package, which ships exactly four runtime kinds — PTY,
// streaming-stdio, jsonrpc-stdio, serve-http — none of them TCP-socket
// based): both are genuine, confirmed gaps in go-agent-wrapper/agentkit's
// current Wrapper.Run composition, not something this adapter package can
// paper over from below. A follow-up that gives [wrapper.Wrapper] a
// [agentsessions.JsonRpcCaller]-forwarding surface (for stdio) and gives
// agentkit a real TCP-session runtime kind (for tcp) would let both gaps
// close without changing this package's [Client] at all — [Client] is
// already transport-agnostic and already does the real work; only the
// Wrapper.Run composition path is capture-only today.
//
// # Known limitation: no fs/terminal proxying
//
// Per docs/engineering/architecture/17-acp.md's own flagged unknown
// ("Tool-call/fs/terminal proxying is a per-agent unknown, not a settled
// no"), this package does not implement `fs/read_text_file`,
// `fs/write_text_file`, `terminal/*`, or `session/request_permission`
// servicing. [Client] declares `fs`/`terminal` capabilities as false
// during `initialize` and answers any server-initiated request Copilot
// sends anyway with a JSON-RPC "method not handled" error, rather than
// hanging forever — the same defensive default agentkit's own
// JsonRpcRequestHook uses when unconfigured. A turn that genuinely
// requires client-served fs/terminal access will fail or degrade rather
// than complete; building a real in-process fs/terminal server was
// explicitly out of scope per the architecture doc's own call ("worth
// flagging as a possible hidden cost rather than folding silently into
// the adapter work").
package copilotacp
