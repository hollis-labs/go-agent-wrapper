package turnoutput

import "strings"

// A question or an approval is something in the turn that needs the user, and
// no runtime says so when one is waiting: in headless mode they all refuse
// automatically, and the agent carries on. So a turn is reported as a question or
// an approval only if it ended on the signal. What the reducer works from, per
// runtime, is the runtime's own record of the refusal (Claude's
// result.permission_denials and agy's denied_actions as PermissionDenied events,
// Codex's declined item, a refused permission request, a question tool call), and
// "ended on it" is its own inference: the agent did no work after it. An agent that
// kept working after a refusal worked around it, and the turn is final.
//
// The facts a signal is judged by are collected as they arrive but resolved when
// the turn ends, keyed by tool call id, because they do not arrive in the order
// they happened: a refusal and a tool result are reported on a different path from
// the tool call they name, and can reach the reducer first.

// maxTools, maxSignals and maxEarly bound what one turn remembers about its tool
// calls, its signals and facts that arrived before the tool call they name; the oldest
// go first. A tool call's result and refusal live with the call and go when it does.
const (
	maxTools   = 256
	maxSignals = 64
	maxEarly   = 256
)

// tool is one tool call of a turn.
type tool struct {
	id string
	// blocksAt is how many text blocks the turn had when the call was made, so
	// text written after it can be told from text before it.
	blocksAt int
	// seq orders the call among the turn's tool calls and tool results, as they
	// arrived.
	seq int
	// question is set for a call to a question tool: it asks, it does not work.
	question bool

	// facts about the call, which may arrive before the call does (see early).
	facts
}

// facts is what the turn has learned about one tool call from other events: its
// result (the first one counts, and seq says where it fell among the turn's calls
// and results) and whether the runtime reported refusing it.
type facts struct {
	result    outcome
	hasResult bool
	refused   bool
}

// outcome is what a tool call's result said, and where it fell in the turn.
type outcome struct {
	isError bool
	seq     int
}

// signal is something in the turn that needs the user: a question or an
// approval.
//
// A signal is placed one of two ways. One that names a tool call (a question tool
// call, or a refusal that names the call it refused) is placed at finish by that
// call's id. One that sits at a point in the turn (a permission request, a
// question the runtime asked by request) is placed where it arrived: at is how many
// text blocks the turn had and seq how far along its tool calls and results.
type signal struct {
	kind Kind
	text string

	byTool bool   // placed by toolID at finish
	toolID string // the call: for a refusal, the one it refused; "" names none
	// toolCall is set for a call to a question tool, whose own result says whether
	// the question was answered.
	toolCall bool

	at  int
	seq int
}

// pendingRequest is a permission request that has not been resolved. ids are every
// id a resolution may name it by.
type pendingRequest struct {
	ids []string
	sig signal
}

// addTool records a tool call and returns its index.
func (t *turn) addTool(id string, question bool) int {
	if len(t.tools) >= maxTools {
		t.tools = t.tools[1:]
	}
	t.seq++
	tl := tool{id: id, blocksAt: len(t.blocks), seq: t.seq, question: question}
	// A result or a refusal can reach the reducer before the call it names.
	if f, ok := t.early[id]; ok && id != "" {
		tl.facts = *f
		t.forgetEarly(id)
	}
	t.tools = append(t.tools, tl)
	return len(t.tools) - 1
}

// factsFor returns the facts store for the call with id: the call's own when the
// turn has it, else a bounded holding place for the call to claim when it arrives.
func (t *turn) factsFor(id string) *facts {
	if i := t.toolIndex(id); i >= 0 {
		return &t.tools[i].facts
	}
	if f, ok := t.early[id]; ok {
		return f
	}
	if len(t.earlyOrder) >= maxEarly {
		t.forgetEarly(t.earlyOrder[0])
	}
	f := &facts{}
	t.early[id] = f
	t.earlyOrder = append(t.earlyOrder, id)
	return f
}

