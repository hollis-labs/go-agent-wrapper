// Package plant defines the wrapper's pre-exec planting contract: lay
// down per-session boot directories, provider settings files, MCP
// configs, hooks/plugins, recovery prompts, and task bundles before the
// wrapped process starts.
//
// The actual planting machinery lives in agentkit:
//
//   - github.com/hollis-labs/agentkit/agentsessions for the boot-dir
//     layout (bootdir_planting.go)
//   - github.com/hollis-labs/agentkit/agentlaunch/providerplant for
//     provider-specific settings, MCP configs, and hooks
//   - github.com/hollis-labs/agentkit/agentruntime/bootdir for the
//     runtime-side bootdir helpers
//
// This package owns the wrapper-facing contract so apps can describe
// what they want planted without reaching into the lower-level libs.
// SharedPlanter adapts Spec to agentkit's neutral artifact tree and
// materialization engine. Callers that already have shared artifacts
// should pass Spec.Artifacts and, when needed, an explicit
// materialize.Operation. Empty Operation defaults to reconcile so the
// wrapper's historical pre-created boot directories continue to work.
//
// The legacy Spec fields remain supported and are converted into
// artifact entries with stable compatibility defaults: Files use mode
// 0644, MCPConfig and ProviderSettings use mode 0600, Hooks use mode
// 0700 under hooks/<provider>/<name>, and RecoveryPrompt is written as
// recovery.md with mode 0600. Legacy entries carry wrapper-owned
// artifact ownership so create/reconcile conflicts are detected by the
// shared engine instead of being silently overwritten.
package plant
