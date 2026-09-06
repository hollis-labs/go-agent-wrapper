package adapters

import (
	"errors"
	"reflect"
	"testing"
)

func TestDeliveryCapabilitiesPlanCoversIdleBusyOfflineAndProviderIDAbsence(t *testing.T) {
	caps := DeliveryCapabilitiesForRuntime("claude", ProtocolClaudeStreamJSON, TransportStdio, InterruptProcess, false)
	if err := caps.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	corr := DeliveryCorrelation{
		SessionID:         "sess_123",
		ProviderSessionID: "",
		BindingID:         "bind_1",
		BindingGeneration: 4,
		DeliveryID:        "delivery_1",
		AttemptID:         "attempt_1",
	}
	plan, err := caps.Plan(DeliveryPlanRequest{
		Capability:                DeliveryCapabilitySendTurn,
		RuntimeState:              DeliveryRuntimeIdle,
		Correlation:               corr,
		RequireBindingGeneration:  true,
		ExpectedBindingGeneration: 4,
	})
	if err != nil {
		t.Fatalf("idle SendTurn plan: %v", err)
	}
	if plan.Capability != DeliveryCapabilitySendTurn {
		t.Fatalf("Capability = %q, want %q", plan.Capability, DeliveryCapabilitySendTurn)
	}
	if plan.Correlation.ProviderSessionID != "" {
		t.Fatalf("ProviderSessionID = %q, want empty when provider omitted it", plan.Correlation.ProviderSessionID)
	}
	if plan.Correlation.BindingGeneration != 4 {
		t.Fatalf("BindingGeneration = %d, want 4", plan.Correlation.BindingGeneration)
	}
	if !reflect.DeepEqual(plan.Receipts, []DeliveryReceiptStage{DeliveryReceiptTurnSubmitted}) {
		t.Fatalf("Receipts = %#v, want only turn_submitted", plan.Receipts)
	}

	_, err = caps.Plan(DeliveryPlanRequest{
		Capability:   DeliveryCapabilitySendTurn,
		RuntimeState: DeliveryRuntimeBusy,
		Correlation:  corr,
	})
	if !errors.Is(err, ErrDeliveryTargetBusy) {
		t.Fatalf("busy SendTurn error = %v, want ErrDeliveryTargetBusy", err)
	}

	_, err = caps.Plan(DeliveryPlanRequest{
		Capability:   DeliveryCapabilitySendTurn,
		RuntimeState: DeliveryRuntimeOffline,
		Correlation:  corr,
	})
	if !errors.Is(err, ErrDeliveryTargetOffline) {
		t.Fatalf("offline SendTurn error = %v, want ErrDeliveryTargetOffline", err)
	}
}

func TestDeliveryCapabilitiesDoNotSilentlyDowngradeUnsupportedBetweenToolCalls(t *testing.T) {
	caps := DeliveryCapabilitiesForRuntime("claude", ProtocolClaudeStreamJSON, TransportStdio, InterruptProcess, false)
	if caps.Supports(DeliveryCapabilityBetweenToolCalls) {
		t.Fatal("between-tool-call delivery must remain absent until an implementation is wired")
	}
	_, err := caps.Plan(DeliveryPlanRequest{
		Capability:   DeliveryCapabilityBetweenToolCalls,
		RuntimeState: DeliveryRuntimeBusy,
		Correlation: DeliveryCorrelation{
			SessionID:         "sess_123",
			BindingID:         "bind_1",
			BindingGeneration: 1,
			DeliveryID:        "delivery_1",
			AttemptID:         "attempt_1",
		},
	})
	if !errors.Is(err, ErrDeliveryCapabilityUnsupported) {
		t.Fatalf("between-tool-call plan error = %v, want ErrDeliveryCapabilityUnsupported", err)
	}
}

