package policy

import (
	"context"
	"testing"
)

func TestNoOpObserverReportsNoRecommendation(t *testing.T) {
	var observer Observer = NoOpObserver{}
	finding, err := observer.Observe(context.Background(), Observation{Kind: "command", Original: "ls"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if finding.Recommendation != RecommendationNone {
		t.Errorf("Recommendation = %q, want %q", finding.Recommendation, RecommendationNone)
	}
}

func TestRecommendationWireValuesAreStable(t *testing.T) {
	// Lock the on-the-wire string values; flipping these would break
	// stored rule files and audit logs.
	cases := map[Recommendation]string{
		RecommendationNone:            "observe",
		RecommendationNudge:           "nudge",
		RecommendationRewrite:         "rewrite",
		RecommendationBlock:           "block",
		RecommendationRequestApproval: "approval",
	}
	for recommendation, want := range cases {
		if string(recommendation) != want {
			t.Errorf("Recommendation %v = %q, want %q (changing this breaks persisted rules)", recommendation, string(recommendation), want)
		}
	}
}
