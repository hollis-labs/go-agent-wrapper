package codexacp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// acpProtocolVersion is the integer protocolVersion sent in `initialize`.
// Confirmed directly against codex-acp 1.6.2: sending the integer 1
// round-trips cleanly — the live response echoes back
// `"protocolVersion":1`, the same convention opencodeacp's own live
// verification found for OpenCode's bridge.
const acpProtocolVersion = 1

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
	bridgeVersion string // default defaultBridgeVersion; "" (via WithClientBridgePackageSpec) means unpinned "@latest"-equivalent bare package name
	bridgePkgSpec string // when set (via WithClientBridgePackageSpec), overrides the whole "package[@version]" token
	extraArgs     []string
	codexBinary   string // explicit CODEX_PATH override; "" resolves only from the launch environment below

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
// constructors don't collide in the package's exported name space —
// same convention [adapters/opencodeacp] already uses.
type ClientOption func(*Client)

// WithClientBinary overrides the executable [Client.Launch] spawns to
// run the bridge — "npx" by default. Set this (combined with
// [WithClientBridgePackageSpec]) to run a globally-installed `codex-acp`
// binary directly instead of going through npx.
func WithClientBinary(path string) ClientOption { return func(c *Client) { c.bridgeBinary = path } }

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
	c := &Client{
		bridgeVersion: defaultBridgeVersion,
		events:        make(chan runtimeevents.Event, 64),
		pending:       make(map[int64]pendingRPCResponse),
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
	return fmt.Sprintf("acp: jsonrpc error %d: %s", e.Code, e.Message)
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

// resolveBridgeCommand returns the binary and args [Client.Launch]
// spawns. Default: `npx -y @agentclientprotocol/codex-acp@<pinned
// version>`. See [WithClientBinary]/[WithClientBridgeVersion]/
// [WithClientBridgePackageSpec]/[WithClientExtraArgs].
func (c *Client) resolveBridgeCommand() (string, []string) {
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
// paramsEnv (or the current process environment, when paramsEnv is empty) plus
// an explicit CODEX_PATH entry resolved only from that resulting environment.
// When it already sets CODEX_PATH, the caller's choice is respected unmodified.
func (c *Client) buildEnv(paramsEnv []string) []string {
	return c.buildEnvForOS(paramsEnv, runtime.GOOS)
}

func (c *Client) buildEnvForOS(paramsEnv []string, goos string) []string {
	env := append([]string(nil), paramsEnv...)
	if len(env) == 0 {
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

// Launch implements [acp.Client]. It spawns the codex-acp bridge (see
// [Client.resolveBridgeCommand]), performs the `initialize` +
// `session/new` (or, when params.SessionIDPreset is set and the capability
// is advertised, a fail-closed `session/load`) handshake,
// and returns once the session is ready to accept a Prompt.
func (c *Client) Launch(ctx context.Context, params acp.LaunchParams) error {
	c.mu.Lock()
	if c.launched {
		c.mu.Unlock()
		return errors.New("codexacp: Launch called more than once")
	}
	c.launched = true
	c.mu.Unlock()

	binary, args := c.resolveBridgeCommand()
	cmd := exec.Command(binary, args...) //nolint:gosec // G204: operator-controlled binary/args
	if params.Cwd != "" {
		cmd.Dir = params.Cwd
	}
	cmd.Env = c.buildEnv(params.Env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("codexacp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("codexacp: stdout pipe: %w", err)
	}
	// Drain stderr into the log rather than leaving it unread (the
	// bridge writes diagnostic/error lines here — an unread pipe can
	// block the child once its OS buffer fills).
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("codexacp: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("codexacp: start %q: %w", binary, err)
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
		return fmt.Errorf("codexacp: initialize: %w", err)
	}
	initialize, err := acp.ParseInitializeResult(initializeResult, acpProtocolVersion)
	if err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("codexacp: initialize negotiation: %w", err)
	}
	if err := c.authenticate(ctx, initialize, params.AuthMethodID); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("codexacp: authenticate: %w", err)
	}
	c.mu.Lock()
	c.sessionClose = initialize.SessionClose
	c.mu.Unlock()

	if params.SessionIDPreset != "" && initialize.LoadSession {
		if err := c.loadSession(ctx, params); err != nil {
			_ = c.Close(context.Background())
			return fmt.Errorf("codexacp: session/load: %w", err)
		}
		c.mu.Lock()
		c.sessionID = params.SessionIDPreset
		c.mu.Unlock()
	} else if err := c.newSession(ctx, params); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("codexacp: session/new: %w", err)
	}
	c.permissions.SetSessionID(c.ProviderSessionID())
	if err := c.configureSession(ctx, params); err != nil {
		_ = c.Close(context.Background())
		return fmt.Errorf("codexacp: configure session: %w", err)
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
// completes. Turn progress (message/thought chunks, tool calls) and
// completion (turn.completed / turn.failed, carrying the ACP
// `stopReason`) surface asynchronously via [Client.Events].
func (c *Client) Prompt(ctx context.Context, prompt string) error {
	c.promptCloseMu.Lock()
	defer c.promptCloseMu.Unlock()

	c.turnMu.Lock()
	if c.currentTurnID != "" {
		c.turnMu.Unlock()
		return errors.New("codexacp: a turn is already in flight")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.turnMu.Unlock()
		return errors.New("codexacp: client is closed")
	}
	sessionID := c.sessionID
	if sessionID == "" {
		c.mu.Unlock()
		c.turnMu.Unlock()
		return errors.New("codexacp: Prompt called before Launch established a session")
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

	// Fire-and-forget from the caller's perspective — same contract
	// opencodeacp's own Prompt documents: the request is written
	// synchronously (so a write failure surfaces immediately), the
	// response is awaited on a background goroutine.
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
// notification for the in-flight turn. Confirmed live AND by reading the
// bridge's own source (see package doc) to reach a genuine native
// interrupt: the bridge's session/cancel handler calls
// `codexAcpClient.turnInterrupt(...)`, which sends a real `turn/interrupt`
// JSON-RPC request to the spawned `codex app-server` process. A no-op
// (returns nil) when no turn is active.
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
// [adapters.InterruptTurn] — verified both by reading codex-acp 1.6.2's
// own published source (its session/cancel handler genuinely calls the
// app-server's `turn/interrupt` JSON-RPC method) and live, against a
// real mid-generation cancel (see package doc for the measured
// cancel-to-response latency). This is a static, per-implementation
// fact, independent of whether [Client.Launch] has been called —
// matching [acp.DescriptorFor]'s expectation that Describe() can be
// answered without a live session.
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

	c.failPending(errors.New("codexacp: client closed"))
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
		// Diagnostic-only — the bridge's own stderr carries npx/node
		// diagnostics and the bridge's own log lines (unless
		// APP_SERVER_LOGS redirects them to a file), not protocol
		// frames. Surface them only through the opt-in redacted diagnostic
		// callback, never as model activity.
		c.reportDiagnostic(acp.NewDiagnostic(acp.DiagnosticStderr, "ACP child stderr", scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		c.reportReadError("reading ACP child stderr", err)
	}
}

// readLoop scans stdout line by line, classifying each newline-delimited
// JSON-RPC frame into response / notification / server-initiated
// request — the same classification opencodeacp's own reader and
// agentkit's jsonrpc-stdio reader use.
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
	c.failPending(errors.New("codexacp: protocol stream closed before response"))
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
		return fmt.Errorf("codexacp: encode: %w", err)
	}
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("codexacp: not launched (no stdin)")
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
// response for every request that carries an id — without one, the
// bridge blocks waiting for it.
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
		// Channel full — drop rather than block the sender (which may
		// be the reader loop itself) indefinitely. Same 64-slot buffer
		// convention opencodeacp uses.
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
