package acp

import (
	"sync"
	"time"
)

// TransportReadResult records the protocol reader's terminal facts before the
// public event stream is closed. Malformed is independent of Err because a
// scanner can successfully reach EOF after observing a malformed frame.
type TransportReadResult struct {
	Malformed bool
	Err       error
}

// TransportProcessResult records the spawned child's final status.
type TransportProcessResult struct {
	ExitCode int
	Err      error
}

// TransportTerminationResult is the joined reader/process outcome returned by
// TransportTermination.Coordinate.
type TransportTerminationResult struct {
	Read              TransportReadResult
	Process           TransportProcessResult
	ProcessExpected   bool
	ReadBeforeProcess bool
	Disconnected      bool
	KilledAfterRead   bool
}

// ShouldEmitProcessExit reports whether the process outcome was independently
// observed. A process killed solely to reap it after an unexpected transport
// EOF is a consequence of the disconnect, not the cause, and must not
// overwrite that classification with child_exit. A malformed read still emits
// the physical process exit; malformed_stream has higher lifecycle precedence.
func (r TransportTerminationResult) ShouldEmitProcessExit() bool {
	if !r.ProcessExpected {
		return false
	}
	if r.Read.Malformed {
		return true
	}
	return !r.Disconnected
}

// TransportTermination is the one join point for a client's protocol-reader
// and child-process goroutines. Neither producer closes the public Events
// channel. The client waits here, after every read diagnostic is recorded, and
// only then emits any causal process exit and closes Events.
//
// When the reader ends while a spawned child is still live, Coordinate invokes
// stop, gives ordinary teardown a bounded grace period, then invokes kill to
// make cleanup finite and joins the resulting process completion. The grace
// period controls resource reaping only; classification is derived from the
// joined facts, not which side wins a timer.
type TransportTermination struct {
	processExpected bool
	readDone        chan TransportReadResult
	processDone     chan TransportProcessResult
	readOnce        sync.Once
	processOnce     sync.Once
}

func NewTransportTermination(processExpected bool) *TransportTermination {
	return &TransportTermination{
		processExpected: processExpected,
		readDone:        make(chan TransportReadResult, 1),
		processDone:     make(chan TransportProcessResult, 1),
	}
}

// ReportRead records the protocol reader's final facts. The first report wins.
func (t *TransportTermination) ReportRead(result TransportReadResult) {
	if t == nil {
		return
	}
	t.readOnce.Do(func() { t.readDone <- result })
}

// ReportProcess records the child process's final facts. The first report wins.
func (t *TransportTermination) ReportProcess(result TransportProcessResult) {
	if t == nil {
		return
	}
	t.processOnce.Do(func() { t.processDone <- result })
}

// Coordinate joins reader and process termination. kill may be nil only when
// processExpected is false.
func (t *TransportTermination) Coordinate(stop func(), kill func() error) TransportTerminationResult {
	if t == nil {
		return TransportTerminationResult{}
	}
	result := TransportTerminationResult{ProcessExpected: t.processExpected}
	if !t.processExpected {
		result.Read = <-t.readDone
		return result
	}

	select {
	case result.Process = <-t.processDone:
		result.Read = <-t.readDone
		return result
	case result.Read = <-t.readDone:
		result.ReadBeforeProcess = true
	}

	// A process-caused EOF is followed by its authoritative wait result.
	// Give that result a cleanup grace period before declaring the still-live
	// child disconnected and actively tearing it down. This timer is a
	// liveness bound, not a race between diagnostic and exit classification:
	// all reader facts, including malformed input, are already recorded.
	select {
	case result.Process = <-t.processDone:
		return result
	case <-time.After(2 * time.Second):
	}
	result.Disconnected = true
	if stop != nil {
		stop()
	}
	select {
	case result.Process = <-t.processDone:
		return result
	case <-time.After(2 * time.Second):
	}
	result.KilledAfterRead = true
	if kill != nil {
		_ = kill() // ReportProcess remains the authoritative exit fact.
	}
	result.Process = <-t.processDone
	return result
}
