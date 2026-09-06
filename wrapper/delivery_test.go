package wrapper

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
)

func TestWrapperPlanDeliveryBeforeRunReportsOffline(t *testing.T) {
	w := newDeliveryTestWrapper(t, adapters.Descriptor{
		Provider:  "fake",
		Protocol:  adapters.ProtocolPTYRaw,
		Transport: adapters.TransportPTY,
		Interrupt: adapters.InterruptProcess,
		Delivery:  adapters.DeliveryCapabilitiesForRuntime("fake", adapters.ProtocolPTYRaw, adapters.TransportPTY, adapters.InterruptProcess, false),
	})
	_, err := w.PlanDelivery(adapters.DeliveryPlanRequest{Capability: adapters.DeliveryCapabilitySendTurn})
	if !errors.Is(err, adapters.ErrDeliveryTargetOffline) {
		t.Fatalf("PlanDelivery before Run error = %v, want offline", err)
	}
}

func TestWrapperPlanDeliveryUsesNativeHealthAndCanonicalSession(t *testing.T) {
	w := newDeliveryTestWrapper(t, adapters.Descriptor{
		Provider:  "fake",
		Protocol:  adapters.ProtocolPTYRaw,
		Transport: adapters.TransportPTY,
		Interrupt: adapters.InterruptProcess,
		Delivery:  adapters.DeliveryCapabilitiesForRuntime("fake", adapters.ProtocolPTYRaw, adapters.TransportPTY, adapters.InterruptProcess, false),
	})
	w.session = healthSession{health: agentsessions.HealthStatus{Alive: true, State: agentsessions.LiveStateIdle}}
	plan, err := w.PlanDelivery(adapters.DeliveryPlanRequest{
		Capability:  adapters.DeliveryCapabilitySendTurn,
		Correlation: adapters.DeliveryCorrelation{BindingID: "binding", BindingGeneration: 3},
	})
	if err != nil {
		t.Fatalf("PlanDelivery idle: %v", err)
	}
	if plan.Correlation.SessionID != w.SessionID() {
		t.Fatalf("SessionID = %q, want wrapper session %q", plan.Correlation.SessionID, w.SessionID())
	}

	w.session = healthSession{health: agentsessions.HealthStatus{Alive: true, State: agentsessions.LiveStateProcessing}}
	_, err = w.PlanDelivery(adapters.DeliveryPlanRequest{Capability: adapters.DeliveryCapabilitySendTurn})
	if !errors.Is(err, adapters.ErrDeliveryTargetBusy) {
		t.Fatalf("PlanDelivery busy error = %v, want busy", err)
	}
}

func TestDeliveryRuntimeStateMapsACPSnapshots(t *testing.T) {
	if got := acpDeliveryRuntimeState(acp.Snapshot{State: acp.StateReady, Live: false}); got != adapters.DeliveryRuntimeOffline {
		t.Fatalf("not-live ready maps to %q, want offline", got)
	}
	if got := acpDeliveryRuntimeState(acp.Snapshot{State: acp.StateReady, Live: true}); got != adapters.DeliveryRuntimeIdle {
		t.Fatalf("ready maps to %q, want idle", got)
	}
	if got := acpDeliveryRuntimeState(acp.Snapshot{State: acp.StateProcessing, Live: true}); got != adapters.DeliveryRuntimeBusy {
		t.Fatalf("processing maps to %q, want busy", got)
	}
	if got := acpDeliveryRuntimeState(acp.Snapshot{State: acp.StateClosed}); got != adapters.DeliveryRuntimeOffline {
		t.Fatalf("closed maps to %q, want offline", got)
	}
}

func newDeliveryTestWrapper(t *testing.T, desc adapters.Descriptor) *Wrapper {
	t.Helper()
	bridge := activity.NewBridge(nil)
	w, err := New(Config{App: "test", Adapter: deliveryTestAdapter{desc: desc}, Activity: bridge, Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

type deliveryTestAdapter struct{ desc adapters.Descriptor }

func (a deliveryTestAdapter) Name() string { return "delivery-test" }
func (a deliveryTestAdapter) Describe() adapters.Descriptor {
	desc := a.desc
	desc.Delivery = desc.Delivery.Clone()
	return desc
}
func (a deliveryTestAdapter) Resolve(adapters.ResolveContext) (adapters.Spec, error) {
	return adapters.Spec{Binary: "/bin/echo"}, nil
}

type healthSession struct{ health agentsessions.HealthStatus }

func (s healthSession) Wait() (int, error)                                    { return 0, nil }
func (s healthSession) Stop(ctx context.Context) error                        { return nil }
func (s healthSession) SendInput(ctx context.Context, data []byte) error      { return nil }
func (s healthSession) Resize(ctx context.Context, rows, cols uint16) error   { return nil }
func (s healthSession) Health() agentsessions.HealthStatus                    { return s.health }
func (s healthSession) CheckpointHints() (agentsessions.CheckpointHint, bool) { return nil, false }
