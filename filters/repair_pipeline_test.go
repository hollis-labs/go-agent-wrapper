package filters

import (
	"context"
	"testing"

	hrepair "github.com/hollis-labs/go-harness-filters/repair"
)

func TestRepairPipelineAppliesNonSemanticRepair(t *testing.T) {
	p := NewRepairPipeline(hrepair.MissingClosingDelimiterJSON{})
	out, err := p.Process(context.Background(), Input{
		Kind:    "envelope",
		Content: []byte(`{"k":"v"`),
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if string(out.Repaired) != `{"k":"v"}` {
		t.Errorf("Repaired = %q", out.Repaired)
	}
	if out.Tags["repair.rule_id"] != "json.missing-closing-delimiter" {
		t.Errorf("repair rule tag = %q", out.Tags["repair.rule_id"])
	}
	if len(out.Notes) != 1 {
		t.Errorf("Notes = %v", out.Notes)
	}
}

func TestRepairPipelineSkipsSemanticRepair(t *testing.T) {
	p := RepairPipeline{Repairer: semanticRepairer{}}
	out, err := p.Process(context.Background(), Input{Kind: "envelope", Content: []byte("x")})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if out.Repaired != nil {
		t.Errorf("semantic repair should not be applied: %q", out.Repaired)
	}
	if len(out.Notes) != 1 {
		t.Errorf("Notes = %v", out.Notes)
	}
}

type semanticRepairer struct{}

func (semanticRepairer) Repair(in hrepair.Input) hrepair.Result {
	return hrepair.Result{
		Repaired:       true,
		Original:       in.Content,
		Replacement:    []byte("rewritten"),
		RuleID:         "semantic",
		Reason:         "would change meaning",
		SemanticChange: true,
	}
}
