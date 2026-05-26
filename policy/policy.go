package policy

import (
	"context"
	"errors"

	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Mode names the policy action to take. Constants match the architecture
// doc; new modes require coordinated rollout across the wrapper, apps,
// and operator UI.
type Mode string

const (
	ModeObserve  Mode = "observe"
	ModeNudge    Mode = "nudge"
	ModeRewrite  Mode = "rewrite"
	ModeBlock    Mode = "block"
	ModeApproval Mode = "approval"
)

// Request is what the wrapper hands to an [Engine] at each interception
// point. The wrapper builds one Request per intercepted command or tool
// call.
type Request struct {
	// App, SessionID, TurnID identify the wrapper context.
	App       string
	SessionID string
	TurnID    string

	// Kind names the interception type ("command", "tool_use",
	// "subagent_spawn", ...). Open string so adapters can introduce
	// new intercept points.
	Kind string

	// Original is the raw command or serialized tool-call the agent
	// produced. Always preserved verbatim so audit and disclosure
	// can show what the agent actually intended.
	Original string

	// Channel describes how the wrapper observed this request. Engines
	// must consider Channel when deciding whether to enforce stricter
	// modes — inferred PTY-text observations cannot reliably support
	// rewrite/block on their own.
	Channel runtimeevents.SourceChannel

	// Confidence is the wrapper's certainty that Original means what
	// it looks like. Mirrors [runtimeevents.Source.Confidence].
	Confidence runtimeevents.Confidence
}

// Decision is what an [Engine] returns. The wrapper applies it,
// dual-publishes it as a runtime event, and logs it.
type Decision struct {
	// Mode is the action to take.
	Mode Mode

	// RuleID is the stable identifier of the matched rule
	// ("hollis.deploy.nanite.cerberus-required"). Required for any
	// non-observe Mode so the agent and operators can find the rule.
	RuleID string

	// Replacement is the command or tool-call to execute when
	// Mode == ModeRewrite. Ignored for other modes.
	Replacement string

	// Message is the human-readable guidance shown to the agent (and
	// to the user, where the app surfaces policy events). Required for
	// ModeNudge, ModeRewrite, ModeBlock, and ModeApproval.
	Message string
}

// Engine decides what to do with one observed command or tool call. The
// wrapper holds one Engine for the lifetime of a session and calls it
// at every configured interception point.
//
// Implementations should be cheap and synchronous in the common path;
// expensive work (LLM classification, network lookups) belongs behind a
// cache or async fetcher inside the Engine.
type Engine interface {
	Decide(ctx context.Context, req Request) (Decision, error)
}

// Store is the app-provided rule backing. Apps plug in any backing they
// like (YAML files, SQLite, dynamic config, central admin service) by
// implementing this interface; the wrapper does not own storage.
//
// Engines built on top of Store typically cache lookups for the
// duration of one session.
type Store interface {
	// Lookup returns rules applicable to req in priority order
	// (highest priority first). Returning an empty slice means no
	// rule matched and the wrapper should fall back to ModeObserve.
	Lookup(ctx context.Context, req Request) ([]Rule, error)
}

// Rule is the persisted form of a policy decision template. A Store
// returns Rules; an Engine evaluates them into a [Decision] for a
// specific [Request].
type Rule struct {
	ID          string
	Mode        Mode
	Match       string // store-defined matcher syntax (glob, regex, structured)
	Replacement string // template; may reference Request fields
	Message     string
	Priority    int
	// Channels, when non-empty, restricts this rule to specific
	// observation channels. Empty means "any channel".
	Channels []runtimeevents.SourceChannel
}

// ObserveOnly is the no-op Engine. Use as a placeholder when the
// wrapper is wired but no policy backing is configured yet.
type ObserveOnly struct{}

// Decide implements [Engine].
func (ObserveOnly) Decide(_ context.Context, _ Request) (Decision, error) {
	return Decision{Mode: ModeObserve}, nil
}

// ErrNoRule is the sentinel an Engine may return when no rule matched
// and the caller should treat that as ModeObserve rather than a real
// error. Callers should prefer [errors.Is] over equality.
var ErrNoRule = errors.New("policy: no rule matched")
