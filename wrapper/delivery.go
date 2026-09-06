package wrapper

import (
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
)

// DeliveryCapabilities returns the configured adapter's static delivery/control
// capability declaration. The returned value is a clone and can be mutated by
// callers without changing adapter state.
func (w *Wrapper) DeliveryCapabilities() adapters.DeliveryCapabilities {
	if w == nil || w.cfg.Adapter == nil {
		return adapters.DeliveryCapabilities{}
	}
	return w.cfg.Adapter.Describe().Delivery.Clone()
}

// PlanDelivery combines the adapter's static delivery capabilities with the
// wrapper's current live-state observation. It never sends input or control
// frames; it only returns an executable plan or a typed adapters delivery error.
func (w *Wrapper) PlanDelivery(req adapters.DeliveryPlanRequest) (adapters.DeliveryPlan, error) {
	if req.Correlation.SessionID == "" && w != nil {
		req.Correlation.SessionID = w.sessionID
	}
	if req.RuntimeState == "" && w != nil {
		req.RuntimeState = w.deliveryRuntimeState()
	}
	if req.Correlation.ProviderSessionID == "" && w != nil {
		req.Correlation.ProviderSessionID = w.ProviderSessionID()
	}
	return w.DeliveryCapabilities().Plan(req)
}

func (w *Wrapper) deliveryRuntimeState() adapters.DeliveryRuntimeState {
	w.sessMu.RLock()
	session := w.session
	acpSession := w.acpSession
	w.sessMu.RUnlock()
	if acpSession != nil {
		return acpDeliveryRuntimeState(acpSession.Snapshot())
	}
	if session != nil {
		return sessionDeliveryRuntimeState(session.Health())
	}
	return adapters.DeliveryRuntimeOffline
}

func acpDeliveryRuntimeState(snapshot acp.Snapshot) adapters.DeliveryRuntimeState {
	if !snapshot.Live {
		return adapters.DeliveryRuntimeOffline
	}
	switch snapshot.State {
	case acp.StateReady:
		return adapters.DeliveryRuntimeIdle
	case acp.StateProcessing:
		return adapters.DeliveryRuntimeBusy
	default:
		return adapters.DeliveryRuntimeOffline
	}
}

func sessionDeliveryRuntimeState(health agentsessions.HealthStatus) adapters.DeliveryRuntimeState {
	if !health.Alive || health.State == agentsessions.LiveStateStopped {
		return adapters.DeliveryRuntimeOffline
	}
	if health.State == agentsessions.LiveStateProcessing {
		return adapters.DeliveryRuntimeBusy
	}
	return adapters.DeliveryRuntimeIdle
}
