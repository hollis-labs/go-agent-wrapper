package wrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	permission "github.com/hollis-labs/go-permission"

	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters/codex"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// CW-20260930-0139: the native Codex app-server path answers Codex's
// approval requests from Config.PermissionPosture instead of refusing every
// one, and still reports each request and its resolution.

const fakeCodexMCPToolCall = `{"threadId":"t","turnId":"u","serverName":"mux","mode":"form","_meta":{"codex_approval_kind":"mcp_tool_call","tool_params":{}},"message":"Allow the mux MCP server to run tool mux_health?","requestedSchema":{"type":"object","properties":{}}}`

const fakeCodexCommandApproval = `{"threadId":"t","turnId":"u","itemId":"i","command":"curl https://example.com","cwd":"/work","reason":"network access"}`

const fakeCodexUserInput = `{"threadId":"t","turnId":"u","itemId":"i","questions":[]}`

// writeFakeCodexApprovals writes a fake codex app-server that sends three
// server-initiated requests (an MCP tool-call elicitation, a command
// approval and a user-input question), waits for each answer, and records
// the answers to answers.jsonl in dir before exiting.
func writeFakeCodexApprovals(t *testing.T, dir string) (script, answers string) {
	t.Helper()
	answers = filepath.Join(dir, "answers.jsonl")
	body := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' '{"jsonrpc":"2.0","id":"srv-1","method":"mcpServer/elicitation/request","params":%s}'
IFS= read -r line
printf '%%s\n' "$line" >> %s
printf '%%s\n' '{"jsonrpc":"2.0","id":"srv-2","method":"item/commandExecution/requestApproval","params":%s}'
IFS= read -r line
printf '%%s\n' "$line" >> %s
printf '%%s\n' '{"jsonrpc":"2.0","id":"srv-3","method":"item/tool/requestUserInput","params":%s}'
IFS= read -r line
printf '%%s\n' "$line" >> %s
`, fakeCodexMCPToolCall, answers, fakeCodexCommandApproval, answers, fakeCodexUserInput, answers)
	return writeShellFixtureLauncher(t, dir, "fake-codex-approvals", []byte(body)), answers
}

func TestRunCodexAppServerAnswersApprovalsFromPosture(t *testing.T) {
	skipUnlessSh(t)
	cases := []struct {
		posture     permission.Mode
		wantMCP     string
		wantCommand string
	}{
		{"", `{"action":"accept","content":{}}`, `{"decision":"decline"}`},
		{permission.ModeDefault, `{"action":"accept","content":{}}`, `{"decision":"decline"}`},
		{permission.ModeAcceptEdits, `{"action":"accept","content":{}}`, `{"decision":"decline"}`},
		{permission.ModePlan, `{"action":"decline"}`, `{"decision":"decline"}`},
		{permission.ModeYolo, `{"action":"accept","content":{}}`, `{"decision":"accept"}`},
	}
	for _, tc := range cases {
		name := string(tc.posture)
		if name == "" {
			name = "zero-value"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			script, answersPath := writeFakeCodexApprovals(t, dir)
			t.Setenv("CODEX_CLI_PATH", script)

			sink := newCapturingSink()
			w, err := New(Config{
				App:               "test-codex-approvals",
				Adapter:           codex.New(),
				Activity:          activity.NewBridge(sink),
				Workdir:           dir,
				PermissionPosture: tc.posture,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := w.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}

			answers := readAnswers(t, answersPath)
			if len(answers) != 3 {
				t.Fatalf("fake codex recorded %d answers, want 3: %v", len(answers), answers)
			}
			if got := string(answers["srv-1"].Result); got != tc.wantMCP {
				t.Errorf("MCP tool-call answer = %s, want %s", got, tc.wantMCP)
			}
			if got := string(answers["srv-2"].Result); got != tc.wantCommand {
				t.Errorf("command answer = %s, want %s", got, tc.wantCommand)
			}
			// A question nobody is present to answer is refused with an
			// error in every posture, never granted.
			if answers["srv-3"].Error == nil || answers["srv-3"].Error.Code != -32601 || len(answers["srv-3"].Result) != 0 {
				t.Errorf("user-input answer = %+v, want a -32601 error and no result", answers["srv-3"])
			}

			wantPosture := string(tc.posture)
			if wantPosture == "" {
				wantPosture = string(permission.ModeDefault)
			}
			assertPermissionEvents(t, sink.snapshot(), wantPosture, map[string]permissionResolved{
				"mcpServer/elicitation/request":         {Allowed: tc.wantMCP != `{"action":"decline"}`, Kind: "mcp_tool_call"},
				"item/commandExecution/requestApproval": {Allowed: tc.wantCommand == `{"decision":"accept"}`, Kind: "command_execution"},
				"item/tool/requestUserInput":            {Allowed: false, Kind: "", HasError: true},
			})
		})
	}
}

func TestNewRejectsUnknownPermissionPosture(t *testing.T) {
	_, err := New(Config{
		App:               "test-codex-approvals",
		Adapter:           codex.New(),
		Activity:          activity.NewBridge(newCapturingSink()),
		Workdir:           t.TempDir(),
		PermissionPosture: "bypassPermissions",
	})
	if err == nil || !strings.Contains(err.Error(), "PermissionPosture") {
		t.Fatalf("New err = %v, want a PermissionPosture error", err)
	}
}

type fakeAnswer struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func readAnswers(t *testing.T, path string) map[string]fakeAnswer {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read answers: %v", err)
	}
	out := map[string]fakeAnswer{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var a fakeAnswer
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Fatalf("answer %q: %v", line, err)
		}
		out[a.ID] = a
	}
	return out
}

type permissionResolved struct {
	Allowed  bool
	Kind     string
	HasError bool
}

// assertPermissionEvents checks that each request emitted a requested event
// and a resolved event parented to it, in the same turn, with the decision.
func assertPermissionEvents(t *testing.T, evs []runtimeevents.Event, posture string, want map[string]permissionResolved) {
	t.Helper()
	requested := map[string]runtimeevents.Event{}
	for _, ev := range evs {
		if ev.Kind != runtimeevents.KindAgentPermissionRequested {
			continue
		}
		var p struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		requested[ev.ID] = ev
		if ev.TurnID == "" {
			t.Errorf("permission.requested for %s carries no TurnID", p.Method)
		}
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		if ev.Kind != runtimeevents.KindAgentPermissionResolved {
			continue
		}
		var p struct {
			Method  string `json:"method"`
			Allowed bool   `json:"allowed"`
			Kind    string `json:"kind"`
			Posture string `json:"posture"`
			Reason  string `json:"reason"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		req, ok := requested[ev.ParentID]
		if !ok {
			t.Errorf("permission.resolved for %s has ParentID %q matching no requested event", p.Method, ev.ParentID)
			continue
		}
		if req.TurnID != ev.TurnID {
			t.Errorf("permission.resolved for %s TurnID %q != requested %q", p.Method, ev.TurnID, req.TurnID)
		}
		w, ok := want[p.Method]
		if !ok {
			t.Errorf("unexpected permission.resolved for %s", p.Method)
			continue
		}
		seen[p.Method] = true
		if p.Allowed != w.Allowed || p.Kind != w.Kind || (p.Error != "") != w.HasError {
			t.Errorf("permission.resolved for %s = %+v, want allowed=%v kind=%q error=%v", p.Method, p, w.Allowed, w.Kind, w.HasError)
		}
		if p.Posture != posture || p.Reason == "" {
			t.Errorf("permission.resolved for %s posture=%q reason=%q, want posture %q and a reason", p.Method, p.Posture, p.Reason, posture)
		}
	}
	for method := range want {
		if !seen[method] {
			t.Errorf("no permission.resolved for %s", method)
		}
	}
}
