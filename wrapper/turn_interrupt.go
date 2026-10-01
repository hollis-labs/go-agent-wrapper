package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-providers/provider"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// turnMarks tracks the open turn and the one CancelTurn interrupted, so
// that turn's terminal event can say why it ended.
type turnMarks struct {
	mu          sync.Mutex
	open        string
	interrupted string
}

func (m *turnMarks) opened(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.open = id
}

func (m *turnMarks) closed(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.open == id {
		m.open = ""
	}
	if m.interrupted == id {
		m.interrupted = ""
	}
}

// markOpen marks the open turn interrupted and returns its id, "" when no
// turn is open.
func (m *turnMarks) markOpen() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.interrupted = m.open
	return m.open
}

func (m *turnMarks) unmark(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id != "" && m.interrupted == id {
		m.interrupted = ""
	}
}

func (m *turnMarks) isInterrupted(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return id != "" && m.interrupted == id
}

// withInterruptedReason marks a failed turn's payload as ended by CancelTurn.
func withInterruptedReason(payload any) any {
	out := map[string]any{}
	if m, ok := payload.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	} else if payload != nil {
		return payload
	}
	out["reason"] = "interrupted"
	return out
}

// requestTurnInterrupt asks a native session to end its turn and keep its
// process (agentsessions.TurnInterrupter). The open turn is marked first, so
// its turn.failed carries reason "interrupted" however soon the CLI ends it.
// A session whose adapter has no interrupt is ErrTurnCancelUnsupported.
func (w *Wrapper) requestTurnInterrupt(ctx context.Context, source runtimeevents.Source, session agentsessions.TurnInterrupter) error {
	requestID := runtimeevents.NewEventID()
	_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindInterruptRequested, source,
		map[string]any{"reason": "turn_cancel"}, runtimeevents.WithID(requestID))
	turnID := w.turns.markOpen()
	err := session.InterruptTurn(ctx)
	if errors.Is(err, agentsessions.ErrInterruptUnsupported) {
		err = ErrTurnCancelUnsupported
	}
	if err != nil {
		w.turns.unmark(turnID)
	}
	ack := map[string]any{"reason": "turn_cancel"}
	if turnID != "" {
		ack["turn_id"] = turnID
	}
	if err != nil {
		ack["error"] = err.Error()
	}
	_ = w.cfg.Activity.Emit(ctx, runtimeevents.KindInterruptAcknowledged, source,
		ack, runtimeevents.WithParentID(requestID))
	return err
}

// interruptiblePreparedAdapter is a preparedAdapter whose inner adapter can
// interrupt a turn. TurnInterrupter has no neutral answer, so a prepared
// adapter claims it only when the adapter it wraps does.
type interruptiblePreparedAdapter struct {
	*preparedAdapter
	interrupter provider.TurnInterrupter
}

var _ provider.TurnInterrupter = (*interruptiblePreparedAdapter)(nil)

func (a *interruptiblePreparedAdapter) InterruptRequest(id string) []byte {
	return a.interrupter.InterruptRequest(id)
}

func (a *interruptiblePreparedAdapter) InterruptResponse(line []byte) (string, bool, error) {
	return a.interrupter.InterruptResponse(line)
}

// rpcInterruptiblePreparedAdapter is a preparedAdapter whose inner adapter
// can interrupt a JSON-RPC turn (Codex app-server). Like TurnInterrupter, it
// is claimed only when the adapter it wraps has it.
type rpcInterruptiblePreparedAdapter struct {
	*preparedAdapter
	interrupter provider.RPCTurnInterrupter
}

var _ provider.RPCTurnInterrupter = (*rpcInterruptiblePreparedAdapter)(nil)

func (a *rpcInterruptiblePreparedAdapter) TurnNotification(method string, params json.RawMessage) (json.RawMessage, bool, bool) {
	return a.interrupter.TurnNotification(method, params)
}

func (a *rpcInterruptiblePreparedAdapter) InterruptCall(handle json.RawMessage) (string, json.RawMessage) {
	return a.interrupter.InterruptCall(handle)
}
