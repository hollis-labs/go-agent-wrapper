package codexacp

import (
	"encoding/json"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// handleNotification routes one inbound ACP notification (a JSON-RPC
// frame carrying `method` but no `id`) to the matching
// [runtimeevents.Event]. Mapping follows
// docs/engineering/architecture/17-acp.md's explicit correspondence
// (Nanite repo): "message/thought chunks → agent.delta,
// tool_call/tool_call_update → agent.tool_use/agent.tool_result,
// permission requests → agent.permission_requested/resolved" — the last
// of those is a server-initiated REQUEST, not a notification, and is
// handled by the shared [acp.NDJSONBridgeClient]'s server-request handler instead.
//
// `session/cancel` is the only other notification method ACP defines,
// and it flows client→agent (this Client sends it; it never receives
// one) — so the only inbound notification method observed or expected
// here is `session/update`.
func handleNotification(c *acp.NDJSONBridgeClient, method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}

	var envelope struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(params, &envelope); err != nil {
		return
	}

	// The discriminator field is `update.sessionUpdate` — confirmed
	// directly against the live codex-acp 1.6.2 bridge (see package
	// doc), the same convention opencodeacp's own bridge/native
	// verification found.
	var kindProbe struct {
		SessionUpdate string `json:"sessionUpdate"`
	}
	if err := json.Unmarshal(envelope.Update, &kindProbe); err != nil {
		return
	}
	turnID := c.CurrentTurnID()

	switch kindProbe.SessionUpdate {
	case "agent_message_chunk":
		var v struct {
			MessageID string `json:"messageId"`
			Content   struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(envelope.Update, &v); err != nil {
			return
		}
		c.Emit(runtimeevents.Event{
			Kind:   runtimeevents.KindAgentDelta,
			TurnID: turnID,
			Payload: mustMarshal(acp.WithBlockID(map[string]any{
				"content": v.Content.Text,
				"phase":   "message",
			}, v.MessageID)),
		})

	case "agent_thought_chunk":
		// Shares zContentChunk's `content.type`/`content.text` shape
		// with agent_message_chunk above — source-verified against
		// codex-acp 1.6.2's own zSessionUpdate union (dist/index.js:
		// both agent_message_chunk and agent_thought_chunk are
		// `zContentChunk.and({sessionUpdate: ...})`), NOT a bare
		// `thought` string field, and confirmed live during this
		// package's own tool-invoking-turn verification (see package
		// doc) — a real reasoning-summary chunk
		// ("**Checking for boot-prompt file**") decoded correctly via
		// this exact shape.
		var v struct {
			MessageID string `json:"messageId"`
			Content   struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(envelope.Update, &v); err != nil {
			return
		}
		c.Emit(runtimeevents.Event{
			Kind:   runtimeevents.KindAgentDelta,
			TurnID: turnID,
			Payload: mustMarshal(acp.WithBlockID(map[string]any{
				"content": v.Content.Text,
				"phase":   "thought",
			}, v.MessageID)),
		})

	case "tool_call":
		var v struct {
			ToolCallID string          `json:"toolCallId"`
			Title      string          `json:"title"`
			Kind       string          `json:"kind"`
			Status     string          `json:"status"`
			RawInput   json.RawMessage `json:"rawInput"`
		}
		if err := json.Unmarshal(envelope.Update, &v); err != nil {
			return
		}
		payload := map[string]any{
			"tool_call_id": v.ToolCallID,
			"title":        v.Title,
			"kind":         v.Kind,
			"status":       v.Status,
		}
		if len(v.RawInput) > 0 {
			var rawInput any
			if err := json.Unmarshal(v.RawInput, &rawInput); err == nil {
				payload["raw_input"] = rawInput
			}
		}
		c.Emit(runtimeevents.Event{
			Kind:    runtimeevents.KindAgentToolUse,
			TurnID:  turnID,
			Payload: mustMarshal(payload),
		})

	case "tool_call_update":
		// codex-acp's ToolCallUpdate shape (source-verified against its
		// own zod schema, dist/index.js) uses `rawInput`/`rawOutput` —
		// NOT opencode's bridge's `result`/`isError` field names. This
		// is a real, per-bridge wire divergence, documented in the
		// package doc rather than silently papered over.
		var v struct {
			ToolCallID string          `json:"toolCallId"`
			Status     string          `json:"status"`
			Kind       string          `json:"kind"`
			Title      string          `json:"title"`
			RawInput   json.RawMessage `json:"rawInput"`
			RawOutput  json.RawMessage `json:"rawOutput"`
		}
		if err := json.Unmarshal(envelope.Update, &v); err != nil {
			return
		}
		payload := map[string]any{
			"tool_call_id": v.ToolCallID,
			"status":       v.Status,
		}
		if v.Title != "" {
			payload["title"] = v.Title
		}
		if len(v.RawOutput) > 0 {
			var rawOutput any
			if err := json.Unmarshal(v.RawOutput, &rawOutput); err == nil {
				payload["result"] = rawOutput
			}
		}
		c.Emit(runtimeevents.Event{
			Kind:    runtimeevents.KindAgentToolResult,
			TurnID:  turnID,
			Payload: mustMarshal(payload),
		})

	default:
		// Informational session/update variants observed live but with
		// no clean runtimeevents home: available_commands_update,
		// usage_update, session_info_update. Skipped deliberately rather
		// than forced into an ill-fitting Kind — matches
		// go-providers' own EventParser convention ("returning a nil
		// slice with nil error is permissible... line was informational
		// and produced no event"), same treatment opencodeacp gives its
		// own informational variants.
	}
}
