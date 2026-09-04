package claudeacp

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
// Confirmed directly against @agentclientprotocol/claude-agent-acp
// 0.70.0: sending the integer 1 round-trips cleanly — the live response
// echoes back `"protocolVersion":1`, same convention
// [adapters/opencodeacp] already verified for its own bridge target.
const acpProtocolVersion = 1

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
	// pending maps an outbound request id to the channel its eventual
	// response is delivered on. Guarded by pendMu.
	pending map[int64]chan rpcResponse

	turnMu        sync.Mutex
	currentTurnID string

	eventsMu     sync.Mutex
	events       chan runtimeevents.Event
	eventsClosed bool

	waitDone     chan struct{}
	waitErr      error
	diagnosticMu sync.Mutex
	diagnostic   func(acp.Diagnostic)
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
	c := &Client{
		events:  make(chan runtimeevents.Event, 64),
		pending: make(map[int64]chan rpcResponse),
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
	return fmt.Sprintf("claudeacp: jsonrpc error %d: %s", e.Code, e.Message)
}

// rpcFrame is the minimal shape the reader loop needs to classify an
// inbound line into response / notification / server-initiated request.
type rpcFrame struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
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

// Launch implements [acp.Client]. It spawns the claude-agent-acp bridge
// (see [Client.resolveCommand] for how the command is resolved),
// performs the `initialize` + `session/new` (or, when
// params.SessionIDPreset is set, a best-effort `session/load` falling
// back to `session/new` on any error) handshake, and returns once the
// session is ready to accept a Prompt.
func (c *Client) Launch(ctx context.Context, params acp.LaunchParams) error {
	c.mu.Lock()
	if c.launched {
		c.mu.Unlock()
		return errors.New("claudeacp: Launch called more than once")
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
		return fmt.Errorf("claudeacp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("claudeacp: stdout pipe: %w", err)
	}
	// Drain stderr into the log rather than leaving it unread —
	// claude-agent-acp deliberately redirects console.log/info/warn/debug
	// to stderr (its own source: "we redirect everything else to stderr
	// to make sure it doesn't interfere with ACP") so stdout stays a
	// clean JSON-RPC channel; an unread stderr pipe can still block the
	// child once its OS buffer fills.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("claudeacp: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("claudeacp: start %q: %w", binary, err)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.waitDone = make(chan struct{})
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

	initializeResult, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": acpProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	})
	if err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("claudeacp: initialize: %w", err)
	}
	if err := c.authenticate(ctx, initializeResult, params.AuthMethodID); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("claudeacp: authenticate: %w", err)
	}
	c.mu.Lock()
	c.sessionClose = acp.InitializeSupportsSessionClose(initializeResult)
	c.mu.Unlock()

	if params.SessionIDPreset != "" {
		if sid, err := c.loadSession(ctx, params); err == nil {
			c.mu.Lock()
			c.sessionID = sid
			c.mu.Unlock()
		} else {
			// Best-effort resume only, same convention
			// [adapters/opencodeacp] uses for its own SessionIDPreset
			// handling — LaunchParams's own doc permits implementations
			// to "ignore [resume] silently" when unsupported. Not
			// exhaustively live-verified for this bridge (this task's
			// live verification exercised fresh sessions; the bridge's
			// `initialize` response does advertise
			// `agentCapabilities.loadSession: true` and
			// `sessionCapabilities.resume`, so `session/load` is a real,
			// advertised capability, not a guess).
			if err := c.newSession(ctx, params); err != nil {
				_ = c.Close(context.Background())
				return fmt.Errorf("claudeacp: session/new (after session/load fallback): %w", err)
			}
		}
	} else if err := c.newSession(ctx, params); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("claudeacp: session/new: %w", err)
	}
	if err := c.configureSession(ctx, params); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("claudeacp: configure session: %w", err)
	}
	c.mu.Lock()
	c.systemPrompt = params.SystemPrompt
	c.mu.Unlock()

	c.emit(runtimeevents.Event{Kind: runtimeevents.KindSessionReady})
	return nil
}

