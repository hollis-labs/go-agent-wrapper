package wrapper

import (
	"errors"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestRuntimeCapsMapping(t *testing.T) {
	cases := []struct {
		runtime string
		want    agentsessions.Capabilities
	}{
		{RuntimePTY, agentsessions.Capabilities{PTY: true, Resize: true, BinaryRequired: true}},
		{RuntimeStreamingStdio, agentsessions.Capabilities{StreamingStdio: true, BinaryRequired: true}},
		{RuntimeJSONRPCStdio, agentsessions.Capabilities{JsonRpcStdio: true, BinaryRequired: true}},
		{RuntimeHTTPSSE, agentsessions.Capabilities{ServeHTTP: true, BinaryRequired: true}},
		{RuntimeAdapter, agentsessions.Capabilities{BinaryRequired: true}},
		{"", agentsessions.Capabilities{BinaryRequired: true}}, // empty == adapter
	}
	for _, c := range cases {
		t.Run(c.runtime, func(t *testing.T) {
			got, err := runtimeCaps(c.runtime)
			if err != nil {
				t.Fatalf("runtimeCaps(%q): %v", c.runtime, err)
			}
			if got != c.want {
				t.Errorf("runtimeCaps(%q) = %+v\n  want %+v", c.runtime, got, c.want)
			}
		})
	}
}

func TestRuntimeCapsUnknownRuntime(t *testing.T) {
	_, err := runtimeCaps("future-runtime-we-dont-know")
	if !errors.Is(err, ErrUnknownRuntime) {
		t.Fatalf("err = %v, want errors.Is(err, ErrUnknownRuntime)", err)
	}
}

func TestRuntimeCapsLifecycleFlagsMutuallyExclusive(t *testing.T) {
	// Sanity: agentsessions itself rejects multiple lifecycle flags.
	// The wrapper's mapping must produce caps that pass that check.
	for _, rt := range []string{
		RuntimePTY, RuntimeStreamingStdio, RuntimeJSONRPCStdio, RuntimeHTTPSSE, RuntimeAdapter, "",
	} {
		caps, err := runtimeCaps(rt)
		if err != nil {
			t.Fatalf("runtimeCaps(%q): %v", rt, err)
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
			t.Errorf("runtime %q produced caps with %d lifecycle flags; want ≤ 1", rt, n)
		}
	}
}

func TestRuntimeSourceChannel(t *testing.T) {
	cases := []struct {
		runtime string
		want    runtimeevents.SourceChannel
	}{
		{RuntimePTY, runtimeevents.ChannelPTY},
		{RuntimeJSONRPCStdio, runtimeevents.ChannelJSONRPC},
		{RuntimeStreamingStdio, runtimeevents.ChannelStdio},
		{RuntimeAdapter, runtimeevents.ChannelStdio},
		{"", runtimeevents.ChannelStdio},
		{"unknown-future", runtimeevents.ChannelStdio}, // safe default
	}
	for _, c := range cases {
		t.Run(c.runtime, func(t *testing.T) {
			if got := runtimeSourceChannel(c.runtime); got != c.want {
				t.Errorf("runtimeSourceChannel(%q) = %q, want %q", c.runtime, got, c.want)
			}
		})
	}
}
