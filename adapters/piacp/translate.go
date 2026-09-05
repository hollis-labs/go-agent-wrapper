package piacp

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
	// directly against the live pi-acp 0.0.33 bridge (see package doc),
	// consistent with every other ACP implementation this repo has
	// verified.
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
				"phase":   "message",
			}),
		})

	case "agent_thought_chunk":
		// Never observed live against a real pi-acp session — see the
		// package doc's "Real wire behavior" section: pi-acp's own
		// README documents "no separate thought stream" as a current
		// limitation. Decoded defensively using the same content.text
		// shape ACP's spec uses (and copilotacp independently verified
		// live for a different ACP agent), in case a future pi-acp
		// release adds it — dead code today, kept for forward
		// compatibility rather than silently dropping the update kind.
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
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
			Status     string `json:"status"`
			Locations  []struct {
				Path string `json:"path"`
			} `json:"locations"`
			RawInput json.RawMessage `json:"rawInput"`
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
		if len(v.Locations) > 0 {
			paths := make([]string, len(v.Locations))
			for i, loc := range v.Locations {
				paths[i] = loc.Path
			}
			payload["locations"] = paths
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
			Title      string          `json:"title"`
			Content    json.RawMessage `json:"content"`
			Meta       struct {
				TerminalOutput *struct {
					TerminalID string `json:"terminal_id"`
					Data       string `json:"data"`
				} `json:"terminal_output"`
				TerminalExit *struct {
					TerminalID string  `json:"terminal_id"`
					ExitCode   *int    `json:"exit_code"`
					Signal     *string `json:"signal"`
				} `json:"terminal_exit"`
			} `json:"_meta"`
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
		if len(v.Content) > 0 {
			var content any
			if err := json.Unmarshal(v.Content, &content); err == nil {
				payload["result"] = content
			}
		}
		// pi-acp-specific: real, incremental terminal output/exit-code
		// metadata for `execute`-kind (bash) tool calls — confirmed live
		// (see package doc). Surfaced when present rather than dropped,
		// since it's genuine turn activity with no other home in the
		// generic tool_call_update shape.
		if v.Meta.TerminalOutput != nil {
			payload["terminal_output"] = v.Meta.TerminalOutput.Data
		}
		if v.Meta.TerminalExit != nil {
			exit := map[string]any{}
			if v.Meta.TerminalExit.ExitCode != nil {
				exit["exit_code"] = *v.Meta.TerminalExit.ExitCode
			}
			if v.Meta.TerminalExit.Signal != nil {
				exit["signal"] = *v.Meta.TerminalExit.Signal
			}
			payload["terminal_exit"] = exit
		}
		c.emit(runtimeevents.Event{
			Kind:    runtimeevents.KindAgentToolResult,
			TurnID:  turnID,
			Payload: mustMarshal(payload),
		})

	default:
		// Informational session/update variants observed live but with
		// no clean runtimeevents home: session_info_update (pi-acp's own
		// queue-depth/running heartbeat), available_commands_update
		// (pi's slash-command catalog), user_message_chunk (observed
		// only as a session/load resume-replay artifact — see package
		// doc). Skipped deliberately rather than forced into an
		// ill-fitting Kind — matches go-providers' own EventParser
		// convention and opencodeacp's/copilotacp's identical precedent.
	}
}

// handleServerRequest answers a server-initiated JSON-RPC request (a
// frame carrying both `method` and `id`) from pi-acp. Per JSON-RPC 2.0,
// every such request requires a response — without one, pi-acp (and,
// transitively, the `pi` process it owns) blocks waiting for it.
//
// `session/request_permission` maps onto
// agent.permission_requested/resolved per 17-acp.md's explicit mapping
// (Nanite repo). A configured best-effort responder selects one exact
// provider-offered option; without one, the Client retains its established
// "cancelled" outcome. Both paths emit a correlated request/resolved pair.
// The callback is defensive rather than comprehensive because Pi normally
// executes tools locally without issuing this request.
//
// Other server-initiated methods this Client's declared
// clientCapabilities (fs: false, terminal: false — see Launch) tell
// pi-acp not to expect (`fs/read_text_file`, `fs/write_text_file`,
// `terminal/*`) get a generic "not supported" JSON-RPC error if pi-acp
// sends one anyway. Unlike opencodeacp's equivalent finding (empirically
// confirmed for exactly one tool-call shape), pi-acp's own README
// documents this as a PERMANENT design choice — "No ACP filesystem
// delegation (fs/*) and no ACP terminal delegation (terminal/*). pi
// reads/writes and executes locally." — not merely an untested unknown;
// this handler still exists defensively in case a future pi-acp release
// changes that.
func (c *Client) handleServerRequest(frame rpcFrame) {
	if frame.Method != "session/request_permission" {
		c.respondToServerRequest(frame.ID, nil, &rpcError{
			Code:    -32601,
			Message: "piacp: no handler configured for server-initiated method " + frame.Method,
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
				"reason":     "piacp: no approval handler configured",
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
