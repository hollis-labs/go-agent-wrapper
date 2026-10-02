package turnoutput

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider/events"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// feed builds runtimeevents for one session.
type feed struct {
	t   *testing.T
	r   *Reducer
	seq uint64
}

func newFeed(t *testing.T, cfg Config) *feed {
	t.Helper()
	return &feed{t: t, r: New(cfg)}
}

func (f *feed) event(kind runtimeevents.EventKind, turnID string, payload any) runtimeevents.Event {
	f.t.Helper()
	f.seq++
	ev := runtimeevents.Event{
		SchemaVersion: runtimeevents.SchemaVersion,
		ID:            fmt.Sprintf("evt_%d", f.seq),
		Kind:          kind,
		SessionID:     "ses_1",
		TurnID:        turnID,
		Sequence:      f.seq,
		Process:       runtimeevents.Process{Provider: "claude"},
	}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			f.t.Fatalf("marshal payload: %v", err)
		}
		ev.Payload = raw
	}
	return ev
}

// send feeds an event and returns the Output it produced, if any.
func (f *feed) send(ev runtimeevents.Event) (Output, bool) {
	f.t.Helper()
	return f.r.Observe(ev)
}

// mustQuiet feeds events that must not end a turn.
func (f *feed) mustQuiet(evs ...runtimeevents.Event) {
	f.t.Helper()
	for _, ev := range evs {
		if out, ok := f.r.Observe(ev); ok {
			f.t.Fatalf("%s ended the turn early: %+v", ev.Kind, out)
		}
	}
}

func (f *feed) delta(turnID, content, blockID, phase string) runtimeevents.Event {
	p := map[string]any{"content": content}
	if blockID != "" {
		p["block_id"] = blockID
	}
	if phase != "" {
		p["phase"] = phase
	}
	return f.event(runtimeevents.KindAgentDelta, turnID, p)
}

func (f *feed) tool(turnID, name string, input map[string]any) runtimeevents.Event {
	return f.event(runtimeevents.KindAgentToolUse, turnID, map[string]any{
		"tool_use": map[string]any{"id": "tu_1", "name": name, "input": input},
	})
}

func (f *feed) done(turnID string, payload map[string]any) runtimeevents.Event {
	return f.event(runtimeevents.KindTurnCompleted, turnID, payload)
}

func (f *feed) failed(turnID string, payload map[string]any) runtimeevents.Event {
	return f.event(runtimeevents.KindTurnFailed, turnID, payload)
}

func want(t *testing.T, got Output, ok bool, exp Output) {
	t.Helper()
	if !ok {
		t.Fatal("no Output at the terminal event")
	}
	if got != exp {
		t.Fatalf("Output =\n %+v\nwant\n %+v", got, exp)
	}
}

func TestACPLastMessageBlockIsHeuristic(t *testing.T) {
	f := newFeed(t, Config{})
	f.mustQuiet(
		f.event(runtimeevents.KindTurnStarted, "turn_1", nil),
		f.delta("turn_1", "Let me look at the repo.", "m1", "message"),
		f.delta("turn_1", "reasoning that must not leak", "m1", "thought"),
		f.tool("turn_1", "read_file", nil),
		f.delta("turn_1", "Found it. ", "m2", "message"),
		f.delta("turn_1", "The bug is in parse().", "m2", "message"),
	)
	got, ok := f.send(f.done("turn_1", map[string]any{"stop_reason": "end_turn"}))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
		Text: "Found it. The bug is in parse().", Kind: KindFinal,
		StopReason: "end_turn", Confidence: ConfidenceHeuristic,
	})
}

func TestFinalPhaseBlockIsExactAndBeatsALaterNarrationBlock(t *testing.T) {
	f := newFeed(t, Config{})
	f.mustQuiet(
		f.delta("turn_1", "thinking out loud", "a", "narration"),
		f.delta("turn_1", "Here is the answer.", "b", "final"),
		f.delta("turn_1", "trailing narration", "c", "narration"),
	)
	got, ok := f.send(f.done("turn_1", nil))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
		Text: "Here is the answer.", Kind: KindFinal, Confidence: ConfidenceExact,
	})
}

