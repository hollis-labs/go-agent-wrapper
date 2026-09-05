// Package claudeacp is the bridge-mediated ACP (Agent Client Protocol,
// agentclientprotocol.com) adapter for Claude — Anthropic ships no native
// ACP mode, so this drives Claude through the pinned third-party bridge,
// [agentclientprotocol/claude-agent-acp] (npm), rather than speaking ACP
// directly to a Claude-owned subprocess. See
// docs/engineering/architecture/17-acp.md (Nanite repo,
// hollis-labs/apps/nanite) for the two-role ACP architecture this fits
// into, and TASKS/agent-host-acp/12-pin-acp-bridge-library.md /
// TASKS/ESCALATIONS.md's 2026-08-21 "Task 12 resolved" entry (same repo)
// for the bridge-selection decision this package implements.
//
// This is additive alongside [github.com/hollis-labs/go-agent-wrapper/
// adapters/claude] (Claude's own native stream-json protocol, untouched
// by this package) — both stay independently selectable per
// docs/engineering/architecture/17-acp.md's "migration is additive, not
// a cutover" framing.
//
// # Node.js/npm/npx runtime requirement — explicit, reversible, not a
// stack commitment
//
// [Client.Launch] spawns the bridge via `npx -y
// @agentclientprotocol/claude-agent-acp` by default — a real runtime
// dependency on Node.js/npm/npx wherever this specific adapter is used
// (Claude's *native*, non-ACP adapter and every other provider's ACP
// adapter that isn't bridge-mediated are unaffected). This is a
// deliberate, explicit, and — per the operator's own framing recorded in
// TASKS/ESCALATIONS.md's "Task 12 resolved" entry — reversible choice,
// not a permanent one: [acp.Client]'s interface already isolates every
// caller (this package's own [Adapter], and anything built on top of it)
// from which concrete implementation sits behind it, so swapping to a
// pure-Go bridge later — if beyond5959/acp-adapter matures, a new
// candidate emerges, or Anthropic ever ships a native ACP mode directly —
// is a new [acp.Client] implementation, not a rearchitecture. [Client]'s
// binary resolution (see its own doc comment) lets an operator bypass npx
// entirely once the bridge is installed some other way (e.g. `npm i -g`),
// trading the npx resolve-and-cache cost on every Launch for an explicit
// install step — still the same npm-published TypeScript package
// underneath, just invoked directly.
//
// # Real wire behavior (verified directly against
// @agentclientprotocol/claude-agent-acp 0.70.0, spawned exactly as
// [Client.Launch] spawns it — not assumed from documentation)
//
// Confirmed via two independent methods: (1) a live Node.js JSON-RPC
// probe driving the real bridge subprocess through a full
// initialize → session/new → session/prompt → session/update stream →
// session/prompt response cycle, using real, already-configured Claude
// credentials on the implementation machine (the same auth the `claude`
// CLI itself uses — the bridge wraps `@anthropic-ai/claude-agent-sdk`
// directly, not a spawned `claude` CLI subprocess, except under its own
// separate `--cli` passthrough flag this package does not use); and (2)
// direct inspection of the bridge's real shipped `dist/acp-agent.js`
// source (npm-packed, not a scraped doc) for the wire shapes a live probe
// couldn't easily trigger on demand (e.g. thinking-content chunks).
//
// `initialize` accepts the same integer `protocolVersion: 1` and
// `clientCapabilities: {fs: {readTextFile: false, writeTextFile: false},
// terminal: false}` shape [adapters/opencodeacp] already uses — same ACP
// wire convention, confirmed to round-trip cleanly against Claude's
// bridge too. `session/new` takes `{cwd, mcpServers: []}` and returns a
// real `sessionId`, live-verified. `session/prompt`'s `stopReason` +
// `usage` response shape decodes generically the same way
// [adapters/opencodeacp]'s does (Claude's usage object carries
// `inputTokens`/`outputTokens`/`cachedReadTokens`/`cachedWriteTokens`/
// `totalTokens` — different field names than OpenCode's, but decoded via
// the same untyped-passthrough `usage` payload key, so no adapter-side
// schema is needed for either).
//
// Two concrete divergences from [adapters/opencodeacp]'s own
// empirically-verified shapes were found and are handled explicitly in
// this package's translate.go — ground truth from THIS bridge's real
// wire behavior wins over the other provider's precedent, exactly the
// same "don't assume, re-verify per implementation" discipline task 09's
// own doc comment establishes:
//
//   - `agent_thought_chunk` carries `content: {type: "text", text:
//     "..."}` (the SAME nested shape `agent_message_chunk` uses), not
//     OpenCode's flat `thought` string field. Live-verified via source
//     (acp-agent.js's thinking-delta branch); a live-triggered thinking
//     chunk was not exercised on-demand (model/effort-dependent), so this
//     is source-verified rather than wire-captured, but against the
//     literal bytes of the exact npm-packed version this adapter spawns,
//     not a secondhand spec summary.
//   - `tool_call_update` carries `rawOutput` (not OpenCode's `result`)
//     for the tool's final output, and carries NO boolean `isError`
//     field at all — error state is folded entirely into `status:
//     "failed"` vs. `"completed"`. Live-verified: a real Bash tool call
//     (`echo hello-from-tool-probe`) produced ONE `tool_call`
//     notification followed by FOUR `tool_call_update` notifications —
//     intermediate refinements (streaming input completion, a
//     human-readable title) carrying neither `status` nor `rawOutput` at
//     all, and only the final one carrying both. This package's
//     [Client.handleNotification] therefore emits a
//     [runtimeevents.KindAgentToolResult] event for every `tool_call_update`
//     frame regardless of whether it is the terminal one (matching
//     [adapters/opencodeacp]'s own unconditional-per-frame forwarding
//     policy, and 17-acp.md's plain "tool_call_update →
//     agent.tool_result" mapping) — callers consuming this adapter's
//     activity stream should expect more than one `agent.tool_result`
//     event per real tool call for Claude specifically, most carrying a
//     partial payload (only whichever of title/raw_input/raw_output/
//     status happened to be present on that particular frame) and only
//     the last carrying a `status`.
//
// `session/cancel` is a notification (no `id`, no response), exactly as
// spec'd — confirmed live. `session/request_permission` (a
// server-initiated request) is a standard ACP `{options, sessionId,
// toolCall: {toolCallId, rawInput, ...}}` shape, structurally identical
// to [adapters/opencodeacp]'s own — this package reuses the exact same
// shared best-effort responder handling, with a cancelled default when none is
// configured. Live-verified that Claude executes ordinary tool calls (a
// real Bash command) without ever invoking `session/request_permission`
// or the declared-false `fs`/`terminal` client capabilities — consistent
// with 17-acp.md's documented expectation that Claude does its own
// fs/terminal work internally regardless of what the client declares,
// for at least this one tool-call shape (not exhaustively confirmed for
// every ACP-proxyable operation). The responder therefore cannot replace an
// authoritative host permission gate; the ordinary Bash bypass is a measured
// limitation, not an edge case hidden behind the ACP abstraction.
//
// # Real, verified Interrupt capability
//
// `session/cancel` was source-verified first: the bridge's real
// `dist/acp-agent.js` `cancel()` method calls `await
// session.query.interrupt()` — the Claude Agent SDK's own genuine
// mid-turn interrupt call, per docs/engineering/architecture/17-acp.md's
// own framing of what "a real capability improvement over Nanite's
// current native Claude adapter" would look like (the native, non-ACP
// [adapters/claude] does stdin-close + SIGTERM/SIGKILL only — no
// wire-level interrupt at all) — with an `AbortController`-based
// force-cancel backstop armed alongside it in case the SDK's own
// `interrupt()` doesn't make a wedged query yield in time. Then
// live-verified directly: a real, deliberately long (2000-word-essay)
// prompt was cancelled after observing 3 real `agent_message_chunk`
// deltas; the `session/prompt` response (`{"stopReason":"cancelled",
// "usage":{...all zero...}}`) arrived 7ms after `session/cancel` was
// sent — not after the model would have naturally finished. This is a
// genuine mid-turn abort, not a wire-level acknowledge-and-let-finish.
// [Client.InterruptCapability] therefore reports [adapters.InterruptTurn]
// — matching [adapters/opencodeacp]'s and [adapters/copilotacp]'s own
// verified InterruptTurn value, and a real, concrete improvement over the
// native (non-ACP) Claude adapter's InterruptProcess-only behavior for
// exactly the ACP-bridged path.
//
// Per the operator's own explicit framing recorded in
// TASKS/ESCALATIONS.md's "Task 12 resolved" entry: this real interrupt
// improvement is worth reporting accurately (see above), but is
// deliberately NOT the deciding criterion for this whole ACP effort — the
// actual goal is protocol-level uniformity for future config-based
// provider extensibility with an honest per-adapter capabilities map, not
// chasing interrupt capability for its own sake.
//
// # Wrapper-owned lifecycle
//
// [Client] spawns and owns its bridge subprocess directly via os/exec —
// real request/response correlation (an id-keyed pending map) and real
// notification dispatch, entirely self-contained, mirroring
// [adapters/opencodeacp.Client]'s own structure. [Adapter] exposes a fresh
// client through [acp.ClientAdapter], and wrapper.Wrapper owns that client's
// full handshake, prompt/cancel, liveness, and cleanup through [acp.Manager].
// [Adapter.CLIAdapter] remains compatibility/introspection glue only; Wrapper
// deliberately selects ClientAdapter first for ProtocolACP.
package claudeacp
