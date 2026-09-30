package claudeacp

import (
	"context"
	"encoding/json"
	"os"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// defaultBridgePackage is the npm package [Client.Launch] resolves via
// `npx -y <package>` by default — the operator-pinned bridge for Claude
// (TASKS/agent-host-acp/12, Nanite repo). Unpinned deliberately: npx
// resolves+caches the latest published version on each cold cache,
// matching this young, fast-moving ecosystem's own "re-verify at
// implementation time" posture rather than freezing a version in code.
// [WithClientBridgePackage] or the CLAUDE_ACP_BRIDGE_PACKAGE env var
// override this for callers who want a pinned version instead.
const defaultBridgePackage = "@agentclientprotocol/claude-agent-acp"

// Client is the bridge-mediated ACP client for Claude — spawns and owns
// the `@agentclientprotocol/claude-agent-acp` bridge subprocess itself
// (via npx by default; see [Client]'s binary-resolution doc below),
// speaks newline-delimited JSON-RPC 2.0 over its stdin/stdout directly.
// See the package doc for the empirical wire-behavior findings this
// implementation is built against, including two real divergences from
// [adapters/opencodeacp]'s own verified shapes.
//
// A Client is single-use: construct via [NewClient], call [Client.Launch]
// once, then [Client.Prompt]/[Client.Cancel] any number of times, and
// [Client.Close] exactly once when done — mirroring [acp.Client]'s own
// documented single-session contract, and structurally identical to
// [adapters/opencodeacp.Client]'s own lifecycle.
type Client struct {
	// npxBinary overrides the `npx` executable path (rarely needed —
	// almost always resolved from PATH). Empty means default resolution.
	npxBinary string
	// bridgePackage overrides the npm package spec passed to `npx -y`.
	// Empty means [defaultBridgePackage].
	bridgePackage string
	// directBinary, when set, bypasses npx entirely: Launch spawns this
	// binary directly (with extraArgs, and no `-y`/package-spec
	// arguments) — for an already-installed `claude-agent-acp` global
	// binary. See [WithClientDirectBinary] and the
	// CLAUDE_ACP_BRIDGE_PATH env var.
	directBinary string
	// extraArgs are appended after the resolved command (forwarded to
	// claude-agent-acp itself, e.g. `--cli`).
	extraArgs []string

	core *acp.NDJSONBridgeClient
}

// ClientOption mutates a [Client] during [NewClient]. Distinct from
// [Option] (which configures the wrapper-level [Adapter]) so the two
// constructors don't collide in the package's exported name space — same
// convention [adapters/opencodeacp] already uses.
type ClientOption func(*Client)

// WithClientNpxBinary overrides the `npx` executable path [Client.Launch]
// spawns when not bypassing npx via [WithClientDirectBinary]. Empty (the
// default) resolves via the CLAUDE_ACP_NPX_PATH env var, then PATH.
func WithClientNpxBinary(path string) ClientOption { return func(c *Client) { c.npxBinary = path } }

// WithClientBridgePackage overrides the npm package spec resolved via
// `npx -y <spec>` — e.g. to pin an exact version
// (`@agentclientprotocol/claude-agent-acp@0.70.0`) instead of the
// unpinned default. Empty (the default) resolves via the
// CLAUDE_ACP_BRIDGE_PACKAGE env var, then [defaultBridgePackage].
func WithClientBridgePackage(spec string) ClientOption {
	return func(c *Client) { c.bridgePackage = spec }
}

// WithClientDirectBinary bypasses npx entirely: [Client.Launch] spawns
// this binary directly (with any [WithClientExtraArgs], no `-y`/package
// arguments) — for an already-installed `claude-agent-acp` global binary,
// trading the npx resolve-and-cache cost on every Launch for an explicit
// install step. See the CLAUDE_ACP_BRIDGE_PATH env var for the
// non-code-change equivalent.
func WithClientDirectBinary(path string) ClientOption {
	return func(c *Client) { c.directBinary = path }
}

// WithClientExtraArgs appends additional CLI arguments forwarded to
// claude-agent-acp itself (after the resolved command).
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
		Component: "claudeacp",
		ResolveCommand: func(acp.LaunchParams) (string, []string, error) {
			binary, args := c.resolveCommand()
			return binary, args, nil
		},
		HandleNotification: handleNotification,
	})
	return c
}

// resolveCommand returns the (binary, args) [Client.Launch] should spawn:
// an explicit [WithClientDirectBinary] override first, then the
// CLAUDE_ACP_BRIDGE_PATH env var (both bypass npx entirely), otherwise
// `npx -y <bridgePackage>` — [WithClientNpxBinary]/CLAUDE_ACP_NPX_PATH
// override the npx executable, [WithClientBridgePackage]/
// CLAUDE_ACP_BRIDGE_PACKAGE override the package spec. extraArgs are
// always appended last, forwarded to claude-agent-acp itself.
func (c *Client) resolveCommand() (string, []string) {
	if c.directBinary != "" {
		return c.directBinary, append([]string(nil), c.extraArgs...)
	}
	if p := os.Getenv("CLAUDE_ACP_BRIDGE_PATH"); p != "" {
		return p, append([]string(nil), c.extraArgs...)
	}

	npx := c.npxBinary
	if npx == "" {
		if p := os.Getenv("CLAUDE_ACP_NPX_PATH"); p != "" {
			npx = p
		} else {
			npx = "npx"
		}
	}
	pkg := c.bridgePackage
	if pkg == "" {
		if p := os.Getenv("CLAUDE_ACP_BRIDGE_PACKAGE"); p != "" {
			pkg = p
		} else {
			pkg = defaultBridgePackage
		}
	}
	args := append([]string{"-y", pkg}, c.extraArgs...)
	return npx, args
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