// A runtime's explicit result text (Claude's result.result) outranks a delta
// marked final: when both are present and differ, the terminal event's text is
// the answer. Both are exact; this pins which one wins.
func TestTerminalTextBeatsFinalPhase(t *testing.T) {
	f := newFeed(t, Config{})
	f.mustQuiet(f.delta("turn_1", "marked final", "b", "final"))
	got, ok := f.send(f.done("turn_1", map[string]any{"text": "  the result  ", "stop_reason": "end_turn"}))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
		Text: "the result", Kind: KindFinal, StopReason: "end_turn", Confidence: ConfidenceExact,
	})
}

func TestStreamDoneContentIsTheExactResult(t *testing.T) {
	r := New(Config{SessionID: "ses_1", Runtime: "claude", NewTurnID: func() string { return "turn_s" }})
	if _, ok := r.ObserveStream(llmtypes.StreamEvent{Type: llmtypes.EventDelta, Content: "narration", BlockID: "a"}); ok {
		t.Fatal("delta ended the turn")
	}
	got, ok := r.ObserveStream(llmtypes.StreamEvent{Type: llmtypes.EventDone, Content: "the result"})
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_s", Runtime: "claude",
		Text: "the result", Kind: KindFinal, Confidence: ConfidenceExact,
	})
}

func TestAnonymousDeltasJoinUntilAToolInterrupts(t *testing.T) {
	f := newFeed(t, Config{})
	f.mustQuiet(
		f.delta("turn_1", "Checking", "", ""),
		f.delta("turn_1", " the tests.", "", ""),
		f.event(runtimeevents.KindAgentToolUse, "turn_1", map[string]any{"title": "run tests", "kind": "execute"}),
		f.event(runtimeevents.KindAgentToolResult, "turn_1", map[string]any{"tool_result": map[string]any{"id": "tu_1"}}),
		f.delta("turn_1", "All ", "", ""),
		f.delta("turn_1", "green.", "", ""),
	)
	got, ok := f.send(f.done("turn_1", nil))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
		Text: "All green.", Kind: KindFinal, Confidence: ConfidenceHeuristic,
	})
}

func TestThinkingIsExcludedInEveryShape(t *testing.T) {
	f := newFeed(t, Config{})
	f.mustQuiet(
		f.delta("turn_1", "the answer", "a", ""),
		f.delta("turn_1", "thought text", "b", "thought"),
		f.delta("turn_1", "legacy thinking text", "c", "thinking"),
		f.event(runtimeevents.KindAgentDelta, "turn_1", map[string]any{
			"thinking": map[string]any{"Thinking": "signed block"}, "phase": "thought", "block_id": "d",
		}),
		f.event(runtimeevents.KindAgentDelta, "turn_1", map[string]any{"content": "old marker", "thinking": true}),
	)
	got, ok := f.send(f.done("turn_1", nil))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
		Text: "the answer", Kind: KindFinal, Confidence: ConfidenceHeuristic,
	})
}

func TestToolOnlyTurnHasEmptyText(t *testing.T) {
	f := newFeed(t, Config{})
	f.mustQuiet(f.tool("turn_1", "Bash", map[string]any{"command": "ls"}))
	got, ok := f.send(f.done("turn_1", map[string]any{"stop_reason": "tool_use"}))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
		Text: "", Kind: KindFinal, StopReason: "tool_use", Confidence: ConfidenceHeuristic,
	})
}

func TestFailureCarriesTheRuntimesError(t *testing.T) {
	f := newFeed(t, Config{})
	f.mustQuiet(f.delta("turn_1", "partial words", "a", ""))
	got, ok := f.send(f.failed("turn_1", map[string]any{"error": "rate limited", "stop_reason": "error"}))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
		Text: "rate limited", Kind: KindFailure, StopReason: "error", Confidence: ConfidenceExact,
	})
}

func TestFailureWithoutAMessageFallsBackToTheStopReason(t *testing.T) {
	f := newFeed(t, Config{})
	got, ok := f.send(f.failed("turn_1", nil))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
		Text: "", Kind: KindFailure, StopReason: "error", Confidence: ConfidenceHeuristic,
	})
}

