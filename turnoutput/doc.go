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
// # Turn ids and repeated events
//
// Observe tracks turns by the id on the envelope: a turn that starts before the
// previous one's terminal event arrives keeps its own text, and an event that
// arrives late for a turn already reported is dropped, never reopening it. A
// session runs one turn at a time, so a producer that loses terminal events
// leaves turns buffered; the Reducer holds at most 64 and drops the oldest.
//
// The provider and stream feeds carry no turn id, so the reducer decides by
// whether a turn is in progress. A turn starts at its first event and ends at
// Done or Error. A Done that arrives with no turn in progress is either a turn
// that had no events of its own (an interrupted one, say) or a repeat of the Done
// that ended the last turn. It is a repeat only if the last turn was completed
// and this one ends the same way, with the same stop reason and text; a
// cancellation is never a repeat, so an interrupted turn that said nothing is
// reported and a lone terminal event is never lost. A stop reason that arrives
// with no turn in progress is held until the next event: a terminal event takes
// it (the stream feed reports a stop reason on a usage event ahead of the done),
// any other drops it (it trailed the turn that ended). An Error with no turn in
// progress is reported as a failure of its own, so a startup failure is never
// lost, unless it is the same error as the one just reported. A lossy feed
// (ObserveStream) that drops a terminal event makes the next turn's events join
// the unfinished one, which the reducer cannot detect.
//
// A turn's buffer is bounded for the case where its terminal event is lost: the
// oldest text blocks go first (the last never does), a single block is cut at
// 1 MiB, a turn keeps at most 4 MiB of text and 128 blocks, and the question or
// approval text is cut at 8 KiB.
//
// # Interrupted turns differ by feed
//
// Observe is told by the wrapper when a turn it interrupted ends (turn.failed
// with reason "interrupted") and reports it as terminal. The provider and stream
// feeds carry only what the runtime says: a Codex app-server turn ended by an
// interrupt is a done with the stop reason llmtypes.StopReasonCancelled, so it is
// terminal too, but a runtime that ends an interrupted turn with an error reports
// a failure. A host that interrupts turns itself and wants every one reported as
// terminal should ask the runtime for the stop reason, or end the turn with
// Flush.
//
// A question tool is judged by the call alone: if the tool is refused (headless
// Claude has no one to ask) and the agent carries on and finishes, the turn is
// still reported as a question, with the agent's later text as its Text.
package turnoutput
