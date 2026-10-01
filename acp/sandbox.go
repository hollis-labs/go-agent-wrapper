package acp

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"slices"

	"github.com/hollis-labs/go-sandbox/sandbox"
)

var ErrRemoteSandboxUnsupported = errors.New("acp: required sandbox policy cannot be verified for remote or pre-existing endpoint")

// LaunchCommand is an exact subprocess command override for locally spawned ACP
// clients. Binary is argv[0]; Args are argv[1:]. When Command is nil, each
// concrete client uses its provider-specific default command.
type LaunchCommand struct {
	Binary string
	Args   []string
}

// ResolveLaunchCommand returns params.Command when present, otherwise the
// concrete client's default command. Args are copied so callers cannot mutate a
// launch after validation.
func ResolveLaunchCommand(params LaunchParams, defaultBinary string, defaultArgs []string) (string, []string, error) {
	if params.Command == nil {
		return defaultBinary, append([]string(nil), defaultArgs...), nil
	}
	if params.Command.Binary == "" {
		return "", nil, errors.New("acp: LaunchParams.Command.Binary is required")
	}
	return params.Command.Binary, append([]string(nil), params.Command.Args...), nil
}

// ProtectOnlyProfileID names the sandbox an ACP launch runs under when
// LaunchParams.ProtectedPaths is set without a required SandboxPolicy. It is
// agentkit's minimal profile of the same name: default-allow, its only
// effect the write protection.
const ProtectOnlyProfileID = "protect-control-plane"

// PrepareLaunchSandbox applies the launch's local process confinement to cmd
// before Start: the resolved SandboxPolicy, with ProtectedPaths merged in, or
// the ProtectOnlyProfileID profile when ProtectedPaths has no required policy
// to merge into. It invokes SandboxOutcomeCallback for setup failures;
// callers must call ReportLaunchSandboxStarted after a successful Start, or
// ReportLaunchSandboxStartFailed if Start itself fails.
func PrepareLaunchSandbox(cmd *exec.Cmd, params LaunchParams) (sandbox.EnforcementOutcome, func(), error) {
	if len(params.ProtectedPaths) > 0 && !requiredPolicy(params.SandboxPolicy) {
		return applyProtectOnly(cmd, params)
	}
	if params.SandboxPolicy == nil {
		return sandbox.EnforcementOutcome{}, func() {}, nil
	}
	policy := *params.SandboxPolicy
	if len(params.ProtectedPaths) > 0 {
		merged, err := policy.WithProtected(params.ProtectedPaths...)
		if err != nil {
			out := sandbox.EnforcementOutcome{
				PolicyID:    policy.ID,
				Mode:        policy.Mode,
				Backend:     policy.Backend,
				State:       sandbox.EnforcementFailed,
				Diagnostics: []string{"protected paths: " + err.Error()},
				BackendGOOS: runtime.GOOS,
			}
			if params.SandboxOutcomeCallback != nil {
				params.SandboxOutcomeCallback(out)
			}
			return out, func() {}, fmt.Errorf("acp: sandbox protected paths: %w", err)
		}
		policy = merged
	}
	out, cleanup, err := sandbox.ApplyResolved(cmd, policy)
	if cleanup == nil {
		cleanup = func() {}
	}
	if err != nil {
		if params.SandboxOutcomeCallback != nil {
			params.SandboxOutcomeCallback(out)
		}
		return out, cleanup, fmt.Errorf("acp: sandbox apply resolved: %w", err)
	}
	return out, cleanup, nil
}

func requiredPolicy(p *sandbox.ResolvedAccessPolicy) bool {
	return p != nil && p.Mode != sandbox.ConfinementDisabled
}

