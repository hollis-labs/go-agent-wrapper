package wrapper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestSelectedClaudeDeveloperStreamingEnvironmentAndEvents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess fixture uses POSIX sh")
	}
	root := t.TempDir()
	probe := filepath.Join(root, "environment.txt")
	argsProbe := filepath.Join(root, "arguments.txt")
	binary := filepath.Join(root, "fake claude with spaces.sh")
	body := `#!/bin/sh
printf '%s\n%s\n' "$SAFE_VALUE" "${LEAK_ME+present}" > "$PROBE_FILE"
printf '%s\n' "$@" > "$ARGS_FILE"
IFS= read -r line
printf '%s\n' '{"type":"result","subtype":"success","result":"selected claude"}'
`
	if err := os.WriteFile(binary, []byte(body), 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv("LEAK_ME", "ambient-secret")
	adapter, err := adapters.Select(adapters.Selection{
		Provider: adapters.ProviderClaude, RuntimeKind: adapters.RuntimeKindCLI,
		LaunchMode: adapters.LaunchStreamingStdio, DeveloperMode: true, Binary: binary,
		ExtraArgs: []string{"--label", "value with spaces;still-data"},
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	sink := newCapturingSink()
	w, err := New(Config{
		App: "selection-claude", Adapter: adapter,
		Activity: activity.NewBridge(sink), Workdir: root,
		Environment: ChildEnvironment{Mode: EnvironmentReplace, Set: []string{
			"PATH=/usr/bin:/bin", "PROBE_FILE=" + probe, "ARGS_FILE=" + argsProbe,
			"SAFE_VALUE=streaming value with spaces; $(not executed)",
		}},
		AutoFireFirstTurn: true,
		FirstTurnPayload:  `{"type":"user","message":{"role":"user","content":"hello"}}`,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(context.Background()) }()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}

	observed, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("read environment probe: %v", err)
	}
	if got, want := string(observed), "streaming value with spaces; $(not executed)\n\n"; got != want {
		t.Fatalf("child environment = %q, want %q", got, want)
	}
	argv, err := os.ReadFile(argsProbe)
	if err != nil {
		t.Fatalf("read argv probe: %v", err)
	}
	args := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n")
	for _, want := range []string{"--dangerously-skip-permissions", "--label", "value with spaces;still-data"} {
		if !containsExactString(args, want) {
			t.Errorf("argv missing distinct entry %q: %#v", want, args)
		}
	}
	if !hasKind(sink.snapshot(), runtimeevents.KindTurnCompleted) || !hasKind(sink.snapshot(), runtimeevents.KindProcessExited) {
		t.Fatalf("normalized terminal events missing: %v", sink.kinds())
	}
}

func TestSelectedClaudeExplicitEmptyEnvironmentDoesNotInherit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess fixture uses POSIX sh")
	}
	root := t.TempDir()
	probe := filepath.Join(root, "empty-environment.txt")
	binary := filepath.Join(root, "empty-environment-claude.sh")
	body := `#!/bin/sh
for last do :; done
printf '%s\n%s\n' "${LEAK_ME+present}" "${GO_AGENT_WRAPPER_EMPTY_ENVIRONMENT-missing}" > "$last"
printf '%s\n' '{"type":"result","subtype":"success","result":"empty environment"}'
`
	if err := os.WriteFile(binary, []byte(body), 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv("LEAK_ME", "ambient-secret")
	adapter, err := adapters.Select(adapters.Selection{
		Provider: adapters.ProviderClaude, LaunchMode: adapters.LaunchStreamingStdio,
		Binary: binary, ExtraArgs: []string{probe},
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	w, err := New(Config{
		App: "selection-empty-environment", Adapter: adapter,
		Activity: activity.NewBridge(newCapturingSink()), Workdir: root,
		Environment: ChildEnvironment{Mode: EnvironmentReplace},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Run(runCtx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	observed, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("read environment probe: %v", err)
	}
	if got, want := string(observed), "\n1\n"; got != want {
		t.Fatalf("empty child environment probe = %q, want %q", got, want)
	}
}

func TestSelectedClaudeStreamingCancellationReapsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess fixture and liveness probe use POSIX process signals")
	}
	root := t.TempDir()
	pidFile := filepath.Join(root, "pid")
	binary := filepath.Join(root, "blocking-claude.sh")
	body := `#!/bin/sh
printf '%s\n' "$$" > "$PID_FILE"
trap 'exit 0' TERM INT
while :; do /bin/sleep 1; done
`
	if err := os.WriteFile(binary, []byte(body), 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	adapter, err := adapters.Select(adapters.Selection{
		Provider: adapters.ProviderClaude, LaunchMode: adapters.LaunchStreamingStdio, Binary: binary,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	sink := newCapturingSink()
	w, err := New(Config{
		App: "selection-claude-cancel", Adapter: adapter,
		Activity: activity.NewBridge(sink), Workdir: root,
		Environment: ChildEnvironment{Mode: EnvironmentReplace, Set: []string{
			"PATH=/usr/bin:/bin", "PID_FILE=" + pidFile,
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(context.Background()) }()
	sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
	pid := waitForPIDFile(t, pidFile, 5*time.Second)
	if err := w.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not unblock after Stop")
	}
	deadline := time.Now().Add(5 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("child pid %d still exists after Stop", pid)
	}
	if !hasKind(sink.snapshot(), runtimeevents.KindProcessExited) {
		t.Fatalf("process.exited missing: %v", sink.kinds())
	}
}

func TestSelectedSubprocessPerTurnEnvironmentArgsEventsAndCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess fixture uses POSIX sh")
	}

	tests := []struct {
		name       string
		provider   adapters.Provider
		scriptLine string
	}{
		{
			name:       "codex",
			provider:   adapters.ProviderCodex,
			scriptLine: `printf '%s\n' '{"type":"item.message","role":"assistant","content":"selected codex"}' '{"type":"turn.completed","turn_id":"turn-fixture"}'`,
		},
		{
			name:       "opencode",
			provider:   adapters.ProviderOpenCode,
			scriptLine: `printf '%s\n' 'selected opencode'`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			binDir := filepath.Join(root, "bin with spaces;not-shell")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatalf("mkdir fixture bin: %v", err)
			}
			probe := filepath.Join(root, "environment.txt")
			argsProbe := filepath.Join(root, "arguments.txt")
			injectionMarker := filepath.Join(root, "must-not-exist")
			binary := filepath.Join(binDir, "fake $(provider).sh")
			body := "#!/bin/sh\n" +
				`printf '%s\n%s\n%s\n' "$SAFE_VALUE" "$META_VALUE" "${LEAK_ME+present}" > "$PROBE_FILE"` + "\n" +
				`printf '%s\n' "$@" > "$ARGS_FILE"` + "\n" +
				tc.scriptLine + "\n"
			if err := os.WriteFile(binary, []byte(body), 0o755); err != nil {
				t.Fatalf("write fixture: %v", err)
			}

			t.Setenv("LEAK_ME", "ambient-secret")
			prompt := "prompt with spaces; $(touch " + injectionMarker + ")"
			extra := []string{"--literal", "arg with spaces", "$(touch " + injectionMarker + ")", "semi;colon"}
			adapter, err := adapters.Select(adapters.Selection{
				Provider: tc.provider, RuntimeKind: adapters.RuntimeKindCLI,
				LaunchMode: adapters.LaunchSubprocessPerTurn,
				Binary:     binary, ExtraArgs: extra,
			})
			if err != nil {
				t.Fatalf("Select: %v", err)
			}

			sink := newCapturingSink()
			w, err := New(Config{
				App: "selection-integration", Adapter: adapter,
				Activity: activity.NewBridge(sink), Workdir: root,
				Environment: ChildEnvironment{
					Mode: EnvironmentReplace,
					Set: []string{
						"PATH=/usr/bin:/bin",
						"PROBE_FILE=" + probe,
						"ARGS_FILE=" + argsProbe,
						"SAFE_VALUE=a value with spaces",
						"META_VALUE=$(touch " + injectionMarker + "); `false`; still data",
					},
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			runCtx, cancelRun := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelRun()
			runDone := make(chan error, 1)
			go func() { runDone <- w.Run(runCtx) }()
			sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)

			if err := w.SendInput(context.Background(), []byte(prompt)); err != nil {
				t.Fatalf("SendInput: %v", err)
			}
			sink.waitFor(t, runtimeevents.KindTurnCompleted, 5*time.Second)
			if err := w.Stop(context.Background()); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			select {
			case err := <-runDone:
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not stop")
			}

			observedEnv, err := os.ReadFile(probe)
			if err != nil {
				t.Fatalf("read environment probe: %v", err)
			}
			wantEnv := "a value with spaces\n$(touch " + injectionMarker + "); `false`; still data\n\n"
			if string(observedEnv) != wantEnv {
				t.Fatalf("child environment = %q, want %q", observedEnv, wantEnv)
			}
			if _, err := os.Stat(injectionMarker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("shell-looking data executed; marker stat err=%v", err)
			}

			observedArgs, err := os.ReadFile(argsProbe)
			if err != nil {
				t.Fatalf("read argv probe: %v", err)
			}
			argLines := strings.Split(strings.TrimSuffix(string(observedArgs), "\n"), "\n")
			for _, value := range append([]string{prompt}, extra...) {
				if !containsExactString(argLines, value) {
					t.Errorf("argv did not preserve %q as one entry: %#v", value, argLines)
				}
			}

			events := sink.snapshot()
			if !hasKind(events, runtimeevents.KindAgentDelta) || !hasKind(events, runtimeevents.KindTurnCompleted) || !hasKind(events, runtimeevents.KindProcessExited) {
				t.Fatalf("normalized events missing: %v", sink.kinds())
			}
			for _, ev := range events {
				if ev.Process.Provider != string(tc.provider) || ev.Process.Runtime != RuntimeAdapter {
					t.Fatalf("event process = %+v, want provider=%q runtime=%q", ev.Process, tc.provider, RuntimeAdapter)
				}
			}
		})
	}
}

func TestSelectedSubprocessPerTurnCancellationReapsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess fixture and liveness probe use POSIX process signals")
	}

	for _, providerID := range []adapters.Provider{adapters.ProviderCodex, adapters.ProviderOpenCode} {
		t.Run(string(providerID), func(t *testing.T) {
			root := t.TempDir()
			pidFile := filepath.Join(root, "pid")
			termFile := filepath.Join(root, "terminated")
			binary := filepath.Join(root, "blocking-provider.sh")
			body := "#!/bin/sh\n" +
				`printf '%s\n' "$$" > "$PID_FILE"` + "\n" +
				`trap 'printf terminated > "$TERM_FILE"; exit 0' TERM INT` + "\n" +
				`while :; do /bin/sleep 1; done` + "\n"
			if err := os.WriteFile(binary, []byte(body), 0o755); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			adapter, err := adapters.Select(adapters.Selection{
				Provider: providerID, LaunchMode: adapters.LaunchSubprocessPerTurn, Binary: binary,
			})
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			sink := newCapturingSink()
			w, err := New(Config{
				App: "selection-cancel", Adapter: adapter,
				Activity: activity.NewBridge(sink), Workdir: root,
				Environment: ChildEnvironment{Mode: EnvironmentReplace, Set: []string{
					"PATH=/usr/bin:/bin", "PID_FILE=" + pidFile, "TERM_FILE=" + termFile,
				}},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			runDone := make(chan error, 1)
			go func() { runDone <- w.Run(context.Background()) }()
			sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
			sendDone := make(chan error, 1)
			go func() { sendDone <- w.SendInput(context.Background(), []byte("block")) }()

			pid := waitForPIDFile(t, pidFile, 5*time.Second)
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			if err := w.Stop(stopCtx); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			select {
			case err := <-sendDone:
				if err == nil {
					t.Fatal("SendInput returned nil after cancellation; want cancellation/process error")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("SendInput did not unblock after Stop")
			}
			select {
			case <-runDone:
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not unblock after Stop")
			}

			deadline := time.Now().Add(5 * time.Second)
			for processExists(pid) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if processExists(pid) {
				t.Fatalf("child pid %d still exists after Stop and SendInput return", pid)
			}
			if _, err := os.Stat(termFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("termination marker stat: %v", err)
			}
			if !hasKind(sink.snapshot(), runtimeevents.KindProcessExited) {
				t.Fatalf("process.exited missing: %v", sink.kinds())
			}
		})
	}
}

func containsExactString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func waitForPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	pid, err := waitForPIDFileValue(path, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func waitForPIDFileValue(path string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	var lastBody []byte
	var lastParseErr error
	sawFile := false
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			sawFile = true
			lastBody = append(lastBody[:0], body...)
			line, complete := strings.CutSuffix(string(body), "\n")
			switch {
			case !complete:
				lastParseErr = errors.New("incomplete write (missing newline terminator)")
			case strings.ContainsRune(line, '\n'):
				lastParseErr = errors.New("multiple lines")
			default:
				pid, convErr := strconv.Atoi(line)
				if convErr == nil && pid > 0 {
					return pid, nil
				}
				if convErr != nil {
					lastParseErr = convErr
				} else {
					lastParseErr = fmt.Errorf("non-positive pid %d", pid)
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("read pid file %q: %w", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sawFile {
		return 0, fmt.Errorf("timed out waiting for valid child pid in %q; last contents %q: %w", path, lastBody, lastParseErr)
	}
	return 0, fmt.Errorf("timed out waiting for child pid file %q", path)
}

func TestWaitForPIDFileRetriesTruncateAndPartialWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pid")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	target := strconv.Itoa(os.Getpid())
	if len(target) < 2 {
		_ = file.Close()
		t.Fatalf("test process pid %q is unexpectedly short", target)
	}
	partialReady := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		if _, err := file.WriteString(target[:1]); err != nil {
			_ = file.Close()
			writerDone <- err
			return
		}
		close(partialReady)
		time.Sleep(40 * time.Millisecond)
		if _, err := file.WriteString(target[1:] + "\n"); err != nil {
			_ = file.Close()
			writerDone <- err
			return
		}
		writerDone <- file.Close()
	}()
	<-partialReady
	got, err := waitForPIDFileValue(path, time.Second)
	if err != nil {
		t.Fatalf("waitForPIDFileValue: %v", err)
	}
	if err := <-writerDone; err != nil {
		t.Fatalf("writer: %v", err)
	}
	if got != os.Getpid() {
		t.Fatalf("pid = %d, want completed pid %d", got, os.Getpid())
	}
}

func TestWaitForPIDFileReportsStableMalformedContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pid")
	if err := os.WriteFile(path, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := waitForPIDFileValue(path, 30*time.Millisecond)
	if err == nil {
		t.Fatal("waitForPIDFileValue returned nil for stable malformed content")
	}
	for _, want := range []string{"not-a-pid", "invalid syntax"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