func TestInterruptedAndExitedTurnsAreTerminal(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    Output
	}{
		{
			name:    "interrupted keeps partial output",
			payload: map[string]any{"error": "interrupted by user", "reason": "interrupted", "stop_reason": "error"},
			want:    Output{Text: "half an ans", Kind: KindTerminal, StopReason: llmtypes.StopReasonCancelled, Confidence: ConfidenceHeuristic},
		},
		{
			name:    "process exit keeps partial output",
			payload: map[string]any{"error": "wrapper: process exited before the turn completed", "reason": "process_exited", "stop_reason": "error"},
			want:    Output{Text: "half an ans", Kind: KindTerminal, StopReason: "error", Confidence: ConfidenceHeuristic},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFeed(t, Config{})
			f.mustQuiet(f.delta("turn_1", "half an ans", "a", ""))
			got, ok := f.send(f.failed("turn_1", tt.payload))
			exp := tt.want
			exp.SessionID, exp.TurnID, exp.Runtime = "ses_1", "turn_1", "claude"
			want(t, got, ok, exp)
		})
	}

	t.Run("without partial output the reason is the text", func(t *testing.T) {
		f := newFeed(t, Config{})
		got, ok := f.send(f.failed("turn_1", map[string]any{"reason": "interrupted"}))
		want(t, got, ok, Output{
			SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
			Text: "interrupted", Kind: KindTerminal, StopReason: llmtypes.StopReasonCancelled, Confidence: ConfidenceHeuristic,
		})
	})
}

func TestCancelledCompletionIsTerminal(t *testing.T) {
	f := newFeed(t, Config{})
	f.mustQuiet(f.delta("turn_1", "got partway", "m1", "message"))
	got, ok := f.send(f.done("turn_1", map[string]any{"stop_reason": llmtypes.StopReasonCancelled}))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
		Text: "got partway", Kind: KindTerminal, StopReason: llmtypes.StopReasonCancelled, Confidence: ConfidenceHeuristic,
	})
}

func TestQuestionToolBecomesAQuestion(t *testing.T) {
	input := map[string]any{"questions": []any{
		map[string]any{"question": "Which database?", "header": "DB"},
		map[string]any{"question": "Keep the old schema?"},
	}}

	t.Run("text is the question when the agent wrote nothing after it", func(t *testing.T) {
		f := newFeed(t, Config{})
		f.mustQuiet(f.delta("turn_1", "Before I start,", "a", ""), f.tool("turn_1", "AskUserQuestion", input))
		got, ok := f.send(f.done("turn_1", nil))
		want(t, got, ok, Output{
			SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
			Text: "Which database?\nKeep the old schema?", Kind: KindQuestion, Confidence: ConfidenceExact,
		})
	})

	t.Run("text is what the agent wrote after it", func(t *testing.T) {
		f := newFeed(t, Config{})
		f.mustQuiet(f.tool("turn_1", "askuserquestion", input), f.delta("turn_1", "Which database should I use?", "b", ""))
		got, ok := f.send(f.done("turn_1", nil))
		want(t, got, ok, Output{
			SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
			Text: "Which database should I use?", Kind: KindQuestion, Confidence: ConfidenceHeuristic,
		})
	})

	t.Run("a custom tool list replaces the default", func(t *testing.T) {
		f := newFeed(t, Config{QuestionTools: []string{"ask_human"}})
		f.mustQuiet(f.tool("turn_1", "AskUserQuestion", input), f.tool("turn_1", "ask_human", map[string]any{"question": "Proceed?"}))
		got, ok := f.send(f.done("turn_1", nil))
		want(t, got, ok, Output{
			SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
			Text: "Proceed?", Kind: KindQuestion, Confidence: ConfidenceExact,
		})
	})

	t.Run("a failed turn is a failure, not a question", func(t *testing.T) {
		f := newFeed(t, Config{})
		f.mustQuiet(f.tool("turn_1", "AskUserQuestion", input))
		got, ok := f.send(f.failed("turn_1", map[string]any{"error": "boom", "stop_reason": "error"}))
		if !ok || got.Kind != KindFailure {
			t.Fatalf("got %+v, want a failure", got)
		}
	})
}

// An ACP agent's tool call carries its tool in "title" and its arguments in
// "raw_input", not the native tool_use object.
func TestACPToolCallCanBeAQuestion(t *testing.T) {
	f := newFeed(t, Config{})
	f.mustQuiet(f.event(runtimeevents.KindAgentToolUse, "turn_1", map[string]any{
		"tool_call_id": "tc_1", "title": "AskUserQuestion", "kind": "other", "status": "pending",
		"raw_input": map[string]any{"questions": []any{map[string]any{"question": "Proceed?"}}},
	}))
	got, ok := f.send(f.done("turn_1", nil))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
		Text: "Proceed?", Kind: KindQuestion, Confidence: ConfidenceExact,
	})
}

