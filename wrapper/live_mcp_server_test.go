package wrapper

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// A minimal MCP server with one tool, echo, for the live MCP tests
// (CW-20260930-0136). The same dispatch serves stdio, where TestMain runs the
// test binary itself as the server when liveMCPServeEnv is set, and
// streamable HTTP, from an httptest server on loopback. Every tools/call is
// appended as one JSON line to a record file (for stdio, the value of
// liveMCPServeEnv), so a test can prove the call reached the server whichever
// process served it.

const liveMCPServeEnv = "_GO_AGENT_WRAPPER_LIVE_MCP_STDIO"

// liveMCPCall is one recorded tools/call.
type liveMCPCall struct {
	Transport string `json:"transport"`
	Tool      string `json:"tool"`
	Text      string `json:"text"`
}

type liveMCPMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type liveMCPServer struct {
	transport string
	record    string
	mu        sync.Mutex
}

// supportedMCPVersions are echoed back when a client asks for one of them;
// anything else is answered with the newest, which every current client
// accepts.
var supportedMCPVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}

// handle answers one JSON-RPC message. It returns nil for a notification.
func (s *liveMCPServer) handle(raw []byte) []byte {
	var msg liveMCPMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return liveMCPError(nil, -32700, "parse error")
	}
	if len(msg.ID) == 0 {
		return nil
	}
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		version := supportedMCPVersions[len(supportedMCPVersions)-1]
		for _, v := range supportedMCPVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		return liveMCPResult(msg.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "wrapper-live-echo", "version": "0.0.0"},
		})
	case "ping":
		return liveMCPResult(msg.ID, map[string]any{})
	case "tools/list":
		return liveMCPResult(msg.ID, map[string]any{"tools": []any{map[string]any{
			"name":        "echo",
			"description": "Echoes the given text back. Call it whenever you are asked to use the echo tool.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
				"required":   []string{"text"},
			},
		}}})
	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				Text string `json:"text"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.Name != "echo" {
			return liveMCPError(msg.ID, -32602, "unknown tool")
		}
		if err := s.recordCall(liveMCPCall{Transport: s.transport, Tool: p.Name, Text: p.Arguments.Text}); err != nil {
			return liveMCPError(msg.ID, -32603, err.Error())
		}
		return liveMCPResult(msg.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "echo: " + p.Arguments.Text}},
			"isError": false,
		})
	default:
		return liveMCPError(msg.ID, -32601, "method not found: "+msg.Method)
	}
}

func (s *liveMCPServer) recordCall(call liveMCPCall) error {
	line, err := json.Marshal(call)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.record, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // G304: test-owned temp path
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func liveMCPResult(id json.RawMessage, result any) []byte {
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return out
}

func liveMCPError(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	return out
}

// serveLiveMCPStdio is the stdio server TestMain runs: newline-delimited
// JSON-RPC on stdin and stdout until stdin closes.
func serveLiveMCPStdio(in io.Reader, out io.Writer, record string) error {
	s := &liveMCPServer{transport: "stdio", record: record}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if reply := s.handle([]byte(line)); reply != nil {
			if _, err := out.Write(append(reply, '\n')); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

// startLiveMCPHTTP serves the echo server over streamable HTTP on a loopback
// httptest server and returns its endpoint. Each POST carries one message
// and gets a JSON response (or 202 for a notification); there is no
// server-to-client stream, so GET is refused with 405 as the spec allows.
func startLiveMCPHTTP(t *testing.T, record string) string {
	t.Helper()
	s := &liveMCPServer{transport: "http", record: record}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply := s.handle(body)
		if reply == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(reply)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp"
}

// readLiveMCPCalls returns every tools/call recorded so far.
func readLiveMCPCalls(t *testing.T, record string) []liveMCPCall {
	t.Helper()
	raw, err := os.ReadFile(record) //nolint:gosec // G304: test-owned temp path
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read MCP call record: %v", err)
	}
	var calls []liveMCPCall
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var c liveMCPCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("decode MCP call record %q: %v", line, err)
		}
		calls = append(calls, c)
	}
	return calls
}

// TestLiveMCPServerSpeaksMCP checks the echo server offline, so a protocol
// mistake shows up without spending a live run: initialize, tools/list and
// tools/call over stdio and HTTP, with the call recorded.
func TestLiveMCPServerSpeaksMCP(t *testing.T) {
	record := t.TempDir() + "/calls.jsonl"
	frames := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"via-stdio"}}}`,
	}
	var out strings.Builder
	if err := serveLiveMCPStdio(strings.NewReader(strings.Join(frames, "\n")+"\n"), &out, record); err != nil {
		t.Fatal(err)
	}
	replies := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(replies) != 3 {
		t.Fatalf("stdio replies = %q, want 3 (no reply to the notification)", replies)
	}
	for _, want := range []string{`"protocolVersion":"2025-06-18"`, `"name":"echo"`, `"text":"echo: via-stdio"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdio replies missing %s:\n%s", want, out.String())
		}
	}

	endpoint := startLiveMCPHTTP(t, record)
	resp, err := http.Post(endpoint, "application/json", strings.NewReader( //nolint:gosec // G107: the test's own loopback server
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"text":"via-http"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"text":"echo: via-http"`) {
		t.Fatalf("HTTP tools/call = %d %s", resp.StatusCode, body)
	}

	got := readLiveMCPCalls(t, record)
	want := []liveMCPCall{{"stdio", "echo", "via-stdio"}, {"http", "echo", "via-http"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("recorded calls = %+v, want %+v", got, want)
	}
}
