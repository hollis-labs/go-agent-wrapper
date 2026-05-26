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

func TestTranslateUsageEmitsTurnCompleted(t *testing.T) {
	kind, payload, ok := translateStreamEvent(llmtypes.StreamEvent{
		Type:  llmtypes.EventUsage,
		Usage: &llmtypes.Usage{InputTokens: 100, OutputTokens: 50},
	})
	if !ok {
		t.Fatal("EventUsage should be mapped")
	}
	if kind != runtimeevents.KindTurnCompleted {
		t.Errorf("kind = %q, want turn.completed", kind)
	}
	p, _ := payload.(map[string]any)
	if _, has := p["usage"]; !has {
		t.Error("payload missing usage")
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