func (t *turn) forgetEarly(id string) {
	delete(t.early, id)
	for i, e := range t.earlyOrder {
		if e == id {
			t.earlyOrder = append(t.earlyOrder[:i], t.earlyOrder[i+1:]...)
			return
		}
	}
}

// toolIndex finds the newest call with id, or -1.
func (t *turn) toolIndex(id string) int {
	if id == "" {
		return -1
	}
	for i := len(t.tools) - 1; i >= 0; i-- {
		if t.tools[i].id == id {
			return i
		}
	}
	return -1
}

// toolResult records what a tool call's result said. The first result for a call
// is the one that counts: a later one (an ACP agent updates a call several times)
// does not overwrite it.
func (t *turn) toolResult(id string, isError bool) {
	if id == "" {
		return
	}
	f := t.factsFor(id)
	if f.hasResult {
		return
	}
	t.seq++
	f.result, f.hasResult = outcome{isError: isError, seq: t.seq}, true
}

// raiseQuestion records that the agent asked the user something by calling a
// question tool, the call at tools[idx].
func (t *turn) raiseQuestion(text string, idx int) {
	// The call was just recorded, so where the turn stands now is where it was
	// made: that is the placement of a call that carries no id.
	sig := &signal{kind: KindQuestion, text: clip(text), toolCall: true, at: len(t.blocks), seq: t.seq}
	if idx >= 0 && idx < len(t.tools) {
		sig.toolID = t.tools[idx].id
		sig.byTool = sig.toolID != ""
	}
	t.addSignal(sig)
}

// raiseRequestedQuestion records a question the runtime asked by request (Codex's
// requestUserInput). It has no tool call of its own: it sits where it arrived, after
// every tool call the turn has so far.
func (t *turn) raiseRequestedQuestion(text string) {
	t.addSignal(&signal{kind: KindQuestion, text: clip(text), at: len(t.blocks), seq: t.seq})
}

// raiseApproval records a refusal: of the call whose id the runtime names, or,
// with none, of whatever the agent did last.
func (t *turn) raiseApproval(text, toolUseID string) {
	if toolUseID != "" {
		t.factsFor(toolUseID).refused = true
	}
	t.addSignal(&signal{kind: KindApproval, text: clip(text), byTool: true, toolID: toolUseID})
}

func (t *turn) addSignal(sig *signal) {
	t.signals = append(t.signals, sig)
	if len(t.signals) > maxSignals {
		t.signals = t.signals[len(t.signals)-maxSignals:]
	}
}

// request records a permission request that has not been answered.
func (t *turn) request(ids []string, text string) {
	t.pending = append(t.pending, pendingRequest{
		ids: ids,
		sig: signal{kind: KindApproval, text: clip(text), at: len(t.blocks), seq: t.seq},
	})
	if len(t.pending) > maxPending {
		t.pending = t.pending[len(t.pending)-maxPending:]
	}
}

// resolve settles the pending request that any of ids names, or the oldest one
// when none does. A refusal is a signal: the agent went without a decision it
// needed.
func (t *turn) resolve(ids []string, allowed bool, text string) {
	idx := -1
	for i, p := range t.pending {
		if overlaps(p.ids, ids) {
			idx = i
			break
		}
	}
	if idx < 0 && len(t.pending) > 0 {
		idx = 0
	}
	sig := signal{kind: KindApproval, text: clip(text), at: len(t.blocks), seq: t.seq}
	if idx >= 0 {
		sig = t.pending[idx].sig
		t.pending = append(t.pending[:idx], t.pending[idx+1:]...)
	}
	if !allowed {
		t.addSignal(&sig)
	}
}

