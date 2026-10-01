package wrapper

import (
	"context"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/go-providers/providertest"
	"github.com/hollis-labs/go-providers/registry"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/launch"
)

// Acceptance for CW-20260930-0134: every runtime in the go-providers registry
// launches through launch.Select + New + Run, in its default mode, against a
// go-providers providertest fake that replays what the real CLI writes. ACP
// is selectable for the runtimes with a native default, and Copilot also
// runs over TCP elsewhere (TestACPWrapperCopilotTCPRealSubprocessLifecycle).
func TestLaunchEveryRegistryRuntimeThroughSelect(t *testing.T) {
	skipUnlessSh(t)
	cases := map[runtimes.ID]struct {
		runs  []providertest.Run
		drive func(t *testing.T, w *Wrapper, sink *capturingSink, manager *acp.Manager)
		cfg   func(*Config)
		check func(t *testing.T, fake *providertest.Fake)
	}{
		// streaming-stdio: two turns on one process.
		runtimes.Claude: {
			runs: []providertest.Run{providertest.Replay("claude/stream_two_turns")},
			drive: func(t *testing.T, w *Wrapper, sink *capturingSink, _ *acp.Manager) {
				sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
				for _, text := range []string{"say hi", "say bye"} {
					if err := w.SendInput(context.Background(), []byte(`{"type":"user","message":{"role":"user","content":"`+text+`"}}`)); err != nil {
						t.Fatalf("SendInput: %v", err)
					}
					sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
				}
			},
		},
		// jsonrpc-stdio (app-server, D-74): the wrapper spawns it and
		// delivers the first-turn payload; the Codex thread protocol
		// belongs to agentkit's turn package, driven by the host.
		runtimes.Codex: {
			runs: []providertest.Run{providertest.Script(providertest.RecvLine(), providertest.Exit(0)).When("app-server")},
			cfg: func(c *Config) {
				c.AutoFireFirstTurn = true
				c.FirstTurnPayload = "kickoff payload"
			},
			check: func(t *testing.T, fake *providertest.Fake) {
				if call := fake.Call(0); len(call.Stdin) != 1 || call.Stdin[0] != "kickoff payload" {
					t.Errorf("codex stdin = %q, want the first-turn payload", call.Stdin)
				}
			},
		},
		runtimes.OpenCode: {
			runs:  []providertest.Run{providertest.Replay("opencode/run_turn1").When("run")},
			drive: drivePerTurn,
		},
		runtimes.Antigravity: {
			runs:  []providertest.Run{providertest.Replay("antigravity/print_turn1")},
			drive: drivePerTurn,
		},
		runtimes.Copilot: {
			runs:  []providertest.Run{providertest.Replay("copilot/acp_turn").When("--acp")},
			drive: driveACP,
		},
		runtimes.Pi: {
			runs:  []providertest.Run{providertest.Replay("pi/acp_turn")},
			drive: driveACP,
		},
	}
	all := registry.All()
	if len(all) != len(cases) {
		t.Fatalf("registry has %d runtimes, test covers %d", len(all), len(cases))
	}
	for _, d := range all {
		tc, ok := cases[d.ID]
		if !ok {
			t.Fatalf("no case for registry runtime %s", d.ID)
		}
		t.Run(string(d.ID)+"/"+string(d.DefaultMode), func(t *testing.T) {
			fake := providertest.New(t, d.ID, tc.runs...)
			runSelected(t, launch.Selection{Runtime: string(d.ID), Binary: fake.Path}, tc.cfg, tc.drive)
			requireCleanFakeRun(t, fake)
			if tc.check != nil {
				tc.check(t, fake)
			}
		})
	}

	// ACP on request for the runtimes whose default is native. The
	// transcript is the generic ACP turn (pi's capture); what is under
	// test is that Select's ACP factory launches it for this runtime.
	for _, id := range []runtimes.ID{runtimes.Claude, runtimes.Codex, runtimes.OpenCode} {
		t.Run(string(id)+"/acp-stdio", func(t *testing.T) {
			fake := providertest.New(t, id, providertest.Replay("pi/acp_turn"))
			runSelected(t, launch.Selection{Runtime: string(id), Mode: runtimes.ModeACPStdio, Binary: fake.Path}, nil, driveACP)
			requireCleanFakeRun(t, fake)
		})
	}
}

// runSelected selects, builds and runs a wrapper, drives it and stops it,
// requiring Run to return nil.
func runSelected(t *testing.T, sel launch.Selection, cfg func(*Config), drive func(*testing.T, *Wrapper, *capturingSink, *acp.Manager)) {
	t.Helper()
	adapter, err := launch.Select(sel)
	if err != nil {
		t.Fatalf("Select(%+v): %v", sel, err)
	}
	sink := newCapturingSink()
	manager := acp.NewManager()
	c := Config{
		App:        "launch-" + sel.Runtime,
		Adapter:    adapter,
		Activity:   activity.NewBridge(sink),
		Workdir:    t.TempDir(),
		ACPManager: manager,
	}
	if cfg != nil {
		cfg(&c)
	}
	w, err := New(c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(ctx) }()
	if drive != nil {
		drive(t, w, sink, manager)
		if err := w.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
	events := sink.snapshot()
	if !hasKind(events, runtimeevents.KindProcessExited) {
		t.Errorf("no process.exited event: %v", sink.kinds())
	}
	if drive != nil && !hasKind(events, runtimeevents.KindAgentDelta) {
		t.Errorf("no agent text reached the event stream: %v", sink.kinds())
	}
}

// requireCleanFakeRun requires one invocation of the fake that ran its
// script to the end and exited 0. Fake-side errors (an unexpected frame, no
// run left) already fail the test at cleanup.
func requireCleanFakeRun(t *testing.T, fake *providertest.Fake) {
	t.Helper()
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("fake %s invoked %d times, want 1", fake.Runtime, len(calls))
	}
	if c := calls[0]; !c.Exited || c.ExitCode != 0 || len(c.Errors) > 0 {
		t.Fatalf("fake %s call: exited=%v code=%d errors=%v args=%q", fake.Runtime, c.Exited, c.ExitCode, c.Errors, c.Args)
	}
}

func drivePerTurn(t *testing.T, w *Wrapper, sink *capturingSink, _ *acp.Manager) {
	t.Helper()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	if err := w.SendInput(context.Background(), []byte("say hi")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
}

func driveACP(t *testing.T, w *Wrapper, sink *capturingSink, manager *acp.Manager) {
	t.Helper()
	waitForACPReady(t, manager, w.SessionID())
	if err := w.SendInput(context.Background(), []byte("say hi")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	sink.waitFor(t, runtimeevents.KindAgentDelta, 5*time.Second)
	waitForACPReady(t, manager, w.SessionID())
}
