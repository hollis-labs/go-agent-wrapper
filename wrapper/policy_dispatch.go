package wrapper

import (
	"context"
	"encoding/json"

	llmtypes "github.com/hollis-labs/go-llm-types"

	"github.com/hollis-labs/go-agent-wrapper/policy"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// observeToolUsePolicy is called from the wrapper's translator goroutine after
// an [llmtypes.EventToolUse] has been emitted and Config.PolicyObserver is
// configured. It builds a [policy.Observation], asks the observer for a
// [policy.Finding], and emits the matching legacy policy.* runtime event with
// ParentID correlating back to the originating tool_use event.
//
// Errors from [policy.Observer.Observe] are silently dropped. The preceding
// agent.tool_use event already fired, so the missing advisory event is the only
// cost. Observers should be cheap and synchronous on the hot path.
//
// The wrapper cannot rewrite, block, pause, or approve the child operation at
// this point. A RecommendationBlock or RecommendationRewrite finding is
// reporting, not enforcement.
func (w *Wrapper) observeToolUsePolicy(
	ctx context.Context,
	source runtimeevents.Source,
	ev llmtypes.StreamEvent,
	toolUseEventID string,
	turnID string,
) {
	if w.cfg.PolicyObserver == nil || ev.ToolUse == nil {
		return
	}

	original := serializeToolUse(ev.ToolUse)
	finding, err := w.cfg.PolicyObserver.Observe(ctx, policy.Observation{
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

	kind, ok := policyRecommendationToLegacyEventKind(finding.Recommendation)
	if !ok {
		return // RecommendationNone and unmapped values emit nothing
	}

	payload := map[string]any{
		"rule_id": finding.RuleID,
		// "mode" is retained for event-payload compatibility. Its value is
		// now the observer's advisory Recommendation, not an applied action.
		"mode":     string(finding.Recommendation),
		"original": original,
	}
	if finding.Message != "" {
		payload["message"] = finding.Message
	}
	if finding.SuggestedReplacement != "" {
		// "replacement" is another retained wire key. It is suggested data;
		// the wrapper never writes it back into the child input.
		payload["replacement"] = finding.SuggestedReplacement
	}

	opts := []runtimeevents.EmitOption{
		runtimeevents.WithParentID(toolUseEventID),
	}
	if turnID != "" {
		opts = append(opts, runtimeevents.WithTurnID(turnID))
	}
	_ = w.cfg.Activity.Emit(ctx, kind, source, payload, opts...)
}

// policyRecommendationToLegacyEventKind maps an advisory
// [policy.Recommendation] onto the stable go-runtime-events policy.* kinds.
// The event-kind names are preserved for wire compatibility and do not imply
// that the wrapper performed the named operation.
func policyRecommendationToLegacyEventKind(recommendation policy.Recommendation) (runtimeevents.EventKind, bool) {
	switch recommendation {
	case policy.RecommendationNudge:
		return runtimeevents.KindPolicyNudge, true
	case policy.RecommendationRewrite:
		return runtimeevents.KindPolicyRewrite, true
	case policy.RecommendationBlock:
		return runtimeevents.KindPolicyBlock, true
	case policy.RecommendationRequestApproval:
		return runtimeevents.KindPolicyApprovalRequested, true
	default:
		return "", false
	}
}

// serializeToolUse encodes the tool use as compact JSON for policy
// matching. The result is what the policy observer sees as
// [policy.Observation.Original]; observers that pattern-match against tool
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
