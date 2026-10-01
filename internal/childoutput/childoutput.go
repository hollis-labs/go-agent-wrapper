// Package childoutput keeps a spawned ACP child's stdout and stderr readable
// until the reader has seen everything the child wrote.
//
// exec.Cmd.Wait closes the read end of StdoutPipe and StderrPipe as soon as
// the child exits. A reader running concurrently with Wait therefore races it
// and can lose the child's last lines: a session/close reply written just
// before exit was discarded and the pending call failed with "protocol stream
// closed before response" (CW-20261001-0039). The ACP transports own the pipe
// instead (os.Pipe, write end handed to the child), and the waiter drains it
// after Wait with [Drain], as agentkit's agentsessions does.
package childoutput

import (
	"io"
	"os"
	"time"
)

// DrainTimeout bounds how long a waiter keeps reading a child's output after
// the child has exited. An ordinary child closes its end on exit, so the
// reader reaches EOF well inside it; the bound only matters when a descendant
// inherited the write end and keeps it open, or the reader is stalled.
const DrainTimeout = time.Second

// Pipe is one child output stream: the read end the parent keeps and the
// write end the child is given.
type Pipe struct {
	R *os.File
	W *os.File
}

// NewPipe returns a pipe for one child output stream. Assign W to cmd.Stdout
// or cmd.Stderr, call [Pipe.Started] after cmd.Start succeeds, and [Pipe.Close]
// if the child never starts.
func NewPipe() (Pipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return Pipe{}, err
	}
	return Pipe{R: r, W: w}, nil
}

// Started closes the parent's copy of the write end, so only the child (and
// its descendants) keep it and the reader sees EOF once they exit.
func (p Pipe) Started() {
	if p.W != nil {
		_ = p.W.Close()
	}
}

// Close closes both ends; for a child that never started.
func (p Pipe) Close() {
	if p.R != nil {
		_ = p.R.Close()
	}
	if p.W != nil {
		_ = p.W.Close()
	}
}

// Drain waits for the reader to finish (readerDone closed) or for deadline,
// whichever is first, then closes out. Closing out unblocks a reader still
// waiting on a descendant that holds the write end. Drain never waits past
// deadline, so a stalled reader cannot hold up the process-exit report.
func Drain(out io.Closer, readerDone <-chan struct{}, deadline time.Time) {
	if out == nil {
		return
	}
	if readerDone != nil {
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-readerDone:
		case <-timer.C:
		}
		timer.Stop()
	}
	_ = out.Close()
}
