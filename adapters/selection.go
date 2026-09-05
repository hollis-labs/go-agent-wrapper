package adapters

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/go-providers/provider"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Provider identifies a built-in upstream CLI agent for [Select].
type Provider string

// Providers supported by the native adapter factory.
const (
	ProviderClaude   Provider = "claude"
	ProviderCodex    Provider = "codex"
	ProviderOpenCode Provider = "opencode"
)

// RuntimeKind is the host-level execution class used by [Selection]. It
// deliberately matches Nanite's Runtime kind vocabulary: protocol and
// transport are separate axes described by the selected Adapter.
type RuntimeKind string

// Host-level runtime kinds understood by the native adapter factory.
const (
	RuntimeKindCLI RuntimeKind = "cli"
	RuntimeKindAPI RuntimeKind = "api"
)

// LaunchMode selects a concrete lifecycle shape within RuntimeKindCLI.
type LaunchMode string

const (
	// LaunchDefault preserves each shipped adapter package's historical
	// default: Claude streaming stdio, Codex app-server, OpenCode serve-http.
	LaunchDefault LaunchMode = ""

	// LaunchStreamingStdio selects one long-lived native streaming-stdio
	// subprocess. Supported by Claude.
	LaunchStreamingStdio LaunchMode = "streaming-stdio"

	// LaunchSubprocessPerTurn selects the adapter runtime: one child process
	// per prompt while the wrapper session remains alive. Supported by Claude,
	// Codex, and OpenCode.
	LaunchSubprocessPerTurn LaunchMode = "subprocess-per-turn"

	// LaunchAppServer selects Codex's long-lived JSON-RPC app-server.
	LaunchAppServer LaunchMode = "app-server"

	// LaunchServeHTTP selects OpenCode's long-lived HTTP+SSE server.
	LaunchServeHTTP LaunchMode = "serve-http"
)

// Selection describes how [Select] should build a native RuntimeAdapter.
// Hosts keep product policy (which provider/mode a profile is allowed to use)
// outside the library and pass the resolved choice here.
type Selection struct {
	Provider      Provider
	RuntimeKind   RuntimeKind
	LaunchMode    LaunchMode
	DeveloperMode bool

	// CLIAdapter, when non-nil, is the host's already-configured
	// go-providers adapter. It is returned through RuntimeAdapter unchanged
	// except for Binary/ExtraArgs decoration. DeveloperMode must be false in
	// this case because the supplied adapter is authoritative for provider-
	// specific policy such as Claude's SkipPermissions. Select validates
	// provider identity and rejects a lifecycle mismatch for the built-in
	// go-providers adapter types. A custom implementation is responsible for
	// matching LaunchMode because its internal shape cannot be inspected.
	CLIAdapter provider.CLIAdapter

	// Binary optionally pins an executable without consulting PATH or ambient
	// environment-variable overrides. It must be absolute. The path is passed
	// directly to os/exec by agentkit; it is never interpolated into a shell.
	Binary string

	// ExtraArgs are appended as distinct argv entries to the provider
	// adapter's BuildArgs result. Spaces and shell metacharacters remain data.
	ExtraArgs []string
}

var (
	// ErrUnsupportedSelection is wrapped when a provider/runtime/launch-mode
	// tuple has no built-in adapter.
	ErrUnsupportedSelection = errors.New("adapters: unsupported selection")

	// ErrInvalidSelection is wrapped when fields are ambiguous or malformed.
	ErrInvalidSelection = errors.New("adapters: invalid selection")
)

