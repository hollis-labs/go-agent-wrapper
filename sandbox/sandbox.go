package sandbox

import "context"

// Applier applies a sandbox profile to a child-process context before
// exec. The wrapper calls Apply once after spawning the child but before
// any agent-visible work begins, and emits a sandbox.applied runtime
// event with the resulting [Result].
type Applier interface {
	Apply(ctx context.Context, pid int) (Result, error)
}

// Result is what an [Applier] reports after a profile has been applied
// (or refused). The wrapper surfaces this verbatim into the
// sandbox.applied runtime event so operators can see what the wrapped
// process actually got, not just what the profile asked for.
type Result struct {
	// Profile is the name of the applied profile ("default-deny",
	// "nanite-cli", ...).
	Profile string

	// Applied reports whether the profile took effect. False with a
	// non-nil error from Apply indicates the wrapper refused to start
	// the child; false with no error indicates the profile was
	// configured to warn-only and the child started without enforcement.
	Applied bool

	// Notes carries human-readable detail about what was applied,
	// downgraded, or skipped (e.g., "seccomp unavailable on darwin —
	// falling back to sandbox-exec").
	Notes []string
}

// NoOpApplier applies nothing. Use as a placeholder when the wrapper is
// wired but no sandbox profile is configured.
type NoOpApplier struct{}

// Apply implements [Applier].
func (NoOpApplier) Apply(_ context.Context, _ int) (Result, error) {
	return Result{Profile: "none", Applied: false, Notes: []string{"no sandbox configured"}}, nil
}
