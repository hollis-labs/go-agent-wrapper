package wrapper

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/agentlaunch/launcher"
	"github.com/hollis-labs/agentkit/agentlaunch/providerplant"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-providers/providertest"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"

	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
)

// nativeTestAdapter runs a real go-providers CLIAdapter through the wrapper's
// subprocess-per-turn shape.
type nativeTestAdapter struct{ cli provider.CLIAdapter }

func (a *nativeTestAdapter) Name() string { return a.cli.Name() }
func (a *nativeTestAdapter) Describe() adapters.Descriptor {
	return adapters.Descriptor{
		Provider:  "prepared-test",
		Interrupt: adapters.InterruptProcess,
		Channels:  []runtimeevents.SourceChannel{runtimeevents.ChannelStdio},
	}
}
func (a *nativeTestAdapter) Resolve(rc adapters.ResolveContext) (adapters.Spec, error) {
	return adapters.Spec{Binary: "unused", Cwd: rc.Cwd}, nil
}
func (a *nativeTestAdapter) CLIAdapter() provider.CLIAdapter { return a.cli }

// preparedClaudePerTurn prepares a Claude subprocess-per-turn launch whose
// binary is binary, the way an app would: compile, Prepare, PrepareExecution.
func preparedClaudePerTurn(t *testing.T, binary string) *agentlaunch.PreparedExecution {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	plan := agentlaunch.LaunchPlan{
		Project:   agentlaunch.ProjectSpec{ID: "proj", Name: "Project", Root: t.TempDir()},
		Agent:     agentlaunch.AgentSpec{ID: "agent-id", Name: "agent-name"},
		Provider:  agentlaunch.ProviderSpec{ID: "claude", Binary: binary},
		Runtime:   runtimes.ModeSubprocessPerTurn,
		Workspace: agentlaunch.WorkspaceSpec{Mode: agentlaunch.WorkspaceTemp, TempPrefix: t.TempDir()},
		BootProfile: agentlaunch.BootProfileRef{Inline: &agentlaunch.BootProfileInline{
			BootPrompt: "PERSONA-PROMPT", BootContent: "TASK-KICKOFF", BootMode: agentlaunch.BootModePlanted,
		}},
		Mode: agentlaunch.LaunchInteractive,
	}
	compiled, err := launcher.Compile(context.Background(), plan)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	prepared, err := launcher.Prepare(context.Background(), compiled)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	exec, err := providerplant.PrepareExecution(context.Background(), prepared)
	if err != nil {
		t.Fatalf("PrepareExecution: %v", err)
	}
	exec.Access = agentlaunch.AccessRequirements{Mode: agentlaunch.AccessOptional}
	return exec
}

