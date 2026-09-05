package sidebyside

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters/claude"
	"github.com/hollis-labs/go-agent-wrapper/adapters/claudeacp"
	"github.com/hollis-labs/go-agent-wrapper/internal/testgate"
	"github.com/hollis-labs/go-agent-wrapper/wrapper"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// ---------------------------------------------------------------------
// TASKS/agent-host-acp/17 (Nanite repo). See this package's doc comment
// for why Claude (native adapters/claude vs. bridge-mediated
// adapters/claudeacp) was chosen as the comparison pair. Every test here
// spawns REAL processes only when GO_AGENT_WRAPPER_LIVE_PROVIDER_TESTS=1
// is set, and then skips when a real dependency (claude binary,
// npx/Node.js, live auth) is unavailable — same
// discipline as adapters/{claudeacp,codexacp,opencodeacp,piacp}'s own
// live_test.go files.
// ---------------------------------------------------------------------

func requireRealClaudeBothPaths(t *testing.T) {
	t.Helper()
	testgate.RequireLiveProvider(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not on PATH; skipping live native-vs-ACP comparison")
	}
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not on PATH; skipping live native-vs-ACP comparison (claude-agent-acp bridge needs Node.js/npm/npx)")
	}
}

// frameUserMessage wraps text as the single NDJSON object Claude's
// `-p --input-format stream-json` mode expects on stdin — the same shape
// internal/runtime/agent/factory.go's streamingStdioUserFrame builds in
// the sibling Nanite repo, reproduced here (test-only, no import cycle
// back to Nanite) rather than imported.
func frameUserMessage(text string) string {
	type userMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type frame struct {
		Type    string  `json:"type"`
		Message userMsg `json:"message"`
	}
	b, err := json.Marshal(frame{Type: "user", Message: userMsg{Role: "user", Content: text}})
	if err != nil {
		panic(err) // static input; Marshal cannot fail
	}
	return string(b)
}

// timedEvent pairs a runtimeevents.Event with the wall-clock instant this
// harness observed it — recorded uniformly at the moment each event
// reaches THIS package's own consumer code, for both paths, so latency
// comparisons are apples-to-apples regardless of whether either
// implementation happens to populate its own internal Time field.
type timedEvent struct {
	at time.Time
	ev runtimeevents.Event
}

// recordingSink is a runtimeevents.Sink that timestamps and stores every
// event the native (wrapper.Wrapper) path emits.
type recordingSink struct {
	mu     sync.Mutex
	events []timedEvent
}

func (s *recordingSink) Write(_ context.Context, ev runtimeevents.Event) error {
	s.mu.Lock()
	s.events = append(s.events, timedEvent{at: time.Now(), ev: ev})
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) snapshot() []timedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]timedEvent, len(s.events))
	copy(out, s.events)
	return out
}

