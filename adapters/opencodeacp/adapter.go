package opencodeacp

import (
	"errors"
	"os/exec"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider"
)

// Adapter is the wrapper adapter for OpenCode's native ACP subprocess
// mode (`opencode acp`). Construct via [New]; pass to
// [github.com/hollis-labs/go-agent-wrapper/wrapper.Config.Adapter].
//
// Name() deliberately returns "opencode-acp", distinct from the existing
// native adapters/opencode package's "opencode" — both represent the
// same upstream agent (Descriptor.Provider is "opencode" for both, per
// [acp.DescriptorFor]'s providerName convention) but over structurally
// different wire protocols, and Adapter.Name()'s own doc comment
// ("used in logs, configuration, and policy rule keys") calls for
// distinct identifiers wherever two adapters could otherwise collide in
// those keyspaces.
type Adapter struct {
	binary    string
	extraArgs []string
}

// Option mutates an [Adapter] during [New].
type Option func(*Adapter)

// WithBinary overrides the executable path used by [Adapter.Resolve] and
// by the [Client] this Adapter constructs. Empty (the default) resolves
// "opencode" via the OPENCODE_CLI_PATH env var, then PATH, at Launch
// time — mirroring adapters/opencode's and adapters/codex's own
// env-var-override convention (same underlying binary as the native
// adapter).
func WithBinary(path string) Option { return func(a *Adapter) { a.binary = path } }

// WithExtraArgs appends additional CLI arguments after the `acp` token.
func WithExtraArgs(args ...string) Option {
	return func(a *Adapter) { a.extraArgs = append(a.extraArgs, args...) }
}

