package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// ClientAdapter is implemented by an ACP-backed wrapper adapter that can
// construct the real protocol client for one session. Wrapper uses this
// capability instead of the capture-only provider.CLIAdapter shape.
type ClientAdapter interface {
	ACPClient() Client
}

// State is the authoritative lifecycle state of a managed ACP session.
type State string

const (
	StateLaunching  State = "launching"
	StateReady      State = "ready"
	StateProcessing State = "processing"
	StateClosing    State = "closing"
	StateClosed     State = "closed"
	StateFailed     State = "failed"
)

// OutcomeKind classifies a managed session's terminal outcome without making
// callers parse provider-specific error strings.
type OutcomeKind string

const (
	OutcomeClosed          OutcomeKind = "closed"
	OutcomeDisconnected    OutcomeKind = "disconnected"
	OutcomeChildExit       OutcomeKind = "child_exit"
	OutcomeMalformedStream OutcomeKind = "malformed_stream"
	OutcomeCanceled        OutcomeKind = "canceled"
)

var (
	ErrNotLive          = errors.New("acp: session is not live")
	ErrDuplicateSession = errors.New("acp: session id is already registered")
	ErrDisconnected     = errors.New("acp: transport disconnected")
	ErrChildExit        = errors.New("acp: child process exited")
	ErrMalformedStream  = errors.New("acp: malformed protocol stream")
	ErrCanceled         = errors.New("acp: operation canceled")
)

// LifecycleError is the normalized error returned for an abnormal managed
// session termination. Cause is deliberately not serialized; Error contains
// only the already-redacted diagnostic summary safe to surface to a host.
type LifecycleError struct {
	Kind       OutcomeKind
	Operation  string
	Diagnostic string
	Cause      error
}

func (e *LifecycleError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := "acp: " + string(e.Kind)
	if e.Operation != "" {
		msg += " during " + e.Operation
	}
	if e.Diagnostic != "" {
		msg += ": " + e.Diagnostic
	}
	return msg
}

func (e *LifecycleError) Unwrap() error {
	if e == nil {
		return nil
	}
	switch e.Kind {
	case OutcomeDisconnected:
		return errors.Join(ErrDisconnected, e.Cause)
	case OutcomeChildExit:
		return errors.Join(ErrChildExit, e.Cause)
	case OutcomeMalformedStream:
		return errors.Join(ErrMalformedStream, e.Cause)
	case OutcomeCanceled:
		return errors.Join(ErrCanceled, e.Cause)
	default:
		return e.Cause
	}
}

// DiagnosticKind identifies protocol diagnostics available to hosts. Raw
// bytes are never exposed without passing through NewDiagnostic's redactor.
type DiagnosticKind string

const (
	DiagnosticStderr        DiagnosticKind = "stderr"
	DiagnosticMalformedJSON DiagnosticKind = "malformed_json"
	DiagnosticProtocol      DiagnosticKind = "protocol"
)

// Diagnostic is a bounded, redacted protocol observation. It is deliberately
// separate from runtimeevents so raw child output is opt-in rather than mixed
// into ordinary model output.
type Diagnostic struct {
	Kind    DiagnosticKind `json:"kind"`
	Message string         `json:"message"`
	Raw     string         `json:"raw,omitempty"`
}

const maxDiagnosticBytes = 4096

var (
	sensitiveHeader = regexp.MustCompile(`(?im)(authorization|proxy-authorization|cookie|set-cookie)(\s*:\s*)([^\r\n]+)`)
	sensitivePair   = regexp.MustCompile(`(?i)("?(?:authorization|proxy-authorization|cookie|set-cookie|api[_-]?key|token|password|secret)"?)(\s*[:=]\s*)(?:"[^"]*"|[^\s,;}]+)`)
)

// NewDiagnostic bounds and redacts a raw diagnostic before it can leave the
// ACP implementation. JSON object keys with secret-shaped names and common
// key=value/header spellings are replaced with "[REDACTED]".
func NewDiagnostic(kind DiagnosticKind, message, raw string) Diagnostic {
	return Diagnostic{Kind: kind, Message: redactDiagnostic(message), Raw: redactDiagnostic(raw)}
}

