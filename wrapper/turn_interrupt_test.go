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
