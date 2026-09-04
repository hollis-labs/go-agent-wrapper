package wrapper

import (
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
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/adapters/claudeacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/codexacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/copilotacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/opencodeacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/piacp"
	"github.com/hollis-labs/go-agent-wrapper/policy"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestACPWrapperRealSubprocessLifecycleAllAdapters(t *testing.T) {
	factories := map[string]func(string) adapters.Adapter{
		"claude": func(path string) adapters.Adapter {
			return claudeacp.New(claudeacp.WithDirectBinary(path))
		},
		"codex": func(path string) adapters.Adapter {
			return codexacp.New(codexacp.WithBinary(path), codexacp.WithBridgePackageSpec("fixture"))
		},
		"copilot": func(path string) adapters.Adapter {
			return copilotacp.New(copilotacp.WithAdapterBinary(path))
		},
		"opencode": func(path string) adapters.Adapter {
			return opencodeacp.New(opencodeacp.WithBinary(path))
		},
		"pi": func(path string) adapters.Adapter {
			return piacp.New(piacp.WithBinary(path))
		},
	}

	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sessionPreset := "resume-123"
			providerSessionID := "resume-123"
			sessionMethod := `"method":"session/load"`
			// Exercise the fresh-session side of the same lifecycle on one
			// shipped adapter; the remaining adapters prove resume support.
			if name == "opencode" {
				sessionPreset = ""
				providerSessionID = "fresh-456"
				sessionMethod = `"method":"session/new"`
			}
			dir := t.TempDir()
			tracePath := filepath.Join(dir, "trace.log")
			fixturePath := writeACPFixture(t, dir, tracePath, "lifecycle")
			manager := acp.NewManager()
			sink := newCapturingSink()
			observer := &recordingPolicyObserver{finding: policy.Finding{
				Recommendation: policy.RecommendationBlock,
				RuleID:         "fixture-observation",
				Message:        "advisory only",
			}}
			var diagnostics []acp.Diagnostic
			w, err := New(Config{
				App:              "acp-fixture-" + name,
				Adapter:          factory(fixturePath),
				Activity:         activity.NewBridge(sink),
				Workdir:          dir,
				SessionID:        "wrapper-" + name,
				SessionIDPreset:  sessionPreset,
				SystemPrompt:     "fixture system instruction",
				ACPManager:       manager,
				ACPAuthMethodID:  "fixture-auth",
				ACPSessionModeID: "plan",
				ACPSessionConfig: map[string]any{"zeta": "high", "alpha": true},
				OnACPDiagnostic:  func(d acp.Diagnostic) { diagnostics = append(diagnostics, d) },
				PolicyObserver:   observer,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			runErrCh := make(chan error, 1)
			go func() { runErrCh <- w.Run(ctx) }()
			waitForACPReady(t, manager, w.SessionID())
			if got := w.ProviderSessionID(); got != providerSessionID {
				t.Fatalf("ProviderSessionID = %q, want %q", got, providerSessionID)
			}

			if err := w.SendInput(ctx, []byte("first turn")); err != nil {
				t.Fatalf("first SendInput: %v", err)
			}
			sink.waitFor(t, runtimeevents.KindAgentDelta, 5*time.Second)
			if err := w.CancelTurn(ctx); err != nil {
				t.Fatalf("CancelTurn: %v", err)
			}
			waitForACPTurnOutcome(t, w, acp.OutcomeCanceled)
			if !manager.IsLive(w.SessionID()) {
				t.Fatal("session was not live after turn-scoped cancellation")
			}

			if err := w.SendInput(ctx, []byte("second turn")); err != nil {
				t.Fatalf("second SendInput: %v", err)
			}
			waitForACPReady(t, manager, w.SessionID())
			if err := w.Stop(ctx); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			select {
			case err := <-runErrCh:
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return after Stop")
			}
			if manager.Len() != 0 {
				t.Fatalf("Manager.Len = %d after Stop, want 0", manager.Len())
			}
			if len(diagnostics) != 0 {
				t.Fatalf("unexpected diagnostics: %+v", diagnostics)
			}
			events := sink.snapshot()
			toolIndex := indexOfKind(events, runtimeevents.KindAgentToolUse)
			policyIndex := indexOfKind(events, runtimeevents.KindPolicyBlock)
			if toolIndex < 0 || policyIndex <= toolIndex {
				t.Fatalf("ACP policy observation order: tool=%d policy=%d kinds=%v", toolIndex, policyIndex, sink.kinds())
			}
			observer.mu.Lock()
			observationCalls := observer.calls
			observer.mu.Unlock()
			if observationCalls != 1 {
				t.Fatalf("PolicyObserver calls = %d, want 1", observationCalls)
			}
			if !hasKind(events, runtimeevents.KindSessionProcessing) || !hasKind(events, runtimeevents.KindSessionIdle) {
				t.Fatalf("missing managed liveness events: %v", sink.kinds())
			}
			processExits := 0
			for _, event := range events {
				if event.Kind == runtimeevents.KindProcessExited {
					processExits++
				}
			}
			if processExits != 1 {
				t.Fatalf("process.exited events = %d, want exactly 1", processExits)
			}

			traceBytes, err := os.ReadFile(tracePath)
			if err != nil {
				t.Fatalf("read trace: %v", err)
			}
			trace := string(traceBytes)
			assertTraceOrder(t, trace,
				`"method":"initialize"`,
				`"method":"authenticate"`,
				sessionMethod,
				`"method":"session/set_mode"`,
				`"configId":"alpha"`,
				`"configId":"zeta"`,
				`fixture system instruction\n\nfirst turn`,
				`"method":"session/cancel"`,
				`second turn`,
				`"method":"session/close"`,
			)
			if !strings.Contains(trace, `"type":"boolean","value":true`) {
				t.Fatalf("boolean session config omitted its type discriminator:\n%s", trace)
			}
			if got := strings.Count(trace, "fixture-cleanup"); got != 1 {
				t.Fatalf("fixture cleanup count = %d, want exactly 1; trace:\n%s", got, trace)
			}
		})
	}
}

