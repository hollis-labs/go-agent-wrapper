package opencodeacp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
)

// writeRecordingACPScript writes a fake ACP agent that records every frame it
// receives and advertises mcpCapabilities.http as given.
func writeRecordingACPScript(t *testing.T, dir string, httpCap bool) (script, record string) {
	t.Helper()
	record = filepath.Join(dir, "frames.ndjson")
	capability := "false"
	if httpCap {
		capability = "true"
	}
	body := `#!/bin/sh
while IFS= read -r line; do
  printf '%s\n' "$line" >> "` + record + `"
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"authMethods":[],"agentCapabilities":{"loadSession":true,"mcpCapabilities":{"http":` + capability + `}}}}\n' "$id" ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_mcp"}}\n' "$id" ;;
    *'"method":"session/load"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id" ;;
  esac
done
`
	script = filepath.Join(dir, "fake-acp.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: test fixture
		t.Fatal(err)
	}
	return script, record
}

// sentMCPServers returns the mcpServers of the first frame calling method.
func sentMCPServers(t *testing.T, record, method string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(record) //nolint:gosec // G304: test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var frame struct {
			Method string `json:"method"`
			Params struct {
				MCPServers []map[string]any `json:"mcpServers"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &frame) == nil && frame.Method == method {
			return frame.Params.MCPServers
		}
	}
	t.Fatalf("no %s frame in %s", method, raw)
	return nil
}

var testACPMCPServers = []acp.MCPServer{
	{Name: "torque", URL: "http://127.0.0.1:8990/mcp", Headers: map[string]string{"Authorization": "Bearer t"}},
	{Name: "nanite", Command: "/bin/nanite", Args: []string{"mcp"}, Env: map[string]string{"TOKEN": "x"}},
}

// CW-20260930-0136 item 2: session/new and session/load carry the app's MCP
// servers in ACP form, HTTP only to an agent that accepts it.
func TestLaunchSendsMCPServers(t *testing.T) {
	skipUnlessSh(t)
	cases := []struct {
		name    string
		httpCap bool
		preset  string
		method  string
		want    []string
	}{
		{"session/new with http", true, "", "session/new", []string{"torque", "nanite"}},
		{"session/new without http", false, "", "session/new", []string{"nanite"}},
		{"session/load with http", true, "ses_old", "session/load", []string{"torque", "nanite"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script, record := writeRecordingACPScript(t, dir, tc.httpCap)
			c := NewClient(WithClientBinary(script))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var diags []acp.Diagnostic
			params := acp.LaunchParams{Cwd: dir, SessionIDPreset: tc.preset, MCPServers: testACPMCPServers,
				OnDiagnostic: func(d acp.Diagnostic) { diags = append(diags, d) }}
			if err := c.Launch(ctx, params); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			defer func() { _ = c.Close(context.Background()) }()
			sent := sentMCPServers(t, record, tc.method)
			var names []string
			for _, s := range sent {
				names = append(names, s["name"].(string))
			}
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("%s mcpServers = %v; want %v", tc.method, sent, tc.want)
			}
			for _, s := range sent {
				switch s["name"] {
				case "torque":
					headers, _ := s["headers"].([]any)
					if s["type"] != "http" || len(headers) != 1 {
						t.Errorf("http server = %v", s)
					}
				case "nanite":
					env, _ := s["env"].([]any)
					if s["command"] != "/bin/nanite" || len(env) != 1 {
						t.Errorf("stdio server = %v", s)
					}
				}
			}
			if !tc.httpCap && len(diags) == 0 {
				t.Error("dropped HTTP server was not reported")
			}
		})
	}
}
