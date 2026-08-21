package codexacp

import (
	"errors"
	"os/exec"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider"
)

// Adapter is the wrapper adapter for Codex driven through the
// agentclientprotocol/codex-acp bridge (`npx -y
// @agentclientprotocol/codex-acp`). Construct via [New]; pass to
// [github.com/hollis-labs/go-agent-wrapper/wrapper.Config.Adapter].
//
// Name() deliberately returns "codex-acp", distinct from the existing
// native adapters/codex package's "codex" — both represent the same
// upstream agent (Descriptor.Provider is "codex" for both, per
// [acp.DescriptorFor]'s providerName convention) but over structurally
// different wire protocols and, unlike the native adapter, mediated by a
// third-party bridge subprocess — see the package doc for the full
// rationale and the Node.js/npm/npx runtime requirement this adapter
// introduces.
type Adapter struct {
	bridgeBinary     string
	bridgeVersion    string
	bridgeVersionSet bool // distinguishes "not overridden" from an explicit WithBridgeVersion("") (track latest) — a bare zero-value string can't tell those apart.
	bridgePkgSpec    string
	extraArgs        []string
	codexBinary      string
}

// Option mutates an [Adapter] during [New].
type Option func(*Adapter)

// WithBinary overrides the executable [Adapter.Resolve] and the [Client]
// this Adapter constructs spawn to run the bridge — "npx" by default.
// See [WithClientBinary] on [Client] for the matching low-level option.
func WithBinary(path string) Option { return func(a *Adapter) { a.bridgeBinary = path } }

// WithBridgeVersion pins a specific codex-acp npm package version other
// than this package's own verified default. Pass "" to explicitly track
// npm's `latest` dist-tag — that empty-string call is itself the
// override (distinct from never calling WithBridgeVersion at all,
// which keeps this package's pinned default). See
// [defaultBridgePackage]'s doc comment for why pinning is the default.
func WithBridgeVersion(version string) Option {
	return func(a *Adapter) { a.bridgeVersion = version; a.bridgeVersionSet = true }
}

// WithBridgePackageSpec overrides the entire npm package spec token
// passed to npx. See [WithClientBridgePackageSpec] on [Client].
func WithBridgePackageSpec(spec string) Option { return func(a *Adapter) { a.bridgePkgSpec = spec } }

// WithExtraArgs appends additional CLI arguments after the resolved
// package spec token.
func WithExtraArgs(args ...string) Option {
	return func(a *Adapter) { a.extraArgs = append(a.extraArgs, args...) }
}

// WithCodexBinary overrides the real `codex` executable the bridge is
// told to run via its own `CODEX_PATH` env var. See
// [WithClientCodexBinary] on [Client] for the full resolution
// precedence.
func WithCodexBinary(path string) Option { return func(a *Adapter) { a.codexBinary = path } }

