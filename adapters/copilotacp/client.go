package copilotacp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/internal/closegate"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Errors returned by [Client]'s methods. Use [errors.Is] to detect.
var (
	// ErrNotLaunched is returned by [Client.Prompt]/[Client.Cancel] when
	// called before [Client.Launch] has completed successfully.
	ErrNotLaunched = errors.New("copilotacp: client not launched")
	// ErrAlreadyLaunched is returned by [Client.Launch] when called more
	// than once on the same Client — mirroring [wrapper.Wrapper]'s own
	// single-use contract.
	ErrAlreadyLaunched = errors.New("copilotacp: client already launched")
	// ErrTurnInFlight is returned by [Client.Prompt] when a previous
	// turn's session/prompt response has not yet arrived.
	ErrTurnInFlight = errors.New("copilotacp: a turn is already in flight")
)

const (
	defaultClientName    = "nanite"
	defaultClientVersion = "0.0.0"
	acpProtocolVersion   = 1
)

// Option configures a [Client] constructed via [NewClient].
type Option func(*Client)

// WithBinary overrides the `copilot` executable path. Empty (the
// default) resolves "copilot" via PATH at spawn time. Ignored when
// [WithDialOnly] is set (no subprocess is spawned in that mode).
func WithBinary(path string) Option { return func(c *Client) { c.binary = path } }

// WithHost sets the host a TCP-transport [Client] dials. Defaults to
// "127.0.0.1". Ignored for stdio transport.
func WithHost(host string) Option { return func(c *Client) { c.host = host } }

// WithPort sets the TCP port a TCP-transport [Client] spawns Copilot CLI
// with (`--acp --port <N>`) or dials directly under [WithDialOnly]. Zero
// (the default) auto-picks a free local port at Launch time — ignored
// under [WithDialOnly], where Port must be the already-listening
// daemon's real port.
func WithPort(port int) Option { return func(c *Client) { c.port = port } }

// WithExtraArgs appends additional CLI arguments after `--acp` (and
// `--port <N>` for TCP). Ignored under [WithDialOnly].
func WithExtraArgs(args ...string) Option {
	return func(c *Client) { c.extraArgs = append(c.extraArgs, args...) }
}

// WithClientInfo overrides the `clientInfo` name/version sent during
// ACP's `initialize` handshake. Defaults to ("nanite", "0.0.0").
func WithClientInfo(name, version string) Option {
	return func(c *Client) { c.clientName, c.clientVersion = name, version }
}

// WithStderr forwards the spawned Copilot CLI process's stderr to w.
// Nil (the default) discards it. Ignored under [WithDialOnly].
func WithStderr(w io.Writer) Option { return func(c *Client) { c.stderr = w } }

// WithDialOnly configures a TCP-transport [Client] to connect to an
// already-running `copilot --acp --port <port>` daemon at host:port
// instead of spawning its own — the "connects to" half of
// [acp.Client.Launch]'s doc comment ("starts (or connects to) the
// underlying ACP-speaking agent"). [Client.Close] does not attempt to
// terminate a dial-only connection's remote process, since this Client
// never spawned it. Ignored for stdio transport.
func WithDialOnly(host string, port int) Option {
	return func(c *Client) {
		c.dialOnly = true
		c.host = host
		c.port = port
	}
}

