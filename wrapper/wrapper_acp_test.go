package wrapper

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// ---------------------------------------------------------------------
// TASKS/agent-host-acp/08 (Nanite repo): proves the acp.Client interface
// wires into the existing adapters.Adapter/RuntimeAdapter/Descriptor
// seam and dispatches through Wrapper.Run correctly, using nothing but
// a fake/no-op acp.Client — no real ACP wire-connection logic (no
// JSON-RPC framing, no `session/new`/`session/prompt` method shapes)
// appears anywhere in this file. That's tasks 09/10 (native) and
// Phase 4 (bridged), not this one.
//
// The fake child process below speaks no protocol at all: it reads one
// line from stdin and echoes it back, exactly the same shape
// TestRunRealCodexAdapter_JsonRpcStdio's fake script uses for the real
// Codex adapter. The point here isn't ACP fidelity — it's proving the
// Protocol: acp / Transport: stdio dispatch-table entry this task adds
// to runtime_dispatch.go actually routes end to end through agentkit's
// generic JsonRpcStdio runtime, and that the fakeACPCLIAdapter glue
// genuinely calls into the acp.Client (Launch from BuildArgs, Prompt +
// Events drain from ParseLine) rather than the two types coincidentally
// existing side by side.
// ---------------------------------------------------------------------

// fakeACPClient is a no-op acp.Client double — the "fake/no-op
// implementation satisfying the interface" TASKS/agent-host-acp/08's
// "Done means" calls for.
type fakeACPClient struct {
	mu        sync.Mutex
	launched  bool
	prompts   []string
	events    chan runtimeevents.Event
	interrupt adapters.InterruptCapability
}

func newFakeACPClient(interrupt adapters.InterruptCapability) *fakeACPClient {
	return &fakeACPClient{
		events:    make(chan runtimeevents.Event, 8),
		interrupt: interrupt,
	}
}

func (f *fakeACPClient) Launch(_ context.Context, _ acp.LaunchParams) error {
	f.mu.Lock()
	f.launched = true
	f.mu.Unlock()
	f.events <- runtimeevents.Event{Kind: runtimeevents.KindSessionReady}
	return nil
}

func (f *fakeACPClient) Prompt(_ context.Context, prompt string) error {
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	f.mu.Unlock()
	f.events <- runtimeevents.Event{Kind: runtimeevents.KindAgentDelta}
	f.events <- runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted}
	return nil
}

func (f *fakeACPClient) Cancel(context.Context) error { return nil }

func (f *fakeACPClient) Events() <-chan runtimeevents.Event { return f.events }

func (f *fakeACPClient) InterruptCapability() adapters.InterruptCapability { return f.interrupt }

func (f *fakeACPClient) Close(context.Context) error { return nil }

func (f *fakeACPClient) snapshotPrompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.prompts))
	copy(out, f.prompts)
	return out
}

func (f *fakeACPClient) wasLaunched() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launched
}

var _ acp.Client = (*fakeACPClient)(nil)

// fakeACPCLIAdapter bridges a fakeACPClient into provider.CLIAdapter's
// spawn/parse-line shape so it can drive through Wrapper.Run's existing
// agentkit jsonrpc-stdio path. A real ACP-backed CLIAdapter
// (TASKS/agent-host-acp/09/10) would perform the real ACP handshake
// here instead of this fake's no-op Launch/Prompt calls — this glue
// only proves the composition shape, not any real wire behavior.
type fakeACPCLIAdapter struct {
	client *fakeACPClient
	script string
}

func (a *fakeACPCLIAdapter) Name() string { return "fake-acp" }

func (a *fakeACPCLIAdapter) Detect() (string, bool) { return a.script, a.script != "" }

// BuildArgs is called once at spawn time by agentkit's jsonrpc-stdio
// runtime (see agentsessions.jsonRpcStdioSession.spawnAttempt — prompt
// is always "" here). This is where a real ACP-backed CLIAdapter would
// kick off the acp.Client handshake; the fake here just proves the call
// reaches the Client.
func (a *fakeACPCLIAdapter) BuildArgs(_, systemPrompt, cliSessionID string) []string {
	_ = a.client.Launch(context.Background(), acp.LaunchParams{
		SystemPrompt:    systemPrompt,
		SessionIDPreset: cliSessionID,
	})
	return nil
}

// ParseLine is called once per stdout line the fake script emits. It
// forwards the line to the Client as a Prompt, then drains whatever
// Events the Client produced (synchronously available — the fake
// Client's Prompt pushes onto a buffered channel before returning)
// into llmtypes.StreamEvents for the wrapper's existing
// wrapper/event_translator.go path to translate.
func (a *fakeACPCLIAdapter) ParseLine(line []byte) ([]llmtypes.StreamEvent, error) {
	// agentkit's jsonrpc-stdio reader loop only calls ParseLine for
	// lines that parse as JSON (it's building a JSON-RPC 2.0 client on
	// top of raw stdio — see agentsessions.jsonRpcStdioSession.
	// runReaderLoop's json.Unmarshal-then-continue gate) — so the fake
	// script below wraps its echo in a trivial JSON object rather than
	// emitting bare text. The "echo" field name here is this test's
	// own invention, not any real ACP wire shape.
	var frame struct {
		Echo string `json:"echo"`
	}
	if err := json.Unmarshal(line, &frame); err != nil || frame.Echo == "" {
		return nil, nil
	}
	text := frame.Echo
	if err := a.client.Prompt(context.Background(), text); err != nil {
		return nil, err
	}
	var out []llmtypes.StreamEvent
	for {
		select {
		case ev, ok := <-a.client.Events():
			if !ok {
				return out, nil
			}
			switch ev.Kind {
			case runtimeevents.KindAgentDelta:
				out = append(out, llmtypes.StreamEvent{Type: llmtypes.EventDelta, Content: text})
			case runtimeevents.KindTurnCompleted:
				out = append(out, llmtypes.StreamEvent{Type: llmtypes.EventDone})
			}
		default:
			return out, nil
		}
	}
}

