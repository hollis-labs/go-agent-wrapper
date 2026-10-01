package acp

import "testing"

func TestWithBlockID(t *testing.T) {
	p := WithBlockID(map[string]any{"content": "x"}, "msg_1")
	if p["block_id"] != "msg_1" {
		t.Errorf("payload = %v, want block_id msg_1", p)
	}
	p = WithBlockID(map[string]any{"content": "x"}, "")
	if _, has := p["block_id"]; has {
		t.Errorf("empty messageId added block_id: %v", p)
	}
}
