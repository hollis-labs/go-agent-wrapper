package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// PermissionOptionKind is the ACP-defined semantic category of an option
// offered by an agent in session/request_permission. The constants below are
// the four standard v1 kinds. Unknown future kinds are preserved rather than
// rejected; option ID, not kind, is the wire-level selection authority.
type PermissionOptionKind string

const (
	PermissionAllowOnce    PermissionOptionKind = "allow_once"
	PermissionAllowAlways  PermissionOptionKind = "allow_always"
	PermissionRejectOnce   PermissionOptionKind = "reject_once"
	PermissionRejectAlways PermissionOptionKind = "reject_always"
)

// PermissionOption is one exact choice offered by the ACP agent. A responder
// selects an option by returning its OptionID unchanged.
type PermissionOption struct {
	OptionID string               `json:"optionId"`
	Name     string               `json:"name"`
	Kind     PermissionOptionKind `json:"kind"`
}

// PermissionToolCall is the approval-relevant subset of ACP's ToolCallUpdate.
// RawInput is intentionally passed to the host responder because it normally
// contains the command, path, or other operation being considered. Hosts must
// treat it as potentially sensitive. It is never copied into diagnostics.
type PermissionToolCall struct {
	ToolCallID string          `json:"toolCallId"`
	Name       string          `json:"name,omitempty"`
	Title      string          `json:"title,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Status     string          `json:"status,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
}

