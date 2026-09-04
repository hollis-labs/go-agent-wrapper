package wrapper

import (
	"errors"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestRuntimeCapsMapping(t *testing.T) {
	cases := []struct {
		name      string
		protocol  adapters.Protocol
		transport adapters.Transport
		want      agentsessions.Capabilities
	}{
		{"pty", adapters.ProtocolPTYRaw, adapters.TransportPTY,
			agentsessions.Capabilities{PTY: true, Resize: true, BinaryRequired: true}},
		{"claude stream-json/stdio", adapters.ProtocolClaudeStreamJSON, adapters.TransportStdio,
			agentsessions.Capabilities{StreamingStdio: true, BinaryRequired: true}},
		{"codex app-server/stdio", adapters.ProtocolCodexAppServer, adapters.TransportStdio,
			agentsessions.Capabilities{JsonRpcStdio: true, BinaryRequired: true}},
		{"opencode native/http-sse", adapters.ProtocolOpenCodeNative, adapters.TransportHTTPSSE,
			agentsessions.Capabilities{ServeHTTP: true, BinaryRequired: true}},
		{"acp/stdio", adapters.ProtocolACP, adapters.TransportStdio,
			agentsessions.Capabilities{JsonRpcStdio: true, BinaryRequired: true}},
		{"unset (adapter fallback)", "", "",
			agentsessions.Capabilities{BinaryRequired: true}}, // empty == adapter
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := runtimeCaps(c.protocol, c.transport)
			if err != nil {
				t.Fatalf("runtimeCaps(%q, %q): %v", c.protocol, c.transport, err)
			}
			if got != c.want {
				t.Errorf("runtimeCaps(%q, %q) = %+v\n  want %+v", c.protocol, c.transport, got, c.want)
			}
		})
	}
}

func TestRuntimeCapsUnknownRuntime(t *testing.T) {
	_, err := runtimeCaps(adapters.Protocol("future-protocol"), adapters.Transport("future-transport"))
	if !errors.Is(err, ErrUnknownRuntime) {
		t.Fatalf("err = %v, want errors.Is(err, ErrUnknownRuntime)", err)
	}
}

// TestRuntimeCapsACPTCPUnmapped documents that ACP over TCP (e.g.
// Copilot CLI's `--acp` daemon mode) deliberately has no runtimeCaps
// case yet — agentkit has no TCP-session runtime kind to select, and
// adding one is out of scope for this task's dispatch-table wiring.
// Only ACP+stdio is wired (see TestRuntimeCapsMapping's "acp/stdio"
// case). This test pins the current, honest "not yet supported" state
// so a future task that adds TCP support does so deliberately rather
// than by accidentally falling through an unrelated default case.
func TestRuntimeCapsACPTCPUnmapped(t *testing.T) {
	_, err := runtimeCaps(adapters.ProtocolACP, adapters.TransportTCP)
	if !errors.Is(err, ErrUnknownRuntime) {
		t.Fatalf("err = %v, want errors.Is(err, ErrUnknownRuntime) (ACP+TCP not yet wired)", err)
	}
}

func TestRuntimeCapsLifecycleFlagsMutuallyExclusive(t *testing.T) {
	// Sanity: agentsessions itself rejects multiple lifecycle flags.
	// The wrapper's mapping must produce caps that pass that check.
	combos := []struct {
		protocol  adapters.Protocol
		transport adapters.Transport
	}{
		{adapters.ProtocolPTYRaw, adapters.TransportPTY},
		{adapters.ProtocolClaudeStreamJSON, adapters.TransportStdio},
		{adapters.ProtocolCodexAppServer, adapters.TransportStdio},
		{adapters.ProtocolOpenCodeNative, adapters.TransportHTTPSSE},
		{adapters.ProtocolACP, adapters.TransportStdio},
		{"", ""},
	}
	for _, c := range combos {
		caps, err := runtimeCaps(c.protocol, c.transport)
		if err != nil {
			t.Fatalf("runtimeCaps(%q, %q): %v", c.protocol, c.transport, err)
		}
		n := 0
		if caps.PTY {
			n++
		}
		if caps.StreamingStdio {
			n++
		}
		if caps.JsonRpcStdio {
			n++
		}
		if caps.ServeHTTP {
			n++
		}
		if n > 1 {
			t.Errorf("protocol=%q transport=%q produced caps with %d lifecycle flags; want ≤ 1",
				c.protocol, c.transport, n)
		}
	}
}

