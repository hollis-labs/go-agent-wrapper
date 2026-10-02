package turnoutput_test

import (
	"encoding/json"
	"testing"

	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/go-providers/providertest"

	"github.com/hollis-labs/go-agent-wrapper/turnoutput"
)

// These tests are the native path: a host (Tether) taps the real go-providers
// adapter, takes its typed events from ParseLineEvents, which is what agentkit's
// StartOptions.TypedEventCallback delivers, and feeds them to ObserveProvider.
// They run what each CLI wrote (go-providers' captured fixtures) through the
// adapter that parses it, so the reducer is held to what the provider really
// reports on its terminal event: the final text, exact, for Claude, agy and
// opencode, and the final-phase delta for Codex exec.

// fixtureFrames returns the lines the CLI wrote to stdout in a fixture: a
// per-turn fixture is its stdout, a duplex transcript the frames it sent.
func fixtureFrames(t *testing.T, name string) [][]byte {
	t.Helper()
	lines := providertest.FixtureLines(t, name)
	if len(name) < len(".transcript.jsonl") || name[len(name)-len(".transcript.jsonl"):] != ".transcript.jsonl" {
		return lines
	}
	var out [][]byte
	for _, line := range lines {
		var step struct {
			Send json.RawMessage `json:"send"`
		}
		if json.Unmarshal(line, &step) == nil && len(step.Send) > 0 {
			out = append(out, step.Send)
		}
	}
	return out
}

// reduceTyped feeds every typed event the adapter parses from the fixture to a
// fresh reducer and returns the Outputs, in order.
func reduceTyped(t *testing.T, adapter provider.EventParser, name string) []turnoutput.Output {
	t.Helper()
	r := turnoutput.New(turnoutput.Config{SessionID: "ses_native", Runtime: "native"})
	var outs []turnoutput.Output
	for _, line := range fixtureFrames(t, name) {
		evs, err := adapter.ParseLineEvents(line)
		if err != nil {
			t.Fatalf("ParseLineEvents: %v", err)
		}
		for _, ev := range evs {
			if out, ok := r.ObserveProvider(ev); ok {
				outs = append(outs, out)
			}
		}
	}
	return outs
}

func TestObserveProviderConsumesTheProvidersFinalText(t *testing.T) {
	const (
		claudeHi     = "Hi! 👋 I'm ready to help with software engineering tasks. What would you like to work on?"
		claudeDenied = "I need your approval to create the file `providertest.txt` in the current directory. Should I proceed?"
	)
	tests := []struct {
		name    string
		adapter func() provider.EventParser
		fixture string
		want    []string // the text of each turn, every one exact
	}{
		{"claude print", func() provider.EventParser { return provider.NewClaudeAdapter() }, "claude/print_turn1.jsonl", []string{claudeHi}},
		{"claude print, last message after tool calls", func() provider.EventParser { return provider.NewClaudeAdapter() }, "claude/print_tool_denied.jsonl", []string{claudeDenied}},
		{"claude streaming, two turns", func() provider.EventParser { return provider.NewClaudeAdapterStreamingStdio() }, "claude/stream_two_turns.transcript.jsonl",
			[]string{"Hi! 👋 I'm ready to help you with software engineering tasks. What would you like to work on?", "Bye! 👋 Feel free to reach out anytime you need help with code."}},
		{"agy", func() provider.EventParser { return provider.NewAntigravityAdapter() }, "antigravity/print_turn1.jsonl", []string{"OK"}},
		{"agy, after a tool step", func() provider.EventParser { return provider.NewAntigravityAdapter() }, "antigravity/print_tool_run.jsonl", []string{"done"}},
		{"agy, after an MCP tool", func() provider.EventParser { return provider.NewAntigravityAdapter() }, "antigravity/print_mcp_tool.jsonl",
			[]string{"MCP=SECRET-WORD-MAGNOLIA; ADR=WORKSPACE-OVERRIDE-MARKER skill for probing precedence"}},
		{"opencode run", func() provider.EventParser { return provider.NewOpencodeAdapter() }, "opencode/run_turn1.jsonl", []string{"OK."}},
		{"opencode run, answer after tool steps", func() provider.EventParser { return provider.NewOpencodeAdapter() }, "opencode/run_tool_use.jsonl", []string{"hello fixture"}},
		{"codex exec, final-phase delta", func() provider.EventParser { return provider.NewCodexAdapter() }, "codex/exec_turn1.jsonl", []string{"Hi!"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outs := reduceTyped(t, tt.adapter(), tt.fixture)
			if len(outs) != len(tt.want) {
				t.Fatalf("reported %d turn(s), want %d: %+v", len(outs), len(tt.want), outs)
			}
			for i, out := range outs {
				if out.Kind != turnoutput.KindFinal || out.Text != tt.want[i] || out.Confidence != turnoutput.ConfidenceExact {
					t.Errorf("turn %d = %+v, want a final turn with text %q and exact confidence", i+1, out, tt.want[i])
				}
			}
		})
	}
}

// A terminal text outranks a final-phase delta that differs, on the provider feed
// as on the others.
func TestObserveProviderDoneTextBeatsAFinalDelta(t *testing.T) {
	r := turnoutput.New(turnoutput.Config{SessionID: "s", Runtime: "claude"})
	r.ObserveProvider(events.Delta{Text: "marked final", Phase: "final", BlockID: "a"})
	got, ok := r.ObserveProvider(events.Done{StopReason: "end_turn", Text: "the result"})
	if !ok || got.Text != "the result" || got.Confidence != turnoutput.ConfidenceExact {
		t.Fatalf("got %+v, want the done's text, exact", got)
	}
}
