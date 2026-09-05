package testgate

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestEveryInstalledProviderTestUsesTheSharedGate(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller did not return this test's source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	lookups := []string{
		`exec.LookPath("claude")`,
		`exec.LookPath("codex")`,
		`exec.LookPath("copilot")`,
		`exec.LookPath("opencode")`,
		`exec.LookPath("pi")`,
		`exec.LookPath("npx")`,
	}
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || path == source || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, lookup := range lookups {
			if strings.Contains(string(contents), lookup) {
				files = append(files, path)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("discover installed-provider tests: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("installed-provider test discovery found no positive control")
	}
	sort.Strings(files)
	for _, path := range files {
		name, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("make installed-provider test path relative: %v", err)
		}
		t.Run(name, func(t *testing.T) {
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read installed-provider test: %v", err)
			}
			if !strings.Contains(string(contents), "testgate.RequireLiveProvider(t)") {
				t.Fatalf("%s does not call the shared installed-provider opt-in gate", name)
			}
		})
	}
}