func TestCodexRequestUserInputIsAQuestion(t *testing.T) {
	f := newFeed(t, Config{})
	params := map[string]any{"questions": []any{map[string]any{"id": "q1", "question": "Which branch?"}}}
	req := f.event(runtimeevents.KindAgentPermissionRequested, "turn_1", map[string]any{
		"method": "item/tool/requestUserInput", "params": params,
	})
	res := f.event(runtimeevents.KindAgentPermissionResolved, "turn_1", map[string]any{
		"method": "item/tool/requestUserInput", "allowed": false,
	})
	res.ParentID = req.ID
	f.mustQuiet(req, res)
	got, ok := f.send(f.done("turn_1", nil))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
		Text: "Which branch?", Kind: KindQuestion, Confidence: ConfidenceExact,
	})
}

func TestApprovals(t *testing.T) {
	t.Run("a request still open at the end", func(t *testing.T) {
		f := newFeed(t, Config{})
		f.mustQuiet(f.event(runtimeevents.KindAgentPermissionRequested, "turn_1", map[string]any{
			"request_id": 7, "method": "session/request_permission",
			"params": map[string]any{"toolCall": map[string]any{"title": "rm -rf build"}},
		}))
		got, ok := f.send(f.done("turn_1", nil))
		want(t, got, ok, Output{
			SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
			Text: "Approval requested: rm -rf build", Kind: KindApproval, Confidence: ConfidenceExact,
		})
	})

	t.Run("an allowed request is not an approval", func(t *testing.T) {
		f := newFeed(t, Config{})
		req := f.event(runtimeevents.KindAgentPermissionRequested, "turn_1", map[string]any{"request_id": "r1", "method": "m"})
		res := f.event(runtimeevents.KindAgentPermissionResolved, "turn_1", map[string]any{"request_id": "r1", "allowed": true})
		f.mustQuiet(req, res, f.delta("turn_1", "done", "a", ""))
		got, ok := f.send(f.done("turn_1", nil))
		want(t, got, ok, Output{
			SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
			Text: "done", Kind: KindFinal, Confidence: ConfidenceHeuristic,
		})
	})

	t.Run("a refused request, matched by parent id", func(t *testing.T) {
		f := newFeed(t, Config{})
		req := f.event(runtimeevents.KindAgentPermissionRequested, "turn_1", map[string]any{
			"method": "item/commandExecution/requestApproval", "params": map[string]any{"command": "make deploy"},
		})
		res := f.event(runtimeevents.KindAgentPermissionResolved, "turn_1", map[string]any{"allowed": false, "reason": "posture"})
		res.ParentID = req.ID
		f.mustQuiet(req, res, f.delta("turn_1", "I could not deploy.", "a", ""))
		got, ok := f.send(f.done("turn_1", nil))
		want(t, got, ok, Output{
			SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
			Text: "I could not deploy.", Kind: KindApproval, Confidence: ConfidenceHeuristic,
		})
	})

	t.Run("a headless refusal", func(t *testing.T) {
		f := newFeed(t, Config{})
		f.mustQuiet(f.event(runtimeevents.KindAgentPermissionDenied, "turn_1", map[string]any{"action": "bash", "display_name": "git push"}))
		got, ok := f.send(f.done("turn_1", nil))
		want(t, got, ok, Output{
			SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
			Text: "Permission denied: git push", Kind: KindApproval, Confidence: ConfidenceExact,
		})
	})

	t.Run("a question beats an approval", func(t *testing.T) {
		f := newFeed(t, Config{})
		f.mustQuiet(
			f.event(runtimeevents.KindAgentPermissionDenied, "turn_1", map[string]any{"action": "bash"}),
			f.tool("turn_1", "AskUserQuestion", map[string]any{"question": "Proceed?"}),
		)
		got, ok := f.send(f.done("turn_1", nil))
		if !ok || got.Kind != KindQuestion {
			t.Fatalf("got %+v, want a question", got)
		}
	})
}

