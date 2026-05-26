// Package wrapper is the top-level entry point for go-agent-wrapper — a
// shared harness for launching, observing, and governing CLI-agent
// subprocesses across Hollis Labs apps.
//
// The wrapper composes the agentkit umbrella module
// (github.com/hollis-labs/agentkit, which absorbed go-agent-sessions /
// go-agent-launch / go-agent-runtime / go-agent-context / go-agent-broker
// in v0.1.0, 2026-05-26) plus the still-standalone primitives
// (github.com/hollis-labs/go-runner for process supervision,
// github.com/hollis-labs/go-providers for provider adapters,
// github.com/hollis-labs/go-sandbox for sandbox profiles) into a single
// standardized execution boundary that owns:
//
//   - process launch (executable, args, env, cwd, process group, limits)
//   - IO proxying (stdin, stdout, stderr, PTY, terminal resize)
//   - planting (per-session boot dirs, MCP config, provider settings,
//     hooks/plugins, recovery prompts) — see [plant]
//   - sandboxing applied before exec — see [sandbox]
//   - runtime activity event emission — see [activity]
//   - policy enforcement (observe / nudge / rewrite / block / approval) —
//     see [policy]
//   - optional filter-pipeline integration (envelope repair, classifier,
//     command normalization) — see [filters]
//   - provider-specific adapters (Claude, Codex, OpenCode, ...) — see
//     [adapters]
//
// The wrapper does not prescribe prompt design, workflow logic, turn
// semantics, or agent cognition. Apps that import this package keep
// ownership of their own state machines and translate the wrapper's
// emitted [github.com/hollis-labs/go-runtime-events.Event] stream into
// their native event vocabulary.
//
// This file carries the package-level documentation; the exported API
// lives in the sibling source files.
package wrapper