// New returns a Codex ACP-bridge Adapter configured by opts.
func New(opts ...Option) *Adapter {
	a := &Adapter{}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// newClient builds a fresh [Client] using this Adapter's configured
// overrides. Used by both [Adapter.Describe] (to answer
// InterruptCapability, a static fact independent of any live session)
// and [Adapter.CLIAdapter] (a distinct instance per call, so the wrapper
// can mutate per-session state without affecting other in-flight
// sessions) — same convention adapters/opencodeacp and adapters/codex
// already use for their own CLIAdapter().
func (a *Adapter) newClient() *Client {
	var opts []ClientOption
	if a.bridgeBinary != "" {
		opts = append(opts, WithClientBinary(a.bridgeBinary))
	}
	if a.bridgeVersionSet {
		opts = append(opts, WithClientBridgeVersion(a.bridgeVersion))
	}
	if a.bridgePkgSpec != "" {
		opts = append(opts, WithClientBridgePackageSpec(a.bridgePkgSpec))
	}
	if len(a.extraArgs) > 0 {
		opts = append(opts, WithClientExtraArgs(a.extraArgs...))
	}
	if a.codexBinary != "" {
		opts = append(opts, WithClientCodexBinary(a.codexBinary))
	}
	return NewClient(opts...)
}

// Name implements [adapters.Adapter].
func (*Adapter) Name() string { return "codex-acp" }

// Describe implements [adapters.Adapter] via [acp.DescriptorFor],
// threading a fresh, unlaunched [Client]'s InterruptCapability() through
// — [adapters.InterruptTurn], verified both against codex-acp 1.6.2's
// own published source and live (see package doc for the measured
// cancel-to-response latency and the source-level `turn/interrupt`
// finding).
func (a *Adapter) Describe() adapters.Descriptor {
	return acp.DescriptorFor(a.newClient(), "codex", adapters.TransportStdio)
}

// Resolve implements [adapters.Adapter]. It builds the informational
// exec [adapters.Spec] for one bridge invocation — informational only on
// the agentkit dispatch path (agentkit constructs its own argv via
// [Adapter.CLIAdapter]'s BuildArgs, not via Resolve) — but is the
// authoritative shape [Client.Launch] itself actually spawns. Note
// Resolve does not itself thread through CODEX_PATH (an env var
// [Client.Launch] computes at spawn time, not part of the static
// argv/binary shape Resolve reports) — see package doc.
func (a *Adapter) Resolve(rc adapters.ResolveContext) (adapters.Spec, error) {
	if rc.PTY {
		return adapters.Spec{}, ErrPTYUnsupported
	}
	c := a.newClient()
	binary, args := c.resolveBridgeCommand()
	return adapters.Spec{
		Binary: binary,
		Args:   args,
		Env:    rc.Env,
		Cwd:    rc.Cwd,
	}, nil
}

// ErrPTYUnsupported is returned by [Adapter.Resolve] when the caller
// requests PTY allocation. ACP is JSON-RPC 2.0 over stdio, incompatible
// with a PTY parent — same rationale adapters/codex and
// adapters/opencodeacp already document for their own non-PTY runtimes.
var ErrPTYUnsupported = errors.New("codexacp: adapter does not support PTY allocation")

// CLIAdapter implements [adapters.RuntimeAdapter]. It returns a fresh
// [cliAdapter] — go-providers' provider.CLIAdapter shape, so a
// wrapper.Wrapper.Run() caller dispatches through agentkit's jsonrpc-
// stdio runtime (Protocol: acp, Transport: stdio → Capabilities.
// JsonRpcStdio, per go-agent-wrapper's runtime_dispatch.go) without
// ErrUnknownRuntime.
//
// This bridge deliberately mirrors adapters/opencodeacp's own
// cliAdapter shape exactly: real Detect/BuildArgs (so the ONE real
// bridge process agentkit spawns is the genuine article, not a
// duplicate) and a pass-through ParseLine returning (nil, nil) — see the
// package doc's "Client ownership of the subprocess" section for why.
// Real, live-verified ACP driving for this task goes through [Client]
// directly (used standalone, not via this bridge) — [cliAdapter] exists
// for interface completeness and dispatch-table compatibility, matching
// the same seam-gap tasks 09/10 already documented for their own native
// ACP adapters.
func (a *Adapter) CLIAdapter() provider.CLIAdapter {
	return &cliAdapter{adapter: a}
}

// cliAdapter implements go-providers' provider.CLIAdapter for the
// codex-acp bridge exec shape. See [Adapter.CLIAdapter]'s doc comment
// for why ParseLine is a deliberate pass-through, not a real ACP parser.
type cliAdapter struct {
	adapter *Adapter
}

func (a *cliAdapter) Name() string { return "codex-acp" }

// Detect resolves the real `npx` (or configured override) binary this
// Adapter would spawn. Unlike [Client.resolveBridgeCommand] (which
// defers the existence check to exec.Command's own failure at Launch
// time), Detect() performs a genuine exec.LookPath so
// jsonRpcStdioRuntime.Prepare's BinaryRequired check reports "not found"
// honestly rather than always claiming ok=true — mirrors
// adapters/opencodeacp.cliAdapter.Detect()'s own convention.
//
// Note this only confirms `npx` itself resolves — it does NOT confirm
// the codex-acp npm package fetches successfully or that the real
// `codex` CLI it drives is reachable; those are only confirmed once
// [Client.Launch] actually runs. See package doc's "Node.js/npm/npx
// runtime requirement" section.
func (a *cliAdapter) Detect() (string, bool) {
	c := a.adapter.newClient()
	binary, _ := c.resolveBridgeCommand()
	p, err := exec.LookPath(binary)
	if err != nil {
		return "", false
	}
	return p, true
}

// BuildArgs implements provider.CLIAdapter. Returns the real bridge argv
// — no client interaction here (unlike a naive glue that would try to
// trigger [Client.Launch] from this call): see the package doc for why
// this bridge does not attempt to drive a real ACP session through this
// path.
func (a *cliAdapter) BuildArgs(_, _, _ string) []string {
	c := a.adapter.newClient()
	_, args := c.resolveBridgeCommand()
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
	_ provider.CLIAdapter     = (*cliAdapter)(nil)
)
