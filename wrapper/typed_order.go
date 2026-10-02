package wrapper

import (
	"sync"

	llmtypes "github.com/hollis-labs/go-llm-types"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// typedMarker is the StreamEvent type of a placeholder that holds a typed event's
// place in the fanout channel. It is never a provider's event type, and the
// translator never sees it: it dequeues the typed event the marker stands for.
const typedMarker llmtypes.EventType = "wrapper.typed"

// typedEvent is a typed event translated and waiting for its turn to be emitted.
type typedEvent struct {
	kind    runtimeevents.EventKind
	payload any
}

// typedQueue puts a session's typed events (tool results, refusals, session and
// permission notices) in order with its stream events (tool calls, text, the
// terminal event).
//
// agentkit parses a line, calls TypedEventCallback synchronously and then sends the
// line's stream events on the fanout channel, which a second goroutine translates.
// A typed event emitted from the callback therefore reached the sink before the
// stream events that preceded it, and a consumer that reads the two together (a
// refusal that names a tool call it had not yet seen) got them in the wrong order.
//
// The callback enqueues the event here and sends a marker into the fanout channel,
// where it sits between the previous line's stream events and this line's, and the
// translator emits the event when it reaches the marker. Markers and events are
// one-for-one and both first-in-first-out, so the k-th marker is the k-th event.
// Unlike agentkit's own sends, which drop when the channel is full, the marker send
// waits, so a typed event is never lost to a slow consumer.
type typedQueue struct {
	fanout chan llmtypes.StreamEvent

	// closeMu orders sends against closing the channel: a callback holds the read
	// side across its send, the closer the write side.
	closeMu sync.RWMutex
	closed  bool

	mu    sync.Mutex
	items []typedEvent
}

// enqueue queues ev and puts its marker in the fanout channel. It reports false,
// leaving ev to the caller, when the channel is already closed.
func (q *typedQueue) enqueue(ev typedEvent) bool {
	q.closeMu.RLock()
	defer q.closeMu.RUnlock()
	if q.closed {
		return false
	}
	q.mu.Lock()
	q.items = append(q.items, ev)
	q.mu.Unlock()
	q.fanout <- llmtypes.StreamEvent{Type: typedMarker}
	return true
}

// dequeue returns the oldest queued typed event.
func (q *typedQueue) dequeue() (typedEvent, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return typedEvent{}, false
	}
	ev := q.items[0]
	q.items = q.items[1:]
	return ev, true
}

// closeFanout closes the fanout channel once, after which enqueue refuses.
func (q *typedQueue) closeFanout() {
	q.closeMu.Lock()
	defer q.closeMu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.fanout)
}
