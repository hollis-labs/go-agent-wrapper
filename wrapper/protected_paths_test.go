package wrapper

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
	sandboxprofile "github.com/hollis-labs/go-sandbox/sandbox"

	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
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

// Without a resolved policy the ACP launcher has nothing to merge into and no
// protect-only sandbox, so it refuses instead of running unprotected.
func TestRunACPProtectedPathsWithoutPolicyRefused(t *testing.T) {
	dir := t.TempDir()
	client := newFakeACPClient(adapters.InterruptTurn)
	adapter := &fakeACPRuntimeAdapter{client: client, cli: &fakeACPCLIAdapter{client: client, script: writeFakeScript(t, dir, []string{"done"})}}
	w, err := New(Config{
		App:            "test-acp-protected-no-policy",
		Adapter:        adapter,
		Activity:       activity.NewBridge(newCapturingSink()),
		Workdir:        dir,
		ProtectedPaths: []string{t.TempDir()},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = w.Run(context.Background())
	if !errors.Is(err, ErrProtectedPathsUnsupported) || !strings.Contains(err.Error(), "CW-20261001-0162") {
		t.Fatalf("Run err = %v, want ErrProtectedPathsUnsupported naming the follow-up", err)
	}
	if client.wasLaunched() {
		t.Fatal("ACP client launched with ProtectedPaths it could not enforce")
	}
}
