package wrapper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/adapters/claudeacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/codexacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/copilotacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/opencodeacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/piacp"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

type permissionClientFixture struct {
	name                string
	legacyDefaultError  bool
	legacyDefaultReason string
	newClient           func(string) acp.Client
}

func permissionClientFixtures() []permissionClientFixture {
	return []permissionClientFixture{
		{name: "claude", legacyDefaultReason: "claudeacp: no approval handler configured", newClient: func(path string) acp.Client {
			return claudeacp.NewClient(claudeacp.WithClientDirectBinary(path))
		}},
		{name: "codex", legacyDefaultReason: "codexacp: no approval handler configured", newClient: func(path string) acp.Client {
			return codexacp.NewClient(codexacp.WithClientBinary(path), codexacp.WithClientBridgePackageSpec("fixture"))
		}},
		{name: "copilot", legacyDefaultError: true, newClient: func(path string) acp.Client {
			return copilotacp.NewClient(adapters.TransportStdio, copilotacp.WithBinary(path))
		}},
		{name: "opencode", legacyDefaultReason: "opencodeacp: no approval handler configured", newClient: func(path string) acp.Client {
			return opencodeacp.NewClient(opencodeacp.WithClientBinary(path))
		}},
		{name: "pi", legacyDefaultReason: "piacp: no approval handler configured", newClient: func(path string) acp.Client {
			return piacp.NewClient(piacp.WithClientBinary(path))
		}},
	}
}

func TestBestEffortPermissionResponderAllACPSubprocesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	scenarios := []string{"default", "allow", "deny", "cancel", "turn-cancel", "error", "invalid", "concurrent", "string-id", "null-id", "invalid-id"}
	for _, fixture := range permissionClientFixtures() {
		fixture := fixture
		for _, scenario := range scenarios {
			scenario := scenario
			t.Run(fixture.name+"/"+scenario, func(t *testing.T) {
				t.Parallel()
				runPermissionSubprocessScenario(t, fixture, scenario)
			})
		}
	}
}

func TestBestEffortPermissionResponderMayCallPromptAndCloseAllACPSubprocesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	for _, fixture := range permissionClientFixtures() {
		fixture := fixture
		t.Run(fixture.name+"/Prompt", func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "permission-responses.ndjson")
			client := fixture.newClient(writePermissionACPFixture(t, dir))
			callbackErr := make(chan error, 1)
			params := acp.LaunchParams{
				Cwd: dir,
				Env: append(os.Environ(),
					"ACP_PERMISSION_MARKER="+marker,
					"ACP_PERMISSION_MODE=callback-prompt",
				),
				BestEffortPermissionRequestResponder: func(context.Context, acp.PermissionRequest) (acp.PermissionSelection, error) {
					callbackErr <- client.Prompt(context.Background(), "must not deadlock")
					return acp.SelectPermissionOption("allow"), nil
				},
			}
			if err := client.Launch(context.Background(), params); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(context.Background()) }()
			if err := client.Prompt(context.Background(), "first prompt"); err != nil {
				t.Fatal(err)
			}
			_ = collectPermissionTurnEvents(t, client)
			select {
			case err := <-callbackErr:
				if err == nil {
					t.Fatal("Prompt from responder unexpectedly admitted an overlapping turn")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Prompt from responder deadlocked")
			}
		})

		t.Run(fixture.name+"/Close", func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "permission-responses.ndjson")
			client := fixture.newClient(writePermissionACPFixture(t, dir))
			callbackDone := make(chan error, 1)
			params := acp.LaunchParams{
				Cwd: dir,
				Env: append(os.Environ(),
					"ACP_PERMISSION_MARKER="+marker,
					"ACP_PERMISSION_MODE=callback-close",
				),
				BestEffortPermissionRequestResponder: func(context.Context, acp.PermissionRequest) (acp.PermissionSelection, error) {
					closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					err := client.Close(closeCtx)
					callbackDone <- err
					return acp.PermissionSelection{}, err
				},
			}
			if err := client.Launch(context.Background(), params); err != nil {
				t.Fatal(err)
			}
			if err := client.Prompt(context.Background(), "close from callback"); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-callbackDone:
				if err != nil {
					t.Fatalf("Close from responder: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Close from responder deadlocked")
			}
		})
	}
}