// place says where a signal sits: at, how many text blocks the turn had there, and
// after, the point past which a tool call counts as the agent working on.
//
//   - A signal at a point (a request) sits where it arrived.
//   - A signal that names a tool call sits at that call, and the point is its
//     result when the turn has one: a call issued in the same batch before the
//     result came back was not a reaction to it. With no result, it is the call's
//     own start.
//   - A refusal that names no call stands for the last thing the agent did, which
//     is how agy reports its refusals (when the turn ends, naming none).
//   - A signal that names a call the turn does not have (it was dropped for age)
//     is older than anything the turn kept.
func (t *turn) place(sig *signal) (at, after int) {
	if !sig.byTool {
		return sig.at, sig.seq
	}
	if sig.toolID == "" {
		if n := len(t.tools); n > 0 {
			last := t.tools[n-1]
			return last.blocksAt, last.seq
		}
		return 0, 0
	}
	i := t.toolIndex(sig.toolID)
	if i < 0 {
		return 0, -1
	}
	after = t.tools[i].seq
	if tl := t.tools[i]; tl.hasResult {
		after = tl.result.seq
	}
	return t.tools[i].blocksAt, after
}

// open reports whether the turn ended on the signal: nothing the agent did after
// it counts as working past it. A tool call that was refused, or that only asked a
// question, is not working; any other call that started after the signal is. A
// question whose own tool call came back answered is no longer waiting.
func (t *turn) open(sig *signal) bool {
	if sig.toolCall && sig.toolID != "" {
		if i := t.toolIndex(sig.toolID); i >= 0 && t.tools[i].hasResult && !t.tools[i].result.isError {
			return false
		}
	}
	_, after := t.place(sig)
	for _, tl := range t.tools {
		if tl.seq > after && !tl.question && !tl.refused {
			return false
		}
	}
	return true
}

// openSignal returns the signal the turn ended on, if any: the last open
// question, else the last open approval, counting a permission request still
// unanswered at the end as an approval.
func (t *turn) openSignal() *signal {
	var question, approval *signal
	consider := func(sig *signal) {
		if !t.open(sig) {
			return
		}
		if sig.kind == KindQuestion {
			question = sig
		} else {
			approval = sig
		}
	}
	for _, sig := range t.signals {
		consider(sig)
	}
	for i := range t.pending {
		consider(&t.pending[i].sig)
	}
	if question != nil {
		return question
	}
	return approval
}

// signalText is the text of an output whose kind a signal decided: what the agent
// wrote after the signal (the terminal event's own text when it has one, which is
// then the last of what the agent wrote), else the signal's own description, else
// the turn's ordinary pick. A runtime that gave its final text but no deltas at
// all has nothing to place it by; its final message is the last thing the agent
// said, so it is used.
func (t *turn) signalText(sig *signal, termText, pickText string, pickConf Confidence) (string, Confidence) {
	at, _ := t.place(sig)
	if t.hasTextAfter(at) {
		if s, c := t.pick(at, termText); s != "" {
			return s, c
		}
	} else if s := strings.TrimSpace(termText); s != "" && len(t.blocks) == 0 {
		return s, ConfidenceExact
	}
	if sig.text != "" {
		return sig.text, ConfidenceExact
	}
	return pickText, pickConf
}

// hasTextAfter reports whether any text block from index from onward has text.
func (t *turn) hasTextAfter(from int) bool {
	if from > len(t.blocks) {
		return false
	}
	for _, b := range t.blocks[from:] {
		if strings.TrimSpace(b.text.String()) != "" {
			return true
		}
	}
	return false
}

// blocksShifted keeps block positions right after the oldest text block was
// dropped.
func (t *turn) blocksShifted() {
	for i := range t.tools {
		if t.tools[i].blocksAt > 0 {
			t.tools[i].blocksAt--
		}
	}
	for _, sig := range t.signals {
		if sig.at > 0 {
			sig.at--
		}
	}
	for i := range t.pending {
		if t.pending[i].sig.at > 0 {
			t.pending[i].sig.at--
		}
	}
}
