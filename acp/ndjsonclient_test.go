package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// The four adapters that share NDJSONBridgeClient. Every behavior test runs
// once per Component so the message text each adapter preserved is exercised.
var ndjsonComponents = []string{"claudeacp", "codexacp", "opencodeacp", "piacp"}

func skipUnlessSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake script needs sh; not running on Windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available on PATH")
	}
}

func nopNotification(*NDJSONBridgeClient, string, json.RawMessage) {}

// newTestClient returns a client whose ResolveCommand runs script with args.
func newTestClient(component, script string, args ...string) *NDJSONBridgeClient {
	return NewNDJSONBridgeClient(NDJSONBridgeConfig{
		Component: component,
		ResolveCommand: func(LaunchParams) (string, []string, error) {
			return script, args, nil
		},
		HandleNotification: nopNotification,
	})
}

func forEachComponent(t *testing.T, fn func(t *testing.T, component string)) {
	t.Helper()
	for _, component := range ndjsonComponents {
		t.Run(component, func(t *testing.T) { fn(t, component) })
	}
}

func TestNDJSONBridgeClient_CancelSharesPromptAdmissionLock(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		client := newTestClient(component, "unused")
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
	})
}

type discardWriteCloser struct{}

func (discardWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriteCloser) Close() error                { return nil }

func TestNDJSONBridgeClient_PromptSharesAdmissionLock(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		client := newTestClient(component, "unused")
		client.mu.Lock()
		client.stdin = discardWriteCloser{}
		client.sessionID = "session"
		var stop context.CancelFunc
		client.lifetimeCtx, stop = context.WithCancel(context.Background())
		client.mu.Unlock()
		defer stop()
		client.promptCloseMu.Lock()
		locked := true
		defer func() {
			if locked {
				client.promptCloseMu.Unlock()
			}
		}()

		done := make(chan error, 1)
		go func() { done <- client.Prompt(context.Background(), "hi") }()
		select {
		case <-done:
			t.Fatal("Prompt was not linearized with Close's admission gate")
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
			t.Fatal("Prompt did not proceed after the admission lock was released")
		}
		_ = client.Close(context.Background())
	})
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

func TestNDJSONBridgeClient_ClosePreemptsBlockedPromptWrite(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		client := newTestClient(component, "unused")
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
	})
}

func TestNDJSONBridgeClient_CloseBackgroundBoundsUnclosedTermination(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		waitDone := make(chan struct{})
		close(waitDone)
		client := newTestClient(component, "unused")
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
	})
}

