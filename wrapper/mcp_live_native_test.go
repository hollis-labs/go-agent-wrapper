package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/agentlaunch/launcher"
	"github.com/hollis-labs/agentkit/agentlaunch/providerplant"
	permission "github.com/hollis-labs/go-permission"
	"github.com/hollis-labs/go-providers/provider"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"

	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/internal/testgate"
	"github.com/hollis-labs/go-agent-wrapper/launch"
)

// The native half of the CW-20260930-0136 acceptance (see mcp_live_test.go):
// the echo servers go into the launch plan's MCPSpec, agentkit's launcher and
// providerplant plant them in each runtime's own config, and the real CLI runs
// through the wrapper with that PreparedExecution.

func (f *liveMCPFixture) launchServers() []agentlaunch.MCPServerSpec {
	return []agentlaunch.MCPServerSpec{
		{Name: liveMCPStdioName, Command: f.binary, Env: map[string]string{liveMCPServeEnv: f.record}},
		{Name: liveMCPHTTPName, URL: f.endpoint},
	}
}

// liveProviderEnv is the part of this process's environment a real CLI needs
// to find its own login. A prepared execution's child gets exactly the env
// the plan carries, nothing inherited.
func liveProviderEnv() map[string]string {
	env := map[string]string{}
	for _, k := range []string{
		"HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TERM", "TMPDIR",
		"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
		"DBUS_SESSION_BUS_ADDRESS",
	} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	return env
}

// preparedLiveMCP compiles and prepares a launch whose MCPSpec names the
// fixture's two servers, the way an app would.
func preparedLiveMCP(t *testing.T, id runtimes.ID, mode runtimes.Mode, binary string, flags []string, posture permission.Mode, fx *liveMCPFixture) *agentlaunch.PreparedExecution {
	t.Helper()
	plan := agentlaunch.LaunchPlan{
		Project:   agentlaunch.ProjectSpec{ID: "live-mcp", Name: "live-mcp", Root: t.TempDir()},
		Agent:     agentlaunch.AgentSpec{ID: "live-mcp", Name: "live-mcp"},
		Provider:  agentlaunch.ProviderSpec{ID: string(id), Binary: binary, Flags: flags, Env: liveProviderEnv(), Permission: posture},
		Runtime:   mode,
		Workspace: agentlaunch.WorkspaceSpec{Mode: agentlaunch.WorkspaceTemp, TempPrefix: t.TempDir()},
		BootProfile: agentlaunch.BootProfileRef{Inline: &agentlaunch.BootProfileInline{
			BootMode: agentlaunch.BootModeNone,
		}},
		Mode: agentlaunch.LaunchInteractive,
		MCP:  agentlaunch.MCPSpec{Servers: fx.launchServers()},
	}
	compiled, err := launcher.Compile(context.Background(), plan)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	prepared, err := launcher.Prepare(context.Background(), compiled)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	execution, err := providerplant.PrepareExecution(context.Background(), prepared)
	if err != nil {
		t.Fatalf("PrepareExecution: %v", err)
	}
	// The default access policy is a required sandbox with loopback-only
	// network, which would keep the CLI from its model API and its login.
	execution.Access = agentlaunch.AccessRequirements{Mode: agentlaunch.AccessOptional}
	return execution
}

