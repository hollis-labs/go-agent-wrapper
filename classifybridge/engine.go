package classifybridge

import (
	"context"
	"strings"

	"github.com/hollis-labs/go-agent-wrapper/policy"
	"github.com/hollis-labs/go-harness-filters/classify"
)

// Engine wraps a [classify.Classifier] and implements
// [policy.Engine]. Each [Engine.Decide] call runs the wrapped
// classifier against the request's Original content; a Match
// produces a Decision, no match falls through to [policy.ModeObserve].
//
// Zero value is a no-op engine (no Classifier → always ModeObserve).
type Engine struct {
	// Classifier is the source of [classify.Match] verdicts. Required
	// for the engine to do anything beyond observe-only.
	Classifier classify.Classifier

	// NudgeMode is returned when a [classify.Match] has Reversible ==
	// false. Zero value is [policy.ModeNudge].
	NudgeMode policy.Mode

	// RewriteMode is returned when a [classify.Match] has Reversible
	// == true. Zero value is [policy.ModeRewrite]. Set to
	// [policy.ModeNudge] to suppress rewrites globally even for
	// reversible rules.
	RewriteMode policy.Mode
}

// Decide implements [policy.Engine]. Runs the wrapped classifier
// against req.Original; on match, translates the resulting
// [classify.Match] into a [policy.Decision]. On no match, returns
// ModeObserve with no rule attribution.
//
// req.App, req.SessionID, req.TurnID, and req.Channel are passed
// through to the classifier as [classify.Input.Metadata] so rule
// implementations can branch on caller context if needed.
func (e *Engine) Decide(_ context.Context, req policy.Request) (policy.Decision, error) {
	if e.Classifier == nil {
		return policy.Decision{Mode: policy.ModeObserve}, nil
	}

	result := e.Classifier.Classify(classify.Input{
		Kind:    req.Kind,
		Content: []byte(req.Original),
		Metadata: map[string]string{
			"app":        req.App,
			"session_id": req.SessionID,
			"turn_id":    req.TurnID,
			"channel":    string(req.Channel),
		},
	})
	if result.Match == nil {
		return policy.Decision{Mode: policy.ModeObserve}, nil
	}

	mode := e.modeFor(result.Match.Reversible)

	return policy.Decision{
		Mode:        mode,
		RuleID:      result.Match.RuleID,
		Message:     formatMessage(result.Match),
		Replacement: formatReplacement(result.Match, mode),
	}, nil
}

func (e *Engine) modeFor(reversible bool) policy.Mode {
	if reversible {
		if e.RewriteMode != "" {
			return e.RewriteMode
		}
		return policy.ModeRewrite
	}
	if e.NudgeMode != "" {
		return e.NudgeMode
	}
	return policy.ModeNudge
}

// formatMessage builds the human-readable guidance shown to the
// agent. For matches with Recommended commands, it lists them under a
// "Recommended:" heading. For matches with no Recommended commands,
// the message is empty (the rule still emits an event by ID).
func formatMessage(m *classify.Match) string {
	if len(m.Recommended) == 0 {
		return ""
	}
	return "Recommended:\n" + strings.Join(m.Recommended, "\n")
}

// formatReplacement returns the substitution command for
// [policy.ModeRewrite] mode. The first Recommended command is used
// as the replacement; multi-command Recommended slices would require
// a richer disclosure shape, deferred until a real use case demands
// it. Other modes return empty.
func formatReplacement(m *classify.Match, mode policy.Mode) string {
	if mode != policy.ModeRewrite || len(m.Recommended) == 0 {
		return ""
	}
	return m.Recommended[0]
}
