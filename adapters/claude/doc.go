// Package claude is the wrapper adapter for Claude Code's
// streaming-stdio runtime mode.
//
// It implements [adapters.Adapter] by declaring the provider×runtime
// pair (claude × streaming-stdio) and resolving the exec shape
// (binary + args + env + cwd) for one invocation. The wrapper
// composes this with [agentkit/agentsessions.NewFromAdapter] +
// Caps.StreamingStdio at session start time; this package does not
// import agentkit directly so it stays usable as a pure exec-shape
// declaration regardless of which runtime the wrapper picks.
//
// Target CLI invocation (per the agentkit/agentsessions doc):
//
//	claude -p --input-format stream-json --output-format stream-json --verbose
//
// PTY allocation is intentionally rejected — Claude's streaming-stdio
// mode is incompatible with a PTY parent. Callers that need a Claude
// PTY session should use a different (forthcoming) PTY-shaped adapter.
package claude
