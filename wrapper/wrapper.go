package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	llmtypes "github.com/hollis-labs/go-llm-types"
	pevents "github.com/hollis-labs/go-providers/provider/events"
	sandboxprofile "github.com/hollis-labs/go-sandbox/sandbox"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/filters"
	"github.com/hollis-labs/go-agent-wrapper/plant"
	"github.com/hollis-labs/go-agent-wrapper/policy"
	"github.com/hollis-labs/go-agent-wrapper/sandbox"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Config describes a single wrapped-process invocation. Build a Config,
// pass it to [New], then call [Wrapper.Run] (which drives the full
// start-wait-emit-exit lifecycle) or [Wrapper.SendInput] / [Wrapper.Stop]
// for finer control alongside a concurrent [Wrapper.Run].
//
// Required fields: App, Adapter, Activity, Workdir.
//
// Everything else is optional — zero values mean "no planting", "no
// sandbox profile", "no policy observer", "no filter pipeline". The wrapper
// degrades cleanly into a pure passthrough when no subsystems are configured.
//
// Non-ACP Adapter values must implement [adapters.RuntimeAdapter]. ACP
// adapters must implement [acp.ClientAdapter], allowing Wrapper to own their
// protocol session through [acp.Manager].
type Config struct {
	// App identifies the calling application ("nanite", "torque",
	// "tachyon", ...). Propagated into every emitted Event.
	App string

	// Adapter is the provider integration (Claude, Codex, OpenCode, ...).
	// Required. Must implement [adapters.RuntimeAdapter] for native runtimes or
	// [acp.ClientAdapter] when Describe reports [adapters.ProtocolACP].
	Adapter adapters.Adapter

	// Activity is the runtime-event emitter the wrapper uses to surface
	// lifecycle, IO, agent, policy, plant, and sandbox events.
	// Required.
	Activity *activity.Bridge

	// Workdir is the absolute path used as the spawned process's
	// working directory and as the workspace argument to
	// [sandbox.Applier.Apply]. Required by the agentkit streaming-stdio
	// runtime.
	Workdir string

	// Environment defines exactly how the child process environment is
	// derived. The zero value preserves the historical behavior (inherit the
	// wrapper process environment). Use EnvironmentReplace with a composed,
	// allowlisted Set for strict isolation; use EnvironmentMerge only when
	// intentionally retaining ambient variables. The materialized environment
	// is passed to native and ACP subprocesses without shell construction.
	Environment ChildEnvironment

	// BootDir, when non-empty, overrides the per-session boot directory
	// the [Planter] writes into. Empty defaults to
	// <Workdir>/.wrapper-boot/<SessionID>/. Ignored when Planter is
	// nil. The wrapper does NOT clean up the boot dir on Run exit —
	// the caller owns retention.
	BootDir string

	// SessionID overrides the wrapper-session identity used in emitted
	// events. Empty allocates a fresh ID via
	// [runtimeevents.NewSessionID].
	SessionID string

	// Planter, when set, runs before exec to lay down per-session boot
	// files (MCP config, provider settings, hooks/plugins, recovery
	// prompts). [Wrapper.Run] emits plant.started before the call and
	// plant.completed after it (including on error). Plant errors abort
	// Run before the agentkit runtime is constructed.
	//
	// The wrapper's planting is complementary to agentkit's own
	// [agentsessions.StartOptions.AutoPlantBootDir] — both can run for
	// the same session, with adapter-specific semantics deciding which
	// files the spawned process actually consumes.
	Planter plant.Planter

	// PlantSpec describes what Planter should lay down. Ignored when
	// Planter is nil.
	PlantSpec plant.Spec

	// Sandbox, when set, runs [sandbox.Applier.Apply] against the
	// session's child PID after [Runtime.Start] returns and emits a
	// sandbox.applied event with the result. Apply errors abort Run
	// (the session is stopped and the error surfaced).
	//
	// Note: for the adapter runtime (subprocess-per-turn) the PID is
	// zero between turns — the Applier's pre-spawn enforcement story
	// belongs in [agentsessions.StartOptions.Profile] for that path.
	// The wrapper's Sandbox is the right hook for runtimes where a
	// long-lived child has a stable PID (PTY, streaming-stdio,
	// jsonrpc-stdio).
	Sandbox sandbox.Applier

	// SandboxProfile is forwarded to agentsessions.StartOptions.Profile so
	// runtimes that support pre-spawn go-sandbox wrapping can constrain the
	// child before exec. The zero-value profile (empty ID) disables this path.
	SandboxProfile sandboxprofile.Profile

	// WorkspaceDir is the per-session persistent root forwarded to
	// agentsessions.StartOptions.WorkspaceDir — distinct from Workdir
	// (the spawned process's cwd). Every one of agentkit's
	// streaming-stdio, jsonrpc-stdio, and serve-http runtime kinds
	// (i.e. every real shipped adapter — Claude, Codex, OpenCode) hard-
	// errors before spawning anything when both WorkspaceDir and LogPath
	// are empty. When both are left empty here, [Wrapper.Run]
	// synthesizes <Workdir>/.wrapper-workspace/<SessionID> — the same
	// "degrade cleanly on a zero value" contract this Config already
	// gives BootDir (above), rather than trading it for a required-field
	// error a caller has to know about agentkit internals to avoid.
	WorkspaceDir string

	// LogPath overrides the per-session log file path forwarded to
	// agentsessions.StartOptions.LogPath. Empty defers to WorkspaceDir
	// (agentkit derives <WorkspaceDir>/logs/session.log). See
	// WorkspaceDir above for the synthesized default when both are
	// empty.
	LogPath string

	// SessionIDPreset, when non-empty, is forwarded to
	// agentsessions.StartOptions.SessionIDPreset — the provider-side
	// session id an adapter should resume from on its very first turn
	// (e.g. Claude streaming-stdio's `--resume <id>`). Adapters that
	// don't understand resume ignore it silently.
	SessionIDPreset string

	// SystemPrompt is prepended to the first prompt of an ACP session. It is
	// currently ignored by non-ACP runtime paths.
	SystemPrompt string

	// ACPManager, when set, is the authoritative registry for ACP sessions
	// launched by this Wrapper. Sharing one Manager across Wrappers gives a
	// host lookup, liveness, cancel, close and shutdown without a duplicate
	// host-side registry. A nil value creates a manager owned by this Wrapper.
	ACPManager *acp.Manager

	// ACPAuthMethodID selects an agent-managed authentication method returned
	// by initialize. Empty uses the ACP agent's existing authenticated state.
	ACPAuthMethodID string

	// ACPSessionModeID and ACPSessionConfig are applied after session/new or
	// session/load and before the first prompt.
	ACPSessionModeID string
	ACPSessionConfig map[string]any

	// OnACPDiagnostic receives bounded, redacted protocol diagnostics. Raw
	// stderr/protocol bytes are never placed on the ordinary event stream.
	OnACPDiagnostic func(acp.Diagnostic)

	// ACPBestEffortPermissionRequestResponder answers ACP
	// session/request_permission calls when the selected agent chooses to ask.
	// Nil retains the selected client's established non-blocking decline (see
	// [acp.LaunchParams]). The seam is
	// deliberately best-effort and does not replace authoritative host gates:
	// providers may execute operation classes without requesting permission.
	ACPBestEffortPermissionRequestResponder acp.BestEffortPermissionRequestResponder

	// OnSessionID, when non-nil, is invoked the first time the running
	// session observes a provider-assigned session id — in addition to,
	// not instead of, [Wrapper.Run]'s own unconditional
	// Process.ProviderSessionID rebind (see Run's doc comment). This
	// field exists because not every agentkit runtime's session-id
	// delivery reaches EventFanout: the serve-http runtime's initial
	// session-creation call (agentkit's serveHTTPSession.createSession —
	// OpenCode's primary, first-session delivery point) invokes
	// StartOptions.OnSessionID directly and never pushes a matching
	// EventFanout frame, so a caller that only observes the
	// activity.Bridge's Sink would never see that particular session id
	// without this field. (Claude's streaming-stdio and OpenCode's
	// secondary SSE session.created path fire OnSessionID and
	// EventFanout together from the same observed event, so for those
	// two the Sink-observed rebind alone would have sufficed — this
	// field closes the one path where it doesn't.) Called from the
	// adapter's own read goroutine — callers must not block inside it.
	OnSessionID func(id string)

	// AutoFireFirstTurn, when true, is forwarded to
	// agentsessions.StartOptions.AutoFireFirstTurn together with
	// FirstTurnPayload (as bytes) — the runtime delivers FirstTurnPayload
	// as the first SendInput automatically once Start succeeds, closing
	// the Launch/SendInput race a caller-driven first turn is otherwise
	// exposed to. Needed by every ModeOneShot/ModeSubagent/ModeBackground
	// boot, which relies on the runtime auto-delivering the kickoff
	// payload as the first turn rather than a caller racing its own
	// SendInput against Start's return.
	AutoFireFirstTurn bool

	// FirstTurnPayload is the kickoff string sent on the auto-fired
	// first turn when AutoFireFirstTurn is true. Ignored when
	// AutoFireFirstTurn is false.
	FirstTurnPayload string

	// PolicyObserver, when set, is consulted by the wrapper's translator
	// goroutine after every observed [runtimeevents.KindAgentToolUse] event.
	// [Wrapper.Run] builds a [policy.Observation] from the tool use and emits
	// the matching legacy policy.nudge / policy.rewrite / policy.block /
	// policy.approval_requested runtime event, correlated via ParentID.
	// [policy.RecommendationNone] emits no derived event.
	//
	// Findings are observation metadata only. Even a
	// [policy.RecommendationBlock] or [policy.RecommendationRewrite] does not
	// change, delay, or prevent the child operation; the event is emitted
	// after agent.tool_use. Enforceable host gates must run before execution.
	//
	// Observers must be cheap and synchronous on the hot path. See
	// [policy.Observer] for the contract and host/ACP boundary.
	PolicyObserver policy.Observer

	// Filters, when set, runs the harness filter pipeline against agent
	// text, tool output, command output, and envelopes. Nil means no
	// filtering. Repairs replace the wrapper-emitted event payload; raw
	// child execution is unchanged.
	Filters filters.Pipeline

	// HeartbeatInterval, when positive, emits session.heartbeat events at
	// this cadence while Run is active. Zero disables wrapper-synthesized
	// heartbeats; adapter-provided heartbeat events may still be surfaced.
	HeartbeatInterval time.Duration
}

