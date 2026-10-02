package turnoutput

import (
	"fmt"
	"strings"
	"testing"

	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider/events"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// A turn is a question or an approval only if it ended on the signal. In headless
// mode every runtime refuses automatically and the agent carries on, so the
// reducer reads the runtime's own record of a refusal (a PermissionDenied naming
// the call, a refused request, a question tool call) and asks whether the agent
// kept working past it (CW-20261002-0073).

func provider(t *testing.T) *Reducer {
	t.Helper()
	n := 0
	return New(Config{SessionID: "s", Runtime: "claude", NewTurnID: func() string { n++; return fmt.Sprintf("turn_%d", n) }})
}

func finishWith(t *testing.T, r *Reducer, done events.Done, steps ...events.Event) Output {
	t.Helper()
	for _, ev := range steps {
		if out, ok := r.ObserveProvider(ev); ok {
			t.Fatalf("%T ended the turn early: %+v", ev, out)
		}
	}
	out, ok := r.ObserveProvider(done)
	if !ok {
		t.Fatal("no Output at the done")
	}
	return out
}

func TestARefusedToolCallTheTurnEndedOnIsAnApproval(t *testing.T) {
	// Claude's shape: the call, its error result, the agent's message asking for
	// the approval, then the refusal the CLI recorded, then the done with the
	// result text.
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn", Text: "I need your approval to run it. Should I proceed?"},
		events.ToolUse{ID: "t1", Name: "Bash", Args: map[string]any{"command": "make deploy"}},
		events.ToolResult{ID: "t1", IsError: true},
		events.Delta{Text: "I need your approval to run it. Should I proceed?", BlockID: "m1"},
		events.PermissionDenied{Action: "Bash", DisplayName: "make deploy", ToolUseID: "t1"},
	)
	if out.Kind != KindApproval || out.Text != "I need your approval to run it. Should I proceed?" || out.Confidence != ConfidenceExact {
		t.Fatalf("got %+v, want an approval carrying the runtime's own final text, exact", out)
	}
}

func TestARefusalTheAgentWorkedAroundIsNotAnApproval(t *testing.T) {
	tests := []struct {
		name  string
		steps []events.Event
	}{
		{"a different tool afterwards", []events.Event{
			events.ToolUse{ID: "t1", Name: "Bash"}, events.ToolResult{ID: "t1", IsError: true},
			events.ToolUse{ID: "t2", Name: "Write"}, events.ToolResult{ID: "t2"},
			events.Delta{Text: "Done another way.", BlockID: "m1"},
			events.PermissionDenied{Action: "Bash", DisplayName: "make deploy", ToolUseID: "t1"},
		}},
		{"a refusal followed by work, then another refusal", []events.Event{
			events.ToolUse{ID: "t1", Name: "Bash"}, events.ToolResult{ID: "t1", IsError: true},
			events.ToolUse{ID: "t2", Name: "Read"},
			events.ToolUse{ID: "t3", Name: "Bash"}, events.ToolResult{ID: "t3", IsError: true},
			events.Delta{Text: "Skipped the commands.", BlockID: "m1"},
			events.PermissionDenied{Action: "Bash", DisplayName: "a", ToolUseID: "t1"},
			events.PermissionDenied{Action: "Bash", DisplayName: "b", ToolUseID: "t3"},
		}},
	}
	// The second case ends on a refusal (t3), so it is an approval; the first is not.
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"}, tests[0].steps...)
	if out.Kind != KindFinal {
		t.Fatalf("%s: got %+v, want a final turn", tests[0].name, out)
	}
	out = finishWith(t, provider(t), events.Done{StopReason: "end_turn"}, tests[1].steps...)
	if out.Kind != KindApproval {
		t.Fatalf("%s: got %+v, want an approval: the turn ended on the second refusal", tests[1].name, out)
	}
}

func TestTwoRefusalsAreOneApproval(t *testing.T) {
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"},
		events.ToolUse{ID: "t1", Name: "Bash"}, events.ToolUse{ID: "t2", Name: "Write"},
		events.Delta{Text: "Neither ran.", BlockID: "m1"},
		events.PermissionDenied{Action: "Bash", DisplayName: "a", ToolUseID: "t1"},
		events.PermissionDenied{Action: "Write", DisplayName: "b", ToolUseID: "t2"},
	)
	if out.Kind != KindApproval || out.Text != "Neither ran." {
		t.Fatalf("got %+v, want an approval with the agent's message", out)
	}
}

// With no text after the refusal the signal speaks for itself, never the text the
// agent wrote before it.
func TestARefusalWithNoTextAfterItSpeaksForItself(t *testing.T) {
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn", Text: "Let me run it."},
		events.Delta{Text: "Let me run it.", BlockID: "m1"},
		events.ToolUse{ID: "t1", Name: "Bash"},
		events.PermissionDenied{Action: "Bash", DisplayName: "make deploy", ToolUseID: "t1"},
	)
	if out.Kind != KindApproval || out.Text != "Permission denied: make deploy" || out.Confidence != ConfidenceExact {
		t.Fatalf("got %+v, want the refusal's own description, not the text before it", out)
	}
}

