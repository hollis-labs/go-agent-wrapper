package claudeacp

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
// handled by [Client.handleServerRequest] instead.
//
// `session/cancel` is the only other notification method ACP defines,
// and it flows client→agent (this Client sends it; it never receives
// one) — so the only inbound notification method observed or expected
// here is `session/update`.
func (c *Client) handleNotification(method string, params json.RawMessage) {
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

	var kindProbe struct {
		SessionUpdate string `json:"sessionUpdate"`
	}
	if err := json.Unmarshal(envelope.Update, &kindProbe); err != nil {
		return
	}
	c.turnMu.Lock()
	turnID := c.currentTurnID
	c.turnMu.Unlock()

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
		c.emit(runtimeevents.Event{
			Kind:   runtimeevents.KindAgentDelta,
			TurnID: turnID,
			Payload: mustMarshal(map[string]any{
				"content": v.Content.Text,
				"phase":   "message",
			}),
		})

	case "agent_thought_chunk":
		// DIVERGES from [adapters/opencodeacp]'s own verified shape:
		// claude-agent-acp nests the thought text under `content.text`
		// (the SAME shape agent_message_chunk uses), not a flat
		// top-level `thought` string field. Source-verified against the
		// bridge's real dist/acp-agent.js (thinking-delta branch) — see
		// package doc for the full citation. Decoding the flat `thought`
		// field here (the other provider's shape) would silently produce
		// an always-empty phase:"thought" delta for every real Claude
		// thinking chunk — exactly the kind of silent
		// reasoning-content-loss task 11's own review caught in the
		// native ACP session backend; this decode is deliberately shaped
		// to avoid repeating it.
		var v struct {
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(envelope.Update, &v); err != nil {
			return
		}
		c.emit(runtimeevents.Event{
			Kind:   runtimeevents.KindAgentDelta,
			TurnID: turnID,
			Payload: mustMarshal(map[string]any{
				"content": v.Content.Text,
				"phase":   "thought",
			}),
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
		c.emit(runtimeevents.Event{
			Kind:    runtimeevents.KindAgentToolUse,
			TurnID:  turnID,
			Payload: mustMarshal(payload),
		})

	case "tool_call_update":
		// DIVERGES from [adapters/opencodeacp]'s own verified shape in
		// two ways — see package doc for the full live-verified citation
		// (a real Bash tool call produced one tool_call followed by FOUR
		// tool_call_update frames, only the last carrying status/output):
		//
		//  1. The final output field is `rawOutput`, not OpenCode's
		//     `result`.
		//  2. There is no boolean `isError` field at all — error state is
		//     folded entirely into `status: "failed"` vs `"completed"`.
		//     Intermediate refinement frames carry neither field.
		//
		// Every tool_call_update frame is still forwarded as its own
		// KindAgentToolResult event, matching [adapters/opencodeacp]'s
		// unconditional per-frame policy and 17-acp.md's plain
		// "tool_call_update → agent.tool_result" mapping — callers should
		// expect more than one agent.tool_result event per real Claude
		// tool call, most carrying only a partial payload.
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
		}
		if v.Status != "" {
			payload["status"] = v.Status
			payload["is_error"] = v.Status == "failed"
		}
		if v.Title != "" {
			payload["title"] = v.Title
		}
		if len(v.RawOutput) > 0 {
			var result any
			if err := json.Unmarshal(v.RawOutput, &result); err == nil {
				payload["result"] = result
			}
		}
		c.emit(runtimeevents.Event{
			Kind:    runtimeevents.KindAgentToolResult,
			TurnID:  turnID,
			Payload: mustMarshal(payload),
		})

	default:
		// Informational session/update variants observed live but with
		// no clean runtimeevents home: available_commands_update,
		// usage_update, session_info_update, current_mode_update,
		// config_option_update, plan. Skipped deliberately rather than
		// forced into an ill-fitting Kind — matches
		// [adapters/opencodeacp]'s own convention and go-providers'
		// EventParser convention ("returning a nil slice with nil error
		// is permissible... line was informational and produced no
		// event").
	}
}

// handleServerRequest answers a server-initiated JSON-RPC request (a
// frame carrying both `method` and `id`) from the bridge. Per JSON-RPC
// 2.0, every such request requires a response — without one, the bridge
// blocks waiting for it.
//
// `session/request_permission` maps onto
// agent.permission_requested/resolved per 17-acp.md's explicit mapping
// (Nanite repo). A configured best-effort responder selects one exact
// provider-offered option; without one, the Client retains its established
// "cancelled" outcome. Both paths emit a correlated request/resolved pair.
// Same generic handling [adapters/opencodeacp.Client.handleServerRequest]
// uses — the
// wire shape (`{options, sessionId, toolCall: {toolCallId, rawInput,
// ...}}`) is structurally identical, confirmed by direct inspection of
// the bridge's real dist/acp-agent.js requestPermissionFromClient call
// sites.
//
// Live-verified that Claude does NOT invoke this method (nor the
// declared-false `fs`/`terminal` client capabilities) for at least one
// real tool-call shape (a plain Bash command) — it executes its own
// tools internally regardless of what the client declares, consistent
// with 17-acp.md's documented expectation, though not exhaustively
// confirmed for every ACP-proxyable operation.
func (c *Client) handleServerRequest(frame rpcFrame) {
	if frame.Method != "session/request_permission" {
		_ = c.respondToServerRequest(frame.ID, nil, &rpcError{
			Code:    -32601,
			Message: "claudeacp: no handler configured for server-initiated method " + frame.Method,
		})
		return
	}

	// Correlate the requested/resolved pair via ACP's OWN JSON-RPC
	// request id (a stable, protocol-native correlator) rather than a
	// synthesized runtimeevents ID/ParentID pair: [acp.Client.Events]'s
	// documented contract leaves Event.ID zero for the caller's own
	// Emitter/activity.Bridge to assign, so this Client must not invent
	// one just to self-correlate two of its own events.
	acpRequestID := append(json.RawMessage(nil), frame.ID...)
	requestID := append(json.RawMessage(nil), frame.ID...)
	configured := c.permissions.Configured()
	var turnID string
	c.permissions.DispatchTurnRequest(frame.Params, func(admission acp.PermissionDispatchAdmission) {
		if !admission.ActiveTurn {
			return
		}
		c.turnMu.Lock()
		turnID = c.currentTurnID
		c.turnMu.Unlock()
		c.emit(runtimeevents.Event{
			Kind:   runtimeevents.KindAgentPermissionRequested,
			TurnID: turnID,
			Payload: mustMarshal(map[string]any{
				"request_id": acpRequestID,
				"method":     frame.Method,
				"params":     frame.Params,
			}),
		})
	}, func(admission acp.PermissionDispatchAdmission, resolution acp.PermissionResolution) error {
		err := c.respondToServerRequest(requestID, resolution.Result(), nil)
		if !admission.ActiveTurn {
			return err
		}
		payload := resolution.ResolvedEventPayload(acpRequestID)
		if !configured {
			payload = map[string]any{
				"request_id": acpRequestID,
				"allowed":    false,
				"reason":     "claudeacp: no approval handler configured",
			}
		}
		if err != nil {
			payload = resolution.DeliveryFailureEventPayload(acpRequestID)
		}
		c.emit(runtimeevents.Event{
			Kind:    runtimeevents.KindAgentPermissionResolved,
			TurnID:  turnID,
			Payload: mustMarshal(payload),
		})
		return err
	}, func(resolution acp.PermissionResolution) {
		if resolution.ResponseError() != nil {
			c.abortPermissionTransport()
		}
		if diagnostic, ok := resolution.Diagnostic(); ok {
			c.reportDiagnostic(diagnostic)
		}
	})
}