// Wrapper owns the launch boundary for one wrapped subprocess. A
// Wrapper is single-use: build via [New], run, observe the
// [activity.Bridge] event stream, dispose. To wrap a new process,
// build a new Wrapper.
type Wrapper struct {
	cfg       Config
	sessionID string

	inputMu      sync.Mutex
	inputWG      sync.WaitGroup
	inputsClosed bool

	sessMu     sync.RWMutex
	session    agentsessions.Session
	acpSession *acp.Session
	acpManager *acp.Manager
	// acpProviderSessionID is retained for postmortem readback after the
	// live acpSession control reference has been cleared.
	acpProviderSessionID string
	typedSource          runtimeevents.Source // set in Run; used by SendInput/Stop for derived events
	rawSource            runtimeevents.Source // set in Run; used for stdin.write events
}

// New validates cfg and returns a Wrapper ready for [Wrapper.Run].
func New(cfg Config) (*Wrapper, error) {
	if cfg.App == "" {
		return nil, errors.New("wrapper: Config.App is required")
	}
	if cfg.Adapter == nil {
		return nil, errors.New("wrapper: Config.Adapter is required")
	}
	if cfg.Activity == nil {
		return nil, errors.New("wrapper: Config.Activity is required")
	}
	if _, _, err := cfg.Environment.resolve(nil); err != nil {
		return nil, err
	}
	cfg.Environment.Allowlist = cloneStringSlice(cfg.Environment.Allowlist)
	cfg.Environment.Set = cloneStringSlice(cfg.Environment.Set)
	cfg.Environment.Unset = cloneStringSlice(cfg.Environment.Unset)
	sessionID := cfg.SessionID
	if sessionID == "" {
		sessionID = runtimeevents.NewSessionID()
	}
	return &Wrapper{cfg: cfg, sessionID: sessionID}, nil
}