// Client is a genuinely self-contained [acp.Client] implementation for
// GitHub Copilot CLI's `--acp` mode, supporting both
// [adapters.TransportStdio] and [adapters.TransportTCP]. See the package
// doc for the full design rationale and what was live-verified against
// the real binary.
//
// A Client is single-use, mirroring [acp.Client]'s own single-session
// contract: construct via [NewClient], call [Client.Launch] once, then
// [Client.Prompt]/[Client.Cancel] any number of times, and
// [Client.Close] exactly once.
type Client struct {
	transport adapters.Transport

	binary    string
	host      string
	port      int
	dialOnly  bool
	extraArgs []string

	clientName    string
	clientVersion string
	stderr        io.Writer
	permissions   *acp.BestEffortPermissionRequests
	diagnosticMu  sync.Mutex
	diagnostic    func(acp.Diagnostic)

	// promptCloseMu orders an admitted Prompt's request write with Close's
	// bounded graceful session/close attempt. The closed state under mu seals
	// new admissions before Close waits for this gate. It must not guard general
	// protocol writes: server-request responses need to remain able to run while
	// Close waits for its response.
	promptCloseMu sync.Mutex

	mu            sync.Mutex
	started       bool
	closed        bool
	launched      bool
	cmd           *exec.Cmd
	conn          net.Conn
	stdin         io.WriteCloser
	writer        io.Writer
	sessionID     string
	systemPrompt  string
	sessionClose  bool
	turnInFlight  bool
	currentTurnID string

	writeMu            sync.Mutex
	transportCloseOnce sync.Once

	nextID  atomic.Int64
	pendMu  sync.Mutex
	pending map[int64]pendingWireResponse

	// turnWG tracks in-flight awaitPromptResult goroutines. closeEvents
	// waits on it before actually closing the events channel — without
	// this, a response delivered right as the reader hits EOF can race
	// onReaderClosed's own closeEvents call: deliverResponse unblocks
	// awaitPromptResult (a separate goroutine, scheduled independently)
	// while the reader goroutine continues straight on to EOF and closes
	// events before awaitPromptResult gets scheduled to push its
	// turn.completed/turn.failed event, silently dropping it. See
	// client_test.go's TestClientTCP_LaunchPromptEvents, which caught
	// this race during implementation.
	turnWG sync.WaitGroup

	events       chan runtimeevents.Event
	eventsMu     sync.Mutex // serializes pushEvent's send against closeEvents' close — see pushEvent's doc comment
	eventsClosed bool
	eventsOnce   sync.Once
	closeOnce    sync.Once
	readerDone   chan struct{}
	waitDone     chan struct{}
	waitErr      error
	termination  *acp.TransportTermination
	terminated   chan struct{}
	closeErr     error
}

type pendingWireResponse struct {
	response chan wireFrame
	prompt   bool
}

