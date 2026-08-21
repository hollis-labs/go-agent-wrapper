// Package sidebyside is TASKS/agent-host-acp/17 (Nanite repo,
// hollis-labs/apps/nanite)'s real, executed native-vs-ACP comparison
// harness: it drives matched real sessions through both go-agent-wrapper's
// native Claude adapter ([github.com/hollis-labs/go-agent-wrapper/adapters/claude],
// wrapper.Wrapper-driven) and its bridge-mediated ACP Claude adapter
// ([github.com/hollis-labs/go-agent-wrapper/adapters/claudeacp], acp.Client-driven)
// with the SAME real prompts, in the SAME test run, and reports the four
// dimensions docs/engineering/architecture/17-acp.md names — activity
// fidelity, interrupt fidelity, tool reporting, latency — with real numbers
// observed contemporaneously rather than compiled from each adapter's own
// separately-run live tests.
//
// Why Claude, not Codex or OpenCode (task 17's own "your call, document
// why"): Claude's native adapter shape (adapters/claude, streaming-stdio)
// is the one Nanite's own production nativeAdapter (internal/runtime/agent/
// wrapper_adapter.go, sibling repo) actually runs unmodified — real
// end-to-end mileage via TASKS/agent-host-acp/07's dogfeed (both runs).
// Codex's shipped go-agent-wrapper adapters/codex package selects the
// app-server JSON-RPC runtime shape, which — per wrapper_adapter.go's own
// doc comment in the sibling Nanite repo — Nanite's production code
// deliberately does NOT use (it drives Codex via the implicit
// subprocess-per-turn "adapter runtime" / `codex exec` instead); driving
// adapters/codex's app-server shape for real would be genuinely
// unvalidated ground with no established turn-composition precedent
// anywhere in this batch (Wrapper.Config.AutoFireFirstTurn/SendInput are
// raw-bytes escape hatches — real app-server turns need thread/turn
// JSON-RPC calls neither this package nor Nanite's own code composes
// today). Claude native vs. Claude ACP is also exactly the pairing task
// 17's own dispatch prompt names as carrying "the most interesting
// contrast" (native InterruptProcess — kill-only — vs. ACP-bridged
// InterruptTurn — genuine mid-turn interrupt), and both sides already have
// real, live-verified single-path precedent (adapters/claude via task 07's
// dogfeed; adapters/claudeacp via task 13's own live_test.go) this package
// builds on rather than duplicates.
//
// All tests here spawn REAL processes — the real `claude` CLI directly for
// the native path, and the real `npx -y @agentclientprotocol/claude-agent-acp`
// bridge (which itself drives the Claude Agent SDK using whatever
// credentials are already configured on the host) for the ACP path. They
// skip (not fail) when a required real dependency (the `claude` binary,
// `npx`/Node.js, or live auth) is unavailable, matching every other live
// test in this repo (adapters/{claudeacp,codexacp,opencodeacp,piacp}/live_test.go).
//
// See TASKS/agent-host-acp/17-native-vs-acp-side-by-side-comparison.md
// (Nanite repo) for this task's own Work Log, which reproduces the numbers
// this package's tests print via t.Logf, plus this comparison's full
// write-up against all four named dimensions.
package sidebyside
