package codexacp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// defaultBridgePackage pins the exact codex-acp version this package was
// verified against (see package doc's "Real wire/spawn behavior"
// section). Pinning rather than tracking `@latest` is deliberate: this
// bridge is actively maintained and fast-moving (pushed the same day
// task 12's decision was recorded), so an unpinned `npx` invocation could
// silently start driving a different, unverified bridge version on a
// future launch. [WithClientBridgeVersion] overrides this for callers who
// want to track latest deliberately.
const defaultBridgePackage = "@agentclientprotocol/codex-acp"

const defaultBridgeVersion = "1.6.2"

// Client is the real, direct ACP client driving Codex through the
// agentclientprotocol/codex-acp bridge — spawns and owns the bridge
// subprocess itself (`npx -y @agentclientprotocol/codex-acp[@version]`),
// speaks newline-delimited JSON-RPC 2.0 over its stdin/stdout directly.
// See the package doc for the empirical wire-behavior findings this
// implementation is built against, why it owns the subprocess rather
// than riding go-agent-wrapper's agentkit/CLIAdapter seam, and the
// Node.js/npm/npx runtime requirement this Client introduces.
//
// A Client is single-use: construct via [NewClient], call [Client.Launch]
// once, then [Client.Prompt]/[Client.Cancel] any number of times, and
// [Client.Close] exactly once when done — mirroring [acp.Client]'s own
// documented single-session contract.
type Client struct {
	bridgeBinary  string // default "npx"
	directBinary  string // when set, spawned directly: no npx, no -y, no package spec
	bridgeVersion string // default defaultBridgeVersion; "" (via WithClientBridgePackageSpec) means unpinned "@latest"-equivalent bare package name
	bridgePkgSpec string // when set (via WithClientBridgePackageSpec), overrides the whole "package[@version]" token
	extraArgs     []string
	codexBinary   string // explicit CODEX_PATH override; "" resolves only from the launch environment below

	core *acp.NDJSONBridgeClient
}

// ClientOption mutates a [Client] during [NewClient]. Distinct from
// [Option] (which configures the wrapper-level [Adapter]) so the two
// constructors don't collide in the package's exported name space —
// same convention [adapters/opencodeacp] already uses.
type ClientOption func(*Client)

// WithClientBinary overrides the executable [Client.Launch] spawns to
// run the bridge — "npx" by default. Set this (combined with
// [WithClientBridgePackageSpec]) to run a globally-installed `codex-acp`
// binary directly instead of going through npx.
func WithClientBinary(path string) ClientOption { return func(c *Client) { c.bridgeBinary = path } }

// WithClientDirectBinary bypasses npx entirely: [Client.Launch] spawns this
// binary directly with only [WithClientExtraArgs] (no `-y`, no package
// spec), for an already-installed `codex-acp`. Unlike [WithClientBinary],
// which replaces only `npx` and keeps the package arguments. Matches
// claudeacp's and piacp's direct-binary options.
func WithClientDirectBinary(path string) ClientOption {
	return func(c *Client) { c.directBinary = path }
}

// WithClientBridgeVersion pins a specific codex-acp npm package version
// other than this package's own verified default
// ([defaultBridgeVersion]). Pass "" to track npm's `latest` dist-tag (an
// explicit opt-in to unpinned behavior — see [defaultBridgePackage]'s
// doc comment for why pinning is the default).
func WithClientBridgeVersion(version string) ClientOption {
	return func(c *Client) { c.bridgeVersion = version }
}

// WithClientBridgePackageSpec overrides the entire npm package spec
// token (e.g. a local tarball path, a different scope, or a bare binary
// name when paired with [WithClientBinary] pointing directly at an
// installed `codex-acp`). When set, [WithClientBridgeVersion] is
// ignored.
func WithClientBridgePackageSpec(spec string) ClientOption {
	return func(c *Client) { c.bridgePkgSpec = spec }
}

// WithClientExtraArgs appends additional CLI arguments after the
// resolved package spec token.
func WithClientExtraArgs(args ...string) ClientOption {
	return func(c *Client) { c.extraArgs = append(c.extraArgs, args...) }
}

// WithClientCodexBinary overrides the real `codex` executable this
// Client tells the bridge to run (via the bridge's own `CODEX_PATH` env
// var — see package doc). Empty (the default) resolves via CODEX_CLI_PATH,
// then PATH, from the environment supplied at Launch. With the default
// inherited launch environment this matches [adapters/codex]'s resolver;
// a sanitized environment cannot silently re-import an excluded host path.
// If resolution finds nothing, CODEX_PATH is left unset and the bridge falls
// back to its own bundled `@openai/codex` dependency.
func WithClientCodexBinary(path string) ClientOption { return func(c *Client) { c.codexBinary = path } }

