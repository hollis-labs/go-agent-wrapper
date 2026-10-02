package turnoutput

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider/events"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// A turn that has been reported stays reported: events that arrive for it
// afterwards are dropped, and they never disturb the turn that is open.
func TestLateEventsOfAFinishedTurnAreDropped(t *testing.T) {
	t.Run("a late delta and a repeated terminal event", func(t *testing.T) {
		f := newFeed(t, Config{})
		f.mustQuiet(f.delta("turn_1", "one", "a", ""))
		if _, ok := f.send(f.done("turn_1", nil)); !ok {
			t.Fatal("no Output for turn_1")
		}
		f.mustQuiet(
			f.event(runtimeevents.KindTurnStarted, "turn_2", nil),
			f.delta("turn_2", "two", "a", ""),
			f.delta("turn_1", "late tail", "z", "message"),
		)
		got, ok := f.send(f.done("turn_2", nil))
		want(t, got, ok, Output{
			SessionID: "ses_1", TurnID: "turn_2", Runtime: "claude",
			Text: "two", Kind: KindFinal, Confidence: ConfidenceHeuristic,
		})
		if out, ok := f.send(f.done("turn_1", nil)); ok {
			t.Fatalf("turn_1 reported a second time: %+v", out)
		}
	})

	t.Run("a late tool result does not reopen it for the process exit", func(t *testing.T) {
		f := newFeed(t, Config{})
		f.mustQuiet(f.delta("turn_1", "one", "a", ""))
		f.send(f.done("turn_1", nil))
		f.mustQuiet(f.event(runtimeevents.KindAgentToolResult, "turn_1", nil))
		if out, ok := f.send(f.event(runtimeevents.KindProcessExited, "", nil)); ok {
			t.Fatalf("process.exited re-reported turn_1: %+v", out)
		}
	})

	t.Run("a late event does not destroy the turn in progress", func(t *testing.T) {
		f := newFeed(t, Config{})
		f.mustQuiet(f.delta("turn_1", "one", "a", ""))
		f.send(f.done("turn_1", nil))
		f.mustQuiet(f.delta("turn_2", "the real answer", "a", ""), f.event(runtimeevents.KindAgentToolResult, "turn_1", nil))
		got, ok := f.send(f.done("turn_2", nil))
		want(t, got, ok, Output{
			SessionID: "ses_1", TurnID: "turn_2", Runtime: "claude",
			Text: "the real answer", Kind: KindFinal, Confidence: ConfidenceHeuristic,
		})
	})
}

// A turn that starts before the previous one's terminal event arrives (an
// interrupt can order them so) does not cost either turn its text.
func TestTurnsInProgressTogetherKeepTheirOwnText(t *testing.T) {
	f := newFeed(t, Config{})
	f.mustQuiet(f.delta("turn_1", "partial of one", "a", ""), f.event(runtimeevents.KindTurnStarted, "turn_2", nil))
	got, ok := f.send(f.failed("turn_1", map[string]any{"reason": "interrupted", "stop_reason": "error"}))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
		Text: "partial of one", Kind: KindTerminal, StopReason: llmtypes.StopReasonCancelled, Confidence: ConfidenceHeuristic,
	})
	f.mustQuiet(f.delta("turn_2", "two", "a", ""))
	got, ok = f.send(f.done("turn_2", nil))
	want(t, got, ok, Output{
		SessionID: "ses_1", TurnID: "turn_2", Runtime: "claude",
		Text: "two", Kind: KindFinal, Confidence: ConfidenceHeuristic,
	})
}

// Flush is for a session that is gone: whatever the host's reason, the turn is
// terminal, with what the agent had said and the reason as the fallback.
func TestFlushIsAlwaysTerminal(t *testing.T) {
	for _, reason := range []string{"session_closed", "process_exited", "interrupted", "", "anything else"} {
		t.Run(fmt.Sprintf("reason %q with partial text", reason), func(t *testing.T) {
			r := New(Config{SessionID: "s", Runtime: "opencode", NewTurnID: func() string { return "turn_f" }})
			r.ObserveProvider(events.Delta{Text: "partial words", BlockID: "a"})
			got, ok := r.Flush(reason)
			if !ok || got.Kind != KindTerminal || got.Text != "partial words" || got.Confidence != ConfidenceHeuristic {
				t.Fatalf("Flush(%q) = %+v, want a terminal turn carrying the partial text", reason, got)
			}
		})
	}

	t.Run("the reason is the text when nothing was said", func(t *testing.T) {
		r := New(Config{SessionID: "s", Runtime: "opencode", NewTurnID: func() string { return "turn_f" }})
		r.ObserveProvider(events.ToolUse{Name: "Bash"})
		got, ok := r.Flush("session_closed")
		want(t, got, ok, Output{
			SessionID: "s", TurnID: "turn_f", Runtime: "opencode",
			Text: "session_closed", Kind: KindTerminal, StopReason: "error", Confidence: ConfidenceHeuristic,
		})
	})
}