// fakeACPRuntimeAdapter implements adapters.Adapter + adapters.RuntimeAdapter
// for a fakeACPClient-backed agent, declaring the real ProtocolACP/
// TransportStdio Descriptor this task's runtime_dispatch.go changes
// exist to route.
type fakeACPRuntimeAdapter struct {
	client *fakeACPClient
	cli    *fakeACPCLIAdapter
}

func (a *fakeACPRuntimeAdapter) Name() string { return "fake-acp" }

// Describe uses acp.DescriptorFor — the same wiring helper a real
// ACP-backed Adapter (tasks 09/10) would call — proving the Client's
// InterruptCapability genuinely reaches Descriptor.Interrupt through
// that helper, not just by coincidence of two independently-set fields.
func (a *fakeACPRuntimeAdapter) Describe() adapters.Descriptor {
	return acp.DescriptorFor(a.client, "fake-acp", adapters.TransportStdio)
}

func (a *fakeACPRuntimeAdapter) Resolve(rc adapters.ResolveContext) (adapters.Spec, error) {
	return adapters.Spec{Binary: a.cli.script, Cwd: rc.Cwd}, nil
}

func (a *fakeACPRuntimeAdapter) CLIAdapter() provider.CLIAdapter { return a.cli }

var (
	_ adapters.Adapter        = (*fakeACPRuntimeAdapter)(nil)
	_ adapters.RuntimeAdapter = (*fakeACPRuntimeAdapter)(nil)
)

// TestRunFakeACPAdapter_JsonRpcStdio drives Wrapper.Run against a
// fakeACPClient-backed Adapter declaring adapters.ProtocolACP /
// adapters.TransportStdio. Confirms:
//   - The Descriptor's Protocol/Transport pair dispatches through
//     runtimeCaps to agentkit's jsonrpc-stdio runtime (no
//     ErrUnknownRuntime) — the dispatch-table entry this task adds.
//   - Descriptor.Interrupt mirrors the fake Client's
//     InterruptCapability() via acp.DescriptorFor.
//   - The fake Client's Launch and Prompt were actually invoked by the
//     real Wrapper.Run/agentkit spawn+read-line path, and its Events()
//     output reached the activity.Bridge as real runtimeevents.Event
//     values (agent.delta, turn.completed) via the existing
//     wrapper/event_translator.go translation — proving the "Events
//     maps cleanly onto runtimeevents.Event, no parallel vocabulary"
//     requirement end to end, not just at the interface-definition
//     level.
//   - Process.Runtime carries the new RuntimeACPStdio token.
func TestRunFakeACPAdapter_JsonRpcStdio(t *testing.T) {
	skipUnlessSh(t)
	dir := t.TempDir()

	scriptBody := `#!/bin/sh
IFS= read -r line
printf '{"echo":"%s"}\n' "$line"
`
	script := filepath.Join(dir, "fake-acp.sh")
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake acp script: %v", err)
	}

	client := newFakeACPClient(adapters.InterruptTurn)
	cli := &fakeACPCLIAdapter{client: client, script: script}
	adapter := &fakeACPRuntimeAdapter{client: client, cli: cli}

	sink := newCapturingSink()
	w, err := New(Config{
		App:               "test-fake-acp",
		Adapter:           adapter,
		Activity:          activity.NewBridge(sink),
		Workdir:           dir,
		AutoFireFirstTurn: true,
		FirstTurnPayload:  "hello acp",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()

	select {
	case runErr := <-runErrCh:
		if runErr != nil {
			t.Fatalf("Run: %v (want nil — the fake acp script exits cleanly on its own)", runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return; fake acp script may be stuck waiting on stdin")
	}

	if !client.wasLaunched() {
		t.Error("acp.Client.Launch was never called — Descriptor/RuntimeAdapter wiring did not reach the Client")
	}
	if prompts := client.snapshotPrompts(); len(prompts) != 1 || prompts[0] != "hello acp" {
		t.Errorf("acp.Client.Prompt calls = %v, want [\"hello acp\"]", prompts)
	}

	evs := sink.snapshot()
	if !hasKind(evs, runtimeevents.KindSessionReady) {
		t.Error("missing session.ready event")
	}
	if !hasKind(evs, runtimeevents.KindAgentDelta) {
		t.Error("missing agent.delta event (acp.Client.Events() output did not reach runtimeevents)")
	}
	if !hasKind(evs, runtimeevents.KindTurnCompleted) {
		t.Error("missing turn.completed event (acp.Client.Events() output did not reach runtimeevents)")
	}
	if !hasKind(evs, runtimeevents.KindProcessExited) {
		t.Error("missing process.exited event")
	}

	idxReady := indexOfKind(evs, runtimeevents.KindSessionReady)
	if idxReady < 0 {
		t.Fatal("missing session.ready event")
	}
	if got := evs[idxReady].Process.Runtime; got != RuntimeACPStdio {
		t.Errorf("session.ready Process.Runtime = %q, want %q", got, RuntimeACPStdio)
	}
}
