package adapters

import (
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Adapter is the provider-integration contract. One Adapter instance
// represents one CLI agent (Claude Code, Codex app-server, OpenCode,
// custom in-house agent, ...).
type Adapter interface {
	// Name returns a short stable identifier ("claude", "codex",
	// "opencode"). Used in logs, configuration, and policy rule keys.
	Name() string

	// Describe returns the static capability declaration for this
	// adapter — which provider it integrates, which runtime channel
	// the wrapper should use, and which observation channels it
	// supports. This is descriptive metadata; it does not imply a
	// pre-execution control capability.
	Describe() Descriptor

	// Resolve materializes a Spec for one invocation. It receives the
	// ResolveContext describing this session's planted boot dir,
	// per-app env, and configured user settings, and returns the exec
	// shape (Binary, Args, Env, Cwd).
	Resolve(ResolveContext) (Spec, error)
}

// Protocol identifies the shape of the wire format an Adapter's
// underlying process speaks — the framing/parsing rules the wrapper
// (or agentkit) applies to interpret bytes on the wire. Distinct from
// [Transport], which identifies the medium those bytes travel over.
//
// The two axes are independent: the same Protocol can, in principle,
// ride more than one Transport (ACP is the motivating case — stdio
// today, TCP for GitHub Copilot CLI's `--acp` daemon mode), and a
// provider's native protocol (Claude's stream-json, Codex's
// app-server) is orthogonal to ACP entirely, not a value on the same
// enum as ACP. See docs/engineering/architecture/17-acp.md in the
// Nanite repo for the full rationale behind this split.
type Protocol string

// Transport identifies the medium an Adapter's underlying process
// communicates over, independent of [Protocol] — the wire-format
// shape riding on top of it.
type Transport string

const (
	// ProtocolClaudeStreamJSON is Claude Code's native streaming
	// NDJSON-over-stdio wire format.
	ProtocolClaudeStreamJSON Protocol = "claude-stream-json"
	// ProtocolCodexAppServer is Codex's native JSON-RPC 2.0 app-server
	// wire format.
	ProtocolCodexAppServer Protocol = "codex-app-server"
	// ProtocolOpenCodeNative is OpenCode's native HTTP+SSE wire format.
	ProtocolOpenCodeNative Protocol = "opencode-native"
	// ProtocolACP is the Agent Client Protocol
	// (agentclientprotocol.com), JSON-RPC 2.0 over stdio or TCP. No
	// shipped Adapter uses this yet — reserved for the ACP-as-client
	// adapter work (docs/engineering/architecture/17-acp.md in the
	// Nanite repo).
	ProtocolACP Protocol = "acp"
	// ProtocolPTYRaw is a placeholder Protocol for the [TransportPTY]
	// pairing: the PTY transport carries no structured wire protocol
	// at all, just raw terminal bytes, so there is no real protocol
	// name to give it. No shipped Adapter uses this pairing yet — see
	// [TransportPTY]'s doc for why the dispatch path stays wired
	// regardless.
	ProtocolPTYRaw Protocol = "pty-raw"

	// TransportStdio carries bytes over the child process's
	// stdin/stdout pipes, framed per the declared Protocol (NDJSON,
	// JSON-RPC, or raw PTY bytes).
	TransportStdio Transport = "stdio"
	// TransportTCP carries bytes over a TCP socket. No shipped Adapter
	// uses this yet — reserved for TCP-mode ACP agents (e.g. GitHub
	// Copilot CLI's `--acp` daemon mode).
	TransportTCP Transport = "tcp"
	// TransportHTTPSSE carries bytes over HTTP request/response plus a
	// Server-Sent-Events stream for the async half.
	TransportHTTPSSE Transport = "http-sse"
	// TransportPTY carries bytes over an allocated pseudo-terminal. No
	// shipped Adapter uses this yet — wired in
	// wrapper/runtime_dispatch.go's dispatch table (see
	// [ProtocolPTYRaw]) because there is currently no concrete
	// PTY-speaking Adapter to model the pairing on.
	TransportPTY Transport = "pty"
)

// InterruptCapability advertises how much of a genuine mid-turn
// cancel an Adapter's Protocol+Transport combination actually offers
// once dispatched into agentkit/agentsessions. This describes what
// [agentsessions.Session.Stop] does today for each shipped Adapter —
// it does not itself change that behavior. See
// docs/engineering/architecture/17-acp.md in the Nanite repo, "Known
// limitations" section, for the full rationale.
type InterruptCapability string

const (
	// InterruptNone means Stop() has no reliable effect at all.
	// Reserved — no current Adapter is this weak (even kill-only
	// still terminates the process).
	InterruptNone InterruptCapability = "none"
	// InterruptProcess means Stop() sends no wire-level cancel — it
	// closes stdin, waits a grace period, then escalates to
	// SIGTERM/SIGKILL. This is the honest current state of both the
	// Claude and Codex adapters: no interrupt/cancel frame is ever
	// sent over the wire before the process is killed.
	InterruptProcess InterruptCapability = "process"
	// InterruptTurn means Stop() calls a genuine native mid-turn
	// cancel (an HTTP/RPC call, or a wire-level control frame) before
	// falling back to the same kill escalation as InterruptProcess.
	// This is the honest current state of the OpenCode adapter (calls
	// its serve-http `/global/dispose` and `/session/{id}/abort`
	// endpoints before signaling).
	InterruptTurn InterruptCapability = "turn"
	// InterruptSteer is reserved for a future capability tier —
	// mid-turn steering (injecting new instructions without fully
	// cancelling the turn). Not used by any current Adapter or
	// agentsessions backend.
	InterruptSteer InterruptCapability = "steer"
)

// Descriptor is an Adapter's static capability declaration.
type Descriptor struct {
	// Provider is the upstream agent identity ("claude", "codex",
	// "opencode"). Mirrored into [runtimeevents.Process.Provider].
	Provider string

	// Protocol is the wire-format shape this adapter's underlying
	// process speaks. See [Protocol].
	Protocol Protocol

	// Transport is the medium this adapter's underlying process
	// communicates over. See [Transport].
	Transport Transport

	// Interrupt advertises how much of a genuine mid-turn cancel this
	// Protocol+Transport combination actually offers. See
	// [InterruptCapability].
	Interrupt InterruptCapability

	// Channels lists the runtime-event source channels this adapter can
	// produce when the wrapper attaches its observer. Channels describe the
	// provenance and confidence of observations; they do not make advisory
	// recommendations enforceable.
	Channels []runtimeevents.SourceChannel
}

// ResolveContext gives an Adapter everything it needs to produce a Spec
// for one invocation. The wrapper builds this from the session's
// planted boot dir, the caller's Config, and the surrounding environment.
type ResolveContext struct {
	// BootDir is the per-session boot directory the planter populated
	// (or "" if no planting was configured).
	BootDir string

	// Cwd is the working directory the wrapper will run the process in.
	Cwd string

	// Env is the already-materialized Config-derived base environment the
	// wrapper will hand to the process. Adapter.Resolve may return nil to
	// retain it, or derive and return a complete replacement in Spec.Env.
	Env []string

	// PTY indicates whether the wrapper plans to allocate a PTY.
	// Adapters that need to flip CLI flags based on TTY-ness consult
	// this.
	PTY bool

	// AppHints carries per-app configuration the adapter declared
	// interest in (model selection, MCP server list, hook plugin
	// settings, ...). Keys and values are adapter-defined.
	AppHints map[string]string
}

// Spec describes the resolved exec shape for one invocation.
type Spec struct {
	// Binary is the absolute path to the CLI executable.
	Binary string

	// Args are the command-line arguments, not including Binary.
	Args []string

	// Env is the complete environment to pass to the child. Nil means retain
	// ResolveContext.Env unchanged; a non-nil value is a final replacement,
	// including when it is empty.
	Env []string

	// Cwd is the working directory. "" means inherit
	// ResolveContext.Cwd.
	Cwd string
}
