package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// InitializeResult is the validated subset of ACP's initialize response that
// affects the client lifecycle. Clients must decode this once, before issuing
// authentication or session requests, so protocol negotiation and optional
// method gating cannot diverge between adapters.
type InitializeResult struct {
	ProtocolVersion int
	AuthMethods     []AuthMethod
	LoadSession     bool
	SessionClose    bool
}

// AuthMethod is one authentication choice advertised by an ACP agent.
type AuthMethod struct {
	ID   string
	Type string
}

// ParseInitializeResult validates an initialize result against the only ACP
// version this module currently speaks and returns its lifecycle capabilities.
// A missing protocolVersion decodes as zero and is rejected as unsupported.
func ParseInitializeResult(result json.RawMessage, supportedVersion int) (InitializeResult, error) {
	if len(result) == 0 {
		return InitializeResult{}, errors.New("acp: initialize returned an empty result")
	}
	var wire struct {
		ProtocolVersion int `json:"protocolVersion"`
		AuthMethods     []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"authMethods"`
		AgentCapabilities struct {
			LoadSession         bool `json:"loadSession"`
			SessionCapabilities struct {
				Close json.RawMessage `json:"close"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(result, &wire); err != nil {
		return InitializeResult{}, fmt.Errorf("acp: decode initialize result: %w", err)
	}
	if wire.ProtocolVersion != supportedVersion {
		return InitializeResult{}, fmt.Errorf("acp: unsupported protocol version %d (want %d)", wire.ProtocolVersion, supportedVersion)
	}
	parsed := InitializeResult{
		ProtocolVersion: wire.ProtocolVersion,
		LoadSession:     wire.AgentCapabilities.LoadSession,
	}
	for _, method := range wire.AuthMethods {
		parsed.AuthMethods = append(parsed.AuthMethods, AuthMethod{ID: method.ID, Type: method.Type})
	}
	closeCapability := wire.AgentCapabilities.SessionCapabilities.Close
	if len(closeCapability) > 0 {
		var capability map[string]any
		parsed.SessionClose = json.Unmarshal(closeCapability, &capability) == nil && capability != nil
	}
	return parsed, nil
}

// Client is the ACP client abstraction: the stable, single Go interface
// a Hollis host drives an underlying ACP-speaking agent through,
// regardless of whether the concrete implementation underneath is a
// thin, direct wire connection to a native ACP agent or a third-party
// bridge library speaking ACP on a non-native CLI's behalf. See the
// package doc for the full architecture and the two implementation
// kinds this interface is designed to plug in swappably.
//
// A Client is single-session, mirroring wrapper.Wrapper's own
// single-use contract: construct one per launched ACP session, call
// Launch once, then Prompt/Cancel any number of times across the
// session's lifetime. Manager is the normal owner and guarantees that Close
// reaches the client at most once even when explicit close races EOF or exit.
type Client interface {
	// Launch starts (or connects to) the underlying ACP-speaking agent
	// and performs initialize, optional authenticate, session/new or
	// session/load, and optional session configuration,
	// returning once the session is ready to accept a Prompt. Launch
	// does not block for the session's full lifetime — Events (below)
	// streams activity for as long as the session is alive, and
	// Prompt/Cancel remain callable after Launch returns.
	Launch(ctx context.Context, params LaunchParams) error

	// Prompt sends one user turn via ACP's `session/prompt` and returns
	// once the implementation has accepted the turn — NOT once the turn
	// has completed. Turn completion, deltas, tool calls, and
	// permission requests surface asynchronously through Events, the
	// same way any other adapters.Adapter's turn activity does
	// (runtimeevents.KindAgentDelta / KindAgentToolUse /
	// KindTurnCompleted / KindTurnFailed, ...).
	Prompt(ctx context.Context, prompt string) error

	// Cancel requests cancellation of the in-flight turn via ACP's
	// `session/cancel`. Despite the wire method's name, ACP
	// session/cancel is spec'd as turn-scoped, not session-scoped: the
	// session survives and remains able to accept the next Prompt after
	// Cancel returns. Callers must route Cancel through Nanite's own
	// Turn.Cancel path, never Session.Stop — see
	// docs/engineering/GLOSSARY.md's "Turn.Cancel vs. Session.Stop..."
	// entry in the sibling Nanite repo. Use Close (below), not Cancel,
	// to end the whole session.
	//
	// Cancel's real effect is implementation-dependent — see
	// InterruptCapability.
	Cancel(ctx context.Context) error

	// Events returns the channel this Client emits activity on for the
	// life of the launched session. Values are runtimeevents.Event with
	// Kind, Payload, and (when turn-scoped) TurnID populated by the
	// implementation; ID, Sequence, SessionID, App, and Process are left
	// zero for the caller to fill in via its own
	// runtimeevents.Emitter/activity.Bridge — the same (kind, payload)
	// split wrapper/event_translator.go already uses to translate every
	// other adapter's native events, reused here rather than duplicated,
	// per this package's doc comment. The channel closes when the
	// session ends (the underlying process/connection exits, or Close
	// is called).
	Events() <-chan runtimeevents.Event

	// InterruptCapability reports what Cancel actually achieves for
	// this Client, using the same vocabulary
	// adapters.Descriptor.Interrupt uses (adapters.InterruptCapability:
	// none / process / turn / steer). This is a per-implementation
	// fact, not a protocol-level guarantee — two Clients speaking the
	// same ACP wire protocol may report different values depending on
	// whether the concrete implementation's session/cancel is actually
	// wired through to the underlying provider's own native interrupt,
	// or only acknowledges cancellation at the wire level while the
	// turn finishes anyway. See
	// docs/engineering/architecture/17-acp.md's "Known limitations"
	// section in the sibling Nanite repo. An ACP-backed
	// adapters.Adapter's Describe() must mirror this value into its
	// Descriptor.Interrupt field — see DescriptorFor.
	InterruptCapability() adapters.InterruptCapability

	// Close releases whatever resources Launch acquired. Implementations send
	// session/close first when the agent advertised that optional capability,
	// then terminate the underlying process/connection if still alive. Close
	// ends the whole session (the analog of Nanite's Session.Stop);
	// Cancel (above) ends only the in-flight turn (the analog of
	// Nanite's Turn.Cancel). Safe to call even if Launch was never
	// called or failed.
	Close(ctx context.Context) error
}

// LaunchParams carries what a Client needs to start (or connect to) the
// underlying ACP-speaking agent and perform the `initialize`/
// `session/new` handshake. Beyond Cwd, fields are
// implementation-defined — concrete implementations (native or
// bridge-mediated) ignore fields that don't apply to their transport
// (e.g. a TCP-daemon implementation that connects to an
// already-running process ignores Env).
type LaunchParams struct {
	// Cwd is the working directory the underlying agent should treat as
	// its project root — the ACP analog of adapters.ResolveContext.Cwd.
	Cwd string

	// Env is the environment to launch a spawned subprocess with
	// (native ACP implementations). Bridge-mediated implementations
	// that don't own subprocess spawning ignore this.
	Env []string

	// SystemPrompt, when non-empty, is prepended to the first ACP prompt. ACP
	// v1 has no dedicated client-supplied system-prompt field in session/new.
	SystemPrompt string

	// SessionIDPreset, when non-empty, is the provider-side session id
	// an implementation should attempt to resume via ACP's
	// `session/load`, mirroring wrapper.Config.SessionIDPreset's
	// existing convention for non-ACP adapters. Implementations that
	// don't advertise loadSession start a fresh session. Once loadSession
	// is advertised, a failed load is returned rather than silently losing
	// continuity through a session/new fallback.
	SessionIDPreset string

	// AuthMethodID, when non-empty, selects one agent-managed authentication
	// method advertised by initialize and calls ACP `authenticate` before
	// creating/loading the session. Terminal authentication methods are not
	// driven implicitly: they require an interactive terminal outside this
	// stdio/TCP client lifecycle.
	AuthMethodID string

	// SessionModeID, when non-empty, is applied with `session/set_mode`
	// after session/new or session/load completes.
	SessionModeID string

	// SessionConfig applies ACP session configuration options after the
	// session is created/loaded. Keys are config option ids and values are
	// either a string value id or a bool toggle. Iteration order is sorted so
	// wire traces and partial-failure behavior are deterministic.
	SessionConfig map[string]any

	// OnDiagnostic receives bounded, safely-redacted stderr and protocol
	// diagnostics. It is opt-in and must return quickly; ordinary activity
	// continues to flow through Events.
	OnDiagnostic func(Diagnostic)
}

// ProviderSessionID returns the provider-assigned id when a concrete client
// exposes it. It accepts Client rather than requiring another method on the
// base interface so existing third-party Client implementations remain source
// compatible; all adapters shipped by this module implement the readback.
func ProviderSessionID(client Client) string {
	if identity, ok := client.(interface{ ProviderSessionID() string }); ok {
		return identity.ProviderSessionID()
	}
	return ""
}

// InitializeSupportsSessionClose reports whether an initialize result
// advertises agentCapabilities.sessionCapabilities.close. Clients use it to
// send session/close before tearing down the transport without breaking older
// ACP agents that do not implement the optional method.
func InitializeSupportsSessionClose(result json.RawMessage) bool {
	// Kept as a compatibility helper for external clients that only need this
	// one bit. Shipped clients use ParseInitializeResult so validation and all
	// lifecycle capabilities come from one decode.
	var response struct {
		AgentCapabilities struct {
			SessionCapabilities struct {
				Close json.RawMessage `json:"close"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	if json.Unmarshal(result, &response) != nil {
		return false
	}
	closeCapability := response.AgentCapabilities.SessionCapabilities.Close
	if len(closeCapability) == 0 {
		return false
	}
	var capability map[string]any
	return json.Unmarshal(closeCapability, &capability) == nil && capability != nil
}

// DescriptorFor builds the [adapters.Descriptor] an ACP-backed
// [adapters.Adapter]'s Describe() should return, given the underlying
// Client's advertised InterruptCapability and which Transport this
// implementation actually uses (adapters.TransportStdio for native
// direct-wire implementations like OpenCode/Copilot CLI's stdio mode,
// adapters.TransportTCP for daemon-mode ones like Copilot CLI's `--acp`
// TCP daemon). providerName is the upstream agent identity mirrored
// into runtimeevents.Process.Provider (e.g. "opencode", "copilot",
// "claude").
//
// This is the one place a concrete ACP-backed Adapter needs to thread
// the Client's real, per-implementation InterruptCapability into the
// Descriptor seam task 02 added — Protocol is always
// [adapters.ProtocolACP] regardless of which underlying agent or
// Transport is selected, since ACP is a protocol, not a transport (see
// [adapters.Protocol]'s own doc comment).
func DescriptorFor(client Client, providerName string, transport adapters.Transport) adapters.Descriptor {
	return adapters.Descriptor{
		Provider:  providerName,
		Protocol:  adapters.ProtocolACP,
		Transport: transport,
		Interrupt: client.InterruptCapability(),
		Channels:  []runtimeevents.SourceChannel{runtimeevents.ChannelJSONRPC},
	}
}
