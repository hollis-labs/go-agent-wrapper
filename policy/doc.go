// Package policy defines advisory observations over command and tool-call
// activity emitted by a child runtime.
//
// The wrapper does not ship policy rules. Apps plug in an [Observer], backed
// by a [Store] if useful, and the wrapper calls it after a semantic tool-use
// event is observed. The resulting [Finding] is activity-stream metadata. It
// cannot rewrite, block, pause, or approve the child operation that produced
// the event.
//
// # Recommendation and legacy wire mapping
//
// Recommendation values preserve the v0.8.1 payload strings and map onto the
// existing go-runtime-events kinds: RecommendationNudge to policy.nudge,
// RecommendationRewrite to policy.rewrite, RecommendationBlock to
// policy.block, and RecommendationRequestApproval to
// policy.approval_requested. Those names are compatibility labels on an
// observation stream; they do not describe an action performed by this
// package. RecommendationNone (wire value "observe") emits no derived event.
// Keeping this mapping avoids a coordinated breaking release of
// go-runtime-events and its consumers while making the Go callback API honest.
//
// # Enforcement boundary
//
// Enforcement belongs to a host-side gate that runs before execution. In
// Nanite, tool-grant checks, skill capability gates, and cancellable plugin
// pre-hooks are authoritative because they can prevent the relevant call or
// subprocess from starting. Their allow/deny/block vocabulary is intentionally
// not shared with this package's Recommendation vocabulary: identical words at
// different phases have different effects.
//
// ACP session/request_permission is a separate protocol request on which an
// agent can block while awaiting a response. It is not routed through Observer.
// Four current direct ACP clients (Claude, Codex, OpenCode, and Pi) default that
// request to a well-formed cancelled outcome; Copilot currently returns a
// JSON-RPC method-not-handled error. A future host-supplied responder may use
// that genuine control point, but only for providers and operation classes that
// choose to ask. It must not be presented as a general enforcement boundary.
package policy
