package copilotacp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
	"github.com/hollis-labs/go-sandbox/sandbox"
)

// ---------------------------------------------------------------------
// These tests exercise Client's own wire-level state machine (request/
// response correlation, notification translation, cancel, close) fast
// and offline, against a small fake ACP-speaking script (stdio) or a
// fake in-process listener (TCP) — NOT the real copilot binary. See
// real_test.go for the required real, live end-to-end coverage.
// ---------------------------------------------------------------------

func skipUnlessSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake script needs sh; not running on Windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available on PATH")
	}
}

func TestCancelSharesPromptAdmissionLock(t *testing.T) {
	client := NewClient(adapters.TransportStdio)
	client.promptCloseMu.Lock()
	locked := true
	defer func() {
		if locked {
			client.promptCloseMu.Unlock()
		}
	}()

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- client.Cancel(context.Background())
	}()
	<-started
	select {
	case <-done:
		t.Fatal("Cancel was not linearized with Prompt admission")
	case <-time.After(25 * time.Millisecond):
	}

	client.promptCloseMu.Unlock()
	locked = false
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Cancel did not proceed after Prompt admission lock was released")
	}
}

func TestDecodeServerRequestID(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{raw: `"permission-1"`, want: true},
		{raw: `99`, want: true},
		{raw: `null`, want: true},
		{raw: `{}`, want: false},
		{raw: `true`, want: false},
	}
	for _, test := range tests {
		if _, ok := acp.DecodeJSONRPCRequestID(json.RawMessage(test.raw)); ok != test.want {
			t.Errorf("DecodeJSONRPCRequestID(%s) valid = %v, want %v", test.raw, ok, test.want)
		}
	}
}

type blockingPermissionWriter struct {
	entered  chan struct{}
	release  chan struct{}
	deadline chan time.Time
}

func newBlockingPermissionWriter() *blockingPermissionWriter {
	return &blockingPermissionWriter{
		entered:  make(chan struct{}, 1),
		release:  make(chan struct{}),
		deadline: make(chan time.Time, 1),
	}
}

func (w *blockingPermissionWriter) Write([]byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	select {
	case <-w.release:
		return 0, context.Canceled
	case deadline := <-w.deadline:
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-w.release:
			return 0, context.Canceled
		case <-timer.C:
			return 0, context.DeadlineExceeded
		}
	}
}

func (w *blockingPermissionWriter) SetWriteDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		return nil
	}
	select {
	case w.deadline <- deadline:
	default:
	}
	return nil
}

func (w *blockingPermissionWriter) Close() error {
	select {
	case <-w.release:
	default:
		close(w.release)
	}
	return nil
}

type enteredConn struct {
	net.Conn
	entered chan struct{}
}

func (c *enteredConn) Write(p []byte) (int, error) {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	return c.Conn.Write(p)
}

func TestClosePreemptsBlockedPromptWrite(t *testing.T) {
	client := NewClient(adapters.TransportStdio)
	writer := newBlockingPermissionWriter()
	client.mu.Lock()
	client.writer = writer
	client.stdin = writer
	client.sessionID = "session"
	client.mu.Unlock()
	promptDone := make(chan error, 1)
	go func() { promptDone <- client.Prompt(context.Background(), "blocked") }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("Prompt did not reach blocked transport write")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-promptDone:
		if err == nil {
			t.Fatal("Prompt returned nil after transport preemption")
		}
	case <-ctx.Done():
		t.Fatal("Close did not preempt blocked Prompt write")
	}
}

func TestClosePreemptsBlockedTCPPromptWrite(t *testing.T) {
	rawClientConn, peerConn := net.Pipe()
	defer func() {
		if err := peerConn.Close(); err != nil {
			t.Errorf("close TCP test peer: %v", err)
		}
	}()
	clientConn := &enteredConn{Conn: rawClientConn, entered: make(chan struct{}, 1)}
	client := NewClient(adapters.TransportTCP)
	client.mu.Lock()
	client.conn = clientConn
	client.writer = clientConn
	client.sessionID = "session"
	client.mu.Unlock()

	promptDone := make(chan error, 1)
	go func() { promptDone <- client.Prompt(context.Background(), "blocked") }()
	select {
	case <-clientConn.entered:
	case <-time.After(time.Second):
		t.Fatal("Prompt did not reach blocked TCP transport write")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-promptDone:
		if err == nil {
			t.Fatal("Prompt returned nil after TCP transport preemption")
		}
	case <-ctx.Done():
		t.Fatal("Close did not preempt blocked TCP Prompt write")
	}
}