func TestBestEffortPermissionResponderFloodIsBoundedAllACPSubprocesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	for _, fixture := range permissionClientFixtures() {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			before := runtime.NumGoroutine()
			dir := t.TempDir()
			client := fixture.newClient(writePermissionFloodACPFixture(t, dir))
			release := make(chan struct{})
			defer close(release)
			entered := make(chan struct{}, acp.MaxConcurrentBestEffortPermissionRequests+1)
			params := acp.LaunchParams{
				Cwd: dir,
				Env: os.Environ(),
				BestEffortPermissionRequestResponder: func(context.Context, acp.PermissionRequest) (acp.PermissionSelection, error) {
					entered <- struct{}{}
					<-release // Deliberately ignore cancellation until test cleanup.
					return acp.PermissionSelection{}, nil
				},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := client.Launch(ctx, params); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(context.Background()) }()
			if err := client.Prompt(ctx, "flood permission requests without reading responses"); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < acp.MaxConcurrentBestEffortPermissionRequests; i++ {
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatalf("only %d bounded callbacks entered", i)
				}
			}
			select {
			case <-entered:
				t.Fatal("adapter admitted more callbacks than the shared bound")
			case <-time.After(300 * time.Millisecond):
			}
			if delta := runtime.NumGoroutine() - before; delta > acp.MaxConcurrentBestEffortPermissionRequests*4 {
				t.Fatalf("permission flood grew %d goroutines, want bounded near %d callbacks", delta, acp.MaxConcurrentBestEffortPermissionRequests)
			}

			closeCtx, closeCancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer closeCancel()
			if err := client.Close(closeCtx); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("Close under permission flood: %v", err)
			}
		})
	}
}

func TestBestEffortPermissionResponseFailureTerminatesAllACPSubprocesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	for _, fixture := range permissionClientFixtures() {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			client := fixture.newClient(writePermissionFloodACPFixture(t, dir))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := client.Launch(ctx, acp.LaunchParams{
				Cwd: dir,
				Env: os.Environ(),
				BestEffortPermissionRequestResponder: func(context.Context, acp.PermissionRequest) (acp.PermissionSelection, error) {
					return acp.SelectPermissionOption("deny"), nil
				},
			}); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(context.Background()) }()
			if err := client.Prompt(ctx, "fail a non-reading permission transport"); err != nil {
				t.Fatal(err)
			}

			deadline := time.After(7 * time.Second)
			sawDeliveryFailure := false
			for {
				select {
				case event, ok := <-client.Events():
					if !ok {
						t.Fatal("event stream closed before turn terminal")
					}
					if event.Kind == runtimeevents.KindAgentPermissionResolved {
						var payload struct {
							Reason string `json:"reason"`
						}
						_ = json.Unmarshal(event.Payload, &payload)
						if payload.Reason == "permission response delivery failed" {
							sawDeliveryFailure = true
						}
					}
					if event.Kind == runtimeevents.KindTurnFailed || event.Kind == runtimeevents.KindTurnCompleted {
						if !sawDeliveryFailure {
							t.Fatalf("turn terminated without a permission delivery-failure event: %+v", event)
						}
						return
					}
				case <-deadline:
					t.Fatal("undeliverable permission response did not terminate the client")
				}
			}
		})
	}
}

func TestBestEffortPermissionLateFrameCannotCrossTurnsAllACPSubprocesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	for _, fixture := range permissionClientFixtures() {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "late-permission-responses.ndjson")
			client := fixture.newClient(writeLatePermissionACPFixture(t, dir))
			var responderCalls atomic.Int64
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := client.Launch(ctx, acp.LaunchParams{
				Cwd: dir,
				Env: append(os.Environ(),
					"ACP_PERMISSION_MARKER="+marker,
				),
				BestEffortPermissionRequestResponder: func(context.Context, acp.PermissionRequest) (acp.PermissionSelection, error) {
					responderCalls.Add(1)
					return acp.SelectPermissionOption("allow"), nil
				},
			}); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(context.Background()) }()

			if err := client.Prompt(ctx, "first turn"); err != nil {
				t.Fatal(err)
			}
			firstEvents := collectPermissionTurnEvents(t, client)
			assertPermissionEventIDs(t, firstEvents, nil)
			firstResponses := waitForPermissionResponseLines(t, marker, 1)
			assertPermissionWireOutcome(t, firstResponses[0], "late-1", "cancelled", "")
			if got := responderCalls.Load(); got != 0 {
				t.Fatalf("late frame invoked first/next-turn responder %d times", got)
			}

			if err := client.Prompt(ctx, "second turn"); err != nil {
				t.Fatal(err)
			}
			secondEvents := collectPermissionTurnEvents(t, client)
			assertPermissionEventIDs(t, secondEvents, []string{`"fresh-2"`})
			responses := waitForPermissionResponseLines(t, marker, 2)
			assertPermissionWireOutcome(t, responses[1], "fresh-2", "selected", "allow")
			if got := responderCalls.Load(); got != 1 {
				t.Fatalf("responder calls after fresh second-turn request = %d, want 1", got)
			}
		})
	}
}

