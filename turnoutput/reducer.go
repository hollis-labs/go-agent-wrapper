package turnoutput

import (
	"strings"
	"sync"
	"unicode/utf8"

	llmtypes "github.com/hollis-labs/go-llm-types"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// finishedMemory is how many finished turn ids a Reducer remembers, so that an
// event that arrives late for a turn already reported is dropped rather than
// reopening it.
const finishedMemory = 64

// doneSignature is how a completed turn ended: its stop reason and its text.
type doneSignature struct {
	stop, text string
}

// maxOpenTurns bounds the turns a Reducer buffers at once. A session runs one
// turn at a time and every turn ends with a terminal event, so reaching it means
// a producer lost terminal events; the oldest turn is dropped without an Output.
const maxOpenTurns = 64

// Per-turn memory bounds. A turn whose terminal event is lost (a dropped Done on
// a lossy feed) keeps buffering, so the buffer is capped: past maxBlocks blocks
// or maxTurnBytes of text the oldest blocks are dropped, since the turn's text
// is its last block; one block is cut at maxBlockBytes. A turn with more than
// maxPending permission requests unresolved forgets the oldest, and the text of
// a question or approval signal is cut at maxSignalBytes.
const (
	maxBlocks      = 128
	maxTurnBytes   = 4 << 20
	maxBlockBytes  = 1 << 20
	maxPending     = 64
	maxSignalBytes = 8 << 10
)

// Reducer folds one session's events into an [Output] per completed turn. It is
// safe for concurrent use. Build one per session with [New].
type Reducer struct {
	mu        sync.Mutex
	sessionID string
	runtime   string
	newTurnID func() string
	questions map[string]struct{}

	// open holds the turns in progress by id; order lists their ids, oldest
	// first, and its last is the current turn, which events with no turn id
	// belong to.
	open  map[string]*turn
	order []string
	// finished is the ids of the turns already reported, oldest first.
	finished []string
	// settled reports that the last turn ended and nothing has opened another.
	// A terminal event with no turn id that arrives while settled is either a
	// turn that had no events of its own (an interrupted one, say) or a repeat of
	// the terminal event that ended the last turn; lastDone tells them apart.
	settled bool
	// lastDone is what the last completed turn ended with.
	lastDone doneSignature
	// heldStop is a stop reason that arrived while settled: the leading stop
	// reason of a turn with no other events, or a trailing one of the turn that
	// just ended. The next event says which: a terminal event takes it, any other
	// drops it.
	heldStop string
	// lastError is the text of the last failure reported, so the same error
	// repeated while settled is reported once.
	lastError string
	// pendingStop is a stop reason that arrived before the turn it belongs to had
	// any other event.
	pendingStop string
}

// New returns a Reducer for one session.
func New(cfg Config) *Reducer {
	r := &Reducer{
		sessionID: cfg.SessionID,
		runtime:   cfg.Runtime,
		newTurnID: cfg.NewTurnID,
		questions: map[string]struct{}{},
		open:      map[string]*turn{},
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
// The text is what the agent had said, else reason (default "process_exited"). It
// reports false when no turn is in progress.
func (r *Reducer) Flush(reason string) (Output, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.current()
	if t == nil {
		return Output{}, false
	}
	if strings.TrimSpace(reason) == "" {
		reason = reasonProcessExited
	}
	return r.finish(t, terminal{failed: true, cut: true, reason: reason}), true
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
	// cut makes the turn terminal whatever its reason: the session ended it.
	cut bool
	// reason is the producer's own reason for a failed turn ("interrupted",
	// "process_exited"); empty for an ordinary failure.
	reason     string
	stopReason string
	// text is the final text the runtime carried on the terminal event.
	text string
}

// current returns the turn that events with no turn id belong to: the one most
// recently started that is still open.
func (r *Reducer) current() *turn {
	if len(r.order) == 0 {
		return nil
	}
	return r.open[r.order[len(r.order)-1]]
}

// begin starts the turn with id.
func (r *Reducer) begin(id string) *turn {
	t := newTurn(id)
	t.usageStop, r.pendingStop = r.pendingStop, ""
	r.heldStop = ""
	r.open[id] = t
	r.order = append(r.order, id)
	for len(r.order) > maxOpenTurns {
		delete(r.open, r.order[0])
		r.order = r.order[1:]
	}
	r.settled = false
	return t
}

// body returns the turn an event inside a turn's body belongs to: the open turn
// with the event's id, started when the id is new; for an event with no id, the
// current turn, started with a minted id when none is open. It returns nil for
// an event that arrived late for a turn already reported, which is dropped.
func (r *Reducer) body(id string) *turn {
	if id == "" {
		if t := r.current(); t != nil {
			return t
		}
		return r.begin(r.newTurnID())
	}
	if t := r.open[id]; t != nil {
		return t
	}
	if r.wasFinished(id) {
		return nil
	}
	return r.begin(id)
}

// ending returns the turn a terminal event closes, and false when the event
// repeats one already reported. An id the reducer saw nothing of closes an empty
// turn: the runtime reported a turn even if none of it reached the reducer.
//
// Without an id the event closes the current turn. With none open, the turn had
// no events of its own, or the event repeats the one that ended the last turn.
// It is a repeat only if the last turn was completed and this one ends the same
// way: the same stop reason and text, and not a cancellation, since an
// interrupted turn that said nothing is a turn of its own and a lone terminal
// event must never vanish. A failure is a repeat only if it is the same error
// again, so a failure with no turn around it (a startup failure) is reported.
func (r *Reducer) ending(id string, term terminal) (*turn, bool) {
	if id != "" {
		if t := r.open[id]; t != nil {
			return t, true
		}
		if r.wasFinished(id) {
			return nil, false
		}
		return newTurn(id), true
	}
	if t := r.current(); t != nil {
		return t, true
	}
	stop := term.stopReason
	if stop == "" {
		stop = r.heldStop
	}
	if r.settled {
		errText := strings.TrimSpace(term.errText)
		switch {
		case term.failed:
			if errText != "" && errText == r.lastError {
				return nil, false
			}
		case stop != stopCancelled && r.lastDone == doneSignature{stop: stop, text: strings.TrimSpace(term.text)}:
			return nil, false
		}
	}
	t := newTurn(r.newTurnID())
	t.usageStop = r.pendingStop
	if r.heldStop != "" {
		t.usageStop = r.heldStop
	}
	r.pendingStop, r.heldStop = "", ""
	return t, true
}

// noteStop records a stop reason reported on its own: on the current turn, or,
// when a turn's first event is not in yet, for the next one. One that arrives
// after a turn ended with nothing since is held until the next event says
// whether it led a terminal event or trailed the turn that ended.
func (r *Reducer) noteStop(stop string) {
	if stop == "" {
		return
	}
	if t := r.current(); t != nil {
		t.usageStop = stop
		return
	}
	if r.settled {
		r.heldStop = stop
		return
	}
	r.pendingStop = stop
}

func (r *Reducer) wasFinished(id string) bool {
	for _, done := range r.finished {
		if done == id {
			return true
		}
	}
	return false
}

func (r *Reducer) finish(t *turn, term terminal) Output {
	delete(r.open, t.id)
	for i, id := range r.order {
		if id == t.id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	r.finished = append(r.finished, t.id)
	if len(r.finished) > finishedMemory {
		r.finished = r.finished[len(r.finished)-finishedMemory:]
	}
	r.settled = len(r.order) == 0
	r.pendingStop, r.heldStop = "", ""
	if term.failed {
		r.lastError = strings.TrimSpace(term.errText)
		r.lastDone = doneSignature{}
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

	if !term.failed {
		r.lastDone = doneSignature{stop: out.StopReason, text: strings.TrimSpace(term.text)}
	}

	cut := term.cut ||
		(term.failed && (term.reason == reasonInterrupted || term.reason == reasonProcessExited)) ||
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
		if sig := t.openSignal(); sig != nil {
			out.Kind = sig.kind
			out.Text, out.Confidence = t.signalText(sig, term.text, out.Text, out.Confidence)
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
	// bytes is the text held across all blocks.
	bytes int

	// tools are the turn's tool calls in order, and signals what in the turn
	// needs the user; see signals.go.
	seq   int
	tools []tool
	// early holds what was learned about a tool call before the call arrived, in
	// the order it was learned (earlyOrder), for the call to claim.
	early      map[string]*facts
	earlyOrder []string
	signals    []*signal
	pending    []pendingRequest
}

func newTurn(id string) *turn {
	return &turn{id: id, byID: map[string]*block{}, early: map[string]*facts{}}
}

// block is one text block of a turn.
type block struct {
	id    string
	final bool
	text  strings.Builder
}

func (t *turn) addDelta(text, blockID string, final bool) {
	if text == "" {
		return
	}
	defer t.trim()
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
	text = cutUTF8(text, maxBlockBytes-b.text.Len())
	b.text.WriteString(text)
	t.bytes += len(text)
	if final {
		b.final = true
	}
}

// trim keeps the turn's buffer within its bounds by dropping the oldest blocks,
// never the last.
func (t *turn) trim() {
	for len(t.blocks) > 1 && (len(t.blocks) > maxBlocks || t.bytes > maxTurnBytes) {
		oldest := t.blocks[0]
		t.bytes -= oldest.text.Len()
		if oldest.id != "" {
			delete(t.byID, oldest.id)
		}
		t.blocks = t.blocks[1:]
		t.blocksShifted()
	}
}

// boundary marks a tool or permission event: the next anonymous delta starts a
// new block.
func (t *turn) boundary() { t.anonOpen = false }

// clip trims a signal's text and cuts it at maxSignalBytes.
func clip(text string) string {
	return cutUTF8(strings.TrimSpace(text), maxSignalBytes)
}

// cutUTF8 returns s cut to at most n bytes, never in the middle of a rune.
func cutUTF8(s string, n int) string {
	if n >= len(s) {
		return s
	}
	n = max(n, 0)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
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
