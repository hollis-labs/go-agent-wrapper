package copilotacp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// CW-20261001-0238: Client.call and Prompt write their request under the
// caller's ctx. They used to write with no deadline, so a Copilot agent that
// stopped reading stdin pinned a canceled call, and the writer lock behind it
// blocked every later write.

// stalledCopilotScript answers initialize (id 1) and session/new (id 2), then
// stops reading stdin and never exits on its own.
func stalledCopilotScript(t *testing.T) string {
	t.Helper()
	skipUnlessSh(t)
	script := filepath.Join(t.TempDir(), "stalled-copilot.sh")
	body := `#!/bin/sh
IFS= read -r _
printf '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{},"authMethods":[]}}\n'
IFS= read -r _
printf '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"stalled-session"}}\n'
exec sleep 60
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: a fixture script that must be executable
		t.Fatalf("write stalled copilot script: %v", err)
	}
	return script
}

func pendingCount(c *Client) int {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	return len(c.pending)
}

// assertReleased checks what a ctx-ended request write leaves behind: the
// writer lock free, nothing pending, and, because the pipe or socket took
// part of the frame, a closed transport that refuses the next request.
func assertReleased(t *testing.T, c *Client) {
	t.Helper()
	if !c.writeMu.TryLock() {
		t.Fatal("writeMu is still held after the call returned")
	}
	c.writeMu.Unlock()
	if left := pendingCount(c); left != 0 {
		t.Fatalf("%d pending entries left after the call returned", left)
	}
	afterCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := c.call(afterCtx, "x/after", nil)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a call after a half-written frame = %v; the transport should be closed and refuse it at once", err)
	}
}

// enteredWriter reports when a request write begins, so a test can end the
// call's ctx only once the real pipe or socket write is under way. Building a
// request frame can take longer than a short ctx under -race on a loaded host,
// and a ctx that ends before the write starts tests a different path.
type enteredWriter struct {
	io.WriteCloser
	entered chan struct{}
}

func (w *enteredWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	return w.WriteCloser.Write(p)
}

func (w *enteredWriter) SetWriteDeadline(t time.Time) error {
	if d, ok := w.WriteCloser.(writeDeadliner); ok {
		return d.SetWriteDeadline(t)
	}
	return os.ErrNoDeadline
}

// observeWrites wraps the launched client's transport writer in an
// enteredWriter. closeTransport still closes the real stdin or conn.
func observeWrites(c *Client) chan struct{} {
	entered := make(chan struct{}, 1)
	c.mu.Lock()
	c.writer = &enteredWriter{WriteCloser: nopCloser{c.writer}, entered: entered}
	c.mu.Unlock()
	return entered
}

// nopCloser lets a plain io.Writer stand in as the WriteCloser; the real
// transport is closed through the client's own closeTransport.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func (n nopCloser) SetWriteDeadline(t time.Time) error {
	if d, ok := n.Writer.(writeDeadliner); ok {
		return d.SetWriteDeadline(t)
	}
	return os.ErrNoDeadline
}

// runStalledCall starts a call whose request is too big for the pipe or socket
// to take, ends its ctx the way mode says, and checks it returns promptly with
// that ctx's error. The ctx ends only after the write has begun, so the
// blocked write is what gets interrupted. A "cancel" ends it then; a
// "deadline" ctx has 4 seconds, long enough to build the frame, and the test
// fails if the write had not begun by then.
func runStalledCall(t *testing.T, c *Client, entered chan struct{}, mode string) {
	t.Helper()
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if mode == "deadline" {
		ctx, cancel = context.WithTimeout(context.Background(), 4*time.Second)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := c.call(ctx, "x/big", map[string]any{"data": strings.Repeat("x", 1<<20)})
		done <- err
	}()
	want := context.DeadlineExceeded
	if mode == "cancel" {
		select {
		case <-entered:
		case err := <-done:
			t.Fatalf("call returned before its write began: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("the request write never began")
		}
		time.Sleep(100 * time.Millisecond) // let the write fill the pipe and block
		cancel()
		want = context.Canceled
	}
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("call err = %v, want %v", err, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("call did not return: its request write ignores ctx")
	}
	if mode == "deadline" {
		select {
		case <-entered:
		default:
			t.Fatalf("the deadline passed after %v without the write beginning, so this run did not exercise the blocked write", time.Since(start))
		}
	}
}

// A real child that has stopped reading stdin: a request bigger than the pipe
// blocks in the write. Canceling, or the ctx deadline passing, releases the
// caller and the writer lock.
func TestClientStdio_CtxReleasesRequestWriteToStalledChild(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			c := NewClient(adapters.TransportStdio, WithBinary(stalledCopilotScript(t)))
			launchCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if err := c.Launch(launchCtx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			t.Cleanup(func() {
				closeCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
				defer cancel()
				_ = c.Close(closeCtx)
			})
			entered := observeWrites(c)
			runStalledCall(t, c, entered, mode)
			assertReleased(t, c)
		})
	}
}

// The same over TCP, to a peer that completes the handshake and then stops
// reading. Both ends' socket buffers are shrunk so a 1 MiB request blocks.
func TestClientTCP_CtxReleasesRequestWriteToStalledPeer(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			host, port := fakeACPListener(t, func(t *testing.T, conn net.Conn) {
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetReadBuffer(4096) // so a 1 MiB request cannot sit in socket buffers
				}
				scanner := bufio.NewScanner(conn)
				scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
				if !scanner.Scan() {
					return
				}
				_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{},"authMethods":[]}}` + "\n"))
				if !scanner.Scan() {
					return
				}
				_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"stalled-tcp-session"}}` + "\n"))
				<-release // stop reading
			})
			c := NewClient(adapters.TransportTCP, WithDialOnly(host, port))
			launchCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if err := c.Launch(launchCtx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			t.Cleanup(func() {
				closeCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
				defer cancel()
				_ = c.Close(closeCtx)
			})
			if tcp, ok := c.conn.(*net.TCPConn); ok {
				_ = tcp.SetWriteBuffer(4096)
			}
			entered := observeWrites(c)
			runStalledCall(t, c, entered, mode)
			assertReleased(t, c)
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

func clientWithWriter(w io.WriteCloser) *Client {
	c := NewClient(adapters.TransportStdio)
	c.mu.Lock()
	c.writer = w
	c.stdin = w
	c.sessionID = "session"
	c.mu.Unlock()
	return c
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
// write is released by its own ctx, with nothing on the wire, so the
// transport stays open.
func TestClient_QueuedCallLeavesWhenItsCtxEnds(t *testing.T) {
	w := newStallingWriter(0)
	c := clientWithWriter(w)

	holderCtx, stopHolder := context.WithCancel(context.Background())
	defer stopHolder()
	holder := make(chan error, 1)
	go func() {
		_, err := c.call(holderCtx, "x/holder", nil)
		holder <- err
	}()
	waitEntered(t, w)

	waiterCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	waiter := make(chan error, 1)
	go func() {
		_, err := c.call(waiterCtx, "x/waiter", nil)
		waiter <- err
	}()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued call err = %v, want a deadline error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a call queued behind a stalled write did not leave when its ctx ended")
	}
	if got := w.writes.Load(); got != 1 {
		t.Fatalf("writes = %d, want only the holder's", got)
	}

	stopHolder()
	select {
	case err := <-holder:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stalled call err = %v, want canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stalled write was not released by its ctx")
	}
	if w.closed.Load() {
		t.Fatal("the transport was closed though nothing reached the wire")
	}
	// The queued call's lock waiter takes writeMu and gives it back.
	deadline := time.Now().Add(time.Second)
	for !c.writeMu.TryLock() {
		if time.Now().After(deadline) {
			t.Fatal("writeMu was never released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.writeMu.Unlock()
	if left := pendingCount(c); left != 0 {
		t.Fatalf("%d pending entries left", left)
	}
}

// A write interrupted after part of the frame reached the pipe leaves the agent
// a line it cannot parse, so the transport is closed.
func TestClient_PartiallyWrittenRequestClosesTransport(t *testing.T) {
	w := newStallingWriter(10)
	c := clientWithWriter(w)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.call(ctx, "x/big", nil)
		done <- err
	}()
	waitEntered(t, w)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("call err = %v, want canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call did not return")
	}
	if !w.closed.Load() {
		t.Fatal("the transport was left open after a half-written frame")
	}
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
// the transport, as notify does.
func TestClient_CtxClosesTransportWhenWriterHasNoDeadline(t *testing.T) {
	w := &closeReleasedWriter{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	c := clientWithWriter(w)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.call(ctx, "x/big", nil)
		done <- err
	}()
	<-w.entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("call err = %v, want canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call did not return though ctx ended")
	}
}

// A ctx that has already ended writes nothing and registers nothing.
func TestClient_EndedCtxWritesNothing(t *testing.T) {
	w := newStallingWriter(0)
	c := clientWithWriter(w)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.call(ctx, "x/never", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("call err = %v, want canceled", err)
	}
	if got := w.writes.Load(); got != 0 {
		t.Fatalf("writes = %d, want none", got)
	}
	if left := pendingCount(c); left != 0 {
		t.Fatalf("%d pending entries left", left)
	}
}

type discardWriteCloser struct{}

func (discardWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriteCloser) Close() error                { return nil }

// A call whose request went out but whose ctx ends before the response drops
// its pending entry.
func TestClient_CanceledCallDropsItsPendingEntry(t *testing.T) {
	c := clientWithWriter(discardWriteCloser{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.call(ctx, "x/silent", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call err = %v, want a deadline error", err)
	}
	if left := pendingCount(c); left != 0 {
		t.Fatalf("%d pending entries left after the call gave up", left)
	}
}

// Prompt writes its request under its caller's ctx too: it returns when ctx
// ends, gives the admission gate and the turn back, and still pairs the
// turn.started it emitted with a turn.failed.
func TestClient_PromptWriteHonorsCtx(t *testing.T) {
	w := newStallingWriter(0)
	c := clientWithWriter(w)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Prompt(ctx, "hi") }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Prompt err = %v, want a deadline error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt did not return: its request write ignores ctx")
	}
	if left := pendingCount(c); left != 0 {
		t.Fatalf("%d pending entries left after Prompt write failed", left)
	}
	if !c.promptCloseMu.TryLock() {
		t.Fatal("the prompt admission gate is still held")
	}
	c.promptCloseMu.Unlock()
	c.mu.Lock()
	inFlight := c.turnInFlight
	c.mu.Unlock()
	if inFlight {
		t.Fatal("a turn is still in flight after the failed Prompt")
	}
	evs, ok := drainEvents(c, 2, 2*time.Second)
	if !ok || len(evs) != 2 || evs[0].Kind != runtimeevents.KindTurnStarted || evs[1].Kind != runtimeevents.KindTurnFailed {
		t.Fatalf("events = %+v, want turn.started then turn.failed", evs)
	}
	var payload map[string]any
	if err := json.Unmarshal(evs[1].Payload, &payload); err != nil || !strings.Contains(payload["error"].(string), "deadline") {
		t.Fatalf("turn.failed payload = %s, want the deadline error", evs[1].Payload)
	}
}