// A refusal with no call id (agy reports denied_actions with none, at the end of
// the turn) cannot be placed, so it stands for the last thing the agent did.
func TestARefusalWithNoCallIDStandsForTheLastToolCall(t *testing.T) {
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"},
		events.ToolUse{ID: "step-2", Name: "run_command"},
		events.PermissionDenied{Action: "command", DisplayName: "RunCommand"},
	)
	if out.Kind != KindApproval || out.Text != "Permission denied: RunCommand" {
		t.Fatalf("got %+v, want an approval", out)
	}
}

func TestQuestionTools(t *testing.T) {
	ask := events.ToolUse{ID: "q1", Name: "AskUserQuestion", Args: map[string]any{"questions": []any{map[string]any{"question": "Which database?"}}}}

	t.Run("refused and the turn ended on it", func(t *testing.T) {
		out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"}, ask, events.ToolResult{ID: "q1", IsError: true})
		if out.Kind != KindQuestion || out.Text != "Which database?" || out.Confidence != ConfidenceExact {
			t.Fatalf("got %+v, want a question with the question's own text", out)
		}
	})

	t.Run("refused, then the agent says what it needs", func(t *testing.T) {
		out := finishWith(t, provider(t), events.Done{StopReason: "end_turn", Text: "Tell me which database to use."}, ask, events.ToolResult{ID: "q1", IsError: true},
			events.Delta{Text: "Tell me which database to use.", BlockID: "m1"})
		if out.Kind != KindQuestion || out.Text != "Tell me which database to use." || out.Confidence != ConfidenceExact {
			t.Fatalf("got %+v, want a question with the runtime's final text", out)
		}
	})

	t.Run("refused and the agent went on to do the work", func(t *testing.T) {
		out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"}, ask, events.ToolResult{ID: "q1", IsError: true},
			events.ToolUse{ID: "t2", Name: "Write"}, events.ToolResult{ID: "t2"},
			events.Delta{Text: "No answer, so I chose Postgres and implemented it.", BlockID: "m1"})
		if out.Kind != KindFinal || !strings.Contains(out.Text, "chose Postgres") {
			t.Fatalf("got %+v, want a final turn: the agent resolved the question itself", out)
		}
	})

	t.Run("answered", func(t *testing.T) {
		out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"}, ask, events.ToolResult{ID: "q1"},
			events.Delta{Text: "Postgres it is.", BlockID: "m1"})
		if out.Kind != KindFinal {
			t.Fatalf("got %+v, want a final turn: the question came back answered", out)
		}
	})

	t.Run("no result at all", func(t *testing.T) {
		out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"}, ask)
		if out.Kind != KindQuestion {
			t.Fatalf("got %+v, want a question: nothing says it was answered", out)
		}
	})

	t.Run("a second question after the first is still the question", func(t *testing.T) {
		out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"}, ask, events.ToolResult{ID: "q1", IsError: true},
			events.ToolUse{ID: "q2", Name: "AskUserQuestion", Args: map[string]any{"question": "And the schema?"}})
		if out.Kind != KindQuestion || out.Text != "And the schema?" {
			t.Fatalf("got %+v, want the last question", out)
		}
	})
}

// A refused call is not the agent working past what it asked: after a question,
// a refused command leaves the turn on the question, which outranks the approval.
func TestARefusedCallAfterAQuestionIsNotWork(t *testing.T) {
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"},
		events.ToolUse{ID: "q1", Name: "AskUserQuestion", Args: map[string]any{"question": "Which database?"}},
		events.ToolResult{ID: "q1", IsError: true},
		events.ToolUse{ID: "t2", Name: "Bash"},
		events.ToolResult{ID: "t2", IsError: true},
		events.PermissionDenied{Action: "Bash", DisplayName: "createdb", ToolUseID: "t2"},
	)
	if out.Kind != KindQuestion || out.Text != "Which database?" {
		t.Fatalf("got %+v, want the question: the only call after it was refused", out)
	}

	// The same command, not refused, is work: the agent went on.
	out = finishWith(t, provider(t), events.Done{StopReason: "end_turn"},
		events.ToolUse{ID: "q1", Name: "AskUserQuestion", Args: map[string]any{"question": "Which database?"}},
		events.ToolResult{ID: "q1", IsError: true},
		events.ToolUse{ID: "t2", Name: "Bash"},
		events.ToolResult{ID: "t2"},
	)
	if out.Kind != KindFinal {
		t.Fatalf("got %+v, want a final turn: the agent ran the command", out)
	}
}

