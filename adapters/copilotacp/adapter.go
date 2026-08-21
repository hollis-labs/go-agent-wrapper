package copilotacp

import (
	"errors"
	"strconv"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-providers/provider"
)

// ErrPTYUnsupported is returned by [Adapter.Resolve] when the caller
// requests PTY allocation. ACP over stdio or TCP is incompatible with a
// PTY parent. Use [errors.Is] to detect.
var ErrPTYUnsupported = errors.New("copilotacp: ACP adapter does not support PTY allocation")

// AdapterOption configures an [Adapter] constructed via [New].
type AdapterOption func(*Adapter)

// WithAdapterBinary overrides the `copilot` executable path used by
// both [Adapter.Resolve] and the [Client] this Adapter wraps. Empty
// (the default) resolves "copilot" via PATH (or the COPILOT_CLI_PATH
// env var, consulted by the [adapters.RuntimeAdapter] glue's Detect) at
// spawn time.
func WithAdapterBinary(path string) AdapterOption { return func(a *Adapter) { a.binary = path } }

// WithAdapterTransport selects [adapters.TransportStdio] (the default)
// or [adapters.TransportTCP]. Both are real, live-verified options for
// Copilot CLI's `--acp` mode — see the package doc.
func WithAdapterTransport(t adapters.Transport) AdapterOption {
	return func(a *Adapter) { a.transport = t }
}

// WithAdapterPort sets the TCP port used when transport is
// [adapters.TransportTCP] (`--acp --port <N>`). Zero (the default)
// auto-picks a free local port at Launch time. Ignored for stdio.
func WithAdapterPort(port int) AdapterOption { return func(a *Adapter) { a.port = port } }

// WithAdapterExtraArgs appends additional CLI arguments after `--acp`
// (and `--port <N>` for TCP).
func WithAdapterExtraArgs(args ...string) AdapterOption {
	return func(a *Adapter) { a.extraArgs = append(a.extraArgs, args...) }
}

// Adapter is the wrapper [adapters.Adapter]/[adapters.RuntimeAdapter]
// for GitHub Copilot CLI's native `--acp` mode. Construct via [New];
// pass to [github.com/hollis-labs/go-agent-wrapper/wrapper.Config.Adapter],
// or drive its underlying [Client] directly via [Adapter.Client] — see
// the package doc for which composition this task's real end-to-end
// tests exercise and why.
type Adapter struct {
	client *Client

	binary    string
	transport adapters.Transport
	port      int
	extraArgs []string
}

// New returns a Copilot CLI ACP Adapter configured by opts. Defaults to
// [adapters.TransportStdio].
func New(opts ...AdapterOption) *Adapter {
	a := &Adapter{transport: adapters.TransportStdio}
	for _, opt := range opts {
		opt(a)
	}

	clientOpts := []Option{WithExtraArgs(a.extraArgs...)}
	if a.binary != "" {
		clientOpts = append(clientOpts, WithBinary(a.binary))
	}
	if a.transport == adapters.TransportTCP {
		clientOpts = append(clientOpts, WithPort(a.port))
	}
	a.client = NewClient(a.transport, clientOpts...)
	return a
}

// Name implements [adapters.Adapter].
func (*Adapter) Name() string { return "copilot" }

// Describe implements [adapters.Adapter], via [acp.DescriptorFor] —
// Protocol is always [adapters.ProtocolACP]; Transport reflects however
// this Adapter was configured ([WithAdapterTransport]); Interrupt
// mirrors [Client.InterruptCapability] ([adapters.InterruptTurn],
// verified live — see package doc).
func (a *Adapter) Describe() adapters.Descriptor {
	return acp.DescriptorFor(a.client, "copilot", a.transport)
}

// Resolve implements [adapters.Adapter]. Builds the exec [adapters.Spec]
// for one `copilot --acp` invocation. Informational for the composition
// [Client] drives directly (which spawns/dials itself inside
// [Client.Launch]) — kept accurate regardless so a caller inspecting
// Resolve's output, or driving this Adapter through
// [github.com/hollis-labs/go-agent-wrapper/wrapper.Wrapper.Run]'s
// stdio-only dispatch path, sees the real invocation shape. See the
// package doc's "Wrapper.Run composition" note for what that path does
// and does not do automatically.
func (a *Adapter) Resolve(rc adapters.ResolveContext) (adapters.Spec, error) {
	if rc.PTY {
		return adapters.Spec{}, ErrPTYUnsupported
	}

	binary := a.binary
	if binary == "" {
		binary = "copilot"
	}
	args := []string{"--acp"}
	if a.transport == adapters.TransportTCP {
		args = append(args, "--port", strconv.Itoa(a.port))
	}
	args = append(args, a.extraArgs...)

	return adapters.Spec{
		Binary: binary,
		Args:   args,
		Env:    rc.Env,
		Cwd:    rc.Cwd,
	}, nil
}

// Client returns the [acp.Client] this Adapter wraps, letting a caller
// drive Launch/Prompt/Cancel/Events directly — the composition this
// task's real end-to-end tests use, and the one that actually performs
// the ACP handshake/turn exchange for real. See the package doc.
func (a *Adapter) Client() acp.Client { return a.client }

// CLIAdapter implements [adapters.RuntimeAdapter] by returning a
// [provider.CLIAdapter] shim so this Adapter can, structurally, be
// dispatched through
// [github.com/hollis-labs/go-agent-wrapper/wrapper.Wrapper.Run]'s
// [adapters.ProtocolACP] + [adapters.TransportStdio] runtime_dispatch.go
// entry (task 08), the same composition shape
// wrapper/wrapper_acp_test.go's fake test proved. See the package doc's
// "Wrapper.Run composition" note for the honest limitation this
// implies: BuildArgs/ParseLine cannot drive [Client]'s own
// Launch/Prompt automatically in this composition (no writer is
// available before spawn), so ParseLine does real `session/update`
// translation for whatever a caller writes via
// [github.com/hollis-labs/go-agent-wrapper/wrapper.Wrapper.SendInput],
// but the handshake itself is not performed automatically here.
func (a *Adapter) CLIAdapter() provider.CLIAdapter {
	return &cliAdapterGlue{adapter: a}
}

var (
	_ adapters.Adapter        = (*Adapter)(nil)
	_ adapters.RuntimeAdapter = (*Adapter)(nil)
)
