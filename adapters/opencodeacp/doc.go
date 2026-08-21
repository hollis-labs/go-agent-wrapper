// Package opencodeacp is the native ACP (Agent Client Protocol,
// agentclientprotocol.com) adapter for OpenCode, driven via its own
// documented `opencode acp` subprocess mode — a thin, direct JSON-RPC
// 2.0-over-stdio wire connection, not a bridge library.
//
// This is additive alongside [github.com/hollis-labs/go-agent-wrapper/
// adapters/opencode] (OpenCode's own native, non-ACP HTTP/SSE protocol,
// untouched by this package) — both stay independently selectable per
// docs/engineering/architecture/17-acp.md's "migration is additive, not
// a cutover" framing (Nanite repo).
//
// # Real wire behavior (verified directly against opencode 1.15.6, not
// assumed from documentation)
//
// `opencode acp --help` confirms the subcommand exists and starts an
// "ACP (Agent Client Protocol) server" over the subprocess's own
// stdin/stdout — no `--port`/`--hostname` args are relevant to that
// mode (those flags exist on the command but govern an unrelated
// optional companion HTTP listener, not the ACP stdio channel itself).
// Framing was confirmed empirically against the live binary (not just
// the spec) to be newline-delimited JSON-RPC 2.0, one message per line,
// matching agentclientprotocol.com's "Messages are delimited by
// newlines... MUST NOT contain embedded newlines" and matching
// agentkit's own bufio.Scanner-based jsonrpc-stdio reader assumption.
//
// A live handshake was captured directly (see this package's Work Log
// entry in TASKS/agent-host-acp/09, Nanite repo, for the full
// transcript): `initialize` → `session/new` → `session/prompt`
// (streaming `session/update` notifications, keyed on the response's
// own `update.sessionUpdate` string discriminator — NOT the `type`
// field a third-party spec summary suggested; ground truth from the
// real binary won out over the scraped doc, per this task's own
// re-verification mandate) → a `session/prompt` JSON-RPC response
// carrying `stopReason`. Observed `sessionUpdate` variants:
// `available_commands_update`, `agent_message_chunk` (message content),
// `agent_thought_chunk` (reasoning/thinking content), `tool_call` /
// `tool_call_update` (tool invocation lifecycle), `usage_update`.
// `session/cancel` was confirmed to be a notification (no `id`, no
// response) exactly as spec'd. `session/load` (session resume) requires
// `sessionId`, `cwd`, AND `mcpServers` (the last is undocumented by the
// scraped spec summary but rejected as a required field by the real
// binary) — confirmed by a live round-trip.
//
// # Real, verified Interrupt capability
//
// `session/cancel` was tested mid-generation against a real, long
// (2000-word-essay) prompt: the in-flight `session/prompt` response
// (carrying the turn's `stopReason`) arrived ~40ms after `session/cancel`
// was sent, with generation only three short message chunks in — not
// after the model naturally finished. This is a genuine mid-turn abort,
// not a wire-level acknowledge-and-let-finish. [Client.InterruptCapability]
// therefore reports [adapters.InterruptTurn], matching the native
// (non-ACP) OpenCode adapter's own verified Stop() behavior
// (adapters/opencode's `/global/dispose` + `/session/{id}/abort`) —
// both wire shapes reach the same underlying OpenCode abort mechanism.
//
// # Client ownership of the subprocess (a documented seam gap, not an
// oversight)
//
// [Client] spawns and owns its `opencode acp` subprocess directly via
// os/exec — real request/response correlation (an id-keyed pending map)
// and real notification dispatch, entirely self-contained. This is
// deliberate, not incidental: [acp.LaunchParams] carries Cwd/Env (spawn
// ingredients, not "attach to an existing connection" parameters), and
// go-providers' provider.CLIAdapter interface (Detect/BuildArgs/
// ParseLine) — the only seam [Adapter.CLIAdapter] can return through —
// has no hook that hands a caller access to the process's real stdin
// once agentkit spawns it, and no hook to make an outbound,
// response-correlated call. [cliAdapter] (this package's
// [adapters.RuntimeAdapter] glue) therefore follows the SAME shape
// go-agent-wrapper's own shipped Codex app-server adapter
// (adapters/codex, provider.NewCodexAdapterAppServer) already uses for
// this exact situation: real Detect/BuildArgs (so a Wrapper.Run() caller
// spawns the one real process, not a duplicate), ParseLine as a pure
// pass-through returning (nil, nil) — "JSON-RPC framing, request
// correlation, and event mapping live in the consumer runtime" per that
// adapter's own doc comment, not yet wired through wrapper.Wrapper for
// EITHER provider. Real, live-verified ACP driving in this task goes
// through [Client] directly, not through wrapper.Wrapper.Run() — see
// this package's Work Log entry (TASKS/agent-host-acp/09, Nanite repo)
// for the full finding and its scope rationale.
package opencodeacp
