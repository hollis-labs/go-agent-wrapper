package adapters

import (
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Adapter is the provider-integration contract. One Adapter instance
// represents one CLI agent (Claude Code, Codex app-server, OpenCode,
// custom in-house agent, ...).
type Adapter interface {
	// Name returns a short stable identifier ("claude", "codex",
	// "opencode"). Used in logs, configuration, and policy rule keys.
	Name() string

	// Describe returns the static capability declaration for this
	// adapter — which provider it integrates, which runtime channel
	// the wrapper should use, and which observation channels it
	// supports.
	Describe() Descriptor

	// Resolve materializes a Spec for one invocation. It receives the
	// ResolveContext describing this session's planted boot dir,
	// per-app env, and configured user settings, and returns the exec
	// shape (Binary, Args, Env, Cwd).
	Resolve(ResolveContext) (Spec, error)
}

// Descriptor is an Adapter's static capability declaration.
type Descriptor struct {
	// Provider is the upstream agent identity ("claude", "codex",
	// "opencode"). Mirrored into [runtimeevents.Process.Provider].
	Provider string

	// Runtime is the transport the wrapper should use for this provider
	// ("pty", "streaming-stdio", "jsonrpc-stdio", "http-sse"). Mirrored
	// into [runtimeevents.Process.Runtime].
	Runtime string

	// Channels lists the runtime-event source channels this adapter
	// can produce when the wrapper attaches its observer. The wrapper
	// uses this to decide whether semantic policy modes (rewrite,
	// block, approval) are available or whether it must fall back to
	// observe-only.
	Channels []runtimeevents.SourceChannel
}

// ResolveContext gives an Adapter everything it needs to produce a Spec
// for one invocation. The wrapper builds this from the session's
// planted boot dir, the caller's Config, and the surrounding environment.
type ResolveContext struct {
	// BootDir is the per-session boot directory the planter populated
	// (or "" if no planting was configured).
	BootDir string

	// Cwd is the working directory the wrapper will run the process in.
	Cwd string

	// Env is the base environment the wrapper will hand to the process.
	// Adapter.Resolve may add, override, or remove entries in the
	// returned Spec.
	Env []string

	// PTY indicates whether the wrapper plans to allocate a PTY.
	// Adapters that need to flip CLI flags based on TTY-ness consult
	// this.
	PTY bool

	// AppHints carries per-app configuration the adapter declared
	// interest in (model selection, MCP server list, hook plugin
	// settings, ...). Keys and values are adapter-defined.
	AppHints map[string]string
}

// Spec describes the resolved exec shape for one invocation.
type Spec struct {
	// Binary is the absolute path to the CLI executable.
	Binary string

	// Args are the command-line arguments, not including Binary.
	Args []string

	// Env is the environment to pass to the child. Nil means inherit
	// ResolveContext.Env unchanged.
	Env []string

	// Cwd is the working directory. "" means inherit
	// ResolveContext.Cwd.
	Cwd string
}
