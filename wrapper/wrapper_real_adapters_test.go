package wrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters/claude"
	"github.com/hollis-labs/go-agent-wrapper/adapters/codex"
	"github.com/hollis-labs/go-agent-wrapper/adapters/opencode"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// ---------------------------------------------------------------------
// TASKS/agent-host-acp/05a (Nanite repo): real-adapter coverage.
//
// Every test above this file drives Wrapper.Run through
// fakeRuntimeAdapter, whose Descriptor deliberately leaves Protocol/
// Transport unset (the "adapter runtime" subprocess-per-turn fallback,
// agentsessions Capabilities{BinaryRequired:true} with no
// StreamingStdio/JsonRpcStdio/ServeHTTP flag). None of those tests ever
// exercised the three runtime kinds go-agent-wrapper's own three real
// shipped adapters (adapters/claude, adapters/codex, adapters/opencode)
// actually select — which is exactly why Wrapper.Run's missing
// WorkspaceDir/LogPath forwarding went undetected until task 06 found
// it empirically. The tests in this file use the real adapters/{claude,
// codex,opencode}.New() Descriptors (real Protocol/Transport pairs),
// pointed at a fake binary via each adapter's real env-var Detect()
// override (CLAUDE_CLI_PATH / CODEX_CLI_PATH / OPENCODE_CLI_PATH), so
// they route through the real streaming-stdio / jsonrpc-stdio /
// serve-http agentkit runtime paths — the ones that hard-error on an
// empty WorkspaceDir/LogPath pair.
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

// workspaceLogPath returns the log file path Wrapper.Run's synthesized
// WorkspaceDir default implies for a session, per resolveWorkspaceLogPath
// and agentkit's own <WorkspaceDir>/logs/session.log convention.
func workspaceLogPath(dir, sessionID string) string {
	return filepath.Join(dir, ".wrapper-workspace", sessionID, "logs", "session.log")
}