func TestCloseBackgroundBoundsUnclosedTermination(t *testing.T) {
	readerDone := make(chan struct{})
	close(readerDone)
	client := NewClient(adapters.TransportStdio)
	client.mu.Lock()
	client.started = true
	client.readerDone = readerDone
	client.terminated = make(chan struct{})
	client.mu.Unlock()

	closed := make(chan error, 1)
	go func() { closed <- client.Close(context.Background()) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close remained blocked on an unclosed termination observer")
	}
}

func TestCancelAndClosePreemptBackpressuredPermissionResponse(t *testing.T) {
	client := NewClient(adapters.TransportStdio)
	writer := newBlockingPermissionWriter()
	requests := acp.NewBestEffortPermissionRequests(func(context.Context, acp.PermissionRequest) (acp.PermissionSelection, error) {
		return acp.SelectPermissionOption("allow"), nil
	})
	requests.SetResponseGate(&client.promptCloseMu)
	requests.SetSessionID("session")
	requests.BeginTurn()
	client.mu.Lock()
	client.writer = writer
	client.stdin = writer
	client.sessionID = "session"
	client.sessionClose = true
	client.permissions = requests
	client.turnInFlight = true
	client.currentTurnID = "turn"
	client.mu.Unlock()

	respondDone := make(chan struct{})
	go func() {
		defer close(respondDone)
		requests.Respond(json.RawMessage(`{"sessionId":"session","toolCall":{"toolCallId":"call"},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"}]}`), func(resolution acp.PermissionResolution) error {
			return client.writeServerResponse(json.RawMessage("99"), marshalPayload(resolution.Result()), nil)
		})
	}()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("permission response did not reach blocked writer")
	}

	select {
	case <-respondDone:
	case <-time.After(time.Second):
		t.Fatal("permission response write deadline did not release lifecycle gate")
	}

	cancelDone := make(chan error, 1)
	go func() { cancelDone <- client.Cancel(context.Background()) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("Cancel notification did not reach blocked writer")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close(context.Background()) }()
	select {
	case <-cancelDone:
	case <-time.After(time.Second):
		t.Fatal("Cancel remained blocked behind response I/O")
	}
	select {
	case <-closeDone:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("concurrent Close remained blocked behind Cancel I/O")
	}
}

// writeFakeCopilotScript writes a small sh script that speaks just
// enough real ACP wire shape (in the exact order Client.Launch/Prompt
// send requests: initialize id=1, session/new id=2, session/prompt
// id=3) to drive one full turn, including a session/update notification
// interleaved before the final response — mirroring what the real
// binary actually did in this task's live verification.
func writeFakeCopilotScript(t *testing.T, dir string) string {
	t.Helper()
	body := `#!/bin/sh
IFS= read -r _
printf '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{},"authMethods":[]}}\n'
IFS= read -r _
printf '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"fake-session-1"}}\n'
IFS= read -r _
printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"fake-session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello from fake"}}}}\n'
printf '{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}\n'
IFS= read -r _ 2>/dev/null
`
	script := filepath.Join(dir, "fake-copilot.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake copilot script: %v", err)
	}
	return script
}

func drainEvents(c *Client, want int, timeout time.Duration) ([]runtimeevents.Event, bool) {
	var out []runtimeevents.Event
	deadline := time.After(timeout)
	for len(out) < want {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				return out, false
			}
			out = append(out, ev)
		case <-deadline:
			return out, false
		}
	}
	return out, true
}

