// Package classifybridge adapts a
// github.com/hollis-labs/go-harness-filters classify.Classifier
// (typically a RuleSet) into a
// github.com/hollis-labs/go-agent-wrapper policy.Engine.
//
// The two libraries deliberately keep their schemas separate —
// classify owns "what is this content?" and policy owns "what should
// the wrapper do about it?". This bridge translates between them so
// rule-driven classification can drive wrapper policy enforcement
// without either side depending on the other.
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
//	engine := &classifybridge.Engine{Classifier: rules}
//
//	w, _ := wrapper.New(wrapper.Config{
//	    // ...
//	    Policy: engine,
//	})
//
// Translation defaults:
//
//   - classify.Match.Reversible == false → policy.ModeNudge
//     (rewrites would change observable system state without operator
//     approval; default-safe is to surface the recommendation and let
//     the agent keep owning the choice).
//   - classify.Match.Reversible == true → policy.ModeRewrite
//     (the recommended alternative is semantically equivalent and safe
//     to substitute automatically).
//
// Both defaults are overridable on the [Engine] struct.
package classifybridge
