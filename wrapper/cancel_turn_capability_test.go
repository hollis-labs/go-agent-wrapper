package wrapper

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-providers/providertest"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/launch"
)

// CW-20261001-0200: the cancel_turn delivery capability an adapter from
// launch.Select advertises agrees with what Wrapper.CancelTurn does on its
// running session, for every native (runtime, mode) the wrapper launches.
// The runtimes with a native turn interrupt are named too, so the claim and
// the behavior cannot drift to "nothing" together.
func TestCancelTurnCapabilityMatchesCancelTurn(t *testing.T) {
	skipUnlessSh(t)
	wantCancel := map[launch.Key]bool{
		{Runtime: runtimes.Claude, Mode: runtimes.ModeStreamingStdio}: true,
		{Runtime: runtimes.Codex, Mode: runtimes.ModeJSONRPCStdio}:    true,
		{Runtime: runtimes.OpenCode, Mode: runtimes.ModeHTTPSSE}:      true,
	}
	// A fake that stays up: an interrupt with no turn open is either
	// answered at once (Codex: nothing to interrupt; OpenCode: abort) or
	// left unanswered (Claude), which CancelTurn's deadline ends.
	binaries := map[launch.Key]func(t *testing.T) string{
		{Runtime: runtimes.Claude, Mode: runtimes.ModeStreamingStdio}: func(t *testing.T) string {
			return providertest.New(t, runtimes.Claude, providertest.Script(providertest.RecvLine(), providertest.Hang())).Path
		},
		{Runtime: runtimes.Codex, Mode: runtimes.ModeJSONRPCStdio}: func(t *testing.T) string {
			return providertest.New(t, runtimes.Codex, providertest.Script(providertest.Hang())).Path
		},
		{Runtime: runtimes.OpenCode, Mode: runtimes.ModeHTTPSSE}: fakeOpenCodeServe,
	}
	native := 0
	for _, key := range launch.Supported() {
		if key.Mode.ACP() {
			continue
		}
		native++
		t.Run(key.String(), func(t *testing.T) {
			binary := providertest.New(t, key.Runtime).Path // per-turn: never spawned here
			if fake, ok := binaries[key]; ok {
				binary = fake(t)
			}
			sel := launch.Selection{Runtime: string(key.Runtime), Mode: key.Mode, Binary: binary}
			adapter, err := launch.Select(sel)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			claims := adapter.Describe().Delivery.Supports(adapters.DeliveryCapabilityCancelTurn)
			if claims != wantCancel[key] {
				t.Errorf("descriptor cancel_turn = %v, want %v", claims, wantCancel[key])
			}
			probeCancelTurn(t, adapter, func(t *testing.T, w *Wrapper, sink *capturingSink, _ *acp.Manager) {
				sink.waitFor(t, runtimeevents.KindSessionReady, 10*time.Second)
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()
				err := w.CancelTurn(ctx)
				unsupported := errors.Is(err, ErrTurnCancelUnsupported) || errors.Is(err, agentsessions.ErrInterruptUnsupported)
				if claims == unsupported {
					t.Errorf("descriptor cancel_turn = %v but CancelTurn returned %v", claims, err)
				}
			})
		})
	}
	if native != len(wantCancel)+4 {
		t.Errorf("checked %d native launches; update this test for the registry's", native)
	}
}

// Every ACP launch advertises cancel_turn: CancelTurn sends session/cancel.
// A running ACP session takes it without an error.
func TestCancelTurnCapabilityACP(t *testing.T) {
	skipUnlessSh(t)
	for _, key := range launch.Supported() {
		if !key.Mode.ACP() {
			continue
		}
		adapter, err := launch.Select(launch.Selection{Runtime: string(key.Runtime), Mode: key.Mode})
		if err != nil {
			t.Fatalf("Select(%s): %v", key, err)
		}
		if !adapter.Describe().Delivery.Supports(adapters.DeliveryCapabilityCancelTurn) {
			t.Errorf("%s: ACP descriptor does not advertise cancel_turn", key)
		}
	}
	fake := providertest.New(t, runtimes.Pi, providertest.Replay("pi/acp_turn"))
	fake.ExpectErrors() // stopped before the prompt its transcript waits for
	adapter, err := launch.Select(launch.Selection{Runtime: string(runtimes.Pi), Binary: fake.Path})
	if err != nil {
		t.Fatal(err)
	}
	probeCancelTurn(t, adapter,
		func(t *testing.T, w *Wrapper, _ *capturingSink, manager *acp.Manager) {
			waitForACPReady(t, manager, w.SessionID())
			if err := w.CancelTurn(context.Background()); err != nil {
				t.Errorf("CancelTurn on a ready ACP session: %v", err)
			}
		})
}

// probeCancelTurn runs adapter under a wrapper, calls probe once the session
// is up, and stops it. Unlike runSelected it asks nothing of the run itself:
// the fakes here are stopped before any turn, so how the process ends is not
// under test.
func probeCancelTurn(t *testing.T, adapter adapters.Adapter, probe func(*testing.T, *Wrapper, *capturingSink, *acp.Manager)) {
	t.Helper()
	sink := newCapturingSink()
	manager := acp.NewManager()
	w, err := New(Config{App: "cancel-turn-probe", Adapter: adapter, Activity: activity.NewBridge(sink), Workdir: t.TempDir(), ACPManager: manager})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(ctx) }()
	probe(t, w, sink, manager)
	_ = w.Stop(context.Background())
	select {
	case <-runDone:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
}

// fakeOpenCodeServe starts an httptest OpenCode server (health, session
// create, event stream, abort, dispose) and returns a launcher that prints
// its address the way `opencode serve` does and then waits.
func fakeOpenCodeServe(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"healthy":true}`))
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ses_cancel"}`))
	})
	mux.HandleFunc("/session/ses_cancel/abort", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`true`))
	})
	mux.HandleFunc("/global/dispose", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`true`))
	})
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	body := fmt.Sprintf("#!/bin/sh\nprintf 'opencode server listening on %s\\n'\ntrap 'exit 0' TERM INT\nwhile true; do sleep 1; done\n", server.URL)
	return writeShellFixtureLauncher(t, t.TempDir(), "fake-opencode-serve", []byte(body))
}
