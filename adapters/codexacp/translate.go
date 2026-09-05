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
	// directly against the live codex-acp 1.6.2 bridge (see package
	// doc), the same convention opencodeacp's own bridge/native
	// verification found.
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
		c.emit(runtimeevents.Event{
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

// handleServerRequest answers a server-initiated JSON-RPC request (a
// frame carrying both `method` and `id`) from the bridge. Per JSON-RPC
// 2.0, every such request requires a response — without one, the bridge
// blocks waiting for it.
//
// `session/request_permission` maps onto
// agent.permission_requested/resolved per 17-acp.md's explicit mapping
// (Nanite repo). A configured best-effort responder selects one exact
// provider-offered option; without one, the Client retains its established
// "cancelled" outcome (source-verified against codex-acp's own
// zRequestPermissionOutcome schema). Both paths emit a correlated
// request/resolved pair. The callback is not a general execution gate because
// Codex may execute operations without issuing this request.
//
// This method was never observed live for a plain shell tool call during
// this package's own verification (see package doc) — Codex executed it
// directly, consistent with 17-acp.md's documented expectation that
// Codex/Claude do their own fs/terminal work regardless of declared
// client capabilities — but the handler is wired for correctness
// regardless, matching opencodeacp's own treatment of the same method.
//
// Other server-initiated methods this Client's declared
// clientCapabilities (fs: false, terminal: false — see Launch) tell the
// bridge not to expect (`fs/read_text_file`, `fs/write_text_file`,
// `terminal/*`) get a generic "not supported" JSON-RPC error if the
// bridge sends one anyway.
func (c *Client) handleServerRequest(frame rpcFrame) {
	if frame.Method != "session/request_permission" {
		c.respondToServerRequest(frame.ID, nil, &rpcError{
			Code:    -32601,
			Message: "codexacp: no handler configured for server-initiated method " + frame.Method,
		})
		return
	}

	c.turnMu.Lock()
	turnID := c.currentTurnID
	c.turnMu.Unlock()

	// Correlate the requested/resolved pair via ACP's OWN JSON-RPC
	// request id (a stable, protocol-native correlator) rather than a
	// synthesized runtimeevents ID/ParentID pair — same convention
	// opencodeacp uses, per [acp.Client.Events]'s documented contract
	// that Event.ID is left zero for the caller's own
	// Emitter/activity.Bridge to assign.
	acpRequestID := json.RawMessage(frame.ID)
	c.emit(runtimeevents.Event{
		Kind:   runtimeevents.KindAgentPermissionRequested,
		TurnID: turnID,
		Payload: mustMarshal(map[string]any{
			"request_id": acpRequestID,
			"method":     frame.Method,
			"params":     frame.Params,
		}),
	})
	if !c.permissions.Configured() {
		if err := c.respondToServerRequest(frame.ID, map[string]any{
			"outcome": map[string]any{"outcome": "cancelled"},
		}, nil); err != nil {
			c.emit(runtimeevents.Event{
				Kind:    runtimeevents.KindAgentPermissionResolved,
				TurnID:  turnID,
				Payload: mustMarshal(acp.PermissionResolution{}.DeliveryFailureEventPayload(acpRequestID)),
			})
			c.reportDiagnostic(acp.NewDiagnostic(acp.DiagnosticProtocol, "ACP permission response delivery failed; transport closed", ""))
			c.abortPermissionTransport()
			return
		}
		c.emit(runtimeevents.Event{
			Kind:   runtimeevents.KindAgentPermissionResolved,
			TurnID: turnID,
			Payload: mustMarshal(map[string]any{
				"request_id": acpRequestID,
				"allowed":    false,
				"reason":     "codexacp: no approval handler configured",
			}),
		})
		return
	}

	requestID := append(json.RawMessage(nil), frame.ID...)
	c.permissions.Dispatch(frame.Params, func(resolution acp.PermissionResolution) error {
		err := c.respondToServerRequest(requestID, resolution.Result(), nil)
		payload := resolution.ResolvedEventPayload(acpRequestID)
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
