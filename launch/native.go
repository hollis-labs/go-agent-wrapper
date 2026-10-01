package launch

import (
	"fmt"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-providers/registry"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
)

// nativeSpec is how the wrapper drives one native (runtime, mode): the wire
// protocol its runtime dispatch keys on. An empty protocol is the
// subprocess-per-turn adapter runtime. Which go-providers adapter serves the
// pair is go-providers' provider.NewAdapter, the one constructor table shared
// with agentkit's planting path.
type nativeSpec struct {
	protocol  adapters.Protocol
	transport adapters.Transport
	interrupt adapters.InterruptCapability
	channel   runtimeevents.SourceChannel
}

func perTurn(channel runtimeevents.SourceChannel) nativeSpec {
	return nativeSpec{interrupt: adapters.InterruptProcess, channel: channel}
}

// nativeSpecs is every native (runtime, mode) the wrapper launches.
// Claude's PTY is the registry's (and provider.NewAdapter builds it) but
// deliberately absent: it is a human TUI path, not one the wrapper drives.
var nativeSpecs = map[Key]nativeSpec{
	{runtimes.Claude, runtimes.ModeStreamingStdio}: {
		protocol: adapters.ProtocolClaudeStreamJSON, transport: adapters.TransportStdio,
		interrupt: adapters.InterruptProcess, channel: runtimeevents.ChannelClaudeStreamJSON,
	},
	{runtimes.Claude, runtimes.ModeSubprocessPerTurn}: perTurn(runtimeevents.ChannelClaudeStreamJSON),
	{runtimes.Codex, runtimes.ModeJSONRPCStdio}: {
		protocol: adapters.ProtocolCodexAppServer, transport: adapters.TransportStdio,
		interrupt: adapters.InterruptProcess, channel: runtimeevents.ChannelJSONRPC,
	},
	{runtimes.Codex, runtimes.ModeSubprocessPerTurn}:    perTurn(runtimeevents.ChannelStdio),
	{runtimes.OpenCode, runtimes.ModeSubprocessPerTurn}: perTurn(runtimeevents.ChannelStdio),
	{runtimes.OpenCode, runtimes.ModeHTTPSSE}: {
		protocol: adapters.ProtocolOpenCodeNative, transport: adapters.TransportHTTPSSE,
		interrupt: adapters.InterruptTurn, channel: runtimeevents.ChannelOpenCodePlugin,
	},
	// agy -p: one process per turn; its stream-json stdin mode does not
	// report turn ends reliably.
	{runtimes.Antigravity, runtimes.ModeSubprocessPerTurn}: perTurn(runtimeevents.ChannelStdio),
}

var nativeFactories = func() map[Key]factory {
	out := make(map[Key]factory, len(nativeSpecs))
	for k, spec := range nativeSpecs {
		out[k] = func(sel Selection) (adapters.Adapter, error) { return newNative(k, spec, sel) }
	}
	return out
}()

func newNative(key Key, spec nativeSpec, sel Selection) (*nativeAdapter, error) {
	if _, err := provider.NewAdapter(key.Runtime, key.Mode); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupportedSelection, err)
	}
	base := func() provider.CLIAdapter {
		cli, _ := provider.NewAdapter(key.Runtime, key.Mode)
		if claude, ok := cli.(*provider.ClaudeAdapter); ok && sel.DeveloperMode {
			claude.SkipPermissions = true
		}
		return cli
	}
	if sel.CLIAdapter != nil {
		cli := sel.CLIAdapter
		base = func() provider.CLIAdapter { return cli }
	}
	// Probe once so a host adapter that cannot take the Binary/ExtraArgs
	// fails Select, not the first spawn.
	if _, err := configure(base(), sel.Binary, sel.ExtraArgs); err != nil {
		return nil, err
	}
	return &nativeAdapter{
		runtime: key.Runtime,
		descriptor: adapters.Descriptor{
			Provider: string(key.Runtime), Protocol: spec.protocol, Transport: spec.transport,
			Interrupt: spec.interrupt,
			Channels:  []runtimeevents.SourceChannel{spec.channel},
			Delivery:  adapters.DeliveryCapabilitiesForRuntime(string(key.Runtime), spec.protocol, spec.transport, spec.interrupt, false),
		},
		cliFactory: func() provider.CLIAdapter {
			cli, _ := configure(base(), sel.Binary, sel.ExtraArgs)
			return cli
		},
	}, nil
}

