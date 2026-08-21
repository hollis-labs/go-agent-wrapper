package claudeacp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func skipUnlessSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake script needs sh; not running on Windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available on PATH")
	}
}

// fakeACPScript writes a small sh script that speaks JUST ENOUGH of the
// real, empirically-verified claude-agent-acp wire shape (see package
// doc) to drive [Client] through a full initialize → session/new →
// session/prompt (with session/update notifications) → response cycle,
// without invoking the real bridge or npx. Method routing is a crude
// substring match on the raw JSON-RPC line — safe here because this test
// file controls exactly what [Client] sends.
//
// Deliberately exercises the two real divergences from
// [adapters/opencodeacp]'s own fake script (see package doc): an
// agent_thought_chunk carrying `content.text` (not a flat `thought`
// field), and a tool_call_update sequence with an intermediate
// no-status/no-output refinement followed by a terminal one carrying
// `rawOutput` (not `result`) and no `isError` field.
func fakeACPScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude-agent-acp.sh")
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true},"authMethods":[]}}\n' "$id"
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_fake123"}}\n' "$id"
      ;;
    *'"method":"session/load"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_loaded456"}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking..."}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"agent_message_chunk","messageId":"msg_1","content":{"type":"text","text":"pong"}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"bash","kind":"execute","status":"pending","rawInput":{"command":"echo hi"}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","title":"echo hi","rawInput":{"command":"echo hi"}}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed","rawOutput":"hi"}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_fake123","update":{"sessionUpdate":"usage_update","used":10,"size":100}}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn","usage":{"totalTokens":5}}}\n' "$id"
      ;;
    *'"method":"session/request_permission"'*)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"fake: unexpected permission request"}}\n' "$id"
      ;;
    *'"method":"session/cancel"'*)
      : # notification, no response expected
      ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: test fixture
		t.Fatalf("write fake script: %v", err)
	}
	return script
}

func drainEvents(t *testing.T, c *Client, timeout time.Duration) []runtimeevents.Event {
	t.Helper()
	var out []runtimeevents.Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
			if ev.Kind == runtimeevents.KindTurnCompleted || ev.Kind == runtimeevents.KindTurnFailed {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out waiting for events; collected so far: %+v", out)
		}
	}
}

func hasKind(evs []runtimeevents.Event, kind runtimeevents.EventKind) bool {
	for _, ev := range evs {
		if ev.Kind == kind {
			return true
		}
	}
	return false
}

