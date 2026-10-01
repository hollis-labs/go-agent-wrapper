package wrapper

import (
	"context"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/go-providers/registry"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/internal/testgate"
	"github.com/hollis-labs/go-agent-wrapper/launch"
)

// TestLiveLaunchEveryInstalledRuntimeThroughSelect is the live counterpart of
// TestLaunchEveryRegistryRuntimeThroughSelect: every registry runtime whose
// binary is installed launches through launch.Select + New + Run in its
// default mode against the real CLI. Gated by the shared live-provider gate;
// a runtime that is not installed is skipped. Codex's app-server only gets as
// far as a ready session, because the thread protocol is the host's
// (agentkit's turn package), not the wrapper's.
func TestLiveLaunchEveryInstalledRuntimeThroughSelect(t *testing.T) {
	testgate.RequireLiveProvider(t)
	for _, d := range registry.All() {
		t.Run(string(d.ID)+"/"+string(d.DefaultMode), func(t *testing.T) {
			binary, err := d.LookPath()
			if err != nil {
				t.Skipf("%s not installed: %v", d.Binary, err)
			}
			adapter, err := launch.Select(launch.Selection{Runtime: string(d.ID), Binary: binary})
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			sink := newCapturingSink()
			manager := acp.NewManager()
			w, err := New(Config{
				App: "live-launch-" + string(d.ID), Adapter: adapter,
				Activity: activity.NewBridge(sink), Workdir: t.TempDir(), ACPManager: manager,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			runDone := make(chan error, 1)
			go func() { runDone <- w.Run(ctx) }()

			const prompt = "Reply with the single word: ok"
			switch {
			case d.DefaultMode.ACP():
				waitForACPReady(t, manager, w.SessionID())
				if err := w.SendInput(ctx, []byte(prompt)); err != nil {
					t.Fatalf("SendInput: %v", err)
				}
				sink.waitFor(t, runtimeevents.KindAgentDelta, 2*time.Minute)
				waitForACPReady(t, manager, w.SessionID())
			case d.DefaultMode == runtimes.ModeJSONRPCStdio:
				sink.waitFor(t, runtimeevents.KindSessionReady, time.Minute)
			case d.DefaultMode == runtimes.ModeStreamingStdio:
				sink.waitFor(t, runtimeevents.KindSessionReady, time.Minute)
				if err := w.SendInput(ctx, []byte(`{"type":"user","message":{"role":"user","content":"`+prompt+`"}}`)); err != nil {
					t.Fatalf("SendInput: %v", err)
				}
				sink.waitFor(t, runtimeevents.KindTurnCompleted, 2*time.Minute)
			default:
				sink.waitFor(t, runtimeevents.KindSessionReady, time.Minute)
				if err := w.SendInput(ctx, []byte(prompt)); err != nil {
					t.Fatalf("SendInput: %v", err)
				}
				sink.waitFor(t, runtimeevents.KindTurnCompleted, 2*time.Minute)
			}
			if err := w.Stop(context.Background()); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			select {
			case err := <-runDone:
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("Run did not return after Stop")
			}
		})
	}
}
