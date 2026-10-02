package wrapper

import (
	"time"

	llmtypes "github.com/hollis-labs/go-llm-types"
	pevents "github.com/hollis-labs/go-providers/provider/events"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// translateStreamEvent converts one [llmtypes.StreamEvent] from the
// agentkit/agentsessions [agentsessions.StartOptions.EventFanout]
// channel into a [runtimeevents.EventKind] + payload pair.
//
// ok == false means the event has no direct envelope mapping
// (e.g. a session_id update, which the wrapper handles separately by
// rebinding the [activity.Bridge] [runtimeevents.Process], or a usage
// report, which [Wrapper.Run] accumulates onto the turn's terminal
// event because usage is not a turn boundary) — the caller should skip
// emission for that frame rather than emit a placeholder.
//
// payload is encoded as a structured map so consumers can json.Marshal
// it into the [runtimeevents.Event.Payload] slot uniformly.
func translateStreamEvent(ev llmtypes.StreamEvent) (kind runtimeevents.EventKind, payload any, ok bool) {
	switch ev.Type {
	case llmtypes.EventDelta:
		return runtimeevents.KindAgentDelta, withBlock(map[string]any{
			"content": ev.Content,
		}, ev.BlockID, ev.Phase), true

	case llmtypes.EventToolUse:
		p := map[string]any{}
		if ev.ToolUse != nil {
			p["tool_use"] = ev.ToolUse
		}
		return runtimeevents.KindAgentToolUse, p, true

	case llmtypes.EventError:
		return runtimeevents.KindTurnFailed, map[string]any{
			"error":       ev.Error,
			"stop_reason": llmtypes.StopReasonError,
		}, true

	case llmtypes.EventDone:
		// A done's Content is the turn's own final message when the provider
		// reports one on its terminal event (Claude's result.result, agy's
		// result.response, the last step of an opencode run). It rides on
		// turn.completed as text, so a consumer takes it as exact instead of
		// guessing the last block of the turn.
		if ev.Content != "" {
			return runtimeevents.KindTurnCompleted, map[string]any{"text": ev.Content}, true
		}
		return runtimeevents.KindTurnCompleted, nil, true

	case llmtypes.EventSessionID:
		// Provider-side session ID. Handled out-of-band by rebinding
		// the activity Bridge's Process.ProviderSessionID; no direct
		// envelope.
		return "", nil, false

	case llmtypes.EventThinking:
		p := map[string]any{}
		if ev.ThinkingBlock != nil {
			p["thinking"] = ev.ThinkingBlock
		}
		return runtimeevents.KindAgentDelta, withBlock(p, ev.BlockID, llmtypes.PhaseThinking), true

	default:
		// Unknown EventType — skip rather than emit a placeholder so
		// downstream consumers don't see "agent.delta with no
		// meaningful payload" frames for events the wrapper doesn't
		// yet model.
		return "", nil, false
	}
}

// withBlock adds block_id and phase to an agent.delta payload when the
// producer knows them. block_id is stable across one content block and
// changes at the next, so consumers separate blocks without provider rules.
// phase is go-llm-types' value as is: narration, final, or thought (the
// spelling the ACP translators also emit), so thinking reads the same on
// every runtime.
func withBlock(p map[string]any, blockID, phase string) map[string]any {
	if blockID != "" {
		p["block_id"] = blockID
	}
	if phase != "" {
		p["phase"] = phase
	}
	return p
}

// translateProviderEvent converts richer provider/events.Event frames
// into runtime events that the legacy llmtypes.StreamEvent surface cannot
// represent. Delta/tool_use/terminal/session events are intentionally skipped
// here because EventFanout already carries those through translateStreamEvent.
func translateProviderEvent(ev pevents.Event) (kind runtimeevents.EventKind, payload any, ok bool) {
	switch e := ev.(type) {
	case pevents.ToolResult:
		return runtimeevents.KindAgentToolResult, map[string]any{
			"tool_result": map[string]any{
				"id":              e.ID,
				"is_error":        e.IsError,
				"content_preview": e.ContentPreview,
			},
		}, true
	case pevents.SubagentSpawn:
		return runtimeevents.KindAgentSubagentSpawn, map[string]any{
			"subagent_spawn": map[string]any{
				"tool": e.Tool,
				"args": e.Args,
			},
		}, true
	case pevents.SessionLost:
		return runtimeevents.KindSessionLost, map[string]any{
			"requested_id": e.RequestedID,
			"actual_id":    e.ActualID,
			"reason":       e.Reason,
		}, true
	case pevents.AuthFailed:
		return runtimeevents.KindSessionAuthFailed, map[string]any{
			"error": e.Message,
		}, true
	case pevents.PermissionDenied:
		p := map[string]any{
			"action":       e.Action,
			"display_name": e.DisplayName,
		}
		// The id of the refused tool call, when the CLI names it, so a consumer can
		// tell a refusal the turn ended on from one the agent worked around.
		if e.ToolUseID != "" {
			p["tool_use_id"] = e.ToolUseID
		}
		return runtimeevents.KindAgentPermissionDenied, p, true
	case pevents.Heartbeat:
		last := e.LastActivityAt
		if last.IsZero() {
			last = time.Now().UTC()
		}
		return runtimeevents.KindSessionHeartbeat, map[string]any{
			"last_activity_at": last,
		}, true
	default:
		return "", nil, false
	}
}
