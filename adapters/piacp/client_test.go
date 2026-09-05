package piacp

import (
	"context"
	"encoding/json"
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

func TestClosePreemptsBlockedPromptWrite(t *testing.T) {
	client := NewClient()
	writer := newBlockingPermissionWriter()
	client.mu.Lock()
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

func TestCloseBackgroundBoundsUnclosedTermination(t *testing.T) {
	waitDone := make(chan struct{})
	close(waitDone)
	client := NewClient()
	client.mu.Lock()
	client.cmd = &exec.Cmd{Process: &os.Process{Pid: -1}}
	client.waitDone = waitDone
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
// real, empirically-verified pi-acp wire shape (see package doc) to
// drive [Client] through a full initialize → session/new →
// session/prompt (with session/update notifications, including pi-acp's
// own real terminal_output/terminal_exit meta shape and a genuine
// session/load resume) → response cycle, without invoking the real
// npx/pi-acp/pi chain. Method routing is a crude substring match on the
// raw JSON-RPC line — safe here because this test file controls exactly
// what [Client] sends.
func fakeACPScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-pi-acp.sh")
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentInfo":{"name":"pi-acp"},"agentCapabilities":{"loadSession":true},"authMethods":[]}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"01a02650-fake-session"}}\n' "$id"
      ;;
    *'"method":"session/load"'*)
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01a02650-fake-session","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"replayed prior turn"}}}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"configOptions":[],"models":{},"modes":{}}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01a02650-fake-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"pi v0.84.2 startup banner"}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01a02650-fake-session","update":{"sessionUpdate":"session_info_update","_meta":{"piAcp":{"queueDepth":0,"running":true}}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01a02650-fake-session","update":{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"for i in 1 2 3; do echo tick; sleep 1; done","kind":"execute","status":"pending","content":[{"type":"terminal","terminalId":"call_1"}],"_meta":{"terminal_info":{"terminal_id":"call_1","cwd":"/tmp"}}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01a02650-fake-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"in_progress","_meta":{"terminal_output":{"terminal_id":"call_1","data":"tick\\n"}}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01a02650-fake-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed","content":[{"type":"diff","path":"/tmp/out.txt","oldText":null,"newText":"tick"}],"_meta":{"terminal_exit":{"terminal_id":"call_1","exit_code":0,"signal":null}}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"01a02650-fake-session","update":{"sessionUpdate":"available_commands_update","availableCommands":[]}}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id"
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

	// tool_call_update fires twice in the fake script (in_progress with
	// terminal_output, then completed with a diff) — both must map to
	// agent.tool_result, matching opencodeacp's "every tool_call_update"
	// precedent (see translate.go).
	var toolResultCount int
	var sawTerminalOutput, sawTerminalExit bool
	for _, ev := range evs {
		if ev.Kind == runtimeevents.KindAgentToolResult {
			toolResultCount++
			payload := string(ev.Payload)
			if strings.Contains(payload, `"terminal_output"`) {
				sawTerminalOutput = true
			}
			if strings.Contains(payload, `"terminal_exit"`) {
				sawTerminalExit = true
			}
		}
	}
	if toolResultCount != 2 {
		t.Errorf("agent.tool_result count = %d, want 2", toolResultCount)
	}
	if !sawTerminalOutput {
		t.Error("expected at least one agent.tool_result payload to carry pi-acp's real terminal_output field")
	}
	if !sawTerminalExit {
		t.Error("expected the completed agent.tool_result payload to carry pi-acp's real terminal_exit field")
	}

	// session_info_update/available_commands_update are deliberately
	// unmapped (informational) — confirm no stray/placeholder events
	// leaked through for them by checking the delta count matches
	// exactly the one message chunk the fake script emits.
	var deltaCount int
	for _, ev := range evs {
		if ev.Kind == runtimeevents.KindAgentDelta {
			deltaCount++
		}
	}
	if deltaCount != 1 {
		t.Errorf("agent.delta count = %d, want 1", deltaCount)
	}
}

// TestClientResumeSession_FakeSubprocess exercises Launch's
// session/load resume path against the fake script's real observed
// shape: a successful session/load whose RESULT carries no sessionId
// (unlike session/new's) — Client must keep using the preset id, not
// decode one out of the response. See client.go's loadSession and the
// package doc's "Real wire behavior" section.
func TestClientResumeSession_FakeSubprocess(t *testing.T) {
	skipUnlessSh(t)
	script := fakeACPScript(t)

	c := NewClient(WithClientBinary(script))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	presetID := "01a02650-preset-session"
	if err := c.Launch(ctx, acp.LaunchParams{Cwd: t.TempDir(), SessionIDPreset: presetID}); err != nil {
		t.Fatalf("Launch (resume): %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	c.mu.Lock()
	got := c.sessionID
	c.mu.Unlock()
	if got != presetID {
		t.Errorf("sessionID after resume = %q, want preset id %q (session/load's result carries no sessionId)", got, presetID)
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

func TestResolveCommandDefaultsToNpx(t *testing.T) {
	c := NewClient()
	binary, args := c.resolveCommand()
	if binary != "npx" {
		t.Errorf("binary = %q, want npx", binary)
	}
	if len(args) != 2 || args[0] != "-y" || args[1] != "pi-acp" {
		t.Errorf("args = %v, want [-y pi-acp]", args)
	}
}

func TestResolveCommandBinaryOverrideOmitsNpxArgs(t *testing.T) {
	c := NewClient(WithClientBinary("/usr/local/bin/pi-acp"), WithClientExtraArgs("--terminal-login"))
	binary, args := c.resolveCommand()
	if binary != "/usr/local/bin/pi-acp" {
		t.Errorf("binary = %q, want override", binary)
	}
	if len(args) != 1 || args[0] != "--terminal-login" {
		t.Errorf("args = %v, want [--terminal-login] (no -y pi-acp prepended for an explicit binary override)", args)
	}
}

func launchParams(t *testing.T) acp.LaunchParams {
	t.Helper()
	return acp.LaunchParams{Cwd: t.TempDir()}
}