// The wrapper's prepared path used to rerun the first turn's frozen argv
// (boot prompt, no --resume) every turn. With agentkit's launch template each
// turn carries its own prompt, and the next turn resumes the session the
// first reported.
func TestRunPreparedExecution_EachTurnResolvesItsOwnArgv(t *testing.T) {
	fake := providertest.New(t, runtimes.Claude,
		providertest.Replay("claude/print_turn1"),
		providertest.Replay("claude/print_turn2_resume"),
	)
	exec := preparedClaudePerTurn(t, fake.Path)
	if exec.Bindings.Launch == nil {
		t.Fatal("prepared execution carries no launch template")
	}

	sink := newCapturingSink()
	var mu sync.Mutex
	var reported []string
	w, err := New(Config{
		App:               "test-prepared-turns",
		Adapter:           &nativeTestAdapter{cli: provider.NewClaudeAdapter()},
		Activity:          activity.NewBridge(sink),
		Workdir:           exec.Bindings.CWD,
		PreparedExecution: exec,
		OnSessionID: func(id string) {
			mu.Lock()
			reported = append(reported, id)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- w.Run(ctx) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)

	for _, prompt := range []string{"first turn", "second turn"} {
		if err := w.SendInput(context.Background(), []byte(prompt)); err != nil {
			t.Fatalf("SendInput(%q): %v", prompt, err)
		}
	}
	_ = w.Stop(context.Background())
	<-runErr

	calls := fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("fake ran %d times, want 2: %+v", len(calls), calls)
	}
	var sid string
	for _, l := range providertest.FixtureLines(t, "claude/print_turn1.jsonl") {
		var ev struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(l, &ev) == nil && ev.SessionID != "" {
			sid = ev.SessionID
			break
		}
	}
	for i, want := range []struct{ prompt, resume string }{{"first turn", ""}, {"second turn", sid}} {
		c := calls[i]
		if got, _ := c.ArgAfter("-p"); got != want.prompt {
			t.Errorf("turn %d: -p %q, want %q: %q", i+1, got, want.prompt, c.Args)
		}
		if got, _ := c.ArgAfter("--resume"); got != want.resume {
			t.Errorf("turn %d: --resume %q, want %q: %q", i+1, got, want.resume, c.Args)
		}
		if c.HasArg("TASK-KICKOFF") {
			t.Errorf("turn %d: the boot kickoff was replayed: %q", i+1, c.Args)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) == 0 || reported[0] != sid {
		t.Errorf("OnSessionID reported %q, want %q first", reported, sid)
	}
}

// Wrapping an adapter for a prepared execution must not hide the optional
// interfaces the session runtimes consult.
func TestPreparedAdapterForwardsOptionalInterfaces(t *testing.T) {
	exec := &agentlaunch.PreparedExecution{Bindings: agentlaunch.ExecutionBindings{Argv: []string{"/bin/agy"}, CWD: t.TempDir()}}
	agy, err := preparedCLIAdapter(provider.NewAntigravityAdapter(), exec)
	if err != nil {
		t.Fatal(err)
	}
	lostStderr := providertest.ReadFixture(t, "antigravity/print_resume_unknown_id.stderr")
	if c, ok := agy.(provider.SessionLostClassifier); !ok || !c.IsSessionLost(lostStderr) {
		t.Error("SessionLostClassifier not forwarded")
	}
	if v, ok := agy.(provider.SessionResumeVerifier); !ok || !v.ResumeKeepsSessionID() {
		t.Error("SessionResumeVerifier not forwarded")
	}
	if c, ok := agy.(provider.AuthFailureClassifier); !ok || !c.IsNotAuthenticated([]byte("error: authentication failed or timed out")) {
		t.Error("AuthFailureClassifier not forwarded")
	}
	if p, ok := agy.(provider.EventParser); !ok {
		t.Error("EventParser not forwarded")
	} else if evs, perr := p.ParseLineEvents(providertest.FixtureLines(t, "antigravity/print_turn1.jsonl")[0]); perr != nil || len(evs) == 0 {
		t.Errorf("ParseLineEvents = %v, %v", evs, perr)
	}
	if _, ok := agy.(provider.BootDirProvider); ok {
		t.Error("BootDirProvider forwarded; a prepared execution is already planted")
	}
	if bin, ok := agy.Detect(); !ok || bin != "/bin/agy" {
		t.Errorf("Detect = %q, %v; want the prepared binary", bin, ok)
	}

	// An inner adapter without the interfaces answers as one would.
	bare, err := preparedCLIAdapter(&fakeCLI{name: "bare", script: "/bin/true"}, exec)
	if err != nil {
		t.Fatal(err)
	}
	if bare.(provider.SessionLostClassifier).IsSessionLost(lostStderr) ||
		bare.(provider.SessionResumeVerifier).ResumeKeepsSessionID() ||
		bare.(provider.AuthFailureClassifier).IsNotAuthenticated([]byte("authentication failed")) ||
		bare.(provider.Preflighter).Preflight() != nil {
		t.Error("a bare inner adapter must get the neutral answers")
	}
}
