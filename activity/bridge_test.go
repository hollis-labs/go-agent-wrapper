package activity

import (
	"context"
	"testing"

	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestBridgeBindsIdentity(t *testing.T) {
	var got runtimeevents.Event
	sink := runtimeevents.SinkFunc(func(_ context.Context, ev runtimeevents.Event) error {
		got = ev
		return nil
	})

	b := NewBridge(sink)
	b.Bind("nanite", "ses_42", runtimeevents.Process{Provider: "claude", Runtime: "pty"})

	if err := b.Emit(context.Background(),
		runtimeevents.KindSessionReady,
		runtimeevents.Source{Channel: runtimeevents.ChannelPTY},
		nil,
	); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if got.App != "nanite" {
		t.Errorf("App = %q, want nanite", got.App)
	}
	if got.SessionID != "ses_42" {
		t.Errorf("SessionID = %q, want ses_42", got.SessionID)
	}
	if got.Process.Provider != "claude" {
		t.Errorf("Process.Provider = %q, want claude", got.Process.Provider)
	}
	if got.Sequence != 1 {
		t.Errorf("Sequence = %d, want 1", got.Sequence)
	}
}

func TestNewBridgeNilSinkIsNoOp(t *testing.T) {
	b := NewBridge(nil)
	b.Bind("test", "ses_x", runtimeevents.Process{})
	if err := b.Emit(context.Background(),
		runtimeevents.KindSessionReady,
		runtimeevents.Source{Channel: runtimeevents.ChannelPTY},
		nil,
	); err != nil {
		t.Fatalf("Emit on nil-sink bridge: %v", err)
	}
}
