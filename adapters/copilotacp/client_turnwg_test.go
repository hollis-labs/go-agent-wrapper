package copilotacp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
)

// CW-20261001-0262: Prompt registers its turn with turnWG.Add under c.mu, but
// only Close sealed admission before closeEvents waited on turnWG. When the
// agent's transport ended by itself, a Prompt admitted after closeEvents began
// waiting added to the WaitGroup concurrently with the Wait: a data race under
// -race and, in a host, "sync: WaitGroup is reused before previous Wait has
// returned", a panic that takes down the process.

// exitOnPromptCopilotScript answers initialize (id 1) and session/new (id 2),
// then exits the moment it reads a prompt, without replying.
func exitOnPromptCopilotScript(t *testing.T) string {
	t.Helper()
	skipUnlessSh(t)
	script := filepath.Join(t.TempDir(), "exit-on-prompt-copilot.sh")
	body := `#!/bin/sh
IFS= read -r _
printf '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{},"authMethods":[]}}\n'
IFS= read -r _
printf '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"exits-session"}}\n'
IFS= read -r _
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: a fixture script that must be executable
		t.Fatalf("write exit-on-prompt copilot script: %v", err)
	}
	return script
}

// promptThroughAgentExit admits one Prompt that the agent's exit leaves in
// flight, spins more Prompts through the exit, waits for Events to close, and
// then checks a late Prompt is refused with no turn left in flight.
func promptThroughAgentExit(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Launch(ctx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_ = c.Close(closeCtx)
	})
	if err := c.Prompt(ctx, "first"); err != nil {
		t.Fatalf("first Prompt: %v", err)
	}

	stop := make(chan struct{})
	var spinners sync.WaitGroup
	for range 4 {
		spinners.Add(1)
		go func() {
			defer spinners.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.Prompt(context.Background(), "again")
			}
		}()
	}
	timeout := time.After(10 * time.Second)
drain:
	for {
		select {
		case _, ok := <-c.Events():
			if !ok {
				break drain
			}
		case <-timeout:
			close(stop)
			spinners.Wait()
			t.Fatal("Events never closed after the agent exited")
		}
	}
	close(stop)
	spinners.Wait()

	if err := c.Prompt(ctx, "late"); err == nil {
		t.Fatal("a Prompt after the agent's transport ended returned nil")
	}
	c.mu.Lock()
	inFlight := c.turnInFlight
	c.mu.Unlock()
	if inFlight {
		t.Fatal("a turn is still in flight after the transport ended")
	}
}

// A Prompt that races the agent's own exit, then one that follows it. Run with
// -race, and -count to repeat. closeEvents is the same code over TCP.
func TestClientStdio_PromptDuringAndAfterAgentExitDoesNotRaceTurnWG(t *testing.T) {
	promptThroughAgentExit(t, NewClient(adapters.TransportStdio, WithBinary(exitOnPromptCopilotScript(t))))
}

// Close seals admission before it waits, as it always did: a Prompt after
// Close is refused, with no turn left in flight.
func TestClient_PromptAfterCloseIsRefused(t *testing.T) {
	c := NewClient(adapters.TransportStdio, WithBinary(writeFakeCopilotScript(t, t.TempDir())))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Launch(ctx, acp.LaunchParams{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Prompt(ctx, "late"); err == nil {
		t.Fatal("a Prompt after Close returned nil")
	}
	c.mu.Lock()
	inFlight := c.turnInFlight
	c.mu.Unlock()
	if inFlight {
		t.Fatal("a turn is in flight after a refused Prompt")
	}
}
