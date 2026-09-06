package adapters

import (
	"errors"
	"fmt"
)

// DeliveryCapability names a concrete provider/runtime delivery or control
// operation that an Adapter can truthfully perform today. Unsupported optional
// operations are omitted from [DeliveryCapabilities]; callers can use Plan to
// receive typed errors instead of silently downgrading to another operation.
type DeliveryCapability string

const (
	// DeliveryCapabilityQueue means the adapter owns a durable queueing layer.
	// Shipped wrapper adapters do not advertise this; go-messaging/Tether own
	// durable queue semantics outside provider drivers.
	DeliveryCapabilityQueue DeliveryCapability = "queue"

	// DeliveryCapabilitySendTurn means a host can submit one turn through the
	// runtime's normal input path (for example Wrapper.SendInput,
	// agentsessions.Session.SendInput, or ACP session/prompt) when the target is
	// idle.
	DeliveryCapabilitySendTurn DeliveryCapability = "send_turn"

	// DeliveryCapabilityBetweenToolCalls means a busy provider session accepts a
	// message between tool calls without waiting for the current turn to finish.
	// No current wrapper adapter advertises this until a provider-backed
	// implementation exists.
	DeliveryCapabilityBetweenToolCalls DeliveryCapability = "between_tool_calls"

	// DeliveryCapabilityInterrupt means the runtime has a genuine provider-level
	// mid-turn interrupt/abort before lifecycle teardown. It is narrower than
	// lifecycle stop and does not imply a non-closing CancelTurn API.
	DeliveryCapabilityInterrupt DeliveryCapability = "interrupt"

	// DeliveryCapabilityCancelTurn means the runtime exposes a turn-scoped cancel
	// operation that does not close the whole session.
	DeliveryCapabilityCancelTurn DeliveryCapability = "cancel_turn"

	// DeliveryCapabilityLifecycleStop means the runtime exposes a session/process
	// stop path. This may be process-level teardown when the provider has no
	// finer control surface.
	DeliveryCapabilityLifecycleStop DeliveryCapability = "lifecycle_stop"
)

// DeliveryRuntimeState is the caller's current observation of the target
// runtime. It is an input to planning, not proof of authorization.
type DeliveryRuntimeState string

const (
	DeliveryRuntimeIdle    DeliveryRuntimeState = "idle"
	DeliveryRuntimeBusy    DeliveryRuntimeState = "busy"
	DeliveryRuntimeOffline DeliveryRuntimeState = "offline"
)

// DeliveryReceiptStage names only wrapper/provider-observable stages. There is
// deliberately no consumed/understood/succeeded stage here.
type DeliveryReceiptStage string

const (
	DeliveryReceiptTurnSubmitted       DeliveryReceiptStage = "turn_submitted"
	DeliveryReceiptInterruptRequested  DeliveryReceiptStage = "interrupt_requested"
	DeliveryReceiptCancelRequested     DeliveryReceiptStage = "turn_cancel_requested"
	DeliveryReceiptLifecycleStopIssued DeliveryReceiptStage = "lifecycle_stop_issued"
)

var (
	ErrDeliveryCapabilityUnsupported = errors.New("adapters: delivery capability unsupported")
	ErrDeliveryTargetBusy            = errors.New("adapters: delivery target busy")
	ErrDeliveryTargetOffline         = errors.New("adapters: delivery target offline")
	ErrDeliveryRouteStale            = errors.New("adapters: delivery route generation stale")
	ErrDeliveryInvalidRequest        = errors.New("adapters: invalid delivery request")
)

// DeliveryCapabilityEvidence ties an advertised capability to the concrete
// implementation/evidence that backs it. Empty evidence is invalid for an
// advertised capability.
type DeliveryCapabilityEvidence struct {
	Capability DeliveryCapability
	Mechanism  string
	Evidence   string
}

// DeliveryCapabilities is a static, cloneable set of supported provider/runtime
// delivery operations. The zero value advertises no provider delivery support.
type DeliveryCapabilities struct {
	Supported []DeliveryCapabilityEvidence
}

// NewDeliveryCapabilities builds a de-duplicated capability set. Empty entries
// are ignored so callers can compose conditionally without manufacturing
// support.
func NewDeliveryCapabilities(entries ...DeliveryCapabilityEvidence) DeliveryCapabilities {
	seen := map[DeliveryCapability]struct{}{}
	out := DeliveryCapabilities{}
	for _, entry := range entries {
		if entry.Capability == "" {
			continue
		}
		if _, ok := seen[entry.Capability]; ok {
			continue
		}
		seen[entry.Capability] = struct{}{}
		out.Supported = append(out.Supported, entry)
	}
	return out
}

