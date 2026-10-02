package turnoutput_test

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/go-providers/providertest"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/launch"
	"github.com/hollis-labs/go-agent-wrapper/turnoutput"
	"github.com/hollis-labs/go-agent-wrapper/wrapper"
)

// These tests run what real CLIs wrote (go-providers' providertest fixtures,
// captured from the CLIs and scrubbed) through the real wrapper and reduce the
// events it emits, so the reducer is checked against each runtime's actual wire
// shape rather than a hand-built one. No real CLI is launched: every runtime
// is a providertest fake.

// replay launches runtime through launch.Select against fixture, drives the
// session with drive, and returns the Outputs the reducer reported but drive did
// not take. It reduces from the wrapper's Activity sink, as a host does.
func replay(t *testing.T, id runtimes.ID, fixture providertest.Run, drive driver) []turnoutput.Output {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("providertest fakes need sh")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available on PATH")
	}
	fake := providertest.New(t, id, fixture)
	return reduceRun(t, launch.Selection{Runtime: string(id), Binary: fake.Path}, 30*time.Second, drive)
}

// driver sends the session its turns and returns once the last Output is in.
type driver func(*testing.T, *wrapper.Wrapper, *acp.Manager, <-chan turnoutput.Output)

// reduceRun launches sel, drives it, stops it and returns the Outputs left in
// the channel.
func reduceRun(t *testing.T, sel launch.Selection, timeout time.Duration, drive driver) []turnoutput.Output {
	t.Helper()
	adapter, err := launch.Select(sel)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}

	reducer := turnoutput.New(turnoutput.Config{})
	outs := make(chan turnoutput.Output, 16)
	manager := acp.NewManager()
	w, err := wrapper.New(wrapper.Config{
		App:     "turnoutput-" + sel.Runtime,
		Adapter: adapter,
		Activity: activity.NewBridge(runtimeevents.SinkFunc(func(_ context.Context, ev runtimeevents.Event) error {
			if out, ok := reducer.Observe(ev); ok {
				outs <- out
			}
			return nil
		})),
		Workdir:    t.TempDir(),
		ACPManager: manager,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(ctx) }()

	drive(t, w, manager, outs)

	if err := w.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after Stop")
	}

	close(outs)
	var left []turnoutput.Output
	for out := range outs {
		left = append(left, out)
	}
	return left
}

// ask sends one turn and returns the Output that ends it.
func ask(t *testing.T, w *wrapper.Wrapper, outs <-chan turnoutput.Output, input string) turnoutput.Output {
	t.Helper()
	return askWithin(t, w, outs, input, 10*time.Second)
}

