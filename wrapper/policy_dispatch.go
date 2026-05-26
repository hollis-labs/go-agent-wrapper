package wrapper

import (
	"context"
	"encoding/json"

	llmtypes "github.com/hollis-labs/go-llm-types"

	"github.com/hollis-labs/go-agent-wrapper/policy"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// applyToolUsePolicy is called from the wrapper's translator goroutine
// whenever an [llmtypes.EventToolUse] is observed and Config.Policy
// is configured. It builds a [policy.Request], asks the engine for a
// [policy.Decision], and emits the matching runtime event with
// ParentID correlating back to the originating tool_use event.
//
// Errors from [policy.Engine.Decide] are silently dropped — the
// preceding agent.tool_use event already fired, so observability is
// preserved; the missing policy event is the only cost. Policy
// engines should be cheap and synchronous on the hot path (see the
// policy.Engine docstring).
//
// This is the OBSERVATION half of policy: the wrapper does not
// rewrite or block the child's input/output based on the decision —
// it only surfaces the policy verdict into the activity stream.
// Rewrite-back semantics belong in a follow-up that touches the
// session input path.
func (w *Wrapper) applyToolUsePolicy(
	ctx context.Context,
	source runtimeevents.Source,
	ev llmtypes.StreamEvent,
	toolUseEventID string,
	turnID string,
) {
	if w.cfg.Policy == nil || ev.ToolUse == nil {
		return
	}

	original := serializeToolUse(ev.ToolUse)
	decision, err := w.cfg.Policy.Decide(ctx, policy.Request{
		App:        w.cfg.App,
		SessionID:  w.sessionID,
		TurnID:     turnID,
		Kind:       "tool_use",
		Original:   original,
		Channel:    source.Channel,
		Confidence: source.Confidence,
	})
	if err != nil {
		return
	}

	kind, ok := policyModeToEventKind(decision.Mode)
	if !ok {
		return // ModeObserve and unmapped modes emit nothing
	}

	payload := map[string]any{
		"rule_id":  decision.RuleID,
		"mode":     string(decision.Mode),
		"original": original,
	}
	if decision.Message != "" {
		payload["message"] = decision.Message
	}
	if decision.Replacement != "" {
		payload["replacement"] = decision.Replacement
	}

	opts := []runtimeevents.EmitOption{
		runtimeevents.WithParentID(toolUseEventID),
	}
	if turnID != "" {
		opts = append(opts, runtimeevents.WithTurnID(turnID))
	}
	_ = w.cfg.Activity.Emit(ctx, kind, source, payload, opts...)
}

// policyModeToEventKind maps a [policy.Mode] to the corresponding
// [runtimeevents.EventKind]. Returns ok=false for modes that should
// not emit a derived event (ModeObserve and any unmapped values).
//
// ModeApproval intentionally maps to policy.block in this skeleton —
// there is no separate approval-request event yet, and block is the
// closest existing kind. A dedicated approval flow lands when the
// wrapper learns to pause and resume sessions.
func policyModeToEventKind(mode policy.Mode) (runtimeevents.EventKind, bool) {
	switch mode {
	case policy.ModeNudge:
		return runtimeevents.KindPolicyNudge, true
	case policy.ModeRewrite:
		return runtimeevents.KindPolicyRewrite, true
	case policy.ModeBlock, policy.ModeApproval:
		return runtimeevents.KindPolicyBlock, true
	default:
		return "", false
	}
}

// serializeToolUse encodes the tool use as compact JSON for policy
// matching. The result is what the policy engine sees as
// [policy.Request.Original]; engines that pattern-match against tool
// usage should treat it as a stable JSON shape (id / name / input).
func serializeToolUse(tu *llmtypes.ToolUseBlock) string {
	if tu == nil {
		return ""
	}
	raw, err := json.Marshal(tu)
	if err != nil {
		return ""
	}
	return string(raw)
}
