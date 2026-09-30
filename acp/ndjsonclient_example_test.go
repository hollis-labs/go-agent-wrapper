package acp_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// ExampleNewNDJSONBridgeClient builds an ACP client for a hypothetical agent
// that speaks newline-delimited JSON-RPC on stdio. Only command resolution and
// notification translation are agent-specific; the handshake, turn lifecycle,
// permission handling and shutdown come from the shared client. It is
// compile-checked only: running it would spawn the agent.
func ExampleNewNDJSONBridgeClient() {
	client := acp.NewNDJSONBridgeClient(acp.NDJSONBridgeConfig{
		Component: "myagentacp",
		ResolveCommand: func(acp.LaunchParams) (string, []string, error) {
			return "myagent", []string{"acp"}, nil
		},
		HandleNotification: func(c *acp.NDJSONBridgeClient, method string, params json.RawMessage) {
			if method != "session/update" {
				return
			}
			c.Emit(runtimeevents.Event{
				Kind:    runtimeevents.KindAgentDelta,
				TurnID:  c.CurrentTurnID(),
				Payload: params,
			})
		},
	})

	ctx := context.Background()
	if err := client.Launch(ctx, acp.LaunchParams{Cwd: "."}); err != nil {
		fmt.Println("launch:", err)
		return
	}
	defer func() { _ = client.Close(ctx) }()
	if err := client.Prompt(ctx, "hello"); err != nil {
		fmt.Println("prompt:", err)
	}
	for ev := range client.Events() {
		if ev.Kind == runtimeevents.KindTurnCompleted {
			break
		}
	}
}
