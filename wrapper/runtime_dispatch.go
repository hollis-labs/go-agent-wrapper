package wrapper

import (
	"errors"
	"fmt"

	"github.com/hollis-labs/agentkit/agentsessions"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Recognized [adapters.Descriptor.Runtime] tokens. The wrapper maps
// each to the corresponding agentkit/agentsessions [Capabilities]
// lifecycle flag at session start time.
const (
	RuntimePTY            = "pty"
	RuntimeStreamingStdio = "streaming-stdio"
	RuntimeJSONRPCStdio   = "jsonrpc-stdio"
	RuntimeHTTPSSE        = "http-sse"

	// RuntimeAdapter selects the subprocess-per-turn fallback shape
	// (no lifecycle flag in [Capabilities]). The wrapper's empty
	// Descriptor.Runtime token is treated as an alias for this.
	RuntimeAdapter = "adapter"
)

// runtimeCaps maps a wrapper runtime token to the
// agentkit/agentsessions [Capabilities] lifecycle flag.
// Unrecognized tokens return [ErrUnknownRuntime].
func runtimeCaps(runtime string) (agentsessions.Capabilities, error) {
	switch runtime {
	case RuntimePTY:
		return agentsessions.Capabilities{PTY: true, Resize: true, BinaryRequired: true}, nil
	case RuntimeStreamingStdio:
		return agentsessions.Capabilities{StreamingStdio: true, BinaryRequired: true}, nil
	case RuntimeJSONRPCStdio:
		return agentsessions.Capabilities{JsonRpcStdio: true, BinaryRequired: true}, nil
	case RuntimeHTTPSSE:
		return agentsessions.Capabilities{ServeHTTP: true, BinaryRequired: true}, nil
	case "", RuntimeAdapter:
		return agentsessions.Capabilities{BinaryRequired: true}, nil
	default:
		return agentsessions.Capabilities{}, fmt.Errorf("%w: %q", ErrUnknownRuntime, runtime)
	}
}

// runtimeSourceChannel returns the canonical
// [runtimeevents.SourceChannel] for TYPED events (agent.delta,
// agent.tool_use, turn.*) produced by the given wrapper runtime token.
// Used when emitting events that come from the parsed stream so
// consumers can tell which transport's parser observed them.
func runtimeSourceChannel(runtime string) runtimeevents.SourceChannel {
	switch runtime {
	case RuntimePTY:
		return runtimeevents.ChannelPTY
	case RuntimeJSONRPCStdio:
		return runtimeevents.ChannelJSONRPC
	case RuntimeStreamingStdio, RuntimeAdapter, "":
		return runtimeevents.ChannelStdio
	default:
		return runtimeevents.ChannelStdio
	}
}

// rawSourceChannel returns the [runtimeevents.SourceChannel] for RAW
// byte-stream events (stdin.write / stdout.raw / stdout.line /
// stderr.raw / stderr.line). Distinct from [runtimeSourceChannel]
// because raw IO is observed at the pipe layer regardless of how
// the higher-level transport (JSON-RPC, stream-json) parses those
// bytes — PTY runtimes report "pty", everything else reports "stdio".
func rawSourceChannel(runtime string) runtimeevents.SourceChannel {
	if runtime == RuntimePTY {
		return runtimeevents.ChannelPTY
	}
	return runtimeevents.ChannelStdio
}

// ErrUnknownRuntime is wrapped and returned by [Wrapper.Run] when the
// configured adapter declares a [adapters.Descriptor.Runtime] token
// the wrapper does not recognize. Use [errors.Is] to detect.
var ErrUnknownRuntime = errors.New("wrapper: unknown runtime token")