func TestNDJSONBridgeClient_CancelAndClosePreemptBackpressuredPermissionResponse(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		client := newTestClient(component, "unused")
		writer := newBlockingPermissionWriter()
		requests := NewBestEffortPermissionRequests(func(context.Context, PermissionRequest) (PermissionSelection, error) {
			return SelectPermissionOption("allow"), nil
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
			requests.Respond(json.RawMessage(`{"sessionId":"session","toolCall":{"toolCallId":"call"},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"}]}`), func(resolution PermissionResolution) error {
				return client.RespondToServerRequest(json.RawMessage("99"), resolution.Result(), nil)
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
	})
}

// scriptedAgent writes an sh script that logs every request method it
// receives to the file named by its first argument, then answers the
// handshake. extra is spliced into the method switch.
func scriptedAgent(t *testing.T, extra string) string {
	t.Helper()
	return scriptedAgentWithLoad(t, extra, true)
}

// scriptedAgentWithLoad is scriptedAgent for an agent that does, or does not,
// advertise agentCapabilities.loadSession.
func scriptedAgentWithLoad(t *testing.T, extra string, loadSession bool) string {
	t.Helper()
	capabilities := `{"loadSession":true,"sessionCapabilities":{"close":{}}}`
	if !loadSession {
		capabilities = `{"sessionCapabilities":{"close":{}}}`
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-agent.sh")
	body := `#!/bin/sh
log="$1"
if [ -n "$2" ]; then printf '%s' "$FOO" > "$2"; fi
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  method=$(printf '%s' "$line" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  printf '%s\n' "$method" >> "$log"
  case "$method" in
    initialize)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":` + capabilities + `,"authMethods":[{"id":"agent","type":"agent"}]}}\n' "$id"
      ;;
    session/new)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_new"}}\n' "$id"
      ;;
    session/load|authenticate|session/set_mode|session/set_config_option|session/close)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
` + extra + `
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: test fixture
		t.Fatalf("write fake agent: %v", err)
	}
	return script
}

func readLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: a test helper; callers pass paths under t.TempDir
	if err != nil {
		t.Fatalf("read request log: %v", err)
	}
	return strings.Fields(string(data))
}

func drainEvents(t *testing.T, c *NDJSONBridgeClient, timeout time.Duration) []runtimeevents.Event {
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

func TestNDJSONBridgeClient_HandshakeSequence(t *testing.T) {
	skipUnlessSh(t)
	tests := []struct {
		name   string
		params LaunchParams
		want   []string // requests before Close
	}{
		{"plain", LaunchParams{}, []string{"initialize", "session/new"}},
		{"resume advertised", LaunchParams{SessionIDPreset: "ses_old"}, []string{"initialize", "session/load"}},
		{"authenticate mode and sorted config", LaunchParams{
			AuthMethodID:  "agent",
			SessionModeID: "plan",
			SessionConfig: map[string]any{"zeta": true, "alpha": "x"},
		}, []string{"initialize", "authenticate", "session/new", "session/set_mode", "session/set_config_option", "session/set_config_option"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			forEachComponent(t, func(t *testing.T, component string) {
				logPath := filepath.Join(t.TempDir(), "requests.log")
				c := newTestClient(component, scriptedAgent(t, ""), logPath)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				params := tc.params
				params.Cwd = t.TempDir()
				if err := c.Launch(ctx, params); err != nil {
					t.Fatalf("Launch: %v", err)
				}
				if got := c.ProviderSessionID(); got == "" {
					t.Fatal("no provider session id after Launch")
				}
				got := readLog(t, logPath)
				if strings.Join(got, ",") != strings.Join(tc.want, ",") {
					t.Fatalf("requests = %v, want %v", got, tc.want)
				}
				// Close sends the optional session/close because the agent advertised it.
				if err := c.Close(context.Background()); err != nil {
					t.Fatalf("Close: %v", err)
				}
				after := readLog(t, logPath)
				if last := after[len(after)-1]; last != "session/close" {
					t.Fatalf("last request = %q, want session/close (log %v)", last, after)
				}
			})
		})
	}
}

func TestNDJSONBridgeClient_LaunchEnv(t *testing.T) {
	skipUnlessSh(t)
	path := os.Getenv("PATH")
	tests := []struct {
		name      string
		launchEnv func(LaunchParams) []string
		want      string
	}{
		{"nil hook uses params.Env", nil, "from-params"},
		{"hook overrides params.Env", func(LaunchParams) []string { return []string{"FOO=from-hook", "PATH=" + path} }, "from-hook"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "requests.log")
			envPath := filepath.Join(dir, "env.txt")
			c := NewNDJSONBridgeClient(NDJSONBridgeConfig{
				Component: "testacp",
				ResolveCommand: func(LaunchParams) (string, []string, error) {
					return scriptedAgent(t, ""), []string{logPath, envPath}, nil
				},
				HandleNotification: nopNotification,
				LaunchEnv:          tc.launchEnv,
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			params := LaunchParams{Cwd: dir, Env: []string{"FOO=from-params", "PATH=" + path}}
			if err := c.Launch(ctx, params); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			defer func() { _ = c.Close(context.Background()) }()
			got, err := os.ReadFile(envPath) //nolint:gosec // G304: the test's own temp file
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("child saw FOO=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestNDJSONBridgeClient_ResolveCommandErrorFailsLaunch(t *testing.T) {
	boom := errors.New("no binary")
	c := NewNDJSONBridgeClient(NDJSONBridgeConfig{
		Component:          "testacp",
		ResolveCommand:     func(LaunchParams) (string, []string, error) { return "", nil, boom },
		HandleNotification: nopNotification,
	})
	err := c.Launch(context.Background(), LaunchParams{})
	if !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), "testacp: ") {
		t.Fatalf("Launch error = %v, want %q wrapped under the component prefix", err, boom)
	}
}

func TestNewNDJSONBridgeClient_RequiresHooks(t *testing.T) {
	resolve := func(LaunchParams) (string, []string, error) { return "", nil, nil }
	for name, cfg := range map[string]NDJSONBridgeConfig{
		"no resolver": {Component: "x", HandleNotification: nopNotification},
		"no handler":  {Component: "x", ResolveCommand: resolve},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewNDJSONBridgeClient accepted an incomplete config")
				}
			}()
			NewNDJSONBridgeClient(cfg)
		})
	}
}

func TestNDJSONBridgeClient_LifecycleErrors(t *testing.T) {
	skipUnlessSh(t)
	forEachComponent(t, func(t *testing.T, component string) {
		c := newTestClient(component, scriptedAgent(t, ""), filepath.Join(t.TempDir(), "requests.log"))
		if err := c.Prompt(context.Background(), "hi"); err == nil || !strings.HasPrefix(err.Error(), component+": ") {
			t.Fatalf("Prompt before Launch = %v, want a %s error", err, component)
		}
		if got := c.InterruptCapability(); got != adapters.InterruptTurn {
			t.Errorf("InterruptCapability() = %q, want turn", got)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.Launch(ctx, LaunchParams{Cwd: t.TempDir()}); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if err := c.Launch(ctx, LaunchParams{Cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), component+": Launch called more than once") {
			t.Fatalf("second Launch = %v", err)
		}
		if err := c.Cancel(ctx); err != nil {
			t.Fatalf("Cancel with no active turn: %v", err)
		}
		if err := c.Close(context.Background()); err != nil {
			t.Fatalf("first Close: %v", err)
		}
		if err := c.Close(context.Background()); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		if err := c.Prompt(context.Background(), "late"); err == nil || !strings.Contains(err.Error(), component+": client is closed") {
			t.Fatalf("Prompt after Close = %v", err)
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
	})
}

func TestNDJSONBridgeClient_RPCErrorCarriesComponent(t *testing.T) {
	skipUnlessSh(t)
	forEachComponent(t, func(t *testing.T, component string) {
		dir := t.TempDir()
		script := filepath.Join(dir, "reject.sh")
		body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1}}\n' "$id" ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"nope"}}\n' "$id" ;;
  esac
done
`
		if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: test fixture
			t.Fatal(err)
		}
		c := newTestClient(component, script)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := c.Launch(ctx, LaunchParams{Cwd: dir})
		want := component + ": session/new: " + component + ": jsonrpc error -32000: nope"
		if err == nil || err.Error() != want {
			t.Fatalf("Launch error = %v, want %q", err, want)
		}
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32000 {
			t.Fatalf("error does not unwrap to the *RPCError: %v", err)
		}
	})
}

