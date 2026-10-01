package acp

// WithBlockID adds block_id to an agent.delta payload when the ACP update
// named its message (agent_message_chunk / agent_thought_chunk messageId).
// Every chunk of one message carries the same id and the next message a
// different one, so a consumer separates messages without joining them.
func WithBlockID(payload map[string]any, messageID string) map[string]any {
	if messageID != "" {
		payload["block_id"] = messageID
	}
	return payload
}
