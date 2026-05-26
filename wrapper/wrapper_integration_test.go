package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider"

	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/classifybridge"
	"github.com/hollis-labs/go-agent-wrapper/filters"
	"github.com/hollis-labs/go-agent-wrapper/plant"
	"github.com/hollis-labs/go-agent-wrapper/policy"
	"github.com/hollis-labs/go-agent-wrapper/sandbox"
	"github.com/hollis-labs/go-harness-filters/classify"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// ---------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------

// fakeCLI implements provider.CLIAdapter, wrapping a shell script that
// emits one line per StreamEvent. Same shape as agentkit's own
// echoAdapter used in agentsessions tests.
type fakeCLI struct {
	name   string
	script string
}

func (f *fakeCLI) Name() string                                           { return f.name }
func (f *fakeCLI) BuildArgs(prompt, sysPrompt, sessionID string) []string { return nil }

func (f *fakeCLI) ParseLine(line []byte) ([]llmtypes.StreamEvent, error) {
	s := strings.TrimRight(string(line), "\r\n")
	switch {
	case strings.HasPrefix(s, "delta:"):
		return []llmtypes.StreamEvent{{
			Type:    llmtypes.EventDelta,
			Content: strings.TrimPrefix(s, "delta:"),
		}}, nil
	case strings.HasPrefix(s, "session:"):
		return []llmtypes.StreamEvent{{
			Type:      llmtypes.EventSessionID,
			SessionID: strings.TrimPrefix(s, "session:"),
		}}, nil
	case strings.HasPrefix(s, "tool_use:"):
		// Format: "tool_use:<JSON of llmtypes.ToolUseBlock>"
		var tu llmtypes.ToolUseBlock
		if err := json.Unmarshal([]byte(strings.TrimPrefix(s, "tool_use:")), &tu); err != nil {
			return nil, nil
		}
		return []llmtypes.StreamEvent{{
			Type:    llmtypes.EventToolUse,
			ToolUse: &tu,
		}}, nil
	case s == "done":
		return []llmtypes.StreamEvent{{Type: llmtypes.EventDone}}, nil
	}
	return nil, nil
}

func (f *fakeCLI) Detect() (string, bool) { return f.script, f.script != "" }

// fakeRuntimeAdapter implements the wrapper's adapters.RuntimeAdapter
// by wrapping a fakeCLI.
type fakeRuntimeAdapter struct {
	cli *fakeCLI
}

func (a *fakeRuntimeAdapter) Name() string { return a.cli.name }

func (a *fakeRuntimeAdapter) Describe() adapters.Descriptor {
	return adapters.Descriptor{
		Provider: a.cli.name,
		Runtime:  RuntimeAdapter, // subprocess-per-turn — script runs to completion
		Channels: []runtimeevents.SourceChannel{runtimeevents.ChannelStdio},
	}
}

func (a *fakeRuntimeAdapter) Resolve(rc adapters.ResolveContext) (adapters.Spec, error) {
	return adapters.Spec{Binary: a.cli.script, Cwd: rc.Cwd}, nil
}

func (a *fakeRuntimeAdapter) CLIAdapter() provider.CLIAdapter { return a.cli }

// capturingSink records every emitted Event for assertion and signals
// kinds on a buffered channel so the test can wait for specific events
// without polling.
type capturingSink struct {
	mu     sync.Mutex
	events []runtimeevents.Event
	sigCh  chan runtimeevents.EventKind
}

func newCapturingSink() *capturingSink {
	return &capturingSink{sigCh: make(chan runtimeevents.EventKind, 128)}
}

func (c *capturingSink) Write(_ context.Context, ev runtimeevents.Event) error {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
	select {
	case c.sigCh <- ev.Kind:
	default:
	}
	return nil
}

func (c *capturingSink) snapshot() []runtimeevents.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]runtimeevents.Event, len(c.events))
	copy(out, c.events)
	return out
}

