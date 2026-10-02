package turnoutput

import "strings"

// A question or an approval is something in the turn that needs the user, and
// no runtime says so when one is waiting: in headless mode they all refuse
// automatically, and the agent carries on. So a turn is reported as a question or
// an approval only if it ended on the signal. What the reducer works from, per
// runtime, is the runtime's own record of the refusal (Claude's
// result.permission_denials and agy's denied_actions as PermissionDenied events,
// Codex's declined item, a refused permission request, a question tool call), and
// "ended on it" is its own inference: no tool call that was not itself refused
// came after it. An agent that kept working after a refusal worked around it, and
// the turn is final.

// maxTools and maxSignals bound what one turn remembers about its tool calls and
// signals; the oldest go first.
const (
	maxTools   = 256
	maxSignals = 64
)

// tool is one tool call of a turn.
type tool struct {
	id string
	// blocksAt is how many text blocks the turn had when the call was made, so
	// text written after it can be told from text before it.
	blocksAt int
	// refused is set when the runtime reported refusing this call.
	refused bool
	// question is set for a call to a question tool: it asks, it does not work.
	question bool
	// result is what the call's result said: resultNone until one arrives.
	result toolResult
}

type toolResult int

const (
	resultNone toolResult = iota
	resultOK
	resultError
)

// signal is something in the turn that needs the user: a question or an
// approval. anchor is the index of the tool call it sits on (-1 when it came
// before any), at how many blocks the turn had there.
type signal struct {
	kind   Kind
	text   string
	at     int
	anchor int
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
		t.toolsShifted()
	}
	t.tools = append(t.tools, tool{id: id, blocksAt: len(t.blocks), question: question})
	return len(t.tools) - 1
}

// toolIndex finds the call with id, or -1.
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

// toolResult records what a tool call's result said.
func (t *turn) toolResult(id string, isError bool) {
	if i := t.toolIndex(id); i >= 0 && t.tools[i].result == resultNone {
		t.tools[i].result = resultOK
		if isError {
			t.tools[i].result = resultError
		}
	}
}

// raiseQuestion records that the agent asked the user something, on the tool
// call at anchor.
func (t *turn) raiseQuestion(text string, anchor int) {
	at := len(t.blocks)
	if anchor >= 0 && anchor < len(t.tools) {
		at = t.tools[anchor].blocksAt
	}
	t.addSignal(&signal{kind: KindQuestion, text: clip(text), at: at, anchor: anchor})
}

// raiseApproval records a refusal: of the call whose id the runtime names, or,
// with none, of whatever the agent did last.
func (t *turn) raiseApproval(text, toolUseID string) {
	sig := &signal{kind: KindApproval, text: clip(text), at: len(t.blocks), anchor: len(t.tools) - 1}
	if i := t.toolIndex(toolUseID); i >= 0 {
		t.tools[i].refused = true
		sig.anchor, sig.at = i, t.tools[i].blocksAt
	}
	t.addSignal(sig)
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
		sig: signal{kind: KindApproval, text: clip(text), at: len(t.blocks), anchor: len(t.tools) - 1},
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
	sig := signal{kind: KindApproval, text: clip(text), at: len(t.blocks), anchor: len(t.tools) - 1}
	if idx >= 0 {
		sig = t.pending[idx].sig
		t.pending = append(t.pending[:idx], t.pending[idx+1:]...)
	}
	if !allowed {
		t.addSignal(&sig)
	}
}

// open reports whether the turn ended on the signal: nothing the agent did after
// it counts as working past it. A tool call that was refused, or that only asked
// a question, is not working; any other tool call after it is. A question whose
// tool call came back answered is no longer waiting.
func (t *turn) open(sig *signal) bool {
	if sig.kind == KindQuestion && sig.anchor >= 0 && sig.anchor < len(t.tools) && t.tools[sig.anchor].result == resultOK {
		return false
	}
	for i := sig.anchor + 1; i < len(t.tools); i++ {
		if tl := t.tools[i]; !tl.refused && !tl.question {
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
// the turn's ordinary pick.
func (t *turn) signalText(sig *signal, termText, pickText string, pickConf Confidence) (string, Confidence) {
	if t.hasTextAfter(sig.at) {
		if s, c := t.pick(sig.at, termText); s != "" {
			return s, c
		}
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

// blocksShifted keeps block positions right after the oldest block was dropped.
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

// toolsShifted keeps anchors right after the oldest tool call was dropped.
func (t *turn) toolsShifted() {
	for _, sig := range t.signals {
		if sig.anchor > -1 {
			sig.anchor--
		}
	}
	for i := range t.pending {
		if t.pending[i].sig.anchor > -1 {
			t.pending[i].sig.anchor--
		}
	}
}
