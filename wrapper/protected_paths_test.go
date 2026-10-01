package wrapper

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
	sandboxprofile "github.com/hollis-labs/go-sandbox/sandbox"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/adapters/copilotacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/opencodeacp"
)

// CW-20260930-0237: Config.ProtectedPaths write-protects a host's
// control-plane directories from the agent the wrapper launches.

func TestNewRejectsRelativeProtectedPaths(t *testing.T) {
	_, err := New(Config{
		App:            "test-protected-relative",
		Adapter:        &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: "/bin/true"}},
		Activity:       activity.NewBridge(newCapturingSink()),
		Workdir:        t.TempDir(),
		ProtectedPaths: []string{"state"},
	})
	if err == nil || !strings.Contains(err.Error(), "ProtectedPaths") {
		t.Fatalf("New err = %v, want a Config.ProtectedPaths error", err)
	}
}

// The native path: the paths reach agentkit, which wraps the child in the
// protect-only sandbox, and the agent's write into the registered directory
// is refused while its own work lands.
func TestRunProtectedPathsBlockAgentWrites(t *testing.T) {
	requireWriteProtectBackend(t)
	dir := t.TempDir()
	state := t.TempDir()
	allow := filepath.Join(state, "workflow-unattended.json")
	if err := os.WriteFile(allow, []byte(`{"allow":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(dir, "ran")
	body := "#!/bin/sh\n" +
		"printf '{\"allow\":[\"*\"]}' > '" + allow + "' 2>/dev/null && printf 'delta:WROTE\\n'\n" +
		"printf ok > '" + ran + "'\n" +
		"printf 'done\\n'\n"
	script := writeShellFixtureLauncher(t, dir, "fake-agent", []byte(body))
	// The launcher is a symlink to this test binary, which dispatches on its
	// own name. go-sandbox v0.5.0's host-filesystem profile rewrites a
	// symlinked command to its target (fixed in v0.5.1, go-sandbox#9), so
	// swap in a hard link at the same path: the name survives either way.
	if target, err := os.Readlink(script); err == nil {
		if err := os.Remove(script); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(target, script); err != nil {
			t.Skipf("cannot hard-link the fixture launcher (%v); needs go-sandbox v0.5.1", err)
		}
	}

	sink := newCapturingSink()
	w, err := New(Config{
		App:            "test-protected-paths",
		Adapter:        &fakeRuntimeAdapter{cli: &fakeCLI{name: "fakecli", script: script}},
		Activity:       activity.NewBridge(sink),
		Workdir:        dir,
		ProtectedPaths: []string{state},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 10*time.Second)
	if err := w.SendInput(context.Background(), []byte("rewrite the allow-list")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	sink.waitFor(t, runtimeevents.KindTurnCompleted, 10*time.Second)
	_ = w.Stop(context.Background())
	select {
	case <-runErrCh:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Stop")
	}

	if got, _ := os.ReadFile(allow); string(got) != `{"allow":[]}` { //nolint:gosec // G304: the test's own t.TempDir file
		t.Fatalf("allow-list = %q after the turn, want unchanged", got)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Fatalf("the agent did not run (or could not write its workdir): %v", err)
	}
}

// The ACP path merges the paths into the resolved policy.
func TestRunACPProtectedPathsMergeIntoResolvedPolicy(t *testing.T) {
	requireWriteProtectCapability(t)
	dir := t.TempDir()
	state := t.TempDir()
	policy, err := sandboxprofile.ResolveAccessPolicy(sandboxprofile.AccessPolicy{
		ID:      "acp-policy",
		Roots:   sandboxprofile.Roots{Project: dir},
		FS:      sandboxprofile.FilesystemAccess{Write: []sandboxprofile.PathRef{{Root: sandboxprofile.ProjectRoot}}},
		Network: sandboxprofile.NetworkAccess{Mode: sandboxprofile.NetworkFull},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := newFakeACPClient(adapters.InterruptTurn)
	adapter := &fakeACPRuntimeAdapter{client: client, cli: &fakeACPCLIAdapter{client: client, script: writeFakeScript(t, dir, []string{"done"})}}
	w, err := New(Config{
		App:            "test-acp-protected",
		Adapter:        adapter,
		Activity:       activity.NewBridge(newCapturingSink()),
		Workdir:        dir,
		SandboxPolicy:  &policy,
		ProtectedPaths: []string{state},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	for i := 0; i < 100 && !client.wasLaunched(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !client.wasLaunched() {
		t.Fatal("ACP client was not launched")
	}
	launch := client.snapshotLaunch()
	want, _ := filepath.EvalSymlinks(state)
	if launch.SandboxPolicy == nil || len(launch.SandboxPolicy.FS.Protect) != 1 || launch.SandboxPolicy.FS.Protect[0].Path != want {
		t.Fatalf("launch.SandboxPolicy = %+v, want Protect [%s]", launch.SandboxPolicy, want)
	}
	if len(policy.FS.Protect) != 0 {
		t.Fatal("Config.SandboxPolicy was mutated")
	}
}

// Without a required policy the paths travel to the ACP launcher, which puts
// the child under the protect-only profile (CW-20261001-0162); a disabled
// policy is passed through unchanged for the launcher to replace.
func TestRunACPProtectedPathsWithoutRequiredPolicyReachLauncher(t *testing.T) {
	requireWriteProtectCapability(t)
	disabled := &sandboxprofile.ResolvedAccessPolicy{ID: "off", Mode: sandboxprofile.ConfinementDisabled}
	for name, policy := range map[string]*sandboxprofile.ResolvedAccessPolicy{"none": nil, "disabled": disabled} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			state := t.TempDir()
			client := newFakeACPClient(adapters.InterruptTurn)
			adapter := &fakeACPRuntimeAdapter{client: client, cli: &fakeACPCLIAdapter{client: client, script: writeFakeScript(t, dir, []string{"done"})}}
			w, err := New(Config{
				App:            "test-acp-protected-no-policy",
				Adapter:        adapter,
				Activity:       activity.NewBridge(newCapturingSink()),
				Workdir:        dir,
				SandboxPolicy:  policy,
				ProtectedPaths: []string{state},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { _ = w.Run(ctx) }()
			for i := 0; i < 100 && !client.wasLaunched(); i++ {
				time.Sleep(10 * time.Millisecond)
			}
			if !client.wasLaunched() {
				t.Fatal("ACP client was not launched")
			}
			launch := client.snapshotLaunch()
			if launch.SandboxPolicy != policy {
				t.Fatalf("launch.SandboxPolicy = %+v, want %+v", launch.SandboxPolicy, policy)
			}
			if len(launch.ProtectedPaths) != 1 || launch.ProtectedPaths[0] != state {
				t.Fatalf("launch.ProtectedPaths = %q, want [%s]", launch.ProtectedPaths, state)
			}
		})
	}
}

// The protect-only sandbox end to end: a real ACP agent (testdata/acpfixture)
// launched with ProtectedPaths and no SandboxPolicy tries, at startup, to
// rewrite a file in a protected directory. The write is refused, the agent's
// own trace in its workdir is written, the session comes up, and the launch
// reports the protect-only profile as applied. Both launchers that go
// through acp.PrepareLaunchSandbox are covered: the NDJSON bridge client and
// copilotacp over stdio and TCP.
func TestRunACPProtectOnlySandboxBlocksAgentWrites(t *testing.T) {
	requireWriteProtectBackend(t)
	fixturePath := filepath.Join(t.TempDir(), "acpfixture")
	if output, err := exec.Command("go", "build", "-o", fixturePath, "./testdata/acpfixture").CombinedOutput(); err != nil { //nolint:gosec // G204: the test's own t.TempDir output path
		t.Fatalf("build ACP fixture: %v\n%s", err, output)
	}
	adapterFor := map[string]func(t *testing.T) adapters.Adapter{
		"opencode-ndjson": func(*testing.T) adapters.Adapter {
			return opencodeacp.New(opencodeacp.WithBinary(fixturePath))
		},
		"copilot-stdio": func(*testing.T) adapters.Adapter {
			return copilotacp.New(copilotacp.WithAdapterBinary(fixturePath))
		},
		"copilot-tcp": func(t *testing.T) adapters.Adapter {
			return copilotacp.New(copilotacp.WithAdapterTransport(adapters.TransportTCP), copilotacp.WithAdapterPort(reserveTCPPort(t)), copilotacp.WithAdapterBinary(fixturePath))
		},
	}
	for name, adapter := range adapterFor {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			state := t.TempDir()
			allow := filepath.Join(state, "workflow-unattended.json")
			if err := os.WriteFile(allow, []byte(`{"allow":[]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			tracePath := filepath.Join(dir, "trace.log")
			manager := acp.NewManager()
			sink := newCapturingSink()
			w, err := New(Config{
				App:      "acp-protect-only-" + name,
				Adapter:  adapter(t),
				Activity: activity.NewBridge(sink),
				Workdir:  dir,
				Environment: ChildEnvironment{Mode: EnvironmentMerge, Set: []string{
					"ACP_FIXTURE_TRACE=" + tracePath,
					"ACP_DENIED_WRITE=" + allow,
				}},
				SessionID:       "wrapper-acp-protect-only",
				ACPManager:      manager,
				ACPAuthMethodID: "fixture-auth",
				ProtectedPaths:  []string{state},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			runErrCh := make(chan error, 1)
			go func() { runErrCh <- w.Run(ctx) }()
			readyDeadline := time.After(10 * time.Second)
			for !manager.IsLive(w.SessionID()) {
				select {
				case runErr := <-runErrCh:
					t.Fatalf("Run returned before ACP ready: %v", runErr)
				case <-readyDeadline:
					t.Fatalf("ACP session %q was not registered", w.SessionID())
				case <-time.After(20 * time.Millisecond):
				}
			}
			if stopErr := w.Stop(ctx); stopErr != nil {
				t.Fatalf("Stop: %v", stopErr)
			}
			if runErr := <-runErrCh; runErr != nil {
				t.Fatalf("Run: %v", runErr)
			}

			if got, _ := os.ReadFile(allow); string(got) != `{"allow":[]}` { //nolint:gosec // G304: the test's own t.TempDir file
				t.Fatalf("allow-list = %q after the launch, want unchanged", got)
			}
			trace, err := os.ReadFile(tracePath) //nolint:gosec // G304: the test's own t.TempDir file
			if err != nil {
				t.Fatalf("the agent did not write its own workdir: %v", err)
			}
			if !strings.Contains(string(trace), "probe-write-denied") || strings.Contains(string(trace), "probe-write-allowed") {
				t.Fatalf("protected write probe not denied; trace:\n%s", trace)
			}
			event, ok := firstKind(sink.snapshot(), runtimeevents.KindSandboxApplied)
			if !ok {
				t.Fatal("missing sandbox.applied")
			}
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode sandbox.applied: %v", err)
			}
			if payload["policy_id"] != acp.ProtectOnlyProfileID || payload["state"] != "applied" || payload["enforced"] != true {
				t.Fatalf("sandbox payload = %#v, want %s applied", payload, acp.ProtectOnlyProfileID)
			}
		})
	}
}

// requireWriteProtectCapability skips where go-sandbox has no backend that
// can write-protect a path, so an ACP launch with ProtectedPaths is refused
// before anything starts.
func requireWriteProtectCapability(t *testing.T) {
	t.Helper()
	caps := sandboxprofile.ResolveBackendCapabilities("", sandboxprofile.BackendAuto)
	if !caps.Supported || !slices.Contains(caps.Capabilities, sandboxprofile.CapWriteProtect) {
		t.Skipf("no write-protect backend on %s", caps.GOOS)
	}
}

// requireWriteProtectBackend skips where go-sandbox cannot write-protect a
// path: no bwrap or no user namespaces on Linux, no sandbox-exec on macOS.
func requireWriteProtectBackend(t *testing.T) {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("bwrap not installed")
		}
		if out, err := exec.Command("bwrap", "--dev-bind", "/", "/", "--unshare-user", "--", "/bin/true").CombinedOutput(); err != nil {
			t.Skipf("bwrap cannot create namespaces here: %v: %s", err, out)
		}
	case "darwin":
		if _, err := exec.LookPath("sandbox-exec"); err != nil {
			t.Skip("sandbox-exec not installed")
		}
	default:
		t.Skipf("no write-protect backend on %s", runtime.GOOS)
	}
}