func (c *capturingSink) kinds() []runtimeevents.EventKind {
	evs := c.snapshot()
	out := make([]runtimeevents.EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

// waitFor blocks until an event of the given kind appears (or d
// elapses). Drains skipped kinds without consuming them — sigCh is
// best-effort signaling, the real source of truth is snapshot().
func (c *capturingSink) waitFor(t *testing.T, kind runtimeevents.EventKind, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		if hasKind(c.snapshot(), kind) {
			return
		}
		select {
		case <-c.sigCh:
			// loop and re-check snapshot
		case <-deadline:
			t.Fatalf("timed out waiting for %q; sink saw: %v", kind, c.kinds())
		}
	}
}

func hasKind(evs []runtimeevents.Event, kind runtimeevents.EventKind) bool {
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// recordingPlanter records every Plant call and returns a configurable
// PlantedFiles list (plus optional error).
type recordingPlanter struct {
	mu       sync.Mutex
	calls    int
	lastDir  string
	lastSpec plant.Spec
	files    []string
	err      error
}

func (p *recordingPlanter) Plant(_ context.Context, bootDir string, spec plant.Spec) (plant.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.lastDir = bootDir
	p.lastSpec = spec
	return plant.Result{PlantedFiles: p.files}, p.err
}

// recordingPolicy records every Decide call and returns a configurable
// Decision + optional error.
type recordingPolicy struct {
	mu       sync.Mutex
	calls    int
	requests []policy.Request
	decision policy.Decision
	err      error
}

func (p *recordingPolicy) Decide(_ context.Context, req policy.Request) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.requests = append(p.requests, req)
	return p.decision, p.err
}

// recordingApplier records every Apply call and returns a configurable
// Result (plus optional error).
type recordingApplier struct {
	mu      sync.Mutex
	calls   int
	lastPID int
	result  sandbox.Result
	err     error
}

type replacingFilter struct {
	mu    sync.Mutex
	kinds []string
	from  string
	to    string
}

func (f *replacingFilter) Process(_ context.Context, in filters.Input) (filters.Output, error) {
	f.mu.Lock()
	f.kinds = append(f.kinds, in.Kind)
	f.mu.Unlock()
	if strings.Contains(string(in.Content), f.from) {
		return filters.Output{Repaired: []byte(strings.ReplaceAll(string(in.Content), f.from, f.to))}, nil
	}
	return filters.Output{}, nil
}

func (a *recordingApplier) Apply(_ context.Context, pid int) (sandbox.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.lastPID = pid
	return a.result, a.err
}

// writeFakeScript drops a tiny sh script that emits one line per entry
// then exits 0. Test inputs must not contain single quotes.
func writeFakeScript(t *testing.T, dir string, lines []string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake script needs sh; not running on Windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available on PATH")
	}
	body := "#!/bin/sh\n"
	for _, l := range lines {
		if strings.ContainsRune(l, '\'') {
			t.Fatalf("test line %q contains a single quote — pick another fixture", l)
		}
		body += "printf '%s\\n' '" + l + "'\n"
	}
	body += "exit 0\n"
	path := filepath.Join(dir, "fake-cli.sh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------

func TestRunEndToEndAdapterRuntime(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{
		"session:ses_provider_abc",
		"delta:hello",
		"delta:world",
		"done",
	})

	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	bridge := activity.NewBridge(sink)

	w, err := New(Config{
		App:      "test-integration",
		Adapter:  adapter,
		Activity: bridge,
		Workdir:  dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()

	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)

	if err := w.SendInput(context.Background(), []byte("ignored")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}

	// turn.completed fires when the script's "done" line is parsed.
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)

	// Stop the session so Run unblocks.
	if err := w.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}

	select {
	case <-runErrCh:
		// Run may return nil or an error (Wait error from Stop is ok).
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Stop")
	}

	// ----- assertions on the recorded event stream -----

	evs := sink.snapshot()
	if len(evs) == 0 {
		t.Fatal("sink recorded no events")
	}

	var deltaContents []string
	var sawReady, sawTurnDone, sawExited bool
	var providerSessionAfterID string
	var seqs []uint64
	for _, ev := range evs {
		seqs = append(seqs, ev.Sequence)
		switch ev.Kind {
		case runtimeevents.KindSessionReady:
			sawReady = true
		case runtimeevents.KindAgentDelta:
			var p map[string]any
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Errorf("decode delta payload: %v", err)
				continue
			}
			if c, ok := p["content"].(string); ok {
				deltaContents = append(deltaContents, c)
			}
		case runtimeevents.KindTurnCompleted:
			sawTurnDone = true
		case runtimeevents.KindProcessExited:
			sawExited = true
		}
		// Track the most-recent ProviderSessionID — the translator
		// rebinds it on the Bridge when EventSessionID fires.
		if ev.Process.ProviderSessionID != "" {
			providerSessionAfterID = ev.Process.ProviderSessionID
		}
	}

	if !sawReady {
		t.Error("missing session.ready event")
	}
	if got := strings.Join(deltaContents, ","); got != "hello,world" {
		t.Errorf("delta contents = %q, want hello,world", got)
	}
	if !sawTurnDone {
		t.Error("missing turn.completed event")
	}
	if !sawExited {
		t.Error("missing process.exited event")
	}
	if providerSessionAfterID != "ses_provider_abc" {
		t.Errorf("ProviderSessionID propagation: got %q, want ses_provider_abc",
			providerSessionAfterID)
	}

	// Sequence numbers must be strictly increasing per session.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Errorf("sequence not monotonic at index %d: %d -> %d (full: %v)",
				i, seqs[i-1], seqs[i], seqs)
			break
		}
	}

	// All events share the same wrapper session ID.
	for i, ev := range evs {
		if ev.SessionID != w.SessionID() {
			t.Errorf("event %d SessionID = %q, want %q", i, ev.SessionID, w.SessionID())
		}
	}

	// All events share the same App identity.
	for i, ev := range evs {
		if ev.App != "test-integration" {
			t.Errorf("event %d App = %q, want test-integration", i, ev.App)
		}
	}
}