// SessionID returns the wrapper-session identity propagated into every
// emitted [runtimeevents.Event]. Stable for the lifetime of the
// Wrapper.
func (w *Wrapper) SessionID() string { return w.sessionID }

// Run drives the full session lifecycle:
//
//  1. Routes ACP adapters through [acp.Manager]; other adapters through their
//     [adapters.RuntimeAdapter].
//  2. For non-ACP adapters, maps [adapters.Descriptor.Protocol] + [adapters.Descriptor.Transport]
//     to agentkit [agentsessions.Capabilities].
//  3. Constructs an [agentsessions.Runtime] via NewFromAdapter,
//     calls Prepare, and Start.
//  4. Fans [llmtypes.StreamEvent]s through [translateStreamEvent]
//     and emits the resulting [runtimeevents.Event]s via the
//     [activity.Bridge].
//  5. Blocks on Session.Wait. Cancellation of ctx triggers
//     Session.Stop.
//
// Emits these envelope kinds: session.ready, process.started,
// process.exited, plus per-stream-event translations (agent.delta,
// agent.tool_use, turn.completed, turn.failed). Provider session_id
// frames re-bind the [activity.Bridge] Process.ProviderSessionID
// rather than producing an event; so does [Config.OnSessionID] firing
// (see its doc comment for why both paths exist).
//
// When Config.WorkspaceDir and Config.LogPath are both empty, Run
// synthesizes <Workdir>/.wrapper-workspace/<SessionID> as the
// WorkspaceDir forwarded to agentsessions.StartOptions — every real
// shipped adapter's runtime kind (streaming-stdio, jsonrpc-stdio,
// serve-http) requires one of the two to be set before it will spawn
// anything.
//
// ACP sessions normalize unexpected disconnect, malformed stream, child exit,
// and cancellation outcomes through [acp.LifecycleError]. Returns the
// underlying wait error if any; ctx.Err() is preserved when cancellation
// stopped the wrapper.
func (w *Wrapper) Run(ctx context.Context) error {
	baseEnv, environmentExplicit, err := w.cfg.Environment.resolve(os.Environ())
	if err != nil {
		return err
	}
	desc := w.cfg.Adapter.Describe()
	if desc.Protocol == adapters.ProtocolACP {
		return w.runACP(ctx, desc, baseEnv, environmentExplicit)
	}
	ra, ok := w.cfg.Adapter.(adapters.RuntimeAdapter)
	if !ok {
		return fmt.Errorf("%w: adapter %q", ErrAdapterNotRuntime, w.cfg.Adapter.Name())
	}
	if w.cfg.Workdir == "" {
		return errors.New("wrapper: Config.Workdir is required")
	}

	caps, err := runtimeCaps(desc.Protocol, desc.Transport)
	if err != nil {
		return err
	}

	w.cfg.Activity.Bind(w.cfg.App, w.sessionID, runtimeevents.Process{
		Provider: desc.Provider,
		Runtime:  legacyRuntimeToken(desc.Protocol, desc.Transport),
	})
	source := runtimeevents.Source{
		Channel:    runtimeSourceChannel(desc.Protocol, desc.Transport),
		Confidence: runtimeevents.ConfidenceExact,
	}
	rawSource := runtimeevents.Source{
		Channel:    rawSourceChannel(desc.Protocol, desc.Transport),
		Confidence: runtimeevents.ConfidenceExact,
	}
	w.sessMu.Lock()
	w.typedSource = source
	w.rawSource = rawSource
	w.sessMu.Unlock()

	// Resolve the wrapper-level Spec to validate the adapter's exec-shape
	// contract and surface PTY/no-PTY mismatches early. The agentkit runtime
	// constructs its own binary and argv via CLIAdapter, while Spec.Env is an
	// honored final replacement for the Config-derived base environment.
	spec, err := w.cfg.Adapter.Resolve(adapters.ResolveContext{
		BootDir: w.cfg.BootDir,
		Cwd:     w.cfg.Workdir,
		Env:     baseEnv,
		PTY:     caps.PTY,
	})
	if err != nil {
		return fmt.Errorf("wrapper: adapter Resolve: %w", err)
	}
	childEnv, adapterEnvironmentExplicit, err := resolvedSpecEnvironment(baseEnv, spec.Env)
	if err != nil {
		return fmt.Errorf("wrapper: adapter Resolve environment: %w", err)
	}
	if len(childEnv) == 0 && (environmentExplicit || adapterEnvironmentExplicit) && capsUsesLongLivedProcess(caps) {
		childEnv = []string{nonInheritingEmptyEnvironment}
	}

	if err := w.runPlanter(ctx, source); err != nil {
		return err
	}

	runtime, err := agentsessions.NewFromAdapter(agentsessions.AdapterRuntimeConfig{
		ID:      "wrapper-" + w.cfg.Adapter.Name(),
		Kind:    "cli",
		Adapter: ra.CLIAdapter(),
		Caps:    caps,
	})
	if err != nil {
		return fmt.Errorf("wrapper: agentsessions.NewFromAdapter: %w", err)
	}
	if err := runtime.Prepare(ctx); err != nil {
		return fmt.Errorf("wrapper: runtime.Prepare: %w", err)
	}

	fanout := make(chan llmtypes.StreamEvent, 128)

	stdoutStream := newStreamWriter(ctx, w.cfg.Activity, rawSource,
		runtimeevents.KindStdoutRaw, runtimeevents.KindStdoutLine, w.cfg.Filters)
	stderrStream := newStreamWriter(ctx, w.cfg.Activity, rawSource,
		runtimeevents.KindStderrRaw, runtimeevents.KindStderrLine, w.cfg.Filters)

	var turnMu sync.Mutex
	var currentTurnID string
	emitObserved := func(kind runtimeevents.EventKind, payload any, ev llmtypes.StreamEvent) {
		payload = w.filterPayload(ctx, kind, payload)
		turnMu.Lock()
		defer turnMu.Unlock()

		if currentTurnID == "" && isTurnInternal(kind) {
			currentTurnID = runtimeevents.NewTurnID()
			_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindTurnStarted, source, nil,
				runtimeevents.WithTurnID(currentTurnID))
			_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindSessionProcessing, source,
				map[string]any{"turn_id": currentTurnID},
				runtimeevents.WithTurnID(currentTurnID))
		}

		eventID := runtimeevents.NewEventID()
		opts := []runtimeevents.EmitOption{runtimeevents.WithID(eventID)}
		if currentTurnID != "" && isTurnScoped(kind) {
			opts = append(opts, runtimeevents.WithTurnID(currentTurnID))
		}
		_ = w.cfg.Activity.Emit(ctx, kind, source, payload, opts...)

		if kind == runtimeevents.KindAgentToolUse {
			w.observeToolUsePolicy(ctx, source, ev, eventID, currentTurnID)
		}

		if kind == runtimeevents.KindTurnCompleted || kind == runtimeevents.KindTurnFailed {
			closedTurnID := currentTurnID
			currentTurnID = ""
			if closedTurnID != "" {
				_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindSessionIdle, source,
					map[string]any{"turn_id": closedTurnID},
					runtimeevents.WithTurnID(closedTurnID))
			}
		}
	}
	emitProviderObserved := func(kind runtimeevents.EventKind, payload any) {
		payload = w.filterPayload(ctx, kind, payload)
		turnMu.Lock()
		defer turnMu.Unlock()

		if currentTurnID == "" && isTurnInternal(kind) {
			currentTurnID = runtimeevents.NewTurnID()
			_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindTurnStarted, source, nil,
				runtimeevents.WithTurnID(currentTurnID))
			_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindSessionProcessing, source,
				map[string]any{"turn_id": currentTurnID},
				runtimeevents.WithTurnID(currentTurnID))
		}

		opts := []runtimeevents.EmitOption{}
		if currentTurnID != "" && isTurnScoped(kind) {
			opts = append(opts, runtimeevents.WithTurnID(currentTurnID))
		}
		_ = w.cfg.Activity.Emit(ctx, kind, source, payload, opts...)
	}

	heartbeatStop := make(chan struct{})
	if w.cfg.HeartbeatInterval > 0 {
		go func() {
			ticker := time.NewTicker(w.cfg.HeartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindSessionHeartbeat, source,
						map[string]any{"last_activity_at": time.Now().UTC()})
				case <-heartbeatStop:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	defer close(heartbeatStop)

	workspaceDir, logPath := resolveWorkspaceLogPath(w.cfg.Workdir, w.sessionID, w.cfg.WorkspaceDir, w.cfg.LogPath)

	session, err := runtime.Start(ctx, agentsessions.StartOptions{
		Workdir:           w.cfg.Workdir,
		WorkspaceDir:      workspaceDir,
		LogPath:           logPath,
		Env:               childEnv,
		EventFanout:       fanout,
		Fanout:            stdoutStream,
		Stderr:            stderrStream,
		Profile:           w.cfg.SandboxProfile,
		SessionIDPreset:   w.cfg.SessionIDPreset,
		AutoFireFirstTurn: w.cfg.AutoFireFirstTurn,
		FirstTurnPayload:  []byte(w.cfg.FirstTurnPayload),
		OnSessionID: func(id string) {
			if id == "" {
				return
			}
			// Unconditional rebind: keeps the Sink-observed
			// Process.ProviderSessionID path (below, in the fanout
			// consumer loop) correct even for runtimes/paths — e.g.
			// serve-http's createSession — that call OnSessionID
			// without also pushing an EventFanout frame. See Config.
			// OnSessionID's doc comment.
			w.cfg.Activity.Emitter().SetProviderSessionID(id)
			if w.cfg.OnSessionID != nil {
				w.cfg.OnSessionID(id)
			}
		},
		TypedEventCallback: func(ev pevents.Event) {
			kind, payload, mapped := translateProviderEvent(ev)
			if !mapped {
				return
			}
			emitProviderObserved(kind, payload)
		},
		JsonRpcRequestHook: func(method string, params json.RawMessage) (any, *agentsessions.JsonRpcError) {
			turnMu.Lock()
			if currentTurnID == "" {
				currentTurnID = runtimeevents.NewTurnID()
				_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindTurnStarted, source, nil,
					runtimeevents.WithTurnID(currentTurnID))
				_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindSessionProcessing, source,
					map[string]any{"turn_id": currentTurnID},
					runtimeevents.WithTurnID(currentTurnID))
			}
			turnID := currentTurnID
			turnMu.Unlock()

			requestID := runtimeevents.NewEventID()
			payload := map[string]any{"method": method, "params": params}
			_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindAgentPermissionRequested, source, payload,
				runtimeevents.WithID(requestID), runtimeevents.WithTurnID(turnID))
			rpcErr := &agentsessions.JsonRpcError{
				Code:    -32601,
				Message: "go-agent-wrapper: no approval handler configured for server-initiated request " + method,
			}
			_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindAgentPermissionResolved, source,
				map[string]any{"method": method, "allowed": false, "error": rpcErr.Message},
				runtimeevents.WithParentID(requestID), runtimeevents.WithTurnID(turnID))
			return nil, rpcErr
		},
	})
	if err != nil {
		return fmt.Errorf("wrapper: runtime.Start: %w", err)
	}
	w.sessMu.Lock()
	w.session = session
	w.sessMu.Unlock()

	_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindSessionReady, source, nil)
	if pid := session.Health().PID; pid != 0 {
		_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindProcessStarted, source,
			map[string]any{"pid": pid})
	}

	translatorDone := make(chan struct{})
	go func() {
		defer close(translatorDone)
		for ev := range fanout {
			if ev.Type == llmtypes.EventSessionID && ev.SessionID != "" {
				// Provider-side session ID: rebind the Process so
				// subsequent emissions carry it. Use the locked
				// setter — stream writers may concurrently emit
				// events that read Process.
				w.cfg.Activity.Emitter().SetProviderSessionID(ev.SessionID)
				continue
			}
			ev = w.filterStreamEvent(ctx, ev)
			kind, payload, mapped := translateStreamEvent(ev)
			if !mapped {
				continue
			}
			emitObserved(kind, payload, ev)
		}
	}()

	if err := w.runSandbox(ctx, source, session); err != nil {
		// Close admission before stopping, then wait for the logical session
		// and every already-accepted input to unwind before closing fanout.
		// This is the same ownership ordering as the normal exit path.
		w.closeInputAdmission()
		_ = session.Stop(context.Background())
		exitCode, waitErr := session.Wait()
		w.inputWG.Wait()
		close(fanout)
		<-translatorDone
		exitPayload := map[string]any{"exit_code": exitCode, "error": err.Error()}
		if waitErr != nil {
			exitPayload["wait_error"] = waitErr.Error()
		}
		_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindProcessExited, source, exitPayload)
		return err
	}

	// Watch ctx for cancellation so a stuck session unblocks. The
	// watcher emits the interrupt event pair around session.Stop so
	// downstream consumers can see why the session ended.
	stopWatcher := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = w.requestInterrupt(context.Background(), source, session, "ctx_cancel")
		case <-stopWatcher:
		}
	}()

	exitCode, waitErr := session.Wait()
	close(stopWatcher)
	w.closeInputAdmission()
	w.inputWG.Wait()
	close(fanout)
	<-translatorDone

	exitPayload := map[string]any{"exit_code": exitCode}
	if waitErr != nil {
		exitPayload["error"] = waitErr.Error()
	}
	_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindProcessExited, source, exitPayload)

	if waitErr != nil {
		return fmt.Errorf("wrapper: session exited with error: %w", waitErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return nil
}

// SendInput pushes input bytes into the running session. Emits a
// stdin.write event with the bytes (before forwarding to the
// session) so audit and replay capture caller-issued input.
// Returns [ErrSessionNotStarted] if [Wrapper.Run] has not started a
// session yet.
func (w *Wrapper) SendInput(ctx context.Context, data []byte) error {
	w.inputMu.Lock()
	if w.inputsClosed {
		w.inputMu.Unlock()
		return ErrSessionNotStarted
	}
	w.sessMu.RLock()
	session := w.session
	acpSession := w.acpSession
	rawSource := w.rawSource
	w.sessMu.RUnlock()
	if session == nil && acpSession == nil {
		w.inputMu.Unlock()
		return ErrSessionNotStarted
	}
	w.inputWG.Add(1)
	w.inputMu.Unlock()
	defer w.inputWG.Done()
	// Emit before forwarding so the event sequence reflects intent
	// even when SendInput errors (the input was attempted regardless).
	_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindStdinWrite, rawSource,
		map[string]any{
			"bytes": string(data),
		})
	if acpSession != nil {
		return acpSession.Prompt(ctx, string(data))
	}
	return session.SendInput(ctx, data)
}