func assertPermissionEventIDs(t *testing.T, events []runtimeevents.Event, want []string) {
	t.Helper()
	var requested, resolved []string
	terminal := -1
	for index, event := range events {
		switch event.Kind {
		case runtimeevents.KindAgentPermissionRequested:
			var payload struct {
				RequestID json.RawMessage `json:"request_id"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			requested = append(requested, string(payload.RequestID))
		case runtimeevents.KindAgentPermissionResolved:
			var payload struct {
				RequestID json.RawMessage `json:"request_id"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			resolved = append(resolved, string(payload.RequestID))
		case runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
			terminal = index
		}
	}
	if terminal < 0 {
		t.Fatalf("events have no terminal: %+v", events)
	}
	if strings.Join(requested, ",") != strings.Join(want, ",") || strings.Join(resolved, ",") != strings.Join(want, ",") {
		t.Fatalf("permission requested/resolved ids = %v/%v, want %v; events=%+v", requested, resolved, want, events)
	}
}

func assertPermissionWireOutcome(t *testing.T, line, id, outcome, optionID string) {
	t.Helper()
	var frame struct {
		ID     string `json:"id"`
		Result struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("decode permission response %q: %v", line, err)
	}
	if frame.ID != id || frame.Result.Outcome.Outcome != outcome || frame.Result.Outcome.OptionID != optionID {
		t.Fatalf("permission response = %s, want id=%q outcome=%q option=%q", line, id, outcome, optionID)
	}
}

func waitForPermissionResponseLines(t *testing.T, marker string, count int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(marker)
		if err == nil {
			trimmed := strings.TrimSpace(string(data))
			if trimmed != "" {
				lines := strings.Split(trimmed, "\n")
				if len(lines) >= count {
					return lines
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("permission marker %s did not reach %d lines", marker, count)
		}
		time.Sleep(time.Millisecond)
	}
}

func runPermissionSubprocessScenario(t *testing.T, fixture permissionClientFixture, scenario string) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "permission-responses.ndjson")
	client := fixture.newClient(writePermissionACPFixture(t, dir))
	entered := make(chan struct{}, 2)

	var responder acp.BestEffortPermissionRequestResponder
	switch scenario {
	case "default":
	case "allow", "string-id", "null-id":
		responder = func(_ context.Context, request acp.PermissionRequest) (acp.PermissionSelection, error) {
			assertSubprocessPermissionRequest(t, request)
			return acp.SelectPermissionOption("allow"), nil
		}
	case "deny":
		responder = func(_ context.Context, request acp.PermissionRequest) (acp.PermissionSelection, error) {
			assertSubprocessPermissionRequest(t, request)
			return acp.SelectPermissionOption("deny"), nil
		}
	case "cancel":
		responder = func(context.Context, acp.PermissionRequest) (acp.PermissionSelection, error) {
			return acp.PermissionSelection{}, nil
		}
	case "turn-cancel":
		responder = func(ctx context.Context, _ acp.PermissionRequest) (acp.PermissionSelection, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return acp.SelectPermissionOption("allow"), ctx.Err()
		}
	case "error":
		responder = func(context.Context, acp.PermissionRequest) (acp.PermissionSelection, error) {
			return acp.PermissionSelection{}, errors.New("password=callback-secret")
		}
	case "invalid":
		responder = func(context.Context, acp.PermissionRequest) (acp.PermissionSelection, error) {
			return acp.SelectPermissionOption("never-offered"), nil
		}
	case "invalid-id":
		responder = func(context.Context, acp.PermissionRequest) (acp.PermissionSelection, error) {
			t.Error("responder invoked for invalid JSON-RPC request id")
			return acp.PermissionSelection{}, nil
		}
	case "concurrent":
		responder = func(_ context.Context, request acp.PermissionRequest) (acp.PermissionSelection, error) {
			if request.ToolCall.ToolCallID == "call-allow" {
				return acp.SelectPermissionOption("allow"), nil
			}
			return acp.SelectPermissionOption("deny"), nil
		}
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}

	var diagnosticsMu sync.Mutex
	var diagnostics []acp.Diagnostic
	params := acp.LaunchParams{
		Cwd: dir,
		Env: append(os.Environ(),
			"ACP_PERMISSION_MARKER="+marker,
			"ACP_PERMISSION_MODE="+scenario,
		),
		BestEffortPermissionRequestResponder: responder,
		OnDiagnostic: func(diagnostic acp.Diagnostic) {
			diagnosticsMu.Lock()
			diagnostics = append(diagnostics, diagnostic)
			diagnosticsMu.Unlock()
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Launch(ctx, params); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	if err := client.Prompt(ctx, "exercise permission responder"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if scenario == "turn-cancel" {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("responder was not entered")
		}
		if err := client.Cancel(ctx); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
	}

	events := collectPermissionTurnEvents(t, client)
	lines := readPermissionResponses(t, marker)
	wantResponses := 1
	if scenario == "concurrent" {
		wantResponses = 2
	} else if scenario == "invalid-id" {
		wantResponses = 0
	}
	if len(lines) != wantResponses {
		t.Fatalf("permission responses = %d, want %d: %q", len(lines), wantResponses, lines)
	}

	for _, line := range lines {
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Outcome struct {
					Outcome  string `json:"outcome"`
					OptionID string `json:"optionId"`
				} `json:"outcome"`
			} `json:"result"`
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("decode permission response %q: %v", line, err)
		}
		if scenario == "default" && fixture.legacyDefaultError {
			if frame.Error == nil || frame.Error.Code != -32601 {
				t.Fatalf("Copilot legacy default response = %s, want -32601", line)
			}
			continue
		}
		if scenario == "default" {
			const want = `{"id":99,"jsonrpc":"2.0","result":{"outcome":{"outcome":"cancelled"}}}`
			if line != want {
				t.Fatalf("legacy default wire response = %s, want exact %s", line, want)
			}
		}
		wantOutcome, wantOption := "cancelled", ""
		switch scenario {
		case "allow", "string-id", "null-id":
			wantOutcome, wantOption = "selected", "allow"
		case "deny":
			wantOutcome, wantOption = "selected", "deny"
		case "concurrent":
			wantOutcome = "selected"
			if string(frame.ID) == "91" {
				wantOption = "allow"
			} else {
				wantOption = "deny"
			}
		}
		if frame.Error != nil || frame.Result.Outcome.Outcome != wantOutcome || frame.Result.Outcome.OptionID != wantOption {
			t.Fatalf("response = %s, want outcome=%q option=%q; events=%+v", line, wantOutcome, wantOption, events)
		}
		if scenario == "string-id" && string(frame.ID) != `"permission-1"` {
			t.Fatalf("response id = %s, want exact string id", frame.ID)
		}
		if scenario == "null-id" && string(frame.ID) != "null" {
			t.Fatalf("response id = %s, want null id", frame.ID)
		}
	}

	wantPermissionEvents := wantResponses
	if scenario == "default" && fixture.legacyDefaultError {
		wantPermissionEvents = 0
	}
	requested, resolved := 0, 0
	terminalIndex := -1
	lastResolvedIndex := -1
	requestedIDs := make(map[string]int)
	resolvedIDs := make(map[string]int)
	for index, event := range events {
		switch event.Kind {
		case runtimeevents.KindAgentPermissionRequested:
			requested++
			var payload struct {
				RequestID json.RawMessage `json:"request_id"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			requestedIDs[string(payload.RequestID)]++
		case runtimeevents.KindAgentPermissionResolved:
			resolved++
			lastResolvedIndex = index
			if scenario == "default" {
				want := `{"allowed":false,"reason":"` + fixture.legacyDefaultReason + `","request_id":99}`
				if string(event.Payload) != want {
					t.Fatalf("legacy default resolved payload = %s, want exact %s", event.Payload, want)
				}
			}
			var payload struct {
				RequestID  json.RawMessage `json:"request_id"`
				Allowed    bool            `json:"allowed"`
				OptionKind string          `json:"option_kind"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.OptionKind == "reject_once" && payload.Allowed {
				t.Fatal("selected reject option emitted allowed=true")
			}
			if payload.OptionKind == "allow_once" && !payload.Allowed {
				t.Fatal("selected allow option emitted allowed=false")
			}
			if payload.OptionKind == "" && payload.Allowed {
				t.Fatal("cancelled permission emitted allowed=true")
			}
			resolvedIDs[string(payload.RequestID)]++
		case runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
			terminalIndex = index
		}
	}
	if requested != wantPermissionEvents || resolved != wantPermissionEvents {
		t.Fatalf("permission event counts requested/resolved = %d/%d, want %d; events=%+v", requested, resolved, wantPermissionEvents, events)
	}
	if wantPermissionEvents > 0 && (terminalIndex < 0 || lastResolvedIndex < 0 || lastResolvedIndex > terminalIndex) {
		t.Fatalf("permission resolution was not emitted before the turn terminal: %+v", events)
	}
	for id, count := range requestedIDs {
		if count != 1 || resolvedIDs[id] != 1 {
			t.Fatalf("permission correlation for request %s = requested %d resolved %d", id, count, resolvedIDs[id])
		}
	}

	if scenario == "error" || scenario == "invalid" || scenario == "invalid-id" {
		deadline := time.Now().Add(time.Second)
		for {
			diagnosticsMu.Lock()
			got := append([]acp.Diagnostic(nil), diagnostics...)
			diagnosticsMu.Unlock()
			if len(got) > 0 {
				encoded, _ := json.Marshal(got)
				if strings.Contains(string(encoded), "callback-secret") || strings.Contains(string(encoded), "never-offered") || strings.Contains(string(encoded), "fixture-secret") {
					t.Fatalf("diagnostic leaked responder data: %s", encoded)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("expected responder diagnostic")
			}
			time.Sleep(time.Millisecond)
		}
	}
}

func assertSubprocessPermissionRequest(t *testing.T, request acp.PermissionRequest) {
	t.Helper()
	if request.SessionID != "fixture-session" || request.ToolCall.ToolCallID == "" || len(request.Options) != 2 {
		t.Errorf("responder request = %+v", request)
	}
	if !strings.Contains(string(request.RawParams), "fixture-secret") {
		t.Errorf("responder request lost raw tool input: %+v", request)
	}
}

func collectPermissionTurnEvents(t *testing.T, client acp.Client) []runtimeevents.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var events []runtimeevents.Event
	for {
		select {
		case event, ok := <-client.Events():
			if !ok {
				t.Fatalf("events closed before turn terminal: %+v", events)
			}
			events = append(events, event)
			if event.Kind == runtimeevents.KindTurnCompleted || event.Kind == runtimeevents.KindTurnFailed {
				return events
			}
		case <-deadline:
			t.Fatalf("timed out waiting for turn terminal: %+v", events)
		}
	}
}

func readPermissionResponses(t *testing.T, marker string) []string {
	t.Helper()
	file, err := os.Open(marker)
	if err != nil {
		t.Fatalf("open permission responses: %v", err)
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func writePermissionACPFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "permission-acp-fixture.sh")
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"authMethods":[],"agentCapabilities":{"sessionCapabilities":{"close":{}}}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"fixture-session"}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      prompt_id=$id
      if [ "$ACP_PERMISSION_MODE" = concurrent ]; then
        printf '{"jsonrpc":"2.0","id":91,"method":"session/request_permission","params":{"sessionId":"fixture-session","toolCall":{"toolCallId":"call-allow","rawInput":{"command":"echo allow","token":"fixture-secret"}},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"deny","name":"Deny","kind":"reject_once"}]}}\n'
        printf '{"jsonrpc":"2.0","id":92,"method":"session/request_permission","params":{"sessionId":"fixture-session","toolCall":{"toolCallId":"call-deny","rawInput":{"command":"echo deny","token":"fixture-secret"}},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"deny","name":"Deny","kind":"reject_once"}]}}\n'
        expected=2
      elif [ "$ACP_PERMISSION_MODE" = string-id ]; then
        printf '{"jsonrpc":"2.0","id":"permission-1","method":"session/request_permission","params":{"sessionId":"fixture-session","toolCall":{"toolCallId":"call-one","rawInput":{"command":"echo one","token":"fixture-secret"}},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"deny","name":"Deny","kind":"reject_once"}]}}\n'
        expected=1
      elif [ "$ACP_PERMISSION_MODE" = null-id ]; then
        printf '{"jsonrpc":"2.0","id":null,"method":"session/request_permission","params":{"sessionId":"fixture-session","toolCall":{"toolCallId":"call-one","rawInput":{"command":"echo one","token":"fixture-secret"}},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"deny","name":"Deny","kind":"reject_once"}]}}\n'
        expected=1
      elif [ "$ACP_PERMISSION_MODE" = invalid-id ]; then
        : > "$ACP_PERMISSION_MARKER"
        printf '{"jsonrpc":"2.0","id":{"bad":"fixture-secret"},"method":"session/request_permission","params":{"sessionId":"fixture-session","toolCall":{"toolCallId":"call-one","rawInput":{"command":"echo one","token":"fixture-secret"}},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"}]}}\n'
        printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$prompt_id"
        continue
      else
        printf '{"jsonrpc":"2.0","id":99,"method":"session/request_permission","params":{"sessionId":"fixture-session","toolCall":{"toolCallId":"call-one","rawInput":{"command":"echo one","token":"fixture-secret"}},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"deny","name":"Deny","kind":"reject_once"}]}}\n'
        expected=1
      fi
	      received=0
	      while [ "$received" -lt "$expected" ] && IFS= read -r answer; do
	        case "$answer" in
	          *'"method":"session/close"'*)
	            close_id=$(printf '%s\n' "$answer" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
	            printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$close_id"
	            exit 0
            ;;
          *'"id":91'*|*'"id":92'*|*'"id":99'*|*'"id":"permission-1"'*|*'"id":null'*)
            printf '%s\n' "$answer" >> "$ACP_PERMISSION_MARKER"
            received=$((received + 1))
            ;;
	        esac
	      done
	      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$prompt_id"
      ;;
    *'"method":"session/close"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
  esac
