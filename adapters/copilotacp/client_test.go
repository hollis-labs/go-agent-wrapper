package copilotacp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
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
	defer c.Close(context.Background())

	if err := c.Prompt(ctx, "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	evs, ok := drainEvents(c, 4, 10*time.Second)
	if !ok {
		t.Fatalf("did not observe 4 events in time; got %d: %+v", len(evs), evs)
	}

	wantKinds := []runtimeevents.EventKind{
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
	if err := json.Unmarshal(evs[2].Payload, &deltaPayload); err != nil {
		t.Fatalf("unmarshal delta payload: %v", err)
	}
	if deltaPayload["content"] != "hello from fake" {
		t.Errorf("delta content = %v, want %q", deltaPayload["content"], "hello from fake")
	}
	if evs[2].TurnID == "" || evs[2].TurnID != evs[1].TurnID || evs[2].TurnID != evs[3].TurnID {
		t.Errorf("turn-scoped events should share one TurnID: %+v", evs[1:4])
	}

	var completedPayload map[string]any
	if err := json.Unmarshal(evs[3].Payload, &completedPayload); err != nil {
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
		defer conn.Close()
		fn(t, conn)
	}()
	return addr.IP.String(), addr.Port
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
	defer c.Close(context.Background())

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
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))

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
	defer c.Close(context.Background())

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
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))

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
	})

	c := NewClient(adapters.TransportTCP, WithDialOnly(host, port))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Launch(ctx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer c.Close(context.Background())

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

func TestClient_DoubleLaunchRejected(t *testing.T) {
	host, port := fakeACPListener(t, func(t *testing.T, conn net.Conn) {
		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
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
	defer c.Close(context.Background())

	if err := c.Launch(ctx, acp.LaunchParams{Cwd: t.TempDir()}); err != ErrAlreadyLaunched {
		t.Errorf("second Launch = %v, want ErrAlreadyLaunched", err)
	}
}