func (w *Wrapper) closeInputAdmission() {
	w.inputMu.Lock()
	w.inputsClosed = true
	w.inputMu.Unlock()
}

// Stop requests termination of the running session and emits the
// interrupt.requested → interrupt.acknowledged event pair around the
// underlying session.Stop call. The two events are correlated via
// ParentID. Returns nil if no session is running (e.g., Run already
// returned).
func (w *Wrapper) Stop(ctx context.Context) error {
	w.sessMu.RLock()
	session := w.session
	acpSession := w.acpSession
	source := w.typedSource
	w.sessMu.RUnlock()
	if session == nil && acpSession == nil {
		return nil
	}
	if acpSession != nil {
		return w.requestACPInterrupt(ctx, source, acpSession, "user_stop", true)
	}
	return w.requestInterrupt(ctx, source, session, "user_stop")
}

// CancelTurn requests ACP session/cancel without closing the session. Native
// adapters do not expose a distinct turn-cancel primitive and return
// ErrTurnCancelUnsupported.
func (w *Wrapper) CancelTurn(ctx context.Context) error {
	w.sessMu.RLock()
	session := w.acpSession
	source := w.typedSource
	w.sessMu.RUnlock()
	if session == nil {
		return ErrTurnCancelUnsupported
	}
	return w.requestACPInterrupt(ctx, source, session, "turn_cancel", false)
}