// applyProtectOnly wraps cmd in the ProtectOnlyProfileID profile. It fails
// closed: a backend that cannot write-protect paths refuses the launch
// rather than starting the agent with them writable.
func applyProtectOnly(cmd *exec.Cmd, params LaunchParams) (sandbox.EnforcementOutcome, func(), error) {
	caps := sandbox.ResolveBackendCapabilities("", sandbox.BackendAuto)
	out := sandbox.EnforcementOutcome{
		PolicyID:     ProtectOnlyProfileID,
		Mode:         sandbox.ConfinementRequired,
		Backend:      caps.Backend,
		BackendGOOS:  caps.GOOS,
		BackendReady: caps.Supported,
	}
	fail := func(state sandbox.EnforcementState, err error) (sandbox.EnforcementOutcome, func(), error) {
		out.State = state
		out.Diagnostics = append(out.Diagnostics, err.Error())
		if params.SandboxOutcomeCallback != nil {
			params.SandboxOutcomeCallback(out)
		}
		return out, func() {}, fmt.Errorf("acp: sandbox protect-only: %w", err)
	}
	if !caps.Supported || !slices.Contains(caps.Capabilities, sandbox.CapWriteProtect) {
		out.Unsupported = []sandbox.Capability{sandbox.CapWriteProtect}
		return fail(sandbox.EnforcementUnsupported, fmt.Errorf("%w: the %s backend on %s cannot write-protect paths", sandbox.ErrUnsupportedPolicy, caps.Backend, caps.GOOS))
	}
	profile := sandbox.Profile{
		ID:             ProtectOnlyProfileID,
		Net:            true,
		Subprocess:     true,
		HostFilesystem: true,
	}
	for _, path := range params.ProtectedPaths {
		if !slices.Contains(profile.FS.Protect, path) {
			profile.FS.Protect = append(profile.FS.Protect, path)
		}
	}
	cleanup, err := sandbox.Apply(cmd, profile, params.Cwd)
	if err != nil {
		state := sandbox.EnforcementFailed
		if errors.Is(err, sandbox.ErrBackendUnavailable) || errors.Is(err, sandbox.ErrUnsupportedPolicy) {
			state = sandbox.EnforcementUnsupported
		}
		return fail(state, err)
	}
	if cleanup == nil {
		cleanup = func() {}
	}
	out.State = sandbox.EnforcementConfigured
	return sandbox.AppliedOutcome(out), cleanup, nil
}

func launchSandboxed(params LaunchParams) bool {
	return params.SandboxPolicy != nil || len(params.ProtectedPaths) > 0
}

func ReportLaunchSandboxStarted(params LaunchParams, out sandbox.EnforcementOutcome) {
	if !launchSandboxed(params) || params.SandboxOutcomeCallback == nil {
		return
	}
	params.SandboxOutcomeCallback(out)
}

func ReportLaunchSandboxStartFailed(params LaunchParams, out sandbox.EnforcementOutcome, err error) {
	if !launchSandboxed(params) || params.SandboxOutcomeCallback == nil {
		return
	}
	if out.State == sandbox.EnforcementApplied {
		out.State = sandbox.EnforcementConfigured
		out.Enforced = false
	}
	if err != nil {
		out.Diagnostics = append(out.Diagnostics, "process start failed: "+err.Error())
	}
	params.SandboxOutcomeCallback(out)
}

// CheckRemoteSandbox reports the only honest outcomes for an ACP endpoint the
// client does not spawn locally. Required confinement is unsupported; explicit
// disabled mode is reported as disabled and allowed.
//
// ProtectedPaths are confinement too: a remote process cannot be kept from
// writing them, so they are refused whatever the policy says.
func CheckRemoteSandbox(params LaunchParams) error {
	if !launchSandboxed(params) {
		return nil
	}
	out := sandbox.EnforcementOutcome{BackendGOOS: runtime.GOOS, BackendReady: false}
	switch {
	case requiredPolicy(params.SandboxPolicy):
		out.PolicyID = params.SandboxPolicy.ID
		out.Mode = params.SandboxPolicy.Mode
		out.Backend = params.SandboxPolicy.Backend
		if len(params.ProtectedPaths) > 0 {
			out.Unsupported = []sandbox.Capability{sandbox.CapWriteProtect}
		}
	case len(params.ProtectedPaths) > 0:
		out.PolicyID = ProtectOnlyProfileID
		out.Mode = sandbox.ConfinementRequired
		out.Unsupported = []sandbox.Capability{sandbox.CapWriteProtect}
	default:
		out.PolicyID = params.SandboxPolicy.ID
		out.Mode = params.SandboxPolicy.Mode
		out.Backend = params.SandboxPolicy.Backend
		out.State = sandbox.EnforcementDisabled
		out.Disabled = true
		out.Diagnostics = append(out.Diagnostics, "confinement explicitly disabled for remote/pre-existing ACP endpoint")
		if params.SandboxOutcomeCallback != nil {
			params.SandboxOutcomeCallback(out)
		}
		return nil
	}
	out.State = sandbox.EnforcementUnsupported
	out.Diagnostics = append(out.Diagnostics, "ACP endpoint is remote or pre-existing; this client cannot verify local OS confinement")
	if params.SandboxOutcomeCallback != nil {
		params.SandboxOutcomeCallback(out)
	}
	return ErrRemoteSandboxUnsupported
}
