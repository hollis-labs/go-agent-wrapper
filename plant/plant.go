package plant

import "context"

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
	// Files maps relative paths inside the boot dir to file contents.
	// Existing files at the same path are overwritten.
	Files map[string][]byte

	// MCPConfig, when non-nil, is the .mcp.json content to plant.
	// Shortcut for adding ".mcp.json" to Files.
	MCPConfig []byte

	// ProviderSettings maps provider name ("claude", "codex",
	// "opencode") to the provider-specific settings-file content. The
	// Planter places each file at the path the provider expects
	// (e.g., ~/.claude/settings.json inside the boot dir's planted
	// HOME).
	ProviderSettings map[string][]byte

	// Hooks lists provider-specific hooks to install (Claude Code
	// PreToolUse/PostToolUse, OpenCode plugin entry points, ...).
	// Encoding is provider-specific; the Planter passes them through
	// to the right destination.
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
	// PlantedFiles lists the absolute paths of every file the Planter
	// wrote (or overwrote). Sorted for deterministic logs.
	PlantedFiles []string
}

// NoOpPlanter writes nothing. Use as a placeholder when the wrapper is
// wired but no planting is configured.
type NoOpPlanter struct{}

// Plant implements [Planter].
func (NoOpPlanter) Plant(_ context.Context, _ string, _ Spec) (Result, error) {
	return Result{}, nil
}
