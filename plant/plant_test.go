package plant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hollis-labs/agentkit/artifact"
	"github.com/hollis-labs/agentkit/materialize"
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
	blob, err := os.ReadFile(filepath.Join(bootDir, "data/blob.bin"))
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
	if err := os.MkdirAll(bootDir, 0o755); err != nil {
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
		".codex/config.toml",
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
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