func redactDiagnostic(in string) string {
	if len(in) > maxDiagnosticBytes {
		in = in[:maxDiagnosticBytes] + "…"
	}
	var value any
	if json.Unmarshal([]byte(in), &value) == nil {
		redactJSON(value)
		if encoded, err := json.Marshal(value); err == nil {
			in = string(encoded)
		}
	}
	in = sensitiveHeader.ReplaceAllString(in, `$1$2[REDACTED]`)
	return sensitivePair.ReplaceAllString(in, `$1$2"[REDACTED]"`)
}

func redactJSON(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if isSensitiveKey(key) {
				v[key] = "[REDACTED]"
				continue
			}
			redactJSON(item)
		}
	case []any:
		for _, item := range v {
			redactJSON(item)
		}
	}
}

func isSensitiveKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	for _, marker := range []string{"authorization", "cookie", "api_key", "apikey", "token", "password", "secret"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

// Snapshot is an immutable view of a managed session's liveness and most
// recent terminal/turn outcome.
type Snapshot struct {
	ID                string
	ProviderSessionID string
	State             State
	Live              bool
	LastActivity      time.Time
	LastTurnOutcome   OutcomeKind
	TerminalOutcome   OutcomeKind
	Err               error
}

// SessionConfig describes one Manager-owned Client lifecycle.
type SessionConfig struct {
	ID     string
	Client Client
	Launch LaunchParams
}

// Session owns one Client from successful registration through exactly-once
// teardown. It is safe for concurrent Prompt, Cancel, Close, Snapshot and Wait
// calls; Client itself remains responsible for its wire-level serialization.
type Session struct {
	id     string
	client Client
	onDone func(string, *Session)

	mu                sync.RWMutex
	state             State
	providerSessionID string
	lastActivity      time.Time
	lastTurnOutcome   OutcomeKind
	terminalOutcome   OutcomeKind
	err               error
	closeRequested    bool
	sawProcessExit    bool
	sawMalformed      bool

	events               chan runtimeevents.Event
	diagnostics          chan Diagnostic
	done                 chan struct{}
	diagnosticMu         sync.Mutex
	diagnosticCallbackMu sync.Mutex
	diagnosticsClosed    bool
	diagnosticWG         sync.WaitGroup
	onDiagnostic         func(Diagnostic)
	closeOnce            sync.Once
	finishOnce           sync.Once
	closeErr             error
}

// ID returns the host-selected manager key.
func (s *Session) ID() string { return s.id }

// ProviderSessionID returns the provider-assigned ACP session id captured
// after new/load. It remains available after termination.
func (s *Session) ProviderSessionID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.providerSessionID
}

func (s *Session) Events() <-chan runtimeevents.Event { return s.events }
func (s *Session) Diagnostics() <-chan Diagnostic     { return s.diagnostics }

func (s *Session) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		ID: s.id, ProviderSessionID: s.providerSessionID, State: s.state,
		Live:         s.state == StateLaunching || s.state == StateReady || s.state == StateProcessing || s.state == StateClosing,
		LastActivity: s.lastActivity, LastTurnOutcome: s.lastTurnOutcome,
		TerminalOutcome: s.terminalOutcome, Err: s.err,
	}
}

// Prompt starts one turn. A prompt rejected after teardown returns ErrNotLive.
func (s *Session) Prompt(ctx context.Context, prompt string) error {
	s.mu.Lock()
	if s.state != StateReady {
		s.mu.Unlock()
		return ErrNotLive
	}
	s.state = StateProcessing
	s.lastActivity = time.Now().UTC()
	s.mu.Unlock()
	if err := s.client.Prompt(ctx, prompt); err != nil {
		s.mu.Lock()
		if s.state == StateProcessing {
			s.state = StateReady
		}
		s.mu.Unlock()
		return normalizeOperationError("prompt", err)
	}
	return nil
}

// Cancel requests cancellation of only the in-flight turn. The session stays
// registered and ready for another prompt once its terminal turn event arrives.
func (s *Session) Cancel(ctx context.Context) error {
	s.mu.RLock()
	live := s.state == StateProcessing
	s.mu.RUnlock()
	if !live {
		return nil
	}
	if err := s.client.Cancel(ctx); err != nil {
		return normalizeOperationError("cancel", err)
	}
	return nil
}

