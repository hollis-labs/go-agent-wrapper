package wrapper

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// writeExecutableFixture publishes a uniquely named, immutable executable only
// after all bytes are durable and the writer is closed. Linux can reject an
// exec with ETXTBSY while any writer still has the inode open, so tests must
// never write or truncate the path that they hand to a subprocess.
func writeExecutableFixture(t testing.TB, dir, pattern string, body []byte) string {
	t.Helper()
	path, err := publishExecutableFixture(dir, pattern, body)
	if err != nil {
		t.Fatalf("publish executable fixture: %v", err)
	}
	return path
}

func publishExecutableFixture(dir, pattern string, body []byte) (_ string, retErr error) {
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
	if err := os.Chmod(stagingPath, 0o555); err != nil {
		return "", fmt.Errorf("make staging file executable: %w", err)
	}

	publishedPath := stagingPath + ".ready"
	if err := os.Rename(stagingPath, publishedPath); err != nil {
		return "", fmt.Errorf("publish executable fixture: %w", err)
	}
	return publishedPath, nil
}

func TestExecutableFixturesPublishUniqueClosedImmutablePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available on PATH")
	}

	const publishers = 64
	dir := t.TempDir()
	type result struct {
		path string
		want string
		got  string
		err  error
	}
	results := make(chan result, publishers)
	var wg sync.WaitGroup
	for i := range publishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("fixture-%d", i)
			path, err := publishExecutableFixture(dir, "concurrent-fixture", []byte("#!/bin/sh\nprintf '%s\\n' '"+want+"'\n"))
			if err != nil {
				results <- result{want: want, err: err}
				return
			}
			output, err := exec.Command(path).CombinedOutput()
			results <- result{path: path, want: want, got: string(output), err: err}
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
		if _, exists := seen[result.path]; exists {
			t.Errorf("executable path reused: %s", result.path)
		}
		seen[result.path] = struct{}{}
		info, err := os.Stat(result.path)
		if err != nil {
			t.Errorf("stat %s: %v", filepath.Base(result.path), err)
			continue
		}
		if info.Mode().Perm()&0o222 != 0 {
			t.Errorf("fixture %s remains writable: mode=%#o", filepath.Base(result.path), info.Mode().Perm())
		}
	}
	if len(seen) != publishers {
		t.Fatalf("unique executable paths = %d, want %d", len(seen), publishers)
	}
}