// TestRunRealClaudeAdapter_StreamingStdio drives Wrapper.Run against
// adapters/claude's real Descriptor (ProtocolClaudeStreamJSON /
// TransportStdio — agentkit's streaming-stdio runtime kind). Confirms:
//   - No WorkspaceDir/LogPath hard error (Config leaves both empty;
//     Run's synthesized default must be enough for the runtime to spawn
//     and for the log file to actually land where the default implies).
//   - Config.SessionIDPreset reaches the real ClaudeAdapter.BuildArgs
//     `--resume <id>` argv shape end to end.
//   - Config.AutoFireFirstTurn/FirstTurnPayload deliver the kickoff
//     payload over stdin without the caller racing its own SendInput.
//   - The Claude stream-json `system`/`init` session id reaches
//     Process.ProviderSessionID via the existing EventFanout rebind
//     (see Config.OnSessionID's doc comment: Claude's path fires
//     OnSessionID and EventFanout together, so the pre-existing rebind
//     alone already covers it).
func TestRunRealClaudeAdapter_StreamingStdio(t *testing.T) {
	skipUnlessSh(t)
	dir := t.TempDir()

	argvFile := filepath.Join(dir, "argv.txt")
	stdinFile := filepath.Join(dir, "stdin.txt")
	scriptTpl := `#!/bin/sh
printf '%%s\n' "$@" > %s
IFS= read -r line
printf '%%s' "$line" > %s
printf '{"type":"system","subtype":"init","session_id":"claude-fake-session"}\n'
printf '{"type":"result","subtype":"success","result":"ok"}\n'
`
	body := fmt.Sprintf(scriptTpl, argvFile, stdinFile)
	script := filepath.Join(dir, "fake-claude.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}
	t.Setenv("CLAUDE_CLI_PATH", script)

	sink := newCapturingSink()
	w, err := New(Config{
		App:               "test-real-claude",
		Adapter:           claude.New(),
		Activity:          activity.NewBridge(sink),
		Workdir:           dir,
		SessionIDPreset:   "claude-resume-id",
		AutoFireFirstTurn: true,
		FirstTurnPayload:  `{"type":"user","message":{"role":"user","content":"hello"}}`,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()

	select {
	case runErr := <-runErrCh:
		if runErr != nil {
			t.Fatalf("Run: %v (want nil — the fake claude script exits cleanly on its own)", runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return; fake claude script may be stuck waiting on stdin")
	}

	// ----- WorkspaceDir/LogPath: no hard error, default landed where documented -----
	logPath := workspaceLogPath(dir, w.SessionID())
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("synthesized workspace log file %q not created: %v", logPath, err)
	}

	// ----- SessionIDPreset reached real BuildArgs as --resume <id> -----
	argvBytes, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read argv capture: %v", err)
	}
	argv := strings.Split(strings.TrimRight(string(argvBytes), "\n"), "\n")
	if !(len(argv) >= 2 && argv[0] == "--resume" && argv[1] == "claude-resume-id") {
		t.Errorf("argv = %v, want to start with [--resume claude-resume-id] (SessionIDPreset not forwarded to real BuildArgs)", argv)
	}

	// ----- AutoFireFirstTurn/FirstTurnPayload delivered over stdin -----
	stdinBytes, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("read stdin capture: %v", err)
	}
	if got := strings.TrimSpace(string(stdinBytes)); got != `{"type":"user","message":{"role":"user","content":"hello"}}` {
		t.Errorf("first-turn stdin payload = %q, want the configured FirstTurnPayload", got)
	}

	// ----- provider session id reached Process.ProviderSessionID -----
	var gotProviderSessionID string
	for _, ev := range sink.snapshot() {
		if ev.Process.ProviderSessionID != "" {
			gotProviderSessionID = ev.Process.ProviderSessionID
		}
	}
	if gotProviderSessionID != "claude-fake-session" {
		t.Errorf("ProviderSessionID = %q, want claude-fake-session", gotProviderSessionID)
	}

	if !hasKind(sink.snapshot(), runtimeevents.KindSessionReady) {
		t.Error("missing session.ready event")
	}
	if !hasKind(sink.snapshot(), runtimeevents.KindProcessExited) {
		t.Error("missing process.exited event")
	}
}

// TestRunRealCodexAdapter_JsonRpcStdio drives Wrapper.Run against
// adapters/codex's real Descriptor (ProtocolCodexAppServer /
// TransportStdio — agentkit's jsonrpc-stdio runtime kind). Confirms no
// WorkspaceDir/LogPath hard error and that AutoFireFirstTurn/
// FirstTurnPayload delivers over stdin the same way as the
// streaming-stdio path.
//
// Does not assert on SessionIDPreset/argv the way the Claude test does:
// CodexAdapter.BuildArgs in app-server mode intentionally ignores its
// cliSessionID parameter (real code, not a test limitation — Codex's
// app-server resumes via a JSON-RPC thread/resume call, not an argv
// flag), so there is no observable argv difference to assert on for
// this adapter.
func TestRunRealCodexAdapter_JsonRpcStdio(t *testing.T) {
	skipUnlessSh(t)
	dir := t.TempDir()

	stdinFile := filepath.Join(dir, "stdin.txt")
	scriptTpl := `#!/bin/sh
IFS= read -r line
printf '%%s' "$line" > %s
`
	body := fmt.Sprintf(scriptTpl, stdinFile)
	script := filepath.Join(dir, "fake-codex.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake codex script: %v", err)
	}
	t.Setenv("CODEX_CLI_PATH", script)

	sink := newCapturingSink()
	w, err := New(Config{
		App:               "test-real-codex",
		Adapter:           codex.New(),
		Activity:          activity.NewBridge(sink),
		Workdir:           dir,
		AutoFireFirstTurn: true,
		FirstTurnPayload:  "kickoff payload",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()

	select {
	case runErr := <-runErrCh:
		if runErr != nil {
			t.Fatalf("Run: %v (want nil — the fake codex script exits cleanly on its own)", runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return; fake codex script may be stuck waiting on stdin")
	}

	logPath := workspaceLogPath(dir, w.SessionID())
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("synthesized workspace log file %q not created: %v", logPath, err)
	}

	stdinBytes, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("read stdin capture: %v", err)
	}
	if got := strings.TrimSpace(string(stdinBytes)); got != "kickoff payload" {
		t.Errorf("first-turn stdin payload = %q, want %q", got, "kickoff payload")
	}

	if !hasKind(sink.snapshot(), runtimeevents.KindSessionReady) {
		t.Error("missing session.ready event")
	}
	if !hasKind(sink.snapshot(), runtimeevents.KindProcessExited) {
		t.Error("missing process.exited event")
	}
}

// TestRunRealOpenCodeAdapter_ServeHTTP drives Wrapper.Run against
// adapters/opencode's real Descriptor (ProtocolOpenCodeNative /
// TransportHTTPSSE — agentkit's serve-http runtime kind). Confirms no
// WorkspaceDir/LogPath hard error, AutoFireFirstTurn/FirstTurnPayload
// delivery via the real /session/{id}/prompt_async call, and — the
// specific gap Config.OnSessionID's doc comment documents — that the
// provider session id assigned by createSession's direct HTTP response
// (OpenCode's primary session-id delivery path, which never touches
// EventFanout) reaches Process.ProviderSessionID anyway, via the new
// unconditional OnSessionID rebind in Wrapper.Run.
func TestRunRealOpenCodeAdapter_ServeHTTP(t *testing.T) {
	skipUnlessSh(t)
	dir := t.TempDir()

	var mu sync.Mutex
	var gotDirectory, gotPrompt string
	var promptCalls int

	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"healthy":true}`))
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		gotDirectory = r.URL.Query().Get("directory")
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ses_fake_opencode"}`))
	})
	mux.HandleFunc("/session/ses_fake_opencode/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		var reqBody struct {
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err == nil && len(reqBody.Parts) == 1 {
			mu.Lock()
			gotPrompt = reqBody.Parts[0].Text
			promptCalls++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/session/ses_fake_opencode/abort", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`true`))
	})
	mux.HandleFunc("/global/dispose", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`true`))
	})
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		flusher.Flush()
		<-r.Context().Done()
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	scriptBody := fmt.Sprintf("#!/bin/sh\nprintf 'opencode server listening on %s\\n'\ntrap 'exit 0' TERM INT\nwhile true; do sleep 1; done\n", server.URL)
	script := filepath.Join(dir, "fake-opencode.sh")
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write fake opencode script: %v", err)
	}
	t.Setenv("OPENCODE_CLI_PATH", script)

	sink := newCapturingSink()
	w, err := New(Config{
		App:               "test-real-opencode",
		Adapter:           opencode.New(),
		Activity:          activity.NewBridge(sink),
		Workdir:           dir,
		AutoFireFirstTurn: true,
		FirstTurnPayload:  "kickoff payload",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()

	// serve-http Start blocks on health-check + session creation +
	// AutoFireFirstTurn's SendInput before returning, so by the time
	// session.ready is observable, the prompt_async call has already
	// landed and OnSessionID has already fired.
	sink.waitFor(t, runtimeevents.KindSessionReady, 10*time.Second)

	if err := w.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}
	select {
	case runErr := <-runErrCh:
		if runErr != nil {
			t.Errorf("Run: %v (want nil after Stop)", runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Stop")
	}

	logPath := workspaceLogPath(dir, w.SessionID())
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("synthesized workspace log file %q not created: %v", logPath, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotDirectory != dir {
		t.Errorf("session create directory = %q, want %q", gotDirectory, dir)
	}
	if promptCalls != 1 {
		t.Errorf("prompt_async calls = %d, want 1 (AutoFireFirstTurn not delivered)", promptCalls)
	}
	if gotPrompt != "kickoff payload" {
		t.Errorf("prompt text = %q, want %q", gotPrompt, "kickoff payload")
	}

	// The specific gap this task's Config.OnSessionID field closes:
	// createSession's session id never touches EventFanout, so before
	// this fix Process.ProviderSessionID on session.ready would have
	// been empty for every OpenCode session.
	evs := sink.snapshot()
	idxReady := indexOfKind(evs, runtimeevents.KindSessionReady)
	if idxReady < 0 {
		t.Fatal("missing session.ready event")
	}
	if got := evs[idxReady].Process.ProviderSessionID; got != "ses_fake_opencode" {
		t.Errorf("session.ready Process.ProviderSessionID = %q, want ses_fake_opencode (OnSessionID rebind not wired)", got)
	}
}
