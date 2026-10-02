// Package turnoutput reduces a session's event stream to one normalized record
// per completed turn: the text the agent addressed to the user, what kind of
// message it was, how it stopped and how far to trust the text.
//
// A host builds one [Reducer] per session and feeds it events as they happen.
// Each feed method returns an [Output] exactly when a turn ends, at most one
// per turn and never twice for the same turn:
//
//   - [Reducer.Observe] takes go-runtime-events envelopes: the wrapper's
//     Activity sink, and every ACP runtime.
//   - [Reducer.ObserveProvider] takes go-providers typed events, which is what
//     agentkit's StartOptions.TypedEventCallback delivers. It is synchronous
//     and lossless, so it is the feed to use for native sessions.
//   - [Reducer.ObserveStream] takes the legacy llmtypes.StreamEvent of
//     StartOptions.EventFanout. agentkit drops events when that channel is
//     full, and a dropped final delta or terminal event corrupts the turn, so
//     prefer ObserveProvider unless the channel is drained generously.
//   - [Reducer.Flush] ends a turn the runtime never finished, for a host that
//     learns the session is gone without a terminal event.
//
// # What Text is
//
// Reasoning, tool calls, tool results and deltas of other blocks are excluded
// by construction. A text block is the run of deltas that share a block id; with
// no block id it is a run of consecutive deltas that no tool or permission event
// interrupts. Text is chosen in this order:
//
//  1. text the runtime carries on its terminal event (Claude's result.result,
//     turn.completed's "text" payload field, an EventDone's Content): exact;
//  2. otherwise the last block a delta marked phase "final": exact;
//  3. otherwise the last non-thought block of the turn: heuristic.
//
// A runtime that gives neither marker, such as an ACP agent whose blocks are all
// phase "message", therefore reports heuristic, and a consumer that cares can
// treat that as a hint rather than a fact. Text is trimmed of surrounding
// whitespace.
//
// # When Text is empty
//
// The reducer always returns an Output for a completed turn; it does not decide
// what is worth publishing. Text is empty when nothing qualified:
//
//   - kind final: the turn produced no assistant text (a tool-only turn, or the
//     runtime reported none). confidence is heuristic. A sink that publishes to
//     people will want to skip these.
//   - kind failure: the text is the runtime's error message; empty only when it
//     failed the turn without one.
//   - kind terminal: the partial output if there is any, else the reason
//     (interrupted, process_exited); empty only when neither exists.
//   - kind question or approval: text after the signal if the agent wrote any,
//     else a description built from the signal itself, so it is empty only when
//     the signal carried nothing.
//
// # Kind
//
// A turn.failed (or Error) is a failure, except that reason "interrupted" or
// "process_exited", or a stop_reason of llmtypes.StopReasonCancelled, makes it
// terminal; a completed turn that stopped that way is terminal too. A question
// tool call (see [Config.QuestionTools]) or a Codex requestUserInput request in
// the turn makes it a question. A permission request that is unresolved or refused, or
// an agent.permission_denied, makes it an approval. Anything else is final.
// Failure and terminal win over question, and question over approval.
//
// A session runs one turn at a time, so a new turn id replaces a turn that never
// finished: its buffered text is dropped without an Output.
package turnoutput
