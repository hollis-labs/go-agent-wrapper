package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/internal/childoutput"
	"github.com/hollis-labs/go-agent-wrapper/internal/closegate"
	llmtypes "github.com/hollis-labs/go-llm-types"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// ndjsonProtocolVersion is the integer protocolVersion NDJSONBridgeClient sends
// in `initialize`. The value 1 round-trips against every bridge and native
// agent the shipped adapters target.
const ndjsonProtocolVersion = 1

// NDJSONBridgeConfig supplies the two axes of real per-agent variation among
// adapters/{claudeacp,codexacp,opencodeacp,piacp}: how to resolve the
// subprocess to spawn, and how to translate that agent's own session/update
// notification shape into runtimeevents. Every other concern -- JSON-RPC
// framing, id-based call/response matching, the initialize -> authenticate ->
// session/new-or-load -> configureSession handshake, Prompt/Cancel/Close
// lifecycle, read-loop/process-exit termination coordination, and
// session/request_permission dispatch -- is identical across all four and lives
// in NDJSONBridgeClient itself. adapters/copilotacp's dual stdio/TCP client is
// architecturally different and is deliberately not unified here.
type NDJSONBridgeConfig struct {
	// Component prefixes every error, diagnostic reason and log line the
	// client produces, e.g. "claudeacp". It preserves each adapter's own
	// error-message text.
	Component string

	// ResolveCommand returns the default binary and arguments Launch spawns
	// when LaunchParams.Command does not override them. Required.
	ResolveCommand func(params LaunchParams) (string, []string, error)

	// HandleNotification translates one inbound JSON-RPC notification (a frame
	// with a method and no id) into runtimeevents, typically by calling
	// c.CurrentTurnID and c.Emit. It runs on the protocol reader goroutine and
	// must not block. Required.
	HandleNotification func(c *NDJSONBridgeClient, method string, params json.RawMessage)

	// LaunchEnv, when non-nil, computes the spawned process's environment from
	// params; nil uses params.Env unchanged. A nil result inherits the parent
	// environment, as with params.Env.
	LaunchEnv func(params LaunchParams) []string
}

// NDJSONBridgeClient implements Client for a bridge-mediated or native ACP
// agent that speaks newline-delimited JSON-RPC 2.0 over a spawned subprocess's
// stdin/stdout. Construct one with NewNDJSONBridgeClient, call Launch once,
// Prompt/Cancel any number of times, and Close when done.
type NDJSONBridgeClient struct {
	cfg NDJSONBridgeConfig

	// promptCloseMu orders an admitted Prompt's request write with Close's
	// bounded graceful session/close attempt. The closed state under mu seals
	// new admissions before Close waits for this gate. It must not guard general
	// protocol writes: server-request responses need to remain able to run while
	// Close waits for its response.
	promptCloseMu sync.Mutex

	mu           sync.Mutex
	launched     bool
	closed       bool
	closeOnce    closegate.Once
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	sessionID    string
	systemPrompt string
	sessionClose bool

	// eventsClosing is set, under mu, by closeEvents before it waits for the
	// in-flight turns. Prompt admits no turn once it is set (CW-20261001-0262):
	// every turnWG.Add happens under mu before it, so it is ordered before the
	// Wait. closed alone could not do this, because the agent exiting on its
	// own reaches closeEvents without Close.
	eventsClosing bool

	writeMu            sync.Mutex // serializes writes to stdin across goroutines
	transportCloseOnce sync.Once

	nextID atomic.Int64
	pendMu sync.Mutex
	// pending maps an outbound request id to its method metadata and eventual
	// response channel. Guarded by pendMu.
	pending map[int64]pendingRPCResponse

	turnMu        sync.Mutex
	currentTurnID string
	turnWG        sync.WaitGroup

	eventsMu     sync.Mutex
	events       chan runtimeevents.Event
	eventsClosed bool

	waitDone       chan struct{}
	waitErr        error
	sandboxCleanup func()
	termination    *TransportTermination
	terminated     chan struct{}
	lifetimeCtx    context.Context
	lifetimeStop   context.CancelFunc
	permissions    *BestEffortPermissionRequests
	diagnosticMu   sync.Mutex
	diagnostic     func(Diagnostic)

	// The child's stdout and stderr read ends, owned here rather than by
	// exec.Cmd so Wait cannot close them under the readers; waitProcess
	// drains them (see internal/childoutput). readDone and stderrDone close
	// when readLoop and drainStderr finish.
	stdout     *os.File
	stderr     *os.File
	readDone   chan struct{}
	stderrDone chan struct{}
}

var _ Client = (*NDJSONBridgeClient)(nil)

// NewNDJSONBridgeClient returns a client ready for Launch. It panics if
// cfg.ResolveCommand or cfg.HandleNotification is nil: both are required, and
// a missing one is a programming error that would otherwise surface as a nil
// call on the first Launch or notification.
func NewNDJSONBridgeClient(cfg NDJSONBridgeConfig) *NDJSONBridgeClient {
	if cfg.ResolveCommand == nil || cfg.HandleNotification == nil {
		panic("acp: NDJSONBridgeConfig requires ResolveCommand and HandleNotification")
	}
	return &NDJSONBridgeClient{
		cfg:     cfg,
		events:  make(chan runtimeevents.Event, 64),
		pending: make(map[int64]pendingRPCResponse),
	}
}