// Close ends the whole session. The underlying Client.Close is invoked at
// most once even when Close races transport EOF or Manager.Shutdown.
func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.state != StateClosed && s.state != StateFailed {
		s.state = StateClosing
	}
	s.closeRequested = true
	s.mu.Unlock()
	s.closeOnce.Do(func() {
		err := s.client.Close(ctx)
		s.mu.Lock()
		s.closeErr = err
		s.mu.Unlock()
	})
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closeErr
}

// Wait blocks for session termination and returns its normalized terminal
// error. Intentional Close is a nil error; abnormal outcomes use LifecycleError.
func (s *Session) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) drain() {
	for ev := range s.client.Events() {
		s.observeEvent(ev)
		select {
		case s.events <- ev:
		case <-s.done:
			return
		default:
			// Runtime activity is best-effort; lifecycle draining must never
			// stall teardown behind a slow host consumer.
		}
	}
	s.finish()
}

func (s *Session) observeEvent(ev runtimeevents.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActivity = time.Now().UTC()
	if identity, ok := s.client.(interface{ ProviderSessionID() string }); ok {
		if id := identity.ProviderSessionID(); id != "" {
			s.providerSessionID = id
		}
	}
	switch ev.Kind {
	case runtimeevents.KindSessionReady:
		if s.state == StateLaunching {
			s.state = StateReady
		}
	case runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
		if s.state != StateClosing {
			s.state = StateReady
		}
		if ev.Kind == runtimeevents.KindTurnFailed {
			s.lastTurnOutcome = classifyTurnFailure(ev.Payload)
		} else if ev.Kind == runtimeevents.KindTurnCompleted {
			s.lastTurnOutcome = classifyTurnCompletion(ev.Payload)
		}
	case runtimeevents.KindTurnStarted:
		if s.state != StateClosing {
			s.state = StateProcessing
		}
	case runtimeevents.KindProcessExited:
		s.sawProcessExit = true
		var payload struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(ev.Payload, &payload)
		if payload.Error != "" {
			s.err = &LifecycleError{Kind: OutcomeChildExit, Operation: "wait", Diagnostic: redactDiagnostic(payload.Error)}
			s.terminalOutcome = OutcomeChildExit
		}
	}
}

func classifyTurnCompletion(payload json.RawMessage) OutcomeKind {
	var body struct {
		StopReason string `json:"stop_reason"`
	}
	_ = json.Unmarshal(payload, &body)
	if strings.EqualFold(body.StopReason, "cancelled") || strings.EqualFold(body.StopReason, "canceled") {
		return OutcomeCanceled
	}
	return ""
}

func classifyTurnFailure(payload json.RawMessage) OutcomeKind {
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(payload, &body)
	if strings.Contains(strings.ToLower(body.Error), "cancel") {
		return OutcomeCanceled
	}
	return ""
}

func (s *Session) emitDiagnostic(d Diagnostic) {
	s.diagnosticMu.Lock()
	if s.diagnosticsClosed {
		s.diagnosticMu.Unlock()
		return
	}
	s.diagnosticWG.Add(1)
	s.diagnosticMu.Unlock()
	defer s.diagnosticWG.Done()

	if d.Kind == DiagnosticMalformedJSON {
		s.mu.Lock()
		s.sawMalformed = true
		s.mu.Unlock()
	}
	select {
	case s.diagnostics <- d:
	default:
	}
	if s.onDiagnostic != nil {
		s.diagnosticCallbackMu.Lock()
		s.onDiagnostic(d)
		s.diagnosticCallbackMu.Unlock()
	}
}

