package opencodeacp

import (
	"context"
	"encoding/json"
	"os"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Client is the real, direct ACP client for OpenCode's `opencode acp`
// subprocess mode — spawns and owns the subprocess itself, speaks
// newline-delimited JSON-RPC 2.0 over its stdin/stdout directly. See the
// package doc for the empirical wire-behavior findings this
// implementation is built against and why it owns the subprocess rather
// than riding go-agent-wrapper's agentkit/CLIAdapter seam.
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
// constructors — Client's, used for direct/standalone driving, and
// Adapter's, used for the adapters.Adapter/RuntimeAdapter seam — don't
// collide in the package's exported name space.
type ClientOption func(*Client)

// WithClientBinary overrides the executable path [Client.Launch] spawns.
// Empty (the default) resolves "opencode" via the OPENCODE_CLI_PATH env
// var, then PATH, at Launch time — mirroring the convention
// adapters/opencode and adapters/codex already use for their own
// env-var overrides.
func WithClientBinary(path string) ClientOption { return func(c *Client) { c.binary = path } }

// WithClientExtraArgs appends additional CLI arguments after the `acp`
// token (e.g. `--print-logs`, `--log-level`). Rarely needed — `opencode
// acp` takes no required flags beyond the subcommand itself.
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
		Component: "opencodeacp",
		ResolveCommand: func(acp.LaunchParams) (string, []string, error) {
			return c.resolveBinary(), append([]string{"acp"}, c.extraArgs...), nil
		},
		HandleNotification: handleNotification,
	})
	return c
}

// resolveBinary applies the WithBinary override, then OPENCODE_CLI_PATH,
// then falls back to "opencode" resolved via PATH at exec time —
// matching adapters/opencode's own Detect() convention (same underlying
// binary, different subcommand).
func (c *Client) resolveBinary() string {
	if c.binary != "" {
		return c.binary
	}
	if p := clientBinaryEnvOverride(); p != "" {
		return p
	}
	return "opencode"
}

// clientBinaryEnvOverride reads the OPENCODE_CLI_PATH env var override —
// shared by [Client.resolveBinary] and [cliAdapter.Detect] so both
// resolution paths agree on the same override precedence.
func clientBinaryEnvOverride() string { return os.Getenv("OPENCODE_CLI_PATH") }

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
