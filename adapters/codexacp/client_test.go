package codexacp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
// real, empirically-verified codex-acp wire shape (see package doc) to
// drive [Client] through a full initialize → session/new →
// session/prompt (with session/update notifications) → response cycle,
// without invoking the real bridge or a real Codex install. Method
// routing is a crude substring match on the raw JSON-RPC line — safe
// here because this test file controls exactly what [Client] sends. The
// fake script is passed as [WithClientBinary], so [Client] spawns it
// directly rather than going through npx — the default `-y pkg@version`
// args are still appended (harmlessly ignored, the script never inspects
// argv).
func fakeACPScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-codex-acp.sh")
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
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed","rawOutput":{"formatted_output":"hi"}}}}\n'
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

	// available_commands_update/usage_update-style variants are
	// deliberately unmapped (informational) — confirm no stray/
	// placeholder events leaked through for the usage_update the fake
	// script emits by checking the delta count matches exactly the two
	// chunks it emits (one thought, one message).
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

func TestClientResumeAcceptsCodexACP162LoadResponseWithoutNewFallback(t *testing.T) {
	skipUnlessSh(t)
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "trace.log")
	script := filepath.Join(dir, "codex-acp-1.6.2-load.sh")
	body := `#!/bin/sh
trace=$3
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$trace"
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true},"authMethods":[]}}\n' "$id"
      ;;
    *'"method":"session/load"'*)
      # Extracted from @agentclientprotocol/codex-acp@1.6.2: load returns
      # modes/configOptions and no sessionId.
      printf '{"jsonrpc":"2.0","id":%s,"result":{"modes":{"availableModes":[],"currentModeId":"default"},"configOptions":[]}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"wrong-fallback"}}\n' "$id"
      ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	c := NewClient(WithClientBinary(script), WithClientBridgePackageSpec("fixture"), WithClientExtraArgs(tracePath))
	const preset = "thread-resume-162"
	if err := c.Launch(context.Background(), acp.LaunchParams{Cwd: dir, SessionIDPreset: preset}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := c.ProviderSessionID(); got != preset {
		t.Fatalf("ProviderSessionID = %q, want %q", got, preset)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	traceBytes, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	trace := string(traceBytes)
	if strings.Count(trace, `"method":"session/load"`) != 1 || strings.Contains(trace, `"method":"session/new"`) {
		t.Fatalf("codex-acp 1.6.2 resume trace:\n%s", trace)
	}
}

func TestClientResolveBridgeCommandDefaultsToPinnedVersion(t *testing.T) {
	c := NewClient()
	binary, args := c.resolveBridgeCommand()
	if binary != "npx" {
		t.Errorf("binary = %q, want npx", binary)
	}
	wantArgs := []string{"-y", defaultBridgePackage + "@" + defaultBridgeVersion}
	if len(args) != len(wantArgs) || args[0] != wantArgs[0] || args[1] != wantArgs[1] {
		t.Errorf("args = %v, want %v", args, wantArgs)
	}
}

func TestClientBuildEnvRespectsExplicitCodexPath(t *testing.T) {
	c := NewClient(WithClientCodexBinary("/should/not/be/used"))
	env := c.buildEnv([]string{"CODEX_PATH=/explicit/override", "FOO=bar"})
	found := false
	for _, kv := range env {
		if kv == "CODEX_PATH=/explicit/override" {
			found = true
		}
		if kv == "CODEX_PATH=/should/not/be/used" {
			t.Errorf("buildEnv overrode a caller-supplied CODEX_PATH; env=%v", env)
		}
	}
	if !found {
		t.Errorf("buildEnv dropped the caller-supplied CODEX_PATH; env=%v", env)
	}
}

func TestClientBuildEnvRespectsWindowsCaseInsensitiveCodexPath(t *testing.T) {
	c := NewClient(WithClientCodexBinary("/must/not/be/appended"))
	env := c.buildEnvForOS([]string{"Codex_Path=C:/explicit/codex.exe", "FOO=bar"}, "windows")
	want := []string{"Codex_Path=C:/explicit/codex.exe", "FOO=bar"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("buildEnvForOS = %#v, want %#v", env, want)
	}
}

func TestClientBuildEnvInjectsResolvedCodexPathWhenAbsent(t *testing.T) {
	c := NewClient(WithClientCodexBinary("/resolved/codex"))
	env := c.buildEnv([]string{"FOO=bar"})
	found := false
	for _, kv := range env {
		if kv == "CODEX_PATH=/resolved/codex" {
			found = true
		}
	}
	if !found {
		t.Errorf("buildEnv did not inject the resolved CODEX_PATH; env=%v", env)
	}
}

func TestClientBuildEnvDoesNotReimportExcludedAmbientCodexPath(t *testing.T) {
	t.Setenv("CODEX_CLI_PATH", "/ambient/secret-codex")
	c := NewClient()
	env := c.buildEnv([]string{"PATH=/definitely/no/codex/here", "SAFE=value"})
	for _, assignment := range env {
		if assignment == "CODEX_PATH=/ambient/secret-codex" || assignment == "CODEX_CLI_PATH=/ambient/secret-codex" {
			t.Fatalf("buildEnv leaked excluded ambient path: %v", env)
		}
	}
}

func TestClientBuildEnvResolvesOnlyFromSuppliedEnvironment(t *testing.T) {
	t.Setenv("CODEX_CLI_PATH", "/ambient/must-not-win")
	c := NewClient()
	env := c.buildEnv([]string{"CODEX_CLI_PATH=/allowed/codex", "PATH=/safe"})
	found := false
	for _, assignment := range env {
		if assignment == "CODEX_PATH=/allowed/codex" {
			found = true
		}
		if assignment == "CODEX_PATH=/ambient/must-not-win" {
			t.Fatalf("buildEnv preferred ambient path: %v", env)
		}
	}
	if !found {
		t.Fatalf("buildEnv did not resolve from supplied environment: %v", env)
	}
}

func launchParams(t *testing.T) acp.LaunchParams {
	t.Helper()
	return acp.LaunchParams{Cwd: t.TempDir()}
}
