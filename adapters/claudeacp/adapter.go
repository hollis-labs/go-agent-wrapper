package claudeacp

import (
	"errors"
	"os/exec"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider"
)

// Adapter is the wrapper adapter for Claude driven through the pinned
// ACP bridge, `@agentclientprotocol/claude-agent-acp` (npm, spawned via
// npx by default — see the package doc's "Node.js/npm/npx runtime
// requirement" section). Construct via [New]; pass to
// [github.com/hollis-labs/go-agent-wrapper/wrapper.Config.Adapter].
//
// Name() deliberately returns "claude-acp", distinct from the existing
// native adapters/claude package's "claude" — both represent the same
// upstream agent (Descriptor.Provider is "claude" for both, per
// [acp.DescriptorFor]'s providerName convention) but over structurally
// different wire protocols (Claude's own native stream-json vs. this
// package's bridge-mediated ACP), and Adapter.Name()'s own doc comment
// ("used in logs, configuration, and policy rule keys") calls for
// distinct identifiers wherever two adapters could otherwise collide in
// those keyspaces — same convention [adapters/opencodeacp]'s
// "opencode-acp" already establishes.
type Adapter struct {
	npxBinary     string
	bridgePackage string
	directBinary  string
	extraArgs     []string
}

// Option mutates an [Adapter] during [New].
type Option func(*Adapter)

// WithNpxBinary overrides the `npx` executable path used by the [Client]
// this Adapter constructs, when not bypassing npx via [WithDirectBinary].
// See [WithClientNpxBinary]'s doc comment for the default resolution.
func WithNpxBinary(path string) Option { return func(a *Adapter) { a.npxBinary = path } }

// WithBridgePackage overrides the npm package spec resolved via
// `npx -y <spec>` — e.g. to pin an exact bridge version. See
// [WithClientBridgePackage]'s doc comment for the default.
func WithBridgePackage(spec string) Option { return func(a *Adapter) { a.bridgePackage = spec } }

// WithDirectBinary bypasses npx entirely, spawning an already-installed
// `claude-agent-acp` binary directly. See [WithClientDirectBinary]'s doc
// comment.
func WithDirectBinary(path string) Option { return func(a *Adapter) { a.directBinary = path } }

// WithExtraArgs appends additional CLI arguments forwarded to
// claude-agent-acp itself.
func WithExtraArgs(args ...string) Option {
	return func(a *Adapter) { a.extraArgs = append(a.extraArgs, args...) }
}