// Select returns a native RuntimeAdapter for cfg. It centralizes descriptor,
// go-providers CLIAdapter, and lifecycle-shape selection so hosts do not have
// to reimplement the wrapper Adapter interface merely to choose Claude's
// streaming/dev variant or Codex/OpenCode's subprocess-per-turn variants.
//
// ACP remains a protocol-specific client seam and is selected through the
// provider ACP adapter packages, not this native factory.
func Select(cfg Selection) (RuntimeAdapter, error) {
	providerID, err := canonicalProvider(cfg.Provider)
	if err != nil {
		return nil, err
	}
	runtimeKind := cfg.RuntimeKind
	if runtimeKind == "" {
		runtimeKind = RuntimeKindCLI
	}
	if runtimeKind != RuntimeKindCLI {
		return nil, fmt.Errorf("%w: provider=%q runtime_kind=%q", ErrUnsupportedSelection, providerID, runtimeKind)
	}
	if cfg.CLIAdapter != nil && cfg.DeveloperMode {
		return nil, fmt.Errorf("%w: DeveloperMode cannot be combined with CLIAdapter", ErrInvalidSelection)
	}
	if cfg.Binary != "" {
		if strings.ContainsRune(cfg.Binary, '\x00') {
			return nil, fmt.Errorf("%w: Binary contains NUL", ErrInvalidSelection)
		}
		if !filepath.IsAbs(cfg.Binary) {
			return nil, fmt.Errorf("%w: Binary must be absolute", ErrInvalidSelection)
		}
	}
	for i, arg := range cfg.ExtraArgs {
		if strings.ContainsRune(arg, '\x00') {
			return nil, fmt.Errorf("%w: ExtraArgs[%d] contains NUL", ErrInvalidSelection, i)
		}
	}

	mode := cfg.LaunchMode
	if mode == LaunchDefault {
		switch providerID {
		case ProviderClaude:
			mode = LaunchStreamingStdio
		case ProviderCodex:
			mode = LaunchAppServer
		case ProviderOpenCode:
			mode = LaunchServeHTTP
		}
	}

	desc, cliFactory, err := selectedRuntime(providerID, mode, cfg.DeveloperMode)
	if err != nil {
		return nil, err
	}
	if cfg.CLIAdapter != nil {
		if got := canonicalAdapterName(cfg.CLIAdapter.Name()); got != string(providerID) {
			return nil, fmt.Errorf("%w: CLIAdapter.Name=%q does not match provider=%q", ErrInvalidSelection, cfg.CLIAdapter.Name(), providerID)
		}
		if err := validateKnownAdapterShape(providerID, mode, cfg.CLIAdapter); err != nil {
			return nil, err
		}
		cli := cfg.CLIAdapter
		cliFactory = func() provider.CLIAdapter { return cli }
	}

	return &selectedAdapter{
		provider:   providerID,
		descriptor: desc,
		cliFactory: cliFactory,
		binary:     cfg.Binary,
		extraArgs:  append([]string(nil), cfg.ExtraArgs...),
	}, nil
}

func validateKnownAdapterShape(providerID Provider, mode LaunchMode, cli provider.CLIAdapter) error {
	valid := true
	switch typed := cli.(type) {
	case *provider.ClaudeAdapter:
		streaming := typed.InputMode == "stream-json" && !typed.Bare && !typed.PTY
		perTurn := !streaming && !typed.PTY
		valid = (mode == LaunchStreamingStdio && streaming) || (mode == LaunchSubprocessPerTurn && perTurn)
	case *provider.CodexAdapter:
		appServer := typed.Mode == "app-server"
		valid = (mode == LaunchAppServer && appServer) || (mode == LaunchSubprocessPerTurn && !appServer)
	case *provider.OpencodeAdapter:
		serveHTTP := typed.Mode == "serve-http"
		valid = (mode == LaunchServeHTTP && serveHTTP) || (mode == LaunchSubprocessPerTurn && !serveHTTP)
	}
	if !valid {
		return fmt.Errorf("%w: %T does not match provider=%q launch_mode=%q", ErrInvalidSelection, cli, providerID, mode)
	}
	return nil
}

func canonicalProvider(value Provider) (Provider, error) {
	switch canonicalAdapterName(string(value)) {
	case "claude":
		return ProviderClaude, nil
	case "codex":
		return ProviderCodex, nil
	case "opencode":
		return ProviderOpenCode, nil
	default:
		return "", fmt.Errorf("%w: provider=%q", ErrUnsupportedSelection, value)
	}
}