func TestClientStdio_LaunchPromptEvents(t *testing.T) {
	skipUnlessSh(t)
	dir := t.TempDir()
	script := writeFakeCopilotScript(t, dir)

	c := NewClient(adapters.TransportStdio, WithBinary(script))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Launch(ctx, acp.LaunchParams{Cwd: dir}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	if err := c.Prompt(ctx, "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	evs, ok := drainEvents(c, 5, 10*time.Second)
	if !ok {
		t.Fatalf("did not observe 5 events in time; got %d: %+v", len(evs), evs)
	}

	wantKinds := []runtimeevents.EventKind{
		runtimeevents.KindProcessStarted,
		runtimeevents.KindSessionReady,
		runtimeevents.KindTurnStarted,
		runtimeevents.KindAgentDelta,
		runtimeevents.KindTurnCompleted,
	}
	for i, want := range wantKinds {
		if evs[i].Kind != want {
			t.Errorf("evs[%d].Kind = %q, want %q", i, evs[i].Kind, want)
		}
	}

	// The agent_message_chunk's text and the turn's stop_reason should
	// both have reached the translated payloads.
	var deltaPayload map[string]any
	if err := json.Unmarshal(evs[3].Payload, &deltaPayload); err != nil {
		t.Fatalf("unmarshal delta payload: %v", err)
	}
	if deltaPayload["content"] != "hello from fake" {
		t.Errorf("delta content = %v, want %q", deltaPayload["content"], "hello from fake")
	}
	if evs[3].TurnID == "" || evs[3].TurnID != evs[2].TurnID || evs[3].TurnID != evs[4].TurnID {
		t.Errorf("turn-scoped events should share one TurnID: %+v", evs[2:5])
	}

	var completedPayload map[string]any
	if err := json.Unmarshal(evs[4].Payload, &completedPayload); err != nil {
		t.Fatalf("unmarshal turn.completed payload: %v", err)
	}
	if completedPayload["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", completedPayload["stop_reason"])
	}
}

func TestClient_PromptBeforeLaunch(t *testing.T) {
	c := NewClient(adapters.TransportStdio)
	if err := c.Prompt(context.Background(), "hi"); err != ErrNotLaunched {
		t.Errorf("Prompt before Launch = %v, want ErrNotLaunched", err)
	}
}

func TestClient_CancelWithoutInFlightTurnIsNoOp(t *testing.T) {
	c := NewClient(adapters.TransportStdio)
	if err := c.Cancel(context.Background()); err != nil {
		t.Errorf("Cancel with no session/turn = %v, want nil (no-op)", err)
	}
}

func TestClient_CloseWithoutLaunchIsSafe(t *testing.T) {
	c := NewClient(adapters.TransportStdio)
	if err := c.Close(context.Background()); err != nil {
		t.Errorf("Close without Launch = %v, want nil", err)
	}
	// Second Close must also be safe (idempotent).
	if err := c.Close(context.Background()); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

func TestClient_InterruptCapabilityIsTurn(t *testing.T) {
	c := NewClient(adapters.TransportStdio)
	if got := c.InterruptCapability(); got != adapters.InterruptTurn {
		t.Errorf("InterruptCapability() = %q, want %q", got, adapters.InterruptTurn)
	}
}

var _ acp.Client = (*Client)(nil)

// ---------------------------------------------------------------------
// TCP-transport tests drive Client against a small in-process fake ACP
// listener (via WithDialOnly) rather than a spawned process — proving
// Client's wire-level logic is genuinely transport-agnostic (the same
// call()/notify()/readLoop code path serves both transports) without
// needing a real binary. real_test.go covers the real spawned-daemon
// TCP path end to end.
// ---------------------------------------------------------------------

// fakeACPListener accepts exactly one connection and hands it to fn for
// the test to drive directly (read/write raw NDJSON lines).
func fakeACPListener(t *testing.T, fn func(t *testing.T, conn net.Conn)) (host string, port int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	addr := l.Addr().(*net.TCPAddr)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		fn(t, conn)
	}()
	return addr.IP.String(), addr.Port
}

func TestClientTCP_DialOnlyRejectsRequiredSandboxBeforeDial(t *testing.T) {
	c := NewClient(adapters.TransportTCP, WithDialOnly("127.0.0.1", 1))
	var outcomes []sandbox.EnforcementOutcome
	err := c.Launch(context.Background(), acp.LaunchParams{
		Cwd: t.TempDir(),
		SandboxPolicy: &sandbox.ResolvedAccessPolicy{
			ID:   "required-remote",
			Mode: sandbox.ConfinementRequired,
		},
		SandboxOutcomeCallback: func(out sandbox.EnforcementOutcome) {
			outcomes = append(outcomes, out)
		},
	})
	if !errors.Is(err, acp.ErrRemoteSandboxUnsupported) {
		t.Fatalf("Launch err = %v, want ErrRemoteSandboxUnsupported", err)
	}
	if len(outcomes) != 1 || outcomes[0].State != sandbox.EnforcementUnsupported {
		t.Fatalf("outcomes = %+v, want one unsupported outcome", outcomes)
	}
}

func TestClientTCP_LaunchPromptEvents(t *testing.T) {
	host, port := fakeACPListener(t, func(t *testing.T, conn net.Conn) {
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		// initialize
		if !scanner.Scan() {
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{},"authMethods":[]}}` + "\n"))

		// session/new
		if !scanner.Scan() {
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"fake-tcp-session"}}` + "\n"))

		// session/prompt
		if !scanner.Scan() {
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"fake-tcp-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello over tcp"}}}}` + "\n"))
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}` + "\n"))
	})

	c := NewClient(adapters.TransportTCP, WithDialOnly(host, port))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Launch(ctx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	if err := c.Prompt(ctx, "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	evs, ok := drainEvents(c, 4, 10*time.Second)
	if !ok {
		t.Fatalf("did not observe 4 events in time; got %d: %+v", len(evs), evs)
	}
	if evs[2].Kind != runtimeevents.KindAgentDelta {
		t.Errorf("evs[2].Kind = %q, want agent.delta", evs[2].Kind)
	}
	var payload map[string]any
	if err := json.Unmarshal(evs[2].Payload, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["content"] != "hello over tcp" {
		t.Errorf("content = %v, want %q", payload["content"], "hello over tcp")
	}
}

func TestClientTCP_CancelSendsRealNotification(t *testing.T) {
	cancelLineCh := make(chan string, 1)

	host, port := fakeACPListener(t, func(t *testing.T, conn net.Conn) {
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		if !scanner.Scan() { // initialize
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}` + "\n"))

		if !scanner.Scan() { // session/new
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"fake-cancel-session"}}` + "\n"))

		if !scanner.Scan() { // session/prompt — do not respond yet
			return
		}

		if !scanner.Scan() { // expected: session/cancel notification
			return
		}
		cancelLineCh <- scanner.Text()

		// Now resolve the turn, as the real binary does after a cancel.
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}` + "\n"))
	})

	c := NewClient(adapters.TransportTCP, WithDialOnly(host, port))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Launch(ctx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	if err := c.Prompt(ctx, "long task"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Give Prompt's write a moment to land before Cancel — Cancel is a
	// no-op unless turnInFlight is already true.
	time.Sleep(100 * time.Millisecond)
	if err := c.Cancel(ctx); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case line := <-cancelLineCh:
		var f wireFrame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("unmarshal captured cancel line: %v", err)
		}
		if f.Method != "session/cancel" {
			t.Errorf("Method = %q, want session/cancel", f.Method)
		}
		if f.ID != nil {
			t.Errorf("session/cancel must be a notification (no id), got id=%v", *f.ID)
		}
		var params sessionCancelParams
		if err := json.Unmarshal(f.Params, &params); err != nil {
			t.Fatalf("unmarshal cancel params: %v", err)
		}
		if params.SessionID != "fake-cancel-session" {
			t.Errorf("sessionId = %q, want fake-cancel-session", params.SessionID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for session/cancel to reach the fake server")
	}
}

func TestClientTCP_PermissionRequestReturnsMethodNotHandled(t *testing.T) {
	respCh := make(chan string, 1)

	host, port := fakeACPListener(t, func(t *testing.T, conn net.Conn) {
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		if !scanner.Scan() { // initialize
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}` + "\n"))

		if !scanner.Scan() { // session/new
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"fake-req-session"}}` + "\n"))

		// Unlike the other four direct ACP clients, Copilot currently routes
		// session/request_permission through its generic unsupported-method
		// path. Lock that actual v0.8.1 behavior down without implementing a
		// responder in this policy-observation task.
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":99,"method":"session/request_permission","params":{"sessionId":"fake-req-session","toolCall":{"toolCallId":"call-1"}}}` + "\n"))

		if scanner.Scan() {
			respCh <- scanner.Text()
		}
		// Keep the transport alive until the client closes it so session.ready
		// cannot race the listener's return and event-channel teardown.
		_ = scanner.Scan()
	})

	c := NewClient(adapters.TransportTCP, WithDialOnly(host, port))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Launch(ctx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	select {
	case line := <-respCh:
		var f wireFrame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if f.ID == nil || *f.ID != 99 {
			t.Fatalf("response id = %v, want 99", f.ID)
		}
		if f.Error == nil {
			t.Fatal("expected an error response declining the server-initiated request, got none")
		}
		if f.Error.Code != -32601 {
			t.Fatalf("permission response error code = %d, want -32601 (method not handled)", f.Error.Code)
		}
		if events, ok := drainEvents(c, 1, time.Second); !ok || len(events) != 1 || events[0].Kind != runtimeevents.KindSessionReady {
			t.Fatalf("unexpected event stream while declining permission request: ok=%v events=%+v", ok, events)
		}
		select {
		case event, open := <-c.Events():
			if open {
				t.Fatalf("Copilot unsupported-method path unexpectedly emitted a permission event: %+v", event)
			}
		default:
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client never responded to the server-initiated request")
	}
}

func TestClientTCP_BestEffortPermissionResponderSelectsOfferedOption(t *testing.T) {
	responseCh := make(chan string, 1)
	host, port := fakeACPListener(t, func(t *testing.T, conn net.Conn) {
		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() { // initialize
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}` + "\n"))
		if !scanner.Scan() { // session/new
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"permission-tcp"}}` + "\n"))
		if !scanner.Scan() { // session/prompt
			return
		}
		var prompt wireFrame
		if err := json.Unmarshal(scanner.Bytes(), &prompt); err != nil || prompt.ID == nil {
			t.Errorf("decode prompt: frame=%s err=%v", scanner.Bytes(), err)
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":"permission-tcp-id","method":"session/request_permission","params":{"sessionId":"permission-tcp","toolCall":{"toolCallId":"call-1","rawInput":{"command":"echo hi"}},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"}]}}` + "\n"))
		if !scanner.Scan() {
			return
		}
		responseCh <- scanner.Text()
		_, _ = conn.Write([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"stopReason":"end_turn"}}`, *prompt.ID) + "\n"))
	})

	client := NewClient(adapters.TransportTCP, WithDialOnly(host, port))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Launch(ctx, acp.LaunchParams{
		Cwd: t.TempDir(),
		BestEffortPermissionRequestResponder: func(_ context.Context, request acp.PermissionRequest) (acp.PermissionSelection, error) {
			if request.SessionID != "permission-tcp" || request.ToolCall.ToolCallID != "call-1" {
				t.Errorf("permission request = %+v", request)
			}
			return acp.SelectPermissionOption("allow"), nil
		},
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	if err := client.Prompt(ctx, "permission over TCP"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	select {
	case response := <-responseCh:
		if !strings.Contains(response, `"id":"permission-tcp-id"`) || !strings.Contains(response, `"outcome":"selected"`) || !strings.Contains(response, `"optionId":"allow"`) {
			t.Fatalf("permission response = %s", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TCP permission response")
	}
}

func TestClient_DoubleLaunchRejected(t *testing.T) {
	host, port := fakeACPListener(t, func(t *testing.T, conn net.Conn) {
		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}` + "\n"))
		if !scanner.Scan() {
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"fake-session"}}` + "\n"))
	})

	c := NewClient(adapters.TransportTCP, WithDialOnly(host, port))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Launch(ctx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	if err := c.Launch(ctx, acp.LaunchParams{Cwd: t.TempDir()}); err != ErrAlreadyLaunched {
		t.Errorf("second Launch = %v, want ErrAlreadyLaunched", err)
	}
}