func TestOneOutputPerTurn(t *testing.T) {
	f := newFeed(t, Config{})
	f.mustQuiet(f.delta("turn_1", "first", "a", ""))
	if _, ok := f.send(f.done("turn_1", nil)); !ok {
		t.Fatal("no Output for turn_1")
	}
	if out, ok := f.send(f.done("turn_1", nil)); ok {
		t.Fatalf("a repeated terminal event produced a second Output: %+v", out)
	}

	f.mustQuiet(f.delta("turn_2", "second", "a", ""))
	got, ok := f.send(f.done("turn_2", nil))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_2", Runtime: "claude",
		Text: "second", Kind: KindFinal, Confidence: ConfidenceHeuristic,
	})
}

func TestAnUnfinishedTurnDoesNotAffectTheNextOne(t *testing.T) {
	f := newFeed(t, Config{})
	f.mustQuiet(f.delta("turn_1", "never finished", "a", ""), f.delta("turn_2", "the real one", "a", ""))
	got, ok := f.send(f.done("turn_2", nil))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_2", Runtime: "claude",
		Text: "the real one", Kind: KindFinal, Confidence: ConfidenceHeuristic,
	})
}

func TestTerminalEventWithNoOpenTurnStillReports(t *testing.T) {
	f := newFeed(t, Config{})
	got, ok := f.send(f.failed("turn_9", map[string]any{"error": "auth failed"}))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_9", Runtime: "claude",
		Text: "auth failed", Kind: KindFailure, StopReason: "error", Confidence: ConfidenceExact,
	})
}

func TestEventsWithoutTurnIDsJoinTheTurnInProgress(t *testing.T) {
	f := newFeed(t, Config{NewTurnID: func() string { return "turn_minted" }})
	f.mustQuiet(f.delta("", "no turn id", "a", ""))
	got, ok := f.send(f.done("", nil))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_minted", Runtime: "claude",
		Text: "no turn id", Kind: KindFinal, Confidence: ConfidenceHeuristic,
	})
}

func TestConfigOverridesTheEnvelope(t *testing.T) {
	f := newFeed(t, Config{SessionID: "ses_cfg", Runtime: "codex"})
	f.mustQuiet(f.delta("turn_1", "hi", "a", ""))
	got, ok := f.send(f.done("turn_1", nil))
	if !ok || got.SessionID != "ses_cfg" || got.Runtime != "codex" {
		t.Fatalf("got %+v, want session ses_cfg and runtime codex", got)
	}
}

func TestProcessExitWithAnOpenTurnIsTerminal(t *testing.T) {
	f := newFeed(t, Config{})
	f.mustQuiet(f.delta("turn_1", "mid-sentence", "a", ""))
	got, ok := f.send(f.event(runtimeevents.KindProcessExited, "", map[string]any{"exit_code": 1}))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
		Text: "mid-sentence", Kind: KindTerminal, StopReason: "error", Confidence: ConfidenceHeuristic,
	})
	if out, ok := f.send(f.event(runtimeevents.KindProcessExited, "", nil)); ok {
		t.Fatalf("a second exit produced %+v", out)
	}
}

func TestFlush(t *testing.T) {
	r := New(Config{SessionID: "ses_1", Runtime: "opencode", NewTurnID: func() string { return "turn_f" }})
	if _, ok := r.Flush("process_exited"); ok {
		t.Fatal("Flush with no turn in progress produced an Output")
	}
	r.ObserveProvider(events.Delta{Text: "unfinished", BlockID: "a"})
	got, ok := r.Flush("")
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_f", Runtime: "opencode",
		Text: "unfinished", Kind: KindTerminal, StopReason: "error", Confidence: ConfidenceHeuristic,
	})
}

func TestProviderFeedMintsOneTurnPerTerminal(t *testing.T) {
	n := 0
	r := New(Config{SessionID: "ses_1", Runtime: "agy", NewTurnID: func() string { n++; return fmt.Sprintf("turn_%d", n) }})

	for i, text := range []string{"first answer", "second answer"} {
		steps := []events.Event{
			events.SessionID{ID: "conv"},
			events.Delta{Text: "working", BlockID: "step-1"},
			events.ToolUse{ID: "step-2", Name: "Bash", Args: map[string]any{"command": "ls"}},
			events.ToolResult{ID: "step-2"},
			events.Delta{Text: text, BlockID: "step-3"},
			events.Usage{StopReason: "end_turn"},
		}
		for _, ev := range steps {
			if out, ok := r.ObserveProvider(ev); ok {
				t.Fatalf("%T ended the turn: %+v", ev, out)
			}
		}
		got, ok := r.ObserveProvider(events.Done{})
		want(t, got, ok, Output{
			SessionID: "ses_1", TurnID: fmt.Sprintf("turn_%d", i+1), Runtime: "agy",
			Text: text, Kind: KindFinal, StopReason: "end_turn", Confidence: ConfidenceHeuristic,
		})
	}
}

