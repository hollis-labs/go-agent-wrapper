package acp

import (
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// SessionLoadUnsupportedReason is the session.lost reason for a launch that
// asked to resume a session (LaunchParams.SessionIDPreset) on an agent that
// does not advertise session/load. The client starts a new session instead,
// and says so rather than leave the host believing it resumed.
const SessionLoadUnsupportedReason = "agent does not support session/load; started a new session"

// NewSessionLostEvent is the session.lost event for a launch whose requested
// session was not continued: requestedID is the preset, actualID the session
// the agent started instead. The payload matches go-agent-wrapper's native
// session.lost (requested_id, actual_id, reason), so a host reads one shape
// whatever the runtime.
func NewSessionLostEvent(requestedID, actualID, reason string) runtimeevents.Event {
	return runtimeevents.Event{
		Kind: runtimeevents.KindSessionLost,
		Payload: mustMarshal(map[string]any{
			"requested_id": requestedID,
			"actual_id":    actualID,
			"reason":       reason,
		}),
	}
}
