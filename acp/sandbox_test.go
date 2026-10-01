package acp

import (
	"errors"
	"os/exec"
	"runtime"
	"slices"
	"testing"

	"github.com/hollis-labs/go-sandbox/sandbox"
)

// A remote or pre-existing endpoint cannot be kept from writing a protected
// path, so ProtectedPaths are refused whatever the policy, an explicitly
// disabled one included (CW-20261001-0162).
func TestCheckRemoteSandboxRefusesProtectedPaths(t *testing.T) {
	disabled := &sandbox.ResolvedAccessPolicy{ID: "off", Mode: sandbox.ConfinementDisabled}
	required := &sandbox.ResolvedAccessPolicy{ID: "required", Mode: sandbox.ConfinementRequired}
	for name, tc := range map[string]struct {
		policy *sandbox.ResolvedAccessPolicy
		wantID string
	}{
		"no policy":       {nil, ProtectOnlyProfileID},
		"disabled policy": {disabled, ProtectOnlyProfileID},
		"required policy": {required, "required"},
	} {
		t.Run(name, func(t *testing.T) {
			var outcomes []sandbox.EnforcementOutcome
			err := CheckRemoteSandbox(LaunchParams{
				SandboxPolicy:          tc.policy,
				ProtectedPaths:         []string{t.TempDir()},
				SandboxOutcomeCallback: func(out sandbox.EnforcementOutcome) { outcomes = append(outcomes, out) },
			})
			if !errors.Is(err, ErrRemoteSandboxUnsupported) {
				t.Fatalf("err = %v, want ErrRemoteSandboxUnsupported", err)
			}
			if len(outcomes) != 1 {
				t.Fatalf("outcomes = %+v, want one", outcomes)
			}
			out := outcomes[0]
			if out.State != sandbox.EnforcementUnsupported || out.PolicyID != tc.wantID || !slices.Contains(out.Unsupported, sandbox.CapWriteProtect) {
				t.Fatalf("outcome = %+v, want unsupported %s lacking %s", out, tc.wantID, sandbox.CapWriteProtect)
			}
		})
	}
}

// With no required policy, PrepareLaunchSandbox wraps the command in the
// protect-only host-filesystem profile: on Linux, bwrap with the host root
// bound writable and the protected directory bound read-only over it.
func TestPrepareLaunchSandboxProtectOnlyLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap argv shape is Linux-only")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}
	state := t.TempDir()
	cmd := exec.Command("/bin/true")
	out, cleanup, err := PrepareLaunchSandbox(cmd, LaunchParams{Cwd: t.TempDir(), ProtectedPaths: []string{state}})
	if err != nil {
		t.Fatalf("PrepareLaunchSandbox: %v", err)
	}
	defer cleanup()
	if out.PolicyID != ProtectOnlyProfileID || out.State != sandbox.EnforcementApplied || !out.Enforced {
		t.Fatalf("outcome = %+v, want %s applied", out, ProtectOnlyProfileID)
	}
	args := cmd.Args
	if !hasArgs(args, "--dev-bind", "/", "/") || !hasArgs(args, "--ro-bind", state, state) {
		t.Fatalf("argv = %q, want the host root writable and %s read-only", args, state)
	}
}

func hasArgs(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		if slices.Equal(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}
