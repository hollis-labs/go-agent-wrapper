package acp

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

type managedFakeClient struct {
	events       chan runtimeevents.Event
	closeOnce    sync.Once
	closeCalls   atomic.Int32
	cancelCalls  atomic.Int32
	launchErr    error
	providerID   string
	onDiagnostic func(Diagnostic)
}

func newManagedFakeClient() *managedFakeClient {
	return &managedFakeClient{events: make(chan runtimeevents.Event, 16), providerID: "provider-1"}
}

func (c *managedFakeClient) Launch(_ context.Context, params LaunchParams) error {
	c.onDiagnostic = params.OnDiagnostic
	if c.launchErr != nil {
		return c.launchErr
	}
	c.events <- runtimeevents.Event{Kind: runtimeevents.KindSessionReady}
	return nil
}

func (c *managedFakeClient) Prompt(context.Context, string) error {
	c.events <- runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: "turn-1"}
	return nil
}

func (c *managedFakeClient) Cancel(context.Context) error {
	c.cancelCalls.Add(1)
	c.events <- runtimeevents.Event{
		Kind: runtimeevents.KindTurnCompleted, TurnID: "turn-1",
		Payload: json.RawMessage(`{"stop_reason":"cancelled"}`),
	}
	return nil
}

func (c *managedFakeClient) Events() <-chan runtimeevents.Event { return c.events }
func (c *managedFakeClient) InterruptCapability() adapters.InterruptCapability {
	return adapters.InterruptTurn
}
func (c *managedFakeClient) Close(context.Context) error {
	c.closeCalls.Add(1)
	c.closeOnce.Do(func() { close(c.events) })
	return nil
}
func (c *managedFakeClient) ProviderSessionID() string { return c.providerID }

func TestManagerOwnsRegistrationCancelCloseAndCleanupExactlyOnce(t *testing.T) {
	m := NewManager()
	client := newManagedFakeClient()
	session, err := m.Launch(context.Background(), SessionConfig{ID: "runtime-1", Client: client})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got, ok := m.Lookup("runtime-1"); !ok || got != session {
		t.Fatalf("Lookup = (%p, %v), want (%p, true)", got, ok, session)
	}
	if !m.IsLive("runtime-1") {
		t.Fatal("IsLive returned false for registered session")
	}
	if got := session.ProviderSessionID(); got != "provider-1" {
		t.Fatalf("ProviderSessionID = %q, want provider-1", got)
	}
	if err := session.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	waitForState(t, session, StateProcessing)
	if err := m.Cancel(context.Background(), "runtime-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitForState(t, session, StateReady)
	if got := session.Snapshot().LastTurnOutcome; got != OutcomeCanceled {
		t.Fatalf("LastTurnOutcome = %q, want %q", got, OutcomeCanceled)
	}
	if err := session.Prompt(context.Background(), "after cancel"); err != nil {
		t.Fatalf("second Prompt after cancel: %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Close(context.Background(), "runtime-1")
		}()
	}
	wg.Wait()
	if err := session.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after intentional Close: %v", err)
	}
	if got := client.closeCalls.Load(); got != 1 {
		t.Fatalf("Client.Close calls = %d, want exactly 1", got)
	}
	if got := client.cancelCalls.Load(); got != 1 {
		t.Fatalf("Client.Cancel calls = %d, want 1", got)
	}
	if _, ok := m.Lookup("runtime-1"); ok || m.Len() != 0 {
		t.Fatalf("terminated session remained registered; lookup=%v len=%d", ok, m.Len())
	}
}

