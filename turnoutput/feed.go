package turnoutput

import (
	"encoding/json"
	"strings"

	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider/events"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Observe feeds one go-runtime-events envelope. It reports an [Output] when the
// event is a turn's terminal event (turn.completed or turn.failed), or the
// process exiting with a turn still open.
//
// The reducer reads the payload conventions documented in the runtimeevents
// package: agent.delta's content, block_id and phase; turn.completed's text and
// stop_reason; turn.failed's error, reason and stop_reason.
func (r *Reducer) Observe(ev runtimeevents.Event) (Output, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessionID == "" {
		r.sessionID = ev.SessionID
	}
	if r.runtime == "" {
		r.runtime = ev.Process.Provider
	}

	switch ev.Kind {
	case runtimeevents.KindTurnStarted:
		if ev.TurnID != "" {
			r.open(ev.TurnID)
		}

	case runtimeevents.KindAgentDelta:
		t := r.eventTurn(ev.TurnID)
		var p struct {
			Content  string          `json:"content"`
			Text     string          `json:"text"`
			Phase    string          `json:"phase"`
			BlockID  string          `json:"block_id"`
			Thinking json.RawMessage `json:"thinking"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil || isThought(p.Phase) || truthy(p.Thinking) {
			return Output{}, false
		}
		text := p.Content
		if text == "" {
			text = p.Text
		}
		t.addDelta(text, p.BlockID, isFinalPhase(p.Phase))

	case runtimeevents.KindAgentToolUse:
		t := r.eventTurn(ev.TurnID)
		t.boundary()
		var p struct {
			ToolUse *struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"tool_use"`
			Name     string          `json:"name"`
			Title    string          `json:"title"`
			RawInput json.RawMessage `json:"raw_input"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil {
			return Output{}, false
		}
		name, input := p.Name, p.RawInput
		if p.ToolUse != nil {
			name, input = p.ToolUse.Name, p.ToolUse.Input
		}
		if name == "" {
			name = p.Title
		}
		r.toolUse(t, name, input)

	case runtimeevents.KindAgentToolResult, runtimeevents.KindAgentSubagentSpawn:
		r.eventTurn(ev.TurnID).boundary()

	case runtimeevents.KindAgentPermissionRequested:
		t := r.eventTurn(ev.TurnID)
		t.boundary()
		var p struct {
			RequestID json.RawMessage `json:"request_id"`
			Method    string          `json:"method"`
			Params    json.RawMessage `json:"params"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil {
			return Output{}, false
		}
		if isQuestionMethod(p.Method) {
			t.raiseQuestion(questionText(p.Params))
			return Output{}, false
		}
		t.request([]string{ev.ID, idKey(p.RequestID)}, "Approval requested: "+describeRequest(p.Method, p.Params))

	case runtimeevents.KindAgentPermissionResolved:
		t := r.eventTurn(ev.TurnID)
		var p struct {
			RequestID json.RawMessage `json:"request_id"`
			Method    string          `json:"method"`
			Allowed   *bool           `json:"allowed"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil {
			return Output{}, false
		}
		// A resolution that does not say it allowed the action did not allow it.
		allowed := p.Allowed != nil && *p.Allowed
		if isQuestionMethod(p.Method) {
			return Output{}, false
		}
		t.resolve([]string{ev.ParentID, idKey(p.RequestID)}, allowed, "Approval refused: "+describeRequest(p.Method, nil))

	case runtimeevents.KindAgentPermissionDenied:
		r.eventTurn(ev.TurnID).raiseApproval(permissionDenied(ev.Payload))

	case runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
		t, ok := r.lookup(ev.TurnID)
		if !ok {
			return Output{}, false
		}
		var p struct {
			Text       string `json:"text"`
			Error      string `json:"error"`
			Message    string `json:"message"`
			Reason     string `json:"reason"`
			StopReason string `json:"stop_reason"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		term := terminal{
			failed:     ev.Kind == runtimeevents.KindTurnFailed,
			errText:    firstNonEmpty(p.Error, p.Message),
			reason:     p.Reason,
			stopReason: p.StopReason,
			text:       p.Text,
		}
		return r.finish(t, term), true

	case runtimeevents.KindProcessExited:
		if r.cur == nil {
			return Output{}, false
		}
		var p struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		return r.finish(r.cur, terminal{failed: true, reason: reasonProcessExited, errText: p.Error}), true

	default:
		// Lifecycle, raw IO, heartbeat and policy kinds are not part of a
		// turn's output, and new kinds may appear.
	}
	return Output{}, false
}

// eventTurn returns the turn an envelope belongs to, starting it when the
// envelope names a turn that is not in progress.
func (r *Reducer) eventTurn(id string) *turn {
	if id == "" {
		return r.implicit()
	}
	return r.open(id)
}

// ObserveProvider feeds one go-providers typed event, the kind agentkit's
// StartOptions.TypedEventCallback delivers. The events carry no turn id, so the
// reducer starts a turn at the first one after a terminal event and mints its id
// from [Config.NewTurnID]. It reports an [Output] on Done and Error.
//
// Values are expected, as the typed callback delivers them; a pointer to an
// event is ignored.
func (r *Reducer) ObserveProvider(ev events.Event) (Output, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch e := ev.(type) {
	case events.Delta:
		t := r.implicit()
		if !isThought(e.Phase) {
			t.addDelta(e.Text, e.BlockID, isFinalPhase(e.Phase))
		}
	case events.Thinking:
		r.implicit()
	case events.ToolUse:
		t := r.implicit()
		t.boundary()
		var input json.RawMessage
		if e.Args != nil {
			input, _ = json.Marshal(e.Args)
		}
		r.toolUse(t, e.Name, input)
	case events.ToolResult, events.SubagentSpawn:
		r.implicit().boundary()
	case events.Usage:
		if e.StopReason != "" {
			r.implicit().usageStop = e.StopReason
		}
	case events.PermissionDenied:
		r.implicit().raiseApproval(permissionDeniedText(e.Action, e.DisplayName))
	case events.Done:
		return r.finish(r.implicit(), terminal{stopReason: e.StopReason}), true
	case events.Error:
		msg := e.Message
		if msg == "" && e.Err != nil {
			msg = e.Err.Error()
		}
		return r.finish(r.implicit(), terminal{failed: true, errText: msg}), true
	}
	return Output{}, false
}

// ObserveStream feeds one legacy llmtypes.StreamEvent, the kind agentkit's
// StartOptions.EventFanout delivers. Like [Reducer.ObserveProvider] it starts a
// turn at the first event after a terminal one. EventDone's Content, when a
// provider sets it, is the turn's own final text. It reports an [Output] on
// EventDone and EventError.
//
// agentkit drops events when the EventFanout channel is full, so a host that
// can use [Reducer.ObserveProvider] should.
func (r *Reducer) ObserveStream(ev llmtypes.StreamEvent) (Output, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch ev.Type {
	case llmtypes.EventDelta:
		t := r.implicit()
		if !isThought(ev.Phase) {
			t.addDelta(ev.Content, ev.BlockID, isFinalPhase(ev.Phase))
		}
	case llmtypes.EventThinking:
		r.implicit()
	case llmtypes.EventToolUse:
		t := r.implicit()
		t.boundary()
		if ev.ToolUse != nil {
			var input json.RawMessage
			if ev.ToolUse.Input != nil {
				input, _ = json.Marshal(ev.ToolUse.Input)
			}
			r.toolUse(t, ev.ToolUse.Name, input)
		}
	case llmtypes.EventUsage:
		if ev.Usage != nil && ev.Usage.StopReason != "" {
			r.implicit().usageStop = llmtypes.NormalizeStopReason(ev.Usage.StopReason)
		}
	case llmtypes.EventDone:
		return r.finish(r.implicit(), terminal{text: ev.Content}), true
	case llmtypes.EventError:
		return r.finish(r.implicit(), terminal{failed: true, errText: ev.Error}), true
	default:
		// A session id is not part of a turn's output.
	}
	return Output{}, false
}

// toolUse notes a tool call: a question tool raises a question.
func (r *Reducer) toolUse(t *turn, name string, input json.RawMessage) {
	if _, ok := r.questions[strings.ToLower(strings.TrimSpace(name))]; ok {
		t.raiseQuestion(questionText(input))
	}
}

func isThought(phase string) bool {
	switch strings.ToLower(phase) {
	case "thought", "thinking":
		return true
	}
	return false
}

func isFinalPhase(phase string) bool {
	switch strings.ToLower(phase) {
	case "final", "final_answer":
		return true
	}
	return false
}

// truthy reports whether a legacy "thinking" marker is set: true, or an object
// carrying the thinking block.
func truthy(raw json.RawMessage) bool {
	switch s := strings.TrimSpace(string(raw)); s {
	case "", "null", "false":
		return false
	}
	return true
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// idKey turns a JSON request id (a number or a string) into a comparable key.
func idKey(raw json.RawMessage) string {
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}

// isQuestionMethod reports whether a server request asks the user a question
// rather than for permission: Codex's item/tool/requestUserInput.
func isQuestionMethod(method string) bool {
	return strings.HasSuffix(strings.ToLower(method), "requestuserinput")
}

// questionText reads the question out of a question tool's input or a
// requestUserInput request's params: {"questions": [{"question": "…"}]} or
// {"question": "…"}.
func questionText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var p struct {
		Questions []struct {
			Question string `json:"question"`
			Text     string `json:"text"`
			Header   string `json:"header"`
		} `json:"questions"`
		Question string `json:"question"`
		Prompt   string `json:"prompt"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	var lines []string
	for _, q := range p.Questions {
		if s := firstNonEmpty(q.Question, q.Text, q.Header); s != "" {
			lines = append(lines, s)
		}
	}
	if len(lines) > 0 {
		return strings.Join(lines, "\n")
	}
	return firstNonEmpty(p.Question, p.Prompt)
}

// describeRequest names what a permission request asks to do, from the
// request's params: an ACP tool call's title, a Codex command, else the method.
func describeRequest(method string, params json.RawMessage) string {
	var p struct {
		ToolCall struct {
			Title string `json:"title"`
		} `json:"toolCall"`
		Command json.RawMessage `json:"command"`
		Reason  string          `json:"reason"`
	}
	if len(params) > 0 && json.Unmarshal(params, &p) == nil {
		if p.ToolCall.Title != "" {
			return p.ToolCall.Title
		}
		var argv []string
		var cmd string
		switch {
		case json.Unmarshal(p.Command, &cmd) == nil && cmd != "":
			return cmd
		case json.Unmarshal(p.Command, &argv) == nil && len(argv) > 0:
			return strings.Join(argv, " ")
		}
		if p.Reason != "" {
			return p.Reason
		}
	}
	if method != "" {
		return method
	}
	return "an action"
}

func permissionDenied(payload json.RawMessage) string {
	var p struct {
		Action      string `json:"action"`
		DisplayName string `json:"display_name"`
	}
	_ = json.Unmarshal(payload, &p)
	return permissionDeniedText(p.Action, p.DisplayName)
}

func permissionDeniedText(action, displayName string) string {
	what := firstNonEmpty(displayName, action)
	if what == "" {
		what = "an action"
	}
	return "Permission denied: " + what
}
