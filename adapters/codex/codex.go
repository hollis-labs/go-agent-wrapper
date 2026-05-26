package codex

import (
	"errors"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-providers/provider"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Adapter is the wrapper adapter for Codex's app-server (JSON-RPC
// stdio) runtime. Construct via [New]; pass to
// [github.com/hollis-labs/go-agent-wrapper/wrapper.Config.Adapter].
type Adapter struct {
	binary string
	extra  []string
}

// Option mutates an [Adapter] during [New].
type Option func(*Adapter)

// WithBinary overrides the executable path used by [Adapter.Resolve].
// Empty path (the default) resolves "codex" via PATH at exec time —
// or via the CODEX_CLI_PATH env var, which the underlying go-providers
// adapter consults first. Use this to pin a specific build.
func WithBinary(path string) Option { return func(a *Adapter) { a.binary = path } }

// WithExtraArgs appends additional CLI arguments after the
// `app-server` token. Useful for `--listen` overrides or any other
// per-invocation Codex flags this package does not model directly.
//
// Extra args appear AFTER the `app-server` mode flag so they cannot
// accidentally override transport selection.
func WithExtraArgs(args ...string) Option {
	return func(a *Adapter) { a.extra = append(a.extra, args...) }
}

// New returns a Codex app-server Adapter configured by opts.
func New(opts ...Option) *Adapter {
	a := &Adapter{}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Name implements [adapters.Adapter].
func (*Adapter) Name() string { return "codex" }

// Describe implements [adapters.Adapter]. It advertises jsonrpc-stdio
// as the runtime and the JSON-RPC channel as the event source.
func (*Adapter) Describe() adapters.Descriptor {
	return adapters.Descriptor{
		Provider: "codex",
		Runtime:  "jsonrpc-stdio",
		Channels: []runtimeevents.SourceChannel{runtimeevents.ChannelJSONRPC},
	}
}

// Resolve implements [adapters.Adapter]. It builds the exec [Spec]
// for one Codex app-server invocation, threading through the
// ResolveContext's Cwd and Env and rejecting PTY allocation (JSON-RPC
// stdio is incompatible with a PTY parent).
func (a *Adapter) Resolve(rc adapters.ResolveContext) (adapters.Spec, error) {
	if rc.PTY {
		return adapters.Spec{}, ErrPTYUnsupported
	}

	args := append([]string{"app-server"}, a.extra...)

	binary := a.binary
	if binary == "" {
		binary = "codex"
	}

	return adapters.Spec{
		Binary: binary,
		Args:   args,
		Env:    rc.Env,
		Cwd:    rc.Cwd,
	}, nil
}

// ErrPTYUnsupported is returned by [Adapter.Resolve] when the caller
// requests PTY allocation. Codex JSON-RPC stdio cannot run under a
// PTY parent. Use [errors.Is] to detect.
var ErrPTYUnsupported = errors.New("codex app-server adapter does not support PTY allocation")

// CLIAdapter implements [adapters.RuntimeAdapter] by returning the
// go-providers Codex app-server adapter. The wrapper hands this to
// agentkit/agentsessions.NewFromAdapter with
// Capabilities.JsonRpcStdio=true at session start time.
//
// A fresh adapter is constructed per call so the wrapper can mutate
// per-session state on it without affecting other in-flight sessions.
//
// Note: a custom [WithBinary] path on this wrapper [Adapter] affects
// the [adapters.Spec] returned by Resolve but does NOT override the
// go-providers adapter's own binary lookup at agentsessions
// Prepare/Start time. For the agentkit runtime path the go-providers
// adapter's Detect() (which honors the CODEX_CLI_PATH env var, then
// falls back to PATH) is the authoritative resolver.
func (*Adapter) CLIAdapter() provider.CLIAdapter {
	return provider.NewCodexAdapterAppServer()
}