func TestACPWrapperRealSubprocessNormalizesMalformedDisconnectAndChildExit(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		want error
	}{
		{name: "malformed", mode: "malformed", want: acp.ErrMalformedStream},
		{name: "disconnect", mode: "disconnect", want: acp.ErrDisconnected},
		{name: "child-exit", mode: "child-exit", want: acp.ErrChildExit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tracePath := filepath.Join(dir, "trace.log")
			fixturePath := writeACPFixture(t, dir, tracePath, tc.mode)
			manager := acp.NewManager()
			var diagnostics []acp.Diagnostic
			sink := newCapturingSink()
			w, err := New(Config{
				App: "acp-failure", Adapter: opencodeacp.New(opencodeacp.WithBinary(fixturePath)),
				Activity: activity.NewBridge(sink), Workdir: dir,
				SessionID: "failure-" + tc.name, ACPManager: manager,
				OnACPDiagnostic: func(d acp.Diagnostic) { diagnostics = append(diagnostics, d) },
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = w.Run(ctx)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Run error = %v, want %v", err, tc.want)
			}
			if manager.Len() != 0 {
				t.Fatalf("Manager.Len = %d after failure, want 0", manager.Len())
			}
			traceBytes, readErr := os.ReadFile(tracePath)
			if readErr != nil {
				t.Fatalf("read cleanup trace: %v", readErr)
			}
			if got := strings.Count(string(traceBytes), "fixture-cleanup"); got != 1 {
				t.Fatalf("fixture cleanup count = %d, want exactly 1; trace:\n%s", got, traceBytes)
			}
			if tc.mode == "malformed" {
				if len(diagnostics) == 0 || diagnostics[0].Kind != acp.DiagnosticMalformedJSON {
					t.Fatalf("malformed diagnostics = %+v", diagnostics)
				}
				joined := fmt.Sprintf("%+v", diagnostics)
				if strings.Contains(joined, "fixture-secret") || !strings.Contains(joined, "[REDACTED]") {
					t.Fatalf("malformed diagnostic was not safely redacted: %s", joined)
				}
			}
			if tc.mode == "child-exit" {
				exitEvent, ok := firstKind(sink.snapshot(), runtimeevents.KindProcessExited)
				if !ok {
					t.Fatal("missing process.exited event")
				}
				var payload struct {
					ExitCode int `json:"exit_code"`
				}
				if decodeErr := json.Unmarshal(exitEvent.Payload, &payload); decodeErr != nil || payload.ExitCode != 7 {
					t.Fatalf("process.exited payload = %s, want exit_code 7 (decode=%v)", exitEvent.Payload, decodeErr)
				}
			}
		})
	}
}

