package acp

import (
	"context"
	"sync"
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// fakeClient is a no-op Client implementation used to prove the
// interface's own contract is well-formed and swappable — no real ACP
// wire-connection logic (no JSON-RPC framing, no `session/new`/
// `session/prompt` method shapes) appears anywhere in this file. See
// TASKS/agent-host-acp/08 (Nanite repo) "Done means": "verifiable via a
// fake/no-op implementation satisfying the interface".
type fakeClient struct {
	mu         sync.Mutex
	launched   bool
	launchArgs LaunchParams
	prompts    []string
	canceled   bool
	closed     bool
	events     chan runtimeevents.Event
	interrupt  adapters.InterruptCapability
}

func newFakeClient(interrupt adapters.InterruptCapability) *fakeClient {
	return &fakeClient{
		events:    make(chan runtimeevents.Event, 8),
		interrupt: interrupt,
	}
}

func (f *fakeClient) Launch(_ context.Context, params LaunchParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launched = true
	f.launchArgs = params
	f.events <- runtimeevents.Event{Kind: runtimeevents.KindSessionReady}
	return nil
}

func (f *fakeClient) Prompt(_ context.Context, prompt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, prompt)
	f.events <- runtimeevents.Event{Kind: runtimeevents.KindAgentDelta}
	f.events <- runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted}
	return nil
}

func (f *fakeClient) Cancel(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = true
	return nil
}

func (f *fakeClient) Events() <-chan runtimeevents.Event { return f.events }

func (f *fakeClient) InterruptCapability() adapters.InterruptCapability { return f.interrupt }

func (f *fakeClient) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.events)
	return nil
}

var _ Client = (*fakeClient)(nil)

func TestClientContract(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(adapters.InterruptTurn)

	if err := c.Launch(ctx, LaunchParams{Cwd: "/work", SystemPrompt: "be helpful"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !c.launched {
		t.Fatal("Launch did not record launched=true")
	}
	if c.launchArgs.Cwd != "/work" {
		t.Errorf("launchArgs.Cwd = %q, want /work", c.launchArgs.Cwd)
	}

	if err := c.Prompt(ctx, "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(c.prompts) != 1 || c.prompts[0] != "hello" {
		t.Errorf("prompts = %v, want [hello]", c.prompts)
	}

	if err := c.Cancel(ctx); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !c.canceled {
		t.Fatal("Cancel did not record canceled=true")
	}

	if got := c.InterruptCapability(); got != adapters.InterruptTurn {
		t.Errorf("InterruptCapability() = %q, want %q", got, adapters.InterruptTurn)
	}

	// Events emitted by Launch + Prompt above should all be observable
	// on the channel — session.ready, agent.delta, turn.completed.
	var kinds []runtimeevents.EventKind
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for ev := range c.Events() {
		kinds = append(kinds, ev.Kind)
	}
	want := []runtimeevents.EventKind{
		runtimeevents.KindSessionReady,
		runtimeevents.KindAgentDelta,
		runtimeevents.KindTurnCompleted,
	}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i, k := range want {
		if kinds[i] != k {
			t.Errorf("kinds[%d] = %q, want %q", i, kinds[i], k)
		}
	}

	// Close is safe to call more than once.
	if err := c.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestDescriptorFor(t *testing.T) {
	c := newFakeClient(adapters.InterruptTurn)
	desc := DescriptorFor(c, "opencode", adapters.TransportStdio)

	if desc.Provider != "opencode" {
		t.Errorf("Provider = %q, want opencode", desc.Provider)
	}
	if desc.Protocol != adapters.ProtocolACP {
		t.Errorf("Protocol = %q, want %q", desc.Protocol, adapters.ProtocolACP)
	}
	if desc.Transport != adapters.TransportStdio {
		t.Errorf("Transport = %q, want %q", desc.Transport, adapters.TransportStdio)
	}
	if desc.Interrupt != adapters.InterruptTurn {
		t.Errorf("Interrupt = %q, want %q (must mirror Client.InterruptCapability())", desc.Interrupt, adapters.InterruptTurn)
	}
	if len(desc.Channels) != 1 || desc.Channels[0] != runtimeevents.ChannelJSONRPC {
		t.Errorf("Channels = %v, want [jsonrpc]", desc.Channels)
	}
}

func TestDescriptorForTCPTransport(t *testing.T) {
	// Copilot CLI's `--acp` daemon mode: same Protocol, different
	// Transport — proves DescriptorFor doesn't hardcode stdio.
	c := newFakeClient(adapters.InterruptProcess)
	desc := DescriptorFor(c, "copilot", adapters.TransportTCP)

	if desc.Transport != adapters.TransportTCP {
		t.Errorf("Transport = %q, want %q", desc.Transport, adapters.TransportTCP)
	}
	if desc.Interrupt != adapters.InterruptProcess {
		t.Errorf("Interrupt = %q, want %q", desc.Interrupt, adapters.InterruptProcess)
	}
}
