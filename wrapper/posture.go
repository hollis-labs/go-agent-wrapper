package wrapper

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentlaunch"
	permission "github.com/hollis-labs/go-permission"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-providers/registry"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
)

// ErrPostureConflict is returned by [Wrapper.Run] when Config.PermissionPosture
// and the prepared execution's own posture (the plan's
// Provider.Permission, which its launch flags were mapped from) are both set
// and differ. A prepared launch's flags are the plan's; answering its
// approval requests from a different posture would apply two.
var ErrPostureConflict = errors.New("wrapper: Config.PermissionPosture differs from the prepared execution's posture")

// sessionPosture is the posture the wrapper answers the agent's approval
// requests from: Config.PermissionPosture, else the prepared execution's,
// else go-permission's default.
func sessionPosture(cfg permission.Mode, prepared *agentlaunch.PreparedExecution) (permission.Mode, error) {
	if prepared != nil && prepared.Posture != "" {
		if cfg != "" && cfg != prepared.Posture {
			return "", fmt.Errorf("%w: %q, prepared %q", ErrPostureConflict, cfg, prepared.Posture)
		}
		return prepared.Posture, nil
	}
	if cfg != "" {
		return cfg, nil
	}
	return permission.ModeDefault, nil
}

// nativePosture maps an explicit Config.PermissionPosture onto a native
// launch's flags and environment through the go-providers registry's Posture
// hook (CW-20260930-0138). An empty posture maps to nothing: the launch is
// exactly what it was before the hook existed. The runtime is the adapter's
// Describe().Provider, else its Name(); the mode is the one its
// Protocol/Transport select.
func nativePosture(adapter adapters.Adapter, desc adapters.Descriptor, posture permission.Mode) (registry.PostureLaunch, error) {
	if posture == "" {
		return registry.PostureLaunch{}, nil
	}
	mode, ok := nativeMode(desc)
	if !ok {
		return registry.PostureLaunch{}, fmt.Errorf("%w: no native mode for %s/%s", registry.ErrNoPostureMapping, desc.Protocol, desc.Transport)
	}
	d, ok := registry.Lookup(desc.Provider)
	if !ok {
		d, ok = registry.Lookup(adapter.Name())
	}
	if !ok {
		return registry.PostureLaunch{}, fmt.Errorf("%w: %q is not a registry runtime", registry.ErrNoPostureMapping, adapter.Name())
	}
	return d.PostureFor(posture, mode)
}

func nativeMode(desc adapters.Descriptor) (runtimes.Mode, bool) {
	switch token := legacyRuntimeToken(desc.Protocol, desc.Transport); token {
	case "":
		return "", false
	case RuntimeAdapter:
		return runtimes.ModeSubprocessPerTurn, true
	default:
		return runtimes.Mode(token), true
	}
}

// withPostureArgs returns a copy of cli with args leading its ExtraArgs, which
// a go-providers adapter places at its launch convention's extra-argument
// slot: before "--" and before any variadic flag. The host's adapter is never
// changed. Another adapter type cannot be given flags safely, the same rule
// launch.Select applies to Binary and ExtraArgs.
func withPostureArgs(cli provider.CLIAdapter, args []string) (provider.CLIAdapter, error) {
	if len(args) == 0 {
		return cli, nil
	}
	lead := func(extra []string) []string { return append(slices.Clone(args), extra...) }
	switch a := cli.(type) {
	case *provider.ClaudeAdapter:
		c := *a
		c.ExtraArgs = lead(a.ExtraArgs)
		return &c, nil
	case *provider.CodexAdapter:
		c := *a
		c.ExtraArgs = lead(a.ExtraArgs)
		return &c, nil
	case *provider.OpencodeAdapter:
		c := *a
		c.ExtraArgs = lead(a.ExtraArgs)
		return &c, nil
	case *provider.AntigravityAdapter:
		c := *a
		c.ExtraArgs = lead(a.ExtraArgs)
		return &c, nil
	}
	return nil, fmt.Errorf("wrapper: Config.PermissionPosture: launch flags need a go-providers adapter, not %T", cli)
}

// withPostureEnv sets each of set over env, a KEY=VALUE list, replacing an
// entry of the same name.
func withPostureEnv(env []string, set map[string]string) []string {
	if len(set) == 0 {
		return env
	}
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if _, replaced := set[name]; !replaced {
			out = append(out, kv)
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		out = append(out, name+"="+set[name])
	}
	return out
}