func (s *Session) finish() {
	s.finishOnce.Do(func() {
		// A transport that closes first still reaches the same exactly-once
		// cleanup path as an explicit Close.
		s.closeOnce.Do(func() {
			err := s.client.Close(context.Background())
			s.mu.Lock()
			s.closeErr = err
			s.mu.Unlock()
		})

		s.mu.Lock()
		switch {
		case s.closeRequested:
			s.state = StateClosed
			s.terminalOutcome = OutcomeClosed
		case s.sawMalformed:
			s.state = StateFailed
			s.terminalOutcome = OutcomeMalformedStream
			s.err = &LifecycleError{Kind: OutcomeMalformedStream, Operation: "read"}
		case s.err != nil:
			s.state = StateFailed
		case s.sawProcessExit:
			s.state = StateClosed
			s.terminalOutcome = OutcomeChildExit
		default:
			s.state = StateFailed
			s.terminalOutcome = OutcomeDisconnected
			s.err = &LifecycleError{Kind: OutcomeDisconnected, Operation: "read"}
		}
		s.mu.Unlock()

		if s.onDone != nil {
			s.onDone(s.id, s)
		}
		close(s.events)
		s.diagnosticMu.Lock()
		s.diagnosticsClosed = true
		s.diagnosticMu.Unlock()
		s.diagnosticWG.Wait()
		close(s.diagnostics)
		close(s.done)
	})
}

func normalizeOperationError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &LifecycleError{Kind: OutcomeCanceled, Operation: operation, Cause: err}
	}
	return fmt.Errorf("acp: %s: %w", operation, err)
}

// Manager is the authoritative in-memory owner for ACP session registration,
// lookup, cancellation, close, automatic unregister and shutdown.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewManager() *Manager { return &Manager{sessions: make(map[string]*Session)} }

// Launch reserves cfg.ID before starting the client, preventing concurrent
// duplicate starts. Any failed launch is closed and unregistered before return.
func (m *Manager) Launch(ctx context.Context, cfg SessionConfig) (*Session, error) {
	if m == nil {
		return nil, errors.New("acp: Manager is nil")
	}
	if cfg.ID == "" {
		return nil, errors.New("acp: SessionConfig.ID is required")
	}
	if cfg.Client == nil {
		return nil, errors.New("acp: SessionConfig.Client is required")
	}
	s := &Session{
		id: cfg.ID, client: cfg.Client, state: StateLaunching,
		lastActivity: time.Now().UTC(), events: make(chan runtimeevents.Event, 128),
		diagnostics: make(chan Diagnostic, 32), done: make(chan struct{}),
		onDiagnostic: cfg.Launch.OnDiagnostic,
	}
	s.onDone = m.unregister
	m.mu.Lock()
	if _, exists := m.sessions[cfg.ID]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrDuplicateSession, cfg.ID)
	}
	m.sessions[cfg.ID] = s
	m.mu.Unlock()

	cfg.Launch.OnDiagnostic = s.emitDiagnostic
	go s.drain()
	if err := cfg.Client.Launch(ctx, cfg.Launch); err != nil {
		s.mu.Lock()
		s.closeRequested = true
		s.state = StateClosing
		s.mu.Unlock()
		s.closeOnce.Do(func() {
			err := cfg.Client.Close(context.Background())
			s.mu.Lock()
			s.closeErr = err
			s.mu.Unlock()
		})
		s.finish()
		return nil, normalizeOperationError("launch", err)
	}
	s.mu.Lock()
	if identity, ok := cfg.Client.(interface{ ProviderSessionID() string }); ok {
		s.providerSessionID = identity.ProviderSessionID()
	}
	if s.state == StateLaunching {
		s.state = StateReady
	}
	s.lastActivity = time.Now().UTC()
	s.mu.Unlock()
	return s, nil
}

func (m *Manager) unregister(id string, expected *Session) {
	m.mu.Lock()
	if m.sessions[id] == expected {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
}

func (m *Manager) Lookup(id string) (*Session, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	return s, ok
}

func (m *Manager) IsLive(id string) bool {
	s, ok := m.Lookup(id)
	return ok && s.Snapshot().Live
}

func (m *Manager) Cancel(ctx context.Context, id string) error {
	s, ok := m.Lookup(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotLive, id)
	}
	return s.Cancel(ctx)
}

func (m *Manager) Close(ctx context.Context, id string) error {
	s, ok := m.Lookup(id)
	if !ok {
		return nil
	}
	return s.Close(ctx)
}

func (m *Manager) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.RUnlock()
	var errs []error
	for _, s := range sessions {
		if err := s.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) Len() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}
