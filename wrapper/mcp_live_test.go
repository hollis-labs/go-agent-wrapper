package wrapper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/internal/testgate"
	"github.com/hollis-labs/go-agent-wrapper/launch"
)

// CW-20260930-0136 acceptance: the same MCP servers yield working MCP tools
// on every installed runtime, natively (planted from the launch plan's
// MCPSpec by agentkit's providerplant) and over ACP (Config.ACPMCPServers).
// Each session gets two servers, the echo server of live_mcp_server_test.go
// over stdio and over streamable HTTP on loopback, and is asked to call both
// tools; the test passes only when both calls reach the server.
//
// Copilot and Pi are not installed on the machine this was written on.
// Their side of the contract (Copilot advertises mcpCapabilities.http, Pi
// does not and pi-acp never wires session MCP servers) is covered by the
// recorded-fixture tests in acp and adapters/*.

const (
	liveMCPStdioName = "wrapstdio"
	liveMCPHTTPName  = "wraphttp"
)

// liveMCPFixture is one session's pair of echo servers and the nonces the
// agent is asked to send them.
type liveMCPFixture struct {
	record     string
	binary     string
	endpoint   string
	stdioNonce string
	httpNonce  string
}

func newLiveMCPFixture(t *testing.T) *liveMCPFixture {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("test binary: %v", err)
	}
	record := filepath.Join(t.TempDir(), "calls.jsonl")
	return &liveMCPFixture{
		record:     record,
		binary:     binary,
		endpoint:   startLiveMCPHTTP(t, record),
		stdioNonce: "stdio-" + liveNonce(t),
		httpNonce:  "http-" + liveNonce(t),
	}
}