// A refusal ends the block the agent was writing, like every other signal does,
// so what it wrote after the refusal is its own block and is the turn's text.
func TestPermissionDeniedStartsANewBlock(t *testing.T) {
	t.Run("runtimeevents", func(t *testing.T) {
		f := newFeed(t, Config{})
		f.mustQuiet(
			f.delta("turn_1", "I will push now. ", "", ""),
			f.event(runtimeevents.KindAgentPermissionDenied, "turn_1", map[string]any{"action": "bash", "display_name": "git push"}),
			f.delta("turn_1", "I could not push; please approve.", "", ""),
		)
		got, ok := f.send(f.done("turn_1", nil))
		want(t, got, ok, Output{
			SessionID: "ses_1", TurnID: "turn_1", Runtime: "claude",
			Text: "I could not push; please approve.", Kind: KindApproval, Confidence: ConfidenceHeuristic,
		})
	})

	t.Run("provider events", func(t *testing.T) {
		r := New(Config{SessionID: "s", Runtime: "agy", NewTurnID: func() string { return "turn_p" }})
		r.ObserveProvider(events.Delta{Text: "I will push now. "})
		r.ObserveProvider(events.PermissionDenied{Action: "bash", DisplayName: "git push"})
		r.ObserveProvider(events.Delta{Text: "I could not push; please approve."})
		got, ok := r.ObserveProvider(events.Done{})
		want(t, got, ok, Output{
			SessionID: "s", TurnID: "turn_p", Runtime: "agy",
			Text: "I could not push; please approve.", Kind: KindApproval, Confidence: ConfidenceHeuristic,
		})
	})
}

// The provider and stream feeds carry no turn id, so a repeated or trailing
// event cannot be matched to a turn; the reducer decides by whether a turn is in
// progress.
func TestIDLessFeedsDoNotInventTurns(t *testing.T) {
	newReducer := func() *Reducer {
		n := 0
		return New(Config{SessionID: "s", Runtime: "claude", NewTurnID: func() string { n++; return fmt.Sprintf("turn_%d", n) }})
	}

	t.Run("a repeated done reports once", func(t *testing.T) {
		r := newReducer()
		r.ObserveProvider(events.Delta{Text: "hello", BlockID: "a"})
		if _, ok := r.ObserveProvider(events.Done{}); !ok {
			t.Fatal("no Output for the turn")
		}
		if out, ok := r.ObserveProvider(events.Done{}); ok {
			t.Fatalf("a repeated Done reported %+v", out)
		}
		if out, ok := r.ObserveStream(llmtypes.StreamEvent{Type: llmtypes.EventDone}); ok {
			t.Fatalf("a repeated stream Done reported %+v", out)
		}
	})

	t.Run("a trailing usage does not leak into the next turn", func(t *testing.T) {
		r := newReducer()
		r.ObserveProvider(events.Delta{Text: "one", BlockID: "a"})
		r.ObserveProvider(events.Done{})
		r.ObserveProvider(events.Usage{StopReason: llmtypes.StopReasonCancelled})
		r.ObserveProvider(events.Delta{Text: "two", BlockID: "a"})
		got, ok := r.ObserveProvider(events.Done{})
		want(t, got, ok, Output{
			SessionID: "s", TurnID: "turn_2", Runtime: "claude",
			Text: "two", Kind: KindFinal, Confidence: ConfidenceHeuristic,
		})
	})

	t.Run("a usage ahead of the turn's own events still counts", func(t *testing.T) {
		r := newReducer()
		r.ObserveProvider(events.Usage{StopReason: llmtypes.StopReasonMaxTokens})
		got, ok := r.ObserveProvider(events.Done{})
		want(t, got, ok, Output{
			SessionID: "s", TurnID: "turn_1", Runtime: "claude",
			Text: "", Kind: KindFinal, StopReason: llmtypes.StopReasonMaxTokens, Confidence: ConfidenceHeuristic,
		})
	})

	t.Run("a lone error is never lost", func(t *testing.T) {
		r := newReducer()
		got, ok := r.ObserveProvider(events.Error{Message: "codex: not signed in"})
		want(t, got, ok, Output{
			SessionID: "s", TurnID: "turn_1", Runtime: "claude",
			Text: "codex: not signed in", Kind: KindFailure, StopReason: "error", Confidence: ConfidenceExact,
		})
	})

	t.Run("an error after a finished turn is reported once", func(t *testing.T) {
		r := newReducer()
		r.ObserveProvider(events.Delta{Text: "hello", BlockID: "a"})
		r.ObserveProvider(events.Done{})
		got, ok := r.ObserveProvider(events.Error{Message: "process exited: exit status 1"})
		if !ok || got.Kind != KindFailure || got.Text != "process exited: exit status 1" || got.TurnID != "turn_2" {
			t.Fatalf("got %+v, want a failure on a turn of its own", got)
		}
		if out, ok := r.ObserveProvider(events.Error{Message: "process exited: exit status 1"}); ok {
			t.Fatalf("the same error twice reported %+v", out)
		}
	})

	t.Run("a turn can follow a lone error", func(t *testing.T) {
		r := newReducer()
		r.ObserveProvider(events.Error{Message: "boom"})
		r.ObserveProvider(events.Delta{Text: "recovered", BlockID: "a"})
		got, ok := r.ObserveProvider(events.Done{})
		want(t, got, ok, Output{
			SessionID: "s", TurnID: "turn_2", Runtime: "claude",
			Text: "recovered", Kind: KindFinal, Confidence: ConfidenceHeuristic,
		})
	})

	t.Run("runtimeevents without turn ids", func(t *testing.T) {
		f := newFeed(t, Config{NewTurnID: func() string { return "turn_m" }})
		f.mustQuiet(f.delta("", "x", "a", ""))
		if _, ok := f.send(f.done("", nil)); !ok {
			t.Fatal("no Output")
		}
		if out, ok := f.send(f.done("", nil)); ok {
			t.Fatalf("a repeated terminal event reported %+v", out)
		}
	})
}

