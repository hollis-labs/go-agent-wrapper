package activity

import (
	"context"
	"sync"
	"testing"
	"time"

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

func TestBridgeSerializesSequenceAssignmentAndSinkDelivery(t *testing.T) {
	firstAtSink := make(chan struct{})
	var mu sync.Mutex
	var sequences []uint64
	sink := runtimeevents.SinkFunc(func(_ context.Context, ev runtimeevents.Event) error {
		if ev.Sequence == 1 {
			close(firstAtSink)
			time.Sleep(25 * time.Millisecond)
		}
		mu.Lock()
		sequences = append(sequences, ev.Sequence)
		mu.Unlock()
		return nil
	})

	b := NewBridge(sink)
	b.Bind("test", "ses_ordered", runtimeevents.Process{})
	done := make(chan struct{}, 2)
	go func() {
		_ = b.Emit(context.Background(), runtimeevents.KindSessionReady, runtimeevents.Source{}, nil)
		done <- struct{}{}
	}()
	<-firstAtSink
	go func() {
		_ = b.Emit(context.Background(), runtimeevents.KindSessionHeartbeat, runtimeevents.Source{}, nil)
		done <- struct{}{}
	}()
	<-done
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(sequences) != 2 || sequences[0] != 1 || sequences[1] != 2 {
		t.Fatalf("sink delivery sequences = %v, want [1 2]", sequences)
	}
}