// ProviderSessionID returns the current provider-assigned ACP session id, or
// the most recently completed ACP session id for postmortem correlation.
func (w *Wrapper) ProviderSessionID() string {
	w.sessMu.RLock()
	session := w.acpSession
	retained := w.acpProviderSessionID
	w.sessMu.RUnlock()
	if session == nil {
		return retained
	}
	return session.ProviderSessionID()
}

// ACPSnapshot returns authoritative managed liveness for an ACP session.
func (w *Wrapper) ACPSnapshot() (acp.Snapshot, bool) {
	w.sessMu.RLock()
	session := w.acpSession
	w.sessMu.RUnlock()
	if session == nil {
		return acp.Snapshot{}, false
	}
	return session.Snapshot(), true
}

// ACPManager returns the manager selected for this Wrapper after Run begins.
func (w *Wrapper) ACPManager() *acp.Manager {
	w.sessMu.RLock()
	defer w.sessMu.RUnlock()
	return w.acpManager
}

// requestInterrupt is the shared interrupt path used by both
// [Wrapper.Stop] (reason="user_stop") and the ctx-watcher goroutine
// in Run (reason="ctx_cancel"). It emits an interrupt.requested
// event, calls session.Stop, then emits interrupt.acknowledged with
// ParentID correlating to the request event.
func (w *Wrapper) requestInterrupt(
	ctx context.Context,
	source runtimeevents.Source,
	session agentsessions.Session,
	reason string,
) error {
	requestID := runtimeevents.NewEventID()
	_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindInterruptRequested, source,
		map[string]any{"reason": reason},
		runtimeevents.WithID(requestID))

	stopErr := session.Stop(ctx)

	ackPayload := map[string]any{"reason": reason}
	if stopErr != nil {
		ackPayload["error"] = stopErr.Error()
	}
	_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindInterruptAcknowledged, source,
		ackPayload, runtimeevents.WithParentID(requestID))

	return stopErr
}

