package acp

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"

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

// PrepareLaunchSandbox applies a resolved local process policy to cmd before
// Start. It invokes SandboxOutcomeCallback for setup failures; callers must call
// ReportLaunchSandboxStarted after a successful Start, or
// ReportLaunchSandboxStartFailed if Start itself fails.
func PrepareLaunchSandbox(cmd *exec.Cmd, params LaunchParams) (sandbox.EnforcementOutcome, func(), error) {
	if params.SandboxPolicy == nil {
		return sandbox.EnforcementOutcome{}, func() {}, nil
	}
	out, cleanup, err := sandbox.ApplyResolved(cmd, *params.SandboxPolicy)
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

func ReportLaunchSandboxStarted(params LaunchParams, out sandbox.EnforcementOutcome) {
	if params.SandboxPolicy == nil || params.SandboxOutcomeCallback == nil {
		return
	}
	params.SandboxOutcomeCallback(out)
}

func ReportLaunchSandboxStartFailed(params LaunchParams, out sandbox.EnforcementOutcome, err error) {
	if params.SandboxPolicy == nil || params.SandboxOutcomeCallback == nil {
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
func CheckRemoteSandbox(params LaunchParams) error {
	if params.SandboxPolicy == nil {
		return nil
	}
	out := sandbox.EnforcementOutcome{
		PolicyID:     params.SandboxPolicy.ID,
		Mode:         params.SandboxPolicy.Mode,
		Backend:      params.SandboxPolicy.Backend,
		BackendGOOS:  runtime.GOOS,
		BackendReady: false,
	}
	if params.SandboxPolicy.Mode == sandbox.ConfinementDisabled {
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
