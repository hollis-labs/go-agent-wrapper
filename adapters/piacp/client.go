package piacp

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

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// acpProtocolVersion is the integer protocolVersion sent in `initialize`.
// Confirmed directly against pi-acp 0.0.33: sending the integer 1 round-trips
// cleanly, matching every other ACP implementation this repo has verified
// (opencodeacp, copilotacp).
const acpProtocolVersion = 1

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

	// promptCloseMu linearizes Prompt's admission and request write with
	// Close's closed transition and session/close request. It must not guard
	// general protocol writes: server-request responses need to remain able to
	// run while Close waits for its response.
	promptCloseMu sync.Mutex

	mu           sync.Mutex
	launched     bool
	closed       bool
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	sessionID    string
	systemPrompt string
	sessionClose bool

	writeMu sync.Mutex // serializes writes to stdin across goroutines

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

	waitDone     chan struct{}
	waitErr      error
	termination  *acp.TransportTermination
	terminated   chan struct{}
	lifetimeCtx  context.Context
	lifetimeStop context.CancelFunc
	permissions  *acp.BestEffortPermissionRequests
	diagnosticMu sync.Mutex
	diagnostic   func(acp.Diagnostic)
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
	c := &Client{
		events:  make(chan runtimeevents.Event, 64),
		pending: make(map[int64]pendingRPCResponse),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// rpcResponse is the internal envelope delivered to a pending Call.
// Exactly one of result/err is set.
type rpcResponse struct {
	result json.RawMessage
	err    *rpcError
}

type pendingRPCResponse struct {
	response chan rpcResponse
	prompt   bool
}

// rpcError mirrors JSON-RPC 2.0's error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "<nil rpc error>"
	}
	return fmt.Sprintf("piacp: jsonrpc error %d: %s", e.Code, e.Message)
}

// rpcFrame is the minimal shape the reader loop needs to classify an
// inbound line into response / notification / server-initiated request.
type rpcFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
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

// Launch implements [acp.Client]. It spawns the `pi-acp` bridge (or the
// configured override binary), performs the `initialize` + `session/new`
// (or, when params.SessionIDPreset is set and the capability is advertised,
// a fail-closed `session/load`) handshake, and returns once
// the session is ready to accept a Prompt.
func (c *Client) Launch(ctx context.Context, params acp.LaunchParams) error {
	c.mu.Lock()
	if c.launched {
		c.mu.Unlock()
		return errors.New("piacp: Launch called more than once")
	}
	c.launched = true
	c.mu.Unlock()

	binary, args := c.resolveCommand()
	cmd := exec.Command(binary, args...) //nolint:gosec // G204: operator-controlled binary/args
	if params.Cwd != "" {
		cmd.Dir = params.Cwd
	}
	if len(params.Env) > 0 {
		cmd.Env = params.Env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("piacp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("piacp: stdout pipe: %w", err)
	}
	// Drain stderr into the log rather than leaving it unread (npx and
	// pi-acp both write diagnostic/error lines here — an unread pipe can
	// block the child once its OS buffer fills).
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("piacp: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("piacp: start %q: %w", binary, err)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.waitDone = make(chan struct{})
	c.terminated = make(chan struct{})
	c.termination = acp.NewTransportTermination(true)
	c.lifetimeCtx, c.lifetimeStop = context.WithCancel(context.Background())
	c.permissions = acp.NewBestEffortPermissionRequests(params.BestEffortPermissionRequestResponder)
	c.permissions.SetResponseGate(&c.promptCloseMu)
	c.diagnostic = params.OnDiagnostic
	c.mu.Unlock()

	if pid := cmd.Process.Pid; pid != 0 {
		c.emit(runtimeevents.Event{
			Kind:    runtimeevents.KindProcessStarted,
			Payload: mustMarshal(map[string]any{"pid": pid}),
		})
	}

	go c.drainStderr(stderr)
	go c.readLoop(stdout)
	go c.waitProcess()
	go c.coordinateTermination()

	initializeResult, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": acpProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	})
	if err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("piacp: initialize: %w", err)
	}
	initialize, err := acp.ParseInitializeResult(initializeResult, acpProtocolVersion)
	if err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("piacp: initialize negotiation: %w", err)
	}
	if err := c.authenticate(ctx, initialize, params.AuthMethodID); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("piacp: authenticate: %w", err)
	}
	c.mu.Lock()
	c.sessionClose = initialize.SessionClose
	c.mu.Unlock()

	if params.SessionIDPreset != "" && initialize.LoadSession {
		if err := c.loadSession(ctx, params); err != nil {
			_ = c.Close(context.Background())
			return fmt.Errorf("piacp: session/load: %w", err)
		}
		c.mu.Lock()
		c.sessionID = params.SessionIDPreset
		c.mu.Unlock()
	} else if err := c.newSession(ctx, params); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("piacp: session/new: %w", err)
	}
	c.permissions.SetSessionID(c.ProviderSessionID())
	if err := c.configureSession(ctx, params); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("piacp: configure session: %w", err)
	}
	c.mu.Lock()
	c.systemPrompt = params.SystemPrompt
	c.mu.Unlock()

	c.emit(runtimeevents.Event{Kind: runtimeevents.KindSessionReady})
	return nil
}

