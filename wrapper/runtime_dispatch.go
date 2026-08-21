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
	case protocol == "" && transport == "":
		// Empty Protocol/Transport: subprocess-per-turn fallback, no
		// agentkit lifecycle flag. Equivalent to the pre-split empty
		// Descriptor.Runtime string.
		return agentsessions.Capabilities{BinaryRequired: true}, nil
	default:
		return agentsessions.Capabilities{}, fmt.Errorf("%w: protocol=%q transport=%q", ErrUnknownRuntime, protocol, transport)
	}
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

// legacyRuntimeToken maps a Protocol+Transport pair to the pre-split
// runtime token string, mirrored into [runtimeevents.Process.Runtime]
// (a field in the separate go-runtime-events package, out of scope
// for this split) so downstream consumers that already parse that
// field's string values keep seeing the same shapes they did before.
// Returns "" for combinations with no legacy equivalent — [Wrapper.Run]
// only calls this after [runtimeCaps] has already validated the pair,
// so in practice every call here hits a known case.
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