// NewClient returns a [Client] for the given transport. Configure via
// opts before calling [Client.Launch].
func NewClient(transport adapters.Transport, opts ...Option) *Client {
	c := &Client{
		transport:     transport,
		host:          "127.0.0.1",
		clientName:    defaultClientName,
		clientVersion: defaultClientVersion,
		pending:       make(map[int64]pendingWireResponse),
		events:        make(chan runtimeevents.Event, 64),
		readerDone:    make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Port returns the TCP port this Client is configured to use (whether
// explicitly set via [WithPort]/[WithDialOnly], or auto-picked by
// [Client.Launch] when left at zero). Zero before Launch has run for an
// auto-picked Client. Meaningless for stdio transport.
func (c *Client) Port() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.port
}

// Launch implements [acp.Client]. It starts (stdio, or TCP unless
// [WithDialOnly]) or connects to (TCP + [WithDialOnly]) the underlying
// `copilot --acp` process, then performs the real `initialize` →
// `session/new` handshake — both genuinely round-tripped over the wire,
// not simulated. Returns once the session is ready to accept a Prompt.
func (c *Client) Launch(ctx context.Context, params acp.LaunchParams) error {
	c.mu.Lock()
	if c.launched {
		c.mu.Unlock()
		return ErrAlreadyLaunched
	}
	c.launched = true
	c.diagnostic = params.OnDiagnostic
	if params.BestEffortPermissionRequestResponder != nil {
		c.permissions = acp.NewBestEffortPermissionRequests(params.BestEffortPermissionRequestResponder)
		c.permissions.SetResponseGate(&c.promptCloseMu)
	}
	c.mu.Unlock()

	var reader io.Reader
	var err error
	switch c.transport {
	case adapters.TransportStdio:
		reader, err = c.startStdio(params)
	case adapters.TransportTCP:
		reader, err = c.startTCP(ctx, params)
	default:
		err = fmt.Errorf("copilotacp: unsupported transport %q", c.transport)
	}
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.started = true
	cmd := c.cmd
	c.termination = acp.NewTransportTermination(cmd != nil)
	c.terminated = make(chan struct{})
	if cmd != nil {
		c.waitDone = make(chan struct{})
	}
	c.mu.Unlock()
	go c.readLoop(reader)
	if cmd != nil {
		c.pushEvent(runtimeevents.Event{Kind: runtimeevents.KindProcessStarted, Payload: marshalPayload(map[string]any{"pid": cmd.Process.Pid})})
		go c.waitProcess()
	}
	go c.coordinateTermination()

	initParams := initializeParams{
		ProtocolVersion: acpProtocolVersion,
		ClientCapabilities: clientCapabilities{
			FS:       fsCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
		ClientInfo: clientInfo{Name: c.clientName, Version: c.clientVersion},
	}
	initializeResult, err := c.call(ctx, "initialize", initParams)
	if err != nil {
		_ = c.Close(ctx)
		return fmt.Errorf("copilotacp: initialize: %w", err)
	}
	initialize, err := acp.ParseInitializeResult(initializeResult, acpProtocolVersion)
	if err != nil {
		_ = c.Close(ctx)
		return fmt.Errorf("copilotacp: initialize negotiation: %w", err)
	}
	if err := c.authenticate(ctx, initialize, params.AuthMethodID); err != nil {
		_ = c.Close(ctx)
		return fmt.Errorf("copilotacp: authenticate: %w", err)
	}
	c.mu.Lock()
	c.sessionClose = initialize.SessionClose
	c.mu.Unlock()

	sessionID := ""
	if params.SessionIDPreset != "" && initialize.LoadSession {
		if _, loadErr := c.call(ctx, "session/load", map[string]any{
			"sessionId":  params.SessionIDPreset,
			"cwd":        params.Cwd,
			"mcpServers": []any{},
		}); loadErr != nil {
			_ = c.Close(ctx)
			return fmt.Errorf("copilotacp: session/load: %w", loadErr)
		}
		sessionID = params.SessionIDPreset
	}
	if sessionID == "" {
		newParams := sessionNewParams{Cwd: params.Cwd, MCPServers: []any{}}
		result, newErr := c.call(ctx, "session/new", newParams)
		if newErr != nil {
			_ = c.Close(ctx)
			return fmt.Errorf("copilotacp: session/new: %w", newErr)
		}
		var sr sessionNewResult
		if decodeErr := json.Unmarshal(result, &sr); decodeErr != nil || sr.SessionID == "" {
			_ = c.Close(ctx)
			return fmt.Errorf("copilotacp: session/new: response missing sessionId")
		}
		sessionID = sr.SessionID
	}

	c.mu.Lock()
	c.sessionID = sessionID
	// LaunchParams.SystemPrompt: real ACP's initialize/session/new
	// params (confirmed live) carry no client-supplied system-prompt
	// field. Rather than silently dropping it (which [acp.LaunchParams]'
	// own doc permits for fields that don't apply), thread it into the
	// FIRST Prompt call as a preamble — a reasonable, documented
	// interpretation of "the agent's initial system-level instruction"
	// given no dedicated handshake field exists.
	c.systemPrompt = params.SystemPrompt
	c.mu.Unlock()
	c.permissions.SetSessionID(sessionID)
	if err := c.configureSession(ctx, params); err != nil {
		_ = c.Close(ctx)
		return fmt.Errorf("copilotacp: configure session: %w", err)
	}

	c.pushEvent(runtimeevents.Event{Kind: runtimeevents.KindSessionReady})
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

func (c *Client) startStdio(params acp.LaunchParams) (io.Reader, error) {
	binary := c.binary
	if binary == "" {
		binary = "copilot"
	}
	args := append([]string{"--acp"}, c.extraArgs...)

	cmd := exec.Command(binary, args...) //nolint:gosec // G204: binary/args are caller-configured, mirroring every other adapter in this repo.
	cmd.Dir = params.Cwd
	if len(params.Env) > 0 {
		cmd.Env = params.Env
	} else {
		cmd.Env = os.Environ()
	}
	if c.stderr != nil {
		cmd.Stderr = c.stderr
	}
	var stderrPipe io.ReadCloser
	if c.stderr == nil && c.diagnostic != nil {
		var pipeErr error
		stderrPipe, pipeErr = cmd.StderrPipe()
		if pipeErr != nil {
			return nil, fmt.Errorf("copilotacp: stderr pipe: %w", pipeErr)
		}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("copilotacp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("copilotacp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("copilotacp: start %s --acp: %w", binary, err)
	}
	if stderrPipe != nil {
		go c.drainStderr(stderrPipe)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.writer = stdin
	c.mu.Unlock()

	return stdout, nil
}

func (c *Client) startTCP(ctx context.Context, params acp.LaunchParams) (io.Reader, error) {
	c.mu.Lock()
	dialOnly := c.dialOnly
	port := c.port
	host := c.host
	c.mu.Unlock()

	var cmd *exec.Cmd
	if !dialOnly {
		if port == 0 {
			p, err := freeTCPPort()
			if err != nil {
				return nil, fmt.Errorf("copilotacp: pick free port: %w", err)
			}
			port = p
		}

		binary := c.binary
		if binary == "" {
			binary = "copilot"
		}
		args := append([]string{"--acp", "--port", strconv.Itoa(port)}, c.extraArgs...)

		cmd = exec.Command(binary, args...) //nolint:gosec // G204: see startStdio.
		cmd.Dir = params.Cwd
		if len(params.Env) > 0 {
			cmd.Env = params.Env
		} else {
			cmd.Env = os.Environ()
		}
		if c.stderr != nil {
			cmd.Stderr = c.stderr
		}
		var stderrPipe io.ReadCloser
		if c.stderr == nil && c.diagnostic != nil {
			var pipeErr error
			stderrPipe, pipeErr = cmd.StderrPipe()
			if pipeErr != nil {
				return nil, fmt.Errorf("copilotacp: stderr pipe: %w", pipeErr)
			}
		}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("copilotacp: start %s --acp --port %d: %w", binary, port, err)
		}
		if stderrPipe != nil {
			go c.drainStderr(stderrPipe)
		}
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := dialWithRetry(ctx, addr, 10*time.Second)
	if err != nil {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return nil, fmt.Errorf("copilotacp: connect to %s: %w", addr, err)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.conn = conn
	c.writer = conn
	c.port = port
	c.mu.Unlock()

	return conn, nil
}

// dialWithRetry retries net.Dial against addr until it succeeds, ctx is
// done, or timeout elapses. Copilot's TCP `--acp` server takes a moment
// to bind after the process starts (confirmed live: immediate connects
// fail with "connection refused" for roughly the first second).
func dialWithRetry(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out after %s", timeout)
	}
	return nil, lastErr
}

func freeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		_ = l.Close()
		return 0, fmt.Errorf("copilotacp: unexpected listener addr type %T", l.Addr())
	}
	port := addr.Port
	if err := l.Close(); err != nil {
		return 0, fmt.Errorf("copilotacp: release reserved TCP port: %w", err)
	}
	return port, nil
}

// writeFrame serializes f as one NDJSON line and writes it to the
// active transport, serialized against concurrent writers.
func (c *Client) writeFrame(f wireFrame) error {
	f.JSONRPC = "2.0"
	return c.writeJSONFrame(f)
}

func (c *Client) writeServerResponse(id json.RawMessage, result json.RawMessage, rpcErr *wireError) error {
	return c.writeJSONFrameWithDeadline(serverResponseFrame{
		JSONRPC: "2.0",
		ID:      append(json.RawMessage(nil), id...),
		Result:  result,
		Error:   rpcErr,
	}, time.Now().Add(250*time.Millisecond))
}

func (c *Client) writeJSONFrame(frame any) error {
	return c.writeJSONFrameWithDeadline(frame, time.Time{})
}

type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

func (c *Client) writeJSONFrameWithDeadline(frame any, deadline time.Time) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	c.mu.Lock()
	w := c.writer
	c.mu.Unlock()
	if w == nil {
		return ErrNotLaunched
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !deadline.IsZero() {
		if writer, ok := w.(writeDeadliner); ok {
			if deadlineErr := writer.SetWriteDeadline(deadline); deadlineErr == nil {
				defer func() { _ = writer.SetWriteDeadline(time.Time{}) }()
			}
		}
	}
	_, err = w.Write(data)
	return err
}

// call sends a JSON-RPC request and blocks for the correlated response
// (or ctx.Done()). Mirrors agentkit's own jsonRpcStdioSession.Call
// shape, reimplemented here because this Client owns its own
// transport/pipes rather than agentkit's.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	id := c.nextID.Add(1)
	ch := make(chan wireFrame, 1)
	c.pendMu.Lock()
	c.pending[id] = pendingWireResponse{response: ch, prompt: method == "session/prompt"}
	c.pendMu.Unlock()
	cleanup := func() {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
	}

	idCopy := id
	if err := c.writeFrame(wireFrame{ID: &idCopy, Method: method, Params: raw}); err != nil {
		cleanup()
		return nil, err
	}

	select {
	case f, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("copilotacp: connection closed waiting for %s response", method)
		}
		if f.Error != nil {
			return nil, f.Error
		}
		return f.Result, nil
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	}
}

// notify sends a JSON-RPC notification (no id, no response expected).
func (c *Client) notify(ctx context.Context, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.writeJSONFrameWithDeadline(
			wireFrame{JSONRPC: "2.0", Method: method, Params: raw},
			time.Now().Add(250*time.Millisecond),
		)
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
func (c *Client) abortPermissionTransport() {
	c.mu.Lock()
	cmd := c.cmd
	c.mu.Unlock()
	c.closeTransport()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// Prompt implements [acp.Client]. It sends `session/prompt` and returns
// once the request has been written to the wire — NOT once the turn
// completes, per [acp.Client.Prompt]'s doc. The eventual
// turn.completed/turn.failed event (carrying the real `stopReason`) is
// pushed to [Client.Events] asynchronously once Copilot's response
// arrives.
func (c *Client) Prompt(ctx context.Context, prompt string) error {
	c.promptCloseMu.Lock()
	defer c.promptCloseMu.Unlock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("copilotacp: client is closed")
	}
	if c.sessionID == "" {
		c.mu.Unlock()
		return ErrNotLaunched
	}
	if c.turnInFlight {
		c.mu.Unlock()
		return ErrTurnInFlight
	}
	sessionID := c.sessionID
	sysPrompt := c.systemPrompt
	c.systemPrompt = "" // prepend at most once, on the first turn
	turnID := runtimeevents.NewTurnID()
	c.turnInFlight = true
	c.currentTurnID = turnID
	c.permissions.BeginTurn()
	// Admit the turn under the same lifecycle mutex Close uses so its
	// closeEvents Wait cannot observe zero before this Add.
	c.turnWG.Add(1)
	c.mu.Unlock()

	text := prompt
	if sysPrompt != "" {
		text = sysPrompt + "\n\n" + prompt
	}

	c.pushEvent(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: turnID})

	params := sessionPromptParams{
		SessionID: sessionID,
		Prompt:    []promptContentBlock{{Type: "text", Text: text}},
	}
	raw, err := json.Marshal(params)
	if err != nil {
		c.failTurn(turnID, err)
		c.turnWG.Done()
		c.endTurn()
		return err
	}

	id := c.nextID.Add(1)
	ch := make(chan wireFrame, 1)
	c.pendMu.Lock()
	c.pending[id] = pendingWireResponse{response: ch, prompt: true}
	c.pendMu.Unlock()

	// turnWG.Add was performed under c.mu before request preparation. That
	// orders admission before either a fast response or concurrent Close can
	// reach closeEvents/turnWG.Wait.
	idCopy := id
	if err := c.writeFrame(wireFrame{ID: &idCopy, Method: "session/prompt", Params: raw}); err != nil {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		wrapped := fmt.Errorf("copilotacp: session/prompt: %w", err)
		c.failTurn(turnID, wrapped)
		c.turnWG.Done()
		c.endTurn()
		return wrapped
	}

	go c.awaitPromptResult(turnID, ch)
	return nil
}

// failTurn pushes a turn.failed event for a turn that never made it far
// enough to get a real JSON-RPC response — e.g. params marshaling or the
// session/prompt write itself failed. Keeps every KindTurnStarted this
// Client pushes matched by exactly one terminal event, the same
// invariant awaitPromptResult maintains for the normal path.
func (c *Client) failTurn(turnID string, err error) {
	payload, _ := json.Marshal(map[string]any{"error": err.Error()})
	c.pushEvent(runtimeevents.Event{Kind: runtimeevents.KindTurnFailed, TurnID: turnID, Payload: payload})
}

func (c *Client) awaitPromptResult(turnID string, ch chan wireFrame) {
	defer c.turnWG.Done()
	f, ok := <-ch
	var event runtimeevents.Event
	if !ok {
		payload, _ := json.Marshal(map[string]any{"error": "connection closed before session/prompt response"})
		event = runtimeevents.Event{Kind: runtimeevents.KindTurnFailed, TurnID: turnID, Payload: payload}
	} else if f.Error != nil {
		payload, _ := json.Marshal(map[string]any{"error": f.Error.Message})
		event = runtimeevents.Event{Kind: runtimeevents.KindTurnFailed, TurnID: turnID, Payload: payload}
	} else {
		var res sessionPromptResult
		_ = json.Unmarshal(f.Result, &res)
		payload, _ := json.Marshal(map[string]any{"stop_reason": res.StopReason})
		event = runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: turnID, Payload: payload}
	}
	c.endTurn()
	c.pushEvent(event)
}

func (c *Client) endTurn() {
	c.permissions.EndTurn()
	c.mu.Lock()
	c.turnInFlight = false
	c.currentTurnID = ""
	c.mu.Unlock()
}

// Cancel implements [acp.Client]. It sends ACP's `session/cancel`
// notification for the in-flight turn — confirmed live to genuinely
// interrupt Copilot mid-generation (see package doc), not just
// acknowledge cancellation while the turn runs to completion regardless.
// No-op (returns nil) when no turn is currently in flight.
func (c *Client) Cancel(ctx context.Context) error {
	c.promptCloseMu.Lock()
	defer c.promptCloseMu.Unlock()

	c.mu.Lock()
	sessionID := c.sessionID
	inFlight := c.turnInFlight
	c.mu.Unlock()
	if sessionID == "" || !inFlight {
		return nil
	}
	c.permissions.CancelTurn()
	return c.notify(ctx, "session/cancel", sessionCancelParams{SessionID: sessionID})
}

// Events implements [acp.Client].
func (c *Client) Events() <-chan runtimeevents.Event { return c.events }

// ProviderSessionID returns the id established by session/new or session/load.
func (c *Client) ProviderSessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// InterruptCapability implements [acp.Client]. Returns
// [adapters.InterruptTurn] — verified directly (see package doc): a
// real in-flight generation was cut off within ~3 seconds of Cancel
// rather than running to completion.
func (c *Client) InterruptCapability() adapters.InterruptCapability {
	return adapters.InterruptTurn
}

// Close implements [acp.Client]. Idempotent and safe to call even if
// Launch was never called or failed. Closes the transport (stdin for
// stdio; the socket for TCP), waits briefly for the spawned process (if
// this Client spawned one — see [WithDialOnly]) to exit on its own, and
// escalates to Kill if it doesn't.
func (c *Client) Close(ctx context.Context) error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		cmd := c.cmd
		started := c.started
		waitDone := c.waitDone
		terminated := c.terminated
		sessionID := c.sessionID
		sessionClose := c.sessionClose
		permissions := c.permissions
		c.mu.Unlock()
		if permissions != nil {
			permissions.Close()
		}
		graceful := closegate.TryLockWithin(ctx, &c.promptCloseMu, closegate.PromptDrainGrace)
		if !graceful {
			// Prompt owns the admission gate across its synchronous transport
			// write. Close the transport first so a backpressured write cannot
			// prevent lifecycle teardown from reaching its deadline.
			c.closeTransport()
		}
		if graceful && sessionClose && sessionID != "" {
			closeCtx, cancel := context.WithTimeout(ctx, time.Second)
			closeResult := make(chan error, 1)
			go func() {
				_, err := c.call(closeCtx, "session/close", map[string]any{"sessionId": sessionID})
				closeResult <- err
			}()
			var closeErr error
			select {
			case closeErr = <-closeResult:
			case <-closeCtx.Done():
				closeErr = closeCtx.Err()
			}
			cancel()
			c.mu.Lock()
			c.closeErr = closeErr
			c.mu.Unlock()
		}
		if graceful {
			c.promptCloseMu.Unlock()
		}

		c.closeTransport()

		if cmd != nil && cmd.Process != nil {
			waited := false
			select {
			case <-waitDone:
				waited = true
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
			case <-ctx.Done():
				_ = cmd.Process.Kill()
			}
			if !waited {
				select {
				case <-waitDone:
				case <-time.After(3 * time.Second):
				case <-ctx.Done():
				}
			}
		}

		if started {
			select {
			case <-c.readerDone:
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
			}
			if terminated != nil {
				select {
				case <-terminated:
				case <-ctx.Done():
				}
			}
		} else {
			c.closeEvents()
		}
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
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
	terminated := c.terminated
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
	if result.ShouldEmitProcessExit() {
		payload := map[string]any{"exit_code": result.Process.ExitCode}
		if result.Process.Err != nil {
			payload["error"] = result.Process.Err.Error()
		}
		c.pushEvent(runtimeevents.Event{Kind: runtimeevents.KindProcessExited, Payload: marshalPayload(payload)})
	}
	c.closeEvents()
	close(terminated)
}

