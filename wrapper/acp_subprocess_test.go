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
			assertACPControlsReleased(t, w, sink, providerSessionID)
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
			assertACPControlsReleased(t, w, sink, "fresh-456")
			traceBytes, readErr := os.ReadFile(tracePath)
			if readErr != nil {
				t.Fatalf("read cleanup trace: %v", readErr)
			}
			if got := strings.Count(string(traceBytes), "fixture-cleanup"); got != 1 {
				t.Fatalf("fixture cleanup count = %d, want exactly 1; trace:\n%s", got, traceBytes)
			}
			if tc.mode == "malformed" {
				var malformedDiagnostic *acp.Diagnostic
				for i := range diagnostics {
					if diagnostics[i].Kind == acp.DiagnosticMalformedJSON {
						malformedDiagnostic = &diagnostics[i]
						break
					}
				}
				if malformedDiagnostic == nil {
					t.Fatalf("malformed diagnostics = %+v", diagnostics)
				}
				encoded := fmt.Sprintf("%+v", *malformedDiagnostic)
				if strings.Contains(encoded, "fixture-secret") || !strings.Contains(encoded, "[REDACTED]") {
					t.Fatalf("malformed diagnostic was not safely redacted: %s", encoded)
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

func TestAcceptedPromptOutlivesCallerContextAllAsyncAdapters(t *testing.T) {
	factories := map[string]func(string) acp.Client{
		"claude": func(path string) acp.Client { return claudeacp.NewClient(claudeacp.WithClientDirectBinary(path)) },
		"codex": func(path string) acp.Client {
			return codexacp.NewClient(codexacp.WithClientBinary(path), codexacp.WithClientBridgePackageSpec("fixture"))
		},
		"opencode": func(path string) acp.Client { return opencodeacp.NewClient(opencodeacp.WithClientBinary(path)) },
		"pi":       func(path string) acp.Client { return piacp.NewClient(piacp.WithClientBinary(path)) },
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tracePath := filepath.Join(dir, "trace.log")
			client := factory(writeACPFixture(t, dir, tracePath, "caller-cancel"))
			launchCtx, launchCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer launchCancel()
			if err := client.Launch(launchCtx, acp.LaunchParams{Cwd: dir}); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			defer func() { _ = client.Close(context.Background()) }()

			promptCtx, cancelPrompt := context.WithCancel(context.Background())
			if err := client.Prompt(promptCtx, "accepted turn"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			if err := client.Prompt(context.Background(), "overlap"); err == nil {
				t.Fatal("overlapping Prompt unexpectedly succeeded")
			}
			cancelPrompt()
			if err := os.WriteFile(tracePath+".release", []byte("release"), 0o600); err != nil {
				t.Fatalf("release fixture prompt: %v", err)
			}
			first := collectClientTurn(t, client, 5*time.Second)
			if first.terminal.Kind != runtimeevents.KindTurnCompleted {
				t.Fatalf("accepted turn terminal = %q payload=%s, want completed", first.terminal.Kind, first.terminal.Payload)
			}
			if first.startedID == "" || first.terminal.TurnID != first.startedID || first.terminals != 1 {
				t.Fatalf("first turn ids/terminal count = %q/%q/%d", first.startedID, first.terminal.TurnID, first.terminals)
			}

			if err := client.Prompt(context.Background(), "next turn"); err != nil {
				t.Fatalf("Prompt after completion: %v", err)
			}
			second := collectClientTurn(t, client, 5*time.Second)
			if second.terminal.Kind != runtimeevents.KindTurnCompleted || second.terminals != 1 || second.terminal.TurnID != second.startedID {
				t.Fatalf("second turn = %+v", second)
			}
		})
	}
}

func TestAcceptedPromptEndsExactlyOnceOnExplicitCloseAllAdapters(t *testing.T) {
	factories := map[string]func(string) acp.Client{
		"claude": func(path string) acp.Client { return claudeacp.NewClient(claudeacp.WithClientDirectBinary(path)) },
		"codex": func(path string) acp.Client {
			return codexacp.NewClient(codexacp.WithClientBinary(path), codexacp.WithClientBridgePackageSpec("fixture"))
		},
		"copilot": func(path string) acp.Client {
			return copilotacp.NewClient(adapters.TransportStdio, copilotacp.WithBinary(path))
		},
		"opencode": func(path string) acp.Client { return opencodeacp.NewClient(opencodeacp.WithClientBinary(path)) },
		"pi":       func(path string) acp.Client { return piacp.NewClient(piacp.WithClientBinary(path)) },
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tracePath := filepath.Join(dir, "trace.log")
			client := factory(writeACPFixture(t, dir, tracePath, "lifecycle"))
			if err := client.Launch(context.Background(), acp.LaunchParams{Cwd: dir}); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if err := client.Prompt(context.Background(), "accepted then closed"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			if err := client.Close(context.Background()); err != nil {
				t.Fatalf("Close: %v", err)
			}

			var startedID string
			terminals := 0
			for event := range client.Events() {
				switch event.Kind {
				case runtimeevents.KindTurnStarted:
					startedID = event.TurnID
				case runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
					if event.TurnID != startedID {
						t.Fatalf("terminal TurnID = %q, want %q", event.TurnID, startedID)
					}
					terminals++
				}
			}
			if startedID == "" || terminals != 1 {
				t.Fatalf("explicit Close turn = started %q, terminals %d; want one", startedID, terminals)
			}
		})
	}
}