// New returns a Claude ACP-bridge Adapter configured by opts.
func New(opts ...Option) *Adapter {
	a := &Adapter{}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// newClient builds a fresh [Client] using this Adapter's configured
// overrides. Used by both [Adapter.Describe] (to answer
// InterruptCapability, a static fact independent of any live session —
// see [Client.InterruptCapability]'s own doc comment) and
// [Adapter.CLIAdapter] (a distinct instance per call, so the wrapper can
// mutate per-session state without affecting other in-flight sessions —
// same convention [adapters/opencodeacp] and [adapters/codex] already
// use for their own CLIAdapter()).
func (a *Adapter) newClient() *Client {
	var opts []ClientOption
	if a.npxBinary != "" {
		opts = append(opts, WithClientNpxBinary(a.npxBinary))
	}
	if a.bridgePackage != "" {
		opts = append(opts, WithClientBridgePackage(a.bridgePackage))
	}
	if a.directBinary != "" {
		opts = append(opts, WithClientDirectBinary(a.directBinary))
	}
	if len(a.extraArgs) > 0 {
		opts = append(opts, WithClientExtraArgs(a.extraArgs...))
	}
	return NewClient(opts...)
}

// Name implements [adapters.Adapter].
func (*Adapter) Name() string { return "claude-acp" }

// Describe implements [adapters.Adapter] via [acp.DescriptorFor],
// threading a fresh, unlaunched [Client]'s InterruptCapability() through
// — [adapters.InterruptTurn], verified both by source (the bridge's real
// `cancel()` calls the Claude Agent SDK's own `query.interrupt()`) and
// live against @agentclientprotocol/claude-agent-acp 0.70.0 (see package
// doc for the measured cancel-to-response latency mid-generation).
func (a *Adapter) Describe() adapters.Descriptor {
	return acp.DescriptorFor(a.newClient(), "claude", adapters.TransportStdio)
}

// Resolve implements [adapters.Adapter]. It builds the informational exec
// [adapters.Spec] for one bridge invocation — informational only on the
// agentkit dispatch path (agentkit constructs its own argv via
// [Adapter.CLIAdapter]'s BuildArgs, not via Resolve), mirroring
// [adapters/opencodeacp]'s and [adapters/codex]'s own Resolve() — but is
// the authoritative shape [Client.Launch] itself actually spawns (via
// [Client.resolveCommand]).
func (a *Adapter) Resolve(rc adapters.ResolveContext) (adapters.Spec, error) {
	if rc.PTY {
		return adapters.Spec{}, ErrPTYUnsupported
	}
	binary, args := a.newClient().resolveCommand()
	return adapters.Spec{
		Binary: binary,
		Args:   args,
		Env:    rc.Env,
		Cwd:    rc.Cwd,
	}, nil
}

// ErrPTYUnsupported is returned by [Adapter.Resolve] when the caller
// requests PTY allocation. ACP is JSON-RPC 2.0 over stdio, incompatible
// with a PTY parent — same rationale [adapters/opencodeacp],
// [adapters/copilotacp], and [adapters/codex] already document for their
// own non-PTY runtimes.
var ErrPTYUnsupported = errors.New("claudeacp: adapter does not support PTY allocation")

// CLIAdapter implements [adapters.RuntimeAdapter]. It returns a fresh
// [cliAdapter] — go-providers' provider.CLIAdapter shape, so a
// wrapper.Wrapper.Run() caller dispatches through agentkit's jsonrpc-
// stdio runtime (Protocol: acp, Transport: stdio → Capabilities.
// JsonRpcStdio, per go-agent-wrapper's runtime_dispatch.go) without
// ErrUnknownRuntime.
//
// This compatibility shape retains real Detect/BuildArgs and a pass-through
// ParseLine. Wrapper.Run selects [Adapter.ACPClient] for ProtocolACP, so its
// end-to-end protocol lifecycle does not run through this read-only seam.
func (a *Adapter) CLIAdapter() provider.CLIAdapter {
	return &cliAdapter{client: a.newClient()}
}

// ACPClient implements [acp.ClientAdapter]. Wrapper calls this to obtain the
// real single-session protocol client instead of the capture-only CLI shim.
func (a *Adapter) ACPClient() acp.Client { return a.newClient() }

// cliAdapter implements go-providers' provider.CLIAdapter for the
// claude-agent-acp bridge exec shape. See [Adapter.CLIAdapter]'s doc
// comment for why ParseLine is a deliberate pass-through, not a real ACP
// parser.
type cliAdapter struct {
	client *Client
}

func (a *cliAdapter) Name() string { return "claude-acp" }

// Detect resolves the real command [Client.Launch] would spawn (npx, an
// already-installed direct binary, or an explicit override — see
// [Client.resolveCommand]) and performs a genuine exec.LookPath on it, so
// jsonRpcStdioRuntime.Prepare's BinaryRequired check reports "not found"
// honestly rather than always claiming ok=true — same convention
// [adapters/opencodeacp.cliAdapter.Detect] already uses.
func (a *cliAdapter) Detect() (string, bool) {
	binary, _ := a.client.resolveCommand()
	p, err := exec.LookPath(binary)
	if err != nil {
		return "", false
	}
	return p, true
}

// BuildArgs implements provider.CLIAdapter. Returns the real argv
// [Client.Launch] would pass to the resolved binary (npx's `-y <package>`
// plus any configured extra args, or the direct-binary equivalent) — no
// client interaction here (unlike a naive glue that would try to trigger
// [Client.Launch] from this call): see the package doc for why this
// bridge does not attempt to drive a real ACP session through this path.
func (a *cliAdapter) BuildArgs(_, _, _ string) []string {
	_, args := a.client.resolveCommand()
	return args
}

// ParseLine implements provider.CLIAdapter as a deliberate pass-through
// — see [Adapter.CLIAdapter]'s doc comment.
func (a *cliAdapter) ParseLine([]byte) ([]llmtypes.StreamEvent, error) {
	return nil, nil
}

var (
	_ adapters.Adapter        = (*Adapter)(nil)
	_ adapters.RuntimeAdapter = (*Adapter)(nil)
	_ acp.ClientAdapter       = (*Adapter)(nil)
	_ provider.CLIAdapter     = (*cliAdapter)(nil)
)
