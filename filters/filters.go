package filters

import "context"

// Pipeline runs the harness filter chain against a unit of wrapper IO
// (agent text, tool output, command output, envelope content). The
// wrapper calls Process at IO observation points; concrete pipelines
// live in (planned) go-harness-filters.
//
// Process is called from the wrapper's IO loop and must not block on
// network or LLM calls in the common path. Pipelines that need
// expensive classification should buffer and answer asynchronously by
// emitting a follow-up filter event.
type Pipeline interface {
	Process(ctx context.Context, in Input) (Output, error)
}

// Input is the unit the wrapper hands to a [Pipeline]. Kind names what
// the content is so the pipeline can dispatch to the right rule set.
type Input struct {
	// Kind classifies the content ("agent_text", "tool_output",
	// "command_output", "envelope", "tag", ...). Open string — new
	// kinds may be added as adapters introduce them.
	Kind string

	// Content is the raw bytes the wrapper observed.
	Content []byte

	// Metadata carries channel-specific context the pipeline may need
	// (provider name, tool name, command intent hint, ...).
	Metadata map[string]string
}

// Output is what a [Pipeline] returns. The wrapper surfaces Repaired
// content in the agent-visible stream when set; otherwise it passes
// Input.Content through unchanged.
type Output struct {
	// Repaired, when non-nil, replaces the original content for
	// downstream consumers. The original is preserved in the raw log
	// regardless.
	Repaired []byte

	// Tags adds metadata the wrapper attaches to subsequent runtime
	// events derived from this input.
	Tags map[string]string

	// Notes carries human-readable diagnostics ("repaired malformed
	// envelope: missing closing brace at offset 124").
	Notes []string
}

// Passthrough is the no-op pipeline. Use as a placeholder when the
// wrapper is wired but no filter pipeline is configured.
type Passthrough struct{}

// Process implements [Pipeline].
func (Passthrough) Process(_ context.Context, _ Input) (Output, error) {
	return Output{}, nil
}
