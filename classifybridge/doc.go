// Package classifybridge adapts a
// github.com/hollis-labs/go-harness-filters classify.Classifier (typically a
// RuleSet) into a github.com/hollis-labs/go-agent-wrapper policy.Observer.
//
// The two libraries deliberately keep their schemas separate —
// classify owns "what is this content?" and policy owns "what advisory
// recommendation accompanies this observation?". This bridge translates
// between them. It does not modify or stop the observed operation.
//
// Wiring:
//
//	import (
//	    "github.com/hollis-labs/go-agent-wrapper/classifybridge"
//	    "github.com/hollis-labs/go-agent-wrapper/wrapper"
//	    "github.com/hollis-labs/go-harness-filters/classify"
//	)
//
//	rules := classify.NewRuleSet(classify.NaniteDeployRule)
//	observer := &classifybridge.Observer{Classifier: rules}
//
//	w, _ := wrapper.New(wrapper.Config{
//	    // ...
//	    PolicyObserver: observer,
//	})
//
// Translation defaults:
//
//   - classify.Match.Reversible == false → policy.RecommendationNudge.
//   - classify.Match.Reversible == true → policy.RecommendationRewrite, with
//     the first recommended command surfaced as inert suggested-replacement
//     data.
//
// Both defaults are overridable on the [Observer] struct.
package classifybridge
