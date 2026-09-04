package classifybridge

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/policy"
	"github.com/hollis-labs/go-harness-filters/classify"
)

func TestObserverSatisfiesPolicyObserverInterface(t *testing.T) {
	var _ policy.Observer = (*Observer)(nil)
}

func TestObserverNilClassifierReportsNoRecommendation(t *testing.T) {
	observer := &Observer{}
	finding, err := observer.Observe(context.Background(), policy.Observation{Original: "anything"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if finding.Recommendation != policy.RecommendationNone {
		t.Errorf("Recommendation = %q, want none", finding.Recommendation)
	}
}

func TestObserverNoMatchReturnsNoRecommendation(t *testing.T) {
	rules := classify.NewRuleSet(classify.Rule{
		ID:         "test.only-ls",
		Intent:     "x",
		ExactMatch: "ls",
		Confidence: classify.ConfidenceExact,
	})
	observer := &Observer{Classifier: rules}
	finding, err := observer.Observe(context.Background(), policy.Observation{Original: "pwd"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if finding.Recommendation != policy.RecommendationNone {
		t.Errorf("Recommendation = %q, want none", finding.Recommendation)
	}
	if finding.RuleID != "" {
		t.Errorf("empty finding should have no RuleID; got %q", finding.RuleID)
	}
}

func TestObserverNonReversibleProducesNudgeRecommendation(t *testing.T) {
	// NaniteDeployRule is the canonical Reversible=false rule —
	// rewriting from local build to production deploy would change
	// semantics silently.
	rules := classify.NewRuleSet(classify.NaniteDeployRule)
	observer := &Observer{Classifier: rules}
	finding, err := observer.Observe(context.Background(), policy.Observation{
		Original: "go build -o nanite ./cmd/nanite",
	})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if finding.Recommendation != policy.RecommendationNudge {
		t.Errorf("Recommendation = %q, want nudge", finding.Recommendation)
	}
	if finding.RuleID != "hollis.deploy.nanite.cerberus-required" {
		t.Errorf("RuleID = %q, want hollis.deploy.nanite.cerberus-required", finding.RuleID)
	}
	if !strings.Contains(finding.Message, "cerberus_resource_deploy nanite-api-service") {
		t.Errorf("Message missing the cerberus deploy command; got %q", finding.Message)
	}
	if finding.SuggestedReplacement != "" {
		t.Errorf("SuggestedReplacement should be empty for nudge; got %q", finding.SuggestedReplacement)
	}
}

func TestObserverReversibleProducesRewriteRecommendation(t *testing.T) {
	reversibleRule := classify.Rule{
		ID:          "test.reversible",
		Intent:      "format.json",
		RegexpMatch: regexp.MustCompile(`cat .*\.json`),
		Confidence:  classify.ConfidenceExact,
		Recommended: []string{"jq . <path>"},
		Reversible:  true,
	}
	observer := &Observer{Classifier: classify.NewRuleSet(reversibleRule)}
	finding, err := observer.Observe(context.Background(), policy.Observation{Original: "cat config.json"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if finding.Recommendation != policy.RecommendationRewrite {
		t.Errorf("Recommendation = %q, want rewrite", finding.Recommendation)
	}
	if finding.SuggestedReplacement != "jq . <path>" {
		t.Errorf("SuggestedReplacement = %q", finding.SuggestedReplacement)
	}
}

func TestObserverReversibleRecommendationOverride(t *testing.T) {
	// An observer can report a nudge even for reversible rules.
	reversibleRule := classify.Rule{
		ID:          "test.reversible",
		Intent:      "x",
		RegexpMatch: regexp.MustCompile(`cat .*\.json`),
		Confidence:  classify.ConfidenceExact,
		Recommended: []string{"jq . <path>"},
		Reversible:  true,
	}
	observer := &Observer{
		Classifier:               classify.NewRuleSet(reversibleRule),
		ReversibleRecommendation: policy.RecommendationNudge,
	}
	finding, _ := observer.Observe(context.Background(), policy.Observation{Original: "cat config.json"})
	if finding.Recommendation != policy.RecommendationNudge {
		t.Errorf("Recommendation = %q, want nudge", finding.Recommendation)
	}
	if finding.SuggestedReplacement != "" {
		t.Errorf("SuggestedReplacement should be empty for a nudge; got %q", finding.SuggestedReplacement)
	}
}

func TestObserverNonReversibleRecommendationOverrideToBlock(t *testing.T) {
	// "Block" remains an advisory label: the observer only reports it.
	rules := classify.NewRuleSet(classify.NaniteDeployRule)
	observer := &Observer{
		Classifier:                  rules,
		NonReversibleRecommendation: policy.RecommendationBlock,
	}
	finding, _ := observer.Observe(context.Background(), policy.Observation{
		Original: "go build -o nanite ./cmd/nanite",
	})
	if finding.Recommendation != policy.RecommendationBlock {
		t.Errorf("Recommendation = %q, want block", finding.Recommendation)
	}
}

func TestObserverPassesObservationContextAsMetadata(t *testing.T) {
	// The bridge passes observation.App / SessionID / TurnID / Channel into
	// Input.Metadata so classifier rules can branch on caller context.
	// We use a stub classifier that captures what it receives.
	captured := &capturingClassifier{}
	observer := &Observer{Classifier: captured}
	_, _ = observer.Observe(context.Background(), policy.Observation{
		App:       "nanite",
		SessionID: "ses_xyz",
		TurnID:    "turn_42",
		Original:  "any content",
		Channel:   "claude-stream-json",
	})
	if captured.lastInput.Metadata["app"] != "nanite" {
		t.Errorf("metadata.app = %q, want nanite", captured.lastInput.Metadata["app"])
	}
	if captured.lastInput.Metadata["session_id"] != "ses_xyz" {
		t.Errorf("metadata.session_id = %q, want ses_xyz", captured.lastInput.Metadata["session_id"])
	}
	if captured.lastInput.Metadata["turn_id"] != "turn_42" {
		t.Errorf("metadata.turn_id = %q, want turn_42", captured.lastInput.Metadata["turn_id"])
	}
	if captured.lastInput.Metadata["channel"] != "claude-stream-json" {
		t.Errorf("metadata.channel = %q, want claude-stream-json", captured.lastInput.Metadata["channel"])
	}
}

type capturingClassifier struct {
	lastInput classify.Input
}

func (c *capturingClassifier) Classify(in classify.Input) classify.Result {
	c.lastInput = in
	return classify.Result{}
}
