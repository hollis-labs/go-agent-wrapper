package sidebyside

import (
	"context"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/adapters/claude"
	"github.com/hollis-labs/go-agent-wrapper/adapters/claudeacp"
	"github.com/hollis-labs/go-agent-wrapper/wrapper"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// longEssayPrompt matches the shape adapters/{codexacp,claudeacp}'s own
// live_test.go files already use for their own mid-generation cancel
// tests: long enough that a real interrupt (vs. an
// acknowledge-and-let-finish) is unambiguous from wall-clock alone.
const longEssayPrompt = "Write a very long, detailed 2000-word essay about the history of " +
	"lighthouses, covering their engineering, their keepers, and their cultural symbolism. " +
	"Do not stop early. Do not use any tools."

// TestLiveClaudeNativeVsACP_InterruptFidelity confirms, live and
// contemporaneously for both paths, that adapters/claude's recorded
// Descriptor.Interrupt == adapters.InterruptProcess (kill-only — Stop()
// closes stdin, then escalates to SIGTERM/SIGKILL with real multi-second
// grace windows per agentkit's streamingStdioSession.Stop) and
// adapters/claudeacp's recorded InterruptCapability ==
// adapters.InterruptTurn (a genuine sub-second mid-turn abort via the
// bridge's real query.interrupt() call) match what actually happens when
// each path's cancel primitive is invoked mid-generation.
func TestLiveClaudeNativeVsACP_InterruptFidelity(t *testing.T) {
	requireRealClaudeBothPaths(t)

	// Sanity: both adapters' own recorded Descriptor/InterruptCapability
	// match what task 02/13's own Work Logs already established, before
	// spending real wall-clock proving it live below.
	if got := claude.New().Describe().Interrupt; got != adapters.InterruptProcess {
		t.Fatalf("adapters/claude Describe().Interrupt = %q, want %q (recorded value drifted — re-check this test's own premise)", got, adapters.InterruptProcess)
	}

	nativeLatency, nativeAcceptedText := runNativeInterruptTest(t)
	acpLatency := runACPInterruptTest(t)

	t.Logf("INTERRUPT FIDELITY — Claude native (adapters/claude, Descriptor.Interrupt=%q) vs. Claude ACP (adapters/claudeacp, InterruptCapability=%q):",
		adapters.InterruptProcess, adapters.InterruptTurn)
	t.Logf("  native Stop()  wall-clock to process/session termination: %v", nativeLatency)
	t.Logf("  ACP    Cancel() wall-clock to turn.completed/turn.failed: %v", acpLatency)
	t.Logf("  native path: %d characters of essay text had already streamed before Stop() was called (proof generation was genuinely underway)", len(nativeAcceptedText))

	if acpLatency > 10*time.Second {
		t.Errorf("ACP Cancel() took %v to produce a terminal event — too slow to be a genuine mid-turn abort (adapters/claudeacp's own live_test.go/doc.go precedent: ~7ms)", acpLatency)
	}
	// No upper-bound assertion on the native path — InterruptProcess is
	// documented as kill-only with a real multi-second grace window
	// (agentkit's streamingStdioSession.Stop: up to 2s EOF grace + up to
	// 5s SIGTERM grace before SIGKILL); this test's job is to MEASURE
	// that reality, not assert a not-yet-established bound on it.
}

// runNativeInterruptTest launches a fresh native Claude streaming-stdio
// session, waits blind (see the real per-line finding in the function
// body for why an activity-based gate is not possible on this path),
// calls Stop(), and returns the wall-clock latency from the Stop() call
// to it returning (agentkit's Stop is synchronous — it blocks until the
// session is confirmed torn down or its own grace timers expire) plus
// whatever essay text had already streamed before Stop was called.
func runNativeInterruptTest(t *testing.T) (latency time.Duration, acceptedTextBeforeStop string) {
	t.Helper()
	dir := t.TempDir()

	sink := &recordingSink{}
	w, err := wrapper.New(wrapper.Config{
		App:               "sidebyside-native-interrupt",
		Adapter:           claude.New(claude.WithExtraArgs("--dangerously-skip-permissions")),
		Activity:          activity.NewBridge(sink),
		Workdir:           dir,
		AutoFireFirstTurn: true,
		FirstTurnPayload:  frameUserMessage(longEssayPrompt),
	})
	if err != nil {
		t.Fatalf("wrapper.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()

	// Real, load-bearing finding this test's first iteration surfaced
	// (see this package's own Work Log entry, sibling Nanite repo, for
	// the full write-up): the native streaming-stdio path does NOT
	// deliver token-level deltas. go-providers' parseClaudeAssistant
	// parses each whole `"assistant"` stream-json line (one per
	// completed content block set) into exactly ONE agent.delta —
	// Claude's own `-p --output-format stream-json` mode flushes an
	// entire generated message as a single JSON line, not incrementally.
	// A real 2000-word-essay run against this path produced exactly ONE
	// agent.delta, containing the ENTIRE essay, only once generation had
	// already finished — so gating on ">=2 deltas" (the discipline
	// adapters/{claudeacp,codexacp}'s own live_test.go files use for the
	// ACP side, where deltas genuinely stream incrementally) is
	// structurally impossible to satisfy on this path and would hang
	// until timeout. This is itself the headline interrupt/activity
	// finding for the native path: there is no mid-generation signal at
	// all to react to — Stop() has to be timed blind, from the caller's
	// side, with no visibility into whether the child is still working.
	//
	// Gate on session.ready (the subprocess is alive; AutoFireFirstTurn
	// already delivered the prompt synchronously inside runtime.Start,
	// before session.ready is emitted — see wrapper.Run's doc comment)
	// plus a fixed buffer chosen to be comfortably shorter than a
	// 2000-word essay's real generation time but long enough that the
	// prompt has genuinely reached the model and generation is
	// underway — not an activity-driven gate, because none exists here.
	waitForKindAfter(t, sink.snapshot, runtimeevents.KindSessionReady, 0, 30*time.Second)
	const blindGenerationBuffer = 3 * time.Second
	time.Sleep(blindGenerationBuffer)

	preStop := sink.snapshot()
	alreadyIdle := false
	for _, te := range preStop {
		if te.ev.Kind == runtimeevents.KindSessionIdle {
			alreadyIdle = true
		}
		if te.ev.Kind == runtimeevents.KindAgentDelta {
			acceptedTextBeforeStop += payloadString(te.ev.Payload, "content")
		}
	}
	if alreadyIdle {
		t.Logf("native path: the essay turn had already completed naturally within %v of session.ready — Stop() below exercises normal teardown of an idle session, not a genuine mid-generation interrupt (a real finding in its own right: for this prompt/model, generation was faster than the buffer chosen)", blindGenerationBuffer)
	} else {
		t.Logf("native path: %v after session.ready, the turn had NOT yet completed (no session.idle observed) — Stop() below is a genuine mid-generation interrupt attempt", blindGenerationBuffer)
	}

	stopAt := time.Now()
	if err := w.Stop(context.Background()); err != nil {
		t.Logf("native Stop returned error (expected for a killed process): %v", err)
	}
	latency = time.Since(stopAt)

	select {
	case <-runErrCh:
	case <-time.After(15 * time.Second):
		t.Error("native Run did not return within 15s of Stop returning")
	}

	// Cross-check against the emitted interrupt.requested/acknowledged
	// pair the wrapper itself produces around session.Stop.
	full := sink.snapshot()
	var reqAt, ackAt time.Time
	for _, te := range full {
		switch te.ev.Kind {
		case runtimeevents.KindInterruptRequested:
			reqAt = te.at
		case runtimeevents.KindInterruptAcknowledged:
			ackAt = te.at
		}
	}
	if !reqAt.IsZero() && !ackAt.IsZero() {
		t.Logf("  (cross-check via interrupt.requested->interrupt.acknowledged event pair: %v)", ackAt.Sub(reqAt))
	}

	return latency, acceptedTextBeforeStop
}

// runACPInterruptTest launches a fresh claudeacp.Client session, waits
// for real mid-generation activity, calls Cancel(), and returns the
// wall-clock latency from the Cancel() call to the resulting
// turn.completed/turn.failed terminal event arriving on Events().
func runACPInterruptTest(t *testing.T) time.Duration {
	t.Helper()
	c := claudeacp.NewClient()
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelLaunch()
	if err := c.Launch(launchCtx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Skipf("live claude-agent-acp Launch failed (environment issue, not necessarily a code defect): %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	if got := c.InterruptCapability(); got != adapters.InterruptTurn {
		t.Fatalf("claudeacp Client.InterruptCapability() = %q, want %q (recorded value drifted — re-check this test's own premise)", got, adapters.InterruptTurn)
	}

	promptCtx, cancelPrompt := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelPrompt()
	if err := c.Prompt(promptCtx, longEssayPrompt); err != nil {
		t.Fatalf("ACP Prompt: %v", err)
	}

	deltas := 0
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
		t.Fatal("never observed 2 real agent.delta events before the cancel-timing deadline; cannot exercise a meaningful mid-turn Cancel")
	}

	cancelAt := time.Now()
	if err := c.Cancel(context.Background()); err != nil {
		t.Fatalf("ACP Cancel: %v", err)
	}

	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev := <-c.Events():
			if ev.Kind == runtimeevents.KindTurnCompleted || ev.Kind == runtimeevents.KindTurnFailed {
				return time.Since(cancelAt)
			}
		case <-deadline:
			t.Fatal("timed out waiting for the turn to end after Cancel")
		}
	}
}
