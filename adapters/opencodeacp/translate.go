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
		var v struct {
			Thought string `json:"thought"`
		}
		if err := json.Unmarshal(envelope.Update, &v); err != nil {
			return
		}
		c.emit(runtimeevents.Event{
			Kind:   runtimeevents.KindAgentDelta,
			TurnID: turnID,
			Payload: mustMarshal(map[string]any{
				"content": v.Thought,
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
		c.emit(runtimeevents.Event{
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

// handleServerRequest answers a server-initiated JSON-RPC request (a
// frame carrying both `method` and `id`) from opencode. Per JSON-RPC
// 2.0, every such request requires a response — without one, opencode
// blocks waiting for it.
//
// `session/request_permission` maps onto
// agent.permission_requested/resolved per 17-acp.md's explicit mapping
// (Nanite repo). A configured best-effort responder selects one exact
// provider-offered option; without one, the Client retains its established
// "cancelled" outcome. Both paths emit a correlated request/resolved pair.
// The callback is not a general execution gate because OpenCode may execute
// operations without issuing this request.
//
// Other server-initiated methods this Client's declared
// clientCapabilities (fs: false, terminal: false — see Launch) tell
// opencode not to expect (`fs/read_text_file`, `fs/write_text_file`,
// `terminal/*`) get a generic "not supported" JSON-RPC error if opencode
// sends one anyway. Live verification (package doc / Work Log) never
// observed opencode invoke any of these for a plain shell tool call —
// consistent with it doing its own tool execution internally regardless
// of declared client capabilities, the same behavior 17-acp.md documents
// for Claude/Codex — but this is empirically confirmed for exactly one
// tool-call shape, not exhaustively, and is flagged as such rather than
// assumed universal.
func (c *Client) handleServerRequest(frame rpcFrame) {
	if frame.Method != "session/request_permission" {
		_ = c.respondToServerRequest(frame.ID, nil, &rpcError{
			Code:    -32601,
			Message: "opencodeacp: no handler configured for server-initiated method " + frame.Method,
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
				"reason":     "opencodeacp: no approval handler configured",
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