func firstKind(events []runtimeevents.Event, kind runtimeevents.EventKind) (runtimeevents.Event, bool) {
	for _, event := range events {
		if event.Kind == kind {
			return event, true
		}
	}
	return runtimeevents.Event{}, false
}

func TestACPWrapperCopilotTCPRealSubprocessLifecycle(t *testing.T) {
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "acpfixture")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", fixturePath, "./testdata/acpfixture")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build TCP fixture: %v\n%s", err, output)
	}
	tracePath := filepath.Join(dir, "trace.log")
	t.Setenv("ACP_FIXTURE_TRACE", tracePath)
	port := reserveTCPPort(t)
	manager := acp.NewManager()
	sink := newCapturingSink()
	w, err := New(Config{
		App: "acp-fixture-copilot-tcp",
		Adapter: copilotacp.New(
			copilotacp.WithAdapterBinary(fixturePath),
			copilotacp.WithAdapterTransport(adapters.TransportTCP),
			copilotacp.WithAdapterPort(port),
		),
		Activity: activity.NewBridge(sink), Workdir: dir,
		SessionID: "wrapper-copilot-tcp", SessionIDPreset: "resume-tcp",
		SystemPrompt: "tcp system", ACPManager: manager,
		ACPAuthMethodID: "fixture-auth", ACPSessionModeID: "plan",
		ACPSessionConfig: map[string]any{"alpha": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	waitForACPReady(t, manager, w.SessionID())
	if got := w.ProviderSessionID(); got != "resume-tcp" {
		t.Fatalf("ProviderSessionID = %q, want resume-tcp", got)
	}
	if err := w.SendInput(ctx, []byte("tcp first")); err != nil {
		t.Fatal(err)
	}
	sink.waitFor(t, runtimeevents.KindAgentDelta, 5*time.Second)
	if err := w.CancelTurn(ctx); err != nil {
		t.Fatal(err)
	}
	waitForACPTurnOutcome(t, w, acp.OutcomeCanceled)
	if err := w.SendInput(ctx, []byte("tcp second")); err != nil {
		t.Fatal(err)
	}
	waitForACPReady(t, manager, w.SessionID())
	if err := w.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-runErrCh; err != nil {
		t.Fatalf("Run: %v", err)
	}
	traceBytes, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	trace := string(traceBytes)
	assertTraceOrder(t, trace,
		`"method":"initialize"`, `"method":"authenticate"`,
		`"method":"session/load"`, `"method":"session/set_mode"`,
		`"method":"session/set_config_option"`, `tcp system\n\ntcp first`,
		`"method":"session/cancel"`, `tcp second`, `"method":"session/close"`,
	)
	if got := strings.Count(trace, "fixture-cleanup"); got != 1 {
		t.Fatalf("fixture cleanup count = %d, want exactly 1; trace:\n%s", got, trace)
	}
}

func reserveTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func writeACPFixture(t *testing.T, dir, tracePath, mode string) string {
	t.Helper()
	scriptPath := filepath.Join(dir, "acp-fixture.sh")
	body := fmt.Sprintf(`#!/bin/sh
trace=%s
mode=%s
trap 'printf '"'"'fixture-cleanup\n'"'"' >> "$trace"' EXIT
prompt_count=0
held_id=
active_session=
while IFS= read -r line; do
  printf '%%s\n' "$line" >> "$trace"
  id=$(printf '%%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"protocolVersion":1,"authMethods":[{"id":"fixture-auth","name":"Fixture","type":"agent"}],"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"close":{}}}}}\n' "$id"
      ;;
    *'"method":"authenticate"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
      ;;
    *'"method":"session/load"'*)
	  active_session=resume-123
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"sessionId":"resume-123"}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
	  active_session=fresh-456
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"sessionId":"fresh-456"}}\n' "$id"
      ;;
    *'"method":"session/set_mode"'*|*'"method":"session/set_config_option"'*|*'"method":"session/close"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      prompt_count=$((prompt_count + 1))
      if [ "$prompt_count" -eq 1 ]; then
        held_id=$id
		printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"%%s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"fixture delta"}}}}\n' "$active_session"
		printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"%%s","update":{"sessionUpdate":"tool_call","toolCallId":"tool-1","title":"Fixture Tool","kind":"other","status":"in_progress","rawInput":{"path":"/tmp/example"}}}}\n' "$active_session"
      else
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      fi
      ;;
    *'"method":"session/cancel"'*)
      if [ -n "$held_id" ]; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"stopReason":"cancelled"}}\n' "$held_id"
        held_id=
      fi
      ;;
  esac
  if [ "$mode" = malformed ] && printf '%%s' "$line" | grep -Eq '"method":"session/(load|new)"'; then
    printf '{"token":"fixture-secret"\n'
    exit 9
  fi
  if [ "$mode" = child-exit ] && printf '%%s' "$line" | grep -Eq '"method":"session/(load|new)"'; then
    exit 7
  fi
  if [ "$mode" = disconnect ] && printf '%%s' "$line" | grep -Eq '"method":"session/(load|new)"'; then
    exec 1>&-
	# Stay alive long enough for the client to classify transport EOF as a
	# disconnect, then exit during its graceful cleanup window so EXIT evidence
	# proves the real child was reaped without relying on a SIGKILL-able trap.
	sleep 1
  fi
done
`, shellQuote(tracePath), shellQuote(mode))
	if err := os.WriteFile(scriptPath, []byte(body), 0o755); err != nil {
		t.Fatalf("write ACP fixture: %v", err)
	}
	return scriptPath
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func waitForACPReady(t *testing.T, manager *acp.Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if session, ok := manager.Lookup(id); ok && session.Snapshot().State == acp.StateReady {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if session, ok := manager.Lookup(id); ok {
		t.Fatalf("ACP session state = %q, want ready", session.Snapshot().State)
	}
	t.Fatalf("ACP session %q was not registered", id)
}

func waitForACPTurnOutcome(t *testing.T, w *Wrapper, want acp.OutcomeKind) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if snapshot, ok := w.ACPSnapshot(); ok && snapshot.LastTurnOutcome == want && snapshot.State == acp.StateReady {
			return
		}
		time.Sleep(time.Millisecond)
	}
	snapshot, _ := w.ACPSnapshot()
	t.Fatalf("ACP turn outcome/state = %q/%q, want %q/ready", snapshot.LastTurnOutcome, snapshot.State, want)
}

func assertTraceOrder(t *testing.T, trace string, values ...string) {
	t.Helper()
	pos := 0
	for _, value := range values {
		relative := strings.Index(trace[pos:], value)
		if relative < 0 {
			t.Fatalf("trace missing %q after byte %d:\n%s", value, pos, trace)
		}
		pos += relative + len(value)
	}
}
