package plant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hollis-labs/go-materialize/artifact"
	"github.com/hollis-labs/go-materialize/materialize"
	"github.com/hollis-labs/go-providers/layout"
	"github.com/hollis-labs/go-providers/registry"
)

func TestNoOpPlanter(t *testing.T) {
	var p Planter = NoOpPlanter{}
	r, err := p.Plant(context.Background(), "/tmp/boot", Spec{
		Files: map[string][]byte{"foo": []byte("bar")},
	})
	if err != nil {
		t.Fatalf("Plant: %v", err)
	}
	if len(r.PlantedFiles) != 0 {
		t.Errorf("NoOpPlanter reported planted files: %v", r.PlantedFiles)
	}
}

func TestSharedPlanterCreatePreservesModernArtifacts(t *testing.T) {
	bootDir := filepath.Join(t.TempDir(), "boot")
	p := SharedPlanter{}
	result, err := p.Plant(context.Background(), bootDir, Spec{
		Operation: materialize.OperationCreate,
		Artifacts: artifact.Tree{Entries: []artifact.Entry{
			{Path: "bin/run.sh", Kind: artifact.EntryFile, Mode: 0o755, Bytes: []byte("#!/bin/sh\nexit 0\n"), Ownership: artifact.Ownership{EntryID: "modern:bin", GroupID: "modern"}},
			{Path: "data/blob.bin", Kind: artifact.EntryFile, Mode: 0o600, Bytes: []byte{0, 1, 2, 3}, Ownership: artifact.Ownership{EntryID: "modern:blob", GroupID: "modern"}},
			{Path: "empty", Kind: artifact.EntryDirectory, Mode: 0o750, Ownership: artifact.Ownership{EntryID: "modern:dir", GroupID: "modern"}},
		}},
	})
	if err != nil {
		t.Fatalf("Plant: %v", err)
	}
	if result.Operation != materialize.OperationCreate || !result.Complete {
		t.Fatalf("result operation/complete = %s/%v", result.Operation, result.Complete)
	}
	assertMode(t, filepath.Join(bootDir, "bin/run.sh"), 0o755)
	assertMode(t, filepath.Join(bootDir, "data/blob.bin"), 0o600)
	assertMode(t, filepath.Join(bootDir, "empty"), 0o750)
	blob, err := os.ReadFile(filepath.Join(bootDir, "data/blob.bin")) //nolint:gosec // G304: the test's own temp file
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !slices.Equal(blob, []byte{0, 1, 2, 3}) {
		t.Fatalf("blob = %v", blob)
	}
	if len(result.WrittenFiles) != 3 || len(result.UnchangedFiles) != 0 || len(result.ConflictFiles) != 0 {
		t.Fatalf("result buckets = written %v unchanged %v conflicts %v", result.WrittenFiles, result.UnchangedFiles, result.ConflictFiles)
	}
}

