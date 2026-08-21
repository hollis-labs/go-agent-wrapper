package copilotacp

import (
	"encoding/json"
	"fmt"
)

// wireFrame is the generic JSON-RPC 2.0 envelope ACP messages ride over
// newline-delimited JSON (NDJSON) — confirmed directly against the real
// `copilot --acp` binary (see package doc). ID is a pointer so "absent"
// (a notification, e.g. `session/update`/`session/cancel`) is
// distinguishable from "id: 0" (a valid numeric id — Copilot's real
// responses use ids starting from 0).
type wireFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

// wireError is the JSON-RPC 2.0 error envelope.
type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *wireError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("copilotacp: jsonrpc error %d: %s", e.Code, e.Message)
}

// initializeParams is the `initialize` request's params shape, per
// agentclientprotocol.com/protocol/initialization and confirmed against
// the real binary's response (which honored a request built exactly
// this way).
type initializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
	ClientInfo         clientInfo         `json:"clientInfo"`
}

type clientCapabilities struct {
	FS       fsCapabilities `json:"fs"`
	Terminal bool           `json:"terminal"`
}

// fsCapabilities declares whether this client will service
// `fs/read_text_file`/`fs/write_text_file` on the agent's behalf. Both
// left false — see the package doc's "no fs/terminal proxying"
// limitation.
type fsCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// sessionNewParams is `session/new`'s params shape — confirmed against
// the real binary (`{"cwd":"...","mcpServers":[]}` was accepted and
// returned a real sessionId).
type sessionNewParams struct {
	Cwd        string `json:"cwd"`
	MCPServers []any  `json:"mcpServers"`
}

type sessionNewResult struct {
	SessionID string `json:"sessionId"`
}

// promptContentBlock is one entry of `session/prompt`'s `prompt` array.
// Only the "text" block kind is used by this adapter.
type promptContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type sessionPromptParams struct {
	SessionID string               `json:"sessionId"`
	Prompt    []promptContentBlock `json:"prompt"`
}

// sessionPromptResult is `session/prompt`'s result shape. StopReason
// values observed/spec'd: end_turn, max_tokens, max_turn_requests,
// refusal, cancelled. Confirmed live: a real cancelled-mid-turn response
// from Copilot 1.0.12 actually carried "end_turn", not "cancelled" — a
// minor spec-compliance quirk in Copilot's own implementation, not a
// bug in this client; StopReason is surfaced verbatim in the emitted
// turn.completed payload rather than re-interpreted.
type sessionPromptResult struct {
	StopReason string `json:"stopReason"`
}

// sessionCancelParams is `session/cancel`'s params shape. Sent as a
// notification (no id) — confirmed against the spec: despite the name,
// session/cancel cancels the in-flight turn, not the session.
type sessionCancelParams struct {
	SessionID string `json:"sessionId"`
}

// sessionUpdateParams is the `session/update` notification's params
// shape. Update is left as raw JSON because its shape varies by the
// `sessionUpdate` discriminator field — see parseSessionUpdate.
type sessionUpdateParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// sessionUpdateDiscriminator peels off just the `sessionUpdate` field
// so parseSessionUpdate can dispatch to the right concrete shape.
type sessionUpdateDiscriminator struct {
	SessionUpdate string `json:"sessionUpdate"`
}

// contentBlock is the `content` shape observed on both
// agent_message_chunk and agent_thought_chunk updates in real captured
// traffic: `{"type":"text","text":"..."}`.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type agentMessageChunkUpdate struct {
	MessageID string       `json:"messageId,omitempty"`
	Content   contentBlock `json:"content"`
}

type agentThoughtChunkUpdate struct {
	ThoughtID string       `json:"thoughtId,omitempty"`
	Content   contentBlock `json:"content"`
}

type toolCallUpdate struct {
	ToolCallID string `json:"toolCallId"`
	Title      string `json:"title,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Status     string `json:"status,omitempty"`
}

type toolCallStatusUpdate struct {
	ToolCallID string         `json:"toolCallId"`
	Status     string         `json:"status,omitempty"`
	Content    []contentBlock `json:"content,omitempty"`
}
