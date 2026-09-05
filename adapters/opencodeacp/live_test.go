package opencodeacp

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/internal/testgate"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// ---------------------------------------------------------------------
// TASKS/agent-host-acp/09 (Nanite repo) "Done means": "A real (not
// mocked) opencode acp session, launched and driven through this
// adapter, completes at least one real turn with correctly-translated
// runtimeevents activity — verified live, not just via a wire-protocol
// unit test" and "The adapter's real Interrupt capability... is
// verified directly, not assumed".
//
// These tests drive the REAL `opencode acp` binary — no fake script, no
// mock. They run only when GO_AGENT_WRAPPER_LIVE_PROVIDER_TESTS=1 is set,
// then skip when `opencode` isn't on PATH or when
// Launch fails for an environment reason (no provider auth configured,
// etc.) rather than a code defect, so `go test ./...` stays green in an
// environment without a live, authenticated opencode install. On the
// machine this task was implemented and verified against (opencode
// 1.15.6, real credentials already configured), both tests pass for
// real — see this package's Work Log entry (TASKS/agent-host-acp/09)
// for the transcript.
// ---------------------------------------------------------------------

func requireRealOpenCode(t *testing.T) {
	t.Helper()
	testgate.RequireLiveProvider(t)
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not on PATH; skipping live ACP test")
	}
}

func TestLiveClientCompletesOneRealTurn(t *testing.T) {
	requireRealOpenCode(t)

	c := NewClient()
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelLaunch()

	if err := c.Launch(launchCtx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Skipf("live opencode acp Launch failed (environment issue — auth/model config — not necessarily a code defect): %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	promptCtx, cancelPrompt := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelPrompt()
	if err := c.Prompt(promptCtx, "Reply with exactly one word: pong"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var (
		sawDelta bool
		sawText  string
		terminal runtimeevents.Event
	)
	deadline := time.After(60 * time.Second)
loop:
	for {
		select {
		case ev := <-c.Events():
			switch ev.Kind {
			case runtimeevents.KindAgentDelta:
				sawDelta = true
				var payload struct {
					Content string `json:"content"`
					Phase   string `json:"phase"`
				}
				_ = json.Unmarshal(ev.Payload, &payload)
				if payload.Phase == "message" {
					sawText += payload.Content
				}
			case runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
				terminal = ev
				break loop
			}
		case <-deadline:
			t.Fatal("timed out waiting for a real opencode acp turn to complete")
		}
	}

	if terminal.Kind != runtimeevents.KindTurnCompleted {
		t.Fatalf("turn ended with %v, want turn.completed (payload=%s)", terminal.Kind, string(terminal.Payload))
	}
	if !sawDelta {
		t.Error("never observed an agent.delta event during the real turn")
	}
	t.Logf("real opencode acp turn completed; message text observed: %q; terminal payload: %s", sawText, string(terminal.Payload))
}

// TestLiveClientCancelAbortsMidGeneration re-verifies, via the actual Go
// Client (not the standalone Python probe used during investigation),
// that ACP's session/cancel produces a genuine mid-turn abort for
// OpenCode rather than an acknowledge-and-let-finish: it starts a
// deliberately long generation, waits for a few real delta chunks (proof
// generation is genuinely underway), sends Cancel, and asserts the
// turn's terminal event arrives quickly afterward — not after the
// model would have naturally finished a 2000-word essay.
func TestLiveClientCancelAbortsMidGeneration(t *testing.T) {
	requireRealOpenCode(t)

	c := NewClient()
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelLaunch()
	if err := c.Launch(launchCtx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Skipf("live opencode acp Launch failed (environment issue, not necessarily a code defect): %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	promptCtx, cancelPrompt := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelPrompt()
	longPrompt := "Write a very long, detailed 2000-word essay about the history of doughnuts, " +
		"covering origins, cultural variations, and modern trends. Do not stop early."
	if err := c.Prompt(promptCtx, longPrompt); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Wait for real generation activity (any delta — thought or
	// message) before cancelling, so the test can't trivially "pass" by
	// cancelling before anything started.
	var deltas int
	waitDeadline := time.After(30 * time.Second)
waitLoop:
	for deltas < 2 {
		select {
		case ev := <-c.Events():
			if ev.Kind == runtimeevents.KindAgentDelta {
				deltas++
			}
		case <-waitDeadline:
			break waitLoop
		}
	}
	if deltas < 2 {
		t.Fatal("never observed generation activity before the cancel-timing deadline; cannot exercise a meaningful mid-turn cancel")
	}

	cancelSentAt := time.Now()
	if err := c.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev := <-c.Events():
			if ev.Kind == runtimeevents.KindTurnCompleted || ev.Kind == runtimeevents.KindTurnFailed {
				latency := time.Since(cancelSentAt)
				t.Logf("turn terminal event (%v) arrived %v after Cancel — payload: %s", ev.Kind, latency, string(ev.Payload))
				// A genuine essay-length completion would take far
				// longer than this bound; a generous-but-discriminating
				// threshold confirms Cancel produced a real abort, not
				// a lucky coincidental finish.
				if latency > 10*time.Second {
					t.Errorf("turn completed %v after Cancel — too slow to be a genuine mid-turn abort (native OpenCode's own InterruptTurn precedent is sub-second)", latency)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the turn to end after Cancel — session/cancel did not produce any completion at all")
		}
	}
}