// DeliveryCapabilitiesForRuntime returns the honest default delivery/control
// declaration for a wrapper runtime descriptor. It intentionally does not infer
// Claude Code cross-session ListAgents/SendMessage support: this repository has
// no provider-backed route/send implementation for that API yet, so
// between-tool-call delivery stays absent.
func DeliveryCapabilitiesForRuntime(provider string, protocol Protocol, transport Transport, interrupt InterruptCapability, turnScopedCancel bool) DeliveryCapabilities {
	mechanism := deliveryMechanism(protocol, transport)
	evidence := deliveryEvidence(provider, protocol, transport)
	entries := []DeliveryCapabilityEvidence{
		{
			Capability: DeliveryCapabilitySendTurn,
			Mechanism:  mechanism,
			Evidence:   evidence,
		},
		{
			Capability: DeliveryCapabilityLifecycleStop,
			Mechanism:  "agentsessions.Session.Stop / wrapper.Stop",
			Evidence:   "existing wrapper lifecycle control path emits interrupt.requested/interrupt.acknowledged and calls the runtime Stop implementation",
		},
	}
	if interrupt == InterruptTurn || interrupt == InterruptSteer {
		entries = append(entries, DeliveryCapabilityEvidence{
			Capability: DeliveryCapabilityInterrupt,
			Mechanism:  "provider-native mid-turn abort before lifecycle teardown",
			Evidence:   "descriptor Interrupt capability is verified as " + string(interrupt),
		})
	}
	if turnScopedCancel {
		entries = append(entries, DeliveryCapabilityEvidence{
			Capability: DeliveryCapabilityCancelTurn,
			Mechanism:  "acp.Session.Cancel / wrapper.CancelTurn",
			Evidence:   "ACP managed session sends session/cancel without closing the session",
		})
	}
	return NewDeliveryCapabilities(entries...)
}

func deliveryMechanism(protocol Protocol, transport Transport) string {
	switch {
	case protocol == ProtocolACP:
		return "ACP session/prompt"
	case protocol == ProtocolClaudeStreamJSON:
		return "Claude stream-json stdin turn"
	case protocol == ProtocolCodexAppServer:
		return "Codex app-server JSON-RPC turn"
	case protocol == ProtocolOpenCodeNative:
		return "OpenCode HTTP/SSE turn"
	case protocol == ProtocolPTYRaw && transport == TransportPTY:
		return "PTY stdin turn"
	case protocol == "" && transport == "":
		return "agentsessions adapter subprocess turn"
	default:
		return "agentsessions Session.SendInput"
	}
}

func deliveryEvidence(provider string, protocol Protocol, transport Transport) string {
	if provider == "" {
		provider = "provider"
	}
	return fmt.Sprintf("%s descriptor maps protocol=%q transport=%q to wrapper.SendInput/agentsessions delivery", provider, protocol, transport)
}

// Clone returns a deep copy safe for Descriptor.Describe callers.
func (c DeliveryCapabilities) Clone() DeliveryCapabilities {
	return DeliveryCapabilities{Supported: append([]DeliveryCapabilityEvidence(nil), c.Supported...)}
}

// Supports reports whether cap is advertised.
func (c DeliveryCapabilities) Supports(cap DeliveryCapability) bool {
	_, ok := c.Evidence(cap)
	return ok
}

// Evidence returns the concrete evidence for cap when advertised.
func (c DeliveryCapabilities) Evidence(cap DeliveryCapability) (DeliveryCapabilityEvidence, bool) {
	for _, entry := range c.Supported {
		if entry.Capability == cap {
			return entry, true
		}
	}
	return DeliveryCapabilityEvidence{}, false
}

// Require returns a typed error when cap is absent or lacks evidence.
func (c DeliveryCapabilities) Require(cap DeliveryCapability) error {
	entry, ok := c.Evidence(cap)
	if !ok {
		return &DeliveryCapabilityError{Capability: cap}
	}
	if entry.Mechanism == "" || entry.Evidence == "" {
		return &DeliveryCapabilityError{Capability: cap, Reason: "missing evidence"}
	}
	return nil
}

// Validate ensures every advertised capability has evidence.
func (c DeliveryCapabilities) Validate() error {
	for _, entry := range c.Supported {
		if entry.Capability == "" || entry.Mechanism == "" || entry.Evidence == "" {
			return &DeliveryCapabilityError{Capability: entry.Capability, Reason: "missing evidence"}
		}
	}
	return nil
}

