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
// rebinding the [activity.Bridge] [runtimeevents.Process]) — the
// caller should skip emission for that frame rather than emit a
// placeholder.
//
// payload is encoded as a structured map so consumers can json.Marshal
// it into the [runtimeevents.Event.Payload] slot uniformly.
func translateStreamEvent(ev llmtypes.StreamEvent) (kind runtimeevents.EventKind, payload any, ok bool) {
	switch ev.Type {
	case llmtypes.EventDelta:
		return runtimeevents.KindAgentDelta, map[string]any{
			"content": ev.Content,
		}, true

	case llmtypes.EventToolUse:
		p := map[string]any{}
		if ev.ToolUse != nil {
			p["tool_use"] = ev.ToolUse
		}
		return runtimeevents.KindAgentToolUse, p, true

	case llmtypes.EventUsage:
		return runtimeevents.KindTurnCompleted, map[string]any{
			"usage": ev.Usage,
		}, true

	case llmtypes.EventError:
		return runtimeevents.KindTurnFailed, map[string]any{
			"error": ev.Error,
		}, true

	case llmtypes.EventDone:
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
		return runtimeevents.KindAgentDelta, p, true

	default:
		// Unknown EventType — skip rather than emit a placeholder so
		// downstream consumers don't see "agent.delta with no
		// meaningful payload" frames for events the wrapper doesn't
		// yet model.
		return "", nil, false
	}
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