// PermissionRequest is the validated subset of an ACP
// session/request_permission request presented to a host responder. RawParams
// preserves provider extensions (including _meta) for hosts that understand
// them. Like RawInput, it may contain sensitive operation data and is never
// copied into diagnostics.
type PermissionRequest struct {
	SessionID string             `json:"sessionId"`
	ToolCall  PermissionToolCall `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
	RawParams json.RawMessage    `json:"-"`
}

// PermissionSelection is a host's answer to one permission request. The zero
// value declines safely with ACP's cancelled outcome. A non-empty OptionID is
// accepted only when it exactly matches one option in the request.
type PermissionSelection struct {
	OptionID string
}

// SelectPermissionOption constructs a selection for an exact provider-offered
// option ID. It does not infer policy from an option's display name or kind.
func SelectPermissionOption(optionID string) PermissionSelection {
	return PermissionSelection{OptionID: optionID}
}

// DecodeJSONRPCRequestID validates one present JSON-RPC request ID and returns
// its event-safe value. JSON-RPC permits string and number IDs; ACP's generated
// schema also represents null, which clients must still echo rather than
// misclassifying as a notification. Objects, arrays, and booleans are invalid.
func DecodeJSONRPCRequestID(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var id any
	if err := decoder.Decode(&id); err != nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, false
	}
	switch id.(type) {
	case nil, string, json.Number:
		return id, true
	default:
		return nil, false
	}
}

// BestEffortPermissionRequestResponder is the optional host callback for ACP
// session/request_permission. It is deliberately named best-effort: it can
// answer only operations for which an agent chooses to ask, and therefore is
// not a general execution gate or a replacement for host authorization.
//
// The callback may wait for an operator decision. It must honor ctx and return
// promptly after cancellation. It may call Client methods: shipped clients run
// it away from their protocol reader and lifecycle locks so Prompt, Cancel, or
// Close cannot deadlock the wire. A zero selection declines the request.
type BestEffortPermissionRequestResponder func(
	ctx context.Context,
	request PermissionRequest,
) (PermissionSelection, error)

// PermissionResolution is the normalized, validated answer produced for a
// permission request. Adapters use it to keep wire results and runtime events
// identical across all ACP implementations.
type PermissionResolution struct {
	Outcome     PermissionOutcome
	Option      PermissionOption
	Reason      string
	diagnostic  *Diagnostic
	responseErr error
}

// PermissionOutcome is ACP's discriminator for a normalized permission
// response.
type PermissionOutcome string

const (
	PermissionOutcomeCancelled PermissionOutcome = "cancelled"
	PermissionOutcomeSelected  PermissionOutcome = "selected"
)

// Allowed reports whether a selected standard ACP option is allow-shaped.
// Unknown extension kinds fail closed for event consumers even though the
// exact option selection is still returned to the agent.
func (r PermissionResolution) Allowed() bool {
	return r.Outcome == PermissionOutcomeSelected &&
		(r.Option.Kind == PermissionAllowOnce || r.Option.Kind == PermissionAllowAlways)
}

// Result returns the ACP RequestPermissionResponse result shape.
func (r PermissionResolution) Result() map[string]any {
	outcome := map[string]any{"outcome": r.Outcome}
	if r.Outcome == PermissionOutcomeSelected {
		outcome["optionId"] = r.Option.OptionID
	}
	return map[string]any{"outcome": outcome}
}

// ResolvedEventPayload returns the normalized permission-resolved payload.
// requestID is the original JSON-RPC request ID (numeric or string).
func (r PermissionResolution) ResolvedEventPayload(requestID any) map[string]any {
	if r.responseErr != nil {
		return r.DeliveryFailureEventPayload(requestID)
	}
	payload := map[string]any{
		"request_id": requestID,
		"allowed":    r.Allowed(),
		"outcome":    r.Outcome,
		"reason":     r.Reason,
	}
	if r.Outcome == PermissionOutcomeSelected {
		payload["option_id"] = r.Option.OptionID
		payload["option_kind"] = r.Option.Kind
	}
	return payload
}

// DeliveryFailureEventPayload reports that a permission decision could not be
// delivered. Adapters use it immediately after a failed bounded write, before
// transport teardown can produce a turn terminal event.
func (r PermissionResolution) DeliveryFailureEventPayload(requestID any) map[string]any {
	return map[string]any{
		"request_id": requestID,
		"allowed":    false,
		"outcome":    PermissionOutcomeCancelled,
		"reason":     "permission response delivery failed",
	}
}

// ResponseError reports that the normalized response could not be delivered
// to the ACP agent. The underlying error is returned for lifecycle handling,
// but never copied into diagnostics or runtime event payloads.
func (r PermissionResolution) ResponseError() error { return r.responseErr }

// Diagnostic returns a bounded diagnostic for a malformed request, responder
// failure, or invalid selection. Diagnostics contain only fixed library text;
// callback errors, option IDs, and raw request/tool input are intentionally
// excluded so an arbitrary secret cannot escape without a recognizable key.
func (r PermissionResolution) Diagnostic() (Diagnostic, bool) {
	if r.diagnostic == nil {
		return Diagnostic{}, false
	}
	return *r.diagnostic, true
}

type permissionCallbackResult struct {
	selection PermissionSelection
	err       error
}

type pendingPermission struct {
	cancel     context.CancelFunc
	canceled   bool
	committed  bool
	generation uint64
}

// MaxConcurrentBestEffortPermissionRequests is the per-client upper bound on
// asynchronously dispatched permission responses and on responder callbacks
// that have started but not returned. The callback bound spans turn
// cancellation: a callback that ignores its context continues to occupy a slot
// until it actually exits, preventing unbounded goroutine and sensitive-input
// retention across turns.
const MaxConcurrentBestEffortPermissionRequests = 64

// BestEffortPermissionRequests coordinates concurrent responder calls for one
// client session. CancelTurn cancels the current turn's calls but permits later
// turns; Close cancels current calls and makes later requests decline.
// Callback goroutines are intentionally not joined: a responder that ignores
// context cannot hold Prompt, Cancel, Close, or the protocol reader hostage.
type BestEffortPermissionRequests struct {
	mu           sync.Mutex
	responder    BestEffortPermissionRequestResponder
	nextID       uint64
	pending      map[uint64]*pendingPermission
	callbacks    int
	dispatches   int
	responseGate sync.Locker
	dispatchDone chan struct{}
	generation   uint64
	turnActive   bool
	turnCanceled bool
	sessionID    string
	closed       bool
}

// NewBestEffortPermissionRequests constructs a session-scoped coordinator.
func NewBestEffortPermissionRequests(responder BestEffortPermissionRequestResponder) *BestEffortPermissionRequests {
	dispatchDone := make(chan struct{})
	close(dispatchDone)
	return &BestEffortPermissionRequests{
		responder:    responder,
		pending:      make(map[uint64]*pendingPermission),
		dispatchDone: dispatchDone,
	}
}

// Configured reports whether this coordinator has a host responder. Adapters
// use it only to preserve their exact legacy nil-responder event payloads.
func (p *BestEffortPermissionRequests) Configured() bool {
	return p != nil && p.responder != nil
}

// SetResponseGate installs the adapter lifecycle lock that orders a final
// permission response against Prompt admission, CancelTurn plus its wire
// notification, and Close. Shipped clients set it before any turn begins. A
// responder callback is never invoked while the gate is held.
func (p *BestEffortPermissionRequests) SetResponseGate(gate sync.Locker) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.responseGate = gate
	p.mu.Unlock()
}

// Dispatch admits and runs Respond away from a protocol reader while bounding
// the number of goroutines and raw request copies retained by response I/O. At
// capacity, it invokes respond synchronously with a cancelled resolution; this
// deliberately backpressures the one reader instead of allocating unbounded
// work. after runs once Respond (including response I/O) has finished.
func (p *BestEffortPermissionRequests) Dispatch(
	rawParams json.RawMessage,
	respond func(PermissionResolution) error,
	after func(PermissionResolution),
) {
	if p == nil {
		resolution := cancelledPermission("no responder configured")
		resolution = deliverPermissionResponse(resolution, respond)
		if after != nil {
			after(resolution)
		}
		return
	}
	p.mu.Lock()
	if p.dispatches >= MaxConcurrentBestEffortPermissionRequests {
		p.mu.Unlock()
		resolution := permissionFailure("responder capacity reached", "ACP permission responder capacity reached; cancelled")
		resolution = deliverPermissionResponse(resolution, respond)
		if after != nil {
			after(resolution)
		}
		return
	}
	if p.dispatches == 0 {
		p.dispatchDone = make(chan struct{})
	}
	p.dispatches++
	p.mu.Unlock()
	rawCopy := append(json.RawMessage(nil), rawParams...)
	go func() {
		resolution := p.Respond(rawCopy, respond)
		p.mu.Lock()
		p.dispatches--
		if p.dispatches == 0 {
			close(p.dispatchDone)
		}
		p.mu.Unlock()
		if after != nil {
			after(resolution)
		}
	}()
}

// SetSessionID binds requests to the one provider session owned by the client.
// A later request naming any other session fails closed before the responder is
// invoked. Shipped clients call this as soon as new/load establishes identity.
func (p *BestEffortPermissionRequests) SetSessionID(sessionID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.sessionID = sessionID
	p.mu.Unlock()
}

// Respond validates rawParams, invokes the configured responder away from the
// calling goroutine, and passes a fail-safe ACP resolution to respond. Respond
// itself must run outside the protocol reader because a legitimate operator
// decision may take time. The final cancellation check and response commit are
// linearized with CancelTurn, so a request from an already-cancelled turn
// cannot race through as selected. Event emission and transport I/O happen
// after that state lock is released. Shipped adapters also bound writes, so
// response backpressure can delay, but cannot indefinitely pin, EndTurn or
// lifecycle coordination.
func (p *BestEffortPermissionRequests) Respond(
	rawParams json.RawMessage,
	respond func(PermissionResolution) error,
) PermissionResolution {
	request, err := parsePermissionRequest(rawParams)
	if err != nil {
		resolution := permissionFailure("invalid request", "invalid ACP permission request; cancelled")
		return deliverPermissionResponse(resolution, respond)
	}
	if p != nil {
		p.mu.Lock()
		wrongSession := p.sessionID != "" && request.SessionID != p.sessionID
		p.mu.Unlock()
		if wrongSession {
			resolution := permissionFailure("session mismatch", "ACP permission request named a different session; cancelled")
			return deliverPermissionResponse(resolution, respond)
		}
	}
	if p == nil || p.responder == nil {
		resolution := cancelledPermission("no responder configured")
		return deliverPermissionResponse(resolution, respond)
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		resolution := cancelledPermission("client closed")
		return deliverPermissionResponse(resolution, respond)
	}
	if !p.turnActive || p.turnCanceled {
		p.mu.Unlock()
		resolution := cancelledPermission("turn not active")
		return deliverPermissionResponse(resolution, respond)
	}
	if p.callbacks >= MaxConcurrentBestEffortPermissionRequests {
		p.mu.Unlock()
		resolution := permissionFailure("responder capacity reached", "ACP permission responder capacity reached; cancelled")
		return deliverPermissionResponse(resolution, respond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.nextID++
	id := p.nextID
	entry := &pendingPermission{cancel: cancel, generation: p.generation}
	p.pending[id] = entry
	p.callbacks++
	p.mu.Unlock()

	resultCh := make(chan permissionCallbackResult, 1)
	go func() {
		invokePermissionResponder(ctx, p.responder, clonePermissionRequest(request), resultCh)
		p.mu.Lock()
		p.callbacks--
		p.mu.Unlock()
	}()

	var callback permissionCallbackResult
	select {
	case callback = <-resultCh:
	case <-ctx.Done():
		callback.err = ctx.Err()
	}

	p.mu.Lock()
	responseGate := p.responseGate
	p.mu.Unlock()
	if responseGate != nil {
		responseGate.Lock()
		defer responseGate.Unlock()
	}

	p.mu.Lock()
	canceled := entry.canceled || p.closed || ctx.Err() != nil ||
		!p.turnActive || p.turnCanceled || entry.generation != p.generation
	var resolution PermissionResolution
	if canceled || errors.Is(callback.err, context.Canceled) || errors.Is(callback.err, context.DeadlineExceeded) {
		resolution = cancelledPermission("request cancelled")
	} else if callback.err != nil {
		resolution = permissionFailure("responder failed", "ACP permission responder failed; cancelled")
	} else if callback.selection.OptionID == "" {
		resolution = cancelledPermission("responder cancelled")
	} else {
		for _, option := range request.Options {
			if option.OptionID != callback.selection.OptionID {
				continue
			}
			resolution = PermissionResolution{
				Outcome: PermissionOutcomeSelected,
				Option:  option,
				Reason:  "responder selected offered option",
			}
			break
		}
		if resolution.Outcome == "" {
			resolution = permissionFailure("invalid selection", "ACP permission responder returned an unoffered option; cancelled")
		}
	}
	// The state transition is the response's concurrency linearization point.
	// Lifecycle methods that win this lock first force a cancelled resolution;
	// once committed, an overlapping lifecycle call may proceed without waiting
	// for event emission or transport I/O.
	entry.committed = true
	p.mu.Unlock()
	resolution = deliverPermissionResponse(resolution, respond)
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
	cancel()
	return resolution
}

func invokePermissionResponder(
	ctx context.Context,
	responder BestEffortPermissionRequestResponder,
	request PermissionRequest,
	resultCh chan<- permissionCallbackResult,
) {
	result := permissionCallbackResult{}
	func() {
		defer func() {
			if recover() != nil {
				result.err = errors.New("permission responder panicked")
			}
		}()
		result.selection, result.err = responder(ctx, request)
	}()
	resultCh <- result
}

// BeginTurn opens a new permission generation. It must be called when Prompt
// admits a turn, before writing session/prompt to the agent.
func (p *BestEffortPermissionRequests) BeginTurn() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.pending {
		if entry.committed {
			continue
		}
		entry.canceled = true
		entry.cancel()
	}
	p.generation++
	p.turnActive = true
	p.turnCanceled = false
}

// EndTurn closes the current permission generation and cancels any responder
// call that outlived the provider's prompt result.
func (p *BestEffortPermissionRequests) EndTurn() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.turnActive = false
	for _, entry := range p.pending {
		if entry.generation == p.generation && !entry.committed {
			entry.canceled = true
			entry.cancel()
		}
	}
	dispatchDone := p.dispatchDone
	p.mu.Unlock()
	<-dispatchDone
}

// CancelTurn marks the current permission generation cancelled and cancels
// every responder currently waiting for it. Late permission frames for that
// turn continue to decline until BeginTurn opens the next generation.
func (p *BestEffortPermissionRequests) CancelTurn() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.turnCanceled = true
	for _, entry := range p.pending {
		if entry.generation == p.generation && !entry.committed {
			entry.canceled = true
			entry.cancel()
		}
	}
}

// Close cancels all current responder calls and makes future requests decline
// immediately. It never waits for callbacks to return.
func (p *BestEffortPermissionRequests) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.turnActive = false
	p.turnCanceled = true
	for _, entry := range p.pending {
		if entry.committed {
			continue
		}
		entry.canceled = true
		entry.cancel()
	}
}

func parsePermissionRequest(raw json.RawMessage) (PermissionRequest, error) {
	var wire struct {
		SessionID string              `json:"sessionId"`
		ToolCall  *PermissionToolCall `json:"toolCall"`
		Options   *[]PermissionOption `json:"options"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &wire) != nil {
		return PermissionRequest{}, errors.New("invalid JSON")
	}
	if wire.SessionID == "" || wire.ToolCall == nil || wire.ToolCall.ToolCallID == "" || wire.Options == nil {
		return PermissionRequest{}, errors.New("missing required identity")
	}
	seen := make(map[string]struct{}, len(*wire.Options))
	for _, option := range *wire.Options {
		if option.OptionID == "" || option.Name == "" || option.Kind == "" {
			return PermissionRequest{}, errors.New("incomplete option")
		}
		if _, duplicate := seen[option.OptionID]; duplicate {
			return PermissionRequest{}, errors.New("duplicate option id")
		}
		seen[option.OptionID] = struct{}{}
	}
	request := PermissionRequest{
		SessionID: wire.SessionID,
		ToolCall:  *wire.ToolCall,
		Options:   append([]PermissionOption(nil), (*wire.Options)...),
		RawParams: append(json.RawMessage(nil), raw...),
	}
	request.ToolCall.RawInput = append(json.RawMessage(nil), request.ToolCall.RawInput...)
	return request, nil
}

func clonePermissionRequest(request PermissionRequest) PermissionRequest {
	request.Options = append([]PermissionOption(nil), request.Options...)
	request.ToolCall.RawInput = append(json.RawMessage(nil), request.ToolCall.RawInput...)
	request.RawParams = append(json.RawMessage(nil), request.RawParams...)
	return request
}

func cancelledPermission(reason string) PermissionResolution {
	return PermissionResolution{Outcome: PermissionOutcomeCancelled, Reason: reason}
}

func permissionFailure(reason, diagnosticMessage string) PermissionResolution {
	diagnostic := NewDiagnostic(DiagnosticProtocol, diagnosticMessage, "")
	return PermissionResolution{
		Outcome:    PermissionOutcomeCancelled,
		Reason:     reason,
		diagnostic: &diagnostic,
	}
}

func deliverPermissionResponse(
	resolution PermissionResolution,
	respond func(PermissionResolution) error,
) PermissionResolution {
	if err := respond(resolution); err != nil {
		resolution.responseErr = err
		if resolution.diagnostic == nil {
			diagnostic := NewDiagnostic(DiagnosticProtocol, "ACP permission response delivery failed; transport closed", "")
			resolution.diagnostic = &diagnostic
		}
	}
	return resolution
}
