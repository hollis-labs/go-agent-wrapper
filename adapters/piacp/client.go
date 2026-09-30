package piacp

import (
	"context"
	"encoding/json"
	"os"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Client is the real, direct ACP client for the `pi-acp` bridge
// (`npx -y pi-acp` by default) — spawns and owns the bridge subprocess
// itself, speaks newline-delimited JSON-RPC 2.0 over its stdin/stdout
// directly. `pi-acp` in turn spawns and owns `pi --mode rpc` internally;
// this Client never touches that inner process directly. See the package
// doc for the empirical wire-behavior findings this implementation is
// built against, the real Node.js/npm/npx runtime requirement, and why
// this Client owns the subprocess rather than riding go-agent-wrapper's
// agentkit/CLIAdapter seam.
//
// A Client is single-use: construct via [NewClient], call [Client.Launch]
// once, then [Client.Prompt]/[Client.Cancel] any number of times, and
// [Client.Close] exactly once when done — mirroring [acp.Client]'s own
// documented single-session contract.
type Client struct {
	binary    string
	extraArgs []string

	core *acp.NDJSONBridgeClient
}

// ClientOption mutates a [Client] during [NewClient]. Distinct from
// [Option] (which configures the wrapper-level [Adapter]) so the two
// constructors don't collide in the package's exported name space.
type ClientOption func(*Client)

// WithClientBinary overrides the executable [Client.Launch] spawns.
// Empty (the default) resolves via the PIACP_CLI_PATH env var, then
// falls back to "npx" (invoked as `npx -y pi-acp`, per the package doc's
// Node.js/npm/npx runtime requirement note) at Launch time.
//
// When this (or PIACP_CLI_PATH) is set, [Client] runs the given binary
// directly with ONLY [WithClientExtraArgs]' args — NOT the default
// `-y pi-acp` npx arguments, which would be meaningless to a
// non-npx binary (e.g. a globally-installed `pi-acp`, or a test fixture
// script). See [resolveCommand].
func WithClientBinary(path string) ClientOption { return func(c *Client) { c.binary = path } }

// WithClientExtraArgs appends additional CLI arguments. For the default
// npx-based invocation these are appended after `-y pi-acp`; for an
// explicit [WithClientBinary] override they are the ENTIRE argv (see
// [resolveCommand]).
func WithClientExtraArgs(args ...string) ClientOption {
	return func(c *Client) { c.extraArgs = append(c.extraArgs, args...) }
}

// NewClient returns a Client configured by opts, ready for [Client.Launch].
func NewClient(opts ...ClientOption) *Client {
	c := &Client{}
	for _, opt := range opts {
		opt(c)
	}
	c.core = acp.NewNDJSONBridgeClient(acp.NDJSONBridgeConfig{
		Component: "piacp",
		ResolveCommand: func(acp.LaunchParams) (string, []string, error) {
			binary, args := c.resolveCommand()
			return binary, args, nil
		},
		HandleNotification: handleNotification,
	})
	return c
}

// bridgeBinaryEnvOverride reads the PIACP_CLI_PATH env var override —
// shared by [Client.resolveCommand] and [cliAdapter.Detect] so both
// resolution paths agree on the same override precedence. Named
// distinctly from `pi-acp`'s own `PI_ACP_*` env var namespace (e.g.
// `PI_ACP_ENABLE_EMBEDDED_CONTEXT`) to avoid any collision or confusion
// with that project's own vocabulary.
func bridgeBinaryEnvOverride() string { return os.Getenv("PIACP_CLI_PATH") }

// resolveCommand returns the real binary and argv [Client.Launch] spawns.
// Precedence: an explicit [WithClientBinary] override, then the
// PIACP_CLI_PATH env var, then the default zero-install `npx -y pi-acp`
// invocation — see the package doc's Node.js/npm/npx runtime requirement
// note. When either override applies, the binary is run with ONLY
// [WithClientExtraArgs]' args (no `-y pi-acp` prepended) since an
// override implies the caller is pointing directly at a `pi-acp`-shaped
// binary (a real global install, or a test fixture), not at `npx`
// itself.
func (c *Client) resolveCommand() (string, []string) {
	if c.binary != "" {
		return c.binary, append([]string(nil), c.extraArgs...)
	}
	if p := bridgeBinaryEnvOverride(); p != "" {
		return p, append([]string(nil), c.extraArgs...)
	}
	return "npx", append([]string{"-y", "pi-acp"}, c.extraArgs...)
}

// Launch implements [acp.Client] by delegating to the shared
// [acp.NDJSONBridgeClient]: it spawns the resolved command, performs the
// initialize/authenticate/session handshake, and returns once the session is
// ready to accept a Prompt.
func (c *Client) Launch(ctx context.Context, params acp.LaunchParams) error {
	return c.core.Launch(ctx, params)
}

// Prompt implements [acp.Client]. It returns once the turn is accepted; the
// turn's progress and completion arrive through [Client.Events].
func (c *Client) Prompt(ctx context.Context, prompt string) error {
	return c.core.Prompt(ctx, prompt)
}

// Cancel implements [acp.Client]: it sends `session/cancel` for the in-flight
// turn and is a no-op when none is active.
func (c *Client) Cancel(ctx context.Context) error { return c.core.Cancel(ctx) }

// Close implements [acp.Client]. It is idempotent and safe before or after a
// failed Launch.
func (c *Client) Close(ctx context.Context) error { return c.core.Close(ctx) }

// Events implements [acp.Client].
func (c *Client) Events() <-chan runtimeevents.Event { return c.core.Events() }

// ProviderSessionID returns the id established by session/new or session/load.
func (c *Client) ProviderSessionID() string { return c.core.ProviderSessionID() }

// InterruptCapability implements [acp.Client]: session/cancel is a genuine
// mid-turn abort for this agent.
func (c *Client) InterruptCapability() adapters.InterruptCapability {
	return c.core.InterruptCapability()
}

func mustMarshal(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

var _ acp.Client = (*Client)(nil)