// New returns an OpenCode ACP Adapter configured by opts.
func New(opts ...Option) *Adapter {
	a := &Adapter{}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// newClient builds a fresh [Client] using this Adapter's configured
// binary/extraArgs overrides. Used by both [Adapter.Describe] (to answer
// InterruptCapability, a static fact independent of any live session —
// see [Client.InterruptCapability]'s own doc comment) and
// [Adapter.CLIAdapter] (a distinct instance per call, so the wrapper can
// mutate per-session state without affecting other in-flight sessions —
// same convention adapters/opencode and adapters/codex already use for
// their own CLIAdapter()).
func (a *Adapter) newClient() *Client {
	var opts []ClientOption
	if a.binary != "" {
		opts = append(opts, WithClientBinary(a.binary))
	}
	if len(a.extraArgs) > 0 {
		opts = append(opts, WithClientExtraArgs(a.extraArgs...))
	}
	return NewClient(opts...)
}

// Name implements [adapters.Adapter].
func (*Adapter) Name() string { return "opencode-acp" }

// Describe implements [adapters.Adapter] via [acp.DescriptorFor],
// threading a fresh, unlaunched [Client]'s InterruptCapability() through
// — [adapters.InterruptTurn], verified live against opencode 1.15.6 (see
// package doc for the measured cancel latency mid-generation).
func (a *Adapter) Describe() adapters.Descriptor {
	return acp.DescriptorFor(a.newClient(), "opencode", adapters.TransportStdio)
}

// Resolve implements [adapters.Adapter]. It builds the informational
// exec [adapters.Spec] for one `opencode acp` invocation. This mirrors
// adapters/codex's and adapters/opencode's own Resolve() — informational
// only on the agentkit dispatch path (agentkit constructs its own argv
// via [Adapter.CLIAdapter]'s BuildArgs, not via Resolve) — but is the
// authoritative shape [Client.Launch] itself actually spawns.
func (a *Adapter) Resolve(rc adapters.ResolveContext) (adapters.Spec, error) {
	if rc.PTY {
		return adapters.Spec{}, ErrPTYUnsupported
	}
	binary := a.binary
	if binary == "" {
		binary = "opencode"
	}
	args := append([]string{"acp"}, a.extraArgs...)
	return adapters.Spec{
		Binary: binary,
		Args:   args,
		Env:    rc.Env,
		Cwd:    rc.Cwd,
	}, nil
}

// ErrPTYUnsupported is returned by [Adapter.Resolve] when the caller
// requests PTY allocation. ACP is JSON-RPC 2.0 over stdio, incompatible
// with a PTY parent — same rationale adapters/codex and adapters/opencode
// already document for their own non-PTY runtimes.
var ErrPTYUnsupported = errors.New("opencodeacp: adapter does not support PTY allocation")

// CLIAdapter implements [adapters.RuntimeAdapter]. It returns a fresh
// [cliAdapter] — go-providers' provider.CLIAdapter shape, so a
// wrapper.Wrapper.Run() caller dispatches through agentkit's jsonrpc-
// stdio runtime (Protocol: acp, Transport: stdio → Capabilities.
// JsonRpcStdio, per go-agent-wrapper's runtime_dispatch.go) without
// ErrUnknownRuntime.
//
// This bridge deliberately mirrors adapters/codex's own
// provider.NewCodexAdapterAppServer() shape exactly: real Detect/
// BuildArgs (so the ONE real `opencode acp` process agentkit spawns is
// the genuine article, not a duplicate) and a pass-through ParseLine
// returning (nil, nil) — see the package doc's "Client ownership of the
// subprocess" section for why. Real, live-verified ACP driving for this
// task goes through [Client] directly (used standalone, not via this
// bridge) — [cliAdapter] exists for interface completeness and dispatch-
// table compatibility, matching the one other jsonrpc-stdio adapter this
// repo already ships.
func (a *Adapter) CLIAdapter() provider.CLIAdapter {
	binary := a.binary
	extraArgs := append([]string(nil), a.extraArgs...)
	return &cliAdapter{binary: binary, extraArgs: extraArgs}
}

// cliAdapter implements go-providers' provider.CLIAdapter for the
// `opencode acp` exec shape. See [Adapter.CLIAdapter]'s doc comment for
// why ParseLine is a deliberate pass-through, not a real ACP parser.
type cliAdapter struct {
	binary    string
	extraArgs []string
}

func (a *cliAdapter) Name() string { return "opencode-acp" }

// Detect resolves the real `opencode` binary — an explicit [WithBinary]
// override first, then the OPENCODE_CLI_PATH env var (same convention
// [Client.resolveBinary] and adapters/opencode's own Detect() use), then
// a real PATH lookup. Unlike [Client.resolveBinary] (which defers the
// existence check to exec.Command's own failure at Launch time), this
// Detect() performs a genuine exec.LookPath so
// jsonRpcStdioRuntime.Prepare's BinaryRequired check reports "not found"
// honestly rather than always claiming ok=true.
func (a *cliAdapter) Detect() (string, bool) {
	if a.binary != "" {
		return a.binary, true
	}
	if p := clientBinaryEnvOverride(); p != "" {
		return p, true
	}
	p, err := exec.LookPath("opencode")
	if err != nil {
		return "", false
	}
	return p, true
}

// BuildArgs implements provider.CLIAdapter. Returns the real `acp`
// subcommand argv — no client interaction here (unlike a naive glue that
// would try to trigger [Client.Launch] from this call): see the package
// doc for why this bridge does not attempt to drive a real ACP session
// through this path.
func (a *cliAdapter) BuildArgs(_, _, _ string) []string {
	return append([]string{"acp"}, a.extraArgs...)
}

// ParseLine implements provider.CLIAdapter as a deliberate pass-through
// — see [Adapter.CLIAdapter]'s doc comment.
func (a *cliAdapter) ParseLine([]byte) ([]llmtypes.StreamEvent, error) {
	return nil, nil
}

var (
	_ adapters.Adapter        = (*Adapter)(nil)
	_ adapters.RuntimeAdapter = (*Adapter)(nil)
	_ provider.CLIAdapter     = (*cliAdapter)(nil)
)
