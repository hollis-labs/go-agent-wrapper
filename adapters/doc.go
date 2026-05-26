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
// Concrete adapters (claude, codex, opencode) ship as sibling files /
// subpackages and compose with the lower-level
// github.com/hollis-labs/go-providers and go-agent-sessions runtimes.
// This package owns only the contract.
package adapters
