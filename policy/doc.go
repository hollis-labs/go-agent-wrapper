// Package policy defines the wrapper's command/tool-call governance
// surface: observe, nudge, rewrite, block, and approval modes.
//
// The wrapper does not ship policy rules. Apps plug in a [Store] —
// file-backed, database-backed, dynamic, whatever they need — and the
// wrapper calls it via the [Engine] interface at the right interception
// points (PreToolUse hooks, JSON-RPC requests, OpenCode plugin events,
// or PTY text classifiers where no semantic channel is available).
//
// Capability matters. The wrapper records which channel produced the
// observation; the engine must respect [Decision.Confidence] and refuse
// to enforce rewrite/block on inferred observations alone unless the
// rule explicitly allows it. See the architecture note
// "CLI Runner / Wrapper Architecture" for the capability matrix.
//
// Every non-observe Decision the wrapper applies must be surfaced both
// to the agent (as a visible policy.rewrite / policy.nudge / policy.block
// runtime event) and to the audit log. Silent rewrites are explicitly
// prohibited — they corrupt the agent's debugging model.
package policy