// isTurnInternal reports whether a runtime event kind is part of an
// agent turn's body. These trigger turn.started allocation if no
// turn is currently active.
func isTurnInternal(kind runtimeevents.EventKind) bool {
	switch kind {
	case runtimeevents.KindAgentDelta,
		runtimeevents.KindAgentToolUse,
		runtimeevents.KindAgentToolResult,
		runtimeevents.KindAgentSubagentSpawn,
		runtimeevents.KindAgentPermissionRequested,
		runtimeevents.KindAgentPermissionResolved:
		return true
	}
	return false
}

// isTurnScoped reports whether a runtime event kind should carry the
// current TurnID when one is active. Includes the turn-internal kinds
// plus the turn-ending kinds (turn.completed / turn.failed) so the
// terminal events also reference the turn they close.
func isTurnScoped(kind runtimeevents.EventKind) bool {
	if isTurnInternal(kind) {
		return true
	}
	return kind == runtimeevents.KindTurnCompleted || kind == runtimeevents.KindTurnFailed
}

// resolveWorkspaceLogPath applies Config.WorkspaceDir/LogPath's
// documented default: both forward verbatim when the caller set
// either one, and when both are empty a WorkspaceDir is synthesized
// under workdir so [Wrapper.Run] doesn't hand agentkit's
// streaming-stdio/jsonrpc-stdio/serve-http runtimes an empty pair —
// every one of them returns a hard error from Start before spawning
// anything in that case. Pulled out of Run as a pure function so the
// default-synthesis decision is independently unit-testable without
// spawning a process.
func resolveWorkspaceLogPath(workdir, sessionID, workspaceDir, logPath string) (resolvedWorkspaceDir, resolvedLogPath string) {
	if workspaceDir == "" && logPath == "" {
		workspaceDir = filepath.Join(workdir, ".wrapper-workspace", sessionID)
	}
	return workspaceDir, logPath
}

