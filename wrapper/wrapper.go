package wrapper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/hollis-labs/agentkit/agentsessions"
	llmtypes "github.com/hollis-labs/go-llm-types"

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
// sandbox profile", "no policy enforcement", "no filter pipeline". The
// wrapper degrades cleanly into a pure passthrough when no subsystems
// are configured.
//
// Adapter must implement [adapters.RuntimeAdapter] (not just
// [adapters.Adapter]) — [Wrapper.Run] needs the underlying
// [provider.CLIAdapter] to drive agentkit/agentsessions. The wrapper
// returns [ErrAdapterNotRuntime] from Run when the assertion fails.
type Config struct {
	// App identifies the calling application ("nanite", "torque",
	// "tachyon", ...). Propagated into every emitted Event.
	App string

	// Adapter is the provider integration (Claude, Codex, OpenCode, ...).
	// Required. Must implement [adapters.RuntimeAdapter] for [Wrapper.Run]
	// to drive it through agentkit/agentsessions.
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

	// Policy, when set, is consulted by the wrapper's translator
	// goroutine for every observed [runtimeevents.KindAgentToolUse]
	// event. [Wrapper.Run] builds a [policy.Request] from the
	// tool_use and emits the matching policy.nudge / policy.rewrite /
	// policy.block runtime event (correlated to the tool_use via
	// ParentID). ModeObserve emits no derived event.
	//
	// This is the OBSERVATION half of policy: the engine's verdict
	// surfaces in the activity stream but does NOT rewrite or block
	// the child's input/output. Rewrite-back semantics belong in a
	// follow-up that touches the session input path.
	//
	// Engines must be cheap and synchronous on the hot path — calls
	// happen inline in the translator goroutine. See
	// [policy.Engine] for the contract.
	Policy policy.Engine

	// Filters, when set, runs the harness filter pipeline against agent
	// text, tool output, command output, and envelopes. Nil means no
	// filtering. NOTE: skeleton scope — Filters is not invoked in this
	// pass.
	Filters filters.Pipeline
}