// DeliveryCapabilityError reports unsupported or invalid capability metadata.
type DeliveryCapabilityError struct {
	Capability DeliveryCapability
	Reason     string
}

func (e *DeliveryCapabilityError) Error() string {
	if e == nil {
		return "<nil>"
	}
	reason := e.Reason
	if reason == "" {
		reason = "unsupported"
	}
	return fmt.Sprintf("%v: %s (%s)", ErrDeliveryCapabilityUnsupported, e.Capability, reason)
}

func (e *DeliveryCapabilityError) Unwrap() error { return ErrDeliveryCapabilityUnsupported }

// DeliveryCorrelation carries content-free IDs needed to tie delivery, route,
// provider and runtime observations together. ProviderSessionID is optional and
// must remain empty when the provider has not supplied one.
type DeliveryCorrelation struct {
	SessionID         string
	ProviderSessionID string
	BindingID         string
	BindingGeneration int64
	DeliveryID        string
	AttemptID         string
}

// DeliveryPlanRequest asks the adapter capability contract for an honest route.
type DeliveryPlanRequest struct {
	Capability                DeliveryCapability
	RuntimeState              DeliveryRuntimeState
	Correlation               DeliveryCorrelation
	RequireBindingGeneration  bool
	ExpectedBindingGeneration int64
}

// DeliveryPlan is an executable provider/runtime operation plus the only receipt
// stages that operation may truthfully report.
type DeliveryPlan struct {
	Capability  DeliveryCapability
	Mechanism   string
	Correlation DeliveryCorrelation
	Receipts    []DeliveryReceiptStage
}

// Plan validates that req can be executed without silently switching to a
// different operation.
func (c DeliveryCapabilities) Plan(req DeliveryPlanRequest) (DeliveryPlan, error) {
	if req.Capability == "" {
		return DeliveryPlan{}, fmt.Errorf("%w: capability is required", ErrDeliveryInvalidRequest)
	}
	if req.Correlation.BindingGeneration < 0 {
		return DeliveryPlan{}, fmt.Errorf("%w: binding generation is negative", ErrDeliveryInvalidRequest)
	}
	if req.RequireBindingGeneration && req.Correlation.BindingGeneration != req.ExpectedBindingGeneration {
		return DeliveryPlan{}, fmt.Errorf("%w: expected=%d actual=%d", ErrDeliveryRouteStale, req.ExpectedBindingGeneration, req.Correlation.BindingGeneration)
	}
	entry, ok := c.Evidence(req.Capability)
	if !ok {
		return DeliveryPlan{}, &DeliveryCapabilityError{Capability: req.Capability}
	}
	if entry.Mechanism == "" || entry.Evidence == "" {
		return DeliveryPlan{}, &DeliveryCapabilityError{Capability: req.Capability, Reason: "missing evidence"}
	}
	switch req.RuntimeState {
	case DeliveryRuntimeOffline:
		return DeliveryPlan{}, ErrDeliveryTargetOffline
	case DeliveryRuntimeBusy:
		if req.Capability == DeliveryCapabilitySendTurn {
			return DeliveryPlan{}, ErrDeliveryTargetBusy
		}
	case DeliveryRuntimeIdle, "":
		// Empty runtime state is accepted as the conservative historical default:
		// callers that do not have liveness still get capability validation.
	default:
		return DeliveryPlan{}, fmt.Errorf("%w: unknown runtime state %q", ErrDeliveryInvalidRequest, req.RuntimeState)
	}
	return DeliveryPlan{
		Capability:  req.Capability,
		Mechanism:   entry.Mechanism,
		Correlation: req.Correlation,
		Receipts:    deliveryReceipts(req.Capability),
	}, nil
}

func deliveryReceipts(cap DeliveryCapability) []DeliveryReceiptStage {
	switch cap {
	case DeliveryCapabilitySendTurn:
		return []DeliveryReceiptStage{DeliveryReceiptTurnSubmitted}
	case DeliveryCapabilityInterrupt:
		return []DeliveryReceiptStage{DeliveryReceiptInterruptRequested}
	case DeliveryCapabilityCancelTurn:
		return []DeliveryReceiptStage{DeliveryReceiptCancelRequested}
	case DeliveryCapabilityLifecycleStop:
		return []DeliveryReceiptStage{DeliveryReceiptLifecycleStopIssued}
	default:
		return nil
	}
}