// runPlanter resolves the boot directory, emits plant.started, calls
// the configured Planter, and emits plant.completed (including on
// error). Returns the planter's error so Run can abort before
// constructing the agentkit runtime.
//
// No-op when Config.Planter is nil.
func (w *Wrapper) runPlanter(ctx context.Context, source runtimeevents.Source) error {
	if w.cfg.Planter == nil {
		return nil
	}
	bootDir := w.cfg.BootDir
	if bootDir == "" {
		bootDir = filepath.Join(w.cfg.Workdir, ".wrapper-boot", w.sessionID)
	}
	if err := os.MkdirAll(bootDir, 0o750); err != nil {
		return fmt.Errorf("wrapper: ensure boot dir %q: %w", bootDir, err)
	}

	_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindPlantStarted, source, map[string]any{
		"boot_dir":            bootDir,
		"files_planned":       countSpecFiles(w.cfg.PlantSpec),
		"hooks_planned":       len(w.cfg.PlantSpec.Hooks),
		"providers_planned":   len(w.cfg.PlantSpec.ProviderSettings),
		"has_mcp":             w.cfg.PlantSpec.MCPConfig != nil,
		"has_recovery_prompt": w.cfg.PlantSpec.RecoveryPrompt != "",
	})

	result, plantErr := w.cfg.Planter.Plant(ctx, bootDir, w.cfg.PlantSpec)

	donePayload := map[string]any{
		"boot_dir":      bootDir,
		"planted_files": result.PlantedFiles,
	}
	if plantErr != nil {
		donePayload["error"] = plantErr.Error()
	}
	_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindPlantCompleted, source, donePayload)

	if plantErr != nil {
		return fmt.Errorf("wrapper: plant: %w", plantErr)
	}
	return nil
}