func TestProviderFeedFinalPhaseAndErrors(t *testing.T) {
	r := New(Config{SessionID: "ses_1", Runtime: "codex", NewTurnID: func() string { return "turn_p" }})
	r.ObserveProvider(events.Delta{Text: "narration", Phase: "narration", BlockID: "a"})
	r.ObserveProvider(events.Thinking{Text: "hidden"})
	r.ObserveProvider(events.Delta{Text: "the answer", Phase: "final", BlockID: "b"})
	got, ok := r.ObserveProvider(events.Done{StopReason: "end_turn"})
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_p", Runtime: "codex",
		Text: "the answer", Kind: KindFinal, StopReason: "end_turn", Confidence: ConfidenceExact,
	})

	r.ObserveProvider(events.Delta{Text: "oops"})
	got, ok = r.ObserveProvider(events.Error{Err: errors.New("provider exploded")})
	if !ok || got.Kind != KindFailure || got.Text != "provider exploded" || got.TurnID != "turn_p" {
		t.Fatalf("got %+v, want a failure carrying the error", got)
	}
}

func TestProviderFeedIgnoresNoiseOutsideATurn(t *testing.T) {
	r := New(Config{SessionID: "ses_1", Runtime: "claude"})
	for _, ev := range []events.Event{events.SessionID{ID: "x"}, events.Heartbeat{}, events.AuthFailed{Message: "no"}} {
		if out, ok := r.ObserveProvider(ev); ok {
			t.Fatalf("%T produced %+v", ev, out)
		}
	}
	if _, ok := r.Flush(""); ok {
		t.Fatal("noise opened a turn")
	}
}

func TestStreamFeedPermissionDeniedAndQuestion(t *testing.T) {
	r := New(Config{SessionID: "ses_1", Runtime: "claude", NewTurnID: func() string { return "turn_s" }})
	r.ObserveStream(llmtypes.StreamEvent{Type: llmtypes.EventToolUse, ToolUse: &llmtypes.ToolUseBlock{
		Name: "AskUserQuestion", Input: map[string]any{"question": "Ready?"},
	}})
	r.ObserveStream(llmtypes.StreamEvent{Type: llmtypes.EventUsage, Usage: &llmtypes.Usage{StopReason: "end_turn"}})
	got, ok := r.ObserveStream(llmtypes.StreamEvent{Type: llmtypes.EventDone})
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_s", Runtime: "claude",
		Text: "Ready?", Kind: KindQuestion, StopReason: "end_turn", Confidence: ConfidenceExact,
	})

	got, ok = r.ObserveStream(llmtypes.StreamEvent{Type: llmtypes.EventError, Error: "turn failed"})
	if !ok || got.Kind != KindFailure || got.Text != "turn failed" {
		t.Fatalf("got %+v, want a failure", got)
	}
}

func TestOutputJSONFieldNames(t *testing.T) {
	raw, err := json.Marshal(Output{
		SessionID: "s", TurnID: "t", Text: "x", Kind: KindFinal,
		StopReason: "end_turn", Runtime: "claude", Confidence: ConfidenceExact,
	})
	if err != nil {
		t.Fatal(err)
	}
	const wantJSON = `{"session_id":"s","turn_id":"t","text":"x","kind":"final","stop_reason":"end_turn","runtime":"claude","confidence":"exact"}`
	if string(raw) != wantJSON {
		t.Fatalf("json = %s\nwant   %s", raw, wantJSON)
	}
}

func TestConcurrentFeedsReportEachTurnOnce(t *testing.T) {
	r := New(Config{SessionID: "ses_1", Runtime: "claude"})
	const turns = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]int{}
	for i := range turns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("turn_%d", i)
			payload, _ := json.Marshal(map[string]any{"content": "t" + id, "block_id": "a"})
			r.Observe(runtimeevents.Event{Kind: runtimeevents.KindAgentDelta, TurnID: id, Payload: payload})
			if out, ok := r.Observe(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: id}); ok {
				mu.Lock()
				seen[out.TurnID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s reported %d times", id, n)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no turn reported")
	}
}
