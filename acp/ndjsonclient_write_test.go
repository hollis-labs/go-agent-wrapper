package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// CW-20261001-0211: a request write is bounded by its caller's ctx. Call and
// Prompt used to honor ctx only while awaiting the response, so an agent that
// stopped reading stdin pinned the write, and writeMu behind it, forever.

// stalledAgent answers the handshake, then stops reading stdin and never exits
// on its own.
func stalledAgent(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "stalled-agent.sh")
	body := `#!/bin/sh
reply() {
  id=$(printf '%s' "$1" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  printf '%s\n' "$2" | sed "s/@ID@/$id/"
}
IFS= read -r line
reply "$line" '{"jsonrpc":"2.0","id":@ID@,"result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}}'
IFS= read -r line
reply "$line" '{"jsonrpc":"2.0","id":@ID@,"result":{"sessionId":"ses_stalled"}}'
exec sleep 60
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: test fixture
		t.Fatalf("write stalled agent: %v", err)
	}
	return script
}

// A real child that has stopped reading stdin: a request bigger than the pipe
// blocks in the write. Canceling, or the ctx deadline passing, releases the
// caller and writeMu. The pipe took part of the frame, so the stream is no
// longer parseable and the transport is closed rather than left half-framed.
func TestNDJSONBridgeClient_CtxReleasesRequestWriteToStalledChild(t *testing.T) {
	skipUnlessSh(t)
	for _, name := range []string{"cancel", "deadline"} {
		t.Run(name, func(t *testing.T) {
			forEachComponent(t, func(t *testing.T, component string) {
				client := newTestClient(component, stalledAgent(t))
				launchCtx, launchCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer launchCancel()
				if err := client.Launch(launchCtx, LaunchParams{Cwd: t.TempDir()}); err != nil {
					t.Fatalf("Launch: %v", err)
				}
				t.Cleanup(func() {
					closeCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
					defer cancel()
					_ = client.Close(closeCtx)
				})

				var (
					ctx    context.Context
					cancel context.CancelFunc
				)
				if name == "deadline" {
					ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
				} else {
					ctx, cancel = context.WithCancel(context.Background())
				}
				defer cancel()

				done := make(chan error, 1)
				go func() {
					_, err := client.Call(ctx, "x/big", map[string]any{"data": strings.Repeat("x", 4<<20)})
					done <- err
				}()
				if name == "cancel" {
					select {
					case err := <-done:
						t.Fatalf("Call returned before it was canceled: %v", err)
					case <-time.After(300 * time.Millisecond):
					}
					cancel()
				}
				want := context.Canceled
				if name == "deadline" {
					want = context.DeadlineExceeded
				}
				select {
				case err := <-done:
					if !errors.Is(err, want) {
						t.Fatalf("Call err = %v, want %v", err, want)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("Call did not return: its request write ignores ctx")
				}
				if !client.writeMu.TryLock() {
					t.Fatal("writeMu is still held after the Call returned")
				}
				client.writeMu.Unlock()
				if _, err := client.Call(context.Background(), "x/after", nil); err == nil {
					t.Fatal("a Call after a half-written frame succeeded; the transport should be closed")
				}
				client.pendMu.Lock()
				left := len(client.pending)
				client.pendMu.Unlock()
				if left != 0 {
					t.Fatalf("%d pending entries left after the Call returned", left)
				}
			})
		})
	}
}

// stallingWriter is a stdin whose Write blocks like a full pipe and honors
// SetWriteDeadline like an *os.File does. partial is what an interrupted Write
// reports as written.
type stallingWriter struct {
	mu       sync.Mutex
	deadline time.Time
	wake     chan struct{}
	release  chan struct{}
	entered  chan struct{}
	partial  int
	writes   atomic.Int32
	closed   atomic.Bool
	once     sync.Once
}

func newStallingWriter(partial int) *stallingWriter {
	return &stallingWriter{
		wake:    make(chan struct{}, 1),
		release: make(chan struct{}),
		entered: make(chan struct{}, 8),
		partial: partial,
	}
}

func (w *stallingWriter) Write([]byte) (int, error) {
	w.writes.Add(1)
	w.entered <- struct{}{}
	for {
		w.mu.Lock()
		dl := w.deadline
		w.mu.Unlock()
		var expire <-chan time.Time
		var timer *time.Timer
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return w.partial, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			expire = timer.C
		}
		select {
		case <-w.wake:
		case <-expire:
			return w.partial, os.ErrDeadlineExceeded
		case <-w.release:
			return w.partial, io.ErrClosedPipe
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (w *stallingWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	w.deadline = t
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return nil
}

func (w *stallingWriter) Close() error {
	w.closed.Store(true)
	w.once.Do(func() { close(w.release) })
	return nil
}

func clientWithStdin(component string, stdin io.WriteCloser) *NDJSONBridgeClient {
	client := newTestClient(component, "unused")
	client.mu.Lock()
	client.stdin = stdin
	client.sessionID = "session"
	client.mu.Unlock()
	return client
}

func waitEntered(t *testing.T, w *stallingWriter) {
	t.Helper()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("the request never reached the blocked write")
	}
}

// A caller queued behind a stalled write leaves when its own ctx ends, rather
// than waiting out the write ahead of it, and writes nothing. The stalled
// write itself is released by its own ctx, with nothing on the wire, so the
// transport stays open.
func TestNDJSONBridgeClient_QueuedCallLeavesWhenItsCtxEnds(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		w := newStallingWriter(0)
		client := clientWithStdin(component, w)

		holderCtx, stopHolder := context.WithCancel(context.Background())
		defer stopHolder()
		holder := make(chan error, 1)
		go func() {
			_, err := client.Call(holderCtx, "x/holder", nil)
			holder <- err
		}()
		waitEntered(t, w)

		waiterCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		waiter := make(chan error, 1)
		go func() {
			_, err := client.Call(waiterCtx, "x/waiter", nil)
			waiter <- err
		}()
		select {
		case err := <-waiter:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("queued Call err = %v, want a deadline error", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a Call queued behind a stalled write did not leave when its ctx ended")
		}
		if got := w.writes.Load(); got != 1 {
			t.Fatalf("writes = %d, want only the holder's", got)
		}

		stopHolder()
		select {
		case err := <-holder:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("stalled Call err = %v, want canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the stalled write was not released by its ctx")
		}
		if w.closed.Load() {
			t.Fatal("the transport was closed though nothing reached the wire")
		}
		// The queued Call's lock waiter takes writeMu and gives it back.
		deadline := time.Now().Add(time.Second)
		for !client.writeMu.TryLock() {
			if time.Now().After(deadline) {
				t.Fatal("writeMu was never released")
			}
			time.Sleep(5 * time.Millisecond)
		}
		client.writeMu.Unlock()
		client.pendMu.Lock()
		left := len(client.pending)
		client.pendMu.Unlock()
		if left != 0 {
			t.Fatalf("%d pending entries left", left)
		}
	})
}

// A write interrupted after part of the frame reached the pipe leaves the
// agent a line it cannot parse, so the transport is closed.
func TestNDJSONBridgeClient_PartiallyWrittenRequestClosesTransport(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		w := newStallingWriter(10)
		client := clientWithStdin(component, w)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := client.Call(ctx, "x/big", nil)
			done <- err
		}()
		waitEntered(t, w)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Call err = %v, want canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Call did not return")
		}
		if !w.closed.Load() {
			t.Fatal("the transport was left open after a half-written frame")
		}
	})
}

// closeReleasedWriter is a stdin with no write deadline: only closing it
// releases a blocked Write.
type closeReleasedWriter struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (w *closeReleasedWriter) Write([]byte) (int, error) {
	w.entered <- struct{}{}
	<-w.release
	return 0, io.ErrClosedPipe
}

func (w *closeReleasedWriter) Close() error {
	w.once.Do(func() { close(w.release) })
	return nil
}

// Without a write deadline to set, ctx ending releases the write by closing
// the transport, as Notify does.
func TestNDJSONBridgeClient_CtxClosesTransportWhenStdinHasNoDeadline(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		w := &closeReleasedWriter{release: make(chan struct{}), entered: make(chan struct{}, 1)}
		client := clientWithStdin(component, w)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := client.Call(ctx, "x/big", nil)
			done <- err
		}()
		<-w.entered
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Call err = %v, want canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Call did not return though ctx ended")
		}
	})
}

// A ctx that has already ended writes nothing and registers nothing.
func TestNDJSONBridgeClient_EndedCtxWritesNothing(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		w := newStallingWriter(0)
		client := clientWithStdin(component, w)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := client.Call(ctx, "x/never", nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("Call err = %v, want canceled", err)
		}
		if got := w.writes.Load(); got != 0 {
			t.Fatalf("writes = %d, want none", got)
		}
		client.pendMu.Lock()
		left := len(client.pending)
		client.pendMu.Unlock()
		if left != 0 {
			t.Fatalf("%d pending entries left", left)
		}
	})
}

// A Call whose request went out but whose ctx ends before the response drops
// its pending entry, and a response that arrives afterwards is ignored like
// one for any unknown id.
func TestNDJSONBridgeClient_CanceledCallDropsItsPendingEntry(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		client := clientWithStdin(component, discardWriteCloser{})
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := client.Call(ctx, "x/silent", nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Call err = %v, want a deadline error", err)
		}
		client.pendMu.Lock()
		left := len(client.pending)
		client.pendMu.Unlock()
		if left != 0 {
			t.Fatalf("%d pending entries left after the Call gave up", left)
		}
		if malformed := client.deliverResponse(json.RawMessage("1"), json.RawMessage("{}"), nil); malformed {
			t.Fatal("a late response for an abandoned Call was classified malformed")
		}
	})
}

// Prompt writes its request under its caller's ctx too, and gives the
// admission gate back when the write is released.
func TestNDJSONBridgeClient_PromptWriteHonorsCtx(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		w := newStallingWriter(0)
		client := clientWithStdin(component, w)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- client.Prompt(ctx, "hi") }()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Prompt err = %v, want a deadline error", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Prompt did not return: its request write ignores ctx")
		}
		if !client.promptCloseMu.TryLock() {
			t.Fatal("the prompt admission gate is still held")
		}
		client.promptCloseMu.Unlock()
		client.turnMu.Lock()
		inFlight := client.currentTurnID
		client.turnMu.Unlock()
		if inFlight != "" {
			t.Fatalf("turn %q still in flight after the failed Prompt", inFlight)
		}
	})
}
