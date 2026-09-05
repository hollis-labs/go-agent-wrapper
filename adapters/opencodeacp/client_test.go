package opencodeacp

import (
	"context"
	"encoding/json"
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
	client := NewClient()
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

func TestCancelAndClosePreemptBackpressuredPermissionResponse(t *testing.T) {
	client := NewClient()
	writer := newBlockingPermissionWriter()
	requests := acp.NewBestEffortPermissionRequests(func(context.Context, acp.PermissionRequest) (acp.PermissionSelection, error) {
		return acp.SelectPermissionOption("allow"), nil
	})
	requests.SetResponseGate(&client.promptCloseMu)
	requests.SetSessionID("session")
	requests.BeginTurn()
	client.mu.Lock()
	client.stdin = writer
	client.sessionID = "session"
	client.sessionClose = true
	client.permissions = requests
	client.mu.Unlock()
	client.turnMu.Lock()
	client.currentTurnID = "turn"
	client.turnMu.Unlock()

	respondDone := make(chan struct{})
	go func() {
		defer close(respondDone)
		requests.Respond(json.RawMessage(`{"sessionId":"session","toolCall":{"toolCallId":"call"},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"}]}`), func(resolution acp.PermissionResolution) error {
			return client.respondToServerRequest(json.RawMessage("99"), resolution.Result(), nil)
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

// fakeACPScript writes a small sh script that speaks JUST ENOUGH of the
// real, empirically-verified opencode-acp wire shape (see package doc)
// to drive [Client] through a full initialize → session/new →
// session/prompt (with session/update notifications) → response cycle,
// without invoking the real opencode binary. Method routing is a crude
// substring match on the raw JSON-RPC line — safe here because this
// test file controls exactly what [Client] sends.
func fakeACPScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-opencode-acp.sh")
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true},"authMethods":[]}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_fake123"}}\n' "$id"
      ;;
    *'"method":"session/load"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"modes":{},"configOptions":[]}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"agent_thought_chunk","thought":"thinking..."}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"agent_message_chunk","messageId":"msg_1","content":{"type":"text","text":"pong"}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"bash","kind":"execute","status":"pending","rawInput":{"command":"echo hi"}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed","isError":false,"result":[{"type":"text","text":"hi"}]}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"usage_update","used":10,"size":100}}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn","usage":{"totalTokens":5}}}\n' "$id"
      ;;
    *'"method":"session/request_permission"'*)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"fake: unexpected permission request"}}\n' "$id"
      ;;
    *'"method":"session/cancel"'*)
      : # notification, no response expected
      ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: test fixture
		t.Fatalf("write fake script: %v", err)
	}
	return script
}

func drainEvents(t *testing.T, c *Client, timeout time.Duration) []runtimeevents.Event {
	t.Helper()
	var out []runtimeevents.Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
			if ev.Kind == runtimeevents.KindTurnCompleted || ev.Kind == runtimeevents.KindTurnFailed {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out waiting for events; collected so far: %+v", out)
		}
	}
}

func hasKind(evs []runtimeevents.Event, kind runtimeevents.EventKind) bool {
	for _, ev := range evs {
		if ev.Kind == kind {
			return true
		}
	}
	return false
}

func TestClientLaunchPromptEvents_FakeSubprocess(t *testing.T) {
	skipUnlessSh(t)
	script := fakeACPScript(t)

	c := NewClient(WithClientBinary(script))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Launch(ctx, launchParams(t)); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	if err := c.Prompt(ctx, "ping"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	evs := drainEvents(t, c, 8*time.Second)

	for _, want := range []runtimeevents.EventKind{
		runtimeevents.KindProcessStarted,
		runtimeevents.KindSessionReady,
		runtimeevents.KindTurnStarted,
		runtimeevents.KindAgentDelta,
		runtimeevents.KindAgentToolUse,
		runtimeevents.KindAgentToolResult,
		runtimeevents.KindTurnCompleted,
	} {
		if !hasKind(evs, want) {
			t.Errorf("missing expected event kind %q in %+v", want, evs)
		}
	}

	// available_commands_update/usage_update are deliberately
	// unmapped (informational) — confirm no stray/placeholder events
	// leaked through for them by checking the delta count matches
	// exactly the two chunks the fake script emits (one thought, one
	// message).
	var deltaCount int
	for _, ev := range evs {
		if ev.Kind == runtimeevents.KindAgentDelta {
			deltaCount++
		}
	}
	if deltaCount != 2 {
		t.Errorf("agent.delta count = %d, want 2 (one thought chunk, one message chunk)", deltaCount)
	}
}

func TestClientPromptBeforeLaunchErrors(t *testing.T) {
	c := NewClient()
	if err := c.Prompt(context.Background(), "hi"); err == nil {
		t.Fatal("Prompt before Launch: want error, got nil")
	}
}

func TestClientLaunchTwiceErrors(t *testing.T) {
	skipUnlessSh(t)
	script := fakeACPScript(t)
	c := NewClient(WithClientBinary(script))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Launch(ctx, launchParams(t)); err != nil {
		t.Fatalf("first Launch: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()
	if err := c.Launch(ctx, launchParams(t)); err == nil {
		t.Fatal("second Launch: want error, got nil")
	}
}

func TestClientCloseIsIdempotentAndClosesEvents(t *testing.T) {
	skipUnlessSh(t)
	script := fakeACPScript(t)
	c := NewClient(WithClientBinary(script))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Launch(ctx, launchParams(t)); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Events channel must be closed (readable to completion, not blocked).
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-c.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("Events channel never closed after Close")
		}
	}
}

func TestClientCancelWithNoActiveTurnIsNoop(t *testing.T) {
	skipUnlessSh(t)
	script := fakeACPScript(t)
	c := NewClient(WithClientBinary(script))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Launch(ctx, launchParams(t)); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()
	if err := c.Cancel(ctx); err != nil {
		t.Fatalf("Cancel with no active turn: %v", err)
	}
}

func TestClientInterruptCapabilityIsTurnAndAnswerableWithoutLaunch(t *testing.T) {
	c := NewClient()
	if got := c.InterruptCapability(); got != adapters.InterruptTurn {
		t.Errorf("InterruptCapability() = %q, want turn", got)
	}
}

func launchParams(t *testing.T) acp.LaunchParams {
	t.Helper()
	return acp.LaunchParams{Cwd: t.TempDir()}
}
