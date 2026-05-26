package classifybridge

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/policy"
	"github.com/hollis-labs/go-harness-filters/classify"
)

func TestEngineSatisfiesPolicyEngineInterface(t *testing.T) {
	var _ policy.Engine = (*Engine)(nil)
}

func TestEngineNilClassifierObservesAll(t *testing.T) {
	e := &Engine{}
	d, err := e.Decide(context.Background(), policy.Request{Original: "anything"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Mode != policy.ModeObserve {
		t.Errorf("Mode = %q, want observe", d.Mode)
	}
}

func TestEngineNoMatchReturnsObserve(t *testing.T) {
	rules := classify.NewRuleSet(classify.Rule{
		ID:         "test.only-ls",
		Intent:     "x",
		ExactMatch: "ls",
		Confidence: classify.ConfidenceExact,
	})
	e := &Engine{Classifier: rules}
	d, err := e.Decide(context.Background(), policy.Request{Original: "pwd"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Mode != policy.ModeObserve {
		t.Errorf("Mode = %q, want observe", d.Mode)
	}
	if d.RuleID != "" {
		t.Errorf("observe decision should have no RuleID; got %q", d.RuleID)
	}
}

func TestEngineReversibleFalseProducesNudge(t *testing.T) {
	// NaniteDeployRule is the canonical Reversible=false rule —
	// rewriting from local build to production deploy would change
	// semantics silently.
	rules := classify.NewRuleSet(classify.NaniteDeployRule)
	e := &Engine{Classifier: rules}
	d, err := e.Decide(context.Background(), policy.Request{
		Original: "go build -o nanite ./cmd/nanite",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Mode != policy.ModeNudge {
		t.Errorf("Mode = %q, want nudge (Reversible=false → safe default)", d.Mode)
	}
	if d.RuleID != "hollis.deploy.nanite.cerberus-required" {
		t.Errorf("RuleID = %q, want hollis.deploy.nanite.cerberus-required", d.RuleID)
	}
	if !strings.Contains(d.Message, "cerberus_resource_deploy nanite-api-service") {
		t.Errorf("Message missing the cerberus deploy command; got %q", d.Message)
	}
	if d.Replacement != "" {
		t.Errorf("Replacement should be empty for nudge; got %q", d.Replacement)
	}
}

func TestEngineReversibleTrueProducesRewrite(t *testing.T) {
	reversibleRule := classify.Rule{
		ID:          "test.reversible",
		Intent:      "format.json",
		RegexpMatch: regexp.MustCompile(`cat .*\.json`),
		Confidence:  classify.ConfidenceExact,
		Recommended: []string{"jq . <path>"},
		Reversible:  true,
	}
	e := &Engine{Classifier: classify.NewRuleSet(reversibleRule)}
	d, err := e.Decide(context.Background(), policy.Request{Original: "cat config.json"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Mode != policy.ModeRewrite {
		t.Errorf("Mode = %q, want rewrite (Reversible=true)", d.Mode)
	}
	if d.Replacement != "jq . <path>" {
		t.Errorf("Replacement = %q", d.Replacement)
	}
}

func TestEngineNudgeModeOverride(t *testing.T) {
	// Operator wants to suppress all rewrites — set RewriteMode to
	// ModeNudge so even reversible rules produce nudges.
	reversibleRule := classify.Rule{
		ID:          "test.reversible",
		Intent:      "x",
		RegexpMatch: regexp.MustCompile(`cat .*\.json`),
		Confidence:  classify.ConfidenceExact,
		Recommended: []string{"jq . <path>"},
		Reversible:  true,
	}
	e := &Engine{
		Classifier:  classify.NewRuleSet(reversibleRule),
		RewriteMode: policy.ModeNudge,
	}
	d, _ := e.Decide(context.Background(), policy.Request{Original: "cat config.json"})
	if d.Mode != policy.ModeNudge {
		t.Errorf("Mode = %q, want nudge (RewriteMode override)", d.Mode)
	}
	if d.Replacement != "" {
		t.Errorf("Replacement should be empty when mode is nudge; got %q", d.Replacement)
	}
}

func TestEngineNudgeModeOverrideToBlock(t *testing.T) {
	// Operator wants to be strict — set NudgeMode to ModeBlock so
	// even non-reversible recommendations become hard stops.
	rules := classify.NewRuleSet(classify.NaniteDeployRule)
	e := &Engine{Classifier: rules, NudgeMode: policy.ModeBlock}
	d, _ := e.Decide(context.Background(), policy.Request{
		Original: "go build -o nanite ./cmd/nanite",
	})
	if d.Mode != policy.ModeBlock {
		t.Errorf("Mode = %q, want block (NudgeMode override)", d.Mode)
	}
}

func TestEnginePassesRequestContextAsMetadata(t *testing.T) {
	// The bridge passes req.App / SessionID / TurnID / Channel into
	// Input.Metadata so classifier rules can branch on caller context.
	// We use a stub classifier that captures what it receives.
	captured := &capturingClassifier{}
	e := &Engine{Classifier: captured}
	_, _ = e.Decide(context.Background(), policy.Request{
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
