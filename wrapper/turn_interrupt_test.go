package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-providers/providertest"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/launch"
)

// CancelTurn on streaming-stdio Claude interrupts the turn and keeps the
// process: the turn fails with reason "interrupted", and the next input runs
// a turn on the same process (CW-20261001-0103). The transcript is
// go-providers' live capture of exactly that, from claude 2.1.286.
func TestCancelTurnInterruptsStreamingClaude(t *testing.T) {
	skipUnlessSh(t)
	fake := providertest.New(t, runtimes.Claude, providertest.Replay("claude/stream_interrupt"))
	var failed runtimeevents.Event
	runSelected(t, launch.Selection{Runtime: "claude", Mode: runtimes.ModeStreamingStdio, Binary: fake.Path}, nil,
		func(t *testing.T, w *Wrapper, sink *capturingSink, _ *acp.Manager) {
			sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
			if err := w.SendInput(context.Background(), []byte(`{"type":"user","message":{"role":"user","content":"run something slow"}}`)); err != nil {
				t.Fatalf("SendInput: %v", err)
			}
			sink.waitFor(t, runtimeevents.KindAgentToolUse, 5*time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := w.CancelTurn(ctx); err != nil {
				t.Fatalf("CancelTurn: %v", err)
			}
			sink.waitFor(t, runtimeevents.KindTurnFailed, 5*time.Second)
			for _, ev := range sink.snapshot() {
				if ev.Kind == runtimeevents.KindTurnFailed {
					failed = ev
				}
			}
			if err := w.SendInput(context.Background(), []byte(`{"type":"user","message":{"role":"user","content":"next"}}`)); err != nil {
				t.Fatalf("SendInput after CancelTurn: %v", err)
			}
			sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
		})
	var payload struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(failed.Payload, &payload); err != nil || payload.Reason != "interrupted" {
		t.Errorf("turn.failed payload = %s, want reason interrupted", failed.Payload)
	}
	if n := len(fake.Calls()); n != 1 {
		t.Errorf("claude started %d times; CancelTurn must keep the process", n)
	}
	if c := fake.Call(0); len(c.Stdin) != 3 || !strings.Contains(c.Stdin[1], `"subtype":"interrupt"`) {
		t.Errorf("stdin = %q, want user, interrupt, user", c.Stdin)
	}
}

// A native runtime without a turn interrupt still reports
// ErrTurnCancelUnsupported, before Run and during it.
func TestCancelTurnUnsupportedWithoutAnInterrupt(t *testing.T) {
	skipUnlessSh(t)
	w, err := New(Config{App: "t", Adapter: &nativeTestAdapter{cli: provider.NewOpencodeAdapter()}, Activity: activity.NewBridge(newCapturingSink())})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.CancelTurn(context.Background()); !errors.Is(err, ErrTurnCancelUnsupported) {
		t.Errorf("CancelTurn before Run = %v", err)
	}
	fake := providertest.New(t, runtimes.OpenCode, providertest.Replay("opencode/run_turn1").When("run"))
	runSelected(t, launch.Selection{Runtime: "opencode", Binary: fake.Path}, nil,
		func(t *testing.T, w *Wrapper, sink *capturingSink, m *acp.Manager) {
			drivePerTurn(t, w, sink, m)
			if err := w.CancelTurn(context.Background()); !errors.Is(err, ErrTurnCancelUnsupported) {
				t.Errorf("CancelTurn on opencode run = %v, want ErrTurnCancelUnsupported", err)
			}
		})
}

// The prepared adapter forwards a turn interrupt only when the adapter it
// wraps has one; claiming it otherwise would send a CLI a frame it does not
// speak.
func TestPreparedAdapterForwardsTurnInterrupterOnlyWhenPresent(t *testing.T) {
	exec := &agentlaunch.PreparedExecution{Bindings: agentlaunch.ExecutionBindings{Argv: []string{"/bin/claude"}, CWD: t.TempDir()}}
	claude, err := preparedCLIAdapter(provider.NewClaudeAdapterStreamingStdio(), exec)
	if err != nil {
		t.Fatal(err)
	}
	ti, ok := claude.(provider.TurnInterrupter)
	if !ok {
		t.Fatal("prepared Claude hides TurnInterrupter")
	}
	if _, ok, _ := ti.InterruptResponse(ti.InterruptRequest("x")); ok {
		t.Error("a request read as its own answer")
	}
	if _, ok := claude.(provider.EventParser); !ok {
		t.Error("the interruptible prepared adapter dropped EventParser")
	}
	opencode, err := preparedCLIAdapter(provider.NewOpencodeAdapter(), exec)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := opencode.(provider.TurnInterrupter); ok {
		t.Error("prepared OpenCode claims a turn interrupt it does not have")
	}
	codex, err := preparedCLIAdapter(provider.NewCodexAdapterAppServer(), exec)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := codex.(provider.RPCTurnInterrupter); !ok {
		t.Error("prepared Codex hides RPCTurnInterrupter")
	}
	if _, ok := codex.(provider.TurnInterrupter); ok {
		t.Error("prepared Codex claims a stdin interrupt it does not have")
	}
	if _, ok := opencode.(provider.RPCTurnInterrupter); ok {
		t.Error("prepared OpenCode claims a JSON-RPC interrupt")
	}
}

func TestTurnMarks(t *testing.T) {
	var m turnMarks
	if id := m.markOpen(); id != "" || m.isInterrupted("") {
		t.Errorf("marking with no open turn = %q", id)
	}
	m.opened("t1")
	if id := m.markOpen(); id != "t1" || !m.isInterrupted("t1") {
		t.Errorf("markOpen = %q", id)
	}
	m.closed("t1")
	m.opened("t2")
	if m.isInterrupted("t2") || m.isInterrupted("t1") {
		t.Error("the mark outlived its turn")
	}
	m.markOpen()
	m.unmark("t2")
	if m.isInterrupted("t2") {
		t.Error("unmark kept the mark")
	}
	if got := withInterruptedReason(map[string]any{"error": "e"}).(map[string]any); got["reason"] != "interrupted" || got["error"] != "e" {
		t.Errorf("withInterruptedReason = %v", got)
	}
}

// stdoutHas reports whether the session's stdout carried a line containing
// all of parts.
func stdoutHas(sink *capturingSink, parts ...string) bool {
	for _, ev := range sink.snapshot() {
		if ev.Kind != runtimeevents.KindStdoutLine {
			continue
		}
		var line struct {
			Line string `json:"line"`
		}
		if json.Unmarshal(ev.Payload, &line) != nil {
			continue
		}
		all := true
		for _, p := range parts {
			all = all && strings.Contains(line.Line, p)
		}
		if all {
			return true
		}
	}
	return false
}

func waitStdout(t *testing.T, sink *capturingSink, parts ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !stdoutHas(sink, parts...) {
		if time.Now().After(deadline) {
			t.Fatalf("stdout never carried %q", parts)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// CancelTurn on Codex app-server, with the host driving the thread protocol
// as raw frames numbered from 1: the wrapper sends turn/interrupt for the
// open turn, Codex completes it "interrupted", and the host's next turn runs
// on the same process. The transcript is go-providers' live capture
// (codex-cli 0.159.2) (CW-20261001-0160).
func TestCancelTurnInterruptsCodexAppServer(t *testing.T) {
	skipUnlessSh(t)
	fake := providertest.New(t, runtimes.Codex, providertest.Replay("codex/app_server_interrupt"))
	runHostDriven(t, launch.Selection{Runtime: "codex", Mode: runtimes.ModeJSONRPCStdio, Binary: fake.Path},
		func(t *testing.T, w *Wrapper, sink *capturingSink) {
			send := func(frame string) {
				if err := w.SendInput(context.Background(), []byte(frame)); err != nil {
					t.Fatalf("SendInput %s: %v", frame, err)
				}
			}
			sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
			send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"t","version":"0"}}}`)
			if _, err := awaitCodexResponse(sink, 1, 10*time.Second); err != nil {
				t.Fatalf("initialize: %v", err)
			}
			send(`{"jsonrpc":"2.0","method":"initialized"}`)
			send(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}`)
			if _, err := awaitCodexResponse(sink, 2, 10*time.Second); err != nil {
				t.Fatalf("thread/start: %v", err)
			}
			send(`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"t","input":[]}}`)
			waitStdout(t, sink, `"item/started"`, `"commandExecution"`)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := w.CancelTurn(ctx); err != nil {
				t.Fatalf("CancelTurn: %v", err)
			}
			waitStdout(t, sink, `"turn/completed"`, `"status":"interrupted"`)
			send(`{"jsonrpc":"2.0","id":4,"method":"turn/start","params":{"threadId":"t","input":[]}}`)
			waitStdout(t, sink, `"turn/completed"`, `"status":"completed"`)
			sink.waitFor(t, runtimeevents.KindInterruptAcknowledged, time.Second)
		})
	if n := len(fake.Calls()); n != 1 {
		t.Errorf("codex started %d times; CancelTurn must keep the process", n)
	}
	var interrupt string
	for _, line := range fake.Call(0).Stdin {
		if strings.Contains(line, `"turn/interrupt"`) {
			interrupt = line
		}
	}
	if !strings.Contains(interrupt, `"turnId":"00000000-0000-4000-8000-000000000003"`) {
		t.Errorf("turn/interrupt = %q, want the captured turn", interrupt)
	}
	if errs := fake.Errors(); len(errs) != 0 {
		t.Errorf("fake errors: %q", errs)
	}
}

// runHostDriven runs a wrapper whose host drives the runtime's own protocol
// (Codex app-server), so no agent text is expected on the event stream.
func runHostDriven(t *testing.T, sel launch.Selection, drive func(*testing.T, *Wrapper, *capturingSink)) {
	t.Helper()
	adapter, err := launch.Select(sel)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	sink := newCapturingSink()
	w, err := New(Config{App: "host-driven", Adapter: adapter, Activity: activity.NewBridge(sink), Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(ctx) }()
	drive(t, w, sink)
	if err := w.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
}
