// Package piacp is the bridge-mediated ACP (Agent Client Protocol,
// agentclientprotocol.com) adapter for Pi (`earendil-works/pi`, npm
// package `@earendil-works/pi-coding-agent`, binary `pi`) — the third of
// the three non-native ACP providers named in
// docs/engineering/architecture/17-acp.md (Nanite repo), and Pi's FIRST
// appearance as a supported agent anywhere in go-agent-wrapper: unlike
// adapters/claude and adapters/codex, there is no pre-existing native
// Pi adapter in this repo to stay additive alongside.
//
// Pi has no native ACP mode of its own — the implementation tier is a
// third-party bridge library that speaks ACP on Pi's behalf, per
// TASKS/agent-host-acp/12's operator-approved decision (Nanite repo,
// `TASKS/ESCALATIONS.md`'s "2026-08-21 — Task 12 resolved" entry): the
// pinned bridge is **`svkozak/pi-acp`** (npm package `pi-acp`, run via
// `npx -y pi-acp`, no separate install required) — the ACP registry's
// canonical Pi bridge, source-verified at task 12's decision time to
// wire ACP's `session/cancel` through to a real `abort` RPC call against
// the `pi --mode rpc` subprocess it spawns internally. This package
// spawns `pi-acp` as a subprocess (via `npx`) and speaks ACP JSON-RPC
// 2.0 directly over its stdio — structurally, [Client] is a direct wire
// client of the BRIDGE process, not of `pi` itself (which `pi-acp` owns
// internally and this package never touches directly).
//
// # Real, explicit runtime requirement: Node.js/npm/npx
//
// This package requires Node.js (with `npm`/`npx` on PATH) at runtime,
// for exactly this one reason: `pi-acp` is an npm package, and `npx -y
// pi-acp` is how [Client] launches it (see [WithClientBinary] for how to
// point at a pre-installed `pi-acp` binary instead, still Node-based).
// This is a real, deliberate, and — per the operator's own framing at
// task 12's decision — RECOVERABLE choice, not a permanent stack
// commitment: [acp.Client] (this package's implemented interface)
// already isolates every caller from which concrete implementation sits
// behind it, so replacing this package with a pure-Go Pi bridge later
// (if `beyond5959/acp-adapter` matures past its task-12-documented
// "Initial" Pi maturity rating, or a new pure-Go bridge emerges, or Pi
// ships real native ACP support) is a new [acp.Client] implementation,
// not a rearchitecture. The same Node.js/npm/npx requirement applies to
// this repo's sibling bridge-mediated adapters for Claude and Codex
// (TASKS/agent-host-acp/13, /14, Nanite repo) — three specific
// bridge-driven providers, not a project-wide Go-plus-Node dependency.
//
// # Live verification: real `pi`, real bridge, no cloud API key available
//
// `pi` was NOT pre-installed on the machine this package was
// implemented and verified against (`which pi` found nothing). It was
// installed for real: `npm install -g @earendil-works/pi-coding-agent`
// (v0.84.2, real npm package, no vendoring/mocking). `pi-acp` was
// likewise installed for real (`npm install -g pi-acp`, and separately
// exercised via the zero-install `npx -y pi-acp` path this package's
// [Client] actually uses).
//
// None of Pi's cloud/subscription providers had usable credentials in
// this environment — `pi auth check --provider {anthropic,openai,
// google}` all returned real `{"status":"not_ready","reason":
// "credentials_not_configured"}` responses (no ambient
// ANTHROPIC_API_KEY/OPENAI_API_KEY/GEMINI_API_KEY, no prior `/login`).
// Rather than skip live verification for lack of a cloud key, this
// package's live tests and the manual wire-behavior investigation that
// designed [translate.go] were run against a REAL, locally-running LLM
// backend: Ollama (already installed on the verification machine, model
// `llama3.1:8b`), wired into `pi` through its own documented Custom
// Providers mechanism (`~/.pi/agent/models.json`, `api:
// "openai-completions"` pointed at Ollama's own OpenAI-compatible
// `http://localhost:11434/v1` endpoint — see `pi`'s bundled
// `docs/models.md`). This is a real, live, end-to-end ACP session
// against a real running `pi --mode rpc` process (spawned by a real
// `pi-acp` process, spawned by this package's [Client]) — not a mock —
// just backed by a free local model instead of a paid cloud one. Nothing
// about the ACP wire protocol, `pi-acp`'s translation layer, or this
// package's own [Client] depends on which LLM backend `pi` itself is
// configured to call; the wire behavior documented below is a property
// of `pi-acp`/`pi`'s own ACP implementation, independent of model
// choice. Small local models are noticeably less reliable at following
// instructions/invoking tools than the frontier cloud models the other
// two ACP bridge tasks likely verified against — this package's live
// tests are written to tolerate that (see live_test.go) rather than
// assert on exact model output content.
//
// # Real wire behavior (verified directly against pi-acp 0.0.33 + pi
// 0.84.2, not assumed from documentation)
//
// `initialize` → `session/new` → `session/prompt` (streaming
// `session/update` notifications) → a `session/prompt` JSON-RPC response
// carrying `stopReason`, framed as newline-delimited JSON-RPC 2.0 — the
// same shape and the same `protocolVersion: 1` (integer, not string)
// handshake [acp.LaunchParams]'s sibling adapters (opencodeacp,
// copilotacp) already established. `session/update`'s discriminator
// field is `update.sessionUpdate`, exactly like every other ACP
// implementation this repo has verified.
//
// Observed `sessionUpdate` variants:
//   - `agent_message_chunk` (`content.text`) — pi-acp's OWN documented
//     limitation: "Assistant streaming is currently sent as
//     agent_message_chunk (no separate thought stream)" — so
//     `agent_thought_chunk` was never observed live; [translate.go]
//     still maps it defensively (using the same `content.text` shape
//     copilotacp verified live for a different ACP agent) in case a
//     future pi-acp release adds it.
//   - `session_info_update` — a `pi-acp`-specific heartbeat
//     (`_meta.piAcp.{queueDepth,running}`), not in 17-acp.md's mapping
//     list; skipped as informational.
//   - `available_commands_update` — pi's slash-command catalog; skipped
//     as informational, same as every sibling adapter.
//   - `tool_call` / `tool_call_update` — real tool activity, mapped to
//     agent.tool_use / agent.tool_result. Confirmed live for pi's `bash`
//     tool (`kind: "execute"`, `_meta.terminal_info`/`_meta.
//     terminal_output`/`_meta.terminal_exit` carrying real, incremental
//     terminal output and a real exit code) and its `write`/`read` tools
//     (`kind: "edit"`/`"read"`, a `content: [{type: "diff", oldText,
//     newText}]` structured diff on completion). `tool_call_update` may
//     fire many times per tool call (each incremental chunk of terminal
//     output is its own update) — every one is mapped to
//     agent.tool_result, matching this repo's own established
//     opencodeacp precedent of mapping every tool_call_update
//     notification, not just a terminal one.
//   - `user_message_chunk` — observed exactly once, as a REPLAY artifact
//     during a real `session/load` resume (see below), never during a
//     live turn. Skipped as informational.
//   - No `session/request_permission` (or `fs/*`/`terminal/*`) call was
//     ever observed — and per `pi-acp`'s own README this is not merely
//     empirically true for the shapes this package happened to test, it
//     is a DOCUMENTED, permanent design limitation: "No ACP filesystem
//     delegation (fs/*) and no ACP terminal delegation (terminal/*). pi
//     reads/writes and executes locally." [Client] still answers any
//     such request defensively through the shared best-effort responder (or a
//     well-formed ACP "cancelled" default) rather than assuming this
//     permanently, in case a future `pi-acp` release changes it. This seam is
//     not general enforcement while Pi continues to execute locally.
//
// `session/cancel` is a JSON-RPC *notification* (no `id`, no direct
// response) exactly as spec'd — confirmed live.
//
// `session/load` (resume) genuinely works, confirmed via a real
// cross-process resume: session A's turn was completed and that `pi-acp`
// process was killed; a FRESH `pi-acp` process then called `session/load`
// with session A's id and successfully resumed it (pi-acp persists a
// session-id → pi-session-file mapping at `~/.pi/pi-acp/session-map.json`
// independent of any one `pi-acp` process's lifetime — see its own
// README). One real, load-bearing shape difference from opencodeacp's
// `session/load`, found by testing rather than assumed the same:
// **`session/load`'s result does NOT carry a `sessionId` field** (unlike
// `session/new`'s) — pi-acp expects the caller to keep using the SAME id
// it requested resume with, not a re-issued one. [Client.Launch]'s
// resume path is written to match this (see loadSession in client.go).
// A successful `session/load` also REPLAYS the resumed session's prior
// turn as a burst of `session/update` notifications (observed:
// `user_message_chunk`, `tool_call`, `tool_call_update`,
// `agent_message_chunk`) before its own JSON-RPC response arrives — all
// captured with an empty TurnID (Launch has not yet started a Prompt-driven
// turn when these arrive), naturally distinguishing replay activity from
// live in-turn activity without any special-case code.
//
// One more real, non-protocol behavior worth documenting plainly: pi-acp
// emits its own "startup info" block (pi version + loaded skills) as a
// real `agent_message_chunk` on a session's first turn — this is
// legitimate real content pi-acp chose to stream this way (see its own
// README's "(Zed) pi-acp emits 'startup info'... You can disable it by
// setting quietStartup: true in pi settings"), not a translation bug in
// this package; [translate.go] passes it through like any other message
// chunk since there is no ACP-level way to distinguish it from an actual
// model reply.
//
// # Real, verified Interrupt capability: adapters.InterruptTurn
//
// `session/cancel` was tested against a REAL, definitely-still-running
// subprocess pi itself spawned (a `bash` tool call running `for i in
// $(seq 1 30); do echo tick $i; sleep 1; done`, confirmed genuinely
// in-flight via multiple real `terminal_output` ticks arriving over
// several real wall-clock seconds before cancellation) rather than
// relying on a small local model's own token-generation speed (which
// proved too fast/unreliable on its own to make a meaningful timing
// claim). Sending `session/cancel` ~5 seconds into what would have been
// a ~30+ second natural completion produced, within single-digit
// milliseconds: the in-flight `tool_call_update` transitioning to
// `status: "failed"` (the child process was genuinely killed, not merely
// disconnected from), and the `session/prompt` response arriving with
// `stopReason: "cancelled"` — not `"end_turn"`. A follow-up
// `session/prompt` on the SAME sessionId immediately afterward completed
// normally (`stopReason: "end_turn"`), confirming — exactly per
// docs/engineering/GLOSSARY.md's "Turn.Cancel vs. Session.Stop..." entry
// and ACP's own turn-scoped semantics for the (misleadingly-named)
// `session/cancel` method — that the SESSION survives cancellation; only
// the in-flight turn/subprocess is aborted. This is a genuine mid-turn
// abort, not an acknowledge-and-let-finish: [Client.InterruptCapability]
// therefore reports [adapters.InterruptTurn], the same tier this repo's
// sibling native ACP adapters (opencodeacp, copilotacp) independently
// verified for their own respective agents.
//
// Per the operator's own explicit framing at task 12's decision (Nanite
// repo, `TASKS/ESCALATIONS.md`): real interrupt/cancel capability was
// NOT the deciding criterion for building this adapter at all — Nanite
// runs without it today and isn't blocked on it. This finding is
// reported here because the Descriptor this package produces must
// reflect it accurately, not because it was the bar for doing the work.
//
// # Wrapper-owned lifecycle
//
// [Client] spawns and owns its `npx -y pi-acp` subprocess directly via
// os/exec — real request/response correlation, real notification
// dispatch, entirely self-contained. [Adapter] exposes a fresh client through
// [acp.ClientAdapter], and wrapper.Wrapper owns its full initialize/auth/config,
// create-or-resume, prompt/cancel, liveness, and cleanup through [acp.Manager].
// [Adapter.CLIAdapter] remains compatibility/introspection glue only.
//
// # Known limitations
//
//   - No fs/terminal proxying — a real, permanent, documented design
//     choice of `pi-acp` itself (see above), not an unknown to audit
//     further the way 17-acp.md's "per-agent unknown" framing applies to
//     OpenCode/Copilot CLI.
//   - MCP servers accepted in `session/new`'s params but not wired
//     through to `pi` by `pi-acp` (per its own README) — this package
//     always sends an empty `mcpServers` array, matching that reality.
//   - `agent_thought_chunk` mapping is unverified/dead code today (see
//     above) — pi-acp does not currently emit it.
//   - CLIAdapter()'s ParseLine remains a deliberate pass-through; Wrapper uses
//     ClientAdapter for real ACP driving.
package piacp
