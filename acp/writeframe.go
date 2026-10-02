package acp

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// writeDeadliner is a writer whose blocked Write can be interrupted by moving
// its write deadline, as an *os.File pipe and a net.Conn can.
type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

// WriteFrameCtx writes one protocol frame to w, bounded by ctx, for a request
// whose caller waits on ctx (CW-20261001-0211, CW-20261001-0238). Both ACP
// clients that own their transport use it, so a stalled agent cannot pin a
// request write, and the lock behind it, past its caller's ctx.
//
//   - It writes nothing once ctx has ended, and stops waiting for mu when ctx
//     ends, so a caller queued behind a stalled write leaves on its own ctx.
//   - It interrupts a write blocked on a full pipe or socket when ctx ends:
//     through w's write deadline when w has one, otherwise by calling
//     closeTransport. mu is free again afterwards.
//   - If the interrupted write put part of the frame on the wire, the stream
//     no longer has frame boundaries the agent can parse, so it calls
//     closeTransport. When nothing was written the transport stays open: the
//     request was never sent and the next one is whole.
//
// The interrupt runs only once ctx has ended, so a write it cuts short always
// finds ctx.Err() set. That holds for a ctx deadline as much as a cancel:
// setting the write deadline from ctx.Deadline() instead would let the write
// time out a moment before ctx reports it.
//
// closeTransport must not take mu: it may run while mu is held by the very
// write it exists to preempt. It may be called more than once.
//
// The error is ctx.Err() when cancellation interrupts the write, and the
// write's own error otherwise. For writers without a working write deadline,
// a cancellation racing the fallback close can still close the transport just
// after a successful write; the completion guard reduces that window but does
// not make the check and close atomic. Stdio pipes and TCP use deadlines.
// A ctx that never ends leaves the write bounded
// only by the transport closing.
func WriteFrameCtx(ctx context.Context, mu *sync.Mutex, w io.Writer, frame []byte, closeTransport func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := lockCtx(ctx, mu); err != nil {
		return err
	}
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	deadliner, canSet := w.(writeDeadliner)
	var writeDone, closedByUs atomic.Bool
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(fired)
		if canSet && deadliner.SetWriteDeadline(time.Now()) == nil {
			return
		}
		if !writeDone.Load() {
			closedByUs.Store(true)
			closeTransport()
		}
	})
	n, err := w.Write(frame)
	writeDone.Store(true)
	if !stop() {
		// The interrupt ran or is running: let it finish before the
		// deadline is cleared, so it cannot land on the next write.
		<-fired
		if canSet {
			_ = deadliner.SetWriteDeadline(time.Time{})
		}
	}
	if err == nil {
		return nil
	}
	if n > 0 {
		closeTransport()
	}
	interrupted := errors.Is(err, os.ErrDeadlineExceeded) || (closedByUs.Load() &&
		(errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed)))
	if ended := ctx.Err(); ended != nil && interrupted {
		return ended
	}
	return err
}

// lockCtx takes mu, giving up if ctx ends first. A caller queued behind a
// stalled write would otherwise wait on it however short its own deadline.
func lockCtx(ctx context.Context, mu *sync.Mutex) error {
	if ctx.Done() == nil {
		mu.Lock()
		return nil
	}
	if mu.TryLock() {
		return nil
	}
	acquired := make(chan struct{})
	go func() {
		mu.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
		return nil
	case <-ctx.Done():
		go func() {
			<-acquired
			mu.Unlock()
		}()
		return ctx.Err()
	}
}
