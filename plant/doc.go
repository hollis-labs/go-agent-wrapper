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
// what they want planted without reaching into the lower-level libs;
// the wrapper's Planter implementation composes those agentkit
// packages.
package plant