// configure sets the selection's Binary and ExtraArgs on the adapter's own
// fields, so the adapter the wrapper hands agentsessions is the go-providers
// adapter itself, never a wrapper that would hide its optional interfaces
// (EventParser, SessionLostClassifier, AuthFailureClassifier, Preflighter,
// SessionResumeVerifier, BootDirProvider). A built-in adapter is copied, so
// a host's adapter is never mutated; ExtraArgs land at the convention's extra
// slot (go-providers BuildArgs). A custom adapter type cannot take them and
// is ErrInvalidSelection unless neither is set.
func configure(cli provider.CLIAdapter, binary string, extra []string) (provider.CLIAdapter, error) {
	if binary == "" && len(extra) == 0 {
		return cli, nil
	}
	switch typed := cli.(type) {
	case *provider.ClaudeAdapter:
		c := *typed
		c.Binary, c.ExtraArgs = pin(c.Binary, binary), appendArgs(c.ExtraArgs, extra)
		return &c, nil
	case *provider.CodexAdapter:
		c := *typed
		c.Binary, c.ExtraArgs = pin(c.Binary, binary), appendArgs(c.ExtraArgs, extra)
		return &c, nil
	case *provider.OpencodeAdapter:
		c := *typed
		c.Binary, c.ExtraArgs = pin(c.Binary, binary), appendArgs(c.ExtraArgs, extra)
		return &c, nil
	case *provider.AntigravityAdapter:
		c := *typed
		c.Binary, c.ExtraArgs = pin(c.Binary, binary), appendArgs(c.ExtraArgs, extra)
		return &c, nil
	default:
		return nil, fmt.Errorf("%w: Binary/ExtraArgs need a go-providers adapter, not %T; set them on the adapter itself", ErrInvalidSelection, cli)
	}
}

func pin(current, binary string) string {
	if binary != "" {
		return binary
	}
	return current
}

func appendArgs(current, extra []string) []string {
	return append(append([]string(nil), current...), extra...)
}

// validateCLIAdapter checks a host-supplied adapter against the selection: its
// name must resolve to the runtime, and a built-in go-providers adapter's
// shape must match the mode. A custom implementation is responsible for
// matching the mode itself, because its internal shape cannot be inspected.
func validateCLIAdapter(key Key, cli provider.CLIAdapter) error {
	if d, ok := registry.Lookup(cli.Name()); !ok || d.ID != key.Runtime {
		return fmt.Errorf("%w: CLIAdapter.Name=%q does not match runtime %s", ErrInvalidSelection, cli.Name(), key.Runtime)
	}
	valid := true
	switch typed := cli.(type) {
	case *provider.ClaudeAdapter:
		streaming := typed.InputMode == "stream-json" && !typed.Bare && !typed.PTY
		perTurn := !streaming && !typed.PTY
		valid = (key.Mode == runtimes.ModeStreamingStdio && streaming) || (key.Mode == runtimes.ModeSubprocessPerTurn && perTurn)
	case *provider.CodexAdapter:
		appServer := typed.Mode == "app-server"
		valid = (key.Mode == runtimes.ModeJSONRPCStdio && appServer) || (key.Mode == runtimes.ModeSubprocessPerTurn && !appServer)
	case *provider.OpencodeAdapter:
		serveHTTP := typed.Mode == "serve-http"
		valid = (key.Mode == runtimes.ModeHTTPSSE && serveHTTP) || (key.Mode == runtimes.ModeSubprocessPerTurn && !serveHTTP)
	}
	if !valid {
		return fmt.Errorf("%w: %T does not match %s", ErrInvalidSelection, cli, key)
	}
	return nil
}

// nativeAdapter is the adapter Select returns for a native mode. Its
// go-providers CLIAdapter, configured with the selection's Binary and
// ExtraArgs, is what the wrapper hands agentkit/agentsessions, unwrapped.
type nativeAdapter struct {
	runtime    runtimes.ID
	descriptor adapters.Descriptor
	cliFactory func() provider.CLIAdapter
}

func (a *nativeAdapter) Name() string { return string(a.runtime) }

func (a *nativeAdapter) Describe() adapters.Descriptor {
	desc := a.descriptor
	desc.Channels = append([]runtimeevents.SourceChannel(nil), desc.Channels...)
	desc.Delivery = desc.Delivery.Clone()
	return desc
}

func (a *nativeAdapter) Resolve(rc adapters.ResolveContext) (adapters.Spec, error) {
	cli := a.CLIAdapter()
	binary, ok := cli.Detect()
	if !ok || binary == "" {
		binary = string(a.runtime)
	}
	return adapters.Spec{
		Binary: binary,
		Args:   cli.BuildArgs("", "", ""),
		Env:    rc.Env,
		Cwd:    rc.Cwd,
	}, nil
}

// CLIAdapter returns the go-providers adapter the wrapper hands
// agentsessions. A Select-built adapter, or a host's adapter configured with
// Binary/ExtraArgs, is a fresh value per call; a host-supplied CLIAdapter
// with neither is returned as-is, the same instance every call, as the host
// passed it.
func (a *nativeAdapter) CLIAdapter() provider.CLIAdapter { return a.cliFactory() }

var _ adapters.RuntimeAdapter = (*nativeAdapter)(nil)
