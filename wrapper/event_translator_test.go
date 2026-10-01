package wrapper

import (
	"testing"
	"time"

	llmtypes "github.com/hollis-labs/go-llm-types"
	pevents "github.com/hollis-labs/go-providers/provider/events"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestTranslateDelta(t *testing.T) {
	kind, payload, ok := translateStreamEvent(llmtypes.StreamEvent{
		Type:    llmtypes.EventDelta,
		Content: "hello world",
	})
	if !ok {
		t.Fatal("EventDelta should be mapped")
	}
	if kind != runtimeevents.KindAgentDelta {
		t.Errorf("kind = %q, want agent.delta", kind)
	}
	p, _ := payload.(map[string]any)
	if got, _ := p["content"].(string); got != "hello world" {
		t.Errorf("payload.content = %q, want hello world", got)
	}
}

func TestTranslateToolUse(t *testing.T) {
	tu := &llmtypes.ToolUseBlock{ID: "tool_1", Name: "Read", Input: map[string]any{"path": "/tmp/x"}}
	kind, payload, ok := translateStreamEvent(llmtypes.StreamEvent{
		Type:    llmtypes.EventToolUse,
		ToolUse: tu,
	})
	if !ok {
		t.Fatal("EventToolUse should be mapped")
	}
	if kind != runtimeevents.KindAgentToolUse {
		t.Errorf("kind = %q, want agent.tool_use", kind)
	}
	p, _ := payload.(map[string]any)
	if got, _ := p["tool_use"].(*llmtypes.ToolUseBlock); got != tu {
		t.Errorf("payload.tool_use did not round-trip the block pointer")
	}
}

// Usage is not a turn boundary. Run accumulates it onto the turn's one
// terminal event; mapping it to turn.completed closed the turn early and sent
// the real terminal out untagged after session.idle (CW-20261001-0019).
func TestTranslateUsageIsNotATurnBoundary(t *testing.T) {
	_, _, ok := translateStreamEvent(llmtypes.StreamEvent{
		Type:  llmtypes.EventUsage,
		Usage: &llmtypes.Usage{InputTokens: 100, OutputTokens: 50},
	})
	if ok {
		t.Fatal("EventUsage should return ok=false (accumulated onto the terminal event)")
	}
}

func TestMergeTurnUsageSumsStepsWithoutAliasing(t *testing.T) {
	first := &llmtypes.Usage{InputTokens: 10, OutputTokens: 1, CacheReadTokens: 3, StopReason: "tool-calls"}
	total := mergeTurnUsage(nil, first)
	if total == first {
		t.Fatal("merge aliased its input")
	}
	total = mergeTurnUsage(total, &llmtypes.Usage{InputTokens: 5, OutputTokens: 2, CacheCreationTokens: 4})
	total = mergeTurnUsage(total, nil)
	want := llmtypes.Usage{InputTokens: 15, OutputTokens: 3, CacheCreationTokens: 4, CacheReadTokens: 3, StopReason: "tool-calls"}
	if *total != want {
		t.Fatalf("total = %+v, want %+v", *total, want)
	}
	total = mergeTurnUsage(total, &llmtypes.Usage{StopReason: "stop"})
	if total.StopReason != "stop" {
		t.Fatalf("StopReason = %q, want the latest non-empty one", total.StopReason)
	}
	if first.InputTokens != 10 {
		t.Fatalf("merge mutated its first input: %+v", *first)
	}
}

func TestWithTurnUsage(t *testing.T) {
	u := &llmtypes.Usage{InputTokens: 7}
	if got := withTurnUsage(nil, nil); got != nil {
		t.Errorf("nil usage changed a nil payload: %v", got)
	}
	got, _ := withTurnUsage(nil, u).(map[string]any)
	if got["usage"] != u {
		t.Errorf("nil payload: got %v", got)
	}
	in := map[string]any{"error": "boom"}
	got, _ = withTurnUsage(in, u).(map[string]any)
	if got["error"] != "boom" || got["usage"] != u {
		t.Errorf("map payload: got %v", got)
	}
	if _, mutated := in["usage"]; mutated {
		t.Error("withTurnUsage mutated the caller's payload map")
	}
	got, _ = withTurnUsage("opaque", u).(map[string]any)
	if got["payload"] != "opaque" || got["usage"] != u {
		t.Errorf("non-map payload: got %v", got)
	}
}

func TestTranslateErrorEmitsTurnFailed(t *testing.T) {
	kind, payload, ok := translateStreamEvent(llmtypes.StreamEvent{
		Type:  llmtypes.EventError,
		Error: "rate limit exceeded",
	})
	if !ok {
		t.Fatal("EventError should be mapped")
	}
	if kind != runtimeevents.KindTurnFailed {
		t.Errorf("kind = %q, want turn.failed", kind)
	}
	p, _ := payload.(map[string]any)
	if got, _ := p["error"].(string); got != "rate limit exceeded" {
		t.Errorf("payload.error = %q, want rate limit exceeded", got)
	}
}

func TestTranslateDoneEmitsTurnCompleted(t *testing.T) {
	kind, _, ok := translateStreamEvent(llmtypes.StreamEvent{Type: llmtypes.EventDone})
	if !ok {
		t.Fatal("EventDone should be mapped")
	}
	if kind != runtimeevents.KindTurnCompleted {
		t.Errorf("kind = %q, want turn.completed", kind)
	}
}

func TestTranslateSessionIDIsSkipped(t *testing.T) {
	// Provider session IDs update Bridge.Process.ProviderSessionID
	// in Run; the translator returns ok=false so no envelope is
	// emitted for them.
	_, _, ok := translateStreamEvent(llmtypes.StreamEvent{
		Type:      llmtypes.EventSessionID,
		SessionID: "claude-session-abc",
	})
	if ok {
		t.Error("EventSessionID should return ok=false (handled out-of-band)")
	}
}

func TestTranslateThinking(t *testing.T) {
	kind, payload, ok := translateStreamEvent(llmtypes.StreamEvent{
		Type:          llmtypes.EventThinking,
		ThinkingBlock: &llmtypes.ThinkingBlock{Thinking: "let me consider"},
	})
	if !ok {
		t.Fatal("EventThinking should be mapped")
	}
	if kind != runtimeevents.KindAgentDelta {
		t.Errorf("kind = %q, want agent.delta", kind)
	}
	p, _ := payload.(map[string]any)
	if _, has := p["thinking"]; !has {
		t.Error("payload missing thinking block")
	}
}

func TestTranslateUnknownReturnsNotMapped(t *testing.T) {
	_, _, ok := translateStreamEvent(llmtypes.StreamEvent{
		Type: llmtypes.EventType("future-event-kind"),
	})
	if ok {
		t.Error("unknown EventType should return ok=false (skip rather than placeholder)")
	}
}

func TestTranslateProviderToolResult(t *testing.T) {
	kind, payload, ok := translateProviderEvent(pevents.ToolResult{
		ID:             "tool_1",
		IsError:        true,
		ContentPreview: "permission denied",
	})
	if !ok {
		t.Fatal("ToolResult should be mapped")
	}
	if kind != runtimeevents.KindAgentToolResult {
		t.Errorf("kind = %q, want agent.tool_result", kind)
	}
	p, _ := payload.(map[string]any)
	tr, _ := p["tool_result"].(map[string]any)
	if got, _ := tr["content_preview"].(string); got != "permission denied" {
		t.Errorf("content_preview = %q", got)
	}
}

func TestTranslateProviderSubagentSpawn(t *testing.T) {
	kind, payload, ok := translateProviderEvent(pevents.SubagentSpawn{
		Tool: "Task",
		Args: map[string]any{"description": "scan repo"},
	})
	if !ok {
		t.Fatal("SubagentSpawn should be mapped")
	}
	if kind != runtimeevents.KindAgentSubagentSpawn {
		t.Errorf("kind = %q, want agent.subagent_spawn", kind)
	}
	p, _ := payload.(map[string]any)
	spawn, _ := p["subagent_spawn"].(map[string]any)
	if got, _ := spawn["tool"].(string); got != "Task" {
		t.Errorf("tool = %q", got)
	}
}

func TestTranslateProviderHeartbeat(t *testing.T) {
	ts := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	kind, payload, ok := translateProviderEvent(pevents.Heartbeat{LastActivityAt: ts})
	if !ok {
		t.Fatal("Heartbeat should be mapped")
	}
	if kind != runtimeevents.KindSessionHeartbeat {
		t.Errorf("kind = %q, want session.heartbeat", kind)
	}
	p, _ := payload.(map[string]any)
	if got, _ := p["last_activity_at"].(time.Time); !got.Equal(ts) {
		t.Errorf("last_activity_at = %v, want %v", got, ts)
	}
}

// CW-20260930-0137 / CW-20260930-0228 §1: block boundaries and phase reach
// agent.delta so consumers can separate blocks without per-provider rules.
func TestTranslateDeltaCarriesBlockAndPhase(t *testing.T) {
	_, payload, ok := translateStreamEvent(llmtypes.StreamEvent{
		Type: llmtypes.EventDelta, Content: "A", BlockID: "msg_1:0", Phase: llmtypes.PhaseFinal,
	})
	p, _ := payload.(map[string]any)
	if !ok || p["content"] != "A" || p["block_id"] != "msg_1:0" || p["phase"] != "final" {
		t.Fatalf("delta payload = %v, ok=%v", p, ok)
	}
	_, payload, _ = translateStreamEvent(llmtypes.StreamEvent{Type: llmtypes.EventDelta, Content: "B"})
	p, _ = payload.(map[string]any)
	if _, has := p["block_id"]; has {
		t.Errorf("unclassified delta has block_id: %v", p)
	}
	if _, has := p["phase"]; has {
		t.Errorf("unclassified delta has phase: %v", p)
	}
	// Thinking uses the phase the ACP translators already emit.
	_, payload, _ = translateStreamEvent(llmtypes.StreamEvent{
		Type: llmtypes.EventThinking, ThinkingBlock: &llmtypes.ThinkingBlock{Thinking: "hmm"}, BlockID: "msg_1:1",
	})
	p, _ = payload.(map[string]any)
	if p["phase"] != "thought" || p["block_id"] != "msg_1:1" || p["thinking"] == nil {
		t.Errorf("thinking payload = %v", p)
	}
}

func TestTranslateErrorCarriesStopReason(t *testing.T) {
	_, payload, _ := translateStreamEvent(llmtypes.StreamEvent{Type: llmtypes.EventError, Error: "boom"})
	p, _ := payload.(map[string]any)
	if p["stop_reason"] != llmtypes.StopReasonError {
		t.Errorf("turn.failed payload = %v, want stop_reason error", p)
	}
}

func TestTranslateProviderSessionLostAuthFailedAndPermissionDenied(t *testing.T) {
	kind, payload, ok := translateProviderEvent(pevents.SessionLost{RequestedID: "a", ActualID: "b", Reason: "new session"})
	p, _ := payload.(map[string]any)
	if !ok || kind != runtimeevents.KindSessionLost || p["requested_id"] != "a" || p["actual_id"] != "b" || p["reason"] != "new session" {
		t.Errorf("session lost = %q %v %v", kind, p, ok)
	}
	kind, payload, ok = translateProviderEvent(pevents.PermissionDenied{Action: "Bash", DisplayName: "rm -rf x"})
	p, _ = payload.(map[string]any)
	if !ok || kind != runtimeevents.KindAgentPermissionDenied || p["action"] != "Bash" || p["display_name"] != "rm -rf x" {
		t.Errorf("permission denied = %q %v %v", kind, p, ok)
	}
	kind, payload, ok = translateProviderEvent(pevents.AuthFailed{Message: "provider not authenticated"})
	p, _ = payload.(map[string]any)
	if !ok || kind != runtimeevents.KindSessionAuthFailed || p["error"] != "provider not authenticated" {
		t.Errorf("auth failed = %q %v %v", kind, p, ok)
	}
	if !isTurnInternal(runtimeevents.KindAgentPermissionDenied) || !isTurnScoped(runtimeevents.KindSessionLost) || isTurnInternal(runtimeevents.KindSessionLost) {
		t.Error("permission_denied must be turn-internal; session.lost turn-scoped but not turn-opening")
	}
}

// CW-20260930-0222 L1: CostUSD is a per-event delta, so the turn's cost is
// the sum.
func TestMergeTurnUsageSumsCost(t *testing.T) {
	total := mergeTurnUsage(nil, &llmtypes.Usage{OutputTokens: 1, CostUSD: 0.2})
	total = mergeTurnUsage(total, &llmtypes.Usage{OutputTokens: 2, CostUSD: 0.016})
	if total.CostUSD < 0.2159 || total.CostUSD > 0.2161 || total.OutputTokens != 3 {
		t.Fatalf("total = %+v, want CostUSD 0.216 and OutputTokens 3", *total)
	}
}

// CW-20260930-0228 §2: the terminal payload carries a normalised stop_reason,
// and never overrides one the payload already names.
func TestWithStopReason(t *testing.T) {
	got, _ := withStopReason(nil, &llmtypes.Usage{StopReason: "length"}).(map[string]any)
	if got["stop_reason"] != llmtypes.StopReasonMaxTokens {
		t.Errorf("payload = %v, want stop_reason max_tokens", got)
	}
	failed := map[string]any{"error": "x", "stop_reason": "error"}
	if got, _ := withStopReason(failed, &llmtypes.Usage{StopReason: "end_turn"}).(map[string]any); got["stop_reason"] != "error" {
		t.Errorf("payload = %v, want the existing stop_reason kept", got)
	}
	if got := withStopReason(nil, &llmtypes.Usage{}); got != nil {
		t.Errorf("no reported reason added one: %v", got)
	}
	if got := withStopReason(nil, nil); got != nil {
		t.Errorf("nil usage added a stop_reason: %v", got)
	}
}
