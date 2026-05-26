package activity

import (
	"context"

	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Bridge couples the wrapper's per-session emission context to a
// downstream runtimeevents.Sink. Apps construct one Bridge per wrapped
// process; the wrapper's lifecycle, IO, policy, plant, and sandbox
// subsystems share it.
//
// Bridge is a thin convenience over [runtimeevents.Emitter]: it owns the
// Emitter, pre-binds the App / SessionID / Process / Sequencer, and
// forwards Emit calls. Apps that need finer control can fetch the
// underlying Emitter via [Bridge.Emitter].
type Bridge struct {
	em *runtimeevents.Emitter
}

// NewBridge returns a Bridge that writes events to sink. A nil sink is
// allowed and yields a no-op bridge — useful for tests and for callers
// that want to wire activity later. Per-session identity (App,
// SessionID, Process) is bound via [Bridge.Bind] before the first emit.
func NewBridge(sink runtimeevents.Sink) *Bridge {
	if sink == nil {
		sink = runtimeevents.SinkFunc(func(context.Context, runtimeevents.Event) error { return nil })
	}
	return &Bridge{
		em: &runtimeevents.Emitter{
			Sink:      sink,
			Sequencer: runtimeevents.NewSequencer(),
		},
	}
}

// Bind sets the per-session identity used to populate every emitted
// [runtimeevents.Event]. Call once after [NewBridge] and before the
// wrapper starts emitting.
func (b *Bridge) Bind(app, sessionID string, process runtimeevents.Process) {
	b.em.App = app
	b.em.SessionID = sessionID
	b.em.Process = process
}

// Emitter exposes the underlying [runtimeevents.Emitter] for callers
// that need to use EmitOptions, swap the Sink, or share the Sequencer
// across multiple Bridges in the same session-ID space.
func (b *Bridge) Emitter() *runtimeevents.Emitter { return b.em }

// Emit is shorthand for b.Emitter().Emit. See
// [runtimeevents.Emitter.Emit] for parameter semantics.
func (b *Bridge) Emit(
	ctx context.Context,
	kind runtimeevents.EventKind,
	source runtimeevents.Source,
	payload any,
	opts ...runtimeevents.EmitOption,
) error {
	return b.em.Emit(ctx, kind, source, payload, opts...)
}
