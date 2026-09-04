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
// # Wrapper-owned lifecycle
//
// [Adapter] exposes a fresh protocol client through [acp.ClientAdapter].
// [wrapper.Wrapper.Run] selects that path for ProtocolACP and owns the full
// initialize/auth/config, create-or-resume, prompt/cancel, liveness, and
// cleanup lifecycle through [acp.Manager]. This works for both stdio and TCP;
// it does not depend on agentkit having a TCP runtime kind. The older
// [provider.CLIAdapter] remains available for compatibility and parsing reuse,
// but is not the execution path Wrapper chooses for ACP.
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
