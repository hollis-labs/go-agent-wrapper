package turnoutput

import (
	"fmt"
	"strings"
	"testing"

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