func liveNonce(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func (f *liveMCPFixture) prompt() string {
	return fmt.Sprintf("You have two MCP servers, %[1]q and %[2]q, and each has one tool named echo. "+
		"Call the echo tool of the %[1]q server with text %[3]q. "+
		"Then call the echo tool of the %[2]q server with text %[4]q. "+
		"Really call both tools; do not just describe the calls. "+
		"When both have returned, reply with the single word: done",
		liveMCPStdioName, liveMCPHTTPName, f.stdioNonce, f.httpNonce)
}

func (f *liveMCPFixture) acpServers() []acp.MCPServer {
	return []acp.MCPServer{
		{Name: liveMCPStdioName, Command: f.binary, Env: map[string]string{liveMCPServeEnv: f.record}},
		{Name: liveMCPHTTPName, URL: f.endpoint},
	}
}

// reached reports whether both nonces arrived over their own transport.
func (f *liveMCPFixture) reached(t *testing.T) (stdio, http bool) {
	t.Helper()
	for _, c := range readLiveMCPCalls(t, f.record) {
		switch {
		case c.Transport == "stdio" && c.Text == f.stdioNonce:
			stdio = true
		case c.Transport == "http" && c.Text == f.httpNonce:
			http = true
		}
	}
	return stdio, http
}

// waitForBothCalls polls the call record until both tool calls have reached
// the server.
func (f *liveMCPFixture) waitForBothCalls(t *testing.T, d time.Duration, sink *capturingSink) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		stdio, http := f.reached(t)
		if stdio && http {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("MCP tool calls reaching the server: stdio=%v http=%v; recorded %+v; events %v",
				stdio, http, readLiveMCPCalls(t, f.record), sink.kinds())
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// waitForACPState waits for the wrapper's ACP session to reach want. The
// bridges start through npx, which can take longer than waitForACPReady's
// ten seconds.
func waitForACPState(t *testing.T, manager *acp.Manager, id string, want acp.State, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if session, ok := manager.Lookup(id); ok && session.Snapshot().State == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if session, ok := manager.Lookup(id); ok {
		t.Fatalf("ACP session state = %q, want %q", session.Snapshot().State, want)
	}
	t.Fatalf("ACP session %q was not registered", id)
}

// approveEchoPermissions answers session/request_permission: allow once for
// a call to the echo tool, reject anything else. It records what it saw.
type approveEchoPermissions struct {
	mu   sync.Mutex
	seen []string
}

func (a *approveEchoPermissions) respond(_ context.Context, req acp.PermissionRequest) (acp.PermissionSelection, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	call := req.ToolCall.Name + " " + req.ToolCall.Title + " " + string(req.ToolCall.RawInput)
	// codex-acp names no tool in an MCP approval, only marks it:
	// _meta.is_mcp_tool_approval.
	var meta struct {
		Meta struct {
			MCPToolApproval bool `json:"is_mcp_tool_approval"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(req.RawParams, &meta)
	if meta.Meta.MCPToolApproval {
		call = "codex MCP tool approval " + req.ToolCall.ToolCallID
	} else if !strings.Contains(call, "echo") {
		a.seen = append(a.seen, "rejected: "+call+" raw="+string(req.RawParams))
		return acp.PermissionSelection{}, nil
	}
	for _, o := range req.Options {
		if o.Kind == acp.PermissionAllowOnce {
			a.seen = append(a.seen, "allowed: "+call)
			return acp.SelectPermissionOption(o.OptionID), nil
		}
	}
	a.seen = append(a.seen, "no allow_once option: "+call)
	return acp.PermissionSelection{}, nil
}

func (a *approveEchoPermissions) log() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

func TestLiveMCPToolsOverACP(t *testing.T) {
	testgate.RequireLiveProvider(t)
	cases := []struct {
		runtime runtimes.ID
		needs   string
		config  map[string]any
	}{
		// The cheapest model each bridge offers.
		{runtimes.Claude, "npx", map[string]any{"model": "haiku"}},
		{runtimes.Codex, "npx", map[string]any{"model": "gpt-5.6-luna"}},
		// opencode's default model is a free one.
		{runtimes.OpenCode, "opencode", nil},
	}
	for _, tc := range cases {
		t.Run(string(tc.runtime), func(t *testing.T) {
			if _, err := exec.LookPath(tc.needs); err != nil {
				t.Skipf("%s not installed: %v", tc.needs, err)
			}
			if tc.runtime == runtimes.Codex {
				if _, err := exec.LookPath("codex"); err != nil {
					t.Skipf("codex not installed: %v", err)
				}
			}
			adapter, err := launch.Select(launch.Selection{Runtime: string(tc.runtime), Mode: runtimes.ModeACPStdio})
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			fx := newLiveMCPFixture(t)
			perms := &approveEchoPermissions{}
			var diagMu sync.Mutex
			var diags []string
			sink := newCapturingSink()
			manager := acp.NewManager()
			w, err := New(Config{
				App: "live-mcp-acp-" + string(tc.runtime), Adapter: adapter,
				Activity: activity.NewBridge(sink), Workdir: t.TempDir(), ACPManager: manager,
				ACPMCPServers:                           fx.acpServers(),
				ACPSessionConfig:                        tc.config,
				ACPBestEffortPermissionRequestResponder: perms.respond,
				OnACPDiagnostic: func(d acp.Diagnostic) {
					diagMu.Lock()
					diags = append(diags, fmt.Sprintf("%+v", d))
					diagMu.Unlock()
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			runDone := make(chan error, 1)
			go func() { runDone <- w.Run(ctx) }()
			defer func() {
				_ = w.Stop(context.Background())
				select {
				case <-runDone:
				case <-time.After(30 * time.Second):
					t.Error("Run did not return after Stop")
				}
				diagMu.Lock()
				t.Logf("permission requests: %q; diagnostics: %q", perms.log(), diags)
				diagMu.Unlock()
			}()

			waitForACPState(t, manager, w.SessionID(), acp.StateReady, 2*time.Minute)
			if err := w.SendInput(ctx, []byte(fx.prompt())); err != nil {
				t.Fatalf("SendInput: %v", err)
			}
			fx.waitForBothCalls(t, 3*time.Minute, sink)
			waitForACPState(t, manager, w.SessionID(), acp.StateReady, 2*time.Minute)
			t.Logf("%s over ACP called both MCP tools: %+v", tc.runtime, readLiveMCPCalls(t, fx.record))
		})
	}
}