func TestManagerDoesNotPublishReadyBeforeHostCommit(t *testing.T) {
	m := NewManager()
	client := newManagedFakeClient()
	commitEntered := make(chan *Session, 1)
	releaseCommit := make(chan struct{})
	launchResult := make(chan *Session, 1)
	launchErr := make(chan error, 1)
	var authorityMu sync.Mutex
	var authority *Session
	go func() {
		session, err := m.Launch(context.Background(), SessionConfig{
			ID: "commit-barrier", Client: client,
			Commit: func(session *Session) error {
				commitEntered <- session
				<-releaseCommit
				authorityMu.Lock()
				authority = session
				authorityMu.Unlock()
				return nil
			},
		})
		launchResult <- session
		launchErr <- err
	}()

	session := <-commitEntered
	if snapshot := session.Snapshot(); snapshot.State != StateLaunching || snapshot.Live {
		t.Fatalf("during host commit snapshot = %+v, want launching and not live", snapshot)
	}
	if got := session.ProviderSessionID(); got != "provider-1" {
		t.Fatalf("provider identity before commit = %q, want provider-1", got)
	}
	if m.IsLive("commit-barrier") {
		t.Fatal("Manager published session live before host authority committed")
	}
	deadline := time.After(time.Second)
	for {
		session.mu.RLock()
		pendingReady := session.pendingReady
		session.mu.RUnlock()
		if pendingReady {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Manager did not observe client's early session.ready")
		default:
			runtime.Gosched()
		}
	}
	select {
	case event := <-session.Events():
		t.Fatalf("Manager published event %q before host authority committed", event.Kind)
	default:
	}
	close(releaseCommit)
	launched := <-launchResult
	if err := <-launchErr; err != nil {
		t.Fatalf("Launch: %v", err)
	}
	authorityMu.Lock()
	committed := authority
	authorityMu.Unlock()
	if committed != launched {
		t.Fatalf("committed authority = %p, launched session = %p", committed, launched)
	}
	if snapshot := launched.Snapshot(); snapshot.State != StateReady || !snapshot.Live {
		t.Fatalf("after commit snapshot = %+v, want ready and live", snapshot)
	}
	select {
	case event := <-launched.Events():
		if event.Kind != runtimeevents.KindSessionReady {
			t.Fatalf("first post-commit event = %q, want session.ready", event.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("session.ready was not published after host commit")
	}
	if err := launched.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestManagerNormalizesDisconnectChildExitMalformedAndLaunchFailure(t *testing.T) {
	t.Run("disconnect", func(t *testing.T) {
		m := NewManager()
		client := newManagedFakeClient()
		session, err := m.Launch(context.Background(), SessionConfig{ID: "disconnect", Client: client})
		if err != nil {
			t.Fatal(err)
		}
		client.closeOnce.Do(func() { close(client.events) })
		if err := session.Wait(context.Background()); !errors.Is(err, ErrDisconnected) {
			t.Fatalf("Wait error = %v, want ErrDisconnected", err)
		}
		if got := client.closeCalls.Load(); got != 1 {
			t.Fatalf("Client.Close calls = %d, want 1 after disconnect cleanup", got)
		}
	})

	t.Run("child exit", func(t *testing.T) {
		m := NewManager()
		client := newManagedFakeClient()
		session, err := m.Launch(context.Background(), SessionConfig{ID: "exit", Client: client})
		if err != nil {
			t.Fatal(err)
		}
		client.events <- runtimeevents.Event{Kind: runtimeevents.KindProcessExited, Payload: json.RawMessage(`{"error":"exit status 7"}`)}
		client.closeOnce.Do(func() { close(client.events) })
		if err := session.Wait(context.Background()); !errors.Is(err, ErrChildExit) {
			t.Fatalf("Wait error = %v, want ErrChildExit", err)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		m := NewManager()
		client := newManagedFakeClient()
		session, err := m.Launch(context.Background(), SessionConfig{ID: "malformed", Client: client})
		if err != nil {
			t.Fatal(err)
		}
		client.onDiagnostic(NewDiagnostic(DiagnosticMalformedJSON, "bad frame", `{"token":"secret-value"`))
		client.closeOnce.Do(func() { close(client.events) })
		if err := session.Wait(context.Background()); !errors.Is(err, ErrMalformedStream) {
			t.Fatalf("Wait error = %v, want ErrMalformedStream", err)
		}
	})

	t.Run("launch failure", func(t *testing.T) {
		m := NewManager()
		client := newManagedFakeClient()
		client.launchErr = errors.New("handshake rejected")
		if _, err := m.Launch(context.Background(), SessionConfig{ID: "failed", Client: client}); err == nil {
			t.Fatal("Launch returned nil error")
		}
		if got := client.closeCalls.Load(); got != 1 {
			t.Fatalf("Client.Close calls = %d, want exactly 1", got)
		}
		if m.Len() != 0 {
			t.Fatalf("Manager.Len = %d after failed launch, want 0", m.Len())
		}
	})
}

func TestDiagnosticRedaction(t *testing.T) {
	d := NewDiagnostic(DiagnosticMalformedJSON, "Authorization: Bearer bearer-value", `{"authorization":"quoted-secret","token":"abc","nested":{"api_key":"def"},"ok":"visible"`)
	if strings.Contains(d.Message, "bearer-value") || strings.Contains(d.Message, "Bearer") || strings.Contains(d.Raw, "quoted-secret") || strings.Contains(d.Raw, "abc") || strings.Contains(d.Raw, "def") {
		t.Fatalf("diagnostic leaked secret: %+v", d)
	}
	if !strings.Contains(d.Raw, "visible") || !strings.Contains(d.Raw, "[REDACTED]") {
		t.Fatalf("diagnostic redaction destroyed safe context or omitted marker: %+v", d)
	}
}

func TestManagerRejectsDuplicateRegistration(t *testing.T) {
	m := NewManager()
	first := newManagedFakeClient()
	session, err := m.Launch(context.Background(), SessionConfig{ID: "same", Client: first})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close(context.Background()) }()
	if _, err := m.Launch(context.Background(), SessionConfig{ID: "same", Client: newManagedFakeClient()}); !errors.Is(err, ErrDuplicateSession) {
		t.Fatalf("duplicate Launch error = %v, want ErrDuplicateSession", err)
	}
}

func TestManagerCleanupDoesNotDependOnEventConsumer(t *testing.T) {
	m := NewManager()
	client := newManagedFakeClient()
	client.events = make(chan runtimeevents.Event, 300)
	session, err := m.Launch(context.Background(), SessionConfig{ID: "burst", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	for range 256 {
		client.events <- runtimeevents.Event{Kind: runtimeevents.KindAgentDelta}
	}
	client.closeOnce.Do(func() { close(client.events) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := session.Wait(ctx); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Wait error = %v, want ErrDisconnected", err)
	}
	if m.Len() != 0 || client.closeCalls.Load() != 1 {
		t.Fatalf("cleanup after unconsumed burst: manager len=%d close calls=%d", m.Len(), client.closeCalls.Load())
	}
}

func waitForState(t *testing.T, session *Session, want State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if session.Snapshot().State == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session state = %q, want %q", session.Snapshot().State, want)
}