// liveTurnOutcome waits for the turn to end and returns the event that ended
// it.
func liveTurnOutcome(t *testing.T, sink *capturingSink, d time.Duration) runtimeevents.Event {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, ev := range sink.snapshot() {
			if ev.Kind == runtimeevents.KindTurnCompleted || ev.Kind == runtimeevents.KindTurnFailed {
				return ev
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("turn did not end within %v; events %v", d, sink.kinds())
	return runtimeevents.Event{}
}

func TestLiveMCPToolsNative(t *testing.T) {
	testgate.RequireLiveProvider(t)
	cases := []struct {
		runtime runtimes.ID
		lookup  func() (string, error)
		flags   []string
		posture permission.Mode
	}{
		// claude -p denies tools it was not allowed; allow exactly the two.
		{runtimes.Claude, func() (string, error) { return exec.LookPath("claude") },
			[]string{"--model", "haiku", "--allowedTools=mcp__" + liveMCPStdioName + "__echo,mcp__" + liveMCPHTTPName + "__echo"}, ""},
		// opencode's free model.
		{runtimes.OpenCode, func() (string, error) { return exec.LookPath("opencode") },
			[]string{"--model", "opencode/big-pickle"}, ""},
		// agy's default review mode auto-denies tool calls in -p and it has
		// no per-tool allow flag, so it runs yolo
		// (--dangerously-skip-permissions through the posture mapping).
		{runtimes.Antigravity, func() (string, error) { return exec.LookPath("agy") }, nil, permission.ModeYolo},
	}
	for _, tc := range cases {
		t.Run(string(tc.runtime), func(t *testing.T) {
			binary, err := tc.lookup()
			if err != nil {
				t.Skipf("%s not installed: %v", tc.runtime, err)
			}
			fx := newLiveMCPFixture(t)
			execution := preparedLiveMCP(t, tc.runtime, runtimes.ModeSubprocessPerTurn, binary, tc.flags, tc.posture, fx)
			adapter, err := launch.Select(launch.Selection{Runtime: string(tc.runtime), Mode: runtimes.ModeSubprocessPerTurn})
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			sink := newCapturingSink()
			w, err := New(Config{
				App: "live-mcp-native-" + string(tc.runtime), Adapter: adapter,
				Activity: activity.NewBridge(sink), Workdir: execution.Bindings.CWD,
				PreparedExecution: execution,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			defer cancel()
			runDone := make(chan error, 1)
			go func() { runDone <- w.Run(ctx) }()
			defer stopLive(t, w, runDone)

			sink.waitFor(t, runtimeevents.KindSessionReady, time.Minute)
			if err := w.SendInput(ctx, []byte(fx.prompt())); err != nil {
				if errors.Is(err, provider.ErrProviderNotAuthenticated) {
					t.Skipf("%s is not signed in on this machine: %v", tc.runtime, err)
				}
				t.Fatalf("SendInput: %v", err)
			}
			end := liveTurnOutcome(t, sink, 5*time.Minute)
			if stdio, http := fx.reached(t); !stdio || !http {
				t.Fatalf("MCP calls reaching the server: stdio=%v http=%v; turn ended %s %s; events %v",
					stdio, http, end.Kind, end.Payload, sink.kinds())
			}
			t.Logf("%s native called both MCP tools: %+v", tc.runtime, readLiveMCPCalls(t, fx.record))
		})
	}
}

func stopLive(t *testing.T, w *Wrapper, runDone <-chan error) {
	t.Helper()
	_ = w.Stop(context.Background())
	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Error("Run did not return after Stop")
	}
}

// TestLiveMCPToolsCodexAppServer drives the native Codex app-server through
// the wrapper. The thread protocol is the host's, so the test writes the
// initialize/thread/turn frames itself. The default posture launches Codex
// with approval_policy on-request, so it asks before each MCP tool call; the
// wrapper answers from PermissionPosture default with MCPAllow naming the two
// echo servers.
func TestLiveMCPToolsCodexAppServer(t *testing.T) {
	testgate.RequireLiveProvider(t)
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("codex not installed: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(home, ".codex", "auth.json")
	if _, statErr := os.Stat(authPath); statErr != nil {
		t.Skipf("codex is not logged in (%v)", statErr)
	}

	fx := newLiveMCPFixture(t)
	execution := preparedLiveMCP(t, runtimes.Codex, runtimes.ModeJSONRPCStdio, binary,
		[]string{"-c", `model="gpt-5.6-luna"`}, permission.ModeDefault, fx)
	// CODEX_HOME is the planted boot dir, whose auth.json is an empty
	// placeholder until the execution edge fills it.
	prep, err := provider.PrepareRuntime(context.Background(), provider.RuntimePreparationRequest{
		Projection: provider.ProviderProjection{
			Provider: runtimes.Codex, Mode: runtimes.ModeJSONRPCStdio,
			Effects: []provider.ProviderEffect{{Kind: provider.EffectCodexAuthJSON, Destination: "auth.json"}},
		},
		Roots:           provider.ProjectionRoots{BootRoot: execution.Roots.BootRoot},
		Policy:          provider.PreparationPolicy{AllowCredentials: true, AllowCleanup: true},
		RequiredEffects: []provider.ProviderEffectKind{provider.EffectCodexAuthJSON},
		CredentialResolver: provider.CredentialResolverFunc(func(context.Context, provider.CredentialRequest) (provider.Credential, error) {
			b, readErr := os.ReadFile(authPath) //nolint:gosec // G304: the user's own codex login
			return provider.Credential{Bytes: b, Mode: 0o600, Source: "~/.codex/auth.json"}, readErr
		}),
	})
	if err != nil {
		t.Fatalf("PrepareRuntime: %v", err)
	}
	defer func() { _ = prep.Cleanup(context.Background()) }()

	adapter, err := launch.Select(launch.Selection{Runtime: string(runtimes.Codex), Mode: runtimes.ModeJSONRPCStdio})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	sink := newCapturingSink()
	w, err := New(Config{
		App: "live-mcp-native-codex", Adapter: adapter,
		Activity: activity.NewBridge(sink), Workdir: execution.Bindings.CWD,
		PreparedExecution: execution,
		PermissionPosture: permission.ModeDefault,
		MCPAllow:          []string{liveMCPStdioName, liveMCPHTTPName},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(ctx) }()
	defer stopLive(t, w, runDone)
	sink.waitFor(t, runtimeevents.KindSessionReady, time.Minute)

	send := func(frame map[string]any) {
		t.Helper()
		frame["jsonrpc"] = "2.0"
		b, _ := json.Marshal(frame)
		if sendErr := w.SendInput(ctx, b); sendErr != nil {
			t.Fatalf("SendInput %s: %v", b, sendErr)
		}
	}
	send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{
		"clientInfo": map[string]any{"name": "wrapper-live-mcp", "version": "0.0.0"},
	}})
	if _, initErr := awaitCodexResponse(sink, 1, time.Minute); initErr != nil {
		t.Fatalf("initialize: %v", initErr)
	}
	send(map[string]any{"method": "initialized"})
	send(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": execution.Roots.ProjectRoot}})
	result, err := awaitCodexResponse(sink, 2, time.Minute)
	if err != nil {
		t.Fatalf("thread/start: %v", err)
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &started); err != nil || started.Thread.ID == "" {
		t.Fatalf("thread/start result %s: no thread id (%v)", result, err)
	}
	send(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{
		"threadId": started.Thread.ID,
		"input":    []any{map[string]any{"type": "text", "text": fx.prompt()}},
	}})
	fx.waitForBothCalls(t, 4*time.Minute, sink)

	// Each call was asked about and approved from the posture.
	approved := 0
	for _, ev := range sink.snapshot() {
		if ev.Kind != runtimeevents.KindAgentPermissionResolved {
			continue
		}
		var p struct {
			Allowed bool   `json:"allowed"`
			Kind    string `json:"kind"`
			Posture string `json:"posture"`
			Reason  string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		t.Logf("permission resolved: %s", ev.Payload)
		if p.Kind == "mcp_tool_call" && p.Allowed && p.Posture == string(permission.ModeDefault) {
			approved++
		}
	}
	if approved < 2 {
		t.Fatalf("approved MCP tool-call requests = %d, want one per call (2)", approved)
	}
	t.Logf("codex app-server called both MCP tools: %+v", readLiveMCPCalls(t, fx.record))
}

// awaitCodexResponse waits for the JSON-RPC response to id on the session's
// stdout lines and returns its result.
func awaitCodexResponse(sink *capturingSink, id int, d time.Duration) (json.RawMessage, error) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, ev := range sink.snapshot() {
			if ev.Kind != runtimeevents.KindStdoutLine {
				continue
			}
			var line struct {
				Line string `json:"line"`
			}
			if json.Unmarshal(ev.Payload, &line) != nil {
				continue
			}
			var frame struct {
				ID     *int            `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal([]byte(line.Line), &frame) != nil || frame.ID == nil || *frame.ID != id || frame.Method != "" {
				continue
			}
			if len(frame.Error) > 0 {
				return nil, fmt.Errorf("error response %s", frame.Error)
			}
			return frame.Result, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, errors.New("no response")
}
