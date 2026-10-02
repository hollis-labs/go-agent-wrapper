package turnoutput

// Kind says what a turn's output is for.
type Kind string

const (
	// KindFinal is the agent's answer: the turn ended normally.
	KindFinal Kind = "final"
	// KindQuestion is the agent asking the user something and waiting.
	KindQuestion Kind = "question"
	// KindApproval is the agent needing a decision on an action it could not
	// take without one.
	KindApproval Kind = "approval"
	// KindFailure is a turn the runtime failed; Text is its error.
	KindFailure Kind = "failure"
	// KindTerminal is a turn that was cut short: interrupted, canceled, or the
	// process went away. Text is any partial output, else the reason.
	KindTerminal Kind = "terminal"
)

// Confidence says how Text was obtained.
type Confidence string

const (
	// ConfidenceExact means the runtime itself marked the text: a result it
	// carried on the terminal event, a delta it labeled "final", or its own
	// error message.
	ConfidenceExact Confidence = "exact"
	// ConfidenceHeuristic means the reducer picked the last text block of the
	// turn because the runtime marked nothing.
	ConfidenceHeuristic Confidence = "heuristic"
)

// Output is the normalized record of one completed turn. The JSON field names
// are the ADR 0049 turn-output contract.
type Output struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	// Text is the turn's message for the user; see the package doc for how it
	// is chosen and when it is empty.
	Text string `json:"text"`
	Kind Kind   `json:"kind"`
	// StopReason is normalised (end_turn, max_tokens, tool_use, turn_limit,
	// refusal, canceled, error) or the provider's own word. It is empty when
	// the runtime reported none.
	StopReason string `json:"stop_reason"`
	// Runtime is the go-providers registry runtime id (claude, codex, opencode,
	// agy, an ACP agent id): the provider the session runs, not its transport.
	Runtime    string     `json:"runtime"`
	Confidence Confidence `json:"confidence"`
}

// Config configures a [Reducer].
type Config struct {
	// SessionID is the session the reducer reduces. Required for
	// [Reducer.ObserveProvider] and [Reducer.ObserveStream], whose events carry
	// no envelope; [Reducer.Observe] falls back to the first event's SessionID.
	SessionID string
	// Runtime is the registry runtime id reported on every Output.
	// [Reducer.Observe] falls back to the first event's Process.Provider.
	Runtime string
	// NewTurnID mints a turn id for the provider and stream feeds, whose events
	// carry none. Default: runtimeevents.NewTurnID.
	NewTurnID func() string
	// QuestionTools names the tools that mean the agent asked the user.
	// Default: [DefaultQuestionTools]. Matching ignores case.
	QuestionTools []string
}

// DefaultQuestionTools are the tools that ask the user a question: Claude's
// AskUserQuestion and Codex's request_user_input.
var DefaultQuestionTools = []string{"AskUserQuestion", "request_user_input"}