done
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write permission ACP fixture: %v", err)
	}
	return path
}

func writePermissionFloodACPFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "permission-flood-acp-fixture.sh")
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"authMethods":[],"agentCapabilities":{}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"flood-session"}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      request_id=1000
      while [ "$request_id" -lt 3000 ]; do
        printf '{"jsonrpc":"2.0","id":%s,"method":"session/request_permission","params":{"sessionId":"flood-session","toolCall":{"toolCallId":"call-%s","rawInput":{"token":"flood-secret"}},"options":[{"optionId":"deny","name":"Deny","kind":"reject_once"}]}}\n' "$request_id" "$request_id"
        request_id=$((request_id + 1))
      done
	      while :; do sleep 60; done
      ;;
  esac
done
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write permission flood ACP fixture: %v", err)
	}
	return path
}

func writeLatePermissionACPFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "late-permission-acp-fixture.sh")
	body := `#!/bin/sh
prompt_count=0
while IFS= read -r line; do
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"authMethods":[],"agentCapabilities":{"sessionCapabilities":{"close":{}}}}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"late-session"}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      prompt_count=$((prompt_count + 1))
      if [ "$prompt_count" -eq 1 ]; then
        printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id"
        printf '{"jsonrpc":"2.0","id":"late-1","method":"session/request_permission","params":{"sessionId":"late-session","toolCall":{"toolCallId":"late-call","rawInput":{"command":"must-not-run"}},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"deny","name":"Deny","kind":"reject_once"}]}}\n'
        IFS= read -r answer
        printf '%s\n' "$answer" >> "$ACP_PERMISSION_MARKER"
      else
        printf '{"jsonrpc":"2.0","id":"fresh-2","method":"session/request_permission","params":{"sessionId":"late-session","toolCall":{"toolCallId":"fresh-call","rawInput":{"command":"safe-synthetic"}},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"deny","name":"Deny","kind":"reject_once"}]}}\n'
        IFS= read -r answer
        printf '%s\n' "$answer" >> "$ACP_PERMISSION_MARKER"
        printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      fi
      ;;
    *'"method":"session/close"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      exit 0
      ;;
  esac
done
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write late permission ACP fixture: %v", err)
	}
	return path
}