func TestNDJSONBridgeClient_PermissionRequestDefaultsToCancelledAndUnblocksChild(t *testing.T) {
	skipUnlessSh(t)
	forEachComponent(t, func(t *testing.T, component string) {
		marker := filepath.Join(t.TempDir(), "permission-response.json")
		//nolint:misspell // the fixture child checks ACP's wire outcome "cancelled"
		extra := `    session/prompt)
      printf '{"jsonrpc":"2.0","id":99,"method":"session/request_permission","params":{"sessionId":"ses_new","options":[{"optionId":"allow_once","name":"Allow once","kind":"allow_once"}],"toolCall":{"toolCallId":"call_1","rawInput":{"command":"echo hi"}}}}\n'
      IFS= read -r permission_response
      printf '%s' "$permission_response" > "$3"
      case "$permission_response" in
        *'"outcome":{"outcome":"cancelled"}'*) ;;
        *) exit 42 ;;
      esac
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      ;;`
		logPath := filepath.Join(t.TempDir(), "requests.log")
		c := newTestClient(component, scriptedAgent(t, extra), logPath, "", marker)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.Launch(ctx, LaunchParams{Cwd: t.TempDir()}); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		defer func() { _ = c.Close(context.Background()) }()

		// The fake child cannot return this prompt until it receives the
		// permission response and verifies the canceled outcome.
		if err := c.Prompt(ctx, "request a tool"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		events := drainEvents(t, c, 5*time.Second)

		response, err := os.ReadFile(marker) //nolint:gosec // G304: the test's own temp file
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
		if string(frame.ID) != "99" || frame.Result.Outcome.Outcome != wireCancelled {
			t.Fatalf("permission response id/outcome = %s/%q, want 99/%s; frame=%s", frame.ID, frame.Result.Outcome.Outcome, wireCancelled, response)
		}

		var requested, resolved bool
		for _, event := range events {
			switch event.Kind {
			case runtimeevents.KindAgentPermissionRequested:
				requested = true
			case runtimeevents.KindAgentPermissionResolved:
				resolved = true
				var payload struct {
					Allowed bool   `json:"allowed"`
					Reason  string `json:"reason"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatalf("decode permission-resolved payload: %v", err)
				}
				if payload.Allowed {
					t.Fatal("default permission resolution reported allowed=true, want false")
				}
				if want := component + ": no approval handler configured"; payload.Reason != want {
					t.Fatalf("resolution reason = %q, want %q", payload.Reason, want)
				}
			default:
			}
		}
		if !requested || !resolved {
			t.Fatalf("missing permission visibility events: requested=%v resolved=%v events=%+v", requested, resolved, events)
		}
	})
}

func TestNDJSONBridgeClient_UnknownServerRequestIsAnsweredWithMethodNotFound(t *testing.T) {
	skipUnlessSh(t)
	forEachComponent(t, func(t *testing.T, component string) {
		marker := filepath.Join(t.TempDir(), "reply.json")
		extra := `    session/prompt)
      printf '{"jsonrpc":"2.0","id":7,"method":"fs/read_text_file","params":{}}\n'
      IFS= read -r reply
      printf '%s' "$reply" > "$3"
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      ;;`
		logPath := filepath.Join(t.TempDir(), "requests.log")
		c := newTestClient(component, scriptedAgent(t, extra), logPath, "", marker)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.Launch(ctx, LaunchParams{Cwd: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close(context.Background()) }()
		if err := c.Prompt(ctx, "go"); err != nil {
			t.Fatal(err)
		}
		drainEvents(t, c, 5*time.Second)
		reply, err := os.ReadFile(marker) //nolint:gosec // G304: the test's own temp file
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			ID    int `json:"id"`
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(reply, &frame); err != nil {
			t.Fatalf("decode %s: %v", reply, err)
		}
		if frame.ID != 7 || frame.Error.Code != -32601 || frame.Error.Message != component+": no handler configured for server-initiated method fs/read_text_file" {
			t.Fatalf("reply = %s", reply)
		}
	})
}

func TestNDJSONBridgeClient_NotificationsReachTheConfiguredHandler(t *testing.T) {
	skipUnlessSh(t)
	extra := `    session/prompt)
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"marker":"one"}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      ;;`
	var mu sync.Mutex
	var got []string
	var turnID string
	c := NewNDJSONBridgeClient(NDJSONBridgeConfig{
		Component: "testacp",
		ResolveCommand: func(LaunchParams) (string, []string, error) {
			return scriptedAgent(t, extra), []string{filepath.Join(t.TempDir(), "requests.log")}, nil
		},
		HandleNotification: func(c *NDJSONBridgeClient, method string, params json.RawMessage) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, method+" "+string(params))
			turnID = c.CurrentTurnID()
			c.Emit(runtimeevents.Event{Kind: runtimeevents.KindAgentDelta, TurnID: turnID})
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Launch(ctx, LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(context.Background()) }()
	if err := c.Prompt(ctx, "go"); err != nil {
		t.Fatal(err)
	}
	events := drainEvents(t, c, 5*time.Second)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != `session/update {"marker":"one"}` {
		t.Fatalf("handler saw %v", got)
	}
	var delta *runtimeevents.Event
	for i := range events {
		if events[i].Kind == runtimeevents.KindAgentDelta {
			delta = &events[i]
		}
	}
	if delta == nil || delta.TurnID == "" || delta.TurnID != turnID {
		t.Fatalf("emitted delta = %+v, want it stamped with the in-flight turn id %q", delta, turnID)
	}
}

// readLoopHarness drives readLoop directly over stdout so the reader's
// terminal facts can be inspected without a process.
func readLoopHarness(t *testing.T, stdout string) (TransportTerminationResult, []Diagnostic) {
	t.Helper()
	c := newTestClient("testacp", "unused")
	var mu sync.Mutex
	var diagnostics []Diagnostic
	c.permissions = NewBestEffortPermissionRequests(nil)
	c.diagnostic = func(d Diagnostic) {
		mu.Lock()
		defer mu.Unlock()
		diagnostics = append(diagnostics, d)
	}
	c.termination = NewTransportTermination(true)
	c.readLoop(strings.NewReader(stdout))
	c.termination.ReportProcess(TransportProcessResult{})
	result := c.termination.Coordinate(nil, nil)
	mu.Lock()
	defer mu.Unlock()
	return result, append([]Diagnostic(nil), diagnostics...)
}

func TestNDJSONBridgeClient_NonNumericResponseID_ClassifiedMalformed(t *testing.T) {
	result, diagnostics := readLoopHarness(t, `{"jsonrpc":"2.0","id":"abc","result":{}}`+"\n")
	if !result.Read.Malformed || !result.ShouldEmitProcessExit() {
		t.Fatalf("termination = %+v, want the read classified malformed", result)
	}
	if len(diagnostics) != 1 || diagnostics[0].Kind != DiagnosticProtocol || !strings.Contains(diagnostics[0].Message, "non-numeric JSON-RPC response id") {
		t.Fatalf("diagnostics = %+v, want one protocol diagnostic naming the non-numeric response id", diagnostics)
	}
}

// A numeric id nobody is waiting for is a late or duplicate response, not
// malformed input: it stays a silent drop.
func TestNDJSONBridgeClient_UnknownNumericResponseIDIsNotMalformed(t *testing.T) {
	result, diagnostics := readLoopHarness(t, `{"jsonrpc":"2.0","id":424242,"result":{}}`+"\n")
	if result.Read.Malformed || len(diagnostics) != 0 {
		t.Fatalf("termination = %+v diagnostics = %+v, want neither", result, diagnostics)
	}
}

func TestNDJSONBridgeClient_PendingCallsFailWhenStreamCloses(t *testing.T) {
	c := newTestClient("testacp", "unused")
	c.permissions = NewBestEffortPermissionRequests(nil)
	c.termination = NewTransportTermination(true)
	respCh := make(chan rpcResponse, 1)
	c.pending[1] = pendingRPCResponse{response: respCh}
	c.readLoop(strings.NewReader(""))
	select {
	case resp := <-respCh:
		if resp.err == nil || resp.err.Error() != "testacp: jsonrpc error -32000: testacp: protocol stream closed before response" {
			t.Fatalf("pending call error = %v", resp.err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending call was never failed when the protocol stream closed")
	}
}