func TestSharedPlanterCreateRefusesPreexistingAndReconcileUpdates(t *testing.T) {
	bootDir := filepath.Join(t.TempDir(), "boot")
	if err := os.MkdirAll(bootDir, 0o755); err != nil { //nolint:gosec // G301: a fixture directory in t.TempDir
		t.Fatalf("mkdir boot: %v", err)
	}
	p := SharedPlanter{}
	_, err := p.Plant(context.Background(), bootDir, Spec{Operation: materialize.OperationCreate, Files: map[string][]byte{"hello.txt": []byte("hi")}})
	if !errors.Is(err, materialize.ErrTargetExists) {
		t.Fatalf("create on preexisting err = %v, want ErrTargetExists", err)
	}

	result, err := p.Plant(context.Background(), bootDir, Spec{Operation: materialize.OperationReconcile, Files: map[string][]byte{"hello.txt": []byte("hi")}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Operation != materialize.OperationReconcile || !result.Complete {
		t.Fatalf("reconcile result operation/complete = %s/%v", result.Operation, result.Complete)
	}
	if got := string(mustRead(t, filepath.Join(bootDir, "hello.txt"))); got != "hi" {
		t.Fatalf("hello.txt = %q", got)
	}

	again, err := p.Plant(context.Background(), bootDir, Spec{Operation: materialize.OperationReconcile, Files: map[string][]byte{"hello.txt": []byte("hi")}})
	if err != nil {
		t.Fatalf("reconcile unchanged: %v", err)
	}
	if len(again.UnchangedFiles) != 1 || len(again.WrittenFiles) != 0 {
		t.Fatalf("unchanged result = written %v unchanged %v", again.WrittenFiles, again.UnchangedFiles)
	}
}

func TestSharedPlanterLegacySpecPathsAndModes(t *testing.T) {
	bootDir := filepath.Join(t.TempDir(), "boot")
	p := SharedPlanter{}
	result, err := p.Plant(context.Background(), bootDir, Spec{
		Files:            map[string][]byte{"notes/readme.md": []byte("hello")},
		MCPConfig:        []byte(`{"mcpServers":{}}`),
		ProviderSettings: map[string][]byte{"claude": []byte(`{"permissions":{}}`), "codex": []byte("sandbox_mode = \"workspace-write\"\n")},
		Hooks:            []Hook{{Provider: "claude", Name: "PreToolUse", Payload: []byte("#!/bin/sh\n")}},
		RecoveryPrompt:   "recover",
	})
	if err != nil {
		t.Fatalf("Plant: %v", err)
	}
	wantFiles := []string{
		"notes/readme.md",
		".mcp.json",
		".claude/settings.json",
		"config.toml", // Codex, under CODEX_HOME=boot (go-providers layout)
		"hooks/claude/PreToolUse",
		"recovery.md",
	}
	for _, rel := range wantFiles {
		if _, err := os.Stat(filepath.Join(bootDir, rel)); err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}
	assertMode(t, filepath.Join(bootDir, ".mcp.json"), 0o600)
	assertMode(t, filepath.Join(bootDir, ".claude/settings.json"), 0o600)
	assertMode(t, filepath.Join(bootDir, "hooks/claude/PreToolUse"), 0o700)
	if len(result.PlannedFiles) != len(wantFiles) || len(result.WrittenFiles) != len(wantFiles) {
		t.Fatalf("result = planned %v written %v", result.PlannedFiles, result.WrittenFiles)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %o, want %o", path, got, want)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: a test helper; callers pass paths under t.TempDir
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// Every runtime the go-providers layout has rows for gets its settings file
// planted at the layout's native-config path, by id and by every alias
// (CW-20261001-0074: no wrapper-side list of paths).
func TestSharedPlanterProviderSettingsFollowTheLayout(t *testing.T) {
	for _, id := range layout.Runtimes() {
		want, ok := layout.Find(id, layout.Shape{}, layout.NativeConfig)
		if !ok {
			t.Errorf("%s has layout rows but no every-mode native-config row to plant settings at", id)
			continue
		}
		d, ok := registry.Lookup(string(id))
		if !ok {
			t.Fatalf("layout runtime %s is not in the registry", id)
		}
		for _, name := range append([]string{string(id)}, d.Aliases...) {
			bootDir := filepath.Join(t.TempDir(), "boot")
			result, err := (SharedPlanter{}).Plant(context.Background(), bootDir, Spec{
				ProviderSettings: map[string][]byte{name: []byte("settings for " + name)},
			})
			if err != nil {
				t.Fatalf("Plant(ProviderSettings[%q]): %v", name, err)
			}
			got, err := os.ReadFile(filepath.Join(bootDir, filepath.FromSlash(want.Rel))) //nolint:gosec // G304: test reads its own t.TempDir boot dir
			if err != nil || string(got) != "settings for "+name {
				t.Errorf("ProviderSettings[%q]: %s = %q, %v; want the settings (planted %v)", name, want.Rel, got, err, result.PlannedFiles)
			}
		}
	}
}

// A runtime the registry does not know, or one launched only over ACP (no
// native-config row), is refused rather than planted at a guessed path.
func TestSharedPlanterProviderSettingsRefusesRuntimesWithoutNativeConfig(t *testing.T) {
	names := []string{"no-such-runtime", ""}
	for _, d := range registry.All() {
		if _, ok := layout.Find(d.ID, layout.Shape{}, layout.NativeConfig); !ok {
			names = append(names, string(d.ID))
		}
	}
	for _, name := range names {
		bootDir := filepath.Join(t.TempDir(), "boot")
		_, err := (SharedPlanter{}).Plant(context.Background(), bootDir, Spec{
			ProviderSettings: map[string][]byte{name: []byte("x")},
		})
		if err == nil {
			t.Errorf("Plant(ProviderSettings[%q]) = nil error, want a refusal", name)
		}
		if entries, _ := os.ReadDir(bootDir); len(entries) != 0 {
			t.Errorf("Plant(ProviderSettings[%q]) wrote %v", name, entries)
		}
	}
}
