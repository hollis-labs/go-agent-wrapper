// Package adapters defines the provider-integration boundary the wrapper
// uses to launch a specific CLI agent (Claude Code, Codex app-server,
// OpenCode, ...).
//
// An Adapter answers three questions:
//
//  1. What process should we spawn? (Binary, Args, Env, Cwd via Resolve.)
//  2. What runtime channel does it speak natively? (PTY, streaming-stdio,
//     JSON-RPC, HTTP/SSE — declared in Descriptor.)
//  3. How do we turn its native output into runtimeevents.Events?
//     (Per-provider observer code, layered on top of the channel.)
//
// Hosts normally obtain an Adapter from launch.Select, which picks a runtime
// by go-providers registry id and mode and returns its native or ACP adapter.
// Concrete adapters also ship as sibling subpackages: native claude, codex
// and opencode, which compose with github.com/hollis-labs/go-providers and
// agentkit's agentsessions runtimes, and the ACP clients claudeacp,
// codexacp, copilotacp, opencodeacp and piacp. This package owns only the
// contract.
package adapters
