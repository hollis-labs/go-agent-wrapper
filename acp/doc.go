// Package acp defines the ACP (Agent Client Protocol,
// agentclientprotocol.com) client abstraction: the "intent" tier a
// Hollis host process (go-agent-wrapper's wrapper.Wrapper, via an
// adapters.Adapter implementing ClientAdapter) drives an underlying
// ACP-speaking agent through, independent of which concrete
// implementation sits underneath it.
//
// This is the client ROLE of ACP — a host driving someone else's agent
// over ACP instead of that agent's bespoke wire format — not the server
// role (an ACP-speaking editor/GUI treating a Hollis session as "the
// agent," already shipped elsewhere as prior art via Tether's `mux
// acp`/`internal/acpadapter`, untouched by this package). See
// docs/engineering/architecture/17-acp.md in the sibling Nanite repo
// (hollis-labs/apps/nanite) for the full architecture.
//
// Manager owns the complete lifecycle: duplicate-safe registration,
// initialize/authenticate, create-or-resume, session configuration, prompting,
// turn cancellation, close, liveness snapshots, normalized terminal outcomes,
// and automatic unregister/cleanup. Wrapper delegates ACP adapters to this
// path directly for both stdio and TCP; it does not rely on agentkit's generic
// capture-only JSON-RPC runtime.
//
// Two kinds of concrete Client implementation sit below this package's Client
// interface:
//
//   - Native ACP agents (OpenCode via `opencode acp`, Copilot CLI via
//     `--acp`) — a thin, direct ACP wire connection to the real
//     subprocess. No bridging.
//   - Non-native agents (Claude, Codex, Pi) — a third-party bridge
//     library speaking ACP on the CLI's behalf (e.g.
//     beyond5959/acp-adapter, agentclientprotocol/claude-agent-acp,
//     codex-acp). go-agent-wrapper consumes such a library as a
//     dependency; it does not hand-roll ACP-bridging logic itself.
//
// Client.Events emits go-runtime-events runtimeevents.Event values —
// the same event vocabulary every other adapter's turn activity uses
// (agent.delta, agent.tool_use, agent.tool_result,
// agent.permission_requested/resolved, turn.completed/failed) — so an
// ACP-driving implementation feeds the existing activity bridge
// (wrapper/event_translator.go's (kind, payload) pattern) rather than
// inventing a parallel event shape.
//
// LaunchParams.BestEffortPermissionRequestResponder is the shared optional
// answer path for agent-initiated session/request_permission calls. It selects
// only exact option IDs offered by the agent, fails closed on invalid input or
// callback failure, and is cancelled with the active turn/session. Shipped
// clients invoke it away from their protocol readers and lifecycle locks. Its
// name is intentional: ACP does not require an agent to request permission for
// every operation, and measured Claude, Codex, OpenCode, and Pi tool paths can
// execute internally without asking. Hosts must retain authoritative controls.
package acp
