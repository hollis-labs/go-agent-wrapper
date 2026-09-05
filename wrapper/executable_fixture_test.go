package wrapper

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const fixtureLauncherSuffix = ".fixture-launcher"

// TestMain turns the already-built wrapper test binary into a stable fixture
// launcher. Each generated shell script is opened by /bin/sh as data; the
// kernel never tries to execute a file that the test just wrote. This matters
// on Linux, where even a closed, atomically published script can transiently
// fail direct execution with ETXTBSY on hosted filesystems.
func TestMain(m *testing.M) {
	if strings.HasSuffix(os.Args[0], fixtureLauncherSuffix) {
		scriptPath := strings.TrimSuffix(os.Args[0], fixtureLauncherSuffix)
		if err := execShellFixture(scriptPath, os.Args[1:]); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "run shell fixture: %v\n", err)
			os.Exit(1)
		}
		panic("execShellFixture returned without an error")
	}
	os.Exit(m.Run())
}

// writeShellFixtureLauncher publishes a uniquely named, read-only shell script
// and returns a symlink to the stable, already-built test binary. TestMain uses
// the symlink name to pass the script path to /bin/sh as data while preserving
// the production command/argv contracts exercised by callers.
func writeShellFixtureLauncher(t testing.TB, dir, pattern string, body []byte) string {
	t.Helper()
	path, err := publishShellFixtureLauncher(dir, pattern, body)
	if err != nil {
		t.Fatalf("publish shell fixture launcher: %v", err)
	}
	return path
}

func shellFixtureScriptPath(launcherPath string) string {
	return strings.TrimSuffix(launcherPath, fixtureLauncherSuffix)
}

func publishShellFixtureLauncher(dir, pattern string, body []byte) (_ string, retErr error) {
	writer, err := os.CreateTemp(dir, "."+pattern+"-*.writing")
	if err != nil {
		return "", fmt.Errorf("create staging file: %w", err)
	}
	stagingPath := writer.Name()
	closed := false
	defer func() {
		if !closed {
			_ = writer.Close()
		}
		if retErr != nil {
			_ = os.Remove(stagingPath)
		}
	}()

	if _, err := writer.Write(body); err != nil {
		return "", fmt.Errorf("write staging file: %w", err)
	}
	if err := writer.Sync(); err != nil {
		return "", fmt.Errorf("sync staging file: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close staging file: %w", err)
	}
	closed = true
	if err := os.Chmod(stagingPath, 0o444); err != nil {
		return "", fmt.Errorf("make staging file read-only: %w", err)
	}

	scriptPath := stagingPath + ".ready"
	if err := os.Rename(stagingPath, scriptPath); err != nil {
		return "", fmt.Errorf("publish shell fixture: %w", err)
	}

	testBinary, err := os.Executable()
	if err != nil {
		_ = os.Remove(scriptPath)
		return "", fmt.Errorf("resolve stable test binary: %w", err)
	}
	launcherPath := scriptPath + fixtureLauncherSuffix
	if err := os.Symlink(testBinary, launcherPath); err != nil {
		_ = os.Remove(scriptPath)
		return "", fmt.Errorf("publish stable fixture launcher: %w", err)
	}
	return launcherPath, nil
}

func TestShellFixturesUseStableExecutableAndUniqueReadOnlyScripts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh is unavailable")
	}

	const publishers = 64
	dir := t.TempDir()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	type result struct {
		launcherPath string
		want         string
		got          string
		err          error
	}
	results := make(chan result, publishers)
	var wg sync.WaitGroup
	for i := range publishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("fixture-%d", i)
			launcherPath, err := publishShellFixtureLauncher(dir, "concurrent-fixture", []byte("#!/bin/sh\nprintf '%s\\n' '"+want+"'\n"))
			if err != nil {
				results <- result{want: want, err: err}
				return
			}
			output, err := exec.Command(launcherPath).CombinedOutput()
			results <- result{launcherPath: launcherPath, want: want, got: string(output), err: err}
		}()
	}
	wg.Wait()
	close(results)

	seen := make(map[string]struct{}, publishers)
	for result := range results {
		if result.err != nil {
			t.Errorf("publish/execute %q: %v (output %q)", result.want, result.err, result.got)
			continue
		}
		if result.got != result.want+"\n" {
			t.Errorf("fixture %q output = %q", result.want, result.got)
		}
		if _, exists := seen[result.launcherPath]; exists {
			t.Errorf("launcher path reused: %s", result.launcherPath)
		}
		seen[result.launcherPath] = struct{}{}
		linkTarget, err := os.Readlink(result.launcherPath)
		if err != nil {
			t.Errorf("read launcher symlink %s: %v", filepath.Base(result.launcherPath), err)
			continue
		}
		if linkTarget != testBinary {
			t.Errorf("launcher %s target = %q, want stable test binary %q", filepath.Base(result.launcherPath), linkTarget, testBinary)
		}
		scriptPath := shellFixtureScriptPath(result.launcherPath)
		info, err := os.Stat(scriptPath)
		if err != nil {
			t.Errorf("stat script data %s: %v", filepath.Base(scriptPath), err)
			continue
		}
		if !info.Mode().IsRegular() {
			t.Errorf("script data %s mode = %s, want regular file", filepath.Base(scriptPath), info.Mode())
		}
		if info.Mode().Perm() != 0o444 {
			t.Errorf("script data %s mode = %#o, want read-only 0444", filepath.Base(scriptPath), info.Mode().Perm())
		}
		body, err := os.ReadFile(scriptPath)
		if err != nil {
			t.Errorf("read script data %s after execution: %v", filepath.Base(scriptPath), err)
			continue
		}
		wantBody := "#!/bin/sh\nprintf '%s\\n' '" + result.want + "'\n"
		if string(body) != wantBody {
			t.Errorf("script data %s was rewritten: got %q, want %q", filepath.Base(scriptPath), body, wantBody)
		}
	}
	if len(seen) != publishers {
		t.Fatalf("unique launcher paths = %d, want %d", len(seen), publishers)
	}
}