func TestConcurrentPromptCloseAdmissionAndWireOrderAllAdapters(t *testing.T) {
	factories := map[string]func(string) acp.Client{
		"claude": func(path string) acp.Client { return claudeacp.NewClient(claudeacp.WithClientDirectBinary(path)) },
		"codex": func(path string) acp.Client {
			return codexacp.NewClient(codexacp.WithClientBinary(path), codexacp.WithClientBridgePackageSpec("fixture"))
		},
		"copilot": func(path string) acp.Client {
			return copilotacp.NewClient(adapters.TransportStdio, copilotacp.WithBinary(path))
		},
		"opencode": func(path string) acp.Client { return opencodeacp.NewClient(opencodeacp.WithClientBinary(path)) },
		"pi":       func(path string) acp.Client { return piacp.NewClient(piacp.WithClientBinary(path)) },
	}
	for name, factory := range factories {
		name, factory := name, factory
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for iteration := 0; iteration < 10; iteration++ {
				dir := t.TempDir()
				tracePath := filepath.Join(dir, "trace.log")
				client := factory(writeACPFixture(t, dir, tracePath, "close-callback"))
				if err := client.Launch(context.Background(), acp.LaunchParams{Cwd: dir}); err != nil {
					t.Fatalf("iteration %d Launch: %v", iteration, err)
				}

				start := make(chan struct{})
				promptErr := make(chan error, 1)
				closeErr := make(chan error, 1)
				go func() {
					<-start
					promptErr <- client.Prompt(context.Background(), "concurrent prompt")
				}()
				go func() {
					<-start
					closeErr <- client.Close(context.Background())
				}()
				close(start)
				pErr := <-promptErr
				if err := <-closeErr; err != nil {
					t.Fatalf("iteration %d Close: %v", iteration, err)
				}

				var startedID string
				terminals := 0
				for event := range client.Events() {
					switch event.Kind {
					case runtimeevents.KindTurnStarted:
						if startedID != "" {
							t.Fatalf("iteration %d saw multiple started turns", iteration)
						}
						startedID = event.TurnID
					case runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
						if startedID == "" || event.TurnID != startedID {
							t.Fatalf("iteration %d terminal TurnID = %q, started = %q", iteration, event.TurnID, startedID)
						}
						terminals++
					}
				}

				trace := readFixtureTrace(t, tracePath)
				if !strings.Contains(trace, `"id":9001`) {
					t.Fatalf("iteration %d did not answer the server request issued while Close held the Prompt/Close barrier:\n%s", iteration, trace)
				}
				promptAt := strings.Index(trace, `"method":"session/prompt"`)
				closeAt := strings.Index(trace, `"method":"session/close"`)
				if pErr == nil {
					if promptAt < 0 || closeAt < 0 || promptAt >= closeAt {
						t.Fatalf("iteration %d accepted Prompt wire order = prompt %d, close %d:\n%s", iteration, promptAt, closeAt, trace)
					}
					if startedID == "" || terminals != 1 {
						t.Fatalf("iteration %d accepted Prompt events = started %q, terminals %d; want one each", iteration, startedID, terminals)
					}
				} else {
					if promptAt >= 0 {
						t.Fatalf("iteration %d rejected Prompt still wrote a request at %d (Close at %d, Prompt error: %v):\n%s", iteration, promptAt, closeAt, pErr, trace)
					}
					if startedID != "" || terminals != 0 {
						t.Fatalf("iteration %d rejected Prompt events = started %q, terminals %d; want none (Prompt error: %v)", iteration, startedID, terminals, pErr)
					}
				}
			}
		})
	}
}

