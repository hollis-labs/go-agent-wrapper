package opencode

import (
	"errors"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-providers/provider"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Adapter is the wrapper adapter for OpenCode's serve-http (HTTP/SSE)
// runtime. Construct via [New]; pass to
// [github.com/hollis-labs/go-agent-wrapper/wrapper.Config.Adapter].
type Adapter struct {
	binary string
	extra  []string
}

// Option mutates an [Adapter] during [New].
type Option func(*Adapter)

// WithBinary overrides the executable path used by [Adapter.Resolve].
// Empty path (the default) resolves "opencode" via PATH at exec time —
// or via the OPENCODE_CLI_PATH env var, which the underlying
// go-providers adapter consults first. Use this to pin a specific
// build.
func WithBinary(path string) Option { return func(a *Adapter) { a.binary = path } }

// WithExtraArgs appends additional CLI arguments to the serve
// invocation. Useful for overriding `--port` or `--hostname`, or
// passing any other per-invocation OpenCode flags this package does
// not model directly.
//
// Extra args appear AFTER the default `serve --port 0 --hostname
// 127.0.0.1` so callers can rely on the defaults being set when no
// extras are supplied.
func WithExtraArgs(args ...string) Option {
	return func(a *Adapter) { a.extra = append(a.extra, args...) }
}

// New returns an OpenCode serve-http Adapter configured by opts.
func New(opts ...Option) *Adapter {
	a := &Adapter{}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Name implements [adapters.Adapter].
func (*Adapter) Name() string { return "opencode" }

// Describe implements [adapters.Adapter]. It advertises http-sse as
// the runtime and the OpenCode plugin channel as the event source.
func (*Adapter) Describe() adapters.Descriptor {
	return adapters.Descriptor{
		Provider: "opencode",
		Runtime:  "http-sse",
		Channels: []runtimeevents.SourceChannel{runtimeevents.ChannelOpenCodePlugin},
	}
}

// Resolve implements [adapters.Adapter]. It builds the exec [Spec]
// for one OpenCode serve invocation, threading through the
// ResolveContext's Cwd and Env and rejecting PTY allocation (HTTP/SSE
// is incompatible with a PTY parent).
func (a *Adapter) Resolve(rc adapters.ResolveContext) (adapters.Spec, error) {
	if rc.PTY {
		return adapters.Spec{}, ErrPTYUnsupported
	}

	args := append([]string{
		"serve",
		"--port", "0",
		"--hostname", "127.0.0.1",
	}, a.extra...)

	binary := a.binary
	if binary == "" {
		binary = "opencode"
	}

	return adapters.Spec{
		Binary: binary,
		Args:   args,
		Env:    rc.Env,
		Cwd:    rc.Cwd,
	}, nil
}

// ErrPTYUnsupported is returned by [Adapter.Resolve] when the caller
// requests PTY allocation. OpenCode serve-http cannot run under a
// PTY parent. Use [errors.Is] to detect.
var ErrPTYUnsupported = errors.New("opencode serve-http adapter does not support PTY allocation")

// CLIAdapter implements [adapters.RuntimeAdapter] by returning the
// go-providers OpenCode serve-http adapter. The wrapper hands this
// to agentkit/agentsessions.NewFromAdapter with
// Capabilities.ServeHTTP=true at session start time.
//
// A fresh adapter is constructed per call so the wrapper can mutate
// per-session state on it without affecting other in-flight sessions.
//
// Note: a custom [WithBinary] path on this wrapper [Adapter] affects
// the [adapters.Spec] returned by Resolve but does NOT override the
// go-providers adapter's own binary lookup at agentsessions
// Prepare/Start time. For the agentkit runtime path the go-providers
// adapter's Detect() (which honors the OPENCODE_CLI_PATH env var,
// then falls back to PATH) is the authoritative resolver.
func (*Adapter) CLIAdapter() provider.CLIAdapter {
	return provider.NewOpencodeAdapterServeHTTP()
}
