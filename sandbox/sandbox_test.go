package sandbox

import (
	"context"
	"testing"
)

func TestNoOpApplier(t *testing.T) {
	var a Applier = NoOpApplier{}
	r, err := a.Apply(context.Background(), 12345)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if r.Applied {
		t.Errorf("NoOpApplier reported Applied=true")
	}
	if r.Profile != "none" {
		t.Errorf("Profile = %q, want none", r.Profile)
	}
}