func TestAllACPClientsValidateInitializeAndGateResume(t *testing.T) {
	factories := map[string]func(string) acp.Client{
		"claude": func(path string) acp.Client { return claudeacp.NewClient(claudeacp.WithClientDirectBinary(path)) },
		"codex": func(path string) acp.Client {
			return codexacp.NewClient(codexacp.WithClientBinary(path), codexacp.WithClientBridgePackageSpec("fixture"))
		},
		"copilot": func(path string) acp.Client {
			return copilotacp.NewClient(adapters.TransportStdio, copilotacp.WithBinary(path))
		},
		"opencode": func(path string) acp.Client { return opencodeacp.NewClient(opencodeacp.WithClientBinary(path)) },
		"pi":       func(path string) acp.Client { return piacp.NewClient(piacp.WithClientBinary(path)) },
	}
	for name, factory := range factories {
		name, factory := name, factory
		t.Run(name, func(t *testing.T) {
			t.Run("version-mismatch-stops-wire-order", func(t *testing.T) {
				dir := t.TempDir()
				tracePath := filepath.Join(dir, "trace.log")
				client := factory(writeACPFixture(t, dir, tracePath, "bad-version"))
				err := client.Launch(context.Background(), acp.LaunchParams{Cwd: dir, SessionIDPreset: "resume-123", AuthMethodID: "fixture-auth"})
				if err == nil || !strings.Contains(err.Error(), "unsupported protocol version 2") {
					t.Fatalf("Launch error = %v, want unsupported protocol version", err)
				}
				trace := readFixtureTrace(t, tracePath)
				if strings.Count(trace, `"method":"initialize"`) != 1 || strings.Contains(trace, `"method":"authenticate"`) || strings.Contains(trace, `"method":"session/`) {
					t.Fatalf("unsupported version did not stop after initialize:\n%s", trace)
				}
			})

			t.Run("load-capability-absent-uses-new", func(t *testing.T) {
				dir := t.TempDir()
				tracePath := filepath.Join(dir, "trace.log")
				client := factory(writeACPFixture(t, dir, tracePath, "no-load"))
				if err := client.Launch(context.Background(), acp.LaunchParams{Cwd: dir, SessionIDPreset: "resume-123"}); err != nil {
					t.Fatalf("Launch: %v", err)
				}
				if got := acp.ProviderSessionID(client); got != "fresh-456" {
					t.Fatalf("ProviderSessionID = %q, want fresh-456", got)
				}
				if err := client.Close(context.Background()); err != nil {
					t.Fatalf("Close: %v", err)
				}
				trace := readFixtureTrace(t, tracePath)
				if strings.Contains(trace, `"method":"session/load"`) || strings.Count(trace, `"method":"session/new"`) != 1 {
					t.Fatalf("capability-absent resume wire order:\n%s", trace)
				}
			})

			t.Run("advertised-load-error-does-not-fallback", func(t *testing.T) {
				dir := t.TempDir()
				tracePath := filepath.Join(dir, "trace.log")
				client := factory(writeACPFixture(t, dir, tracePath, "load-error"))
				err := client.Launch(context.Background(), acp.LaunchParams{Cwd: dir, SessionIDPreset: "resume-123"})
				if err == nil || !strings.Contains(err.Error(), "session/load") {
					t.Fatalf("Launch error = %v, want session/load failure", err)
				}
				trace := readFixtureTrace(t, tracePath)
				if strings.Count(trace, `"method":"session/load"`) != 1 || strings.Contains(trace, `"method":"session/new"`) {
					t.Fatalf("failed load silently fell back to new:\n%s", trace)
				}
			})
		})
	}
}

