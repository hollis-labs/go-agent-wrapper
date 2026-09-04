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
// or drive its underlying [Client] directly via [Adapter.Client]. Wrapper owns
// the protocol lifecycle through [Adapter.ACPClient] for both transports.
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
// for one `copilot --acp` invocation. It remains useful for inspection even
// though Wrapper.Run obtains a fresh protocol client through ACPClient.
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
// drive Launch/Prompt/Cancel/Events directly. Wrapper callers normally do not
// need it; Wrapper.Run obtains a fresh client through ACPClient.
func (a *Adapter) Client() acp.Client { return a.client }

// ACPClient implements [acp.ClientAdapter]. It returns a fresh client so one
// Adapter value can safely launch successive wrapper sessions.
func (a *Adapter) ACPClient() acp.Client {
	clientOpts := []Option{WithExtraArgs(a.extraArgs...)}
	if a.binary != "" {
		clientOpts = append(clientOpts, WithBinary(a.binary))
	}
	if a.transport == adapters.TransportTCP {
		clientOpts = append(clientOpts, WithPort(a.port))
	}
	return NewClient(a.transport, clientOpts...)
}

// CLIAdapter implements [adapters.RuntimeAdapter] by returning a
// [provider.CLIAdapter] compatibility shim. Wrapper.Run selects ACPClient for
// ProtocolACP; BuildArgs/ParseLine remain available to legacy callers and
// ParseLine continues to share the real session/update translator.
func (a *Adapter) CLIAdapter() provider.CLIAdapter {
	return &cliAdapterGlue{adapter: a}
}

var (
	_ adapters.Adapter        = (*Adapter)(nil)
	_ adapters.RuntimeAdapter = (*Adapter)(nil)
	_ acp.ClientAdapter       = (*Adapter)(nil)
)