// closeTransport interrupts transport writes exactly once. It intentionally
// does not take promptCloseMu or writeMu: either may be owned by the blocked
// write this method exists to preempt.
func (c *Client) closeTransport() {
	c.transportCloseOnce.Do(func() {
		c.mu.Lock()
		stdin := c.stdin
		conn := c.conn
		c.mu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}
		if conn != nil {
			_ = conn.Close()
		}
	})
}

// closeEvents closes the events channel exactly once. It first waits
// for turnWG — any awaitPromptResult goroutine already unblocked (either
// by a real response, or by onReaderClosed closing its pending-response
// channel just before calling closeEvents) is guaranteed to still be
// able to push its terminal turn.completed/turn.failed event before the
// channel actually closes. Then takes eventsMu so the close itself can
// never race a concurrent pushEvent send — see pushEvent's doc comment
// for why that matters (sending on a closed channel panics;
// select-with-default does NOT protect against that, only against a
// full buffer).
func (c *Client) closeEvents() {
	c.eventsOnce.Do(func() {
		c.turnWG.Wait()
		c.eventsMu.Lock()
		defer c.eventsMu.Unlock()
		c.eventsClosed = true
		close(c.events)
	})
}

// pushEvent sends ev on the events channel without blocking — mirroring
// agentkit's own EventFanout contract ("Sends are non-blocking; when
// the channel is full the event is dropped silently"). Guarded by
// eventsMu against closeEvents: pushEvent can be called concurrently
// from the reader goroutine (handleNotification), an awaitPromptResult
// goroutine, and Launch/Prompt's own callers, any of which may still be
// in flight when Close (hence onReaderClosed/closeEvents) runs —
// without this guard, a send racing a close would panic.
func (c *Client) pushEvent(ev runtimeevents.Event) {
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()
	if c.eventsClosed {
		return
	}
	select {
	case c.events <- ev:
	default:
	}
}

