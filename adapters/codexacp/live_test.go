package codexacp

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
// TASKS/agent-host-acp/14 (Nanite repo) "Done means": "A real (not
// mocked) bridge-driven Codex session completes at least one real turn
// with correctly-translated runtimeevents activity — verified live" and
// "The adapter's real Interrupt capability is verified directly, not
// assumed".
//
// These tests spawn the REAL `npx -y @agentclientprotocol/codex-acp`
// bridge, which in turn spawns a REAL `codex app-server` subprocess (the
// real system `codex` CLI, resolved the same way [WithClientCodexBinary]
// resolves it by default) — no fake script, no mock. They run only when
// GO_AGENT_WRAPPER_LIVE_PROVIDER_TESTS=1 is set, then skip when `npx`
// isn't on PATH or when Launch fails for an
// environment reason (no Node.js, no real Codex credentials configured,
// network unavailable to fetch the npm package, ...) rather than a code
// defect, so `go test ./...` stays green in an environment without a
// live, authenticated Codex install and working Node.js/npm/npx — see
// package doc's "Node.js/npm/npx runtime requirement" section. On the
// machine this task was implemented and verified against (Node
// v22.12.0, npm/npx 10.9.0, codex-acp 1.6.2, real system codex CLI
// 0.147.0 with real ChatGPT-authenticated credentials already present),
// both tests pass for real — see this package's Work Log entry
// (TASKS/agent-host-acp/14, Nanite repo) for the transcript.
// ---------------------------------------------------------------------

func requireRealBridge(t *testing.T) {
	t.Helper()
	testgate.RequireLiveProvider(t)
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not on PATH (Node.js/npm not installed); skipping live ACP bridge test")
	}
}

func TestLiveClientCompletesOneRealTurn(t *testing.T) {
	requireRealBridge(t)

	c := NewClient()
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelLaunch()

	if err := c.Launch(launchCtx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Skipf("live codex-acp Launch failed (environment issue — Node/npm/auth/network — not necessarily a code defect): %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	promptCtx, cancelPrompt := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelPrompt()
	if err := c.Prompt(promptCtx, "Reply with exactly one word: pong"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var (
		sawDelta bool
		sawText  string
		terminal runtimeevents.Event
	)
	deadline := time.After(90 * time.Second)
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
			t.Fatal("timed out waiting for a real codex-acp turn to complete")
		}
	}

	if terminal.Kind != runtimeevents.KindTurnCompleted {
		t.Fatalf("turn ended with %v, want turn.completed (payload=%s)", terminal.Kind, string(terminal.Payload))
	}
	if !sawDelta {
		t.Error("never observed an agent.delta event during the real turn")
	}
	t.Logf("real codex-acp turn completed; message text observed: %q; terminal payload: %s", sawText, string(terminal.Payload))
}

// TestLiveClientCancelAbortsMidGeneration re-verifies, via the actual Go
// Client (not the standalone Python probe used during investigation —
// see package doc), that ACP's session/cancel produces a genuine
// mid-turn abort for the codex-acp bridge rather than an
// acknowledge-and-let-finish: it starts a deliberately long generation,
// waits for a few real delta chunks (proof generation is genuinely
// underway), sends Cancel, and asserts the turn's terminal event arrives
// quickly afterward — not after the model would have naturally finished
// a 2000-word essay. This is the live half of [Client.InterruptCapability]'s
// verification; the other half (reading the bridge's own source to
// confirm session/cancel really calls the app-server's `turn/interrupt`
// JSON-RPC method) is documented in the package doc.
func TestLiveClientCancelAbortsMidGeneration(t *testing.T) {
	requireRealBridge(t)

	c := NewClient()
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelLaunch()
	if err := c.Launch(launchCtx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Skipf("live codex-acp Launch failed (environment issue, not necessarily a code defect): %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	promptCtx, cancelPrompt := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancelPrompt()
	longPrompt := "Write a very long, detailed 2000-word essay about the history of doughnuts, " +
		"covering origins, cultural variations, and modern trends. Do not stop early. Do not use any tools."
	if err := c.Prompt(promptCtx, longPrompt); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Wait for real generation activity (any delta — thought or
	// message) before cancelling, so the test can't trivially "pass" by
	// cancelling before anything started.
	var deltas int
	waitDeadline := time.After(60 * time.Second)
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

	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev := <-c.Events():
			if ev.Kind == runtimeevents.KindTurnCompleted || ev.Kind == runtimeevents.KindTurnFailed {
				latency := time.Since(cancelSentAt)
				t.Logf("turn terminal event (%v) arrived %v after Cancel — payload: %s", ev.Kind, latency, string(ev.Payload))
				// A genuine essay-length completion would take far
				// longer than this bound; a generous-but-discriminating
				// threshold confirms Cancel produced a real abort, not a
				// lucky coincidental finish. Live investigation (see
				// package doc) measured ~12ms via a raw probe; this
				// bound leaves generous headroom for the extra Go
				// Client hop.
				if latency > 10*time.Second {
					t.Errorf("turn completed %v after Cancel — too slow to be a genuine mid-turn abort (raw-probe precedent is ~12ms)", latency)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the turn to end after Cancel — session/cancel did not produce any completion at all")
		}
	}
}