func (c *Client) authenticate(ctx context.Context, initialize acp.InitializeResult, methodID string) error {
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
		_, err := c.call(ctx, "authenticate", map[string]any{"methodId": methodID})
		return err
	}
	return fmt.Errorf("authentication method %q was not advertised", methodID)
}

func (c *Client) configureSession(ctx context.Context, params acp.LaunchParams) error {
	sessionID := c.ProviderSessionID()
	if params.SessionModeID != "" {
		if _, err := c.call(ctx, "session/set_mode", map[string]any{"sessionId": sessionID, "modeId": params.SessionModeID}); err != nil {
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
		if _, err := c.call(ctx, "session/set_config_option", request); err != nil {
			return fmt.Errorf("option %q: %w", key, err)
		}
	}
	return nil
}

func (c *Client) newSession(ctx context.Context, params acp.LaunchParams) error {
	result, err := c.call(ctx, "session/new", map[string]any{
		"cwd":        params.Cwd,
		"mcpServers": []any{},
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

// loadSession attempts to resume params.SessionIDPreset via
// `session/load`. Unlike [Client.newSession], it does not decode a
// sessionId out of the result — pi-acp's session/load response carries
// no such field (confirmed live, see package doc); the caller
// ([Client.Launch]) keeps using params.SessionIDPreset directly on
// success.
func (c *Client) loadSession(ctx context.Context, params acp.LaunchParams) error {
	_, err := c.call(ctx, "session/load", map[string]any{
		"sessionId":  params.SessionIDPreset,
		"cwd":        params.Cwd,
		"mcpServers": []any{},
	})
	return err
}

// Prompt implements [acp.Client]. It sends `session/prompt` and returns
// once the request has been written to the wire — NOT once the turn
// completes. Turn progress (message chunks, tool calls) and completion
// (turn.completed / turn.failed, carrying the ACP `stopReason`) surface
// asynchronously via [Client.Events].
func (c *Client) Prompt(ctx context.Context, prompt string) error {
	c.promptCloseMu.Lock()
	defer c.promptCloseMu.Unlock()

	c.turnMu.Lock()
	if c.currentTurnID != "" {
		c.turnMu.Unlock()
		return errors.New("piacp: a turn is already in flight")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.turnMu.Unlock()
		return errors.New("piacp: client is closed")
	}
	sessionID := c.sessionID
	if sessionID == "" {
		c.mu.Unlock()
		c.turnMu.Unlock()
		return errors.New("piacp: Prompt called before Launch established a session")
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
	// [acp.Client.Prompt]'s documented contract.
	_, respCh, err := c.beginCall(ctx, "session/prompt", params)
	if err != nil {
		c.finishTurn(turnID, nil, err)
		c.turnWG.Done()
		return err
	}

	go func() {
		defer c.turnWG.Done()
		result, err := c.awaitCall(lifetimeCtx, respCh)
		c.finishTurn(turnID, result, err)
	}()

	return nil
}

func (c *Client) finishTurn(turnID string, result json.RawMessage, callErr error) {
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
			Payload: mustMarshal(map[string]any{"error": callErr.Error()}),
		})
		return
	}

	var decoded struct {
		StopReason string          `json:"stopReason"`
		Usage      json.RawMessage `json:"usage,omitempty"`
	}
	_ = json.Unmarshal(result, &decoded)
	payload := map[string]any{"stop_reason": decoded.StopReason}
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

// Cancel implements [acp.Client]. It sends ACP's `session/cancel`
// notification for the in-flight turn. Confirmed live (see package doc)
// to be a genuine mid-turn abort for Pi via pi-acp, not an
// acknowledge-and-let-finish: a real, definitely-still-running `bash`
// tool subprocess was killed and the in-flight `session/prompt` response
// arrived within single-digit milliseconds carrying `stopReason:
// "cancelled"`. Cancel does not itself wait for that response —
// [Client.Prompt]'s own background goroutine emits the resulting
// turn.completed/turn.failed event when it arrives. A no-op (returns
// nil) when no turn is active.
func (c *Client) Cancel(ctx context.Context) error {
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
	return c.notify(ctx, map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/cancel",
		"params":  map[string]any{"sessionId": sessionID},
	})
}

// InterruptCapability implements [acp.Client]. Returns
// [adapters.InterruptTurn] — verified live against pi-acp 0.0.33 + pi
// 0.84.2 (see package doc for the measured cancel-to-response latency
// against a genuinely still-running subprocess). This is a static,
// per-implementation fact, independent of whether [Client.Launch] has
// been called — matching [acp.DescriptorFor]'s expectation that
// Describe() can be answered without a live session.
func (c *Client) InterruptCapability() adapters.InterruptCapability {
	return adapters.InterruptTurn
}

// Events implements [acp.Client].
func (c *Client) Events() <-chan runtimeevents.Event { return c.events }

// ProviderSessionID returns the id established by session/new or session/load.
func (c *Client) ProviderSessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// Close implements [acp.Client]. Terminates the bridge subprocess
// (closing stdin first to give it a chance to exit on EOF, then killing
// it after a grace period) and closes the Events channel exactly once.
// Safe to call even if Launch was never called or failed, and safe to
// call more than once.
func (c *Client) Close(ctx context.Context) error {
	c.promptCloseMu.Lock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.promptCloseMu.Unlock()
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
	permissions.Close()
	if lifetimeStop != nil {
		lifetimeStop()
	}
	var closeErr error
	if sessionClose && sessionID != "" {
		closeCtx, cancel := context.WithTimeout(ctx, time.Second)
		closeResult := make(chan error, 1)
		go func() {
			_, err := c.call(closeCtx, "session/close", map[string]any{"sessionId": sessionID})
			closeResult <- err
		}()
		select {
		case closeErr = <-closeResult:
		case <-closeCtx.Done():
			closeErr = closeCtx.Err()
		}
		cancel()
	}
	c.promptCloseMu.Unlock()

	c.failPending(errors.New("piacp: client closed"))
	if stdin != nil {
		_ = stdin.Close()
	}
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
			<-terminated
		}
		return closeErr
	case <-time.After(grace):
	case <-ctx.Done():
	}

	_ = cmd.Process.Kill()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
	}
	if terminated != nil {
		select {
		case <-terminated:
		case <-ctx.Done():
		}
	}
	return closeErr
}

