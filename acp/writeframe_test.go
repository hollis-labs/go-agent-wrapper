package acp

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hold the cancellation callback inside SetWriteDeadline after a successful
// real pipe write. The helper must wait for it and reset the resulting deadline
// before a healthy follow-up write can reuse the transport.
type completionDeadlineWriter struct {
	*os.File
	cancel      context.CancelFunc
	entered     chan struct{}
	release     chan struct{}
	wrote       chan struct{}
	deadlineErr error
}

func (w *completionDeadlineWriter) Write(p []byte) (int, error) {
	n, err := w.File.Write(p)
	w.cancel()
	<-w.entered
	close(w.wrote)
	return n, err
}

func (w *completionDeadlineWriter) SetWriteDeadline(d time.Time) error {
	if !d.IsZero() {
		close(w.entered)
		<-w.release
	}
	if w.deadlineErr != nil {
		return w.deadlineErr
	}
	return w.File.SetWriteDeadline(d)
}

func TestWriteFrameCtx_CompletingWriteWaitsForInterruptAndClearsDeadline(t *testing.T) {
	for _, deadlineErr := range []error{nil, os.ErrNoDeadline} {
		t.Run("deadline="+errorLabel(deadlineErr), func(t *testing.T) {
			for range 20 {
				r, pipe, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				w := &completionDeadlineWriter{File: pipe, cancel: cancel, entered: make(chan struct{}), release: make(chan struct{}), wrote: make(chan struct{}), deadlineErr: deadlineErr}
				var mu sync.Mutex
				var closed atomic.Bool
				done := make(chan error, 1)
				go func() {
					done <- WriteFrameCtx(ctx, &mu, w, []byte("one\n"), func() { closed.Store(true); _ = pipe.Close() })
				}()
				select {
				case <-w.wrote:
				case <-time.After(time.Second):
					t.Fatal("write did not complete")
				}
				select {
				case result := <-done:
					t.Fatalf("returned before callback completed: %v", result)
				case <-time.After(5 * time.Millisecond):
				}
				close(w.release)
				select {
				case result := <-done:
					if result != nil {
						t.Fatal(result)
					}
				case <-time.After(time.Second):
					t.Fatal("write did not return")
				}
				if closed.Load() {
					t.Fatal("completed write closed the transport")
				}
				nextCtx, nextCancel := context.WithTimeout(context.Background(), time.Second)
				err = WriteFrameCtx(nextCtx, &mu, pipe, []byte("two\n"), func() { _ = pipe.Close() })
				nextCancel()
				if err != nil {
					t.Fatalf("healthy follow-up write: %v", err)
				}
				got := make([]byte, 8)
				if _, err := io.ReadFull(r, got); err != nil {
					t.Fatal(err)
				}
				if string(got) != "one\ntwo\n" {
					t.Fatalf("frames = %q", got)
				}
				cancel()
				_ = pipe.Close()
				_ = r.Close()
			}
		})
	}
}

func errorLabel(err error) string {
	if err == nil {
		return "supported"
	}
	return "unsupported"
}

type cancelErrorWriter struct {
	cancel context.CancelFunc
	err    error
}

func (w cancelErrorWriter) Write([]byte) (int, error) { w.cancel(); return 0, w.err }

func TestWriteFrameCtx_PreservesUnrelatedWriteError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	want := errors.New("unrelated write failure")
	var mu sync.Mutex
	if err := WriteFrameCtx(ctx, &mu, cancelErrorWriter{cancel: cancel, err: want}, []byte("frame\n"), func() {}); !errors.Is(err, want) {
		t.Fatalf("write error = %v, want %v", err, want)
	}
}
