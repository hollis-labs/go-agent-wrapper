package copilotacp

import (
	"encoding/json"

	llmtypes "github.com/hollis-labs/go-llm-types"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// acpUpdate is the normalized, output-agnostic result of parsing one
// `session/update` notification's `update` payload. Parsed once by
// parseSessionUpdate and mapped to two different output shapes below —
// [runtimeevents.Event] for [Client]'s own Events() channel, and
// [llmtypes.StreamEvent] for the [cliAdapterGlue].ParseLine composition
// — so the ACP-specific parsing logic (session/update's `sessionUpdate`
// discriminator and its per-kind field shapes) is written once rather
// than duplicated per output type.
type acpUpdate struct {
	kind       acpUpdateKind
	text       string
	toolID     string
	toolName   string
	toolKind   string
	toolStatus string
}

type acpUpdateKind int

const (
	acpUpdateNone acpUpdateKind = iota
	acpUpdateDelta
	acpUpdateThinking
	acpUpdateToolUse
	acpUpdateToolResult
)

// parseSessionUpdate parses a `session/update` notification's `update`
// field. Per-kind field shapes are per
// agentclientprotocol.com/protocol/prompt-turn and confirmed against
// real captured traffic from `copilot --acp` for agent_message_chunk
// and agent_thought_chunk specifically (tool_call/tool_call_update
// shapes are per the spec; not independently reproduced against a real
// tool-using turn during this task — see the package doc's "no
// fs/terminal proxying" note for why a tool-using turn wasn't exercised
// live).
//
// `plan`, `available_commands_update`, and `usage_update` are
// recognized-but-unmapped: they have no current runtimeevents/StreamEvent
// analog, so they're skipped (ok=false) rather than forced into a
// misleading kind — the same "unknown -> skip" precedent
// wrapper/event_translator.go already establishes for other adapters.
func parseSessionUpdate(raw json.RawMessage) (acpUpdate, bool) {
	if len(raw) == 0 {
		return acpUpdate{}, false
	}
	var disc sessionUpdateDiscriminator
	if err := json.Unmarshal(raw, &disc); err != nil {
		return acpUpdate{}, false
	}

	switch disc.SessionUpdate {
	case "agent_message_chunk":
		var u agentMessageChunkUpdate
		if err := json.Unmarshal(raw, &u); err != nil || u.Content.Text == "" {
			return acpUpdate{}, false
		}
		return acpUpdate{kind: acpUpdateDelta, text: u.Content.Text}, true

	case "agent_thought_chunk":
		var u agentThoughtChunkUpdate
		if err := json.Unmarshal(raw, &u); err != nil || u.Content.Text == "" {
			return acpUpdate{}, false
		}
		return acpUpdate{kind: acpUpdateThinking, text: u.Content.Text}, true

	case "tool_call":
		var u toolCallUpdate
		if err := json.Unmarshal(raw, &u); err != nil {
			return acpUpdate{}, false
		}
		return acpUpdate{
			kind:       acpUpdateToolUse,
			toolID:     u.ToolCallID,
			toolName:   u.Title,
			toolKind:   u.Kind,
			toolStatus: u.Status,
		}, true

	case "tool_call_update":
		var u toolCallStatusUpdate
		if err := json.Unmarshal(raw, &u); err != nil {
			return acpUpdate{}, false
		}
		text := ""
		for _, c := range u.Content {
			text += c.Text
		}
		return acpUpdate{
			kind:       acpUpdateToolResult,
			toolID:     u.ToolCallID,
			toolStatus: u.Status,
			text:       text,
		}, true

	default:
		// plan / available_commands_update / usage_update / anything
		// future ACP adds — deliberate no-op, see doc comment above.
		return acpUpdate{}, false
	}
}

// acpUpdateToRuntimeEvent maps a parsed update to a
// [runtimeevents.Event] for [Client.Events]. turnID is attached when
// non-empty — [acp.Client]'s own doc requires implementations to
// populate TurnID for turn-scoped events themselves.
func acpUpdateToRuntimeEvent(u acpUpdate, turnID string) (runtimeevents.Event, bool) {
	var kind runtimeevents.EventKind
	var payload map[string]any

	switch u.kind {
	case acpUpdateDelta:
		kind = runtimeevents.KindAgentDelta
		payload = map[string]any{"content": u.text}
	case acpUpdateThinking:
		kind = runtimeevents.KindAgentDelta
		payload = map[string]any{"content": u.text, "thinking": true}
	case acpUpdateToolUse:
		kind = runtimeevents.KindAgentToolUse
		payload = map[string]any{"tool_use": map[string]any{
			"id":     u.toolID,
			"name":   u.toolName,
			"kind":   u.toolKind,
			"status": u.toolStatus,
		}}
	case acpUpdateToolResult:
		kind = runtimeevents.KindAgentToolResult
		payload = map[string]any{"tool_result": map[string]any{
			"id":              u.toolID,
			"status":          u.toolStatus,
			"content_preview": u.text,
		}}
	default:
		return runtimeevents.Event{}, false
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return runtimeevents.Event{}, false
	}
	ev := runtimeevents.Event{Kind: kind, Payload: raw}
	if turnID != "" {
		ev.TurnID = turnID
	}
	return ev, true
}

// acpUpdateToStreamEvent maps a parsed update to a legacy
// [llmtypes.StreamEvent], for [cliAdapterGlue.ParseLine]'s composition
// (see package doc's "Wrapper.Run composition" limitation).
func acpUpdateToStreamEvent(u acpUpdate) (llmtypes.StreamEvent, bool) {
	switch u.kind {
	case acpUpdateDelta:
		return llmtypes.StreamEvent{Type: llmtypes.EventDelta, Content: u.text}, true
	case acpUpdateThinking:
		return llmtypes.StreamEvent{
			Type:          llmtypes.EventThinking,
			ThinkingBlock: &llmtypes.ThinkingBlock{Thinking: u.text},
		}, true
	case acpUpdateToolUse:
		return llmtypes.StreamEvent{
			Type:    llmtypes.EventToolUse,
			ToolUse: &llmtypes.ToolUseBlock{ID: u.toolID, Name: u.toolName},
		}, true
	default:
		// tool_call_update has no llmtypes.StreamEvent analog richer
		// than what tool_call (EventToolUse) already carries — skip
		// rather than emit a misleading second ToolUse for the same
		// call.
		return llmtypes.StreamEvent{}, false
	}
}