func (c *Client) readLoop(r io.Reader) {
	defer close(c.readerDone)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	malformed := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		if c.handleLine(cp) {
			malformed = true
		}
	}
	if err := scanner.Err(); err != nil {
		c.reportReadError("reading ACP protocol stream", err)
	}
	c.permissions.Close()
	c.onReaderClosed()
	c.mu.Lock()
	termination := c.termination
	c.mu.Unlock()
	termination.ReportRead(acp.TransportReadResult{Malformed: malformed, Err: scanner.Err()})
}

func (c *Client) handleLine(line []byte) bool {
	var f incomingWireFrame
	if err := json.Unmarshal(line, &f); err != nil {
		c.reportDiagnostic(acp.NewDiagnostic(acp.DiagnosticMalformedJSON, "invalid JSON-RPC frame", string(line)))
		return true
	}
	switch {
	case f.Method != "" && len(f.ID) != 0:
		// Server-initiated request (fs/*, terminal/*,
		// session/request_permission, ...). Decline rather than hang —
		// see package doc's "no fs/terminal proxying" note.
		requestID, ok := acp.DecodeJSONRPCRequestID(f.ID)
		if !ok {
			c.reportDiagnostic(acp.NewDiagnostic(acp.DiagnosticProtocol, "invalid JSON-RPC request id", ""))
			return true
		}
		if f.Method == "session/request_permission" && c.permissions != nil {
			c.handlePermissionRequest(f.ID, requestID, f.Params)
		} else {
			c.respondUnsupported(f.ID, f.Method)
		}
	case f.Method != "":
		c.handleNotification(f.Method, f.Params)
	case len(f.ID) != 0:
		var id int64
		if err := json.Unmarshal(f.ID, &id); err != nil {
			c.reportDiagnostic(acp.NewDiagnostic(acp.DiagnosticProtocol, "unexpected non-numeric JSON-RPC response id", ""))
			return true
		}
		idCopy := id
		c.deliverResponse(id, wireFrame{
			JSONRPC: f.JSONRPC,
			ID:      &idCopy,
			Result:  f.Result,
			Error:   f.Error,
		})
	}
	return false
}

