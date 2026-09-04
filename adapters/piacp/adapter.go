package piacp

import (
	"errors"
	"os/exec"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider"
)

// Adapter is the wrapper adapter for Pi via the `pi-acp` bridge
// (`npx -y pi-acp`). Construct via [New]; pass to
// [github.com/hollis-labs/go-agent-wrapper/wrapper.Config.Adapter].
//
// Name() returns "pi-acp", not the bare upstream identity "pi" —
// Descriptor.Provider (see [Describe]) carries "pi" as the upstream
// agent identity instead, per [acp.DescriptorFor]'s convention. This
// mirrors opencodeacp's own naming discipline (Adapter.Name()'s doc
// comment: "used in logs, configuration, and policy rule keys... calls
// for distinct identifiers wherever two adapters could otherwise collide
// in those keyspaces") rather than copilotacp's — chosen deliberately
// even though Pi has no sibling native adapter in this repo TODAY (per
// TASKS/agent-host-acp/15, Nanite repo, this is Pi's first appearance at
// all): this Adapter is fundamentally bridge-mediated, not a native Pi
// wire client, and a future native adapter speaking Pi's own `--mode
// rpc`/`--mode json` protocol directly is plausible enough that leaving
// room for a plain "pi" identifier is worth the small naming discipline
// cost now.
type Adapter struct {
	binary    string
	extraArgs []string
}

// Option mutates an [Adapter] during [New].
type Option func(*Adapter)

// WithBinary overrides the executable path used by both
// [Adapter.Resolve] and the [Client] this Adapter constructs. See
// [WithClientBinary]'s doc comment for the argv implications of setting
// this (or the PIACP_CLI_PATH env var).
func WithBinary(path string) Option { return func(a *Adapter) { a.binary = path } }

// WithExtraArgs appends additional CLI arguments — see
// [WithClientExtraArgs].
func WithExtraArgs(args ...string) Option {
	return func(a *Adapter) { a.extraArgs = append(a.extraArgs, args...) }
}

// New returns a Pi ACP (bridge-mediated) Adapter configured by opts.
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
// same convention opencodeacp already uses for its own CLIAdapter()).
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
func (*Adapter) Name() string { return "pi-acp" }

// Describe implements [adapters.Adapter] via [acp.DescriptorFor],
// threading a fresh, unlaunched [Client]'s InterruptCapability() through
// — [adapters.InterruptTurn], verified live against pi-acp 0.0.33 + pi
// 0.84.2 (see package doc for the measured cancel behavior against a
// genuinely still-running subprocess).
func (a *Adapter) Describe() adapters.Descriptor {
	return acp.DescriptorFor(a.newClient(), "pi", adapters.TransportStdio)
}

// Resolve implements [adapters.Adapter]. It builds the informational
// exec [adapters.Spec] for one bridge invocation — mirrors
// opencodeacp's/adapters/codex's own Resolve(): informational only on
// the agentkit dispatch path (agentkit constructs its own argv via
// [Adapter.CLIAdapter]'s BuildArgs, not via Resolve) — but is the
// authoritative shape [Client.Launch] itself actually spawns (default:
// `npx -y pi-acp`; see [resolveCommand] for the override precedence this
// mirrors).
func (a *Adapter) Resolve(rc adapters.ResolveContext) (adapters.Spec, error) {
	if rc.PTY {
		return adapters.Spec{}, ErrPTYUnsupported
	}
	binary := a.binary
	var args []string
	if binary == "" {
		binary = "npx"
		args = append([]string{"-y", "pi-acp"}, a.extraArgs...)
	} else {
		args = append([]string(nil), a.extraArgs...)
	}
	return adapters.Spec{
		Binary: binary,
		Args:   args,
		Env:    rc.Env,
		Cwd:    rc.Cwd,
	}, nil
}

// ErrPTYUnsupported is returned by [Adapter.Resolve] when the caller
// requests PTY allocation. ACP is JSON-RPC 2.0 over stdio, incompatible
// with a PTY parent — same rationale opencodeacp/adapters/codex already
// document for their own non-PTY runtimes.
var ErrPTYUnsupported = errors.New("piacp: adapter does not support PTY allocation")

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
	binary := a.binary
	extraArgs := append([]string(nil), a.extraArgs...)
	return &cliAdapter{binary: binary, extraArgs: extraArgs}
}

// ACPClient implements [acp.ClientAdapter].
func (a *Adapter) ACPClient() acp.Client { return a.newClient() }

// cliAdapter implements go-providers' provider.CLIAdapter for the
// `npx -y pi-acp` (or overridden) exec shape. See [Adapter.CLIAdapter]'s
// doc comment for why ParseLine is a deliberate pass-through, not a real
// ACP parser.
type cliAdapter struct {
	binary    string
	extraArgs []string
}

func (a *cliAdapter) Name() string { return "pi-acp" }

// Detect resolves the real binary to spawn — an explicit [WithBinary]
// override first, then the PIACP_CLI_PATH env var (same convention
// [Client.resolveCommand] uses), then a real PATH lookup for "npx"
// (this bridge's zero-install default). Unlike [Client.resolveCommand]
// (which defers the existence check to exec.Command's own failure at
// Launch time), this Detect() performs a genuine exec.LookPath so
// jsonRpcStdioRuntime.Prepare's BinaryRequired check reports "not found"
// honestly rather than always claiming ok=true.
func (a *cliAdapter) Detect() (string, bool) {
	if a.binary != "" {
		return a.binary, true
	}
	if p := bridgeBinaryEnvOverride(); p != "" {
		return p, true
	}
	p, err := exec.LookPath("npx")
	if err != nil {
		return "", false
	}
	return p, true
}

// BuildArgs implements provider.CLIAdapter. Returns the real argv — no
// client interaction here (unlike a naive glue that would try to
// trigger [Client.Launch] from this call): see the package doc for why
// this bridge does not attempt to drive a real ACP session through this
// path. Mirrors [Client.resolveCommand]'s argv logic: an explicit
// binary override runs with only extraArgs; the default npx path
// prepends `-y pi-acp`.
func (a *cliAdapter) BuildArgs(_, _, _ string) []string {
	if a.binary != "" || bridgeBinaryEnvOverride() != "" {
		return append([]string(nil), a.extraArgs...)
	}
	return append([]string{"-y", "pi-acp"}, a.extraArgs...)
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
