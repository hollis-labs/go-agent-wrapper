package launch

import (
	"fmt"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-providers/registry"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
)

// nativeSpec is how the wrapper drives one native (runtime, mode): the
// go-providers adapter it wraps (and the Claude developer variant) and the
// wire protocol the wrapper's runtime dispatch keys on. An empty protocol
// is the subprocess-per-turn adapter runtime.
type nativeSpec struct {
	adapter   func() provider.CLIAdapter
	developer func() provider.CLIAdapter
	protocol  adapters.Protocol
	transport adapters.Transport
	interrupt adapters.InterruptCapability
	channel   runtimeevents.SourceChannel
}

func perTurn(adapter, developer func() provider.CLIAdapter, channel runtimeevents.SourceChannel) nativeSpec {
	return nativeSpec{adapter: adapter, developer: developer, interrupt: adapters.InterruptProcess, channel: channel}
}

// nativeSpecs is every native (runtime, mode) the wrapper launches.
// Claude's PTY is the registry's but deliberately absent: it is a human TUI
// path, not one the wrapper drives.
var nativeSpecs = map[Key]nativeSpec{
	{runtimes.Claude, runtimes.ModeStreamingStdio}: {
		adapter:   func() provider.CLIAdapter { return provider.NewClaudeAdapterStreamingStdio() },
		developer: func() provider.CLIAdapter { return provider.NewClaudeAdapterDevStreamingStdio() },
		protocol:  adapters.ProtocolClaudeStreamJSON, transport: adapters.TransportStdio,
		interrupt: adapters.InterruptProcess, channel: runtimeevents.ChannelClaudeStreamJSON,
	},
	{runtimes.Claude, runtimes.ModeSubprocessPerTurn}: perTurn(
		func() provider.CLIAdapter { return provider.NewClaudeAdapter() },
		func() provider.CLIAdapter { return provider.NewClaudeAdapterDev() },
		runtimeevents.ChannelClaudeStreamJSON),
	{runtimes.Codex, runtimes.ModeJSONRPCStdio}: {
		adapter:  func() provider.CLIAdapter { return provider.NewCodexAdapterAppServer() },
		protocol: adapters.ProtocolCodexAppServer, transport: adapters.TransportStdio,
		interrupt: adapters.InterruptProcess, channel: runtimeevents.ChannelJSONRPC,
	},
	{runtimes.Codex, runtimes.ModeSubprocessPerTurn}: perTurn(
		func() provider.CLIAdapter { return provider.NewCodexAdapter() }, nil, runtimeevents.ChannelStdio),
	{runtimes.OpenCode, runtimes.ModeSubprocessPerTurn}: perTurn(
		func() provider.CLIAdapter { return provider.NewOpencodeAdapter() }, nil, runtimeevents.ChannelStdio),
	{runtimes.OpenCode, runtimes.ModeHTTPSSE}: {
		adapter:  func() provider.CLIAdapter { return provider.NewOpencodeAdapterServeHTTP() },
		protocol: adapters.ProtocolOpenCodeNative, transport: adapters.TransportHTTPSSE,
		interrupt: adapters.InterruptTurn, channel: runtimeevents.ChannelOpenCodePlugin,
	},
	// agy -p: one process per turn; its stream-json stdin mode does not
	// report turn ends reliably.
	{runtimes.Antigravity, runtimes.ModeSubprocessPerTurn}: perTurn(
		func() provider.CLIAdapter { return provider.NewAntigravityAdapter() }, nil, runtimeevents.ChannelStdio),
}

var nativeFactories = func() map[Key]factory {
	out := make(map[Key]factory, len(nativeSpecs))
	for k, spec := range nativeSpecs {
		out[k] = func(sel Selection) (adapters.Adapter, error) { return newNative(k, spec, sel), nil }
	}
	return out
}()

func newNative(key Key, spec nativeSpec, sel Selection) *nativeAdapter {
	cliFactory := spec.adapter
	if sel.DeveloperMode && spec.developer != nil {
		cliFactory = spec.developer
	}
	if sel.CLIAdapter != nil {
		cli := sel.CLIAdapter
		cliFactory = func() provider.CLIAdapter { return cli }
	}
	return &nativeAdapter{
		runtime: key.Runtime,
		descriptor: adapters.Descriptor{
			Provider: string(key.Runtime), Protocol: spec.protocol, Transport: spec.transport,
			Interrupt: spec.interrupt,
			Channels:  []runtimeevents.SourceChannel{spec.channel},
			Delivery:  adapters.DeliveryCapabilitiesForRuntime(string(key.Runtime), spec.protocol, spec.transport, spec.interrupt, false),
		},
		cliFactory: cliFactory,
		binary:     sel.Binary,
		extraArgs:  sel.ExtraArgs,
	}
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
// go-providers CLIAdapter is what the wrapper hands agentkit/agentsessions.
type nativeAdapter struct {
	runtime    runtimes.ID
	descriptor adapters.Descriptor
	cliFactory func() provider.CLIAdapter
	binary     string
	extraArgs  []string
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

func (a *nativeAdapter) CLIAdapter() provider.CLIAdapter {
	return &configuredCLIAdapter{
		CLIAdapter: a.cliFactory(),
		binary:     a.binary,
		extraArgs:  append([]string(nil), a.extraArgs...),
	}
}

// configuredCLIAdapter decorates a go-providers adapter with the selection's
// Binary pin and ExtraArgs.
type configuredCLIAdapter struct {
	provider.CLIAdapter
	binary    string
	extraArgs []string
}

func (a *configuredCLIAdapter) BuildArgs(prompt, systemPrompt, cliSessionID string) []string {
	args := a.CLIAdapter.BuildArgs(prompt, systemPrompt, cliSessionID)
	return append(append([]string(nil), args...), a.extraArgs...)
}

func (a *configuredCLIAdapter) Detect() (string, bool) {
	if a.binary != "" {
		return a.binary, true
	}
	return a.CLIAdapter.Detect()
}

var _ adapters.RuntimeAdapter = (*nativeAdapter)(nil)
