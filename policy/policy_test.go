package policy

import (
	"context"
	"testing"
)

func TestObserveOnlyDecides(t *testing.T) {
	var e Engine = ObserveOnly{}
	d, err := e.Decide(context.Background(), Request{Kind: "command", Original: "ls"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Mode != ModeObserve {
		t.Errorf("Mode = %q, want %q", d.Mode, ModeObserve)
	}
}

func TestModesAreStable(t *testing.T) {
	// Lock the on-the-wire string values; flipping these would break
	// stored rule files and audit logs.
	cases := map[Mode]string{
		ModeObserve:  "observe",
		ModeNudge:    "nudge",
		ModeRewrite:  "rewrite",
		ModeBlock:    "block",
		ModeApproval: "approval",
	}
	for m, want := range cases {
		if string(m) != want {
			t.Errorf("Mode %v = %q, want %q (changing this breaks persisted rules)", m, string(m), want)
		}
	}
}
