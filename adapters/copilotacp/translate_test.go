package copilotacp

import (
	"encoding/json"
	"testing"

	llmtypes "github.com/hollis-labs/go-llm-types"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// The raw JSON literals below are copied verbatim from real
// `session/update` notifications captured against the live
// `copilot --acp` binary (1.0.12) during this task's implementation —
// not hand-invented shapes. See the package doc.
const (
	realAgentMessageChunk = `{"sessionId":"2913b08c-c27c-427c-be98-aa0f1cd79612","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"PONG"}}}`
	realAgentThoughtChunk = `{"sessionId":"2d62d6cb-e407-472f-b436-1a887b509563","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":" Let me write a comprehensive, detailed essay."}}}`
)

func rawUpdateField(t *testing.T, envelope string) json.RawMessage {
	t.Helper()
	var su sessionUpdateParams
	if err := json.Unmarshal([]byte(envelope), &su); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	return su.Update
}

func TestParseSessionUpdate_RealAgentMessageChunk(t *testing.T) {
	upd, ok := parseSessionUpdate(rawUpdateField(t, realAgentMessageChunk))
	if !ok {
		t.Fatal("parseSessionUpdate returned ok=false for a real agent_message_chunk frame")
	}
	if upd.kind != acpUpdateDelta {
		t.Errorf("kind = %v, want acpUpdateDelta", upd.kind)
	}
	if upd.text != "PONG" {
		t.Errorf("text = %q, want %q", upd.text, "PONG")
	}
}

func TestParseSessionUpdate_RealAgentThoughtChunk(t *testing.T) {
	upd, ok := parseSessionUpdate(rawUpdateField(t, realAgentThoughtChunk))
	if !ok {
		t.Fatal("parseSessionUpdate returned ok=false for a real agent_thought_chunk frame")
	}
	if upd.kind != acpUpdateThinking {
		t.Errorf("kind = %v, want acpUpdateThinking", upd.kind)
	}
	if upd.text != " Let me write a comprehensive, detailed essay." {
		t.Errorf("text = %q", upd.text)
	}
}

func TestParseSessionUpdate_ToolCall(t *testing.T) {
	raw := json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"tc_1","title":"Read file","kind":"read","status":"pending"}`)
	upd, ok := parseSessionUpdate(raw)
	if !ok {
		t.Fatal("parseSessionUpdate returned ok=false for tool_call")
	}
	if upd.kind != acpUpdateToolUse || upd.toolID != "tc_1" || upd.toolName != "Read file" || upd.toolStatus != "pending" {
		t.Errorf("unexpected parse result: %+v", upd)
	}
}

func TestParseSessionUpdate_ToolCallUpdate(t *testing.T) {
	raw := json.RawMessage(`{"sessionUpdate":"tool_call_update","toolCallId":"tc_1","status":"completed","content":[{"type":"text","text":"ok"}]}`)
	upd, ok := parseSessionUpdate(raw)
	if !ok {
		t.Fatal("parseSessionUpdate returned ok=false for tool_call_update")
	}
	if upd.kind != acpUpdateToolResult || upd.toolID != "tc_1" || upd.toolStatus != "completed" || upd.text != "ok" {
		t.Errorf("unexpected parse result: %+v", upd)
	}
}

func TestParseSessionUpdate_UnknownKindSkipped(t *testing.T) {
	for _, kind := range []string{"plan", "available_commands_update", "usage_update", "something_future"} {
		raw := json.RawMessage(`{"sessionUpdate":"` + kind + `"}`)
		if _, ok := parseSessionUpdate(raw); ok {
			t.Errorf("parseSessionUpdate(%q) = ok=true, want false (unmapped, deliberate skip)", kind)
		}
	}
}

func TestParseSessionUpdate_MalformedJSON(t *testing.T) {
	if _, ok := parseSessionUpdate(json.RawMessage(`not json`)); ok {
		t.Error("parseSessionUpdate on malformed JSON = ok=true, want false")
	}
	if _, ok := parseSessionUpdate(nil); ok {
		t.Error("parseSessionUpdate(nil) = ok=true, want false")
	}
}

func TestAcpUpdateToRuntimeEvent(t *testing.T) {
	upd, ok := parseSessionUpdate(rawUpdateField(t, realAgentMessageChunk))
	if !ok {
		t.Fatal("setup: parseSessionUpdate failed")
	}
	ev, ok := acpUpdateToRuntimeEvent(upd, "turn_abc")
	if !ok {
		t.Fatal("acpUpdateToRuntimeEvent returned ok=false")
	}
	if ev.Kind != runtimeevents.KindAgentDelta {
		t.Errorf("Kind = %q, want agent.delta", ev.Kind)
	}
	if ev.TurnID != "turn_abc" {
		t.Errorf("TurnID = %q, want turn_abc", ev.TurnID)
	}
	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["content"] != "PONG" {
		t.Errorf("payload[content] = %v, want PONG", payload["content"])
	}
}

func TestAcpUpdateToRuntimeEvent_EmptyTurnIDOmitted(t *testing.T) {
	upd, ok := parseSessionUpdate(rawUpdateField(t, realAgentMessageChunk))
	if !ok {
		t.Fatal("setup: parseSessionUpdate failed")
	}
	ev, ok := acpUpdateToRuntimeEvent(upd, "")
	if !ok {
		t.Fatal("acpUpdateToRuntimeEvent returned ok=false")
	}
	if ev.TurnID != "" {
		t.Errorf("TurnID = %q, want empty", ev.TurnID)
	}
}

func TestAcpUpdateToStreamEvent(t *testing.T) {
	upd, ok := parseSessionUpdate(rawUpdateField(t, realAgentMessageChunk))
	if !ok {
		t.Fatal("setup: parseSessionUpdate failed")
	}
	se, ok := acpUpdateToStreamEvent(upd)
	if !ok {
		t.Fatal("acpUpdateToStreamEvent returned ok=false")
	}
	if se.Type != llmtypes.EventDelta || se.Content != "PONG" {
		t.Errorf("unexpected StreamEvent: %+v", se)
	}
}

func TestAcpUpdateToStreamEvent_ToolResultSkipped(t *testing.T) {
	raw := json.RawMessage(`{"sessionUpdate":"tool_call_update","toolCallId":"tc_1","status":"completed"}`)
	upd, ok := parseSessionUpdate(raw)
	if !ok {
		t.Fatal("setup: parseSessionUpdate failed")
	}
	if _, ok := acpUpdateToStreamEvent(upd); ok {
		t.Error("acpUpdateToStreamEvent(tool_result) = ok=true, want false (no StreamEvent analog, deliberate skip)")
	}
}