// runSandbox calls the configured sandbox.Applier with the session's
// current PID and emits sandbox.applied with the result. Returns the
// applier's error so Run can stop the session and abort.
//
// No-op when Config.Sandbox is nil.
func (w *Wrapper) runSandbox(ctx context.Context, source runtimeevents.Source, session agentsessions.Session) error {
	if w.cfg.Sandbox == nil {
		return nil
	}
	pid := session.Health().PID
	result, applyErr := w.cfg.Sandbox.Apply(ctx, pid)

	payload := map[string]any{
		"profile": result.Profile,
		"applied": result.Applied,
		"notes":   result.Notes,
		"pid":     pid,
	}
	if applyErr != nil {
		payload["error"] = applyErr.Error()
	}
	_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindSandboxApplied, source, payload)

	if applyErr != nil {
		return fmt.Errorf("wrapper: sandbox.Apply: %w", applyErr)
	}
	return nil
}

// countSpecFiles tallies the discrete file slots in a plant.Spec for
// the plant.started event's "files_planned" payload. Counts entries in
// Files plus MCPConfig (if present); ProviderSettings and Hooks are
// reported separately.
func countSpecFiles(spec plant.Spec) int {
	n := len(spec.Files)
	if spec.MCPConfig != nil {
		n++
	}
	return n
}

// ErrAdapterNotRuntime is returned by [Wrapper.Run] when the
// configured [adapters.Adapter] does not also implement
// [adapters.RuntimeAdapter]. The agentkit/agentsessions integration
// path requires a [provider.CLIAdapter] which only [RuntimeAdapter]
// implementations expose. Use [errors.Is] to detect.
var ErrAdapterNotRuntime = errors.New("wrapper: adapter does not implement adapters.RuntimeAdapter")

// ErrSessionNotStarted is returned by [Wrapper.SendInput] when called
// before [Wrapper.Run] has started a session. Use [errors.Is] to
// detect.
var ErrSessionNotStarted = errors.New("wrapper: session not started")

// ErrTurnCancelUnsupported is returned by CancelTurn for a non-ACP runtime.
var ErrTurnCancelUnsupported = errors.New("wrapper: runtime does not expose turn-scoped cancellation")

// ErrAdapterNotACPClient is returned when an adapter declares ProtocolACP but
// cannot construct the real ACP client lifecycle.
var ErrAdapterNotACPClient = errors.New("wrapper: ACP adapter does not implement acp.ClientAdapter")