func TestDeliveryCapabilitiesDistinguishInterruptCancelTurnAndLifecycleStop(t *testing.T) {
	native := DeliveryCapabilitiesForRuntime("opencode", ProtocolOpenCodeNative, TransportHTTPSSE, InterruptTurn, false)
	if !native.Supports(DeliveryCapabilityInterrupt) {
		t.Fatal("native InterruptTurn descriptor should advertise interrupt")
	}
	if !native.Supports(DeliveryCapabilityLifecycleStop) {
		t.Fatal("native descriptor should advertise lifecycle stop")
	}
	if native.Supports(DeliveryCapabilityCancelTurn) {
		t.Fatal("native lifecycle Stop must not advertise non-closing CancelTurn")
	}
	_, err := native.Plan(DeliveryPlanRequest{Capability: DeliveryCapabilityCancelTurn, RuntimeState: DeliveryRuntimeBusy})
	if !errors.Is(err, ErrDeliveryCapabilityUnsupported) {
		t.Fatalf("native cancel-turn error = %v, want unsupported", err)
	}

	acp := DeliveryCapabilitiesForRuntime("claude", ProtocolACP, TransportStdio, InterruptTurn, true)
	if !acp.Supports(DeliveryCapabilityCancelTurn) {
		t.Fatal("ACP descriptor should advertise turn-scoped cancel")
	}
	plan, err := acp.Plan(DeliveryPlanRequest{
		Capability:   DeliveryCapabilityCancelTurn,
		RuntimeState: DeliveryRuntimeBusy,
		Correlation:  DeliveryCorrelation{SessionID: "sess_123", ProviderSessionID: "provider_456", BindingID: "bind_1", BindingGeneration: 2},
	})
	if err != nil {
		t.Fatalf("ACP cancel-turn plan: %v", err)
	}
	if !reflect.DeepEqual(plan.Receipts, []DeliveryReceiptStage{DeliveryReceiptCancelRequested}) {
		t.Fatalf("ACP cancel receipts = %#v, want cancel_requested only", plan.Receipts)
	}
}

func TestDeliveryCapabilitiesFenceRouteGeneration(t *testing.T) {
	caps := DeliveryCapabilitiesForRuntime("codex", ProtocolCodexAppServer, TransportStdio, InterruptProcess, false)
	_, err := caps.Plan(DeliveryPlanRequest{
		Capability:                DeliveryCapabilitySendTurn,
		RuntimeState:              DeliveryRuntimeIdle,
		Correlation:               DeliveryCorrelation{SessionID: "sess_123", BindingID: "bind_1", BindingGeneration: 8},
		RequireBindingGeneration:  true,
		ExpectedBindingGeneration: 7,
	})
	if !errors.Is(err, ErrDeliveryRouteStale) {
		t.Fatalf("stale route error = %v, want ErrDeliveryRouteStale", err)
	}
}

func TestDeliveryCapabilitiesValidateAdvertisedEvidence(t *testing.T) {
	caps := NewDeliveryCapabilities(DeliveryCapabilityEvidence{Capability: DeliveryCapabilitySendTurn})
	if err := caps.Validate(); !errors.Is(err, ErrDeliveryCapabilityUnsupported) {
		t.Fatalf("Validate missing evidence = %v, want capability unsupported", err)
	}
	_, err := caps.Plan(DeliveryPlanRequest{Capability: DeliveryCapabilitySendTurn})
	if !errors.Is(err, ErrDeliveryCapabilityUnsupported) {
		t.Fatalf("Plan missing evidence = %v, want capability unsupported", err)
	}
}

func TestDeliveryCapabilitiesCloneIsIndependent(t *testing.T) {
	caps := DeliveryCapabilitiesForRuntime("claude", ProtocolClaudeStreamJSON, TransportStdio, InterruptProcess, false)
	clone := caps.Clone()
	clone.Supported[0].Evidence = "mutated"
	if caps.Supported[0].Evidence == "mutated" {
		t.Fatal("Clone shares backing storage with original")
	}
}