func (c *Client) deliverResponse(id int64, f wireFrame) {
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
	pending.response <- f
}

func (c *Client) respondUnsupported(id json.RawMessage, method string) {
	_ = c.writeServerResponse(id, nil, &wireError{
		Code:    -32601,
		Message: "copilotacp: no handler for server-initiated method " + method,
	})
}

func (c *Client) handlePermissionRequest(id json.RawMessage, eventID any, params json.RawMessage) {
	id = append(json.RawMessage(nil), id...)
	var turnID string
	c.permissions.DispatchTurnRequest(params, func(admission acp.PermissionDispatchAdmission) {
		if !admission.ActiveTurn {
			return
		}
		c.mu.Lock()
		turnID = c.currentTurnID
		c.mu.Unlock()
		c.pushEvent(runtimeevents.Event{
			Kind:   runtimeevents.KindAgentPermissionRequested,
			TurnID: turnID,
			Payload: marshalPayload(map[string]any{
				"request_id": eventID,
				"method":     "session/request_permission",
				"params":     params,
			}),
		})
	}, func(admission acp.PermissionDispatchAdmission, resolution acp.PermissionResolution) error {
		err := c.writeServerResponse(id, marshalPayload(resolution.Result()), nil)
		if !admission.ActiveTurn {
			return err
		}
		payload := resolution.ResolvedEventPayload(eventID)
		if err != nil {
			payload = resolution.DeliveryFailureEventPayload(eventID)
		}
		c.pushEvent(runtimeevents.Event{
			Kind:    runtimeevents.KindAgentPermissionResolved,
			TurnID:  turnID,
			Payload: marshalPayload(payload),
		})
		return err
	}, func(resolution acp.PermissionResolution) {
		if resolution.ResponseError() != nil {
			c.abortPermissionTransport()
		}
		if diagnostic, ok := resolution.Diagnostic(); ok {
			c.reportDiagnostic(diagnostic)
		}
	})
}

func (c *Client) handleNotification(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var su sessionUpdateParams
	if err := json.Unmarshal(params, &su); err != nil {
		return
	}
	upd, ok := parseSessionUpdate(su.Update)
	if !ok {
		return
	}
	c.mu.Lock()
	turnID := c.currentTurnID
	c.mu.Unlock()
	ev, ok := acpUpdateToRuntimeEvent(upd, turnID)
	if !ok {
		return
	}
	c.pushEvent(ev)
}

// onReaderClosed fails any still-pending Call goroutines. The shared
// transport coordinator, not this producer, closes Events after joining the
// reader and process outcomes.
func (c *Client) onReaderClosed() {
	c.pendMu.Lock()
	pending := c.pending
	c.pending = make(map[int64]pendingWireResponse)
	c.pendMu.Unlock()
	for _, call := range pending {
		close(call.response)
	}
}

func (c *Client) drainStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		c.reportDiagnostic(acp.NewDiagnostic(acp.DiagnosticStderr, "ACP child stderr", scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		c.reportReadError("reading ACP child stderr", err)
	}
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

func marshalPayload(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

var _ acp.Client = (*Client)(nil)