func TestARefusedPermissionRequest(t *testing.T) {
	f := func(t *testing.T) *feed { return newFeed(t, Config{}) }

	t.Run("refused and the turn ended on it", func(t *testing.T) {
		f := f(t)
		req := f.event(runtimeevents.KindAgentPermissionRequested, "turn_1", map[string]any{"request_id": 1, "method": "m", "params": map[string]any{"toolCall": map[string]any{"title": "rm -rf build"}}})
		res := f.event(runtimeevents.KindAgentPermissionResolved, "turn_1", map[string]any{"request_id": 1, "allowed": false})
		f.mustQuiet(f.tool("turn_1", "Bash", nil), req, res, f.delta("turn_1", "I could not remove it.", "a", ""))
		got, ok := f.send(f.done("turn_1", nil))
		want(t, got, ok, Output{SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude", Text: "I could not remove it.", Kind: KindApproval, Confidence: ConfidenceHeuristic})
	})

	t.Run("refused and the agent worked around it", func(t *testing.T) {
		f := f(t)
		req := f.event(runtimeevents.KindAgentPermissionRequested, "turn_1", map[string]any{"request_id": 1, "method": "m"})
		res := f.event(runtimeevents.KindAgentPermissionResolved, "turn_1", map[string]any{"request_id": 1, "allowed": false})
		f.mustQuiet(f.tool("turn_1", "Bash", nil), req, res, f.tool("turn_1", "Read", nil), f.delta("turn_1", "Read it instead.", "a", ""))
		got, ok := f.send(f.done("turn_1", nil))
		if !ok || got.Kind != KindFinal {
			t.Fatalf("got %+v, want a final turn", got)
		}
	})

	t.Run("allowed", func(t *testing.T) {
		f := f(t)
		req := f.event(runtimeevents.KindAgentPermissionRequested, "turn_1", map[string]any{"request_id": 1, "method": "m"})
		res := f.event(runtimeevents.KindAgentPermissionResolved, "turn_1", map[string]any{"request_id": 1, "allowed": true})
		f.mustQuiet(f.tool("turn_1", "Bash", nil), req, res, f.delta("turn_1", "Done.", "a", ""))
		got, ok := f.send(f.done("turn_1", nil))
		if !ok || got.Kind != KindFinal {
			t.Fatalf("got %+v, want a final turn", got)
		}
	})

	t.Run("still unanswered when the turn ended", func(t *testing.T) {
		f := f(t)
		f.mustQuiet(f.tool("turn_1", "Bash", nil), f.event(runtimeevents.KindAgentPermissionRequested, "turn_1", map[string]any{"request_id": 1, "method": "m", "params": map[string]any{"toolCall": map[string]any{"title": "rm -rf build"}}}))
		got, ok := f.send(f.done("turn_1", nil))
		want(t, got, ok, Output{SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude", Text: "Approval requested: rm -rf build", Kind: KindApproval, Confidence: ConfidenceExact})
	})
}

// The wrapper path: an agent.permission_denied that names the refused call, and the
// tool call's id and result, place the refusal the same way.
func TestRuntimeEventsPlaceARefusalByItsToolCall(t *testing.T) {
	denied := func(f *feed, id string) runtimeevents.Event {
		return f.event(runtimeevents.KindAgentPermissionDenied, "turn_1", map[string]any{"action": "Bash", "display_name": "make deploy", "tool_use_id": id})
	}
	t.Run("ended on it", func(t *testing.T) {
		f := newFeed(t, Config{})
		f.mustQuiet(f.tool("turn_1", "Bash", nil), f.delta("turn_1", "Needs approval.", "a", ""), denied(f, "tu_1"))
		got, ok := f.send(f.done("turn_1", map[string]any{"text": "Needs approval."}))
		want(t, got, ok, Output{SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude", Text: "Needs approval.", Kind: KindApproval, Confidence: ConfidenceExact})
	})
	t.Run("worked around", func(t *testing.T) {
		f := newFeed(t, Config{})
		other := f.event(runtimeevents.KindAgentToolUse, "turn_1", map[string]any{"tool_use": map[string]any{"id": "tu_2", "name": "Read"}})
		f.mustQuiet(f.tool("turn_1", "Bash", nil), other, f.delta("turn_1", "Used Read.", "a", ""), denied(f, "tu_1"))
		got, ok := f.send(f.done("turn_1", nil))
		if !ok || got.Kind != KindFinal {
			t.Fatalf("got %+v, want a final turn", got)
		}
	})
	t.Run("a question answered, by the tool result", func(t *testing.T) {
		f := newFeed(t, Config{})
		ask := f.event(runtimeevents.KindAgentToolUse, "turn_1", map[string]any{"tool_use": map[string]any{"id": "q1", "name": "AskUserQuestion", "input": map[string]any{"question": "Which?"}}})
		result := f.event(runtimeevents.KindAgentToolResult, "turn_1", map[string]any{"tool_result": map[string]any{"id": "q1", "is_error": false}})
		f.mustQuiet(ask, result, f.delta("turn_1", "Going with it.", "a", ""))
		got, ok := f.send(f.done("turn_1", nil))
		if !ok || got.Kind != KindFinal {
			t.Fatalf("got %+v, want a final turn", got)
		}
	})
}

// The bounds on what a turn remembers about its tool calls and signals.
func TestSignalMemoryIsBounded(t *testing.T) {
	r := provider(t)
	for i := range maxTools * 2 {
		r.ObserveProvider(events.ToolUse{ID: fmt.Sprintf("t%d", i), Name: "Read"})
	}
	if n := len(r.current().tools); n != maxTools {
		t.Fatalf("holds %d tool calls, want %d", n, maxTools)
	}
	for i := range maxSignals * 2 {
		r.ObserveProvider(events.PermissionDenied{Action: "a", DisplayName: fmt.Sprint(i), ToolUseID: fmt.Sprintf("t%d", maxTools*2-1)})
	}
	if n := len(r.current().signals); n != maxSignals {
		t.Fatalf("holds %d signals, want %d", n, maxSignals)
	}
}

// ---- Review of #41 --------------------------------------------------------

// The refusal, the tool result and the tool call they name reach the reducer on
// different paths and in either order; the answer must not depend on which.
func TestTheOrderTheEventsArriveInDoesNotChangeTheKind(t *testing.T) {
	use := func(id string) events.Event { return events.ToolUse{ID: id, Name: "Bash"} }
	result := func(id string, isErr bool) events.Event { return events.ToolResult{ID: id, IsError: isErr} }
	denial := func(id string) events.Event {
		return events.PermissionDenied{Action: "Bash", DisplayName: "make deploy", ToolUseID: id}
	}
	text := events.Delta{Text: "I need your approval.", BlockID: "m1"}

	// Each case is one story told in the order it happened and with the refusal and
	// result ahead of the call they name (the order the wrapper used to deliver).
	tests := []struct {
		name string
		want Kind
		// inOrder and reordered carry the same events.
		inOrder, reordered []events.Event
	}{
		{"ended on the refusal", KindApproval,
			[]events.Event{use("t1"), result("t1", true), text, denial("t1")},
			[]events.Event{denial("t1"), result("t1", true), use("t1"), text}},
		{"worked around it", KindFinal,
			[]events.Event{use("t1"), result("t1", true), use("t2"), result("t2", false), text, denial("t1")},
			[]events.Event{denial("t1"), result("t1", true), result("t2", false), use("t1"), use("t2"), text}},
	}
	for _, tt := range tests {
		for name, steps := range map[string][]events.Event{"in order": tt.inOrder, "typed events first": tt.reordered} {
			t.Run(tt.name+"/"+name, func(t *testing.T) {
				out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"}, steps...)
				if out.Kind != tt.want {
					t.Fatalf("got %+v, want kind %s", out, tt.want)
				}
			})
		}
	}
}

// A call issued in the same batch as the refused one, before its result came back,
// was not a reaction to the refusal.
func TestAParallelCallInTheSameBatchIsNotWorkingPastARefusal(t *testing.T) {
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"},
		events.ToolUse{ID: "t1", Name: "Bash"},
		events.ToolUse{ID: "t2", Name: "Read"},
		events.ToolResult{ID: "t1", IsError: true},
		events.ToolResult{ID: "t2"},
		events.Delta{Text: "I need your approval to run it.", BlockID: "m1"},
		events.PermissionDenied{Action: "Bash", DisplayName: "make deploy", ToolUseID: "t1"},
	)
	if out.Kind != KindApproval || out.Text != "I need your approval to run it." {
		t.Fatalf("got %+v, want an approval: the Read was issued with the refused call", out)
	}

	// A call that started after the refused call's result is a reaction to it.
	out = finishWith(t, provider(t), events.Done{StopReason: "end_turn"},
		events.ToolUse{ID: "t1", Name: "Bash"},
		events.ToolResult{ID: "t1", IsError: true},
		events.ToolUse{ID: "t3", Name: "Read"},
		events.ToolResult{ID: "t3"},
		events.Delta{Text: "Read it instead.", BlockID: "m1"},
		events.PermissionDenied{Action: "Bash", DisplayName: "make deploy", ToolUseID: "t1"},
	)
	if out.Kind != KindFinal {
		t.Fatalf("got %+v, want a final turn", out)
	}
}

// The result that counts is the first: an ACP agent updates a call several times.
func TestAToolsFirstResultIsTheOneThatCounts(t *testing.T) {
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"},
		events.ToolUse{ID: "t1", Name: "Bash"},
		events.ToolResult{ID: "t1", IsError: true},
		events.ToolUse{ID: "t2", Name: "Read"},
		events.ToolResult{ID: "t2"},
		events.ToolResult{ID: "t1", IsError: false}, // a later update of the refused call
		events.Delta{Text: "Read it instead.", BlockID: "m1"},
		events.PermissionDenied{Action: "Bash", DisplayName: "x", ToolUseID: "t1"},
	)
	if out.Kind != KindFinal {
		t.Fatalf("got %+v, want a final turn: t2 started after t1's first result", out)
	}
}

// agy names no call and reports its refusals when the turn ends, so the agent's own
// message after its last tool call is text after the signal, not before it. Built
// from the real adapter against the captured turn, with the agent's message spliced
// in after the refused step.
func TestAnUnnamedRefusalKeepsTheAgentsMessageAfterTheLastCall(t *testing.T) {
	r := New(Config{SessionID: "s", Runtime: "agy", NewTurnID: func() string { return "turn_a" }})
	var out Output
	spliced := false
	for _, line := range agyFixtureLines(t, "antigravity/print_tool_denied.jsonl") {
		parsed, err := newAgy().ParseLineEvents(line)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range parsed {
			if _, isDenial := ev.(events.PermissionDenied); isDenial && !spliced {
				// The refused call came first; the agent says what it needs after it.
				r.ObserveProvider(events.Delta{Text: "I could not run that command. May I?", BlockID: "step-9"})
				spliced = true
			}
			if o, ok := r.ObserveProvider(ev); ok {
				out = o
			}
		}
	}
	if !spliced {
		t.Fatal("the capture reported no refusal")
	}
	if out.Kind != KindApproval || out.Text != "I could not run that command. May I?" {
		t.Fatalf("got %+v, want an approval carrying the agent's own message", out)
	}
}

// A refusal that names a call the turn has dropped for age is older than anything
// it kept: 299 calls that worked after it are the agent working past it.
func TestARefusalOfADroppedCallIsOlderThanWhatWasKept(t *testing.T) {
	r := provider(t)
	r.ObserveProvider(events.ToolUse{ID: "t0", Name: "Bash"})
	r.ObserveProvider(events.ToolResult{ID: "t0", IsError: true})
	for i := 1; i < 300; i++ {
		r.ObserveProvider(events.ToolUse{ID: fmt.Sprintf("r%d", i), Name: "Read"})
		r.ObserveProvider(events.ToolResult{ID: fmt.Sprintf("r%d", i)})
	}
	r.ObserveProvider(events.Delta{Text: "All read.", BlockID: "m1"})
	r.ObserveProvider(events.PermissionDenied{Action: "Bash", DisplayName: "x", ToolUseID: "t0"})
	out, ok := r.ObserveProvider(events.Done{StopReason: "end_turn"})
	if !ok || out.Kind != KindFinal {
		t.Fatalf("got %+v, want a final turn", out)
	}
}

// Codex asks a question by request, which is no tool call of its own: it sits after
// every call the turn had, and an earlier call's result says nothing about it.
func TestARequestedQuestionSitsAfterEveryEarlierCall(t *testing.T) {
	f := newFeed(t, Config{})
	bash := f.tool("turn_1", "Bash", nil)
	result := f.event(runtimeevents.KindAgentToolResult, "turn_1", map[string]any{"tool_result": map[string]any{"id": "tu_1", "is_error": false}})
	ask := f.event(runtimeevents.KindAgentPermissionRequested, "turn_1", map[string]any{
		"request_id": 4, "method": "item/tool/requestUserInput",
		"params": map[string]any{"questions": []any{map[string]any{"question": "Which db?"}}},
	})
	f.mustQuiet(bash, result, f.delta("turn_1", "Tests pass; moving on.", "a", ""), ask)
	got, ok := f.send(f.done("turn_1", nil))
	want(t, got, ok, Output{SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude", Text: "Which db?", Kind: KindQuestion, Confidence: ConfidenceExact})
}

// The runtime's final text with no deltas to place it by is the last thing the
// agent said, and is used.
func TestARefusedTurnWithOnlyAFinalTextUsesIt(t *testing.T) {
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn", Text: "I need approval to deploy."},
		events.ToolUse{ID: "t1", Name: "Bash"},
		events.PermissionDenied{Action: "Bash", DisplayName: "make deploy", ToolUseID: "t1"},
	)
	if out.Kind != KindApproval || out.Text != "I need approval to deploy." || out.Confidence != ConfidenceExact {
		t.Fatalf("got %+v, want the runtime's final text, exact", out)
	}
}

// A question asked and answered, after a refusal, is still not the agent working: the
// refusal stands. A question tool call never counts as work.
func TestAskingIsNotWorkingPastARefusal(t *testing.T) {
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"},
		events.ToolUse{ID: "t1", Name: "Bash"}, events.ToolResult{ID: "t1", IsError: true},
		events.ToolUse{ID: "q1", Name: "AskUserQuestion", Args: map[string]any{"question": "Proceed?"}}, events.ToolResult{ID: "q1"},
		events.PermissionDenied{Action: "Bash", DisplayName: "make deploy", ToolUseID: "t1"},
	)
	if out.Kind != KindApproval {
		t.Fatalf("got %+v, want the approval: asking is not work", out)
	}
}

// Text before a question call is not the question.
func TestTextBeforeAQuestionCallIsNotTheQuestion(t *testing.T) {
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn", Text: "Let me ask."},
		events.Delta{Text: "Let me ask.", BlockID: "m1"},
		events.ToolUse{ID: "q1", Name: "AskUserQuestion", Args: map[string]any{"question": "Which database?"}},
		events.ToolResult{ID: "q1", IsError: true},
	)
	if out.Kind != KindQuestion || out.Text != "Which database?" {
		t.Fatalf("got %+v, want the question itself", out)
	}
}

// The same id on two calls is the newer call.
func TestARefusalNamesTheNewestCallWithThatID(t *testing.T) {
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"},
		events.ToolUse{ID: "x", Name: "Bash"},
		events.ToolUse{ID: "y", Name: "Read"},
		events.ToolUse{ID: "x", Name: "Bash"},
		events.PermissionDenied{Action: "Bash", DisplayName: "make deploy", ToolUseID: "x"},
	)
	if out.Kind != KindApproval {
		t.Fatalf("got %+v, want an approval: the refused call is the last one", out)
	}
}

// Positions survive the oldest text blocks being dropped: a signal raised after
// 128 blocks is still before the blocks that follow it.
func TestPositionsSurviveDroppedBlocks(t *testing.T) {
	r := provider(t)
	for i := range maxBlocks {
		r.ObserveProvider(events.Delta{Text: fmt.Sprintf("early %d", i), BlockID: fmt.Sprintf("e%d", i)})
	}
	r.ObserveProvider(events.ToolUse{ID: "q1", Name: "AskUserQuestion", Args: map[string]any{"question": "Which?"}})
	r.ObserveProvider(events.ToolResult{ID: "q1", IsError: true})
	// A request-raised signal, and a pending request, at the same point.
	for i := range maxBlocks * 2 {
		r.ObserveProvider(events.Delta{Text: fmt.Sprintf("later %d", i), BlockID: fmt.Sprintf("l%d", i)})
	}
	out, ok := r.ObserveProvider(events.Done{StopReason: "end_turn"})
	if !ok || out.Kind != KindQuestion || out.Text != fmt.Sprintf("later %d", maxBlocks*2-1) {
		t.Fatalf("got %+v, want the question with the last block the agent wrote", out)
	}
}

func TestPositionsOfRequestsSurviveDroppedBlocks(t *testing.T) {
	f := newFeed(t, Config{})
	for i := range maxBlocks {
		f.mustQuiet(f.delta("turn_1", fmt.Sprintf("early %d", i), fmt.Sprintf("e%d", i), ""))
	}
	f.mustQuiet(f.event(runtimeevents.KindAgentPermissionRequested, "turn_1", map[string]any{"request_id": 1, "method": "m", "params": map[string]any{"toolCall": map[string]any{"title": "rm -rf build"}}}))
	for i := range maxBlocks * 2 {
		f.mustQuiet(f.delta("turn_1", fmt.Sprintf("later %d", i), fmt.Sprintf("l%d", i), ""))
	}
	got, ok := f.send(f.done("turn_1", nil))
	if !ok || got.Kind != KindApproval || got.Text != fmt.Sprintf("later %d", maxBlocks*2-1) {
		t.Fatalf("got %+v, want the approval with the last block the agent wrote", got)
	}
}

// ACP agents send a tool result flat and update a call several times: only a failed
// or completed status (or an explicit is_error) is the result.
func TestACPToolResultShapes(t *testing.T) {
	denied := func(f *feed) runtimeevents.Event {
		return f.event(runtimeevents.KindAgentPermissionDenied, "turn_1", map[string]any{"action": "x", "display_name": "x", "tool_use_id": "tc1"})
	}
	acpUse := func(f *feed, id string) runtimeevents.Event {
		return f.event(runtimeevents.KindAgentToolUse, "turn_1", map[string]any{"tool_call_id": id, "title": "Run shell command", "kind": "execute"})
	}
	t.Run("a flat failed status is an error", func(t *testing.T) {
		f := newFeed(t, Config{})
		ask := f.event(runtimeevents.KindAgentToolUse, "turn_1", map[string]any{"tool_call_id": "q1", "title": "AskUserQuestion", "raw_input": map[string]any{"question": "Which?"}})
		f.mustQuiet(ask, f.event(runtimeevents.KindAgentToolResult, "turn_1", map[string]any{"tool_call_id": "q1", "status": "failed", "is_error": true}))
		got, ok := f.send(f.done("turn_1", nil))
		if !ok || got.Kind != KindQuestion {
			t.Fatalf("got %+v, want a question: the call failed", got)
		}
	})
	t.Run("a nested status failed with no is_error is an error", func(t *testing.T) {
		f := newFeed(t, Config{})
		ask := f.event(runtimeevents.KindAgentToolUse, "turn_1", map[string]any{"tool_use": map[string]any{"id": "q1", "name": "AskUserQuestion", "input": map[string]any{"question": "Which?"}}})
		f.mustQuiet(ask, f.event(runtimeevents.KindAgentToolResult, "turn_1", map[string]any{"tool_result": map[string]any{"id": "q1", "status": "failed"}}))
		got, ok := f.send(f.done("turn_1", nil))
		if !ok || got.Kind != KindQuestion {
			t.Fatalf("got %+v, want a question: a failed status is an error", got)
		}
	})
	t.Run("a completed status answers a question", func(t *testing.T) {
		f := newFeed(t, Config{})
		ask := f.event(runtimeevents.KindAgentToolUse, "turn_1", map[string]any{"tool_call_id": "q1", "title": "AskUserQuestion", "raw_input": map[string]any{"question": "Which?"}})
		f.mustQuiet(ask, f.event(runtimeevents.KindAgentToolResult, "turn_1", map[string]any{"tool_call_id": "q1", "status": "completed", "is_error": false}))
		got, ok := f.send(f.done("turn_1", nil))
		if !ok || got.Kind != KindFinal {
			t.Fatalf("got %+v, want a final turn: the question was answered", got)
		}
	})
	// The batch rule needs the result's place in the turn, and an agent that sends
	// a failed status with no is_error (copilot) still sends the result.
	t.Run("a flat failed status with no is_error is the result", func(t *testing.T) {
		f := newFeed(t, Config{})
		f.mustQuiet(acpUse(f, "tc1"), acpUse(f, "tc2"),
			f.event(runtimeevents.KindAgentToolResult, "turn_1", map[string]any{"tool_call_id": "tc1", "status": "failed"}),
			denied(f))
		got, ok := f.send(f.done("turn_1", nil))
		if !ok || got.Kind != KindApproval {
			t.Fatalf("got %+v, want an approval: tc2 was issued with tc1, before its result", got)
		}
	})
	// A progress update is not the result: it must not move the point past which a
	// call counts as work.
	t.Run("a progress update is not the result", func(t *testing.T) {
		f := newFeed(t, Config{})
		f.mustQuiet(acpUse(f, "tc1"),
			f.event(runtimeevents.KindAgentToolResult, "turn_1", map[string]any{"tool_call_id": "tc1", "status": "in_progress"}),
			acpUse(f, "tc2"),
			f.event(runtimeevents.KindAgentToolResult, "turn_1", map[string]any{"tool_call_id": "tc1", "status": "failed"}),
			denied(f))
		got, ok := f.send(f.done("turn_1", nil))
		if !ok || got.Kind != KindApproval {
			t.Fatalf("got %+v, want an approval: tc2 started before tc1's result", got)
		}
	})
}

// A request-raised question sits where it arrived, and keeps its place when the
// oldest text blocks are dropped.
func TestPositionsOfRequestedQuestionsSurviveDroppedBlocks(t *testing.T) {
	f := newFeed(t, Config{})
	for i := range maxBlocks {
		f.mustQuiet(f.delta("turn_1", fmt.Sprintf("early %d", i), fmt.Sprintf("e%d", i), ""))
	}
	f.mustQuiet(f.event(runtimeevents.KindAgentPermissionRequested, "turn_1", map[string]any{
		"request_id": 1, "method": "item/tool/requestUserInput",
		"params": map[string]any{"questions": []any{map[string]any{"question": "Which db?"}}},
	}))
	for i := range maxBlocks * 2 {
		f.mustQuiet(f.delta("turn_1", fmt.Sprintf("later %d", i), fmt.Sprintf("l%d", i), ""))
	}
	got, ok := f.send(f.done("turn_1", nil))
	if !ok || got.Kind != KindQuestion || got.Text != fmt.Sprintf("later %d", maxBlocks*2-1) {
		t.Fatalf("got %+v, want the question with the last block the agent wrote", got)
	}
}

// A question tool call that carries no id is placed where it was made: the text
// before it is not the question, and a call after it is work.
func TestAQuestionCallWithNoIDIsPlacedWhereItWasMade(t *testing.T) {
	ask := events.ToolUse{Name: "AskUserQuestion", Args: map[string]any{"question": "Which database?"}}
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn", Text: "Let me ask."},
		events.Delta{Text: "Let me ask.", BlockID: "m1"}, ask)
	if out.Kind != KindQuestion || out.Text != "Which database?" {
		t.Fatalf("got %+v, want the question itself, not the text before it", out)
	}
	out = finishWith(t, provider(t), events.Done{StopReason: "end_turn"}, ask,
		events.ToolUse{ID: "t2", Name: "Write"}, events.Delta{Text: "Chose Postgres.", BlockID: "m1"})
	if out.Kind != KindFinal {
		t.Fatalf("got %+v, want a final turn: the agent wrote after asking", out)
	}
}

// ---- Second review of #41 ---------------------------------------------------

// A tool call's result and refusal live with the call: after hundreds of earlier
// results, a later call's are still counted (an earlier build kept results in a
// shared map that stopped taking entries at 512).
func TestFactsAboutALaterCallStillCountAfterManyResults(t *testing.T) {
	prime := func(r *Reducer) {
		for i := range 600 {
			r.ObserveProvider(events.ToolUse{ID: fmt.Sprintf("r%d", i), Name: "Read"})
			r.ObserveProvider(events.ToolResult{ID: fmt.Sprintf("r%d", i)})
		}
	}

	t.Run("an answered question", func(t *testing.T) {
		r := provider(t)
		prime(r)
		out := finishWith(t, r, events.Done{StopReason: "end_turn"},
			events.ToolUse{ID: "q1", Name: "AskUserQuestion", Args: map[string]any{"question": "Which?"}},
			events.ToolResult{ID: "q1"},
			events.Delta{Text: "Going with it.", BlockID: "m1"},
		)
		if out.Kind != KindFinal {
			t.Fatalf("got %+v, want a final turn: the question came back answered", out)
		}
	})

	t.Run("a refusal in a parallel batch", func(t *testing.T) {
		r := provider(t)
		prime(r)
		out := finishWith(t, r, events.Done{StopReason: "end_turn"},
			events.ToolUse{ID: "t1", Name: "Bash"},
			events.ToolUse{ID: "t2", Name: "Read"},
			events.ToolResult{ID: "t1", IsError: true},
			events.ToolResult{ID: "t2"},
			events.Delta{Text: "I need approval.", BlockID: "m1"},
			events.PermissionDenied{Action: "Bash", DisplayName: "x", ToolUseID: "t1"},
		)
		if out.Kind != KindApproval {
			t.Fatalf("got %+v, want an approval", out)
		}
	})
}

// What arrives before the call it names is held for the call, in a bounded place.
func TestFactsThatArriveBeforeTheirCallAreHeldBounded(t *testing.T) {
	r := provider(t)
	for i := range maxEarly * 2 {
		r.ObserveProvider(events.ToolResult{ID: fmt.Sprintf("u%d", i), IsError: true})
	}
	if n := len(r.current().early); n != maxEarly {
		t.Fatalf("holds %d early facts, want %d", n, maxEarly)
	}

	// The newest are kept and a call claims its own when it arrives.
	newest := fmt.Sprintf("u%d", maxEarly*2-1)
	r.ObserveProvider(events.ToolUse{ID: newest, Name: "AskUserQuestion", Args: map[string]any{"question": "Which?"}})
	out, ok := r.ObserveProvider(events.Done{StopReason: "end_turn"})
	if !ok || out.Kind != KindQuestion {
		t.Fatalf("got %+v, want a question: its error result arrived first and was kept", out)
	}
}

// One reducer can take a call from the stream feed and its result from the typed
// feed; the call's id is what pairs them.
func TestAToolCallFromTheStreamFeedPairsWithTypedFacts(t *testing.T) {
	r := provider(t)
	r.ObserveStream(llmtypes.StreamEvent{Type: llmtypes.EventToolUse, ToolUse: &llmtypes.ToolUseBlock{ID: "t1", Name: "Bash"}})
	r.ObserveStream(llmtypes.StreamEvent{Type: llmtypes.EventToolUse, ToolUse: &llmtypes.ToolUseBlock{ID: "t2", Name: "Read"}})
	r.ObserveProvider(events.ToolResult{ID: "t1", IsError: true})
	r.ObserveProvider(events.ToolResult{ID: "t2"})
	r.ObserveProvider(events.PermissionDenied{Action: "Bash", DisplayName: "x", ToolUseID: "t1"})
	out, ok := r.ObserveStream(llmtypes.StreamEvent{Type: llmtypes.EventDone})
	if !ok || out.Kind != KindApproval {
		t.Fatalf("got %+v, want an approval: t2 was issued with t1, before its result", out)
	}
}

// A completed or failed status with no is_error is still the result (copilot sends
// the status alone), nested or flat.
func TestACPStatusAloneIsTheResult(t *testing.T) {
	for _, shape := range []struct {
		name  string
		build func(f *feed, id, status string) runtimeevents.Event
	}{
		{"nested", func(f *feed, id, status string) runtimeevents.Event {
			return f.event(runtimeevents.KindAgentToolResult, "turn_1", map[string]any{"tool_result": map[string]any{"id": id, "status": status}})
		}},
		{"flat", func(f *feed, id, status string) runtimeevents.Event {
			return f.event(runtimeevents.KindAgentToolResult, "turn_1", map[string]any{"tool_call_id": id, "status": status})
		}},
	} {
		t.Run(shape.name+" completed answers a question", func(t *testing.T) {
			f := newFeed(t, Config{})
			ask := f.event(runtimeevents.KindAgentToolUse, "turn_1", map[string]any{"tool_call_id": "q1", "title": "AskUserQuestion", "raw_input": map[string]any{"question": "Which?"}})
			f.mustQuiet(ask, shape.build(f, "q1", "completed"))
			got, ok := f.send(f.done("turn_1", nil))
			if !ok || got.Kind != KindFinal {
				t.Fatalf("got %+v, want a final turn: a completed status is the answer", got)
			}
		})
	}
}

// A result that arrives before its call is the call's when the call arrives: an OK
// result for a question tool, reported first, still means it was answered.
func TestAnEarlyResultBelongsToTheCallThatArrivesAfterIt(t *testing.T) {
	out := finishWith(t, provider(t), events.Done{StopReason: "end_turn"},
		events.ToolResult{ID: "q1"},
		events.ToolUse{ID: "q1", Name: "AskUserQuestion", Args: map[string]any{"question": "Which?"}},
		events.Delta{Text: "Going with it.", BlockID: "m1"},
	)
	if out.Kind != KindFinal {
		t.Fatalf("got %+v, want a final turn: the question's OK result came first", out)
	}
}