// NewClient returns a Client configured by opts, ready for [Client.Launch].
func NewClient(opts ...ClientOption) *Client {
	c := &Client{bridgeVersion: defaultBridgeVersion}
	for _, opt := range opts {
		opt(c)
	}
	c.core = acp.NewNDJSONBridgeClient(acp.NDJSONBridgeConfig{
		Component: "codexacp",
		ResolveCommand: func(acp.LaunchParams) (string, []string, error) {
			binary, args := c.resolveBridgeCommand()
			return binary, args, nil
		},
		HandleNotification: handleNotification,
		LaunchEnv:          c.launchEnv,
	})
	return c
}

// resolveBridgeCommand returns the binary and args [Client.Launch]
// spawns. Default: `npx -y @agentclientprotocol/codex-acp@<pinned
// version>`. See [WithClientBinary]/[WithClientBridgeVersion]/
// [WithClientBridgePackageSpec]/[WithClientExtraArgs].
func (c *Client) resolveBridgeCommand() (string, []string) {
	if c.directBinary != "" {
		return c.directBinary, append([]string(nil), c.extraArgs...)
	}
	binary := c.bridgeBinary
	if binary == "" {
		binary = "npx"
	}
	pkg := c.bridgePkgSpec
	if pkg == "" {
		pkg = defaultBridgePackage
		if c.bridgeVersion != "" {
			pkg = pkg + "@" + c.bridgeVersion
		}
	}
	args := []string{"-y", pkg}
	args = append(args, c.extraArgs...)
	return binary, args
}

// resolveCodexPath applies the [WithClientCodexBinary] override, then the
// supplied launch environment's CODEX_CLI_PATH, then its PATH for "codex" — see
// [WithClientCodexBinary]'s doc comment for why this mirrors
// [adapters/codex]'s own resolver precedence. Returns "" when none
// resolve, leaving CODEX_PATH unset for the spawned bridge (which then
// falls back to its own bundled `@openai/codex` dependency).
func (c *Client) resolveCodexPath(env []string) string {
	if c.codexBinary != "" {
		return c.codexBinary
	}
	if p := environmentValue(env, "CODEX_CLI_PATH"); p != "" {
		return p
	}
	return executableInEnvironmentPath("codex", env)
}

func environmentValue(env []string, name string) string {
	var value string
	for _, assignment := range env {
		key, candidate, ok := strings.Cut(assignment, "=")
		if !ok || !environmentNamesEqual(key, name, runtime.GOOS) {
			continue
		}
		value = candidate
	}
	return value
}

func environmentNamesEqual(left, right, goos string) bool {
	if goos == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func executableInEnvironmentPath(name string, env []string) string {
	pathValue := environmentValue(env, "PATH")
	if pathValue == "" {
		return ""
	}
	extensions := []string{""}
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		extensions = filepath.SplitList(environmentValue(env, "PATHEXT"))
		if len(extensions) == 0 {
			extensions = []string{".com", ".exe", ".bat", ".cmd"}
		}
	}
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			directory = "."
		}
		for _, extension := range extensions {
			candidate := filepath.Join(directory, name+extension)
			info, err := os.Stat(candidate)
			if err != nil || info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
				continue
			}
			absolute, err := filepath.Abs(candidate)
			if err == nil {
				return absolute
			}
		}
	}
	return ""
}

// buildEnv assembles the environment the bridge subprocess runs with:
// paramsEnv (or the current process environment, when paramsEnv is nil) plus
// an explicit CODEX_PATH entry resolved only from that resulting environment.
// When it already sets CODEX_PATH, the caller's choice is respected unmodified.
func (c *Client) buildEnv(paramsEnv []string) []string {
	return c.buildEnvForOS(paramsEnv, runtime.GOOS)
}

func (c *Client) launchEnv(params acp.LaunchParams) []string {
	if params.Command != nil {
		return append([]string(nil), params.Env...)
	}
	return c.buildEnv(params.Env)
}

func (c *Client) buildEnvForOS(paramsEnv []string, goos string) []string {
	env := append([]string(nil), paramsEnv...)
	if paramsEnv == nil {
		env = os.Environ()
	}
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if ok && environmentNamesEqual(key, "CODEX_PATH", goos) {
			return env
		}
	}
	if codexPath := c.resolveCodexPath(env); codexPath != "" {
		env = append(env, "CODEX_PATH="+codexPath)
	}
	return env
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
