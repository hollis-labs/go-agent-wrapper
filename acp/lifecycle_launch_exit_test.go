package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// CW-20261001-0129. Manager.Launch's failure path finishes the session from
// the launching goroutine while drain may still be forwarding the client's
// events. finish closed s.events with drain mid-select on a send to it, and
// the host panicked with "send on closed channel".

// chattyFailingClient emits events continuously from the moment Launch
// starts until Close, and fails Launch: an agent that exits during launch
// while its reader is still delivering what it said.
type chattyFailingClient struct {
	events chan runtimeevents.Event

	mu        sync.Mutex
	closed    bool
	stopped   chan struct{}
	firstSent chan struct{}
	wg        sync.WaitGroup
}

func newChattyFailingClient() *chattyFailingClient {
	return &chattyFailingClient{
		events:    make(chan runtimeevents.Event),
		stopped:   make(chan struct{}),
		firstSent: make(chan struct{}),
	}
}

func (c *chattyFailingClient) Launch(context.Context, LaunchParams) error {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		kinds := []runtimeevents.EventKind{
			runtimeevents.KindAgentDelta, runtimeevents.KindTurnStarted,
			runtimeevents.KindTurnCompleted, runtimeevents.KindProcessExited,
		}
		for i := 0; ; i++ {
			select {
			case c.events <- runtimeevents.Event{Kind: kinds[i%len(kinds)], Payload: json.RawMessage(`{}`)}:
				if i == 0 {
					close(c.firstSent)
				}
			case <-c.stopped:
				return
			}
		}
	}()
	// Fail only once drain is taking events, so it is forwarding while
	// Launch's failure path finishes the session.
	<-c.firstSent
	return errors.New("agent exited during launch")
}

func (c *chattyFailingClient) Prompt(context.Context, string) error { return nil }
func (c *chattyFailingClient) Cancel(context.Context) error         { return nil }
func (c *chattyFailingClient) Events() <-chan runtimeevents.Event   { return c.events }
func (c *chattyFailingClient) InterruptCapability() adapters.InterruptCapability {
	return adapters.InterruptTurn
}

func (c *chattyFailingClient) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.stopped)
	go func() {
		c.wg.Wait()
		close(c.events)
	}()
	return nil
}

func TestManagerLaunchFailureWhileEventsInFlightNeverPanics(t *testing.T) {
	for i := 0; i < 500; i++ {
		m := NewManager()
		_, err := m.Launch(context.Background(), SessionConfig{ID: "chatty", Client: newChattyFailingClient()})
		if err == nil {
			t.Fatal("Launch succeeded, want the client's launch error")
		}
		if m.Len() != 0 {
			t.Fatalf("iteration %d: failed launch left %d sessions registered", i, m.Len())
		}
	}
}

// A finished session is terminal. Commit hands the host the *Session, so a
// host can hold it past a failed commit; an event the client delivers after
// finish must neither move it back to ready nor be published.
func TestManagerFinishedSessionIgnoresLateEvents(t *testing.T) {
	client := newManagedFakeClient()
	client.events = make(chan runtimeevents.Event) // unbuffered: a send returns once drain has it
	client.closeOnce.Do(func() {})                 // Close must not close events; the test does
	m := NewManager()
	var held *Session
	launched := make(chan error, 1)
	go func() {
		_, err := m.Launch(context.Background(), SessionConfig{ID: "late", Client: client,
			Commit: func(s *Session) error { held = s; return errors.New("host refused") }})
		launched <- err
	}()
	if got := <-client.events; got.Kind != runtimeevents.KindSessionReady {
		t.Fatalf("first event = %v, want session.ready", got.Kind)
	}
	if err := <-launched; err == nil {
		t.Fatal("Launch succeeded, want the commit error")
	}
	if got := held.Snapshot().State; got != StateClosed {
		t.Fatalf("state after failed commit = %q, want closed", got)
	}
	// drain keeps reading the client after finish, discarding, so a client
	// that blocks on its own sends is never stranded. The second send
	// returns only once drain has finished with the first.
	send := func(ev runtimeevents.Event) {
		t.Helper()
		select {
		case client.events <- ev:
		case <-time.After(5 * time.Second):
			t.Fatalf("drain stopped reading the client after finish; %v never delivered", ev.Kind)
		}
	}
	send(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, Payload: json.RawMessage(`{}`)})
	send(runtimeevents.Event{Kind: runtimeevents.KindAgentDelta})
	close(client.events)
	if got := held.Snapshot().State; got != StateClosed {
		t.Fatalf("state after a late turn.completed = %q, want closed", got)
	}
	for ev := range held.Events() {
		if ev.Kind == runtimeevents.KindTurnCompleted || ev.Kind == runtimeevents.KindAgentDelta {
			t.Fatalf("finished session published a late %v", ev.Kind)
		}
	}
}

// The reported repro: an ACP agent binary that exits immediately, launched
// through the NDJSON bridge client. The child's exit reaches drain as
// process.exited while Launch's failure path finishes the session.
func TestManagerLaunchOfImmediatelyExitingAgentNeverPanics(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}
	iterations := 300
	if testing.Short() {
		iterations = 50
	}
	for i := 0; i < iterations; i++ {
		client := NewNDJSONBridgeClient(NDJSONBridgeConfig{
			Component: "exit-test",
			ResolveCommand: func(LaunchParams) (string, []string, error) {
				return sh, []string{"-c", "exit 0"}, nil
			},
			HandleNotification: func(*NDJSONBridgeClient, string, json.RawMessage) {},
		})
		m := NewManager()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := m.Launch(ctx, SessionConfig{ID: "exits", Client: client, Launch: LaunchParams{Cwd: t.TempDir()}})
		cancel()
		if err == nil {
			t.Fatal("Launch of an agent that exits immediately succeeded")
		}
		if m.Len() != 0 {
			t.Fatalf("iteration %d: failed launch left %d sessions registered", i, m.Len())
		}
	}
}