func (c *Client) waitProcess() {
	c.mu.Lock()
	cmd := c.cmd
	waitDone := c.waitDone
	c.mu.Unlock()
	err := cmd.Wait()
	c.mu.Lock()
	c.waitErr = err
	termination := c.termination
	c.mu.Unlock()
	termination.ReportProcess(acp.TransportProcessResult{ExitCode: cmd.ProcessState.ExitCode(), Err: err})
	close(waitDone)
}

func (c *Client) coordinateTermination() {
	c.mu.Lock()
	termination := c.termination
	cmd := c.cmd
	stdin := c.stdin
	terminated := c.terminated
	lifetimeStop := c.lifetimeStop
	permissions := c.permissions
	c.mu.Unlock()
	result := termination.Coordinate(func() {
		if stdin != nil {
			_ = stdin.Close()
		}
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
func (c *Client) closeEvents() {
	c.turnWG.Wait()
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()
	if c.eventsClosed {
		return
	}
	c.eventsClosed = true
	close(c.events)
}

func (c *Client) drainStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		// Diagnostic-only. npx/pi-acp's own stderr carries install/log
		// noise, not protocol frames. Surface it only through the opt-in
		// redacted diagnostic callback, never as model activity.
		c.reportDiagnostic(acp.NewDiagnostic(acp.DiagnosticStderr, "ACP child stderr", scanner.Text()))
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
func (c *Client) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	malformed := false
	for scanner.Scan() {
		raw := scanner.Bytes()
		var frame rpcFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			malformed = true
			c.reportDiagnostic(acp.NewDiagnostic(acp.DiagnosticMalformedJSON, "invalid JSON-RPC frame", string(raw)))
			continue
		}
		switch {
		case frame.Method != "" && len(frame.ID) != 0:
			if _, ok := acp.DecodeJSONRPCRequestID(frame.ID); !ok {
				malformed = true
				c.reportDiagnostic(acp.NewDiagnostic(acp.DiagnosticProtocol, "invalid JSON-RPC request id", ""))
				continue
			}
			c.handleServerRequest(frame)
		case frame.Method != "":
			c.handleNotification(frame.Method, frame.Params)
		case len(frame.ID) != 0:
			c.deliverResponse(frame.ID, frame.Result, frame.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		c.reportReadError("reading ACP protocol stream", err)
	}
	c.permissions.Close()
	c.failPending(errors.New("piacp: protocol stream closed before response"))
	c.mu.Lock()
	termination := c.termination
	c.mu.Unlock()
	termination.ReportRead(acp.TransportReadResult{Malformed: malformed, Err: scanner.Err()})
}

func (c *Client) reportDiagnostic(d acp.Diagnostic) {
	c.diagnosticMu.Lock()
	defer c.diagnosticMu.Unlock()
	c.mu.Lock()
	fn := c.diagnostic
	c.mu.Unlock()
	if fn != nil {
		fn(d)
	}
}

func (c *Client) reportReadError(message string, err error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if !closed {
		c.reportDiagnostic(acp.NewDiagnostic(acp.DiagnosticProtocol, message, err.Error()))
	}
}

func (c *Client) deliverResponse(idRaw json.RawMessage, result json.RawMessage, rpcErr *rpcError) {
	var id int64
	if err := json.Unmarshal(idRaw, &id); err != nil {
		return // non-numeric id we never allocated; drop.
	}
	c.pendMu.Lock()
	pending, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.pendMu.Unlock()
	if !ok {
		return
	}
	if pending.prompt {
		c.permissions.CloseTurnAdmission()
	}
	pending.response <- rpcResponse{result: result, err: rpcErr}
	close(pending.response)
}

func (c *Client) failPending(err error) {
	c.pendMu.Lock()
	pending := c.pending
	c.pending = make(map[int64]pendingRPCResponse)
	c.pendMu.Unlock()
	for _, call := range pending {
		call.response <- rpcResponse{err: &rpcError{Code: -32000, Message: err.Error()}}
		close(call.response)
	}
}

// beginCall allocates a request id, registers the pending channel, and
// writes the request frame — but does not wait for the response. Pairs
// with [Client.awaitCall].
func (c *Client) beginCall(ctx context.Context, method string, params any) (int64, chan rpcResponse, error) {
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
	if err := c.writeLine(reqFrame{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		return 0, nil, err
	}
	return id, respCh, nil
}

func (c *Client) awaitCall(ctx context.Context, respCh chan rpcResponse) (json.RawMessage, error) {
	select {
	case resp := <-respCh:
		if resp.err != nil {
			return nil, resp.err
		}
		return resp.result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// call sends a JSON-RPC request and blocks for the matching response.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	_, respCh, err := c.beginCall(ctx, method, params)
	if err != nil {
		return nil, err
	}
	return c.awaitCall(ctx, respCh)
}

// notify writes a pre-built JSON-RPC notification frame (no id).
func (c *Client) notify(ctx context.Context, frame any) error {
	writeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.writeLineWithDeadline(frame, time.Now().Add(250*time.Millisecond))
	}()
	select {
	case err := <-done:
		if err != nil {
			c.mu.Lock()
			stdin := c.stdin
			c.mu.Unlock()
			if stdin != nil {
				_ = stdin.Close()
			}
		}
		return err
	case <-writeCtx.Done():
		c.mu.Lock()
		stdin := c.stdin
		c.mu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}
		return writeCtx.Err()
	}
}

// abortPermissionTransport is the fail-closed path for an undeliverable
// permission response. It intentionally avoids promptCloseMu: response I/O may
// still own that ordering gate when the failure is observed.
func (c *Client) abortPermissionTransport() {
	c.mu.Lock()
	stdin := c.stdin
	cmd := c.cmd
	lifetimeStop := c.lifetimeStop
	c.mu.Unlock()
	if lifetimeStop != nil {
		lifetimeStop()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (c *Client) writeLine(v any) error {
	return c.writeLineWithDeadline(v, time.Time{})
}

type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

func (c *Client) writeLineWithDeadline(v any, deadline time.Time) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("piacp: encode: %w", err)
	}
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("piacp: not launched (no stdin)")
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

// respondToServerRequest answers a server-initiated JSON-RPC request
// (a frame carrying both `method` and `id`). JSON-RPC 2.0 requires a
// response for every request that carries an id — without one, pi-acp
// (and, transitively, the `pi` process it owns) blocks waiting for it.
func (c *Client) respondToServerRequest(id json.RawMessage, result any, rpcErr *rpcError) error {
	resp := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id)}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	return c.writeLineWithDeadline(resp, time.Now().Add(250*time.Millisecond))
}

// emit pushes ev onto the Events channel. ID/Sequence/SessionID/App/
// Process are deliberately left zero — [acp.Client.Events]'s own
// contract says the caller fills those in via its own
// Emitter/activity.Bridge. Guarded by eventsMu against
// [Client.closeEvents] so a concurrent send never races the channel's
// close — see closeEvents's doc comment.
func (c *Client) emit(ev runtimeevents.Event) {
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()
	if c.eventsClosed {
		return
	}
	select {
	case c.events <- ev:
	default:
		// Channel full — drop rather than block the sender (which may be
		// the reader loop itself) indefinitely. The 64-slot buffer
		// comfortably covers a single turn's chunk cadence observed live
		// (sub-second intervals between session/update notifications,
		// including pi-acp's own incremental terminal_output ticks); a
		// stalled consumer is a caller bug, not something this Client
		// should deadlock over.
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

var _ acp.Client = (*Client)(nil)