// askWithin is ask with a time limit for the turn to end.
func askWithin(t *testing.T, w *wrapper.Wrapper, outs <-chan turnoutput.Output, input string, limit time.Duration) turnoutput.Output {
	t.Helper()
	// The wrapper refuses input until Run has started the session.
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := w.SendInput(context.Background(), []byte(input))
		if err == nil {
			break
		}
		if !errors.Is(err, wrapper.ErrSessionNotStarted) || time.Now().After(deadline) {
			t.Fatalf("SendInput: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case out := <-outs:
		return out
	case <-time.After(limit):
		t.Fatalf("no turn output for %q", input)
		return turnoutput.Output{}
	}
}

// waitReady waits for an ACP session to be ready to prompt.
func waitReady(t *testing.T, w *wrapper.Wrapper, manager *acp.Manager) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if session, ok := manager.Lookup(w.SessionID()); ok && session.Snapshot().State == acp.StateReady {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("ACP session never became ready")
}

func requireOutput(t *testing.T, got turnoutput.Output, kind turnoutput.Kind, text string) {
	t.Helper()
	if got.Kind != kind || got.Text != text {
		t.Fatalf("Output = {kind %q, text %q}, want {kind %q, text %q}\n full: %+v", got.Kind, got.Text, kind, text, got)
	}
	if got.SessionID == "" || got.TurnID == "" || got.Runtime == "" {
		t.Fatalf("Output is missing identity: %+v", got)
	}
}

func TestReplayClaudeStreamingTwoTurns(t *testing.T) {
	var turn1, turn2 turnoutput.Output
	got := replay(t, runtimes.Claude, providertest.Replay("claude/stream_two_turns"),
		func(t *testing.T, w *wrapper.Wrapper, _ *acp.Manager, outs <-chan turnoutput.Output) {
			turn1 = ask(t, w, outs, `{"type":"user","message":{"role":"user","content":"say hi"}}`)
			turn2 = ask(t, w, outs, `{"type":"user","message":{"role":"user","content":"say bye"}}`)
		})
	if len(got) != 0 {
		t.Fatalf("unreported Outputs: %+v", got)
	}
	requireOutput(t, turn1, turnoutput.KindFinal, "Hi! 👋 I'm ready to help you with software engineering tasks. What would you like to work on?")
	requireOutput(t, turn2, turnoutput.KindFinal, "Bye! 👋 Feel free to reach out anytime you need help with code.")
	if turn1.TurnID == turn2.TurnID {
		t.Fatalf("both turns report %s", turn1.TurnID)
	}
	if turn1.Runtime != "claude" || turn1.StopReason != "end_turn" {
		t.Fatalf("turn 1 = %+v, want runtime claude, stop end_turn", turn1)
	}
	// Claude's result.result is the exact text, but the wrapper does not carry it
	// on turn.completed yet; until it does the reducer falls back to the last
	// block, which is the same words.
	if turn1.Confidence != turnoutput.ConfidenceHeuristic {
		t.Fatalf("confidence = %s", turn1.Confidence)
	}
}

func TestReplayOpenCodeRun(t *testing.T) {
	var out turnoutput.Output
	replay(t, runtimes.OpenCode, providertest.Replay("opencode/run_turn1").When("run"),
		func(t *testing.T, w *wrapper.Wrapper, _ *acp.Manager, outs <-chan turnoutput.Output) {
			out = ask(t, w, outs, "say hi")
		})
	requireOutput(t, out, turnoutput.KindFinal, "OK.")
	if out.Runtime != "opencode" || out.Confidence != turnoutput.ConfidenceHeuristic {
		t.Fatalf("Output = %+v, want runtime opencode and heuristic confidence", out)
	}
}

func TestReplayAntigravityPrint(t *testing.T) {
	var out turnoutput.Output
	replay(t, runtimes.Antigravity, providertest.Replay("antigravity/print_turn1"),
		func(t *testing.T, w *wrapper.Wrapper, _ *acp.Manager, outs <-chan turnoutput.Output) {
			out = ask(t, w, outs, "say hi")
		})
	requireOutput(t, out, turnoutput.KindFinal, "OK")
	if out.Confidence != turnoutput.ConfidenceHeuristic {
		t.Fatalf("Output = %+v, want heuristic confidence", out)
	}
}

func TestReplayACPAgents(t *testing.T) {
	tests := []struct {
		name    string
		id      runtimes.ID
		fixture providertest.Run
		text    string
	}{
		{"copilot", runtimes.Copilot, providertest.Replay("copilot/acp_turn").When("--acp"), "Hi!"},
		{"pi", runtimes.Pi, providertest.Replay("pi/acp_turn"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out turnoutput.Output
			replay(t, tt.id, tt.fixture,
				func(t *testing.T, w *wrapper.Wrapper, manager *acp.Manager, outs <-chan turnoutput.Output) {
					waitReady(t, w, manager)
					out = ask(t, w, outs, "say hi")
				})
			if out.Kind != turnoutput.KindFinal || out.StopReason != "end_turn" || out.Confidence != turnoutput.ConfidenceHeuristic {
				t.Fatalf("Output = %+v, want a heuristic final turn that ended with end_turn", out)
			}
			if tt.text != "" && out.Text != tt.text {
				t.Fatalf("text = %q, want %q", out.Text, tt.text)
			}
			if out.Text == "" {
				t.Fatal("an ACP turn that answered reduced to empty text")
			}
		})
	}
}