// waitForKindAfter polls snap (via fetch) until an event of kind kind
// appears at or after fromIdx, returning its index and the event, or
// fails the test after d.
func waitForKindAfter(t *testing.T, fetch func() []timedEvent, kind runtimeevents.EventKind, fromIdx int, d time.Duration) (int, timedEvent) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		evs := fetch()
		for i := fromIdx; i < len(evs); i++ {
			if evs[i].ev.Kind == kind {
				return i, evs[i]
			}
		}
		if time.Now().After(deadline) {
			var kinds []string
			for _, e := range evs {
				kinds = append(kinds, string(e.ev.Kind))
			}
			t.Fatalf("timed out after %v waiting for %q at/after index %d; observed: %v", d, kind, fromIdx, kinds)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// countKindsAfter tallies how many events of kind appear at or after
// fromIdx (exclusive upper bound toIdx, or end of slice when toIdx < 0).
func countKindsAfter(evs []timedEvent, kind runtimeevents.EventKind, fromIdx, toIdx int) int {
	n := 0
	for i := fromIdx; i < len(evs) && (toIdx < 0 || i < toIdx); i++ {
		if evs[i].ev.Kind == kind {
			n++
		}
	}
	return n
}

// payloadString extracts a top-level string field from a
// runtimeevents.Event's JSON payload; returns "" on any decode failure.
func payloadString(payload json.RawMessage, field string) string {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	v, _ := m[field].(string)
	return v
}

// ---------------------------------------------------------------------
// Test 1: activity fidelity + tool reporting, matched real turns.
// ---------------------------------------------------------------------

// TestLiveClaudeNativeVsACP_ActivityAndToolReporting drives ONE plain
// turn ("reply with one word, no tool") and ONE tool-using turn (a real
// Bash `echo`) through both the native and ACP paths with the identical
// prompts, and reports what each path's activity stream carried.
func TestLiveClaudeNativeVsACP_ActivityAndToolReporting(t *testing.T) {
	requireRealClaudeBothPaths(t)

	const plainPrompt = "Reply with exactly one word: pong. Do not use any tool."
	toolMarker := fmt.Sprintf("hello-sidebyside-%d", time.Now().UnixNano())
	toolPrompt := fmt.Sprintf("Use the Bash tool to run `echo %s` and then reply with exactly the output it printed, nothing else.", toolMarker)

	native := runNativeClaudeSession(t, plainPrompt, toolPrompt)
	acpRun := runACPClaudeSession(t, plainPrompt, toolPrompt)

	t.Logf("NATIVE (adapters/claude, streaming-stdio, real `claude` subprocess):")
	t.Logf("  turn 1 (plain):    launch-to-first-delta=%v  first-delta-to-completed=%v  total=%v  text=%q",
		native.turn1FirstDelta.Sub(native.turn1Start), native.turn1Done.Sub(native.turn1FirstDelta), native.turn1Done.Sub(native.turn1Start), native.turn1Text)
	t.Logf("  turn 2 (tool use): total=%v  tool_use_events=%d  tool_result_events=%d  saw_marker_in_result=%v",
		native.turn2Done.Sub(native.turn2Start), native.turn2ToolUseCount, native.turn2ToolResultCount, native.turn2SawMarker)

	t.Logf("ACP (adapters/claudeacp, via real npx claude-agent-acp bridge):")
	t.Logf("  turn 1 (plain):    launch-to-first-delta=%v  first-delta-to-completed=%v  total=%v  text=%q",
		acpRun.turn1FirstDelta.Sub(acpRun.turn1Start), acpRun.turn1Done.Sub(acpRun.turn1FirstDelta), acpRun.turn1Done.Sub(acpRun.turn1Start), acpRun.turn1Text)
	t.Logf("  turn 2 (tool use): total=%v  tool_use_events=%d  tool_result_events=%d  saw_marker_in_result=%v",
		acpRun.turn2Done.Sub(acpRun.turn2Start), acpRun.turn2ToolUseCount, acpRun.turn2ToolResultCount, acpRun.turn2SawMarker)

	// ----- activity fidelity: both paths must have produced at least
	// one agent.delta for the plain turn -----
	if !native.turn1SawDelta {
		t.Error("native path: no agent.delta observed for the plain turn")
	}
	if !acpRun.turn1SawDelta {
		t.Error("ACP path: no agent.delta observed for the plain turn")
	}

	// ----- tool reporting: both paths must report the real tool call
	// AND its real result, and both must actually observe the marker
	// text in the final response -----
	if native.turn2ToolUseCount == 0 {
		t.Error("native path: no agent.tool_use observed for the tool-use turn")
	}
	if native.turn2ToolResultCount == 0 {
		t.Error("native path: no agent.tool_result observed for the tool-use turn")
	}
	if !native.turn2SawMarker {
		t.Errorf("native path: marker %q never observed in tool_result/delta content for the tool-use turn", toolMarker)
	}
	if acpRun.turn2ToolUseCount == 0 {
		t.Error("ACP path: no agent.tool_use (tool_call) observed for the tool-use turn")
	}
	if acpRun.turn2ToolResultCount == 0 {
		t.Error("ACP path: no agent.tool_result (tool_call_update) observed for the tool-use turn")
	}
	if !acpRun.turn2SawMarker {
		t.Errorf("ACP path: marker %q never observed in tool_result/delta content for the tool-use turn", toolMarker)
	}
}

type nativeRunResult struct {
	turn1Start      time.Time
	turn1FirstDelta time.Time
	turn1Done       time.Time
	turn1SawDelta   bool
	turn1Text       string

	turn2Start           time.Time
	turn2Done            time.Time
	turn2ToolUseCount    int
	turn2ToolResultCount int
	turn2SawMarker       bool
}

// runNativeClaudeSession drives ONE long-lived native streaming-stdio
// Claude session (adapters/claude, via wrapper.Wrapper) through a plain
// turn then a tool-using turn, mirroring wrapper/wrapper_real_adapters_test.go's
// established real-run pattern (AutoFireFirstTurn for turn 1, manual
// SendInput for turn 2). --dangerously-skip-permissions is passed via
// claude.WithExtraArgs so the tool-use turn does not block on an
// interactive approval prompt neither this harness nor a headless CLI
// session has any TTY to answer — the same flag Nanite's own dev-mode
// adapter variant (provider.NewClaudeAdapterDevStreamingStdio) sets by
// default; the shipped go-agent-wrapper adapters/claude package itself
// does not set it, so this is this harness's own explicit choice, not a
// hidden default.
func runNativeClaudeSession(t *testing.T, plainPrompt, toolPrompt string) nativeRunResult {
	t.Helper()
	dir := t.TempDir()

	sink := &recordingSink{}
	w, err := wrapper.New(wrapper.Config{
		App:               "sidebyside-native",
		Adapter:           claude.New(claude.WithExtraArgs("--dangerously-skip-permissions")),
		Activity:          activity.NewBridge(sink),
		Workdir:           dir,
		AutoFireFirstTurn: true,
		FirstTurnPayload:  frameUserMessage(plainPrompt),
	})
	if err != nil {
		t.Fatalf("wrapper.New: %v", err)
	}

	var result nativeRunResult
	result.turn1Start = time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()

	// ----- turn 1: plain, auto-fired -----
	// Use session.idle, not turn.completed, as the turn-boundary marker:
	// a real observed finding of this task (see this package's own
	// Work Log entry in the sibling Nanite repo) — the native path's
	// event_translator.go maps BOTH llmtypes.EventUsage and
	// llmtypes.EventDone to runtimeevents.KindTurnCompleted, so ONE real
	// Claude turn emits TWO turn.completed events in immediate
	// succession. wrapper.go's own currentTurnID bookkeeping only emits
	// session.idle once per real turn close (the second turn.completed
	// arrives after currentTurnID has already been reset to "", so its
	// own closedTurnID check is empty and no second session.idle
	// fires) — the reliable, single-fire boundary this harness needs to
	// correctly separate turn 1 from turn 2 below.
	idx1, ev1 := waitForKindAfter(t, sink.snapshot, runtimeevents.KindSessionIdle, 0, 90*time.Second)
	result.turn1Done = ev1.at
	for _, te := range sink.snapshot()[:idx1] {
		if te.ev.Kind == runtimeevents.KindAgentDelta {
			if !result.turn1SawDelta {
				result.turn1FirstDelta = te.at
			}
			result.turn1SawDelta = true
			result.turn1Text += payloadString(te.ev.Payload, "content")
		}
	}

	// ----- turn 2: real tool use, manually sent -----
	result.turn2Start = time.Now()
	if err := w.SendInput(context.Background(), []byte(frameUserMessage(toolPrompt))); err != nil {
		t.Fatalf("native SendInput (turn 2): %v", err)
	}
	idx2, ev2 := waitForKindAfter(t, sink.snapshot, runtimeevents.KindSessionIdle, idx1+1, 90*time.Second)
	result.turn2Done = ev2.at

	full := sink.snapshot()
	result.turn2ToolUseCount = countKindsAfter(full, runtimeevents.KindAgentToolUse, idx1+1, idx2)
	result.turn2ToolResultCount = countKindsAfter(full, runtimeevents.KindAgentToolResult, idx1+1, idx2)
	toolMarker := extractMarkerFromToolPrompt(toolPrompt)
	for i := idx1 + 1; i <= idx2 && i < len(full); i++ {
		te := full[i]
		if te.ev.Kind == runtimeevents.KindAgentToolResult || te.ev.Kind == runtimeevents.KindAgentDelta {
			if strings.Contains(string(te.ev.Payload), toolMarker) {
				result.turn2SawMarker = true
			}
		}
	}

	if err := w.Stop(context.Background()); err != nil {
		t.Errorf("native Stop (teardown): %v", err)
	}
	select {
	case <-runErrCh:
	case <-time.After(15 * time.Second):
		t.Error("native Run did not return within 15s of Stop")
	}
	return result
}

// extractMarkerFromToolPrompt pulls the `echo <marker>` token back out of
// the tool prompt this file itself constructed — avoids threading an
// extra parameter through runNativeClaudeSession/runACPClaudeSession
// just for the marker string.
func extractMarkerFromToolPrompt(toolPrompt string) string {
	const prefix = "run `echo "
	i := strings.Index(toolPrompt, prefix)
	if i < 0 {
		return ""
	}
	rest := toolPrompt[i+len(prefix):]
	j := strings.Index(rest, "`")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

type acpRunResult struct {
	turn1Start      time.Time
	turn1FirstDelta time.Time
	turn1Done       time.Time
	turn1SawDelta   bool
	turn1Text       string

	turn2Start           time.Time
	turn2Done            time.Time
	turn2ToolUseCount    int
	turn2ToolResultCount int
	turn2SawMarker       bool
}

// runACPClaudeSession drives ONE claudeacp.Client session (the real npx
// claude-agent-acp bridge) through the same plain-then-tool-use turn
// pair, consuming c.Events() directly (this package's own capture rather
// than the go-agent-wrapper activity.Bridge, since acp.Client emits
// runtimeevents.Event directly with ID/Sequence/SessionID/Process left
// zero for the caller — see acp.Client.Events's doc comment).
func runACPClaudeSession(t *testing.T, plainPrompt, toolPrompt string) acpRunResult {
	t.Helper()
	toolMarker := extractMarkerFromToolPrompt(toolPrompt)

	c := claudeacp.NewClient()
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelLaunch()
	if err := c.Launch(launchCtx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Skipf("live claude-agent-acp Launch failed (environment issue, not necessarily a code defect): %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	var mu sync.Mutex
	var events []timedEvent
	collectDone := make(chan struct{})
	go func() {
		defer close(collectDone)
		for ev := range c.Events() {
			mu.Lock()
			events = append(events, timedEvent{at: time.Now(), ev: ev})
			mu.Unlock()
		}
	}()
	snapshot := func() []timedEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]timedEvent, len(events))
		copy(out, events)
		return out
	}

	var result acpRunResult

	// ----- turn 1: plain -----
	result.turn1Start = time.Now()
	promptCtx1, cancelPrompt1 := context.WithTimeout(context.Background(), 60*time.Second)
	if err := c.Prompt(promptCtx1, plainPrompt); err != nil {
		cancelPrompt1()
		t.Fatalf("ACP Prompt (turn 1): %v", err)
	}
	idx1, ev1 := waitForKindAfter(t, snapshot, runtimeevents.KindTurnCompleted, 0, 60*time.Second)
	cancelPrompt1()
	result.turn1Done = ev1.at
	for _, te := range snapshot()[:idx1] {
		if te.ev.Kind == runtimeevents.KindAgentDelta {
			if !result.turn1SawDelta {
				result.turn1FirstDelta = te.at
			}
			result.turn1SawDelta = true
			result.turn1Text += payloadString(te.ev.Payload, "content")
		}
	}

	// ----- turn 2: real tool use -----
	result.turn2Start = time.Now()
	promptCtx2, cancelPrompt2 := context.WithTimeout(context.Background(), 60*time.Second)
	if err := c.Prompt(promptCtx2, toolPrompt); err != nil {
		cancelPrompt2()
		t.Fatalf("ACP Prompt (turn 2): %v", err)
	}
	idx2, ev2 := waitForKindAfter(t, snapshot, runtimeevents.KindTurnCompleted, idx1+1, 60*time.Second)
	cancelPrompt2()
	result.turn2Done = ev2.at

	full := snapshot()
	result.turn2ToolUseCount = countKindsAfter(full, runtimeevents.KindAgentToolUse, idx1+1, idx2)
	result.turn2ToolResultCount = countKindsAfter(full, runtimeevents.KindAgentToolResult, idx1+1, idx2)
	for i := idx1 + 1; i <= idx2 && i < len(full); i++ {
		te := full[i]
		if te.ev.Kind == runtimeevents.KindAgentToolResult || te.ev.Kind == runtimeevents.KindAgentDelta {
			if strings.Contains(string(te.ev.Payload), toolMarker) {
				result.turn2SawMarker = true
			}
		}
	}

	_ = c.Close(context.Background())
	<-collectDone
	return result
}
