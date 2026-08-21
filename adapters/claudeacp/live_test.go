package claudeacp

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// ---------------------------------------------------------------------
// TASKS/agent-host-acp/13 (Nanite repo) "Done means": "A real (not
// mocked) bridge-driven Claude session completes at least one real turn
// with correctly-translated runtimeevents activity — verified live" and
// "The adapter's real Interrupt capability is verified directly (does
// session/cancel genuinely abort mid-turn, or just acknowledge?)".
//
// These tests spawn the REAL `npx -y @agentclientprotocol/claude-agent-acp`
// bridge — no fake script, no mock — which in turn drives the real Claude
// Agent SDK using whatever credentials are already configured on the
// host (the same auth the `claude` CLI itself uses). They skip (not
// fail) when `npx` isn't on PATH, or when Launch fails for an
// environment reason (no provider auth configured, network unavailable,
// etc.) rather than a code defect, so `go test ./...` stays green in an
// environment without Node.js/npm/npx or live, authenticated Claude
// credentials — see the package doc's "Node.js/npm/npx runtime
// requirement" section. On the machine this task was implemented and
// verified against (Node v22.12.0, @agentclientprotocol/claude-agent-acp
// 0.70.0, real credentials already configured for the `claude` CLI), both
// tests pass for real — see this package's Work Log entry
// (TASKS/agent-host-acp/13, Nanite repo) for the transcript.
// ---------------------------------------------------------------------

func requireRealBridgeRuntime(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not on PATH; skipping live ACP bridge test (Node.js/npm/npx runtime requirement — see package doc)")
	}
}

func TestLiveClientCompletesOneRealTurn(t *testing.T) {
	requireRealBridgeRuntime(t)

	c := NewClient()
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelLaunch()

	if err := c.Launch(launchCtx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Skipf("live claude-agent-acp Launch failed (environment issue — auth/npx-resolve/network — not necessarily a code defect): %v", err)
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
			t.Fatal("timed out waiting for a real claude-agent-acp turn to complete")
		}
	}

	if terminal.Kind != runtimeevents.KindTurnCompleted {
		t.Fatalf("turn ended with %v, want turn.completed (payload=%s)", terminal.Kind, string(terminal.Payload))
	}
	if !sawDelta {
		t.Error("never observed an agent.delta event during the real turn")
	}
	t.Logf("real claude-agent-acp turn completed; message text observed: %q; terminal payload: %s", sawText, string(terminal.Payload))
}

// TestLiveClientCancelAbortsMidGeneration re-verifies, via the actual Go
// Client (not the standalone Node.js probe used during investigation),
// that ACP's session/cancel produces a genuine mid-turn abort for the
// Claude bridge rather than an acknowledge-and-let-finish: it starts a
// deliberately long generation, waits for a few real delta chunks (proof
// generation is genuinely underway), sends Cancel, and asserts the
// turn's terminal event arrives quickly afterward — not after the model
// would have naturally finished a 2000-word essay. Matches
// [adapters/opencodeacp]'s and [adapters/copilotacp]'s own live cancel
// tests.
func TestLiveClientCancelAbortsMidGeneration(t *testing.T) {
	requireRealBridgeRuntime(t)

	c := NewClient()
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelLaunch()
	if err := c.Launch(launchCtx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Skipf("live claude-agent-acp Launch failed (environment issue, not necessarily a code defect): %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	promptCtx, cancelPrompt := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelPrompt()
	longPrompt := "Write a very long, detailed 2000-word essay about the history of doughnuts, " +
		"covering origins, cultural variations, and modern trends. Do not stop early."
	if err := c.Prompt(promptCtx, longPrompt); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Wait for real generation activity (any delta — thought or message)
	// before cancelling, so the test can't trivially "pass" by cancelling
	// before anything started.
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
				// A genuine essay-length completion would take far longer
				// than this bound; a generous-but-discriminating
				// threshold confirms Cancel produced a real abort, not a
				// lucky coincidental finish. The bridge's own source
				// confirms this reaches the SDK's real query.interrupt()
				// (see package doc) — this live measurement corroborates
				// that against the real subprocess.
				if latency > 10*time.Second {
					t.Errorf("turn completed %v after Cancel — too slow to be a genuine mid-turn abort (OpenCode/Copilot CLI's own InterruptTurn precedent is sub-second)", latency)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the turn to end after Cancel — session/cancel did not produce any completion at all")
		}
	}
}
