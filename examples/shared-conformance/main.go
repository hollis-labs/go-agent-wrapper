package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hollis-labs/agentkit/agentcontext"
	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/artifact"
	"github.com/hollis-labs/agentkit/materialize"
	"github.com/hollis-labs/go-agent-wrapper/plant"
)

type scenarioResult struct {
	Scenario     string   `json:"scenario"`
	BootRoot     string   `json:"boot_root"`
	EntryPaths   []string `json:"entry_paths"`
	Checks       []string `json:"checks"`
	RuntimeNeeds []string `json:"runtime_needs,omitempty"`
}

func main() {
	var scenario string
	var root string
	flag.StringVar(&scenario, "scenario", "all", "scenario to run: all, cairn, nanite, torque, tether")
	flag.StringVar(&root, "root", "", "optional root for generated temp data")
	flag.Parse()

	results, err := runSelected(context.Background(), scenario, root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}

func runSelected(ctx context.Context, scenario, root string) ([]scenarioResult, error) {
	base, cleanup, err := scenarioRoot(root)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	runners := map[string]func(context.Context, string) (scenarioResult, error){
		"cairn":  runCairn,
		"nanite": runNanite,
		"torque": runTorque,
		"tether": runTether,
	}
	order := []string{"cairn", "nanite", "torque", "tether"}
	if scenario != "all" {
		if _, ok := runners[scenario]; !ok {
			return nil, fmt.Errorf("unknown scenario %q", scenario)
		}
		order = []string{scenario}
	}
	results := make([]scenarioResult, 0, len(order))
	for _, name := range order {
		scenarioBase := filepath.Join(base, name)
		if err := os.MkdirAll(scenarioBase, 0o755); err != nil {
			return nil, err
		}
		result, err := runners[name](ctx, scenarioBase)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func scenarioRoot(root string) (string, func(), error) {
	if root != "" {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return "", nil, err
		}
		return root, func() {}, nil
	}
	base, err := os.MkdirTemp("", "shared-conformance-*")
	if err != nil {
		return "", nil, err
	}
	return base, func() { _ = os.RemoveAll(base) }, nil
}

func runCairn(ctx context.Context, base string) (scenarioResult, error) {
	sourceRoot := filepath.Join(base, "source")
	bootRoot := filepath.Join(base, "boot")
	if err := writeFile(filepath.Join(sourceRoot, "bin", "install.sh"), []byte("#!/bin/sh\necho cairn\n"), 0o755); err != nil {
		return scenarioResult{}, err
	}
	if err := writeFile(filepath.Join(sourceRoot, "templates", "prompt.md"), []byte("synthetic prompt\n"), 0o644); err != nil {
		return scenarioResult{}, err
	}
	if err := os.MkdirAll(filepath.Join(sourceRoot, "empty-dir"), 0o755); err != nil {
		return scenarioResult{}, err
	}
	fsTree, err := artifact.NewResolver(artifact.ResolverOptions{}).ResolveArtifacts(ctx, artifact.SourceRequest{
		Source:            artifact.Source{Kind: artifact.SourceFilesystemTree, Filesystem: &artifact.FilesystemSource{Root: sourceRoot}},
		DestinationPrefix: "filesystem-tree",
		OwnershipGroup:    "cairn:copied-tree",
	})
	if err != nil {
		return scenarioResult{}, err
	}
	jsonDoc, err := materialize.MergeDocument(materialize.DocumentPatch{
		Kind:     materialize.DocumentJSON,
		Existing: []byte(`{"unowned":true}`),
		Desired: []materialize.ManagedKey{{
			Key:     "managed",
			Value:   json.RawMessage(`"cairn"`),
			EntryID: "cairn-json-managed",
			GroupID: "cairn:install-docs",
		}},
	})
	if err != nil {
		return scenarioResult{}, err
	}
	tomlDoc, err := materialize.MergeDocument(materialize.DocumentPatch{
		Kind:     materialize.DocumentTOML,
		Existing: []byte("unowned = true\n"),
		Desired: []materialize.ManagedKey{{
			Key:     "managed",
			Raw:     `"cairn"`,
			EntryID: "cairn-toml-managed",
			GroupID: "cairn:install-docs",
		}},
	})
	if err != nil {
		return scenarioResult{}, err
	}
	fsTree.Entries = append(fsTree.Entries,
		fileEntry("managed/config.json", jsonDoc.Bytes, 0o600, "cairn-json-managed", "cairn:install-docs"),
		fileEntry("managed/config.toml", tomlDoc.Bytes, 0o600, "cairn-toml-managed", "cairn:install-docs"),
	)
	defs := []agentcontext.AuthoredRecipe{{
		ID: "cairn-base",
		Documents: []agentcontext.Document{{ID: "instructions", Path: "AGENTS.md", Sections: []agentcontext.Section{{
			ID: "base", Title: "## Base", Content: "synthetic Cairn base profile",
		}}}},
	}, {
		ID:        "cairn-install",
		Artifacts: fsTree,
		Documents: []agentcontext.Document{{ID: "instructions", Sections: []agentcontext.Section{{
			ID: "install", Title: "## Install", Content: "filesystem tree plus managed JSON/TOML install",
		}}}},
	}}
	prepared, err := agentlaunch.ResolvePreparation(ctx, agentlaunch.PrepareRequest{
		Kind:   agentlaunch.PrepareInputAuthoredRecipe,
		Recipe: &agentcontext.AuthoredRecipe{ID: "cairn-root", Base: "cairn-base", Parts: []agentcontext.PartRef{{ID: "cairn-install"}}},
		Roots:  roots(base, bootRoot),
		Projection: agentlaunch.ProviderProjection{Bindings: agentlaunch.ExecutionBindings{
			Argv: []string{"/bin/sh", "-c", "exit 0"},
			CWD:  filepath.Join(base, "project"),
			Env:  map[string]agentlaunch.EnvVar{"CAIRN_BOOT": {Value: bootRoot, Source: "synthetic-cairn"}},
		}},
		Access: agentlaunch.AccessRequirements{Mode: agentlaunch.AccessDisabled, Host: agentlaunch.ExecutionHostLocal},
	}, agentlaunch.WithCompositionDefinitions(defs))
	if err != nil {
		return scenarioResult{}, err
	}
	if err := prepared.Validate(); err != nil {
		return scenarioResult{}, err
	}
	checks := []string{
		mustContainFile(filepath.Join(bootRoot, "AGENTS.md"), "filesystem tree plus managed JSON/TOML install"),
		mustMode(filepath.Join(bootRoot, "filesystem-tree", "bin", "install.sh"), 0o755),
		mustContainFile(filepath.Join(bootRoot, "managed", "config.json"), `"managed": "cairn"`),
		mustContainFile(filepath.Join(bootRoot, "managed", "config.toml"), `managed = "cairn"`),
	}
	if err := firstError(checks); err != nil {
		return scenarioResult{}, err
	}
	return scenarioResult{Scenario: "cairn", BootRoot: bootRoot, EntryPaths: handlePaths(prepared.Materialization), Checks: checks}, nil
}

func runNanite(ctx context.Context, base string) (scenarioResult, error) {
	bootRoot := filepath.Join(base, "boot")
	blob := []byte{0, 1, 2, 3, 255}
	ref := artifact.ImmutableRef{Store: "memory", Key: "blob.bin", Digest: artifact.DigestBytes(blob), SizeBytes: int64(len(blob))}
	blobTree, err := artifact.NewResolver(artifact.ResolverOptions{ImmutableStore: artifact.MemoryStore{Objects: map[string][]byte{ref.Key: blob}}}).ResolveArtifacts(ctx, artifact.SourceRequest{
		Source:            artifact.Source{Kind: artifact.SourceImmutableObject, Immutable: &artifact.ImmutableSource{Ref: ref}},
		DestinationPrefix: "skills/nanite/assets",
		OwnershipGroup:    "nanite:skill-package",
	})
	if err != nil {
		return scenarioResult{}, err
	}
	tree := artifact.Tree{Entries: append([]artifact.Entry{
		fileEntry("skills/nanite/SKILL.md", []byte("---\nname: nanite-synthetic\n---\nSynthetic Nanite skill\n"), 0o644, "nanite-skill-md", "nanite:skill-package"),
		{Path: "skills/nanite/empty", Kind: artifact.EntryDirectory, Mode: 0o755, Ownership: artifact.Ownership{EntryID: "nanite-empty", GroupID: "nanite:skill-package"}},
	}, blobTree.Entries...)}
	created, err := agentlaunch.MaterializeArtifacts(ctx, agentlaunch.ArtifactMaterializationRequest{
		TargetRoot: bootRoot,
		Roots:      roots(base, bootRoot),
		Artifacts:  tree,
		Operation:  materialize.OperationCreate,
		Generation: "gen-1",
	})
	if err != nil {
		return scenarioResult{}, err
	}
	refreshedTree := artifact.Tree{Entries: []artifact.Entry{
		fileEntry("skills/nanite/SKILL.md", []byte("---\nname: nanite-synthetic\n---\nRefreshed Nanite skill\n"), 0o644, "nanite-skill-md", "nanite:skill-package"),
		{Path: "skills/nanite/empty", Kind: artifact.EntryDirectory, Mode: 0o755, Ownership: artifact.Ownership{EntryID: "nanite-empty", GroupID: "nanite:skill-package"}},
	}}
	refreshed, err := agentlaunch.MaterializeArtifacts(ctx, agentlaunch.ArtifactMaterializationRequest{
		TargetRoot:         bootRoot,
		Roots:              roots(base, bootRoot),
		Artifacts:          refreshedTree,
		Operation:          materialize.OperationRefresh,
		ExpectedGeneration: created.Manifest.Generation,
		Generation:         "gen-2",
		Selection:          materialize.Selection{Groups: []string{"nanite:skill-package"}},
		Reconcile:          materialize.ReconcilePolicy{RemoveOwned: true},
	})
	if err != nil {
		return scenarioResult{}, err
	}
	checks := []string{
		mustContainFile(filepath.Join(bootRoot, "skills", "nanite", "SKILL.md"), "Refreshed Nanite skill"),
		mustAbsent(filepath.Join(bootRoot, "skills", "nanite", "assets", "blob.bin")),
		mustDir(filepath.Join(bootRoot, "skills", "nanite", "empty")),
	}
	if err := firstError(checks); err != nil {
		return scenarioResult{}, err
	}
	return scenarioResult{Scenario: "nanite", BootRoot: bootRoot, EntryPaths: handlePaths(refreshed), Checks: checks}, nil
}

func runTorque(ctx context.Context, base string) (scenarioResult, error) {
	copiedRoot := filepath.Join(base, "copied-source")
	bootRoot := filepath.Join(base, "boot")
	if err := writeFile(filepath.Join(copiedRoot, "task-assets", "notes.md"), []byte("copied task notes\n"), 0o644); err != nil {
		return scenarioResult{}, err
	}
	if err := os.MkdirAll(filepath.Join(copiedRoot, "task-assets", "empty"), 0o755); err != nil {
		return scenarioResult{}, err
	}
	copied, err := artifact.NewResolver(artifact.ResolverOptions{}).ResolveArtifacts(ctx, artifact.SourceRequest{
		Source:            artifact.Source{Kind: artifact.SourceFilesystemTree, Filesystem: &artifact.FilesystemSource{Root: copiedRoot}},
		DestinationPrefix: "tasks/T-100/copied",
		OwnershipGroup:    "torque:copied-tree",
	})
	if err != nil {
		return scenarioResult{}, err
	}
	tree := artifact.Tree{Entries: append([]artifact.Entry{
		fileEntry("tasks/T-100/task.json", []byte(`{"id":"T-100","loopback":true}`), 0o644, "torque-task-json", "torque:task"),
		fileEntry("tasks/T-100/AGENTS.md", []byte("Synthetic Torque task context\n"), 0o644, "torque-task-agents", "torque:task"),
	}, copied.Entries...)}
	prepared, err := agentlaunch.ResolvePreparation(ctx, agentlaunch.PrepareRequest{
		Kind:      agentlaunch.PrepareInputArtifacts,
		Artifacts: &tree,
		Roots:     roots(base, bootRoot),
		Projection: agentlaunch.ProviderProjection{Bindings: agentlaunch.ExecutionBindings{
			Argv: []string{"/bin/sh", "-c", "exit 0"},
			CWD:  filepath.Join(base, "project"),
			Env:  map[string]agentlaunch.EnvVar{"TORQUE_TASK_ID": {Value: "T-100", Source: "synthetic-torque"}},
		}, Effects: []agentlaunch.RuntimeEffect{{Kind: agentlaunch.RuntimeEffectLoopback, Name: "torque-loopback", Required: true, Owner: "torque"}}},
		Access: agentlaunch.AccessRequirements{
			Mode:       agentlaunch.AccessRequired,
			Host:       agentlaunch.ExecutionHostLocal,
			Roots:      roots(base, bootRoot),
			Filesystem: []agentlaunch.AccessPath{{Mode: agentlaunch.AccessRead, Root: agentlaunch.RootBoot, Path: bootRoot}, {Mode: agentlaunch.AccessWrite, Root: agentlaunch.RootProject, Path: filepath.Join(base, "project")}},
			Network:    agentlaunch.NetworkAccess{Loopback: true},
			Subprocess: agentlaunch.SubprocessAccess{Allowed: true},
		},
	})
	if err != nil {
		return scenarioResult{}, err
	}
	if err := prepared.Validate(); err != nil {
		return scenarioResult{}, err
	}
	checks := []string{
		mustContainFile(filepath.Join(bootRoot, "tasks", "T-100", "task.json"), `"loopback":true`),
		mustContainFile(filepath.Join(bootRoot, "tasks", "T-100", "copied", "task-assets", "notes.md"), "copied task notes"),
		mustDir(filepath.Join(bootRoot, "tasks", "T-100", "copied", "task-assets", "empty")),
	}
	if !prepared.Access.Network.Loopback || len(prepared.Effects) != 1 || prepared.Effects[0].Kind != agentlaunch.RuntimeEffectLoopback {
		checks = append(checks, "missing loopback access/effect")
	}
	if err := firstError(checks); err != nil {
		return scenarioResult{}, err
	}
	return scenarioResult{Scenario: "torque", BootRoot: bootRoot, EntryPaths: handlePaths(prepared.Materialization), Checks: checks, RuntimeNeeds: []string{"loopback"}}, nil
}

func runTether(ctx context.Context, base string) (scenarioResult, error) {
	copiedRoot := filepath.Join(base, "copied-source")
	bootRoot := filepath.Join(base, "boot")
	if err := writeFile(filepath.Join(copiedRoot, "shared", "prompt.md"), []byte("copied Tether prompt\n"), 0o644); err != nil {
		return scenarioResult{}, err
	}
	copied, err := artifact.NewResolver(artifact.ResolverOptions{}).ResolveArtifacts(ctx, artifact.SourceRequest{
		Source:            artifact.Source{Kind: artifact.SourceFilesystemTree, Filesystem: &artifact.FilesystemSource{Root: copiedRoot}},
		DestinationPrefix: "bundles/copied",
		OwnershipGroup:    "tether:copied-tree",
	})
	if err != nil {
		return scenarioResult{}, err
	}
	tree := artifact.Tree{Entries: append([]artifact.Entry{
		fileEntry("bundles/task-alpha/AGENTS.md", []byte("alpha task bundle\n"), 0o644, "tether-alpha", "tether:task-alpha"),
		fileEntry("bundles/task-beta/AGENTS.md", []byte("beta task bundle\n"), 0o644, "tether-beta", "tether:task-beta"),
	}, copied.Entries...)}
	result, err := (plant.SharedPlanter{}).Plant(ctx, bootRoot, plant.Spec{
		Artifacts: tree,
		Operation: materialize.OperationCreate,
		ProviderSettings: map[string][]byte{
			"claude": []byte(`{"provider":"claude","source":"synthetic-tether"}`),
			"codex":  []byte("model = \"synthetic\"\n"),
		},
	})
	if err != nil {
		return scenarioResult{}, err
	}
	checks := []string{
		mustContainFile(filepath.Join(bootRoot, "bundles", "task-alpha", "AGENTS.md"), "alpha task bundle"),
		mustContainFile(filepath.Join(bootRoot, "bundles", "task-beta", "AGENTS.md"), "beta task bundle"),
		mustContainFile(filepath.Join(bootRoot, "bundles", "copied", "shared", "prompt.md"), "copied Tether prompt"),
		mustContainFile(filepath.Join(bootRoot, ".claude", "settings.json"), "synthetic-tether"),
		mustContainFile(filepath.Join(bootRoot, ".codex", "config.toml"), "synthetic"),
	}
	if err := firstError(checks); err != nil {
		return scenarioResult{}, err
	}
	paths := append([]string(nil), result.PlannedFiles...)
	sort.Strings(paths)
	return scenarioResult{Scenario: "tether", BootRoot: bootRoot, EntryPaths: paths, Checks: checks}, nil
}

func roots(base, bootRoot string) agentlaunch.ExecutionRoots {
	return agentlaunch.ExecutionRoots{
		ProjectRoot: filepath.Join(base, "project"),
		BootRoot:    bootRoot,
		StateRoot:   filepath.Join(base, "state"),
		ScratchRoot: filepath.Join(base, "scratch"),
		CWD:         filepath.Join(base, "project"),
	}
}

func fileEntry(path string, data []byte, mode os.FileMode, entryID, groupID string) artifact.Entry {
	return artifact.Entry{
		Path:  path,
		Kind:  artifact.EntryFile,
		Mode:  mode,
		Bytes: append([]byte(nil), data...),
		Ownership: artifact.Ownership{
			EntryID: entryID,
			GroupID: groupID,
		},
		Provenance: artifact.Provenance{Source: "go-agent-wrapper/examples/shared-conformance"},
	}
}

func handlePaths(handle *materialize.Handle) []string {
	if handle == nil {
		return nil
	}
	paths := make([]string, 0, len(handle.Manifest.Entries))
	for _, entry := range handle.Manifest.Entries {
		paths = append(paths, entry.Path)
	}
	sort.Strings(paths)
	return paths
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

func mustContainFile(path, want string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		return fmt.Sprintf("%s missing %q", path, want)
	}
	return "ok: " + filepath.Base(path)
}

func mustMode(path string, want os.FileMode) string {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		return fmt.Sprintf("%s mode %o want %o", path, got, want)
	}
	return fmt.Sprintf("ok: %s mode %o", filepath.Base(path), want)
}

func mustDir(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("stat %s: %v", path, err)
	}
	if !info.IsDir() {
		return fmt.Sprintf("%s is not a directory", path)
	}
	return "ok: dir " + filepath.Base(path)
}

func mustAbsent(path string) string {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "ok: absent " + filepath.Base(path)
	}
	if err != nil {
		return fmt.Sprintf("stat %s: %v", path, err)
	}
	return fmt.Sprintf("%s exists", path)
}

func firstError(checks []string) error {
	for _, check := range checks {
		if !strings.HasPrefix(check, "ok:") {
			return errors.New(check)
		}
	}
	return nil
}