// A turn whose terminal event never comes keeps buffering, so the buffer is
// bounded: the oldest blocks go first, the last is never dropped, and one block
// is cut at its cap.
func TestAnUnfinishedTurnsBufferIsBounded(t *testing.T) {
	t.Run("block count", func(t *testing.T) {
		r := New(Config{SessionID: "s", Runtime: "claude", NewTurnID: func() string { return "turn_b" }})
		for i := range maxBlocks * 3 {
			r.ObserveProvider(events.Delta{Text: fmt.Sprintf("block %d", i), BlockID: fmt.Sprintf("b%d", i)})
		}
		if n := len(r.current().blocks); n != maxBlocks {
			t.Fatalf("holds %d blocks, want %d", n, maxBlocks)
		}
		got, ok := r.ObserveProvider(events.Done{})
		if !ok || got.Text != fmt.Sprintf("block %d", maxBlocks*3-1) {
			t.Fatalf("got %+v, want the last block", got)
		}
	})

	t.Run("total bytes", func(t *testing.T) {
		r := New(Config{SessionID: "s", Runtime: "claude", NewTurnID: func() string { return "turn_b" }})
		chunk := strings.Repeat("x", maxBlockBytes)
		for i := range 3 * maxTurnBytes / maxBlockBytes {
			r.ObserveProvider(events.Delta{Text: chunk, BlockID: fmt.Sprintf("b%d", i)})
		}
		if n := r.current().bytes; n > maxTurnBytes {
			t.Fatalf("holds %d bytes, want at most %d", n, maxTurnBytes)
		}
		if got, ok := r.ObserveProvider(events.Done{}); !ok || len(got.Text) != maxBlockBytes {
			t.Fatalf("the last block was not kept whole: %d bytes", len(got.Text))
		}
	})

	t.Run("one block is cut on a rune boundary", func(t *testing.T) {
		r := New(Config{SessionID: "s", Runtime: "claude", NewTurnID: func() string { return "turn_b" }})
		r.ObserveProvider(events.Delta{Text: strings.Repeat("é", maxBlockBytes), BlockID: "a"}) // 2 bytes each
		r.ObserveProvider(events.Delta{Text: "more", BlockID: "a"})
		got, ok := r.ObserveProvider(events.Done{})
		if !ok || !utf8.ValidString(got.Text) || len(got.Text) > maxBlockBytes {
			t.Fatalf("got %d bytes (valid utf-8: %v), want a whole-rune cut at %d", len(got.Text), utf8.ValidString(got.Text), maxBlockBytes)
		}
	})

	t.Run("open turns", func(t *testing.T) {
		r := New(Config{SessionID: "s", Runtime: "claude"})
		for i := range maxOpenTurns * 2 {
			r.Observe(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: fmt.Sprintf("turn_%d", i)})
		}
		if len(r.open) != maxOpenTurns || len(r.order) != maxOpenTurns {
			t.Fatalf("holds %d turns (%d ordered), want %d", len(r.open), len(r.order), maxOpenTurns)
		}
	})

	t.Run("pending permission requests", func(t *testing.T) {
		f := newFeed(t, Config{})
		for i := range maxPending * 2 {
			f.mustQuiet(f.event(runtimeevents.KindAgentPermissionRequested, "turn_1", map[string]any{"request_id": i, "method": "m"}))
		}
		if n := len(f.r.open["turn_1"].pending); n != maxPending {
			t.Fatalf("holds %d pending requests, want %d", n, maxPending)
		}
	})

	t.Run("a signal's text", func(t *testing.T) {
		r := New(Config{SessionID: "s", Runtime: "claude", NewTurnID: func() string { return "turn_b" }})
		r.ObserveProvider(events.ToolUse{Name: "AskUserQuestion", Args: map[string]any{"question": strings.Repeat("q", 3*maxSignalBytes)}})
		got, ok := r.ObserveProvider(events.Done{})
		if !ok || got.Kind != KindQuestion || len(got.Text) != maxSignalBytes {
			t.Fatalf("got a %s of %d bytes, want a question cut at %d", got.Kind, len(got.Text), maxSignalBytes)
		}
	})
}