func TestClientLaunchPromptEvents_FakeSubprocess(t *testing.T) {
	skipUnlessSh(t)
	script := fakeACPScript(t)

	c := NewClient(WithClientDirectBinary(script))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Launch(ctx, launchParams(t)); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	if err := c.Prompt(ctx, "ping"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	evs := drainEvents(t, c, 8*time.Second)

	for _, want := range []runtimeevents.EventKind{
		runtimeevents.KindProcessStarted,
		runtimeevents.KindSessionReady,
		runtimeevents.KindTurnStarted,
		runtimeevents.KindAgentDelta,
		runtimeevents.KindAgentToolUse,
		runtimeevents.KindAgentToolResult,
		runtimeevents.KindTurnCompleted,
	} {
		if !hasKind(evs, want) {
			t.Errorf("missing expected event kind %q in %+v", want, evs)
		}
	}

	// available_commands_update/usage_update are deliberately unmapped
	// (informational) — confirm no stray/placeholder events leaked
	// through for them by checking the delta count matches exactly the
	// two chunks the fake script emits (one thought, one message).
	var deltaCount int
	var thoughtSeen, messageSeen bool
	for _, ev := range evs {
		if ev.Kind == runtimeevents.KindAgentDelta {
			deltaCount++
			var payload struct {
				Content string `json:"content"`
				Phase   string `json:"phase"`
			}
			_ = json.Unmarshal(ev.Payload, &payload)
			switch payload.Phase {
			case "thought":
				thoughtSeen = true
				if payload.Content != "thinking..." {
					t.Errorf("thought delta content = %q, want %q (agent_thought_chunk's content.text field, not a flat `thought` field)", payload.Content, "thinking...")
				}
			case "message":
				messageSeen = true
			}
		}
	}
	if deltaCount != 2 {
		t.Errorf("agent.delta count = %d, want 2 (one thought chunk, one message chunk)", deltaCount)
	}
	if !thoughtSeen || !messageSeen {
		t.Errorf("expected both a thought and a message delta; thoughtSeen=%v messageSeen=%v", thoughtSeen, messageSeen)
	}

	// tool_call_update: two frames in the fake script, only the second
	// carries status/rawOutput (see fakeACPScript's doc comment) — both
	// must still produce a KindAgentToolResult event (unconditional
	// per-frame forwarding), and the terminal one must decode `rawOutput`
	// into the event's `result` key.
	var toolResultCount int
	var sawTerminalResult bool
	for _, ev := range evs {
		if ev.Kind != runtimeevents.KindAgentToolResult {
			continue
		}
		toolResultCount++
		var payload struct {
			Status string `json:"status"`
			Result any    `json:"result"`
		}
		_ = json.Unmarshal(ev.Payload, &payload)
		if payload.Status == "completed" {
			sawTerminalResult = true
			if payload.Result != "hi" {
				t.Errorf("terminal tool_call_update result = %v, want %q (decoded from rawOutput)", payload.Result, "hi")
			}
		}
	}
	if toolResultCount != 2 {
		t.Errorf("agent.tool_result count = %d, want 2 (one intermediate refinement, one terminal)", toolResultCount)
	}
	if !sawTerminalResult {
		t.Error("never saw the terminal tool_call_update (status=completed) translated")
	}
}

func TestClientPromptBeforeLaunchErrors(t *testing.T) {
	c := NewClient()
	if err := c.Prompt(context.Background(), "hi"); err == nil {
		t.Fatal("Prompt before Launch: want error, got nil")
	}
}

func TestClientLaunchTwiceErrors(t *testing.T) {
	skipUnlessSh(t)
	script := fakeACPScript(t)
	c := NewClient(WithClientDirectBinary(script))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Launch(ctx, launchParams(t)); err != nil {
		t.Fatalf("first Launch: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()
	if err := c.Launch(ctx, launchParams(t)); err == nil {
		t.Fatal("second Launch: want error, got nil")
	}
}

func TestClientCloseIsIdempotentAndClosesEvents(t *testing.T) {
	skipUnlessSh(t)
	script := fakeACPScript(t)
	c := NewClient(WithClientDirectBinary(script))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Launch(ctx, launchParams(t)); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-c.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("Events channel never closed after Close")
		}
	}
}

func TestClientCancelWithNoActiveTurnIsNoop(t *testing.T) {
	skipUnlessSh(t)
	script := fakeACPScript(t)
	c := NewClient(WithClientDirectBinary(script))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Launch(ctx, launchParams(t)); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()
	if err := c.Cancel(ctx); err != nil {
		t.Fatalf("Cancel with no active turn: %v", err)
	}
}

func TestClientInterruptCapabilityIsTurnAndAnswerableWithoutLaunch(t *testing.T) {
	c := NewClient()
	if got := c.InterruptCapability(); got != adapters.InterruptTurn {
		t.Errorf("InterruptCapability() = %q, want turn", got)
	}
}

func TestClientResolveCommandDefaultsToNpx(t *testing.T) {
	c := NewClient()
	binary, args := c.resolveCommand()
	if binary != "npx" {
		t.Errorf("binary = %q, want npx", binary)
	}
	wantArgs := []string{"-y", defaultBridgePackage}
	if len(args) != len(wantArgs) || args[0] != wantArgs[0] || args[1] != wantArgs[1] {
		t.Errorf("args = %v, want %v", args, wantArgs)
	}
}

func TestClientResolveCommandDirectBinaryBypassesNpx(t *testing.T) {
	c := NewClient(WithClientDirectBinary("/opt/bin/claude-agent-acp"), WithClientExtraArgs("--verbose"))
	binary, args := c.resolveCommand()
	if binary != "/opt/bin/claude-agent-acp" {
		t.Errorf("binary = %q, want the direct override", binary)
	}
	if len(args) != 1 || args[0] != "--verbose" {
		t.Errorf("args = %v, want [--verbose] (no npx/-y/package args when bypassing npx)", args)
	}
}

func TestClientResolveCommandEnvOverrides(t *testing.T) {
	t.Setenv("CLAUDE_ACP_BRIDGE_PATH", "/env/bin/claude-agent-acp")
	c := NewClient()
	binary, args := c.resolveCommand()
	if binary != "/env/bin/claude-agent-acp" {
		t.Errorf("binary = %q, want env override", binary)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want empty", args)
	}
}

func TestClientResolveCommandBridgePackageOverride(t *testing.T) {
	c := NewClient(WithClientBridgePackage("@agentclientprotocol/claude-agent-acp@0.70.0"))
	_, args := c.resolveCommand()
	if len(args) != 2 || args[1] != "@agentclientprotocol/claude-agent-acp@0.70.0" {
		t.Errorf("args = %v, want [-y @agentclientprotocol/claude-agent-acp@0.70.0]", args)
	}
}

func launchParams(t *testing.T) acp.LaunchParams {
	t.Helper()
	return acp.LaunchParams{Cwd: t.TempDir()}
}
