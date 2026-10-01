package wrapper

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/adapters/claudeacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/codexacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/opencodeacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/piacp"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// CW-20261001-0223: a host that sets Config.SessionIDPreset on an ACP agent
// without loadSession used to get a new session and no word of it, while it
// believed it had resumed. The wrapper now emits session.lost, and
// OnSessionID reports the session the agent actually started.
func TestACPWrapperReportsSessionLostWhenAgentLacksLoadSession(t *testing.T) {
	factories := map[string]func(string) adapters.Adapter{
		"claude": func(path string) adapters.Adapter {
			return claudeacp.New(claudeacp.WithDirectBinary(path))
		},
		"codex": func(path string) adapters.Adapter {
			return codexacp.New(codexacp.WithBinary(path), codexacp.WithBridgePackageSpec("fixture"))
		},
		"opencode": func(path string) adapters.Adapter { return opencodeacp.New(opencodeacp.WithBinary(path)) },
		"pi":       func(path string) adapters.Adapter { return piacp.New(piacp.WithBinary(path)) },
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				mode         string
				wantLost     bool
				wantActualID string
			}{
				{"no-load", true, "fresh-456"},
				{"lifecycle", false, "resume-123"},
			} {
				t.Run(tc.mode, func(t *testing.T) {
					dir := t.TempDir()
					fixture := writeACPFixture(t, dir, filepath.Join(dir, "trace.log"), tc.mode)
					manager := acp.NewManager()
					sink := newCapturingSink()
					var (
						mu  sync.Mutex
						ids []string
					)
					w, err := New(Config{
						App:             "acp-session-lost-" + name,
						Adapter:         factory(fixture),
						Activity:        activity.NewBridge(sink),
						Workdir:         dir,
						SessionID:       "wrapper-lost-" + name,
						SessionIDPreset: "resume-123",
						ACPManager:      manager,
						OnSessionID: func(id string) {
							mu.Lock()
							ids = append(ids, id)
							mu.Unlock()
						},
					})
					if err != nil {
						t.Fatalf("New: %v", err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					runErrCh := make(chan error, 1)
					go func() { runErrCh <- w.Run(ctx) }()
					waitForACPReady(t, manager, w.SessionID())
					sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
					if err := w.Stop(ctx); err != nil {
						t.Fatalf("Stop: %v", err)
					}
					if err := <-runErrCh; err != nil {
						t.Fatalf("Run: %v", err)
					}

					mu.Lock()
					gotIDs := append([]string(nil), ids...)
					mu.Unlock()
					if len(gotIDs) == 0 || gotIDs[0] != tc.wantActualID {
						t.Fatalf("OnSessionID reported %v, want %q first", gotIDs, tc.wantActualID)
					}
					lost, ok := firstKind(sink.snapshot(), runtimeevents.KindSessionLost)
					if ok != tc.wantLost {
						t.Fatalf("session.lost emitted = %v, want %v; sink saw %v", ok, tc.wantLost, sink.kinds())
					}
					if !ok {
						return
					}
					var payload map[string]string
					if err := json.Unmarshal(lost.Payload, &payload); err != nil {
						t.Fatalf("decode session.lost payload: %v", err)
					}
					if payload["requested_id"] != "resume-123" || payload["actual_id"] != "fresh-456" || payload["reason"] != acp.SessionLoadUnsupportedReason {
						t.Errorf("session.lost payload = %v", payload)
					}
				})
			}
		})
	}
}
