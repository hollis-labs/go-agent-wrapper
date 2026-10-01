package acp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// CW-20261001-0223: a launch that asks to resume a session on an agent without
// loadSession gets a new session. It used to get it silently, while the host
// believed it had resumed.

// buffered returns the events Launch left on the client's channel, without
// waiting for more.
func buffered(c *NDJSONBridgeClient) []runtimeevents.Event {
	var out []runtimeevents.Event
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

func kindIndexes(events []runtimeevents.Event, kind runtimeevents.EventKind) []int {
	var at []int
	for i, ev := range events {
		if ev.Kind == kind {
			at = append(at, i)
		}
	}
	return at
}

func launchWithPreset(t *testing.T, component string, loadSession bool, preset string) (*NDJSONBridgeClient, []runtimeevents.Event, []string) {
	t.Helper()
	skipUnlessSh(t)
	logPath := filepath.Join(t.TempDir(), "requests.log")
	c := newTestClient(component, scriptedAgentWithLoad(t, "", loadSession), logPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Launch(ctx, LaunchParams{Cwd: t.TempDir(), SessionIDPreset: preset}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_ = c.Close(closeCtx)
	})
	return c, buffered(c), readLog(t, logPath)
}

// The agent has no loadSession, so the preset cannot be loaded: the client
// calls session/new, and reports it. session.lost carries the preset and the
// session the agent started instead, ahead of session.ready.
func TestNDJSONBridgeClient_PresetWithoutLoadSessionReportsSessionLost(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		c, events, requests := launchWithPreset(t, component, false, "ses_old")

		if got := strings.Join(requests, ","); got != "initialize,session/new" {
			t.Fatalf("requests = %s, want initialize,session/new (no session/load)", got)
		}
		if got := c.ProviderSessionID(); got != "ses_new" {
			t.Fatalf("ProviderSessionID = %q, want ses_new", got)
		}
		lost := kindIndexes(events, runtimeevents.KindSessionLost)
		if len(lost) != 1 {
			t.Fatalf("session.lost events = %d, want 1; events: %+v", len(lost), events)
		}
		var payload map[string]string
		if err := json.Unmarshal(events[lost[0]].Payload, &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		want := map[string]string{"requested_id": "ses_old", "actual_id": "ses_new", "reason": SessionLoadUnsupportedReason}
		for k, v := range want {
			if payload[k] != v {
				t.Errorf("payload[%s] = %q, want %q", k, payload[k], v)
			}
		}
		ready := kindIndexes(events, runtimeevents.KindSessionReady)
		if len(ready) != 1 || lost[0] > ready[0] {
			t.Fatalf("session.lost must precede session.ready: lost at %v, ready at %v", lost, ready)
		}
	})
}

// With loadSession advertised the preset is loaded and kept, with nothing to
// report; a failed load stays a launch error (wrapper's
// TestAllACPClientsValidateInitializeAndGateResume covers that path).
func TestNDJSONBridgeClient_PresetWithLoadSessionReportsNoSessionLost(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		c, events, requests := launchWithPreset(t, component, true, "ses_old")

		if got := strings.Join(requests, ","); got != "initialize,session/load" {
			t.Fatalf("requests = %s, want initialize,session/load", got)
		}
		if got := c.ProviderSessionID(); got != "ses_old" {
			t.Fatalf("ProviderSessionID = %q, want the preset ses_old", got)
		}
		if lost := kindIndexes(events, runtimeevents.KindSessionLost); len(lost) != 0 {
			t.Fatalf("a loaded session reported session.lost: %+v", events)
		}
	})
}

// With no preset there is nothing to resume, whether or not the agent can
// load: a new session is the ask, not a loss.
func TestNDJSONBridgeClient_NoPresetReportsNoSessionLost(t *testing.T) {
	forEachComponent(t, func(t *testing.T, component string) {
		for _, loadSession := range []bool{true, false} {
			_, events, requests := launchWithPreset(t, component, loadSession, "")
			if got := strings.Join(requests, ","); got != "initialize,session/new" {
				t.Fatalf("loadSession=%v: requests = %s, want initialize,session/new", loadSession, got)
			}
			if lost := kindIndexes(events, runtimeevents.KindSessionLost); len(lost) != 0 {
				t.Fatalf("loadSession=%v: a plain new session reported session.lost: %+v", loadSession, events)
			}
		}
	})
}