func TestRuntimeSourceChannel(t *testing.T) {
	cases := []struct {
		name      string
		protocol  adapters.Protocol
		transport adapters.Transport
		want      runtimeevents.SourceChannel
	}{
		{"pty", adapters.ProtocolPTYRaw, adapters.TransportPTY, runtimeevents.ChannelPTY},
		{"codex jsonrpc", adapters.ProtocolCodexAppServer, adapters.TransportStdio, runtimeevents.ChannelJSONRPC},
		{"acp jsonrpc/stdio", adapters.ProtocolACP, adapters.TransportStdio, runtimeevents.ChannelJSONRPC},
		{"claude streaming-stdio", adapters.ProtocolClaudeStreamJSON, adapters.TransportStdio, runtimeevents.ChannelStdio},
		{"opencode http-sse (no dedicated case, falls to default)", adapters.ProtocolOpenCodeNative, adapters.TransportHTTPSSE, runtimeevents.ChannelStdio},
		{"unset (adapter fallback)", "", "", runtimeevents.ChannelStdio},
		{"unknown future combo", adapters.Protocol("unknown-future"), adapters.Transport("unknown-future"), runtimeevents.ChannelStdio}, // safe default
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runtimeSourceChannel(c.protocol, c.transport); got != c.want {
				t.Errorf("runtimeSourceChannel(%q, %q) = %q, want %q", c.protocol, c.transport, got, c.want)
			}
		})
	}
}

func TestRawSourceChannel(t *testing.T) {
	cases := []struct {
		name      string
		protocol  adapters.Protocol
		transport adapters.Transport
		want      runtimeevents.SourceChannel
	}{
		{"pty", adapters.ProtocolPTYRaw, adapters.TransportPTY, runtimeevents.ChannelPTY},
		{"claude streaming-stdio", adapters.ProtocolClaudeStreamJSON, adapters.TransportStdio, runtimeevents.ChannelStdio},
		{"codex jsonrpc-stdio", adapters.ProtocolCodexAppServer, adapters.TransportStdio, runtimeevents.ChannelStdio},
		{"opencode http-sse", adapters.ProtocolOpenCodeNative, adapters.TransportHTTPSSE, runtimeevents.ChannelStdio},
		{"acp/stdio", adapters.ProtocolACP, adapters.TransportStdio, runtimeevents.ChannelStdio},
		{"unset (adapter fallback)", "", "", runtimeevents.ChannelStdio},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rawSourceChannel(c.protocol, c.transport); got != c.want {
				t.Errorf("rawSourceChannel(%q, %q) = %q, want %q", c.protocol, c.transport, got, c.want)
			}
		})
	}
}

func TestLegacyRuntimeToken(t *testing.T) {
	cases := []struct {
		name      string
		protocol  adapters.Protocol
		transport adapters.Transport
		want      string
	}{
		{"pty", adapters.ProtocolPTYRaw, adapters.TransportPTY, RuntimePTY},
		{"claude streaming-stdio", adapters.ProtocolClaudeStreamJSON, adapters.TransportStdio, RuntimeStreamingStdio},
		{"codex jsonrpc-stdio", adapters.ProtocolCodexAppServer, adapters.TransportStdio, RuntimeJSONRPCStdio},
		{"opencode http-sse", adapters.ProtocolOpenCodeNative, adapters.TransportHTTPSSE, RuntimeHTTPSSE},
		{"acp/stdio", adapters.ProtocolACP, adapters.TransportStdio, RuntimeACPStdio},
		{"acp/tcp", adapters.ProtocolACP, adapters.TransportTCP, RuntimeACPTCP},
		{"unset (adapter fallback)", "", "", RuntimeAdapter},
		{"unrecognized combo", adapters.Protocol("future"), adapters.Transport("future"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := legacyRuntimeToken(c.protocol, c.transport); got != c.want {
				t.Errorf("legacyRuntimeToken(%q, %q) = %q, want %q", c.protocol, c.transport, got, c.want)
			}
		})
	}
}
