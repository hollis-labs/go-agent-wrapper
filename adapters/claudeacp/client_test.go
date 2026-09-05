package claudeacp

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
// real, empirically-verified claude-agent-acp wire shape (see package
// doc) to drive [Client] through a full initialize → session/new →
// session/prompt (with session/update notifications) → response cycle,
// without invoking the real bridge or npx. Method routing is a crude
// substring match on the raw JSON-RPC line — safe here because this test
// file controls exactly what [Client] sends.
//
// Deliberately exercises the two real divergences from
// [adapters/opencodeacp]'s own fake script (see package doc): an
// agent_thought_chunk carrying `content.text` (not a flat `thought`
// field), and a tool_call_update sequence with an intermediate
// no-status/no-output refinement followed by a terminal one carrying
// `rawOutput` (not `result`) and no `isError` field.
func fakeACPScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude-agent-acp.sh")
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
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking..."}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"agent_message_chunk","messageId":"msg_1","content":{"type":"text","text":"pong"}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"bash","kind":"execute","status":"pending","rawInput":{"command":"echo hi"}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","title":"echo hi","rawInput":{"command":"echo hi"}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed","rawOutput":"hi"}}}\n'
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

// fakePermissionACPScript makes session/prompt block on a
// session/request_permission response before it returns the prompt result. The
// first command-line argument is a marker file where the script records the
// exact client response for the test to inspect.
func fakePermissionACPScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude-agent-acp-permission.sh")
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_permission"}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      printf '{"jsonrpc":"2.0","id":99,"method":"session/request_permission","params":{"sessionId":"ses_permission","options":[{"optionId":"allow_once","name":"Allow once","kind":"allow_once"}],"toolCall":{"toolCallId":"call_1","rawInput":{"command":"echo hi"}}}}\n'
      IFS= read -r permission_response
      printf '%s' "$permission_response" > "$1"
      case "$permission_response" in
        *'"outcome":{"outcome":"cancelled"}'*) ;;
        *) exit 42 ;;
      esac
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: test fixture
		t.Fatalf("write permission fake script: %v", err)
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

	c := NewClient(WithClientDirectBinary(script))
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

	// available_commands_update/usage_update are deliberately unmapped
	// (informational) — confirm no stray/placeholder events leaked
	// through for them by checking the delta count matches exactly the
	// two chunks the fake script emits (one thought, one message).
	var deltaCount int
	var thoughtSeen, messageSeen bool
	for _, ev := range evs {
		if ev.Kind == runtimeevents.KindAgentDelta {
			deltaCount++
			var payload struct {
				Content string `json:"content"`
				Phase   string `json:"phase"`
			}
			_ = json.Unmarshal(ev.Payload, &payload)
			switch payload.Phase {
			case "thought":
				thoughtSeen = true
				if payload.Content != "thinking..." {
					t.Errorf("thought delta content = %q, want %q (agent_thought_chunk's content.text field, not a flat `thought` field)", payload.Content, "thinking...")
				}
			case "message":
				messageSeen = true
			}
		}
	}
	if deltaCount != 2 {
		t.Errorf("agent.delta count = %d, want 2 (one thought chunk, one message chunk)", deltaCount)
	}
	if !thoughtSeen || !messageSeen {
		t.Errorf("expected both a thought and a message delta; thoughtSeen=%v messageSeen=%v", thoughtSeen, messageSeen)
	}

	// tool_call_update: two frames in the fake script, only the second
	// carries status/rawOutput (see fakeACPScript's doc comment) — both
	// must still produce a KindAgentToolResult event (unconditional
	// per-frame forwarding), and the terminal one must decode `rawOutput`
	// into the event's `result` key.
	var toolResultCount int
	var sawTerminalResult bool
	for _, ev := range evs {
		if ev.Kind != runtimeevents.KindAgentToolResult {
			continue
		}
		toolResultCount++
		var payload struct {
			Status string `json:"status"`
			Result any    `json:"result"`
		}
		_ = json.Unmarshal(ev.Payload, &payload)
		if payload.Status == "completed" {
			sawTerminalResult = true
			if payload.Result != "hi" {
				t.Errorf("terminal tool_call_update result = %v, want %q (decoded from rawOutput)", payload.Result, "hi")
			}
		}
	}
	if toolResultCount != 2 {
		t.Errorf("agent.tool_result count = %d, want 2 (one intermediate refinement, one terminal)", toolResultCount)
	}
	if !sawTerminalResult {
		t.Error("never saw the terminal tool_call_update (status=completed) translated")
	}
}

