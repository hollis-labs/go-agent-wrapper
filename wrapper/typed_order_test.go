package wrapper

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	llmtypes "github.com/hollis-labs/go-llm-types"
	pevents "github.com/hollis-labs/go-providers/provider/events"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// The k-th marker stands for the k-th queued function, so a consumer that reads the
// channel in order runs them in the order they were queued, between the stream
// events they were queued among.
func TestTypedQueueKeepsEmissionsInLineWithStreamEvents(t *testing.T) {
	fanout := make(chan llmtypes.StreamEvent, 16)
	q := &typedQueue{fanout: fanout}
	var got []string
	emit := func(s string) func() { return func() { got = append(got, s) } }

	// A parser's order: a stream event, a queued one, a stream event, two queued ones.
	fanout <- llmtypes.StreamEvent{Type: llmtypes.EventDelta, Content: "a"}
	q.enqueue(emit("r1"))
	fanout <- llmtypes.StreamEvent{Type: llmtypes.EventDelta, Content: "b"}
	q.enqueue(emit("d1"))
	q.enqueue(emit("r2"))
	q.closeFanout()

	for ev := range fanout {
		if ev.Type == typedMarker {
			run, ok := q.dequeue()
			if !ok {
				t.Fatal("a marker with no queued function")
			}
			run()
			continue
		}
		got = append(got, ev.Content)
	}
	if want := fmt.Sprint([]string{"a", "r1", "b", "d1", "r2"}); fmt.Sprint(got) != want {
		t.Fatalf("ran %v, want %s", got, want)
	}
	if _, ok := q.dequeue(); ok {
		t.Fatal("a function was left queued with no marker")
	}
}

// A queued emission is never dropped for a full channel: the send waits for the consumer.
func TestTypedQueueWaitsForAFullChannelInsteadOfDropping(t *testing.T) {
	fanout := make(chan llmtypes.StreamEvent, 1)
	q := &typedQueue{fanout: fanout}
	const n = 50
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range n {
			if !q.enqueue(func() { _ = i }) {
				t.Errorf("enqueue %d refused", i)
			}
		}
		q.closeFanout()
	}()
	ran := 0
	for ev := range fanout {
		if ev.Type != typedMarker {
			continue
		}
		run, _ := q.dequeue()
		run()
		ran++
	}
	wg.Wait()
	if ran != n {
		t.Fatalf("ran %d of %d queued emissions", ran, n)
	}
}

// After the channel is closed a late callback is refused rather than panicking, so
// the caller can emit directly, and closing twice is harmless.
func TestTypedQueueRefusesAfterClose(t *testing.T) {
	q := &typedQueue{fanout: make(chan llmtypes.StreamEvent, 1)}
	q.closeFanout()
	q.closeFanout()
	if q.enqueue(func() {}) {
		t.Fatal("enqueue after close reported success")
	}
}

// enqueue holds the read side of the close lock across its send, so a close that
// races it neither panics (send on a closed channel) nor loses an emission that was
// accepted: every enqueue either ran its function through a marker or was refused.
// Run under -race.
func TestTypedQueueEnqueueRacingClose(t *testing.T) {
	for range 20 {
		fanout := make(chan llmtypes.StreamEvent, 4)
		q := &typedQueue{fanout: fanout}
		var accepted, refused, ran atomic.Int64

		consumerDone := make(chan struct{})
		go func() {
			defer close(consumerDone)
			for ev := range fanout {
				if ev.Type != typedMarker {
					continue
				}
				if run, ok := q.dequeue(); ok {
					run()
				}
			}
		}()

		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 50 {
					if q.enqueue(func() { ran.Add(1) }) {
						accepted.Add(1)
					} else {
						refused.Add(1)
					}
				}
			}()
		}
		q.closeFanout()
		wg.Wait()
		<-consumerDone

		if ran.Load() != accepted.Load() {
			t.Fatalf("%d accepted but %d ran (%d refused)", accepted.Load(), ran.Load(), refused.Load())
		}
		if accepted.Load()+refused.Load() != 8*50 {
			t.Fatalf("accepted %d + refused %d != %d", accepted.Load(), refused.Load(), 8*50)
		}
	}
}

// The refused call's id travels on agent.permission_denied, so a consumer can place
// the refusal where the call was.
func TestTranslateProviderEventCarriesTheRefusedCallsID(t *testing.T) {
	kind, payload, ok := translateProviderEvent(pevents.PermissionDenied{Action: "Bash", DisplayName: "make deploy", ToolUseID: "toolu_1"})
	if !ok || kind != runtimeevents.KindAgentPermissionDenied {
		t.Fatalf("kind = %q (mapped %v), want agent.permission_denied", kind, ok)
	}
	p, _ := payload.(map[string]any)
	if p["tool_use_id"] != "toolu_1" || p["action"] != "Bash" || p["display_name"] != "make deploy" {
		t.Fatalf("payload = %v", p)
	}

	// With no id (agy) the field is absent, not empty.
	_, payload, _ = translateProviderEvent(pevents.PermissionDenied{Action: "command", DisplayName: "RunCommand"})
	if _, has := payload.(map[string]any)["tool_use_id"]; has {
		t.Fatalf("payload = %v, want no tool_use_id when the provider names none", payload)
	}
}
