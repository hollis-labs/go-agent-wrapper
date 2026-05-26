// Package activity bridges the wrapper's internal lifecycle into the
// shared runtime-event schema in
// github.com/hollis-labs/go-runtime-events.
//
// The wrapper itself does not own the event schema — that lives in
// go-runtime-events so consumers (Tether, Torque, Nanite, Hadron, Stack
// Explorer) can depend on event types without pulling in the wrapper.
// This package is the glue: a Bridge wraps a runtimeevents.Sink and
// pre-binds the per-session identity (App, SessionID, Process,
// Sequencer) so the rest of the wrapper can emit events without
// re-stating that context on every call.
package activity
