package filters

import (
	"context"
	"testing"
)

func TestPassthrough(t *testing.T) {
	var p Pipeline = Passthrough{}
	out, err := p.Process(context.Background(), Input{
		Kind:    "agent_text",
		Content: []byte("hello"),
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if out.Repaired != nil {
		t.Errorf("Passthrough produced Repaired content: %q", out.Repaired)
	}
}
