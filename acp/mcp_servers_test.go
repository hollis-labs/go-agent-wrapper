package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

// CW-20260930-0136 item 2: MCP servers in ACP's session/new and session/load
// shapes.
func TestSessionMCPServers(t *testing.T) {
	servers := []MCPServer{
		{Name: "torque", URL: "http://127.0.0.1:8990/mcp", Headers: map[string]string{"X-B": "2", "Authorization": "Bearer t"}},
		{Name: "nanite", Command: "/bin/nanite", Args: []string{"mcp"}, Env: map[string]string{"Z": "1", "A": "0"}},
		{Name: "bare", Command: "/bin/bare"},
	}
	wire, skipped, err := SessionMCPServers(servers, true)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("err=%v skipped=%v", err, skipped)
	}
	got, _ := json.Marshal(wire)
	want := `[{"headers":[{"name":"Authorization","value":"Bearer t"},{"name":"X-B","value":"2"}],"name":"torque","type":"http","url":"http://127.0.0.1:8990/mcp"},` +
		`{"args":["mcp"],"command":"/bin/nanite","env":[{"name":"A","value":"0"},{"name":"Z","value":"1"}],"name":"nanite"},` +
		`{"args":[],"command":"/bin/bare","env":[],"name":"bare"}]`
	if string(got) != want {
		t.Errorf("wire =\n%s\nwant\n%s", got, want)
	}
}

// An agent without mcpCapabilities.http gets the stdio servers only; the
// HTTP ones are named, never their headers.
func TestSessionMCPServersDropsHTTPWithoutCapability(t *testing.T) {
	servers := []MCPServer{
		{Name: "torque", URL: "http://x/mcp", Headers: map[string]string{"Authorization": "Bearer secret"}},
		{Name: "nanite", Command: "/bin/nanite"},
	}
	wire, skipped, err := SessionMCPServers(servers, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != 1 || len(skipped) != 1 || skipped[0] != "torque" {
		t.Fatalf("wire=%v skipped=%v", wire, skipped)
	}
	var diag Diagnostic
	ReportSkippedMCPServers(func(d Diagnostic) { diag = d }, skipped)
	text, _ := json.Marshal(diag)
	if !strings.Contains(string(text), "torque") || strings.Contains(string(text), "secret") {
		t.Errorf("diagnostic = %s; want the server name and no header value", text)
	}
}

func TestSessionMCPServersAlwaysAnArray(t *testing.T) {
	wire, _, err := SessionMCPServers(nil, true)
	if err != nil || wire == nil {
		t.Fatalf("wire=%v err=%v; want an empty, non-nil array", wire, err)
	}
	if b, _ := json.Marshal(wire); string(b) != "[]" {
		t.Errorf("wire = %s", b)
	}
}

func TestSessionMCPServersRejectsBadSpecs(t *testing.T) {
	for name, servers := range map[string][]MCPServer{
		"empty name":     {{Command: "/bin/x"}},
		"duplicate":      {{Name: "a", Command: "/bin/a"}, {Name: "a", URL: "http://a"}},
		"two transports": {{Name: "a", Command: "/bin/a", URL: "http://a"}},
		"no transport":   {{Name: "a"}},
	} {
		if _, _, err := SessionMCPServers(servers, true); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParseInitializeResultMCPHTTP(t *testing.T) {
	got, err := ParseInitializeResult(json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"mcpCapabilities":{"http":true,"sse":true}}}`), 1)
	if err != nil || !got.MCPHTTP {
		t.Fatalf("MCPHTTP = %v, err %v; want true", got.MCPHTTP, err)
	}
	got, _ = ParseInitializeResult(json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{}}`), 1)
	if got.MCPHTTP {
		t.Error("MCPHTTP true without mcpCapabilities")
	}
}
