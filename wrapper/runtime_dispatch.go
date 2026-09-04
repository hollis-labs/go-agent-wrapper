package wrapper

import (
	"errors"
	"fmt"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Legacy runtime tokens, predating the [adapters.Descriptor]
// Protocol/Transport split. [legacyRuntimeToken] maps a Protocol+
// Transport pair back to these strings so the value mirrored into
// [runtimeevents.Process.Runtime] — a plain string field in the
// separate go-runtime-events package, out of scope for this split —
// stays exactly what it was before the split, for any downstream
// consumer that already parses it.
const (
	RuntimePTY            = "pty"
	RuntimeStreamingStdio = "streaming-stdio"
	RuntimeJSONRPCStdio   = "jsonrpc-stdio"
	RuntimeHTTPSSE        = "http-sse"

	// RuntimeAdapter is the legacy token for the subprocess-per-turn
	// fallback shape (no lifecycle flag in [agentsessions.Capabilities]).
	// A Descriptor that leaves Protocol and Transport at their zero
	// values selects this shape — matching the pre-split behavior
	// where an empty Descriptor.Runtime string aliased this token.
	RuntimeAdapter = "adapter"

	// RuntimeACPStdio is the runtime token for the
	// [adapters.ProtocolACP] + [adapters.TransportStdio] pairing.
	// Unlike the other Runtime* tokens above, this one has no legacy
	// predecessor — ACP is new as of the Protocol/Transport split
	// (task 02 in TASKS/agent-host-acp, the Nanite repo), reserved but
	// unmapped until this task (08) gave it a dispatch entry. It exists
	// purely as a stable [runtimeevents.Process.Runtime] string for
	// ACP-driven adapters, not as backward-compat scaffolding.
	RuntimeACPStdio = "acp-stdio"

	// RuntimeACPTCP is the stable runtime token for a wrapper-owned ACP
	// client connected over TCP (currently Copilot CLI's daemon mode).
	RuntimeACPTCP = "acp-tcp"
)

// runtimeCaps maps an adapter-declared Protocol+Transport pair to the
// agentkit/agentsessions [agentsessions.Capabilities] lifecycle flag.
// Unrecognized combinations return [ErrUnknownRuntime].
func runtimeCaps(protocol adapters.Protocol, transport adapters.Transport) (agentsessions.Capabilities, error) {
	switch {
	case protocol == adapters.ProtocolPTYRaw && transport == adapters.TransportPTY:
		return agentsessions.Capabilities{PTY: true, Resize: true, BinaryRequired: true}, nil
	case protocol == adapters.ProtocolClaudeStreamJSON && transport == adapters.TransportStdio:
		return agentsessions.Capabilities{StreamingStdio: true, BinaryRequired: true}, nil
	case protocol == adapters.ProtocolCodexAppServer && transport == adapters.TransportStdio:
		return agentsessions.Capabilities{JsonRpcStdio: true, BinaryRequired: true}, nil
	case protocol == adapters.ProtocolOpenCodeNative && transport == adapters.TransportHTTPSSE:
		return agentsessions.Capabilities{ServeHTTP: true, BinaryRequired: true}, nil
	case protocol == adapters.ProtocolACP && transport == adapters.TransportStdio:
		// ACP is JSON-RPC 2.0 over stdio (agentclientprotocol.com) —
		// the same wire framing agentkit's JsonRpcStdio runtime already
		// speaks generically for Codex's app-server. This mapping is
		// framing-level only: it says nothing about ACP's own method
		// vocabulary (`initialize`/`session/new`/`session/prompt`/
		// `session/cancel`/`session/update`), which a concrete ACP
		// adapter's [adapters.RuntimeAdapter.CLIAdapter] implementation
		// supplies (TASKS/agent-host-acp/09, 10, Phase 4 — the Nanite
		// repo). TCP-transport ACP (e.g. Copilot CLI's `--acp` daemon
		// mode) has no case here yet — agentkit has no TCP-session
		// runtime kind to select, and adding one is out of scope for
		// this dispatch-table wiring task; falls to the default
		// [ErrUnknownRuntime] case below until a concrete TCP-based ACP
		// adapter needs it.
		return agentsessions.Capabilities{JsonRpcStdio: true, BinaryRequired: true}, nil
	case protocol == "" && transport == "":
		// Empty Protocol/Transport: subprocess-per-turn fallback, no
		// agentkit lifecycle flag. Equivalent to the pre-split empty
		// Descriptor.Runtime string.
		return agentsessions.Capabilities{BinaryRequired: true}, nil
	default:
		return agentsessions.Capabilities{}, fmt.Errorf("%w: protocol=%q transport=%q", ErrUnknownRuntime, protocol, transport)
	}
}

func capsUsesLongLivedProcess(caps agentsessions.Capabilities) bool {
	return caps.PTY || caps.StreamingStdio || caps.JsonRpcStdio || caps.ServeHTTP
}

// runtimeSourceChannel returns the canonical
// [runtimeevents.SourceChannel] for TYPED events (agent.delta,
// agent.tool_use, turn.*) produced by the given adapter-declared
// Protocol+Transport pair. Used when emitting events that come from
// the parsed stream so consumers can tell which parser observed them.
//
// Unrecognized combinations fall back to [runtimeevents.ChannelStdio]
// — the same safe default the pre-split single-string switch used for
// any token without a dedicated case (including "http-sse", which
// never had one).
func runtimeSourceChannel(protocol adapters.Protocol, transport adapters.Transport) runtimeevents.SourceChannel {
	switch {
	case protocol == adapters.ProtocolPTYRaw && transport == adapters.TransportPTY:
		return runtimeevents.ChannelPTY
	case protocol == adapters.ProtocolCodexAppServer && transport == adapters.TransportStdio:
		return runtimeevents.ChannelJSONRPC
	case protocol == adapters.ProtocolACP && transport == adapters.TransportStdio:
		return runtimeevents.ChannelJSONRPC
	default:
		return runtimeevents.ChannelStdio
	}
}

// rawSourceChannel returns the [runtimeevents.SourceChannel] for RAW
// byte-stream events (stdin.write / stdout.raw / stdout.line /
// stderr.raw / stderr.line). Distinct from [runtimeSourceChannel]
// because raw IO is observed at the pipe layer regardless of how the
// higher-level Protocol (JSON-RPC, stream-json) parses those bytes —
// keyed on Transport alone (PTY reports "pty", every other transport
// reports "stdio"), matching the pre-split behavior where only the
// "pty" runtime token got a dedicated case.
func rawSourceChannel(protocol adapters.Protocol, transport adapters.Transport) runtimeevents.SourceChannel {
	if transport == adapters.TransportPTY {
		return runtimeevents.ChannelPTY
	}
	return runtimeevents.ChannelStdio
}

// legacyRuntimeToken maps a Protocol+Transport pair to a stable
// [runtimeevents.Process.Runtime] token string (a field in the
// separate go-runtime-events package, out of scope for the task 02
// split). For the four pairs that predate the split, the token is the
// literal pre-split runtime string, so downstream consumers that
// already parse that field's values keep seeing the same shapes they
// did before. [RuntimeACPStdio] is the one exception — ACP has no
// pre-split predecessor (it did not exist as a Descriptor.Runtime
// value at all) — its token exists purely as a stable string, not as
// backward-compat scaffolding. Returns "" for combinations with no
// token at all — [Wrapper.Run] only calls this after [runtimeCaps] has
// already validated the pair, so in practice every call here hits a
// known case.
func legacyRuntimeToken(protocol adapters.Protocol, transport adapters.Transport) string {
	switch {
	case protocol == adapters.ProtocolPTYRaw && transport == adapters.TransportPTY:
		return RuntimePTY
	case protocol == adapters.ProtocolClaudeStreamJSON && transport == adapters.TransportStdio:
		return RuntimeStreamingStdio
	case protocol == adapters.ProtocolCodexAppServer && transport == adapters.TransportStdio:
		return RuntimeJSONRPCStdio
	case protocol == adapters.ProtocolOpenCodeNative && transport == adapters.TransportHTTPSSE:
		return RuntimeHTTPSSE
	case protocol == adapters.ProtocolACP && transport == adapters.TransportStdio:
		return RuntimeACPStdio
	case protocol == adapters.ProtocolACP && transport == adapters.TransportTCP:
		return RuntimeACPTCP
	case protocol == "" && transport == "":
		return RuntimeAdapter
	default:
		return ""
	}
}

// ErrUnknownRuntime is wrapped and returned by [Wrapper.Run] when the
// configured adapter declares an [adapters.Descriptor] Protocol+
// Transport pair the wrapper does not recognize. Use [errors.Is] to
// detect.
var ErrUnknownRuntime = errors.New("wrapper: unknown runtime token")