// Wrapper owns the launch boundary for one wrapped subprocess. A
// Wrapper is single-use: build via [New], run, observe the
// [activity.Bridge] event stream, dispose. To wrap a new process,
// build a new Wrapper.
type Wrapper struct {
	cfg       Config
	sessionID string

	sessMu      sync.RWMutex
	session     agentsessions.Session
	typedSource runtimeevents.Source // set in Run; used by SendInput/Stop for derived events
	rawSource   runtimeevents.Source // set in Run; used for stdin.write events
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
//  1. Type-asserts Config.Adapter to [adapters.RuntimeAdapter].
//  2. Maps [adapters.Descriptor.Runtime] to agentkit
//     [agentsessions.Capabilities].
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
// rather than producing an event.
//
// Returns the Session.Wait error if any. ctx.Err() is preserved when
// the wrapper stopped because the caller cancelled.
func (w *Wrapper) Run(ctx context.Context) error {
	ra, ok := w.cfg.Adapter.(adapters.RuntimeAdapter)
	if !ok {
		return fmt.Errorf("%w: adapter %q", ErrAdapterNotRuntime, w.cfg.Adapter.Name())
	}
	if w.cfg.Workdir == "" {
		return errors.New("wrapper: Config.Workdir is required")
	}

	desc := w.cfg.Adapter.Describe()
	caps, err := runtimeCaps(desc.Runtime)
	if err != nil {
		return err
	}

	w.cfg.Activity.Bind(w.cfg.App, w.sessionID, runtimeevents.Process{
		Provider: desc.Provider,
		Runtime:  desc.Runtime,
	})
	source := runtimeevents.Source{
		Channel:    runtimeSourceChannel(desc.Runtime),
		Confidence: runtimeevents.ConfidenceExact,
	}
	rawSource := runtimeevents.Source{
		Channel:    rawSourceChannel(desc.Runtime),
		Confidence: runtimeevents.ConfidenceExact,
	}
	w.sessMu.Lock()
	w.typedSource = source
	w.rawSource = rawSource
	w.sessMu.Unlock()

	// Resolve the wrapper-level Spec for symmetry (validates the
	// adapter's exec-shape contract, surfaces PTY/no-PTY mismatches
	// early). The agentkit runtime constructs its own argv via
	// adapter.BuildArgs — Spec is informational on this path.
	if _, err := w.cfg.Adapter.Resolve(adapters.ResolveContext{
		Cwd: w.cfg.Workdir,
		PTY: caps.PTY,
	}); err != nil {
		return fmt.Errorf("wrapper: adapter Resolve: %w", err)
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
		runtimeevents.KindStdoutRaw, runtimeevents.KindStdoutLine)
	stderrStream := newStreamWriter(ctx, w.cfg.Activity, rawSource,
		runtimeevents.KindStderrRaw, runtimeevents.KindStderrLine)

	session, err := runtime.Start(ctx, agentsessions.StartOptions{
		Workdir:     w.cfg.Workdir,
		EventFanout: fanout,
		Fanout:      stdoutStream,
		Stderr:      stderrStream,
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

	if err := w.runSandbox(ctx, source, session); err != nil {
		// Stop the session so we don't leave a child running with no
		// caller waiting on it.
		_ = session.Stop(context.Background())
		return err
	}

	translatorDone := make(chan struct{})
	go func() {
		defer close(translatorDone)
		var currentTurnID string
		for ev := range fanout {
			if ev.Type == llmtypes.EventSessionID && ev.SessionID != "" {
				// Provider-side session ID: rebind the Process so
				// subsequent emissions carry it. Use the locked
				// setter — stream writers may concurrently emit
				// events that read Process.
				w.cfg.Activity.Emitter().SetProviderSessionID(ev.SessionID)
				continue
			}
			kind, payload, mapped := translateStreamEvent(ev)
			if !mapped {
				continue
			}

			// Turn-boundary management: emit a turn.started event
			// the first time a turn-internal event appears with no
			// active turn ID. The turn ends on turn.completed /
			// turn.failed and the next turn-internal event allocates
			// a fresh ID.
			if currentTurnID == "" && isTurnInternal(kind) {
				currentTurnID = runtimeevents.NewTurnID()
				_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindTurnStarted, source, nil,
					runtimeevents.WithTurnID(currentTurnID))
			}

			// Pre-generate the event ID so policy events emitted
			// downstream can correlate via ParentID.
			eventID := runtimeevents.NewEventID()
			opts := []runtimeevents.EmitOption{runtimeevents.WithID(eventID)}
			if currentTurnID != "" && isTurnScoped(kind) {
				opts = append(opts, runtimeevents.WithTurnID(currentTurnID))
			}
			_ = w.cfg.Activity.Emit(ctx, kind, source, payload, opts...)

			if kind == runtimeevents.KindAgentToolUse {
				w.applyToolUsePolicy(ctx, source, ev, eventID, currentTurnID)
			}

			if kind == runtimeevents.KindTurnCompleted || kind == runtimeevents.KindTurnFailed {
				currentTurnID = ""
			}
		}
	}()

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
	w.sessMu.RLock()
	session := w.session
	rawSource := w.rawSource
	w.sessMu.RUnlock()
	if session == nil {
		return ErrSessionNotStarted
	}
	// Emit before forwarding so the event sequence reflects intent
	// even when SendInput errors (the input was attempted regardless).
	_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindStdinWrite, rawSource,
		map[string]any{
			"bytes": string(data),
		})
	return session.SendInput(ctx, data)
}

// Stop requests termination of the running session and emits the
// interrupt.requested → interrupt.acknowledged event pair around the
// underlying session.Stop call. The two events are correlated via
// ParentID. Returns nil if no session is running (e.g., Run already
// returned).
func (w *Wrapper) Stop(ctx context.Context) error {
	w.sessMu.RLock()
	session := w.session
	source := w.typedSource
	w.sessMu.RUnlock()
	if session == nil {
		return nil
	}
	return w.requestInterrupt(ctx, source, session, "user_stop")
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
		"boot_dir":           bootDir,
		"files_planned":      countSpecFiles(w.cfg.PlantSpec),
		"hooks_planned":      len(w.cfg.PlantSpec.Hooks),
		"providers_planned":  len(w.cfg.PlantSpec.ProviderSettings),
		"has_mcp":            w.cfg.PlantSpec.MCPConfig != nil,
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
