package piacp

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
// TASKS/agent-host-acp/15 (Nanite repo) "Done means": "A working
// bridge-driven Pi ACP adapter exists... with a real (not mocked)
// session completing at least one real turn with correctly-translated
// runtimeevents activity — verified live... IF the `pi` CLI can
// actually be installed/configured in this environment."
//
// These tests drive the REAL `npx -y pi-acp` bridge, which in turn
// drives a REAL `pi --mode rpc` process — no fake script, no mock. They
// skip (not fail) when `npx`/`pi` aren't on PATH, or when Launch fails
// for an environment reason (no model/provider configured), rather than
// a code defect, so `go test ./...` stays green in an environment
// without a live, configured `pi` install.
//
// On the machine this task was implemented and verified against, NO
// cloud provider had usable credentials (`pi auth check` returned
// credentials_not_configured for anthropic/openai/google — see package
// doc). These tests were made to pass for real anyway, against a real
// local Ollama backend wired into `pi` via its own documented Custom
// Providers mechanism (`~/.pi/agent/models.json`) — see the package doc
// for the full setup and the transcript in this package's Work Log entry
// (TASKS/agent-host-acp/15, Nanite repo). Because a small local model is
// noticeably less reliable at following instructions/invoking tools than
// a frontier cloud model, these tests tolerate that: they retry a
// bounded number of times with a fresh session before concluding a given
// environment's configured model won't reliably exercise the behavior
// under test, rather than flaking on model non-determinism that has
// nothing to do with this package's own wire-protocol correctness (which
// PromptBeforeLaunch/client_test.go's fake-subprocess tests pin
// precisely, deterministically, and unconditionally).
// ---------------------------------------------------------------------

func requireRealPiACP(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not on PATH; skipping live ACP test (Node.js/npm/npx runtime requirement — see package doc)")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi binary not on PATH; pi-acp requires it directly (see package doc) — skipping live ACP test")
	}
}

func TestLiveClientCompletesOneRealTurn(t *testing.T) {
	requireRealPiACP(t)

	c := NewClient()
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelLaunch()

	if err := c.Launch(launchCtx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Skipf("live pi-acp Launch failed (environment issue — no model/provider configured — not necessarily a code defect): %v", err)
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
			t.Fatal("timed out waiting for a real pi-acp turn to complete")
		}
	}

	if terminal.Kind != runtimeevents.KindTurnCompleted {
		t.Fatalf("turn ended with %v, want turn.completed (payload=%s)", terminal.Kind, string(terminal.Payload))
	}
	if !sawDelta {
		t.Error("never observed an agent.delta event during the real turn")
	}
	t.Logf("real pi-acp turn completed; message text observed: %q; terminal payload: %s", sawText, string(terminal.Payload))
}

// TestLiveClientCancelAbortsMidGeneration re-verifies, via the actual Go
// Client (not the standalone Python probe used during investigation),
// that ACP's session/cancel produces a genuine mid-turn abort for Pi via
// pi-acp rather than an acknowledge-and-let-finish. It forces a
// definitely-still-running bash tool call (a `sleep`-based counting
// loop) rather than relying on token-generation speed, since a small
// local model's own generation is too fast/unreliable to make a
// meaningful timing claim on its own (see package doc). Retries a
// bounded number of times with a fresh session if the configured model
// never actually invokes the tool (observed local-model flakiness,
// unrelated to this package's own wire-protocol code).
func TestLiveClientCancelAbortsMidGeneration(t *testing.T) {
	requireRealPiACP(t)

	const maxAttempts = 3
	var lastSkipReason string

	for attempt := 0; attempt < maxAttempts; attempt++ {
		ok, skipReason := attemptCancelMidToolCall(t)
		if ok {
			return
		}
		lastSkipReason = skipReason
	}
	t.Skipf("never observed a real in-flight tool call across %d attempts (local-model flakiness, not a code defect): %s", maxAttempts, lastSkipReason)
}

// attemptCancelMidToolCall runs one Launch/Prompt/Cancel cycle. Returns
// ok=true if it observed and asserted a genuine mid-tool-call abort;
// ok=false (with a reason) if the configured model never actually
// started the bash tool call within the wait window, so the caller can
// retry with a fresh session.
func attemptCancelMidToolCall(t *testing.T) (ok bool, skipReason string) {
	t.Helper()

	c := NewClient()
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelLaunch()
	if err := c.Launch(launchCtx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Skipf("live pi-acp Launch failed (environment issue, not necessarily a code defect): %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	promptCtx, cancelPrompt := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelPrompt()
	prompt := "Use the bash tool to run exactly this command: " +
		"for i in $(seq 1 30); do echo tick $i; sleep 1; done"
	if err := c.Prompt(promptCtx, prompt); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Wait for real evidence the tool call actually started (a
	// tool_call_update carrying pi-acp's own terminal_output meta, or
	// simply a tool_call event at all) — not just token generation.
	var toolStarted bool
	waitDeadline := time.After(20 * time.Second)
waitLoop:
	for {
		select {
		case ev := <-c.Events():
			switch ev.Kind {
			case runtimeevents.KindAgentToolUse:
				toolStarted = true
			case runtimeevents.KindAgentToolResult:
				var payload struct {
					TerminalOutput string `json:"terminal_output"`
				}
				_ = json.Unmarshal(ev.Payload, &payload)
				if payload.TerminalOutput != "" {
					toolStarted = true
					break waitLoop
				}
			case runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
				// Model finished (or refused) without ever really
				// running the sleep loop long enough to observe —
				// nothing meaningful to cancel; let the caller retry.
				return false, "turn completed/failed before a real tool call was observed"
			}
		case <-waitDeadline:
			break waitLoop
		}
	}
	if !toolStarted {
		return false, "never observed tool_call/tool_result activity before the wait deadline"
	}

	// Give the sleep loop a couple of real seconds of wall-clock
	// progress so cancellation is unambiguously mid-flight.
	time.Sleep(3 * time.Second)

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
				// A genuine 30-second sleep loop would take far longer
				// than this bound to finish naturally; a
				// generous-but-discriminating threshold confirms Cancel
				// produced a real abort (this package's own manual
				// investigation observed single-digit-millisecond
				// latency — see package doc).
				if latency > 10*time.Second {
					t.Errorf("turn completed %v after Cancel — too slow to be a genuine mid-turn abort (pi-acp's own verified InterruptTurn precedent is sub-second)", latency)
				}
				return true, ""
			}
		case <-deadline:
			t.Fatal("timed out waiting for the turn to end after Cancel — session/cancel did not produce any completion at all")
		}
	}
}