func readFixtureTrace(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture trace: %v", err)
	}
	return string(contents)
}

type collectedClientTurn struct {
	startedID string
	terminal  runtimeevents.Event
	terminals int
}

func collectClientTurn(t *testing.T, client acp.Client, timeout time.Duration) collectedClientTurn {
	t.Helper()
	var result collectedClientTurn
	deadline := time.After(timeout)
	for {
		select {
		case event, ok := <-client.Events():
			if !ok {
				t.Fatalf("client Events closed before turn terminal: %+v", result)
			}
			switch event.Kind {
			case runtimeevents.KindTurnStarted:
				result.startedID = event.TurnID
			case runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
				result.terminal = event
				result.terminals++
				return result
			}
		case <-deadline:
			t.Fatalf("timed out waiting for turn terminal: %+v", result)
		}
	}
}

func TestACPWrapperClearsControlAuthorityOnNaturalExitAndContextCancel(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		cancelRun  bool
		wantRunErr error
	}{
		{name: "natural-exit", mode: "natural-exit"},
		{name: "context-cancel", mode: "lifecycle", cancelRun: true, wantRunErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fixturePath := writeACPFixture(t, dir, filepath.Join(dir, "trace.log"), tc.mode)
			manager := acp.NewManager()
			sink := newCapturingSink()
			w, err := New(Config{
				App:      "acp-release-" + tc.name,
				Adapter:  opencodeacp.New(opencodeacp.WithBinary(fixturePath)),
				Activity: activity.NewBridge(sink), Workdir: dir,
				SessionID: "release-" + tc.name, ACPManager: manager,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runErrCh := make(chan error, 1)
			go func() { runErrCh <- w.Run(ctx) }()
			if tc.cancelRun {
				waitForACPReady(t, manager, w.SessionID())
				cancel()
			}
			select {
			case runErr := <-runErrCh:
				if !errors.Is(runErr, tc.wantRunErr) {
					t.Fatalf("Run error = %v, want %v", runErr, tc.wantRunErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return")
			}
			assertACPControlsReleased(t, w, sink, "fresh-456")
			if manager.Len() != 0 {
				t.Fatalf("Manager.Len = %d after Run, want 0", manager.Len())
			}
		})
	}
}

func assertACPControlsReleased(t *testing.T, w *Wrapper, sink *capturingSink, wantProviderID string) {
	t.Helper()
	if snapshot, ok := w.ACPSnapshot(); ok {
		t.Fatalf("ACPSnapshot after Run = %+v, true; want no live control authority", snapshot)
	}
	if got := w.ProviderSessionID(); got != wantProviderID {
		t.Fatalf("postmortem ProviderSessionID = %q, want %q", got, wantProviderID)
	}
	if err := w.SendInput(context.Background(), []byte("after-run")); !errors.Is(err, ErrSessionNotStarted) {
		t.Fatalf("SendInput after Run = %v, want ErrSessionNotStarted", err)
	}
	before := countACPInterruptEvents(sink.snapshot())
	if err := w.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after Run: %v", err)
	}
	after := countACPInterruptEvents(sink.snapshot())
	if after != before {
		t.Fatalf("Stop after Run emitted %d additional interrupt events", after-before)
	}
}

func countACPInterruptEvents(events []runtimeevents.Event) int {
	count := 0
	for _, event := range events {
		if event.Kind == runtimeevents.KindInterruptRequested || event.Kind == runtimeevents.KindInterruptAcknowledged {
			count++
		}
	}
	return count
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
	build := exec.Command("go", "build", "-o", fixturePath, "./testdata/acpfixture")
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
release="$trace.release"
trap 'printf '"'"'fixture-cleanup\n'"'"' >> "$trace"' EXIT
prompt_count=0
held_id=
close_id=
active_session=
while IFS= read -r line; do
  printf '%%s\n' "$line" >> "$trace"
  id=$(printf '%%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      if [ "$mode" = bad-version ]; then
		printf '{"jsonrpc":"2.0","id":%%s,"result":{"protocolVersion":2,"authMethods":[],"agentCapabilities":{}}}\n' "$id"
	  elif [ "$mode" = no-load ]; then
		printf '{"jsonrpc":"2.0","id":%%s,"result":{"protocolVersion":1,"authMethods":[],"agentCapabilities":{"sessionCapabilities":{"close":{}}}}}\n' "$id"
	  else
		printf '{"jsonrpc":"2.0","id":%%s,"result":{"protocolVersion":1,"authMethods":[{"id":"fixture-auth","name":"Fixture","type":"agent"}],"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"close":{}}}}}\n' "$id"
	  fi
      ;;
    *'"method":"authenticate"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
      ;;
    *'"method":"session/load"'*)
	  active_session=resume-123
      if [ "$mode" = load-error ]; then
		printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":-32001,"message":"resume rejected"}}\n' "$id"
	  else
		# ACP v1 load returns modes/configuration, not a sessionId. The client
		# retains the requested id as its provider identity.
		printf '{"jsonrpc":"2.0","id":%%s,"result":{"modes":{"availableModes":[],"currentModeId":""},"configOptions":[]}}\n' "$id"
	  fi
      ;;
    *'"method":"session/new"'*)
	  active_session=fresh-456
      printf '{"jsonrpc":"2.0","id":%%s,"result":{"sessionId":"fresh-456"}}\n' "$id"
      ;;
	*'"method":"session/set_mode"'*|*'"method":"session/set_config_option"'*)
      printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
      ;;
	*'"method":"session/close"'*)
	  if [ "$mode" = close-callback ]; then
		close_id=$id
		printf '{"jsonrpc":"2.0","id":9001,"method":"session/request_permission","params":{"sessionId":"%%s","toolCall":{"toolCallId":"close-callback","title":"Close callback"},"options":[]}}\n' "$active_session"
	  else
		printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
	  fi
	  ;;
	*'"id":9001'*)
	  if [ -n "$close_id" ]; then
		printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$close_id"
		close_id=
	  fi
	  ;;
    *'"method":"session/prompt"'*)
      prompt_count=$((prompt_count + 1))
      if [ "$mode" = caller-cancel ] && [ "$prompt_count" -eq 1 ]; then
		while [ ! -e "$release" ]; do sleep 0.01; done
		printf '{"jsonrpc":"2.0","id":%%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      elif [ "$prompt_count" -eq 1 ]; then
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
  if [ "$mode" = natural-exit ] && printf '%%s' "$line" | grep -Eq '"method":"session/(load|new)"'; then
    exit 0
  fi
  if [ "$mode" = disconnect ] && printf '%%s' "$line" | grep -Eq '"method":"session/(load|new)"'; then
    exec 1>&-
	# Remain live after protocol EOF until the coordinator closes stdin. This
	# makes disconnect causal and deterministic while the EXIT trap proves the
	# real child was reaped rather than abandoned or SIGKILLed.
	while IFS= read -r ignored; do :; done
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
