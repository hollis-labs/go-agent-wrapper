package plant

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/artifact"
	"github.com/hollis-labs/agentkit/materialize"
)

// Planter lays down per-session boot files into a boot dir the wrapper
// owns. The wrapper calls Plant once before exec; the returned Result
// reports what was actually written so the activity stream can emit a
// plant.completed event with the manifest.
type Planter interface {
	Plant(ctx context.Context, bootDir string, spec Spec) (Result, error)
}

// Spec describes what to plant. All fields are optional — an empty Spec
// is a valid no-op planting.
type Spec struct {
	// Artifacts is the modern, lossless input path. Entries preserve modes,
	// binary bytes, explicit directories, ownership and provenance. Legacy
	// fields below are adapted into additional artifact entries with explicit
	// compatibility defaults.
	Artifacts artifact.Tree

	// Operation selects create/reconcile/refresh behavior for SharedPlanter.
	// Empty defaults to materialize.OperationReconcile for compatibility with
	// the wrapper's historical pre-created boot directories.
	Operation materialize.Operation

	// Generation and ExpectedGeneration are forwarded to the shared engine so
	// callers can make refresh/reconcile concurrency explicit.
	Generation         string
	ExpectedGeneration string
	Selection          materialize.Selection
	Reconcile          materialize.ReconcilePolicy

	// Files maps relative paths inside the boot dir to file contents.
	// Existing files at the same path are overwritten under reconcile. Legacy
	// map entries are written with mode 0644 and wrapper legacy ownership.
	Files map[string][]byte

	// MCPConfig, when non-nil, is the .mcp.json content to plant.
	// Shortcut for adding ".mcp.json" to Files, with mode 0600.
	MCPConfig []byte

	// ProviderSettings maps provider name ("claude", "codex",
	// "opencode") to the provider-specific settings-file content. The
	// SharedPlanter places each file at the path the provider expects
	// inside the boot dir's planted HOME.
	ProviderSettings map[string][]byte

	// Hooks lists provider-specific hooks to install (Claude Code
	// PreToolUse/PostToolUse, OpenCode plugin entry points, ...).
	// Encoding is provider-specific; legacy Hook entries are written as
	// executable files under hooks/<provider>/<name>.
	Hooks []Hook

	// RecoveryPrompt is a per-session prompt the wrapper can re-inject
	// when a session needs to resume context after restart.
	RecoveryPrompt string
}

// Hook is a provider-specific hook the planter should install.
type Hook struct {
	Provider string
	Name     string
	Payload  []byte
}

// Result is what a [Planter] reports after planting completes.
type Result struct {
	// PlantedFiles lists the absolute paths of files the Planter wrote,
	// updated, changed mode for, or removed. Kept for compatibility with the
	// original wrapper event payload.
	PlantedFiles []string

	// PlannedFiles lists every desired file/directory path considered by the
	// shared engine.
	PlannedFiles []string

	// WrittenFiles lists created, updated, mode-changed and removed paths.
	WrittenFiles []string

	// UnchangedFiles lists paths that were already correct.
	UnchangedFiles []string

	// ConflictFiles lists paths the engine refused because ownership or current
	// content did not match the requested operation.
	ConflictFiles []string

	// Operation and Complete mirror the shared materialization report.
	Operation materialize.Operation
	Complete  bool

	// Handle is the shared engine result for callers that need manifest,
	// ownership or detailed per-entry change data.
	Handle *materialize.Handle
}

// SharedPlanter adapts wrapper Spec into agentkit's neutral artifact and
// materialization engine.
type SharedPlanter struct {
	Engine materialize.Engine
}

// Plant implements [Planter].
func (p SharedPlanter) Plant(ctx context.Context, bootDir string, spec Spec) (Result, error) {
	tree, err := specArtifactTree(spec)
	if err != nil {
		return Result{}, err
	}
	if len(tree.Entries) == 0 {
		return Result{Operation: effectiveOperation(spec), Complete: true}, nil
	}
	handle, err := agentlaunch.MaterializeArtifacts(ctx, agentlaunch.ArtifactMaterializationRequest{
		TargetRoot:         bootDir,
		Roots:              agentlaunch.ExecutionRoots{BootRoot: bootDir},
		Artifacts:          tree,
		Operation:          effectiveOperation(spec),
		Generation:         spec.Generation,
		ExpectedGeneration: spec.ExpectedGeneration,
		Selection:          spec.Selection,
		Reconcile:          spec.Reconcile,
		Engine:             p.Engine,
	})
	result := resultFromHandle(bootDir, handle)
	if err != nil {
		return result, err
	}
	return result, nil
}

