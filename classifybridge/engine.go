package classifybridge

import (
	"context"
	"strings"

	"github.com/hollis-labs/go-agent-wrapper/policy"
	"github.com/hollis-labs/go-harness-filters/classify"
)

// Observer wraps a [classify.Classifier] and implements [policy.Observer].
// Each [Observer.Observe] call runs the wrapped classifier against an
// observation's Original content; a Match produces a Finding, and no match
// falls through to [policy.RecommendationNone].
//
// Zero value is a no-op observer (no Classifier means no recommendation).
type Observer struct {
	// Classifier is the source of [classify.Match] verdicts. Required
	// for the observer to report a recommendation.
	Classifier classify.Classifier

	// NonReversibleRecommendation is returned when a [classify.Match] has
	// Reversible == false. Zero value is [policy.RecommendationNudge].
	NonReversibleRecommendation policy.Recommendation

	// ReversibleRecommendation is returned when a [classify.Match] has
	// Reversible == true. Zero value is [policy.RecommendationRewrite]. Set
	// it to [policy.RecommendationNudge] to report a nudge instead of a
	// suggested replacement for reversible rules.
	ReversibleRecommendation policy.Recommendation
}

// Observe implements [policy.Observer]. It runs the wrapped classifier
// against observation.Original; on match, it translates the resulting
// [classify.Match] into a [policy.Finding]. On no match, it returns
// RecommendationNone with no rule attribution.
//
// observation.App, observation.SessionID, observation.TurnID, and
// observation.Channel are passed
// through to the classifier as [classify.Input.Metadata] so rule
// implementations can branch on caller context if needed.
func (o *Observer) Observe(_ context.Context, observation policy.Observation) (policy.Finding, error) {
	if o.Classifier == nil {
		return policy.Finding{Recommendation: policy.RecommendationNone}, nil
	}

	result := o.Classifier.Classify(classify.Input{
		Kind:    observation.Kind,
		Content: []byte(observation.Original),
		Metadata: map[string]string{
			"app":        observation.App,
			"session_id": observation.SessionID,
			"turn_id":    observation.TurnID,
			"channel":    string(observation.Channel),
		},
	})
	if result.Match == nil {
		return policy.Finding{Recommendation: policy.RecommendationNone}, nil
	}

	recommendation := o.recommendationFor(result.Match.Reversible)

	return policy.Finding{
		Recommendation:       recommendation,
		RuleID:               result.Match.RuleID,
		Message:              formatMessage(result.Match),
		SuggestedReplacement: formatSuggestedReplacement(result.Match, recommendation),
	}, nil
}

func (o *Observer) recommendationFor(reversible bool) policy.Recommendation {
	if reversible {
		if o.ReversibleRecommendation != "" {
			return o.ReversibleRecommendation
		}
		return policy.RecommendationRewrite
	}
	if o.NonReversibleRecommendation != "" {
		return o.NonReversibleRecommendation
	}
	return policy.RecommendationNudge
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

// formatSuggestedReplacement returns the first recommended command when the
// finding recommends a rewrite. It remains inert data; the bridge and wrapper
// never substitute it into child input. Multi-command Recommended slices would
// require a richer disclosure shape, deferred until a real use case demands it.
func formatSuggestedReplacement(m *classify.Match, recommendation policy.Recommendation) string {
	if recommendation != policy.RecommendationRewrite || len(m.Recommended) == 0 {
		return ""
	}
	return m.Recommended[0]
}
