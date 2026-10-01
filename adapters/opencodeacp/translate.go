package opencodeacp

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
	// directly against the live opencode 1.15.6 binary (see package
	// doc). A third-party spec summary suggested `type`; the real wire
	// behavior does not use that name, so this reads `sessionUpdate`.
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
		var v struct {
			MessageID string `json:"messageId"`
			Thought   string `json:"thought"`
		}
		if err := json.Unmarshal(envelope.Update, &v); err != nil {
			return
		}
		c.Emit(runtimeevents.Event{
			Kind:   runtimeevents.KindAgentDelta,
			TurnID: turnID,
			Payload: mustMarshal(acp.WithBlockID(map[string]any{
				"content": v.Thought,
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
		var v struct {
			ToolCallID string          `json:"toolCallId"`
			Status     string          `json:"status"`
			Kind       string          `json:"kind"`
			Title      string          `json:"title"`
			RawInput   json.RawMessage `json:"rawInput"`
			Result     json.RawMessage `json:"result"`
			IsError    *bool           `json:"isError"`
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
		if v.IsError != nil {
			payload["is_error"] = *v.IsError
		}
		if len(v.Result) > 0 {
			var result any
			if err := json.Unmarshal(v.Result, &result); err == nil {
				payload["result"] = result
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
		// usage_update, current_mode_update, plan. Skipped deliberately
		// rather than forced into an ill-fitting Kind — matches
		// go-providers' own EventParser convention ("returning a nil
		// slice with nil error is permissible... line was informational
		// and produced no event").
	}
}
