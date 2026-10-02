package turnoutput

import (
	"strings"
	"sync"

	llmtypes "github.com/hollis-labs/go-llm-types"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// finishedMemory is how many finished turn ids a Reducer remembers so that a
// late duplicate terminal event does not produce a second Output.
const finishedMemory = 16

// Reducer folds one session's events into an [Output] per completed turn. It is
// safe for concurrent use. Build one per session with [New].
type Reducer struct {
	mu        sync.Mutex
	sessionID string
	runtime   string
	newTurnID func() string
	questions map[string]struct{}

	cur      *turn
	finished []string
}

// New returns a Reducer for one session.
func New(cfg Config) *Reducer {
	r := &Reducer{
		sessionID: cfg.SessionID,
		runtime:   cfg.Runtime,
		newTurnID: cfg.NewTurnID,
		questions: map[string]struct{}{},
	}
	if r.newTurnID == nil {
		r.newTurnID = runtimeevents.NewTurnID
	}
	tools := cfg.QuestionTools
	if tools == nil {
		tools = DefaultQuestionTools
	}
	for _, name := range tools {
		r.questions[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
	}
	return r
}

// Flush ends the turn in progress with kind terminal, for a host that learns
// the session is gone without the runtime having reported a terminal event.
// reason says why (default "process_exited"). It reports false when no turn is
// in progress.
func (r *Reducer) Flush(reason string) (Output, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return Output{}, false
	}
	if strings.TrimSpace(reason) == "" {
		reason = reasonProcessExited
	}
	return r.finish(r.cur, terminal{failed: true, reason: reason}), true
}

const (
	reasonInterrupted   = "interrupted"
	reasonProcessExited = "process_exited"

	stopCancelled = llmtypes.StopReasonCancelled
	stopError     = llmtypes.StopReasonError
)

// terminal is what a turn's terminal event says.
type terminal struct {
	// failed is a turn.failed or Error; otherwise the turn completed.
	failed  bool
	errText string
	// reason is the producer's own reason for a failed turn ("interrupted",
	// "process_exited"); empty for an ordinary failure.
	reason     string
	stopReason string
	// text is the final text the runtime carried on the terminal event.
	text string
}

// open returns the turn with id, starting it when it is new. A session runs one
// turn at a time, so starting a turn abandons any that never finished.
func (r *Reducer) open(id string) *turn {
	if r.cur != nil && r.cur.id == id {
		return r.cur
	}
	r.cur = newTurn(id)
	return r.cur
}

// implicit returns the turn in progress, starting one with a minted id when
// there is none. Events with no turn id of their own belong to it.
func (r *Reducer) implicit() *turn {
	if r.cur == nil {
		r.cur = newTurn(r.newTurnID())
	}
	return r.cur
}

// lookup returns the turn a terminal event closes: the one in progress when id
// is empty or matches it, or a transient empty turn for an unknown id, which
// closes without disturbing the one in progress. It reports false for a turn
// already reported.
func (r *Reducer) lookup(id string) (*turn, bool) {
	if id == "" {
		return r.implicit(), true
	}
	if r.cur != nil && r.cur.id == id {
		return r.cur, true
	}
	for _, done := range r.finished {
		if done == id {
			return nil, false
		}
	}
	return newTurn(id), true
}

func (r *Reducer) finish(t *turn, term terminal) Output {
	if r.cur == t {
		r.cur = nil
	}
	r.finished = append(r.finished, t.id)
	if len(r.finished) > finishedMemory {
		r.finished = r.finished[len(r.finished)-finishedMemory:]
	}

	out := Output{
		SessionID:  r.sessionID,
		TurnID:     t.id,
		Runtime:    r.runtime,
		StopReason: term.stopReason,
	}
	if out.StopReason == "" {
		out.StopReason = t.usageStop
	}

	cut := (term.failed && (term.reason == reasonInterrupted || term.reason == reasonProcessExited)) ||
		out.StopReason == stopCancelled
	switch {
	case term.failed && !cut:
		out.Kind = KindFailure
		out.Text, out.Confidence = strings.TrimSpace(term.errText), ConfidenceExact
		if out.Text == "" {
			out.Text, out.Confidence = term.reason, ConfidenceHeuristic
		}
		if out.StopReason == "" {
			out.StopReason = stopError
		}
	case cut:
		out.Kind = KindTerminal
		out.Text, out.Confidence = t.pick(0, term.text)
		if out.Text == "" {
			out.Text, out.Confidence = strings.TrimSpace(term.errText), ConfidenceExact
		}
		if out.Text == "" {
			out.Text, out.Confidence = term.reason, ConfidenceHeuristic
		}
		if term.reason == reasonInterrupted {
			out.StopReason = stopCancelled
		}
		if out.StopReason == "" {
			out.StopReason = stopError
		}
	default:
		out.Kind = KindFinal
		out.Text, out.Confidence = t.pick(0, term.text)
		if sig := t.signal(); sig != nil {
			out.Kind = sig.kind
			out.Text, out.Confidence = t.signalText(sig, out.Text, out.Confidence)
		}
	}
	return out
}

// turn is the buffer for one turn in progress.
type turn struct {
	id     string
	blocks []*block
	byID   map[string]*block
	// anonOpen reports that the last block has no id and is still taking
	// deltas; a tool or permission event closes it.
	anonOpen  bool
	usageStop string

	question *signal
	approval *signal
	pending  []pendingRequest
}

func newTurn(id string) *turn {
	return &turn{id: id, byID: map[string]*block{}}
}

// block is one text block of a turn.
type block struct {
	id    string
	final bool
	text  strings.Builder
}

// signal is something in the turn that needs the user: a question or an
// approval. at is how many blocks existed when it was raised, so text written
// after it can be told from text before it.
type signal struct {
	kind Kind
	text string
	at   int
}

// pendingRequest is a permission request that has not been resolved. ids are
// every id a resolution may name it by.
type pendingRequest struct {
	ids []string
	sig signal
}

func (t *turn) addDelta(text, blockID string, final bool) {
	if text == "" {
		return
	}
	var b *block
	switch {
	case blockID != "":
		b = t.byID[blockID]
		if b == nil {
			b = &block{id: blockID}
			t.byID[blockID] = b
			t.blocks = append(t.blocks, b)
		}
		t.anonOpen = false
	case t.anonOpen:
		b = t.blocks[len(t.blocks)-1]
	default:
		b = &block{}
		t.blocks = append(t.blocks, b)
		t.anonOpen = true
	}
	b.text.WriteString(text)
	if final {
		b.final = true
	}
}

// boundary marks a tool or permission event: the next anonymous delta starts a
// new block.
func (t *turn) boundary() { t.anonOpen = false }

func (t *turn) raiseQuestion(text string) {
	if t.question == nil {
		t.question = &signal{kind: KindQuestion, text: strings.TrimSpace(text), at: len(t.blocks)}
	}
}

func (t *turn) raiseApproval(text string) {
	if t.approval == nil {
		t.approval = &signal{kind: KindApproval, text: strings.TrimSpace(text), at: len(t.blocks)}
	}
}

func (t *turn) request(ids []string, text string) {
	t.pending = append(t.pending, pendingRequest{
		ids: ids,
		sig: signal{kind: KindApproval, text: strings.TrimSpace(text), at: len(t.blocks)},
	})
}

// resolve settles the pending request that any of ids names, or the oldest one
// when none does. A refusal raises an approval: the agent went without a
// decision it needed.
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
	var sig signal
	if idx >= 0 {
		sig = t.pending[idx].sig
		t.pending = append(t.pending[:idx], t.pending[idx+1:]...)
	} else {
		sig = signal{kind: KindApproval, text: strings.TrimSpace(text), at: len(t.blocks)}
	}
	if !allowed {
		t.raiseApproval(sig.text)
	}
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		if x == "" {
			continue
		}
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// signal returns the signal that decides the turn's kind, if any: a question
// over an approval, and any request still unresolved at the end counts as an
// approval.
func (t *turn) signal() *signal {
	if t.question != nil {
		return t.question
	}
	if t.approval != nil {
		return t.approval
	}
	if len(t.pending) > 0 {
		sig := t.pending[0].sig
		return &sig
	}
	return nil
}

// signalText is the text of an output whose kind a signal decided: what the
// agent wrote after the signal, else the signal's own description, else the
// turn's ordinary pick.
func (t *turn) signalText(sig *signal, pickText string, pickConf Confidence) (string, Confidence) {
	if s, c := t.pick(sig.at, ""); s != "" {
		return s, c
	}
	if sig.text != "" {
		return sig.text, ConfidenceExact
	}
	return pickText, pickConf
}

// pick chooses the turn's text from the blocks at index from onward: the
// terminal event's own text, else the last block a delta marked final, else the
// last block with any text.
func (t *turn) pick(from int, termText string) (string, Confidence) {
	if s := strings.TrimSpace(termText); s != "" {
		return s, ConfidenceExact
	}
	blocks := t.blocks
	if from > len(blocks) {
		from = len(blocks)
	}
	blocks = blocks[from:]
	for i := len(blocks) - 1; i >= 0; i-- {
		if !blocks[i].final {
			continue
		}
		if s := strings.TrimSpace(blocks[i].text.String()); s != "" {
			return s, ConfidenceExact
		}
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(blocks[i].text.String()); s != "" {
			return s, ConfidenceHeuristic
		}
	}
	return "", ConfidenceHeuristic
}