func (c *Client) authenticate(ctx context.Context, initializeResult json.RawMessage, methodID string) error {
	if methodID == "" {
		return nil
	}
	var response struct {
		AuthMethods []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"authMethods"`
	}
	if err := json.Unmarshal(initializeResult, &response); err != nil {
		return fmt.Errorf("decode initialize authMethods: %w", err)
	}
	for _, method := range response.AuthMethods {
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

func (c *Client) loadSession(ctx context.Context, params acp.LaunchParams) (string, error) {
	result, err := c.call(ctx, "session/load", map[string]any{
		"sessionId":  params.SessionIDPreset,
		"cwd":        params.Cwd,
		"mcpServers": []any{},
	})
	if err != nil {
		return "", err
	}
	var decoded struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		return "", fmt.Errorf("decode session/load result: %w", err)
	}
	if decoded.SessionID == "" {
		return "", errors.New("session/load returned an empty sessionId")
	}
	return decoded.SessionID, nil
}

// Prompt implements [acp.Client]. It sends `session/prompt` and returns
// once the request has been written to the wire — NOT once the turn
// completes. Turn progress (message/thought chunks, tool calls) and
// completion (turn.completed / turn.failed, carrying the ACP
// `stopReason`) surface asynchronously via [Client.Events].
func (c *Client) Prompt(ctx context.Context, prompt string) error {
	c.mu.Lock()
	sessionID := c.sessionID
	systemPrompt := c.systemPrompt
	c.systemPrompt = ""
	c.mu.Unlock()
	if sessionID == "" {
		return errors.New("claudeacp: Prompt called before Launch established a session")
	}

	turnID := runtimeevents.NewTurnID()
	c.turnMu.Lock()
	c.currentTurnID = turnID
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
		return err
	}

	go func() {
		result, err := c.awaitCall(ctx, respCh)
		c.finishTurn(turnID, result, err)
	}()

	return nil
}

func (c *Client) finishTurn(turnID string, result json.RawMessage, callErr error) {
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
// notification for the in-flight turn. Confirmed both by source (the
// bridge's `cancel()` calls the Claude Agent SDK's own
// `query.interrupt()`) and live (see package doc for the measured
// cancel-to-response latency mid-generation) to be a genuine mid-turn
// abort, not an acknowledge-and-let-finish. Cancel does not itself wait
// for the resulting response — [Client.Prompt]'s own background goroutine
// emits the resulting turn.completed/turn.failed event when it arrives. A
// no-op (returns nil) when no turn is active.
func (c *Client) Cancel(ctx context.Context) error {
	c.mu.Lock()
	sessionID := c.sessionID
	c.mu.Unlock()
	c.turnMu.Lock()
	turnID := c.currentTurnID
	c.turnMu.Unlock()
	if sessionID == "" || turnID == "" {
		return nil
	}
	return c.notify(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/cancel",
		"params":  map[string]any{"sessionId": sessionID},
	})
}

// InterruptCapability implements [acp.Client]. Returns
// [adapters.InterruptTurn] — verified both by source (the bridge's real
// `cancel()` calls `query.interrupt()`) and live against
// @agentclientprotocol/claude-agent-acp 0.70.0 (see package doc for the
// measured cancel-to-response latency mid-generation). This is a static,
// per-implementation fact, independent of whether [Client.Launch] has
// been called — matching [acp.DescriptorFor]'s expectation that
// Describe() can be answered without a live session.
func (c *Client) InterruptCapability() adapters.InterruptCapability {
	return adapters.InterruptTurn
}

// Events implements [acp.Client].
func (c *Client) Events() <-chan runtimeevents.Event { return c.events }

func (c *Client) ProviderSessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// Close implements [acp.Client]. Terminates the subprocess (closing
// stdin first to give it a chance to exit on EOF, then killing it after
// a grace period) and closes the Events channel exactly once. Safe to
// call even if Launch was never called or failed, and safe to call more
// than once.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cmd := c.cmd
	stdin := c.stdin
	waitDone := c.waitDone
	sessionID := c.sessionID
	sessionClose := c.sessionClose
	c.mu.Unlock()
	var closeErr error
	if sessionClose && sessionID != "" {
		closeCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, closeErr = c.call(closeCtx, "session/close", map[string]any{"sessionId": sessionID})
		cancel()
	}

	c.failPending(errors.New("claudeacp: client closed"))
	c.closeEvents()

	if cmd == nil || cmd.Process == nil {
		return closeErr
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if waitDone == nil {
		return closeErr
	}

	grace := 2 * time.Second
	select {
	case <-waitDone:
		return closeErr
	case <-time.After(grace):
	case <-ctx.Done():
	}

	_ = cmd.Process.Kill()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
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
	c.mu.Unlock()
	c.failPending(errors.New("claudeacp: process exited before response"))

	payload := map[string]any{"exit_code": cmd.ProcessState.ExitCode()}
	if err != nil {
		payload["error"] = err.Error()
	}
	c.emit(runtimeevents.Event{
		Kind:    runtimeevents.KindProcessExited,
		Payload: mustMarshal(payload),
	})
	close(waitDone)
	c.closeEvents()
}

// closeEvents closes the Events channel exactly once, guarded by
// eventsMu so a concurrent [Client.emit] call either completes its send
// before the close (still open) or observes eventsClosed==true and skips
// the send — never races the close() call itself. Called from both
// [Client.Close] (explicit teardown) and [Client.waitProcess] (the
// subprocess exiting on its own), whichever happens first.
func (c *Client) closeEvents() {
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
		// Diagnostic-only. claude-agent-acp deliberately routes its own
		// console.log/info/warn/debug here (see Launch's doc comment), not
		// protocol frames. Surface them only through the opt-in redacted
		// diagnostic callback, never as model activity.
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
// notification; id-only => response to one of ours) — same structure
// [adapters/opencodeacp.Client.readLoop] already uses.
func (c *Client) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	for scanner.Scan() {
		raw := scanner.Bytes()
		var frame rpcFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			c.reportDiagnostic(acp.NewDiagnostic(acp.DiagnosticMalformedJSON, "invalid JSON-RPC frame", string(raw)))
			continue
		}
		switch {
		case frame.Method != "" && frame.ID != nil:
			c.handleServerRequest(frame)
		case frame.Method != "":
			c.handleNotification(frame.Method, frame.Params)
		case frame.ID != nil:
			c.deliverResponse(*frame.ID, frame.Result, frame.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		c.reportReadError("reading ACP protocol stream", err)
	}
	c.failPending(errors.New("claudeacp: protocol stream closed before response"))
	c.mu.Lock()
	waitDone := c.waitDone
	c.mu.Unlock()
	if waitDone != nil {
		select {
		case <-waitDone:
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	c.closeEvents()
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
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.pendMu.Unlock()
	if !ok {
		return
	}
	ch <- rpcResponse{result: result, err: rpcErr}
	close(ch)
}

func (c *Client) failPending(err error) {
	c.pendMu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan rpcResponse)
	c.pendMu.Unlock()
	for _, ch := range pending {
		ch <- rpcResponse{err: &rpcError{Code: -32000, Message: err.Error()}}
		close(ch)
	}
}

// beginCall allocates a request id, registers the pending channel, and
// writes the request frame — but does not wait for the response. Pairs
// with [Client.awaitCall].
func (c *Client) beginCall(ctx context.Context, method string, params any) (int64, chan rpcResponse, error) {
	id := c.nextID.Add(1)
	respCh := make(chan rpcResponse, 1)
	c.pendMu.Lock()
	c.pending[id] = respCh
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
func (c *Client) notify(frame any) error {
	return c.writeLine(frame)
}

func (c *Client) writeLine(v any) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("claudeacp: encode: %w", err)
	}
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("claudeacp: not launched (no stdin)")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = stdin.Write(append(encoded, '\n'))
	return err
}

// respondToServerRequest answers a server-initiated JSON-RPC request
// (a frame carrying both `method` and `id`). JSON-RPC 2.0 requires a
// response for every request that carries an id — without one, the
// bridge blocks waiting for it.
func (c *Client) respondToServerRequest(id json.RawMessage, result any, rpcErr *rpcError) {
	resp := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id)}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	_ = c.writeLine(resp)
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

var _ acp.Client = (*Client)(nil)