func canonicalAdapterName(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude", "claude-code", "claudecode":
		return "claude"
	case "codex":
		return "codex"
	case "opencode", "open-code":
		return "opencode"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func selectedRuntime(
	providerID Provider,
	mode LaunchMode,
	developerMode bool,
) (Descriptor, func() provider.CLIAdapter, error) {
	unsupported := func() (Descriptor, func() provider.CLIAdapter, error) {
		return Descriptor{}, nil, fmt.Errorf("%w: provider=%q launch_mode=%q", ErrUnsupportedSelection, providerID, mode)
	}

	switch providerID {
	case ProviderClaude:
		switch mode {
		case LaunchStreamingStdio:
			factory := func() provider.CLIAdapter { return provider.NewClaudeAdapterStreamingStdio() }
			if developerMode {
				factory = func() provider.CLIAdapter { return provider.NewClaudeAdapterDevStreamingStdio() }
			}
			return Descriptor{
				Provider: string(providerID), Protocol: ProtocolClaudeStreamJSON,
				Transport: TransportStdio, Interrupt: InterruptProcess,
				Channels: []runtimeevents.SourceChannel{runtimeevents.ChannelClaudeStreamJSON},
			}, factory, nil
		case LaunchSubprocessPerTurn:
			factory := func() provider.CLIAdapter { return provider.NewClaudeAdapter() }
			if developerMode {
				factory = func() provider.CLIAdapter { return provider.NewClaudeAdapterDev() }
			}
			return subprocessDescriptor(providerID, runtimeevents.ChannelClaudeStreamJSON), factory, nil
		default:
			return unsupported()
		}

	case ProviderCodex:
		if developerMode {
			return Descriptor{}, nil, fmt.Errorf("%w: DeveloperMode is only defined for Claude", ErrInvalidSelection)
		}
		switch mode {
		case LaunchSubprocessPerTurn:
			return subprocessDescriptor(providerID, runtimeevents.ChannelStdio), func() provider.CLIAdapter {
				return provider.NewCodexAdapter()
			}, nil
		case LaunchAppServer:
			return Descriptor{
				Provider: string(providerID), Protocol: ProtocolCodexAppServer,
				Transport: TransportStdio, Interrupt: InterruptProcess,
				Channels: []runtimeevents.SourceChannel{runtimeevents.ChannelJSONRPC},
			}, func() provider.CLIAdapter { return provider.NewCodexAdapterAppServer() }, nil
		default:
			return unsupported()
		}

	case ProviderOpenCode:
		if developerMode {
			return Descriptor{}, nil, fmt.Errorf("%w: DeveloperMode is only defined for Claude", ErrInvalidSelection)
		}
		switch mode {
		case LaunchSubprocessPerTurn:
			return subprocessDescriptor(providerID, runtimeevents.ChannelStdio), func() provider.CLIAdapter {
				return provider.NewOpencodeAdapter()
			}, nil
		case LaunchServeHTTP:
			return Descriptor{
				Provider: string(providerID), Protocol: ProtocolOpenCodeNative,
				Transport: TransportHTTPSSE, Interrupt: InterruptTurn,
				Channels: []runtimeevents.SourceChannel{runtimeevents.ChannelOpenCodePlugin},
			}, func() provider.CLIAdapter { return provider.NewOpencodeAdapterServeHTTP() }, nil
		default:
			return unsupported()
		}
	}
	return unsupported()
}

func subprocessDescriptor(providerID Provider, channel runtimeevents.SourceChannel) Descriptor {
	return Descriptor{
		Provider: string(providerID), Interrupt: InterruptProcess,
		Channels: []runtimeevents.SourceChannel{channel},
	}
}

type selectedAdapter struct {
	provider   Provider
	descriptor Descriptor
	cliFactory func() provider.CLIAdapter
	binary     string
	extraArgs  []string
}

func (a *selectedAdapter) Name() string { return string(a.provider) }

func (a *selectedAdapter) Describe() Descriptor {
	desc := a.descriptor
	desc.Channels = append([]runtimeevents.SourceChannel(nil), desc.Channels...)
	return desc
}

func (a *selectedAdapter) Resolve(rc ResolveContext) (Spec, error) {
	cli := a.CLIAdapter()
	binary, ok := cli.Detect()
	if !ok || binary == "" {
		binary = string(a.provider)
	}
	return Spec{
		Binary: binary,
		Args:   cli.BuildArgs("", "", ""),
		Env:    rc.Env,
		Cwd:    rc.Cwd,
	}, nil
}

func (a *selectedAdapter) CLIAdapter() provider.CLIAdapter {
	return &configuredCLIAdapter{
		CLIAdapter: a.cliFactory(),
		binary:     a.binary,
		extraArgs:  append([]string(nil), a.extraArgs...),
	}
}

type configuredCLIAdapter struct {
	provider.CLIAdapter
	binary    string
	extraArgs []string
}

func (a *configuredCLIAdapter) BuildArgs(prompt, systemPrompt, cliSessionID string) []string {
	args := a.CLIAdapter.BuildArgs(prompt, systemPrompt, cliSessionID)
	return append(append([]string(nil), args...), a.extraArgs...)
}

func (a *configuredCLIAdapter) Detect() (string, bool) {
	if a.binary != "" {
		return a.binary, true
	}
	return a.CLIAdapter.Detect()
}

var _ RuntimeAdapter = (*selectedAdapter)(nil)