// NoOpPlanter writes nothing. Use as a placeholder when the wrapper is
// wired but no planting is configured.
type NoOpPlanter struct{}

// Plant implements [Planter].
func (NoOpPlanter) Plant(_ context.Context, _ string, _ Spec) (Result, error) {
	return Result{}, nil
}

func effectiveOperation(spec Spec) materialize.Operation {
	if spec.Operation != "" {
		return spec.Operation
	}
	return materialize.OperationReconcile
}

func specArtifactTree(spec Spec) (artifact.Tree, error) {
	entries := append([]artifact.Entry(nil), spec.Artifacts.Entries...)
	for rel, content := range spec.Files {
		entries = append(entries, legacyFileEntry(rel, content, 0o644, "file:"+rel))
	}
	if spec.MCPConfig != nil {
		entries = append(entries, legacyFileEntry(".mcp.json", spec.MCPConfig, 0o600, "mcp"))
	}
	for provider, content := range spec.ProviderSettings {
		rel := providerSettingsPath(provider)
		entries = append(entries, legacyFileEntry(rel, content, 0o600, "provider-settings:"+provider))
	}
	for _, hook := range spec.Hooks {
		rel, err := hookPath(hook)
		if err != nil {
			return artifact.Tree{}, err
		}
		entries = append(entries, legacyFileEntry(rel, hook.Payload, 0o700, "hook:"+hook.Provider+":"+hook.Name))
	}
	if spec.RecoveryPrompt != "" {
		entries = append(entries, legacyFileEntry("recovery.md", []byte(spec.RecoveryPrompt), 0o600, "recovery"))
	}
	if len(entries) == 0 {
		return artifact.Tree{}, nil
	}
	normalized, err := artifact.Normalize(entries)
	if err != nil {
		return artifact.Tree{}, err
	}
	return artifact.Tree{Entries: normalized, Provenance: spec.Artifacts.Provenance}, nil
}

func legacyFileEntry(rel string, content []byte, mode fs.FileMode, entryID string) artifact.Entry {
	return artifact.Entry{
		Path:  rel,
		Kind:  artifact.EntryFile,
		Mode:  mode,
		Bytes: append([]byte(nil), content...),
		Ownership: artifact.Ownership{
			EntryID: "wrapper:legacy:" + entryID,
			GroupID: "wrapper:legacy",
		},
		Provenance: artifact.Provenance{Source: "go-agent-wrapper.plant.Spec"},
	}
}

func providerSettingsPath(provider string) string {
	switch strings.ToLower(provider) {
	case "claude":
		return ".claude/settings.json"
	case "codex":
		return ".codex/config.toml"
	case "opencode":
		return ".config/opencode/opencode.json"
	default:
		clean := strings.Trim(strings.ToLower(provider), "/")
		if clean == "" {
			clean = "unknown"
		}
		return filepath.ToSlash(filepath.Join(".config", clean, "settings"))
	}
}

func hookPath(h Hook) (string, error) {
	provider := strings.Trim(strings.ToLower(h.Provider), "/")
	name := strings.Trim(h.Name, "/")
	if provider == "" || name == "" {
		return "", fmt.Errorf("plant: hook provider and name are required")
	}
	return filepath.ToSlash(filepath.Join("hooks", provider, name)), nil
}

func resultFromHandle(bootDir string, handle *materialize.Handle) Result {
	if handle == nil {
		return Result{}
	}
	result := Result{
		Operation: handle.Report.Operation,
		Complete:  handle.Report.Complete,
		Handle:    handle,
	}
	for _, entry := range handle.Manifest.Entries {
		result.PlannedFiles = append(result.PlannedFiles, filepath.Join(bootDir, filepath.FromSlash(entry.Path)))
	}
	for _, change := range handle.Report.Changes {
		abs := filepath.Join(bootDir, filepath.FromSlash(change.Path))
		switch change.Kind {
		case materialize.ChangeUnchanged:
			result.UnchangedFiles = append(result.UnchangedFiles, abs)
		case materialize.ChangeConflict:
			result.ConflictFiles = append(result.ConflictFiles, abs)
		default:
			result.WrittenFiles = append(result.WrittenFiles, abs)
		}
	}
	sort.Strings(result.PlannedFiles)
	sort.Strings(result.WrittenFiles)
	sort.Strings(result.UnchangedFiles)
	sort.Strings(result.ConflictFiles)
	result.PlantedFiles = append([]string(nil), result.WrittenFiles...)
	return result
}
