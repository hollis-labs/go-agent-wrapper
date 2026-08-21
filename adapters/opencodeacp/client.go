package opencodeacp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// acpProtocolVersion is the integer protocolVersion sent in `initialize`.
// Confirmed directly against opencode 1.15.6: sending the integer 1 (not
// the string the scraped spec summary suggested) round-trips cleanly —
// the live response echoes back `"protocolVersion":1`.
const acpProtocolVersion = 1

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

	mu        sync.Mutex
	launched  bool
	closed    bool
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	sessionID string

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

	waitDone chan struct{}
	waitErr  error
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
	return fmt.Sprintf("acp: jsonrpc error %d: %s", e.Code, e.Message)
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

// Launch implements [acp.Client]. It spawns `opencode acp` (or the
// configured override binary), performs the `initialize` + `session/new`
// (or, when params.SessionIDPreset is set, a best-effort `session/load`
// falling back to `session/new` on any error) handshake, and returns once
// the session is ready to accept a Prompt.
func (c *Client) Launch(ctx context.Context, params acp.LaunchParams) error {
	c.mu.Lock()
	if c.launched {
		c.mu.Unlock()
		return errors.New("opencodeacp: Launch called more than once")
	}
	c.launched = true
	c.mu.Unlock()

	binary := c.resolveBinary()
	args := append([]string{"acp"}, c.extraArgs...)
	cmd := exec.Command(binary, args...) //nolint:gosec // G204: operator-controlled binary/args
	if params.Cwd != "" {
		cmd.Dir = params.Cwd
	}
	if len(params.Env) > 0 {
		cmd.Env = params.Env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("opencodeacp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("opencodeacp: stdout pipe: %w", err)
	}
	// Drain stderr into the log rather than leaving it unread (opencode
	// acp writes diagnostic/error lines here — an unread pipe can block
	// the child once its OS buffer fills).
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("opencodeacp: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opencodeacp: start %q: %w", binary, err)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.waitDone = make(chan struct{})
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

	if _, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": acpProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("opencodeacp: initialize: %w", err)
	}

	if params.SessionIDPreset != "" {
		if sid, err := c.loadSession(ctx, params); err == nil {
			c.mu.Lock()
			c.sessionID = sid
			c.mu.Unlock()
		} else {
			// Best-effort resume only — session/load's exact param shape
			// beyond {sessionId, cwd, mcpServers} is not exhaustively
			// verified against every opencode version, so any failure
			// here (unknown session, incompatible shape, ...) falls back
			// to a fresh session rather than failing Launch outright.
			// LaunchParams's own doc permits implementations to "ignore
			// [resume] silently" when unsupported.
			if err := c.newSession(ctx, params); err != nil {
				_ = c.Close(context.Background())
				return fmt.Errorf("opencodeacp: session/new (after session/load fallback): %w", err)
			}
		}
	} else if err := c.newSession(ctx, params); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("opencodeacp: session/new: %w", err)
	}

	c.emit(runtimeevents.Event{Kind: runtimeevents.KindSessionReady})
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
	c.mu.Unlock()
	if sessionID == "" {
		return errors.New("opencodeacp: Prompt called before Launch established a session")
	}

	turnID := runtimeevents.NewTurnID()
	c.turnMu.Lock()
	c.currentTurnID = turnID
	c.turnMu.Unlock()

	c.emit(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: turnID})

	params := map[string]any{
		"sessionId": sessionID,
		"prompt": []map[string]any{
			{"type": "text", "text": prompt},
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
// notification for the in-flight turn. Confirmed live (see package doc)
// to be a genuine mid-turn abort for OpenCode, not an
// acknowledge-and-let-finish: the in-flight `session/prompt` response
// arrives almost immediately afterward, well before natural completion.
// Cancel does not itself wait for that response — [Client.Prompt]'s own
// background goroutine emits the resulting turn.completed/turn.failed
// event when it arrives. A no-op (returns nil) when no turn is active.
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
// [adapters.InterruptTurn] — verified live against opencode 1.15.6 (see
// package doc for the measured cancel-to-response latency mid-generation).
// This is a static, per-implementation fact, independent of whether
// [Client.Launch] has been called — matching [acp.DescriptorFor]'s
// expectation that Describe() can be answered without a live session.
func (c *Client) InterruptCapability() adapters.InterruptCapability {
	return adapters.InterruptTurn
}

// Events implements [acp.Client].
func (c *Client) Events() <-chan runtimeevents.Event { return c.events }

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
	c.mu.Unlock()

	c.failPending(errors.New("opencodeacp: client closed"))
	c.closeEvents()

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if waitDone == nil {
		return nil
	}

	grace := 2 * time.Second
	select {
	case <-waitDone:
		return nil
	case <-time.After(grace):
	case <-ctx.Done():
	}

	_ = cmd.Process.Kill()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
	}
	return nil
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
	close(waitDone)

	c.failPending(errors.New("opencodeacp: process exited before response"))

	payload := map[string]any{}
	if err != nil {
		payload["error"] = err.Error()
	}
	c.emit(runtimeevents.Event{
		Kind:    runtimeevents.KindProcessExited,
		Payload: mustMarshal(payload),
	})
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
		// Diagnostic-only. opencode acp's own stderr carries structured
		// log lines (per --print-logs/--log-level), not protocol frames.
		// A future revision could surface these as a low-priority
		// runtimeevents kind; today they are intentionally dropped
		// rather than misclassified as protocol activity.
		_ = scanner.Text()
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
	for scanner.Scan() {
		raw := scanner.Bytes()
		var frame rpcFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
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
		return fmt.Errorf("opencodeacp: encode: %w", err)
	}
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("opencodeacp: not launched (no stdin)")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = stdin.Write(append(encoded, '\n'))
	return err
}

// respondToServerRequest answers a server-initiated JSON-RPC request
// (a frame carrying both `method` and `id`). JSON-RPC 2.0 requires a
// response for every request that carries an id — without one, opencode
// blocks waiting for it.
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
		// live (sub-100ms intervals between session/update
		// notifications); a stalled consumer is a caller bug, not
		// something this Client should deadlock over.
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
