package wrapper

import (
	"context"
	"encoding/json"

	llmtypes "github.com/hollis-labs/go-llm-types"

	"github.com/hollis-labs/go-agent-wrapper/filters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func (w *Wrapper) filterStreamEvent(ctx context.Context, ev llmtypes.StreamEvent) llmtypes.StreamEvent {
	if w.cfg.Filters == nil {
		return ev
	}
	switch ev.Type {
	case llmtypes.EventDelta:
		out, err := w.cfg.Filters.Process(ctx, filters.Input{
			Kind:    "agent_text",
			Content: []byte(ev.Content),
			Metadata: map[string]string{
				"provider": w.cfg.Adapter.Describe().Provider,
			},
		})
		if err == nil && out.Repaired != nil {
			ev.Content = string(out.Repaired)
		}
	case llmtypes.EventToolUse:
		if ev.ToolUse == nil {
			return ev
		}
		raw, err := json.Marshal(ev.ToolUse)
		if err != nil {
			return ev
		}
		out, err := w.cfg.Filters.Process(ctx, filters.Input{
			Kind:    "envelope",
			Content: raw,
			Metadata: map[string]string{
				"provider":  w.cfg.Adapter.Describe().Provider,
				"tool_name": ev.ToolUse.Name,
			},
		})
		if err != nil || out.Repaired == nil {
			return ev
		}
		var repaired llmtypes.ToolUseBlock
		if err := json.Unmarshal(out.Repaired, &repaired); err == nil {
			ev.ToolUse = &repaired
		}
	}
	return ev
}

func (w *Wrapper) filterPayload(ctx context.Context, kind runtimeevents.EventKind, payload any) any {
	if w.cfg.Filters == nil {
		return payload
	}
	p, ok := payload.(map[string]any)
	if !ok {
		return payload
	}

	switch kind {
	case runtimeevents.KindAgentToolResult:
		return w.filterToolResultPayload(ctx, p)
	default:
		return payload
	}
}

func (w *Wrapper) filterToolResultPayload(ctx context.Context, payload map[string]any) any {
	tr, ok := payload["tool_result"].(map[string]any)
	if !ok {
		return payload
	}
	preview, _ := tr["content_preview"].(string)
	out, err := w.cfg.Filters.Process(ctx, filters.Input{
		Kind:    "tool_output",
		Content: []byte(preview),
		Metadata: map[string]string{
			"provider": w.cfg.Adapter.Describe().Provider,
		},
	})
	if err == nil && out.Repaired != nil {
		cp := cloneMap(payload)
		nested := cloneMap(tr)
		nested["content_preview"] = string(out.Repaired)
		cp["tool_result"] = nested
		return cp
	}
	return payload
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
