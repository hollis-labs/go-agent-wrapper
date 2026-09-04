// Package codexacp is the bridge-mediated ACP (Agent Client Protocol,
// agentclientprotocol.com) adapter for OpenAI Codex. Unlike
// [github.com/hollis-labs/go-agent-wrapper/adapters/opencodeacp] and
// [github.com/hollis-labs/go-agent-wrapper/adapters/copilotacp] (both
// thin, direct wire connections to a native ACP subprocess mode), Codex
// has no native ACP mode of its own — OpenAI has an open, unresolved
// feature request (openai/codex#9085) — so this package drives Codex
// through a third-party bridge, [agentclientprotocol/codex-acp]
// (npm package, spawned via `npx`), the pinned candidate task 12's
// operator-approved decision selected. See
// docs/engineering/architecture/17-acp.md in the sibling Nanite repo
// (hollis-labs/apps/nanite) and TASKS/agent-host-acp/12/14 there for the
// full decision record.
//
// This is additive alongside
// [github.com/hollis-labs/go-agent-wrapper/adapters/codex] (Codex's own
// native app-server JSON-RPC protocol, untouched by this package) — both
// stay independently selectable per 17-acp.md's "migration is additive,
// not a cutover" framing.
//
// # Node.js/npm/npx runtime requirement (explicit, reversible — not a
// permanent stack commitment)
//
// [Client.Launch] spawns `npx -y @agentclientprotocol/codex-acp[@version]`
// as a real OS subprocess. This requires a working Node.js/npm/npx
// installation on the machine running go-agent-wrapper — a real runtime
// dependency this package introduces for Codex-via-ACP specifically (NOT
// for the native [adapters/codex] package, NOT for
// [adapters/opencodeacp]/[adapters/copilotacp], which spawn their real
// upstream binaries directly with no bridge). Per the operator's own
// framing recorded in TASKS/ESCALATIONS.md's 2026-08-21 "Task 12
// resolved" entry (Nanite repo): this is an explicit, reversible choice,
// not a permanent one — the [acp.Client] interface this package
// implements already isolates every caller from which concrete
// implementation sits behind it, so swapping to a pure-Go bridge later
// (if one matures, or Codex ever ships real native ACP support) is a new
// acp.Client implementation, not a rearchitecture. Verified present on
// the implementation machine: Node v22.12.0, npm/npx 10.9.0.
//
// # Real wire/spawn behavior (verified directly against the published
// npm package's own source, not assumed from its README)
//
// codex-acp's own README states it "starts the Codex App Server,
// translates ACP requests into Codex operations, and maps Codex events
// back into the client." Decompiling the published v1.6.2 tarball
// (dist/index.js) confirms this literally: `startCodexConnection`
// spawns a REAL `codex app-server` child process via node:child_process
// — either the binary named by the `CODEX_PATH` env var, or, if unset, a
// bundled `@openai/codex` npm dependency (the bridge's own package.json
// pins `@openai/codex: ^0.148.0` — a DIFFERENT version than whatever
// system `codex` CLI happens to be on PATH; on the machine this task was
// verified against, the real system `codex` reported `codex-cli
// 0.147.0`). To avoid the bridge silently driving a different, unpinned
// Codex build than the one [adapters/codex]'s native adapter drives,
// [Client.Launch] explicitly sets `CODEX_PATH` in the spawned bridge
// process's environment: an explicit [WithCodexBinary] override, else the
// supplied launch environment's `CODEX_CLI_PATH`, else a `codex` executable
// found in that environment's `PATH`. The default inherited environment
// therefore drives the same install as the native adapter, while a sanitized
// environment cannot silently import an excluded host path. The bridge's own
// bundled dependency is used only when none of those resolve.
//
// # Real ACP wire behavior (verified live against codex-acp 1.6.2 + the
// real system `codex` CLI 0.147.0, using real ChatGPT-authenticated
// credentials already present on the verifying machine — not a mocked
// probe)
//
// A raw JSON-RPC probe (bypassing this Go package, driving the bridge
// subprocess directly) confirmed the full initialize -> session/new ->
// session/prompt handshake succeeds with an INTEGER protocolVersion (1,
// matching opencode's own bridge — NOT the string a scraped spec summary
// might suggest), and that session authentication is picked up
// automatically from the real codex CLI's own on-disk credentials
// (~/.codex/auth.json) with no separate `authenticate` call needed when
// those credentials already exist. session/update's discriminator field
// is `update.sessionUpdate` (same convention opencodeacp's own live
// verification found) with these observed variants: `agent_message_chunk`
// / `agent_thought_chunk` (both `update.content.type`/`update.content.
// text` — same shape as opencode's for message chunks; the thought-chunk
// shape was confirmed live too, not just from source, via a real
// reasoning-summary delta ("**Checking for boot-prompt file**") during a
// tool-invoking turn), `tool_call` / `tool_call_update` (a REAL,
// source-AND-live-verified DIVERGENCE from opencode's bridge: codex-acp's
// tool-call payloads use `rawInput`/`rawOutput` field names, not
// opencode's `result`/`isError` — confirmed both by decompiling the
// zToolCallUpdate zod schema in dist/index.js and by a real `ls`-tool
// live turn through this package's own [Client] end to end, whose
// tool_call_update correctly decoded a real `rawOutput` payload
// (`{"exit_code":0,"formatted_output":"go.mod\ngo.sum\nmain.go\n"}`) into
// runtimeevents' `result` field), plus informational variants
// (`available_commands_update`, `usage_update`, `session_info_update`)
// skipped the same way opencodeacp skips its own informational variants
// — no clean runtimeevents home, matching go-providers' "informational,
// produces no event" convention. `session/request_permission` was never
// observed live for a plain shell tool call (Codex executed it directly,
// consistent with docs/engineering/architecture/17-acp.md's documented
// expectation that Codex/Claude do their own fs/terminal work regardless
// of declared client capabilities) but IS wired per
// [Client.handleServerRequest] for correctness, using the same "respond
// cancelled, emit requested/resolved for visibility" pattern opencodeacp
// uses.
//
// # Real, verified Interrupt capability
//
// Confirmed BOTH by reading the bridge's own published source and by a
// live mid-generation cancel test — not assumed from either alone.
// Source: the bridge's ACP `session/cancel` handler
// (`interruptSessionTurn`/`interruptPromptTurn` in dist/index.js) calls
// `codexAcpClient.turnInterrupt({threadId, turnId})`, which sends a REAL
// `turn/interrupt` JSON-RPC 2.0 request
// (`CodexAppServerClient.turnInterrupt`: `sendRequest({method:
// "turn/interrupt", params})`) to the spawned `codex app-server`
// subprocess — the same native interrupt mechanism
// docs/engineering/architecture/17-acp.md names as the real one, genuine
// surface Codex's own app-server exposes but which neither
// agentkit/agentsessions nor go-agent-wrapper's own native
// [adapters/codex] currently calls (that adapter's Stop() is
// stdin-close + SIGTERM/SIGKILL only — [adapters.InterruptProcess], see
// its own Describe() doc comment). Live: a raw probe sent a real
// 2000-word-essay prompt, waited for 10 real `agent_message_chunk`
// deltas (proof generation was genuinely underway), then sent
// `session/cancel` — the in-flight `session/prompt` response (carrying
// `stopReason: "cancelled"`) arrived ~12ms later, not after the model
// would have naturally finished. This is a genuine mid-turn abort.
// [Client.InterruptCapability] therefore reports [adapters.InterruptTurn]
// — a real, concrete capability improvement over the native Codex
// adapter's [adapters.InterruptProcess], though per this task's own
// scope note (TASKS/agent-host-acp/14, Nanite repo) this is NOT the
// deciding criterion for choosing this bridge — the actual goal of the
// whole ACP effort is protocol-level uniformity for future
// config-based provider extensibility with an honest per-adapter
// capabilities map, not immediate interrupt gain.
//
// # Wrapper-owned lifecycle
//
// [Client] spawns and owns its `npx ... @agentclientprotocol/codex-acp`
// subprocess directly via os/exec, exactly mirroring
// [adapters/opencodeacp.Client]'s own request/response correlation and
// notification dispatch. [Adapter] exposes a fresh client through
// [acp.ClientAdapter], and wrapper.Wrapper owns its initialize/auth/config,
// create-or-resume, prompt/cancel, liveness, and cleanup through [acp.Manager].
// The provider.CLIAdapter implementation remains compatibility/introspection
// glue; Wrapper selects ClientAdapter for ProtocolACP.
package codexacp
