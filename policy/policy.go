package policy

import (
	"context"
	"errors"

	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// Recommendation names the advisory interpretation of an observed command or
// tool call. A Recommendation never changes, delays, or prevents execution.
// Hosts may use a Finding as one input to their own enforcement, but that
// enforcement happens outside this package and must run before the side effect.
//
// The string values intentionally preserve the v0.8.1 policy-event payload
// vocabulary. See the package documentation for the legacy wire mapping.
type Recommendation string

const (
	RecommendationNone            Recommendation = "observe"
	RecommendationNudge           Recommendation = "nudge"
	RecommendationRewrite         Recommendation = "rewrite"
	RecommendationBlock           Recommendation = "block"
	RecommendationRequestApproval Recommendation = "approval"
)

// Observation describes a command or tool call the wrapper has already
// observed in the child runtime's output. It is not a pre-execution request.
type Observation struct {
	// App, SessionID, TurnID identify the wrapper context.
	App       string
	SessionID string
	TurnID    string

	// Kind names the observation type ("command", "tool_use",
	// "subagent_spawn", ...). Open string so adapters can introduce new
	// observation points.
	Kind string

	// Original is the raw command or serialized tool call the agent produced.
	// It is preserved verbatim so audit and disclosure can show what the
	// agent emitted.
	Original string

	// Channel describes how the wrapper observed this operation. Consumers can
	// use it when interpreting the Finding; an inferred text observation is
	// weaker evidence than a semantic protocol event.
	Channel runtimeevents.SourceChannel

	// Confidence is the wrapper's certainty that Original means what
	// it looks like. Mirrors [runtimeevents.Source.Confidence].
	Confidence runtimeevents.Confidence
}

// Finding is what an [Observer] returns for one [Observation]. The wrapper
// publishes mapped recommendations as correlated runtime events. It does not
// apply them to the child process.
type Finding struct {
	// Recommendation is advisory only. RecommendationNone emits no derived
	// runtime event.
	Recommendation Recommendation

	// RuleID is the stable identifier of the matched rule
	// ("hollis.deploy.nanite.cerberus-required"). Required for any
	// Recommendation other than RecommendationNone so the agent and operators
	// can find the rule.
	RuleID string

	// SuggestedReplacement is the proposed command or tool call when
	// Recommendation == RecommendationRewrite. The wrapper reports it but
	// never substitutes it into the child runtime.
	SuggestedReplacement string

	// Message is the human-readable guidance shown to the agent (and
	// to the user, where the app surfaces policy events). Required for
	// RecommendationNudge, RecommendationRewrite, RecommendationBlock, and
	// RecommendationRequestApproval.
	Message string
}

// Observer classifies one already-observed command or tool call. The wrapper
// holds one Observer for the lifetime of a session and calls it after emitting
// the originating activity event.
//
// Implementations should be cheap and synchronous in the common path;
// expensive work (LLM classification, network lookups) belongs behind a
// cache or async fetcher inside the Observer.
type Observer interface {
	Observe(ctx context.Context, observation Observation) (Finding, error)
}

// Store is the app-provided rule backing. Apps plug in any backing they
// like (YAML files, SQLite, dynamic config, central admin service) by
// implementing this interface; the wrapper does not own storage.
//
// Observers built on top of Store typically cache lookups for the
// duration of one session.
type Store interface {
	// Lookup returns rules applicable to observation in priority order
	// (highest priority first). Returning an empty slice means no
	// rule matched and the wrapper should return RecommendationNone.
	Lookup(ctx context.Context, observation Observation) ([]Rule, error)
}

// Rule is the persisted form of a policy-observation template. A Store
// returns Rules; an Observer evaluates them into a [Finding] for a specific
// [Observation].
type Rule struct {
	ID                   string
	Recommendation       Recommendation
	Match                string // store-defined matcher syntax (glob, regex, structured)
	SuggestedReplacement string // template; may reference Observation fields
	Message              string
	Priority             int
	// Channels, when non-empty, restricts this rule to specific
	// observation channels. Empty means "any channel".
	Channels []runtimeevents.SourceChannel
}

// NoOpObserver is an Observer that reports no recommendation.
type NoOpObserver struct{}

// Observe implements [Observer].
func (NoOpObserver) Observe(_ context.Context, _ Observation) (Finding, error) {
	return Finding{Recommendation: RecommendationNone}, nil
}

// ErrNoRule is the sentinel an Observer may return when no rule matched and
// the caller should treat that as RecommendationNone rather than a real error.
// Callers should prefer [errors.Is] over equality.
var ErrNoRule = errors.New("policy: no rule matched")
