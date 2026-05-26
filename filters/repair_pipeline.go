package filters

import (
	"context"

	hrepair "github.com/hollis-labs/go-harness-filters/repair"
)

// RepairPipeline adapts go-harness-filters repairers to the wrapper's
// Pipeline interface. It only returns Repaired content for syntactic,
// non-semantic repairs; semantic-changing repairs are exposed as Notes
// and leave content untouched.
type RepairPipeline struct {
	Repairer hrepair.Repairer
}

// NewRepairPipeline composes repairers in order and returns a Pipeline.
func NewRepairPipeline(repairers ...hrepair.Repairer) RepairPipeline {
	return RepairPipeline{Repairer: hrepair.Chain(repairers)}
}

// Process implements [Pipeline].
func (p RepairPipeline) Process(_ context.Context, in Input) (Output, error) {
	if p.Repairer == nil {
		return Output{}, nil
	}
	out := p.Repairer.Repair(hrepair.Input{
		Kind:     repairKind(in.Kind),
		Content:  in.Content,
		Metadata: in.Metadata,
	})
	if !out.Repaired {
		return Output{}, nil
	}
	if out.SemanticChange {
		return Output{Notes: []string{out.Reason}}, nil
	}
	return Output{
		Repaired: out.Replacement,
		Tags: map[string]string{
			"repair.rule_id": out.RuleID,
		},
		Notes: []string{out.Reason},
	}, nil
}

func repairKind(kind string) string {
	switch kind {
	case "envelope", "tool_output", "command_output", "command_output_line":
		return "envelope"
	default:
		return kind
	}
}