func TestClientPermissionRequestDefaultsToCancelledAndUnblocksChild(t *testing.T) {
	skipUnlessSh(t)
	marker := filepath.Join(t.TempDir(), "permission-response.json")
	client := NewClient(
		WithClientDirectBinary(fakePermissionACPScript(t)),
		WithClientExtraArgs(marker),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Launch(ctx, launchParams(t)); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	// The fake child cannot return this prompt until it receives the
	// permission response and verifies the cancelled outcome.
	if err := client.Prompt(ctx, "request a tool"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	events := drainEvents(t, client, 5*time.Second)

	response, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read recorded permission response: %v", err)
	}
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Outcome struct {
				Outcome string `json:"outcome"`
			} `json:"outcome"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &frame); err != nil {
		t.Fatalf("decode recorded permission response: %v", err)
	}
	if string(frame.ID) != "99" || frame.Result.Outcome.Outcome != "cancelled" {
		t.Fatalf("permission response id/outcome = %s/%q, want 99/cancelled; frame=%s", frame.ID, frame.Result.Outcome.Outcome, response)
	}

	var requested, resolved bool
	for _, event := range events {
		switch event.Kind {
		case runtimeevents.KindAgentPermissionRequested:
			requested = true
		case runtimeevents.KindAgentPermissionResolved:
			resolved = true
			var payload struct {
				Allowed bool `json:"allowed"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode permission-resolved payload: %v", err)
			}
			if payload.Allowed {
				t.Fatal("default permission resolution reported allowed=true, want false")
			}
		}
	}
	if !requested || !resolved {
		t.Fatalf("missing permission visibility events: requested=%v resolved=%v events=%+v", requested, resolved, events)
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
	c := NewClient(WithClientDirectBinary(script))
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
	c := NewClient(WithClientDirectBinary(script))
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
	c := NewClient(WithClientDirectBinary(script))
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

func TestClientResolveCommandDefaultsToNpx(t *testing.T) {
	c := NewClient()
	binary, args := c.resolveCommand()
	if binary != "npx" {
		t.Errorf("binary = %q, want npx", binary)
	}
	wantArgs := []string{"-y", defaultBridgePackage}
	if len(args) != len(wantArgs) || args[0] != wantArgs[0] || args[1] != wantArgs[1] {
		t.Errorf("args = %v, want %v", args, wantArgs)
	}
}

func TestClientResolveCommandDirectBinaryBypassesNpx(t *testing.T) {
	c := NewClient(WithClientDirectBinary("/opt/bin/claude-agent-acp"), WithClientExtraArgs("--verbose"))
	binary, args := c.resolveCommand()
	if binary != "/opt/bin/claude-agent-acp" {
		t.Errorf("binary = %q, want the direct override", binary)
	}
	if len(args) != 1 || args[0] != "--verbose" {
		t.Errorf("args = %v, want [--verbose] (no npx/-y/package args when bypassing npx)", args)
	}
}

func TestClientResolveCommandEnvOverrides(t *testing.T) {
	t.Setenv("CLAUDE_ACP_BRIDGE_PATH", "/env/bin/claude-agent-acp")
	c := NewClient()
	binary, args := c.resolveCommand()
	if binary != "/env/bin/claude-agent-acp" {
		t.Errorf("binary = %q, want env override", binary)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want empty", args)
	}
}

func TestClientResolveCommandBridgePackageOverride(t *testing.T) {
	c := NewClient(WithClientBridgePackage("@agentclientprotocol/claude-agent-acp@0.70.0"))
	_, args := c.resolveCommand()
	if len(args) != 2 || args[1] != "@agentclientprotocol/claude-agent-acp@0.70.0" {
		t.Errorf("args = %v, want [-y @agentclientprotocol/claude-agent-acp@0.70.0]", args)
	}
}

func launchParams(t *testing.T) acp.LaunchParams {
	t.Helper()
	return acp.LaunchParams{Cwd: t.TempDir()}
}
