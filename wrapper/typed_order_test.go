package wrapper

import (
	"fmt"
	"sync"
	"testing"

	llmtypes "github.com/hollis-labs/go-llm-types"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// The k-th marker stands for the k-th typed event, so a consumer that reads the
// channel in order emits the typed events in the order they were queued, between
// the stream events they were queued among.
func TestTypedQueueKeepsTypedEventsInLineWithStreamEvents(t *testing.T) {
	fanout := make(chan llmtypes.StreamEvent, 16)
	q := &typedQueue{fanout: fanout}

	// A parser's order: a stream event, a typed one, a stream event, two typed ones.
	fanout <- llmtypes.StreamEvent{Type: llmtypes.EventDelta, Content: "a"}
	q.enqueue(typedEvent{kind: runtimeevents.KindAgentToolResult, payload: "r1"})
	fanout <- llmtypes.StreamEvent{Type: llmtypes.EventDelta, Content: "b"}
	q.enqueue(typedEvent{kind: runtimeevents.KindAgentPermissionDenied, payload: "d1"})
	q.enqueue(typedEvent{kind: runtimeevents.KindAgentToolResult, payload: "r2"})
	q.closeFanout()

	var got []string
	for ev := range fanout {
		if ev.Type == typedMarker {
			te, ok := q.dequeue()
			if !ok {
				t.Fatal("a marker with no queued event")
			}
			got = append(got, fmt.Sprint(te.payload))
			continue
		}
		got = append(got, ev.Content)
	}
	if want := fmt.Sprint([]string{"a", "r1", "b", "d1", "r2"}); fmt.Sprint(got) != want {
		t.Fatalf("emitted %v, want %s", got, want)
	}
	if _, ok := q.dequeue(); ok {
		t.Fatal("an event was left queued with no marker")
	}
}

// A typed event is never dropped for a full channel: the send waits for the consumer.
func TestTypedQueueWaitsForAFullChannelInsteadOfDropping(t *testing.T) {
	fanout := make(chan llmtypes.StreamEvent, 1)
	q := &typedQueue{fanout: fanout}
	const n = 50
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range n {
			if !q.enqueue(typedEvent{payload: i}) {
				t.Errorf("enqueue %d refused", i)
			}
		}
		q.closeFanout()
	}()
	var got []int
	for ev := range fanout {
		if ev.Type != typedMarker {
			continue
		}
		te, _ := q.dequeue()
		got = append(got, te.payload.(int))
	}
	wg.Wait()
	if len(got) != n {
		t.Fatalf("emitted %d of %d typed events", len(got), n)
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("event %d emitted as %d: out of order", i, v)
		}
	}
}

// After the channel is closed a late callback is refused rather than panicking, so
// the caller can emit directly, and closing twice is harmless.
func TestTypedQueueRefusesAfterClose(t *testing.T) {
	q := &typedQueue{fanout: make(chan llmtypes.StreamEvent, 1)}
	q.closeFanout()
	q.closeFanout()
	if q.enqueue(typedEvent{payload: "late"}) {
		t.Fatal("enqueue after close reported success")
	}
}
