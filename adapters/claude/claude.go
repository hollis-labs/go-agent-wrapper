package claude

import (
	"errors"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-providers/provider"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Adapter is the wrapper adapter for Claude Code's streaming-stdio
// runtime. Construct via [New]; pass to [github.com/hollis-labs/go-agent-wrapper/wrapper.Config.Adapter].
type Adapter struct {
	binary string
	extra  []string
}

// Option mutates an [Adapter] during [New].
type Option func(*Adapter)

// WithBinary overrides the executable path used by [Adapter.Resolve].
// Empty path (the default) resolves "claude" via PATH at exec time.
// Use this to pin a specific build (a homebrew install, a CI runner
// pinned to a known version, or a developer's local checkout).
func WithBinary(path string) Option { return func(a *Adapter) { a.binary = path } }

// WithExtraArgs appends additional CLI arguments to the streaming-stdio
// invocation. Useful for passing model selection, system-prompt-file
// paths, or any other per-invocation Claude flags this package does
// not model directly.
//
// Extra args appear AFTER the streaming-stdio core flags so they
// cannot accidentally override transport configuration.
func WithExtraArgs(args ...string) Option {
	return func(a *Adapter) { a.extra = append(a.extra, args...) }
}

// New returns a Claude streaming-stdio Adapter configured by opts.
func New(opts ...Option) *Adapter {
	a := &Adapter{}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Name implements [adapters.Adapter].
func (*Adapter) Name() string { return "claude" }

// Describe implements [adapters.Adapter]. It advertises streaming-stdio
// as the runtime and the Claude stream-JSON channel as the event
// source. Hooks may add the [runtimeevents.ChannelHook] channel at
// runtime depending on the planted hook configuration; that's recorded
// at hook-plant time, not here.
func (*Adapter) Describe() adapters.Descriptor {
	return adapters.Descriptor{
		Provider: "claude",
		Runtime:  "streaming-stdio",
		Channels: []runtimeevents.SourceChannel{runtimeevents.ChannelClaudeStreamJSON},
	}
}

// Resolve implements [adapters.Adapter]. It builds the exec [Spec] for
// one Claude streaming-stdio invocation, threading through the
// ResolveContext's Cwd and Env and rejecting PTY allocation (Claude's
// streaming-stdio mode is incompatible with a PTY parent).
func (a *Adapter) Resolve(rc adapters.ResolveContext) (adapters.Spec, error) {
	if rc.PTY {
		return adapters.Spec{}, ErrPTYUnsupported
	}

	args := append([]string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}, a.extra...)

	binary := a.binary
	if binary == "" {
		binary = "claude"
	}

	return adapters.Spec{
		Binary: binary,
		Args:   args,
		Env:    rc.Env,
		Cwd:    rc.Cwd,
	}, nil
}

// ErrPTYUnsupported is returned by [Adapter.Resolve] when the caller
// requests PTY allocation. Claude streaming-stdio cannot run under a
// PTY parent — use a PTY-shaped Claude adapter instead. Use
// [errors.Is] to detect.
var ErrPTYUnsupported = errors.New("claude streaming-stdio adapter does not support PTY allocation")

// CLIAdapter implements [adapters.RuntimeAdapter] by returning the
// go-providers Claude streaming-stdio adapter. The wrapper hands this
// to agentkit/agentsessions.NewFromAdapter with
// Capabilities.StreamingStdio=true at session start time.
//
// A fresh adapter is constructed per call so the wrapper can mutate
// per-session state (resume IDs, bare-mode injection paths) on it
// without affecting other in-flight sessions.
//
// Note: a custom [WithBinary] path on this wrapper [Adapter] affects
// the [Spec] returned by Resolve but does NOT override the
// go-providers adapter's own binary lookup at agentsessions
// Prepare/Start time. The two binary-lookup paths are independent;
// for the agentkit runtime path the go-providers Detect() is the
// authoritative resolver.
func (*Adapter) CLIAdapter() provider.CLIAdapter {
	return provider.NewClaudeAdapterStreamingStdio()
}