func TestRunCtxCancelStopsSession(t *testing.T) {
	dir := t.TempDir()
	// Script that emits one delta then loops on stdin forever — only
	// ctx cancel or Stop will end it.
	script := writeFakeScript(t, dir, []string{
		"delta:about-to-block",
	})
	// Patch the script to add a tail that reads stdin (blocks until EOF).
	scriptBody, _ := os.ReadFile(script)
	scriptBody = append(scriptBody[:len(scriptBody)-len("exit 0\n")], []byte("cat > /dev/null\nexit 0\n")...)
	if err := os.WriteFile(script, scriptBody, 0o755); err != nil {
		t.Fatalf("rewrite script: %v", err)
	}

	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, err := New(Config{
		App:      "test-cancel",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()

	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)

	if err := w.SendInput(context.Background(), []byte("trigger")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	sink.waitFor(t, runtimeevents.KindAgentDelta, 5*time.Second)

	// Cancelling ctx should propagate Stop to the session and unblock
	// Run.
	cancel()

	select {
	case <-runErrCh:
		// Either nil or ctx.Err() — both acceptable.
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if !hasKind(sink.snapshot(), runtimeevents.KindProcessExited) {
		t.Error("expected process.exited event after ctx cancel")
	}
}

// TestRunInvokesPlanter wires a recordingPlanter into Config and
// verifies that Run calls it with the resolved boot dir + spec, and
// emits plant.started + plant.completed events around the call.
func TestRunInvokesPlanter(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{"done"})

	planter := &recordingPlanter{files: []string{"/tmp/wrapper-boot/.mcp.json"}}
	spec := plant.Spec{
		Files: map[string][]byte{"hello.txt": []byte("hi")},
		Hooks: []plant.Hook{{Provider: "claude", Name: "PreToolUse", Payload: []byte("{}")}},
	}

	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, err := New(Config{
		App:       "test-plant",
		Adapter:   adapter,
		Activity:  activity.NewBridge(sink),
		Workdir:   dir,
		Planter:   planter,
		PlantSpec: spec,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()

	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	if err := w.SendInput(context.Background(), []byte("ignored")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
	_ = w.Stop(context.Background())
	<-runErrCh

	// Planter called exactly once with the auto-resolved boot dir.
	planter.mu.Lock()
	defer planter.mu.Unlock()
	if planter.calls != 1 {
		t.Errorf("planter.calls = %d, want 1", planter.calls)
	}
	wantDir := filepath.Join(dir, ".wrapper-boot", w.SessionID())
	if planter.lastDir != wantDir {
		t.Errorf("planter.lastDir = %q, want %q", planter.lastDir, wantDir)
	}
	if _, err := os.Stat(wantDir); err != nil {
		t.Errorf("boot dir %q was not created: %v", wantDir, err)
	}
	if len(planter.lastSpec.Files) != 1 || string(planter.lastSpec.Files["hello.txt"]) != "hi" {
		t.Errorf("planter received unexpected spec.Files: %#v", planter.lastSpec.Files)
	}
	if len(planter.lastSpec.Hooks) != 1 {
		t.Errorf("planter received %d hooks, want 1", len(planter.lastSpec.Hooks))
	}

	// plant.started fires before plant.completed; both before session.ready.
	evs := sink.snapshot()
	idxStart := indexOfKind(evs, runtimeevents.KindPlantStarted)
	idxDone := indexOfKind(evs, runtimeevents.KindPlantCompleted)
	idxReady := indexOfKind(evs, runtimeevents.KindSessionReady)
	if idxStart < 0 || idxDone < 0 || idxReady < 0 {
		t.Fatalf("missing plant/session events: started=%d done=%d ready=%d", idxStart, idxDone, idxReady)
	}
	if !(idxStart < idxDone && idxDone < idxReady) {
		t.Errorf("event order wrong: plant.started=%d plant.completed=%d session.ready=%d", idxStart, idxDone, idxReady)
	}

	// plant.completed payload includes the planter's PlantedFiles result.
	var donePayload map[string]any
	if err := json.Unmarshal(evs[idxDone].Payload, &donePayload); err != nil {
		t.Fatalf("decode plant.completed payload: %v", err)
	}
	if got, _ := donePayload["boot_dir"].(string); got != wantDir {
		t.Errorf("plant.completed boot_dir = %q, want %q", got, wantDir)
	}
	files, _ := donePayload["planted_files"].([]any)
	if len(files) != 1 || files[0].(string) != "/tmp/wrapper-boot/.mcp.json" {
		t.Errorf("plant.completed planted_files = %v, want [/tmp/wrapper-boot/.mcp.json]", files)
	}
}

// TestRunPlanterErrorAborts verifies that a Planter error stops Run
// before the agentkit runtime is constructed, surfaces the error to
// the caller, and emits plant.completed with the error payload.
func TestRunPlanterErrorAborts(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{"done"})

	plantErr := errors.New("disk full")
	planter := &recordingPlanter{err: plantErr}

	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, err := New(Config{
		App:      "test-plant-err",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
		Planter:  planter,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = w.Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil; want plant error")
	}
	if !errors.Is(err, plantErr) {
		t.Errorf("Run err = %v; want errors.Is(err, plantErr)", err)
	}

	evs := sink.snapshot()
	if hasKind(evs, runtimeevents.KindSessionReady) {
		t.Error("session.ready emitted despite plant error — agentkit runtime should not have started")
	}
	idxDone := indexOfKind(evs, runtimeevents.KindPlantCompleted)
	if idxDone < 0 {
		t.Fatal("plant.completed not emitted on planter error")
	}
	var p map[string]any
	if err := json.Unmarshal(evs[idxDone].Payload, &p); err != nil {
		t.Fatalf("decode plant.completed: %v", err)
	}
	if errStr, _ := p["error"].(string); errStr != "disk full" {
		t.Errorf("plant.completed error = %q, want %q", errStr, "disk full")
	}
}

// TestRunInvokesSandbox wires a recordingApplier into Config and
// verifies Run calls Apply with the session's PID after Start, and
// emits sandbox.applied with the result.
func TestRunInvokesSandbox(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{"done"})

	applier := &recordingApplier{
		result: sandbox.Result{
			Profile: "nanite-cli",
			Applied: true,
			Notes:   []string{"applied via sandbox-exec"},
		},
	}

	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, err := New(Config{
		App:      "test-sandbox",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
		Sandbox:  applier,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()

	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	sink.waitFor(t, runtimeevents.KindSandboxApplied, 5*time.Second)
	if err := w.SendInput(context.Background(), []byte("ignored")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
	_ = w.Stop(context.Background())
	<-runErrCh

	applier.mu.Lock()
	defer applier.mu.Unlock()
	if applier.calls != 1 {
		t.Errorf("applier.calls = %d, want 1", applier.calls)
	}

	evs := sink.snapshot()
	idxReady := indexOfKind(evs, runtimeevents.KindSessionReady)
	idxSandbox := indexOfKind(evs, runtimeevents.KindSandboxApplied)
	if idxReady < 0 || idxSandbox < 0 {
		t.Fatalf("missing events: ready=%d sandbox=%d", idxReady, idxSandbox)
	}
	if !(idxReady < idxSandbox) {
		t.Errorf("sandbox.applied (idx %d) should fire after session.ready (idx %d)", idxSandbox, idxReady)
	}

	var p map[string]any
	if err := json.Unmarshal(evs[idxSandbox].Payload, &p); err != nil {
		t.Fatalf("decode sandbox.applied: %v", err)
	}
	if got, _ := p["profile"].(string); got != "nanite-cli" {
		t.Errorf("sandbox.applied profile = %q, want nanite-cli", got)
	}
	if got, _ := p["applied"].(bool); !got {
		t.Errorf("sandbox.applied applied = %v, want true", got)
	}
	notes, _ := p["notes"].([]any)
	if len(notes) != 1 || notes[0].(string) != "applied via sandbox-exec" {
		t.Errorf("sandbox.applied notes = %v", notes)
	}
}

// TestRunSandboxErrorStopsSession verifies that an Applier error
// stops the session and surfaces the error from Run.
func TestRunSandboxErrorStopsSession(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{"done"})

	sbErr := errors.New("seccomp filter rejected")
	applier := &recordingApplier{
		result: sandbox.Result{Profile: "strict", Applied: false},
		err:    sbErr,
	}

	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, err := New(Config{
		App:      "test-sandbox-err",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
		Sandbox:  applier,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = w.Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil; want sandbox error")
	}
	if !errors.Is(err, sbErr) {
		t.Errorf("Run err = %v; want errors.Is(err, sbErr)", err)
	}

	evs := sink.snapshot()
	idxSandbox := indexOfKind(evs, runtimeevents.KindSandboxApplied)
	if idxSandbox < 0 {
		t.Fatal("sandbox.applied not emitted on applier error")
	}
	var p map[string]any
	if err := json.Unmarshal(evs[idxSandbox].Payload, &p); err != nil {
		t.Fatalf("decode sandbox.applied: %v", err)
	}
	if errStr, _ := p["error"].(string); errStr != "seccomp filter rejected" {
		t.Errorf("sandbox.applied error = %q, want %q", errStr, "seccomp filter rejected")
	}
}

func TestRunFiltersAgentDeltaAndStdout(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{
		"delta:unsafe text",
		"done",
	})

	filter := &replacingFilter{from: "unsafe", to: "safe"}
	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, err := New(Config{
		App:      "test-filter",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
		Filters:  filter,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	_ = w.SendInput(context.Background(), []byte("trigger"))
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
	_ = w.Stop(context.Background())
	<-runErrCh

	var sawFilteredDelta, sawFilteredStdout bool
	for _, ev := range sink.snapshot() {
		var p map[string]any
		_ = json.Unmarshal(ev.Payload, &p)
		switch ev.Kind {
		case runtimeevents.KindAgentDelta:
			if got, _ := p["content"].(string); got == "safe text" {
				sawFilteredDelta = true
			}
		case runtimeevents.KindStdoutRaw, runtimeevents.KindStdoutLine:
			raw := ""
			if s, _ := p["bytes"].(string); s != "" {
				raw = s
			}
			if s, _ := p["line"].(string); s != "" {
				raw = s
			}
			if strings.Contains(raw, "safe text") {
				sawFilteredStdout = true
			}
		}
	}
	if !sawFilteredDelta {
		t.Fatalf("agent.delta was not filtered; kinds: %v", kindList(sink.snapshot()))
	}
	if !sawFilteredStdout {
		t.Fatalf("stdout events were not filtered; kinds: %v", kindList(sink.snapshot()))
	}

	filter.mu.Lock()
	defer filter.mu.Unlock()
	if !containsString(filter.kinds, "agent_text") {
		t.Errorf("filter kinds = %v, want agent_text", filter.kinds)
	}
	if !containsString(filter.kinds, "command_output") {
		t.Errorf("filter kinds = %v, want command_output", filter.kinds)
	}
}

func TestRunEmitsSessionProcessingIdleAndHeartbeat(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{"delta:hello", "done"})
	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, err := New(Config{
		App:               "test-session-state",
		Adapter:           adapter,
		Activity:          activity.NewBridge(sink),
		Workdir:           dir,
		HeartbeatInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	sink.waitFor(t, runtimeevents.KindSessionHeartbeat, 5*time.Second)
	_ = w.SendInput(context.Background(), []byte("trigger"))
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
	_ = w.Stop(context.Background())
	<-runErrCh

	evs := sink.snapshot()
	idxProcessing := indexOfKind(evs, runtimeevents.KindSessionProcessing)
	idxDelta := indexOfKind(evs, runtimeevents.KindAgentDelta)
	idxDone := indexOfKind(evs, runtimeevents.KindTurnCompleted)
	idxIdle := indexOfKind(evs, runtimeevents.KindSessionIdle)
	if idxProcessing < 0 || idxDelta < 0 || idxDone < 0 || idxIdle < 0 {
		t.Fatalf("missing session state events: processing=%d delta=%d done=%d idle=%d kinds=%v",
			idxProcessing, idxDelta, idxDone, idxIdle, kindList(evs))
	}
	if !(idxProcessing < idxDelta && idxDone < idxIdle) {
		t.Errorf("session state order wrong: processing=%d delta=%d done=%d idle=%d",
			idxProcessing, idxDelta, idxDone, idxIdle)
	}
	if evs[idxProcessing].TurnID == "" || evs[idxIdle].TurnID != evs[idxProcessing].TurnID {
		t.Errorf("session state TurnID mismatch: processing=%q idle=%q",
			evs[idxProcessing].TurnID, evs[idxIdle].TurnID)
	}
}

func indexOfKind(evs []runtimeevents.Event, kind runtimeevents.EventKind) int {
	for i, e := range evs {
		if e.Kind == kind {
			return i
		}
	}
	return -1
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// toolUseScriptLine returns the printf-safe line the fake script
// emits to trigger an EventToolUse with the given tool name + input.
func toolUseScriptLine(t *testing.T, name string, input map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(llmtypes.ToolUseBlock{
		ID:    "tool_test",
		Name:  name,
		Input: input,
	})
	if err != nil {
		t.Fatalf("marshal tool use: %v", err)
	}
	return "tool_use:" + string(raw)
}

// runWrapperUntilToolUsePolicy starts the wrapper, sends one input,
// waits for the tool_use + policy event pair (or just tool_use if
// observeOnly), stops, and returns the recorded events.
func runWrapperUntilToolUsePolicy(t *testing.T, dir, scriptName string, pol policy.Engine, observeOnly bool) (*Wrapper, []runtimeevents.Event) {
	t.Helper()
	script := writeFakeScript(t, dir, []string{
		toolUseScriptLine(t, scriptName, map[string]any{"path": "/tmp/x"}),
		"done",
	})
	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, err := New(Config{
		App:      "test-policy",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
		Policy:   pol,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()

	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	if err := w.SendInput(context.Background(), []byte("trigger")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	sink.waitFor(t, runtimeevents.KindAgentToolUse, 5*time.Second)
	if !observeOnly {
		// Brief settle so the policy.* event lands in the sink
		// after the tool_use it correlates to.
		time.Sleep(50 * time.Millisecond)
	}
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
	_ = w.Stop(context.Background())
	<-runErrCh

	return w, sink.snapshot()
}

// TestRunPolicyNudgeEmitsCorrelatedEvent verifies a ModeNudge decision
// fires a policy.nudge event with ParentID correlated to the tool_use
// it was derived from.
func TestRunPolicyNudgeEmitsCorrelatedEvent(t *testing.T) {
	pol := &recordingPolicy{
		decision: policy.Decision{
			Mode:    policy.ModeNudge,
			RuleID:  "hollis.deploy.nanite.cerberus-required",
			Message: "Use cerberus_resource_deploy instead of go build",
		},
	}
	_, evs := runWrapperUntilToolUsePolicy(t, t.TempDir(), "Bash", pol, false)

	pol.mu.Lock()
	defer pol.mu.Unlock()
	if pol.calls != 1 {
		t.Errorf("policy.Decide calls = %d, want 1", pol.calls)
	}
	if pol.requests[0].Kind != "tool_use" {
		t.Errorf("policy.Request.Kind = %q, want tool_use", pol.requests[0].Kind)
	}
	if pol.requests[0].App != "test-policy" {
		t.Errorf("policy.Request.App = %q, want test-policy", pol.requests[0].App)
	}
	if !strings.Contains(pol.requests[0].Original, `"name":"Bash"`) {
		t.Errorf("policy.Request.Original should contain serialized tool name; got %q", pol.requests[0].Original)
	}

	idxTool := indexOfKind(evs, runtimeevents.KindAgentToolUse)
	idxNudge := indexOfKind(evs, runtimeevents.KindPolicyNudge)
	if idxTool < 0 {
		t.Fatal("missing agent.tool_use event")
	}
	if idxNudge < 0 {
		t.Fatalf("missing policy.nudge event; kinds: %v", kindList(evs))
	}
	if !(idxTool < idxNudge) {
		t.Errorf("policy.nudge (idx %d) should fire after agent.tool_use (idx %d)", idxNudge, idxTool)
	}
	if evs[idxNudge].ParentID != evs[idxTool].ID {
		t.Errorf("policy.nudge ParentID = %q, want tool_use ID %q",
			evs[idxNudge].ParentID, evs[idxTool].ID)
	}

	var p map[string]any
	if err := json.Unmarshal(evs[idxNudge].Payload, &p); err != nil {
		t.Fatalf("decode policy.nudge payload: %v", err)
	}
	if got, _ := p["rule_id"].(string); got != "hollis.deploy.nanite.cerberus-required" {
		t.Errorf("rule_id = %q", got)
	}
	if got, _ := p["mode"].(string); got != "nudge" {
		t.Errorf("mode = %q", got)
	}
	if got, _ := p["message"].(string); got != "Use cerberus_resource_deploy instead of go build" {
		t.Errorf("message = %q", got)
	}
}

func TestRunPolicyRewriteEmitsRewriteEvent(t *testing.T) {
	pol := &recordingPolicy{
		decision: policy.Decision{
			Mode:        policy.ModeRewrite,
			RuleID:      "test.rewrite",
			Replacement: `{"name":"Read","input":{"path":"/safe/x"}}`,
			Message:     "rewritten to safe path",
		},
	}
	_, evs := runWrapperUntilToolUsePolicy(t, t.TempDir(), "Read", pol, false)

	idx := indexOfKind(evs, runtimeevents.KindPolicyRewrite)
	if idx < 0 {
		t.Fatalf("missing policy.rewrite event; kinds: %v", kindList(evs))
	}
	var p map[string]any
	if err := json.Unmarshal(evs[idx].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got, _ := p["replacement"].(string); got != `{"name":"Read","input":{"path":"/safe/x"}}` {
		t.Errorf("replacement = %q", got)
	}
}

func TestRunPolicyBlockEmitsBlockEvent(t *testing.T) {
	pol := &recordingPolicy{
		decision: policy.Decision{
			Mode:    policy.ModeBlock,
			RuleID:  "test.block",
			Message: "destructive ops disabled in this session",
		},
	}
	_, evs := runWrapperUntilToolUsePolicy(t, t.TempDir(), "Delete", pol, false)

	if indexOfKind(evs, runtimeevents.KindPolicyBlock) < 0 {
		t.Fatalf("missing policy.block event; kinds: %v", kindList(evs))
	}
}

func TestRunPolicyApprovalEmitsApprovalRequestedEvent(t *testing.T) {
	pol := &recordingPolicy{
		decision: policy.Decision{
			Mode:    policy.ModeApproval,
			RuleID:  "test.approval",
			Message: "operator approval required",
		},
	}
	_, evs := runWrapperUntilToolUsePolicy(t, t.TempDir(), "Write", pol, false)

	idx := indexOfKind(evs, runtimeevents.KindPolicyApprovalRequested)
	if idx < 0 {
		t.Fatalf("missing policy.approval_requested event; kinds: %v", kindList(evs))
	}
	var p map[string]any
	if err := json.Unmarshal(evs[idx].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got, _ := p["mode"].(string); got != "approval" {
		t.Errorf("mode = %q", got)
	}
}

func TestRunPolicyObserveEmitsNoDerivedEvent(t *testing.T) {
	pol := &recordingPolicy{
		decision: policy.Decision{Mode: policy.ModeObserve},
	}
	_, evs := runWrapperUntilToolUsePolicy(t, t.TempDir(), "Read", pol, true)

	for _, kind := range []runtimeevents.EventKind{
		runtimeevents.KindPolicyNudge,
		runtimeevents.KindPolicyRewrite,
		runtimeevents.KindPolicyBlock,
		runtimeevents.KindPolicyApprovalRequested,
	} {
		if indexOfKind(evs, kind) >= 0 {
			t.Errorf("ModeObserve should emit no derived events, saw %q", kind)
		}
	}

	pol.mu.Lock()
	defer pol.mu.Unlock()
	if pol.calls != 1 {
		t.Errorf("policy.Decide calls = %d, want 1 (called once even on observe)", pol.calls)
	}
}

func TestRunPolicyDecideErrorIsSwallowed(t *testing.T) {
	// A Decide error must NOT stop the wrapper's event stream — the
	// tool_use already fired; we only lose the policy decision.
	pol := &recordingPolicy{
		err: errors.New("policy lookup timeout"),
	}
	_, evs := runWrapperUntilToolUsePolicy(t, t.TempDir(), "Read", pol, true)

	if indexOfKind(evs, runtimeevents.KindAgentToolUse) < 0 {
		t.Error("tool_use event missing — Decide error broke the stream")
	}
	for _, kind := range []runtimeevents.EventKind{
		runtimeevents.KindPolicyNudge,
		runtimeevents.KindPolicyRewrite,
		runtimeevents.KindPolicyBlock,
		runtimeevents.KindPolicyApprovalRequested,
	} {
		if indexOfKind(evs, kind) >= 0 {
			t.Errorf("Decide error should suppress derived policy events, saw %q", kind)
		}
	}
	if indexOfKind(evs, runtimeevents.KindProcessExited) < 0 {
		t.Error("process.exited missing — Decide error broke the lifecycle")
	}
}

func TestRunPolicyNotInvokedForNonToolUseEvents(t *testing.T) {
	// Verify Policy.Decide is NOT called for events other than
	// tool_use (delta, session_id, done, etc.).
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{
		"session:ses_abc",
		"delta:hello",
		"done",
	})
	pol := &recordingPolicy{
		decision: policy.Decision{Mode: policy.ModeNudge, RuleID: "should-not-fire"},
	}
	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, err := New(Config{
		App:      "test-policy-noop",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
		Policy:   pol,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	_ = w.SendInput(context.Background(), []byte("trigger"))
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
	_ = w.Stop(context.Background())
	<-runErrCh

	pol.mu.Lock()
	defer pol.mu.Unlock()
	if pol.calls != 0 {
		t.Errorf("policy.Decide called %d times for a tool_use-free turn; want 0", pol.calls)
	}
}

func kindList(evs []runtimeevents.Event) []runtimeevents.EventKind {
	out := make([]runtimeevents.EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

// writeFakeScriptWithStderr is like writeFakeScript plus one stderr
// line emitted before the stdout sequence. Used for stderr-event tests.
func writeFakeScriptWithStderr(t *testing.T, dir string, stderrLine string, stdoutLines []string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake script needs sh; not running on Windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available on PATH")
	}
	if strings.ContainsRune(stderrLine, '\'') {
		t.Fatalf("stderr line %q contains a single quote", stderrLine)
	}
	body := "#!/bin/sh\n"
	body += "printf '%s\\n' '" + stderrLine + "' 1>&2\n"
	for _, l := range stdoutLines {
		if strings.ContainsRune(l, '\'') {
			t.Fatalf("test line %q contains a single quote", l)
		}
		body += "printf '%s\\n' '" + l + "'\n"
	}
	body += "exit 0\n"
	path := filepath.Join(dir, "fake-cli-stderr.sh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// TestRunEmitsStdoutRawAndLineEvents verifies that session output
// surfaces as both stdout.raw (per Write) and stdout.line (per
// logical line) events with monotonic RawOffsets.
//
// Note on adapter-runtime fidelity: agentkit's runner does not tee
// raw child stdout bytes — it writes per-turn formatted output to
// Fanout (concatenated delta content + a "[turn_done]" marker on
// terminal events). The PTY runtime is the path for byte-exact
// child stdout. See streamWriter docstring.
func TestRunEmitsStdoutRawAndLineEvents(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{
		"delta:hello",
		"delta:world",
		"done",
	})
	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, err := New(Config{
		App:      "test-stdio",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	_ = w.SendInput(context.Background(), []byte("trigger"))
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
	_ = w.Stop(context.Background())
	<-runErrCh

	evs := sink.snapshot()

	var rawCount, lineCount int
	var lines []string
	var rawOffsets []int64
	for _, ev := range evs {
		switch ev.Kind {
		case runtimeevents.KindStdoutRaw:
			rawCount++
			if ev.RawOffset == nil {
				t.Errorf("stdout.raw event missing RawOffset")
			} else {
				rawOffsets = append(rawOffsets, *ev.RawOffset)
			}
		case runtimeevents.KindStdoutLine:
			lineCount++
			var p map[string]any
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Errorf("decode stdout.line: %v", err)
				continue
			}
			if l, ok := p["line"].(string); ok {
				lines = append(lines, l)
			}
		}
	}

	if rawCount == 0 {
		t.Error("no stdout.raw events emitted")
	}
	if lineCount == 0 {
		t.Error("no stdout.line events emitted")
	}

	// Adapter runtime writes per-turn formatted output: delta
	// contents concatenated, then a "[turn_done]" marker. Verify
	// both pieces appear somewhere in the line stream.
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "hello") {
		t.Errorf("stdout.line content missing 'hello'; saw: %q", joined)
	}
	if !strings.Contains(joined, "world") {
		t.Errorf("stdout.line content missing 'world'; saw: %q", joined)
	}
	if !strings.Contains(joined, "turn_done") {
		t.Errorf("stdout.line content missing turn terminator; saw: %q", joined)
	}

	// RawOffsets must be monotonically non-decreasing.
	for i := 1; i < len(rawOffsets); i++ {
		if rawOffsets[i] < rawOffsets[i-1] {
			t.Errorf("RawOffset went backwards at index %d: %d -> %d (full: %v)",
				i, rawOffsets[i-1], rawOffsets[i], rawOffsets)
		}
	}
}

// TestRunEmitsStderrEvents verifies stderr writes surface as
// stderr.raw + stderr.line events.
func TestRunEmitsStderrEvents(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScriptWithStderr(t, dir, "boot warning: stale cache", []string{
		"delta:hello",
		"done",
	})
	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, err := New(Config{
		App:      "test-stderr",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	_ = w.SendInput(context.Background(), []byte("trigger"))
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
	_ = w.Stop(context.Background())
	<-runErrCh

	evs := sink.snapshot()
	var stderrLines []string
	for _, ev := range evs {
		if ev.Kind == runtimeevents.KindStderrLine {
			var p map[string]any
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				continue
			}
			if l, ok := p["line"].(string); ok {
				stderrLines = append(stderrLines, l)
			}
		}
	}
	if !strings.Contains(strings.Join(stderrLines, ","), "boot warning: stale cache") {
		t.Errorf("expected stderr.line containing 'boot warning: stale cache'; saw: %v", stderrLines)
	}
}

// TestRunEmitsStdinWriteOnSendInput verifies that SendInput emits a
// stdin.write event carrying the input bytes.
func TestRunEmitsStdinWriteOnSendInput(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{"delta:ack", "done"})
	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, err := New(Config{
		App:      "test-stdin",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	if err := w.SendInput(context.Background(), []byte("my prompt")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
	_ = w.Stop(context.Background())
	<-runErrCh

	evs := sink.snapshot()
	idx := indexOfKind(evs, runtimeevents.KindStdinWrite)
	if idx < 0 {
		t.Fatalf("missing stdin.write event; kinds: %v", kindList(evs))
	}
	var p map[string]any
	if err := json.Unmarshal(evs[idx].Payload, &p); err != nil {
		t.Fatalf("decode stdin.write: %v", err)
	}
	if got, _ := p["bytes"].(string); got != "my prompt" {
		t.Errorf("stdin.write bytes = %q, want %q", got, "my prompt")
	}
}

// TestRunStopEmitsInterruptPair verifies Wrapper.Stop emits the
// interrupt.requested → interrupt.acknowledged pair correlated by
// ParentID, with reason=user_stop.
func TestRunStopEmitsInterruptPair(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{"delta:hello"})
	// Patch script to read stdin forever after emitting — Stop is
	// the only way to terminate it.
	scriptBody, _ := os.ReadFile(script)
	scriptBody = append(scriptBody[:len(scriptBody)-len("exit 0\n")], []byte("cat > /dev/null\nexit 0\n")...)
	if err := os.WriteFile(script, scriptBody, 0o755); err != nil {
		t.Fatalf("rewrite script: %v", err)
	}

	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, _ := New(Config{
		App:      "test-stop-interrupt",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	_ = w.SendInput(context.Background(), []byte("trigger"))
	sink.waitFor(t, runtimeevents.KindAgentDelta, 5*time.Second)

	if err := w.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}
	<-runErrCh

	evs := sink.snapshot()
	idxReq := indexOfKind(evs, runtimeevents.KindInterruptRequested)
	idxAck := indexOfKind(evs, runtimeevents.KindInterruptAcknowledged)
	if idxReq < 0 || idxAck < 0 {
		t.Fatalf("missing interrupt events: req=%d ack=%d (kinds: %v)", idxReq, idxAck, kindList(evs))
	}
	if !(idxReq < idxAck) {
		t.Errorf("interrupt.requested (idx %d) should fire before interrupt.acknowledged (idx %d)", idxReq, idxAck)
	}
	if evs[idxAck].ParentID != evs[idxReq].ID {
		t.Errorf("interrupt.acknowledged ParentID = %q, want interrupt.requested ID %q",
			evs[idxAck].ParentID, evs[idxReq].ID)
	}

	var p map[string]any
	if err := json.Unmarshal(evs[idxReq].Payload, &p); err != nil {
		t.Fatalf("decode interrupt.requested: %v", err)
	}
	if reason, _ := p["reason"].(string); reason != "user_stop" {
		t.Errorf("interrupt reason = %q, want user_stop", reason)
	}
}

// TestRunCtxCancelEmitsInterruptPair verifies the ctx-watcher
// goroutine emits the same interrupt pair with reason=ctx_cancel.
func TestRunCtxCancelEmitsInterruptPair(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{"delta:hello"})
	scriptBody, _ := os.ReadFile(script)
	scriptBody = append(scriptBody[:len(scriptBody)-len("exit 0\n")], []byte("cat > /dev/null\nexit 0\n")...)
	if err := os.WriteFile(script, scriptBody, 0o755); err != nil {
		t.Fatalf("rewrite script: %v", err)
	}

	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, _ := New(Config{
		App:      "test-ctx-interrupt",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	_ = w.SendInput(context.Background(), []byte("trigger"))
	sink.waitFor(t, runtimeevents.KindAgentDelta, 5*time.Second)

	cancel()
	<-runErrCh

	evs := sink.snapshot()
	idxReq := indexOfKind(evs, runtimeevents.KindInterruptRequested)
	if idxReq < 0 {
		t.Fatalf("missing interrupt.requested on ctx cancel; kinds: %v", kindList(evs))
	}
	var p map[string]any
	if err := json.Unmarshal(evs[idxReq].Payload, &p); err != nil {
		t.Fatalf("decode interrupt.requested: %v", err)
	}
	if reason, _ := p["reason"].(string); reason != "ctx_cancel" {
		t.Errorf("interrupt reason = %q, want ctx_cancel", reason)
	}
}

// TestRunEmitsTurnStartedBeforeFirstAgentEvent verifies turn.started
// fires immediately before the first turn-internal event, and that
// turn-internal + turn-ending events carry the same TurnID.
func TestRunEmitsTurnStartedBeforeFirstAgentEvent(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{
		"session:ses_provider",
		"delta:hello",
		"delta:world",
		"done",
	})
	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, err := New(Config{
		App:      "test-turns",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	_ = w.SendInput(context.Background(), []byte("trigger"))
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
	_ = w.Stop(context.Background())
	<-runErrCh

	evs := sink.snapshot()
	idxStart := indexOfKind(evs, runtimeevents.KindTurnStarted)
	idxDelta := indexOfKind(evs, runtimeevents.KindAgentDelta)
	idxDone := indexOfKind(evs, runtimeevents.KindTurnCompleted)
	if idxStart < 0 || idxDelta < 0 || idxDone < 0 {
		t.Fatalf("missing turn-lifecycle events: start=%d delta=%d done=%d (kinds: %v)",
			idxStart, idxDelta, idxDone, kindList(evs))
	}
	if !(idxStart < idxDelta && idxDelta < idxDone) {
		t.Errorf("event order wrong: turn.started=%d delta=%d turn.completed=%d",
			idxStart, idxDelta, idxDone)
	}

	turnID := evs[idxStart].TurnID
	if turnID == "" || !strings.HasPrefix(turnID, "turn_") {
		t.Errorf("turn.started TurnID = %q, want non-empty turn_<hex>", turnID)
	}
	// All deltas + the turn.completed should share the same TurnID.
	for i, ev := range evs {
		if ev.Kind == runtimeevents.KindAgentDelta || ev.Kind == runtimeevents.KindTurnCompleted {
			if ev.TurnID != turnID {
				t.Errorf("event %d (%s) TurnID = %q, want %q", i, ev.Kind, ev.TurnID, turnID)
			}
		}
	}
}

// TestRunSessionLifecycleEventsHaveNoTurnID verifies that
// non-turn-scoped events (session.ready, process.started/exited)
// do NOT carry a TurnID even when emitted while a turn is in flight.
func TestRunSessionLifecycleEventsHaveNoTurnID(t *testing.T) {
	dir := t.TempDir()
	script := writeFakeScript(t, dir, []string{"delta:x", "done"})
	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()
	w, _ := New(Config{
		App:      "test-turn-noscope",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	_ = w.SendInput(context.Background(), []byte("trigger"))
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
	_ = w.Stop(context.Background())
	<-runErrCh

	for _, ev := range sink.snapshot() {
		switch ev.Kind {
		case runtimeevents.KindSessionReady,
			runtimeevents.KindProcessStarted,
			runtimeevents.KindProcessExited:
			if ev.TurnID != "" {
				t.Errorf("non-turn-scoped event %s carries TurnID %q", ev.Kind, ev.TurnID)
			}
		}
	}
}

// TestRunPolicyEventInheritsTurnID verifies that derived policy
// events fire under the same TurnID as the tool_use they correlate to.
func TestRunPolicyEventInheritsTurnID(t *testing.T) {
	pol := &recordingPolicy{
		decision: policy.Decision{Mode: policy.ModeNudge, RuleID: "test.rule"},
	}
	w, evs := runWrapperUntilToolUsePolicy(t, t.TempDir(), "Bash", pol, false)
	_ = w

	idxTool := indexOfKind(evs, runtimeevents.KindAgentToolUse)
	idxNudge := indexOfKind(evs, runtimeevents.KindPolicyNudge)
	if idxTool < 0 || idxNudge < 0 {
		t.Fatalf("missing events: tool=%d nudge=%d", idxTool, idxNudge)
	}
	if evs[idxTool].TurnID == "" {
		t.Fatal("tool_use event has no TurnID — turn-tracking broken")
	}
	if evs[idxNudge].TurnID != evs[idxTool].TurnID {
		t.Errorf("policy.nudge TurnID = %q, want tool_use TurnID %q",
			evs[idxNudge].TurnID, evs[idxTool].TurnID)
	}
}

// TestRunEndToEndClassifyBridgeWithNaniteRule wires the
// classifybridge.Engine with the worked NaniteDeployRule from the
// architecture doc and proves the full classify → bridge → policy
// → runtime-event chain works end-to-end.
func TestRunEndToEndClassifyBridgeWithNaniteRule(t *testing.T) {
	dir := t.TempDir()
	// Tool use with a command input that contains the Nanite deploy
	// pattern.
	toolLine := toolUseScriptLine(t, "Bash", map[string]any{
		"command": "go build -o nanite ./cmd/nanite",
	})
	script := writeFakeScript(t, dir, []string{toolLine, "done"})
	adapter := &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}}
	sink := newCapturingSink()

	rules := classify.NewRuleSet(classify.NaniteDeployRule)
	engine := &classifybridge.Engine{Classifier: rules}

	w, err := New(Config{
		App:      "test-bridge-e2e",
		Adapter:  adapter,
		Activity: activity.NewBridge(sink),
		Workdir:  dir,
		Policy:   engine,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	_ = w.SendInput(context.Background(), []byte("trigger"))
	sink.waitFor(t, runtimeevents.KindAgentToolUse, 5*time.Second)
	// Settle so policy.nudge lands.
	time.Sleep(100 * time.Millisecond)
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
	_ = w.Stop(context.Background())
	<-runErrCh

	evs := sink.snapshot()
	idxNudge := indexOfKind(evs, runtimeevents.KindPolicyNudge)
	if idxNudge < 0 {
		t.Fatalf("missing policy.nudge event; kinds: %v", kindList(evs))
	}
	var p map[string]any
	if err := json.Unmarshal(evs[idxNudge].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got, _ := p["rule_id"].(string); got != "hollis.deploy.nanite.cerberus-required" {
		t.Errorf("rule_id = %q; want hollis.deploy.nanite.cerberus-required (bridge did not match the Nanite rule end-to-end)", got)
	}
	msg, _ := p["message"].(string)
	if !strings.Contains(msg, "cerberus_resource_deploy nanite-api-service") {
		t.Errorf("message missing cerberus deploy command; got %q", msg)
	}
	if got, _ := p["mode"].(string); got != "nudge" {
		t.Errorf("mode = %q, want nudge (Reversible=false)", got)
	}
}