// RPCError mirrors JSON-RPC 2.0's error object. Errors returned from Call
// carry the client's Component so their text names the adapter they came from.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`

	// Component is the owning client's NDJSONBridgeConfig.Component. It is set
	// locally, never on the wire.
	Component string `json:"-"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil rpc error>"
	}
	component := e.Component
	if component == "" {
		component = "acp"
	}
	return fmt.Sprintf("%s: jsonrpc error %d: %s", component, e.Code, e.Message)
}

// CurrentTurnID returns the id of the in-flight turn, or "" when none is
// active. A HandleNotification implementation stamps it on the events it emits.
func (c *NDJSONBridgeClient) CurrentTurnID() string {
	c.turnMu.Lock()
	defer c.turnMu.Unlock()
	return c.currentTurnID
}

// Emit pushes ev onto the Events channel. ID, Sequence, SessionID, App and
// Process are left zero for the caller's own emitter to fill in, per
// [Client.Events]. Emit never blocks: if the channel is full the event is
// dropped.
func (c *NDJSONBridgeClient) Emit(ev runtimeevents.Event) { c.emit(ev) }

// rpcResponse is the internal envelope delivered to a pending Call.
// Exactly one of result/err is set.
type rpcResponse struct {
	result json.RawMessage
	err    *RPCError
}

type pendingRPCResponse struct {
	response chan rpcResponse
	prompt   bool
}

// rpcFrame is the minimal shape the reader loop needs to classify an
// inbound line into response / notification / server-initiated request.
type rpcFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Launch implements [Client]. It spawns the process NDJSONBridgeConfig.ResolveCommand
// resolves (unless LaunchParams.Command overrides it), performs the `initialize` + `session/new` (or, when
// params.SessionIDPreset is set and the capability is advertised, a
// fail-closed `session/load`) handshake, and returns once the
// session is ready to accept a Prompt.
func (c *NDJSONBridgeClient) Launch(ctx context.Context, params LaunchParams) error {
	c.mu.Lock()
	if c.launched {
		c.mu.Unlock()
		return errors.New(c.cfg.Component + ": Launch called more than once")
	}
	c.launched = true
	c.mu.Unlock()

	defaultBinary, defaultArgs, err := c.cfg.ResolveCommand(params)
	if err != nil {
		return fmt.Errorf("%s: resolve command: %w", c.cfg.Component, err)
	}
	binary, args, err := ResolveLaunchCommand(params, defaultBinary, defaultArgs)
	if err != nil {
		return fmt.Errorf("%s: launch command: %w", c.cfg.Component, err)
	}
	cmd := exec.Command(binary, args...) //nolint:gosec // G204: operator-controlled binary/args
	if params.Cwd != "" {
		cmd.Dir = params.Cwd
	}
	env := params.Env
	if c.cfg.LaunchEnv != nil {
		env = c.cfg.LaunchEnv(params)
	}
	if env != nil {
		cmd.Env = env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("%s: stdin pipe: %w", c.cfg.Component, err)
	}
	// Own stdout rather than using cmd.StdoutPipe: Wait closes that as soon
	// as the child exits, racing readLoop and discarding a reply the child
	// wrote just before exiting (CW-20261001-0039).
	stdout, err := childoutput.NewPipe()
	if err != nil {
		return fmt.Errorf("%s: stdout pipe: %w", c.cfg.Component, err)
	}
	cmd.Stdout = stdout.W
	// Drain stderr rather than leaving it unread: bridges and native agents
	// keep stdout a clean JSON-RPC channel by routing their own logging to
	// stderr, and an unread stderr pipe can still block the child once its OS
	// buffer fills. Owned for the same reason as stdout.
	stderr, err := childoutput.NewPipe()
	if err != nil {
		stdout.Close()
		return fmt.Errorf("%s: stderr pipe: %w", c.cfg.Component, err)
	}
	cmd.Stderr = stderr.W

	sandboxOutcome, sandboxCleanup, err := PrepareLaunchSandbox(cmd, params)
	if err != nil {
		stdout.Close()
		stderr.Close()
		return fmt.Errorf("%s: sandbox: %w", c.cfg.Component, err)
	}
	if err = cmd.Start(); err != nil {
		stdout.Close()
		stderr.Close()
		sandboxCleanup()
		ReportLaunchSandboxStartFailed(params, sandboxOutcome, err)
		return fmt.Errorf("%s: start %q: %w", c.cfg.Component, binary, err)
	}
	stdout.Started()
	stderr.Started()
	ReportLaunchSandboxStarted(params, sandboxOutcome)

	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.waitDone = make(chan struct{})
	c.sandboxCleanup = sandboxCleanup
	c.stdout, c.stderr = stdout.R, stderr.R
	readDone, stderrDone := make(chan struct{}), make(chan struct{})
	c.readDone, c.stderrDone = readDone, stderrDone
	c.terminated = make(chan struct{})
	c.termination = NewTransportTermination(true)
	c.lifetimeCtx, c.lifetimeStop = context.WithCancel(context.Background())
	c.permissions = NewBestEffortPermissionRequests(params.BestEffortPermissionRequestResponder)
	c.permissions.SetResponseGate(&c.promptCloseMu)
	c.diagnostic = params.OnDiagnostic
	c.mu.Unlock()

	if pid := cmd.Process.Pid; pid != 0 {
		c.emit(runtimeevents.Event{
			Kind:    runtimeevents.KindProcessStarted,
			Payload: mustMarshal(map[string]any{"pid": pid}),
		})
	}

	go func() {
		defer close(stderrDone)
		c.drainStderr(stderr.R)
	}()
	go func() {
		defer close(readDone)
		c.readLoop(stdout.R)
	}()
	go c.waitProcess()
	go c.coordinateTermination()

	initializeResult, err := c.Call(ctx, "initialize", map[string]any{
		"protocolVersion": ndjsonProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	})
	if err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("%s: initialize: %w", c.cfg.Component, err)
	}
	initialize, err := ParseInitializeResult(initializeResult, ndjsonProtocolVersion)
	if err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("%s: initialize negotiation: %w", c.cfg.Component, err)
	}
	if err = c.authenticate(ctx, initialize, params.AuthMethodID); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("%s: authenticate: %w", c.cfg.Component, err)
	}
	c.mu.Lock()
	c.sessionClose = initialize.SessionClose
	c.mu.Unlock()

	mcpServers, skipped, err := SessionMCPServers(params.MCPServers, initialize.MCPHTTP)
	if err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("%s: %w", c.cfg.Component, err)
	}
	ReportSkippedMCPServers(params.OnDiagnostic, skipped)

	// A preset the agent cannot load is not resumed: it gets a new session. The
	// host is told with session.lost (below), not left to infer it.
	presetNotHonored := params.SessionIDPreset != "" && !initialize.LoadSession
	if params.SessionIDPreset != "" && initialize.LoadSession {
		if err := c.loadSession(ctx, params, mcpServers); err != nil {
			_ = c.Close(context.Background())
			return fmt.Errorf("%s: session/load: %w", c.cfg.Component, err)
		}
		c.mu.Lock()
		c.sessionID = params.SessionIDPreset
		c.mu.Unlock()
	} else if err := c.newSession(ctx, params, mcpServers); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("%s: session/new: %w", c.cfg.Component, err)
	}
	c.permissions.SetSessionID(c.ProviderSessionID())
	if err := c.configureSession(ctx, params); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("%s: configure session: %w", c.cfg.Component, err)
	}
	c.mu.Lock()
	c.systemPrompt = params.SystemPrompt
	c.mu.Unlock()

	if presetNotHonored {
		c.emit(NewSessionLostEvent(params.SessionIDPreset, c.ProviderSessionID(), SessionLoadUnsupportedReason))
	}
	c.emit(runtimeevents.Event{Kind: runtimeevents.KindSessionReady})
	return nil
}

func (c *NDJSONBridgeClient) authenticate(ctx context.Context, initialize InitializeResult, methodID string) error {
	if methodID == "" {
		return nil
	}
	for _, method := range initialize.AuthMethods {
		if method.ID != methodID {
			continue
		}
		if method.Type == "terminal" {
			return fmt.Errorf("authentication method %q requires an interactive terminal", methodID)
		}
		_, err := c.Call(ctx, "authenticate", map[string]any{"methodId": methodID})
		return err
	}
	return fmt.Errorf("authentication method %q was not advertised", methodID)
}

func (c *NDJSONBridgeClient) configureSession(ctx context.Context, params LaunchParams) error {
	sessionID := c.ProviderSessionID()
	if params.SessionModeID != "" {
		if _, err := c.Call(ctx, "session/set_mode", map[string]any{"sessionId": sessionID, "modeId": params.SessionModeID}); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(params.SessionConfig))
	for key := range params.SessionConfig {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := params.SessionConfig[key]
		switch value.(type) {
		case string, bool:
		default:
			return fmt.Errorf("option %q has unsupported value type %T (want string or bool)", key, value)
		}
		request := map[string]any{"sessionId": sessionID, "configId": key, "value": value}
		if _, ok := value.(bool); ok {
			request["type"] = "boolean"
		}
		if _, err := c.Call(ctx, "session/set_config_option", request); err != nil {
			return fmt.Errorf("option %q: %w", key, err)
		}
	}
	return nil
}

func (c *NDJSONBridgeClient) newSession(ctx context.Context, params LaunchParams, mcpServers []any) error {
	result, err := c.Call(ctx, "session/new", map[string]any{
		"cwd":        params.Cwd,
		"mcpServers": mcpServers,
	})
	if err != nil {
		return err
	}
	var decoded struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		return fmt.Errorf("decode session/new result: %w", err)
	}
	if decoded.SessionID == "" {
		return errors.New("session/new returned an empty sessionId")
	}
	c.mu.Lock()
	c.sessionID = decoded.SessionID
	c.mu.Unlock()
	return nil
}

func (c *NDJSONBridgeClient) loadSession(ctx context.Context, params LaunchParams, mcpServers []any) error {
	_, err := c.Call(ctx, "session/load", map[string]any{
		"sessionId":  params.SessionIDPreset,
		"cwd":        params.Cwd,
		"mcpServers": mcpServers,
	})
	return err
}

// Prompt implements [Client]. It sends `session/prompt` and returns
// once the request has been written to the wire — NOT once the turn
// completes. Turn progress (message/thought chunks, tool calls) and
// completion (turn.completed / turn.failed, carrying the ACP
// `stopReason`) surface asynchronously via [Client.Events].
func (c *NDJSONBridgeClient) Prompt(ctx context.Context, prompt string) error {
	c.promptCloseMu.Lock()
	defer c.promptCloseMu.Unlock()

	c.turnMu.Lock()
	if c.currentTurnID != "" {
		c.turnMu.Unlock()
		return errors.New(c.cfg.Component + ": a turn is already in flight")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.turnMu.Unlock()
		return errors.New(c.cfg.Component + ": client is closed")
	}
	if c.eventsClosing {
		c.mu.Unlock()
		c.turnMu.Unlock()
		return errors.New(c.cfg.Component + ": the agent's transport has ended")
	}
	sessionID := c.sessionID
	if sessionID == "" {
		c.mu.Unlock()
		c.turnMu.Unlock()
		return errors.New(c.cfg.Component + ": Prompt called before Launch established a session")
	}
	systemPrompt := c.systemPrompt
	c.systemPrompt = ""
	lifetimeCtx := c.lifetimeCtx
	turnID := runtimeevents.NewTurnID()
	c.currentTurnID = turnID
	c.permissions.BeginTurn()
	c.turnWG.Add(1)
	c.mu.Unlock()
	c.turnMu.Unlock()

	c.emit(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: turnID})

	text := prompt
	if systemPrompt != "" {
		text = systemPrompt + "\n\n" + prompt
	}
	params := map[string]any{
		"sessionId": sessionID,
		"prompt": []map[string]any{
			{"type": "text", "text": text},
		},
	}

	// Fire-and-forget from the caller's perspective: the request is
	// written synchronously (so a write failure surfaces to the caller
	// immediately), but the response is awaited on a background
	// goroutine so Prompt returns once the turn is merely accepted, per
	// [Client.Prompt]'s documented contract.
	call, err := c.beginCall(ctx, "session/prompt", params)
	if err != nil {
		c.finishTurn(turnID, nil, err)
		c.turnWG.Done()
		return err
	}

	go func() {
		defer c.turnWG.Done()
		result, err := c.awaitCall(lifetimeCtx, call)
		c.finishTurn(turnID, result, err)
	}()

	return nil
}

func (c *NDJSONBridgeClient) finishTurn(turnID string, result json.RawMessage, callErr error) {
	c.permissions.EndTurn()
	c.turnMu.Lock()
	if c.currentTurnID == turnID {
		c.currentTurnID = ""
	}
	c.turnMu.Unlock()

	if callErr != nil {
		c.emit(runtimeevents.Event{
			Kind:    runtimeevents.KindTurnFailed,
			TurnID:  turnID,
			Payload: mustMarshal(map[string]any{"error": callErr.Error(), "stop_reason": llmtypes.StopReasonError}),
		})
		return
	}

	var decoded struct {
		StopReason string          `json:"stopReason"`
		Usage      json.RawMessage `json:"usage,omitempty"`
	}
	_ = json.Unmarshal(result, &decoded)
	// The shared stop-reason vocabulary: ACP's end_turn, max_tokens and
	// refusal are already in it, as is its cancellation value;
	// max_turn_requests becomes turn_limit.
	payload := map[string]any{"stop_reason": llmtypes.NormalizeStopReason(decoded.StopReason)}
	if len(decoded.Usage) > 0 {
		var usage any
		if err := json.Unmarshal(decoded.Usage, &usage); err == nil {
			payload["usage"] = usage
		}
	}
	c.emit(runtimeevents.Event{
		Kind:    runtimeevents.KindTurnCompleted,
		TurnID:  turnID,
		Payload: mustMarshal(payload),
	})
}

// Cancel implements [Client]. It sends ACP's `session/cancel`
// notification for the in-flight turn; each adapter's package doc records
// the live-measured evidence that it is a genuine mid-turn abort for that
// agent. Cancel does not itself wait for the resulting response —
// Prompt's own background goroutine emits the resulting turn.completed/turn.failed event when it arrives. A
// no-op (returns nil) when no turn is active.
func (c *NDJSONBridgeClient) Cancel(ctx context.Context) error {
	c.promptCloseMu.Lock()
	defer c.promptCloseMu.Unlock()

	c.mu.Lock()
	sessionID := c.sessionID
	c.mu.Unlock()
	c.turnMu.Lock()
	turnID := c.currentTurnID
	c.turnMu.Unlock()
	if sessionID == "" || turnID == "" {
		return nil
	}
	c.mu.Lock()
	permissions := c.permissions
	c.mu.Unlock()
	permissions.CancelTurn()
	return c.Notify(ctx, map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/cancel",
		"params":  map[string]any{"sessionId": sessionID},
	})
}

// InterruptCapability implements [Client]. Returns
// [adapters.InterruptTurn]: session/cancel is verified live to abort a turn for
// all four agents this client serves (see each adapter's package doc). This is
// a static fact, independent of whether Launch has been called — matching [DescriptorFor]'s expectation that
// Describe() can be answered without a live session.
func (c *NDJSONBridgeClient) InterruptCapability() adapters.InterruptCapability {
	return adapters.InterruptTurn
}

// Events implements [Client].
func (c *NDJSONBridgeClient) Events() <-chan runtimeevents.Event { return c.events }

// ProviderSessionID returns the id established by session/new or session/load.
func (c *NDJSONBridgeClient) ProviderSessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// Close implements [Client]. Terminates the subprocess (closing
// stdin first to give it a chance to exit on EOF, then killing it after
// a grace period) and closes the Events channel exactly once. Safe to
// call even if Launch was never called or failed, and safe to call more
// than once.
func (c *NDJSONBridgeClient) Close(ctx context.Context) error {
	return c.closeOnce.Do(func() error {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil
		}
		c.closed = true
		cmd := c.cmd
		stdin := c.stdin
		waitDone := c.waitDone
		terminated := c.terminated
		lifetimeStop := c.lifetimeStop
		permissions := c.permissions
		sessionID := c.sessionID
		sessionClose := c.sessionClose
		c.mu.Unlock()
		if permissions != nil {
			permissions.Close()
		}
		graceful := closegate.TryLockWithin(ctx, &c.promptCloseMu, closegate.PromptDrainGrace)
		if lifetimeStop != nil {
			lifetimeStop()
		}
		if !graceful && stdin != nil {
			// Prompt owns the admission gate across its synchronous transport
			// write. Closing stdin first is the only operation that can preempt a
			// backpressured write; waiting for the gate would deadlock Close.
			c.closeTransport()
		}
		var closeErr error
		if graceful && sessionClose && sessionID != "" {
			closeCtx, cancel := context.WithTimeout(ctx, time.Second)
			closeResult := make(chan error, 1)
			go func() {
				_, err := c.Call(closeCtx, "session/close", map[string]any{"sessionId": sessionID})
				closeResult <- err
			}()
			select {
			case closeErr = <-closeResult:
			case <-closeCtx.Done():
				closeErr = closeCtx.Err()
			}
			cancel()
		}
		if graceful {
			c.promptCloseMu.Unlock()
		}

		c.failPending(errors.New(c.cfg.Component + ": client closed"))
		c.closeTransport()
		if cmd == nil || cmd.Process == nil {
			c.closeEvents()
			return closeErr
		}
		if waitDone == nil {
			return closeErr
		}

		grace := 2 * time.Second
		select {
		case <-waitDone:
			if terminated != nil {
				select {
				case <-terminated:
				case <-ctx.Done():
				case <-time.After(closegate.TerminationDrainGrace):
				}
			}
			return closeErr
		case <-time.After(grace):
		case <-ctx.Done():
		}

		_ = cmd.Process.Kill()
		select {
		case <-waitDone:
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
		}
		if terminated != nil {
			select {
			case <-terminated:
			case <-ctx.Done():
			case <-time.After(closegate.TerminationDrainGrace):
			}
		}
		return closeErr
	})
}

func (c *NDJSONBridgeClient) waitProcess() {
	c.mu.Lock()
	cmd := c.cmd
	waitDone := c.waitDone
	stdout, readDone := c.stdout, c.readDone
	stderr, stderrDone := c.stderr, c.stderrDone
	c.mu.Unlock()
	err := cmd.Wait()
	// Let the readers see everything the child wrote before its exit is
	// reported: the last frame is often the reply a pending call awaits.
	deadline := time.Now().Add(childoutput.DrainTimeout)
	childoutput.Drain(stdout, readDone, deadline)
	childoutput.Drain(stderr, stderrDone, deadline)
	if c.sandboxCleanup != nil {
		c.sandboxCleanup()
	}
	c.mu.Lock()
	c.waitErr = err
	termination := c.termination
	c.mu.Unlock()
	termination.ReportProcess(TransportProcessResult{ExitCode: cmd.ProcessState.ExitCode(), Err: err})
	close(waitDone)
}

func (c *NDJSONBridgeClient) coordinateTermination() {
	c.mu.Lock()
	termination := c.termination
	cmd := c.cmd
	terminated := c.terminated
	lifetimeStop := c.lifetimeStop
	permissions := c.permissions
	c.mu.Unlock()
	result := termination.Coordinate(func() {
		c.closeTransport()
	}, func() error {
		if cmd != nil && cmd.Process != nil {
			return cmd.Process.Kill()
		}
		return nil
	})
	permissions.Close()
	if lifetimeStop != nil {
		lifetimeStop()
	}
	if result.ShouldEmitProcessExit() {
		payload := map[string]any{"exit_code": result.Process.ExitCode}
		if result.Process.Err != nil {
			payload["error"] = result.Process.Err.Error()
		}
		c.emit(runtimeevents.Event{Kind: runtimeevents.KindProcessExited, Payload: mustMarshal(payload)})
	}
	c.closeEvents()
	close(terminated)
}

// closeEvents closes the Events channel exactly once, guarded by
// eventsMu so a concurrent [Client.emit] call either completes its send
// before the close (still open) or observes eventsClosed==true and skips
// the send — never races the close() call itself. Called from both
// [Client.Close] (explicit teardown) and [Client.waitProcess] (the
// subprocess exiting on its own), whichever happens first.
func (c *NDJSONBridgeClient) closeEvents() {
	// No new turn is admitted from here on, so no turnWG.Add can run against
	// the Wait below: Prompt adds under mu, before this flag is set or not at
	// all. Without it an Add from zero races the Wait, which panics with "sync:
	// WaitGroup is reused before previous Wait has returned".
	c.mu.Lock()
	c.eventsClosing = true
	c.mu.Unlock()
	c.turnWG.Wait()
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()
	if c.eventsClosed {
		return
	}
	c.eventsClosed = true
	close(c.events)
}

func (c *NDJSONBridgeClient) drainStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		// Diagnostic-only: agents route their own logging here (see Launch),
		// not protocol frames. Surface them only through the opt-in redacted
		// diagnostic callback, never as model activity.
		c.reportDiagnostic(NewDiagnostic(DiagnosticStderr, "ACP child stderr", scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		c.reportReadError("reading ACP child stderr", err)
	}
}

// readLoop scans stdout line by line, classifying each newline-delimited
// JSON-RPC frame into response / notification / server-initiated
// request, exactly mirroring the classification agentkit's own
// jsonrpc-stdio reader uses (method+id => server request; method-only =>
// notification; id-only => response to one of ours).
func (c *NDJSONBridgeClient) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	malformed := false
	for scanner.Scan() {
		raw := scanner.Bytes()
		var frame rpcFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			malformed = true
			c.reportDiagnostic(NewDiagnostic(DiagnosticMalformedJSON, "invalid JSON-RPC frame", string(raw)))
			continue
		}
		switch {
		case frame.Method != "" && len(frame.ID) != 0:
			if _, ok := DecodeJSONRPCRequestID(frame.ID); !ok {
				malformed = true
				c.reportDiagnostic(NewDiagnostic(DiagnosticProtocol, "invalid JSON-RPC request id", ""))
				continue
			}
			c.handleServerRequest(frame)
		case frame.Method != "":
			c.cfg.HandleNotification(c, frame.Method, frame.Params)
		case len(frame.ID) != 0:
			if c.deliverResponse(frame.ID, frame.Result, frame.Error) {
				malformed = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		c.reportReadError("reading ACP protocol stream", err)
	}
	c.permissions.Close()
	c.failPending(errors.New(c.cfg.Component + ": protocol stream closed before response"))
	c.mu.Lock()
	termination := c.termination
	c.mu.Unlock()
	termination.ReportRead(TransportReadResult{Malformed: malformed, Err: scanner.Err()})
}

func (c *NDJSONBridgeClient) reportDiagnostic(d Diagnostic) {
	c.diagnosticMu.Lock()
	defer c.diagnosticMu.Unlock()
	c.mu.Lock()
	fn := c.diagnostic
	c.mu.Unlock()
	if fn != nil {
		fn(d)
	}
}

func (c *NDJSONBridgeClient) reportReadError(message string, err error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if !closed {
		c.reportDiagnostic(NewDiagnostic(DiagnosticProtocol, message, err.Error()))
	}
}

// deliverResponse routes a response frame to the pending call it answers. It
// reports whether the frame was malformed: a response whose id is not an
// integer cannot answer any request this client allocated, so it is surfaced
// as a protocol diagnostic and counted as malformed input for the termination
// coordinator (the same classification adapters/copilotacp applies), rather
// than being dropped without a trace.
func (c *NDJSONBridgeClient) deliverResponse(idRaw json.RawMessage, result json.RawMessage, rpcErr *RPCError) (malformed bool) {
	var id int64
	if err := json.Unmarshal(idRaw, &id); err != nil {
		c.reportDiagnostic(NewDiagnostic(DiagnosticProtocol, "unexpected non-numeric JSON-RPC response id", ""))
		return true
	}
	c.pendMu.Lock()
	pending, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.pendMu.Unlock()
	if !ok {
		return false
	}
	if pending.prompt {
		c.permissions.CloseTurnAdmission()
	}
	if rpcErr != nil {
		rpcErr.Component = c.cfg.Component
	}
	pending.response <- rpcResponse{result: result, err: rpcErr}
	close(pending.response)
	return false
}

func (c *NDJSONBridgeClient) failPending(err error) {
	c.pendMu.Lock()
	pending := c.pending
	c.pending = make(map[int64]pendingRPCResponse)
	c.pendMu.Unlock()
	for _, call := range pending {
		call.response <- rpcResponse{err: &RPCError{Code: -32000, Message: err.Error(), Component: c.cfg.Component}}
		close(call.response)
	}
}

// outboundCall is a request that has been written and awaits its response.
type outboundCall struct {
	id   int64
	resp chan rpcResponse
}

// beginCall allocates a request id, registers the pending channel, and
// writes the request frame, bounded by ctx, but does not wait for the
// response. Pairs with [Client.awaitCall]. A ctx that ends while the frame is
// still being written, or while the write waits behind another, releases it
// (see writeLineCtx); a ctx that never ends leaves the write bounded only by
// the transport closing (CW-20261001-0211).
func (c *NDJSONBridgeClient) beginCall(ctx context.Context, method string, params any) (outboundCall, error) {
	id := c.nextID.Add(1)
	respCh := make(chan rpcResponse, 1)
	c.pendMu.Lock()
	c.pending[id] = pendingRPCResponse{response: respCh, prompt: method == "session/prompt"}
	c.pendMu.Unlock()

	type reqFrame struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}
	if err := c.writeLineCtx(ctx, reqFrame{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.dropPending(id)
		return outboundCall{}, err
	}
	return outboundCall{id: id, resp: respCh}, nil
}

// dropPending forgets an outbound request whose caller stopped waiting. A
// response that still arrives for it finds no entry and is ignored, as one for
// any unknown id is.
func (c *NDJSONBridgeClient) dropPending(id int64) {
	c.pendMu.Lock()
	delete(c.pending, id)
	c.pendMu.Unlock()
}

func (c *NDJSONBridgeClient) awaitCall(ctx context.Context, call outboundCall) (json.RawMessage, error) {
	select {
	case resp := <-call.resp:
		if resp.err != nil {
			return nil, resp.err
		}
		return resp.result, nil
	case <-ctx.Done():
		c.dropPending(call.id)
		return nil, ctx.Err()
	}
}

// Call sends a JSON-RPC request and blocks for the matching response.
// It is the primitive the handshake is built from; a translator or an
// adapter with an agent-specific extension method may use it too. A JSON-RPC
// error response comes back as a *RPCError.
func (c *NDJSONBridgeClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	call, err := c.beginCall(ctx, method, params)
	if err != nil {
		return nil, err
	}
	return c.awaitCall(ctx, call)
}

// Notify writes a pre-built JSON-RPC notification frame (no id), bounded by a
// short write deadline so a stalled agent cannot hold the caller.
func (c *NDJSONBridgeClient) Notify(ctx context.Context, frame any) error {
	writeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.writeLineWithDeadline(frame, time.Now().Add(250*time.Millisecond))
	}()
	select {
	case err := <-done:
		if err != nil {
			c.closeTransport()
		}
		return err
	case <-writeCtx.Done():
		c.closeTransport()
		return writeCtx.Err()
	}
}

// abortPermissionTransport is the fail-closed path for an undeliverable
// permission response. It intentionally avoids promptCloseMu: response I/O may
// still own that ordering gate when the failure is observed.
func (c *NDJSONBridgeClient) abortPermissionTransport() {
	c.mu.Lock()
	cmd := c.cmd
	lifetimeStop := c.lifetimeStop
	c.mu.Unlock()
	if lifetimeStop != nil {
		lifetimeStop()
	}
	c.closeTransport()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// closeTransport interrupts transport writes exactly once. It intentionally
// does not take promptCloseMu or writeMu: either may be owned by the blocked
// write this method exists to preempt.
func (c *NDJSONBridgeClient) closeTransport() {
	c.transportCloseOnce.Do(func() {
		c.mu.Lock()
		stdin := c.stdin
		c.mu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}
	})
}

type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

// lockWrite takes writeMu, giving up if ctx ends first. A caller queued behind
// a stalled write would otherwise wait on it however short its own deadline.
func (c *NDJSONBridgeClient) lockWrite(ctx context.Context) error {
	if c.writeMu.TryLock() {
		return nil
	}
	acquired := make(chan struct{})
	go func() {
		c.writeMu.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
		return nil
	case <-ctx.Done():
		go func() {
			<-acquired
			c.writeMu.Unlock()
		}()
		return ctx.Err()
	}
}

// writeLineCtx writes one frame to stdin, bounded by ctx, for a request whose
// caller waits on ctx (CW-20261001-0211). It writes nothing once ctx has
// ended, stops waiting for writeMu when ctx ends, and interrupts a write
// blocked on a full pipe: through the write deadline when stdin has one,
// otherwise by closing the transport. A ctx that ends mid-write leaves
// writeMu free for the next caller.
//
// If the interrupted write put part of the frame on the wire, the stream no
// longer has frame boundaries the agent can parse, so the transport is
// closed, as Notify closes it. When nothing was written it stays open: the
// request was never sent and the next one is whole.
func (c *NDJSONBridgeClient) writeLineCtx(ctx context.Context, v any) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%s: encode: %w", c.cfg.Component, err)
	}
	frame := append(encoded, '\n')
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New(c.cfg.Component + ": not launched (no stdin)")
	}
	ctxErr := func(err error) error { return fmt.Errorf("%s: write request: %w", c.cfg.Component, err) }
	if endedErr := ctx.Err(); endedErr != nil {
		return ctxErr(endedErr)
	}
	if lockErr := c.lockWrite(ctx); lockErr != nil {
		return ctxErr(lockErr)
	}
	defer c.writeMu.Unlock()
	if endedErr := ctx.Err(); endedErr != nil {
		return ctxErr(endedErr)
	}

	// The interrupt runs only once ctx has ended, so a write it cuts short
	// always finds ctx.Err() set. That holds for a ctx deadline as much as a
	// cancel: setting the write deadline from ctx.Deadline() instead would let
	// the write time out a moment before ctx reports it.
	deadliner, canSet := stdin.(writeDeadliner)
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(fired)
		if canSet && deadliner.SetWriteDeadline(time.Now()) == nil {
			return
		}
		c.closeTransport()
	})
	n, err := stdin.Write(frame)
	if !stop() {
		// The interrupt ran or is running: let it finish before the
		// deadline is cleared, so it cannot land on the next write.
		<-fired
		if canSet {
			_ = deadliner.SetWriteDeadline(time.Time{})
		}
	}
	if err == nil {
		return nil
	}
	if n > 0 {
		c.closeTransport()
	}
	if ctxEnded := ctx.Err(); ctxEnded != nil {
		return ctxErr(ctxEnded)
	}
	return err
}

func (c *NDJSONBridgeClient) writeLineWithDeadline(v any, deadline time.Time) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%s: encode: %w", c.cfg.Component, err)
	}
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New(c.cfg.Component + ": not launched (no stdin)")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !deadline.IsZero() {
		if writer, ok := stdin.(writeDeadliner); ok {
			if deadlineErr := writer.SetWriteDeadline(deadline); deadlineErr == nil {
				defer func() { _ = writer.SetWriteDeadline(time.Time{}) }()
			}
		}
	}
	_, err = stdin.Write(append(encoded, '\n'))
	return err
}

// RespondToServerRequest answers a server-initiated JSON-RPC request
// (a frame carrying both `method` and `id`) with result, or with rpcErr when
// that is non-nil. JSON-RPC 2.0 requires a response for every request that
// carries an id — without one, the agent blocks waiting for it.
func (c *NDJSONBridgeClient) RespondToServerRequest(id json.RawMessage, result any, rpcErr *RPCError) error {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	return c.writeLineWithDeadline(resp, time.Now().Add(250*time.Millisecond))
}

// emit pushes ev onto the Events channel. ID/Sequence/SessionID/App/
// Process are deliberately left zero — [Client.Events]'s own
// contract says the caller fills those in via its own
// Emitter/activity.Bridge. Guarded by eventsMu against
// [Client.closeEvents] so a concurrent send never races the channel's
// close — see closeEvents's doc comment.
func (c *NDJSONBridgeClient) emit(ev runtimeevents.Event) {
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()
	if c.eventsClosed {
		return
	}
	select {
	case c.events <- ev:
	default:
		// Channel full — drop rather than block the sender (which may
		// be the reader loop itself) indefinitely. The 64-slot buffer
		// comfortably covers a single turn's chunk cadence observed
		// live; a stalled consumer is a caller bug, not something this
		// Client should deadlock over.
	}
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

// handleServerRequest answers a server-initiated JSON-RPC request (a
// frame carrying both `method` and `id`) from the agent. Per JSON-RPC
// 2.0, every such request requires a response — without one, the agent
// blocks waiting for it.
//
// `session/request_permission` maps onto
// agent.permission_requested/resolved per 17-acp.md's explicit mapping
// (Nanite repo). A configured best-effort responder selects one exact
// provider-offered option; without one, the Client retains its established
// "cancelled" outcome. Both paths emit a correlated request/resolved pair.
// The wire shape (`{options, sessionId, toolCall: {toolCallId, rawInput,
// ...}}`) is structurally identical across the four agents. Measured Claude,
// Codex, OpenCode and Pi tool paths can execute internally without ever
// invoking this method, consistent with 17-acp.md's documented expectation,
// though not exhaustively confirmed for every ACP-proxyable operation.
//
//nolint:misspell // quotes the wire outcome "cancelled"
func (c *NDJSONBridgeClient) handleServerRequest(frame rpcFrame) {
	if frame.Method != "session/request_permission" {
		_ = c.RespondToServerRequest(frame.ID, nil, &RPCError{
			Code:    -32601,
			Message: c.cfg.Component + ": no handler configured for server-initiated method " + frame.Method,
		})
		return
	}

	// Correlate the requested/resolved pair via ACP's OWN JSON-RPC
	// request id (a stable, protocol-native correlator) rather than a
	// synthesized runtimeevents ID/ParentID pair: [Client.Events]'s
	// documented contract leaves Event.ID zero for the caller's own
	// Emitter/activity.Bridge to assign, so this Client must not invent
	// one just to self-correlate two of its own events.
	acpRequestID := append(json.RawMessage(nil), frame.ID...)
	requestID := append(json.RawMessage(nil), frame.ID...)
	configured := c.permissions.Configured()
	var turnID string
	c.permissions.DispatchTurnRequest(frame.Params, func(admission PermissionDispatchAdmission) {
		if !admission.ActiveTurn {
			return
		}
		c.turnMu.Lock()
		turnID = c.currentTurnID
		c.turnMu.Unlock()
		c.emit(runtimeevents.Event{
			Kind:   runtimeevents.KindAgentPermissionRequested,
			TurnID: turnID,
			Payload: mustMarshal(map[string]any{
				"request_id": acpRequestID,
				"method":     frame.Method,
				"params":     frame.Params,
			}),
		})
	}, func(admission PermissionDispatchAdmission, resolution PermissionResolution) error {
		err := c.RespondToServerRequest(requestID, resolution.Result(), nil)
		if !admission.ActiveTurn {
			return err
		}
		payload := resolution.ResolvedEventPayload(acpRequestID)
		if !configured {
			payload = map[string]any{
				"request_id": acpRequestID,
				"allowed":    false,
				"reason":     c.cfg.Component + ": no approval handler configured",
			}
		}
		if err != nil {
			payload = resolution.DeliveryFailureEventPayload(acpRequestID)
		}
		c.emit(runtimeevents.Event{
			Kind:    runtimeevents.KindAgentPermissionResolved,
			TurnID:  turnID,
			Payload: mustMarshal(payload),
		})
		return err
	}, func(resolution PermissionResolution) {
		if resolution.ResponseError() != nil {
			c.abortPermissionTransport()
		}
		if diagnostic, ok := resolution.Diagnostic(); ok {
			c.reportDiagnostic(diagnostic)
		}
	})
}
