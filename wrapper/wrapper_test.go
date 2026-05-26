package wrapper

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
)

// stubAdapter implements [adapters.Adapter] but NOT
// [adapters.RuntimeAdapter] — used to verify Run rejects pure
// exec-shape adapters that can't drive the agentkit runtime.
type stubAdapter struct{}

func (stubAdapter) Name() string                  { return "stub" }
func (stubAdapter) Describe() adapters.Descriptor { return adapters.Descriptor{Provider: "stub", Runtime: "stub"} }
func (stubAdapter) Resolve(adapters.ResolveContext) (adapters.Spec, error) {
	return adapters.Spec{Binary: "/bin/true"}, nil
}

func TestNewRequiresApp(t *testing.T) {
	_, err := New(Config{
		Adapter:  stubAdapter{},
		Activity: activity.NewBridge(nil),
	})
	if err == nil || !contains(err.Error(), "App") {
		t.Fatalf("New without App: err=%v, want App-mentioning error", err)
	}
}

func TestNewRequiresAdapter(t *testing.T) {
	_, err := New(Config{
		App:      "test",
		Activity: activity.NewBridge(nil),
	})
	if err == nil || !contains(err.Error(), "Adapter") {
		t.Fatalf("New without Adapter: err=%v, want Adapter-mentioning error", err)
	}
}

func TestNewRequiresActivity(t *testing.T) {
	_, err := New(Config{
		App:     "test",
		Adapter: stubAdapter{},
	})
	if err == nil || !contains(err.Error(), "Activity") {
		t.Fatalf("New without Activity: err=%v, want Activity-mentioning error", err)
	}
}

func TestNewAcceptsMinimumValidConfig(t *testing.T) {
	w, err := New(Config{
		App:      "test",
		Adapter:  stubAdapter{},
		Activity: activity.NewBridge(nil),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if w == nil {
		t.Fatal("New returned nil Wrapper without error")
	}
}

func TestNewAssignsSessionIDWhenAbsent(t *testing.T) {
	w, err := New(Config{
		App:      "test",
		Adapter:  stubAdapter{},
		Activity: activity.NewBridge(nil),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if w.SessionID() == "" {
		t.Error("SessionID() returned empty after New with no explicit SessionID")
	}
}

func TestNewPreservesExplicitSessionID(t *testing.T) {
	w, err := New(Config{
		App:       "test",
		Adapter:   stubAdapter{},
		Activity:  activity.NewBridge(nil),
		SessionID: "ses_custom",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := w.SessionID(); got != "ses_custom" {
		t.Errorf("SessionID() = %q, want ses_custom", got)
	}
}

func TestRunRejectsNonRuntimeAdapter(t *testing.T) {
	// stubAdapter implements Adapter but NOT RuntimeAdapter.
	w, err := New(Config{
		App:      "test",
		Adapter:  stubAdapter{},
		Activity: activity.NewBridge(nil),
		Workdir:  "/tmp",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = w.Run(context.Background())
	if !errors.Is(err, ErrAdapterNotRuntime) {
		t.Fatalf("Run: err=%v, want ErrAdapterNotRuntime", err)
	}
}

func TestSendInputBeforeRun(t *testing.T) {
	w, err := New(Config{
		App:      "test",
		Adapter:  stubAdapter{},
		Activity: activity.NewBridge(nil),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.SendInput(context.Background(), []byte("hi")); !errors.Is(err, ErrSessionNotStarted) {
		t.Fatalf("SendInput before Run: err=%v, want ErrSessionNotStarted", err)
	}
}

func TestStopBeforeRunIsNoOp(t *testing.T) {
	w, err := New(Config{
		App:      "test",
		Adapter:  stubAdapter{},
		Activity: activity.NewBridge(nil),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Stop(context.Background()); err != nil {
		t.Errorf("Stop before Run: err=%v, want nil (no-op)", err)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
