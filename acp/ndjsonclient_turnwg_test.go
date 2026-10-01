package acp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// CW-20261001-0262: Prompt registers its turn with turnWG.Add while the
// transport's own exit path, closeEvents, waits on turnWG. Only Close sealed
// admission first. When the agent exited by itself, a Prompt admitted after
// closeEvents began waiting added to the WaitGroup concurrently with the
// Wait: a data race under -race, and, in a host, "sync: WaitGroup is reused
// before previous Wait has returned", a panic that takes down the process.

// exitOnPromptAgent answers the handshake, then exits the moment it reads a
// prompt, without replying: its transport ends with a turn in flight.
func exitOnPromptAgent(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "exit-on-prompt-agent.sh")
	body := `#!/bin/sh
reply() {
  id=$(printf '%s' "$1" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  printf '%s\n' "$2" | sed "s/@ID@/$id/"
}
IFS= read -r line
reply "$line" '{"jsonrpc":"2.0","id":@ID@,"result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}}'
IFS= read -r line
reply "$line" '{"jsonrpc":"2.0","id":@ID@,"result":{"sessionId":"ses_exits"}}'
IFS= read -r _
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: a fixture script that must be executable
		t.Fatalf("write exit-on-prompt agent: %v", err)
	}
	return script
}

// A Prompt that races the agent's own exit, then one that follows it. The
// first Prompt is admitted and the agent exits with it in flight, so
// closeEvents blocks in turnWG.Wait until that turn fails. Other goroutines
// call Prompt through the exit: as the first turn clears, one of them is
// admitted, which in the unfixed client is an Add from zero against the Wait.
// Run with -race, and -count to repeat: the race detector reports it on an
// unordered Add and Wait whatever their wall-clock spacing.
//
// Once the agent's transport has ended, Prompt must refuse a new turn, and
// leave none in flight.
func TestNDJSONBridgeClient_PromptDuringAndAfterAgentExitDoesNotRaceTurnWG(t *testing.T) {
	skipUnlessSh(t)
	forEachComponent(t, func(t *testing.T, component string) {
		client := newTestClient(component, exitOnPromptAgent(t))
		launchCtx, launchCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer launchCancel()
		if err := client.Launch(launchCtx, LaunchParams{Cwd: t.TempDir()}); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		t.Cleanup(func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = client.Close(closeCtx)
		})
		if err := client.Prompt(launchCtx, "first"); err != nil {
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
					_ = client.Prompt(context.Background(), "again")
				}
			}()
		}

		// The agent exits on the first prompt; closeEvents closes Events
		// once the in-flight turn has failed.
		timeout := time.After(10 * time.Second)
	drain:
		for {
			select {
			case _, ok := <-client.Events():
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

		if err := client.Prompt(launchCtx, "late"); err == nil {
			t.Fatal("a Prompt after the agent's transport ended returned nil")
		}
		client.turnMu.Lock()
		inFlight := client.currentTurnID
		client.turnMu.Unlock()
		if inFlight != "" {
			t.Fatalf("turn %q still in flight after the transport ended", inFlight)
		}
	})
}

// Close seals admission before it waits, as it always did: a Prompt after
// Close is refused, with no turn left in flight.
func TestNDJSONBridgeClient_PromptAfterCloseIsRefused(t *testing.T) {
	skipUnlessSh(t)
	forEachComponent(t, func(t *testing.T, component string) {
		client := newTestClient(component, scriptedAgent(t, ""), filepath.Join(t.TempDir(), "requests.log"))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Launch(ctx, LaunchParams{Cwd: t.TempDir()}); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if err := client.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := client.Prompt(ctx, "late"); err == nil {
			t.Fatal("a Prompt after Close returned nil")
		}
		client.turnMu.Lock()
		inFlight := client.currentTurnID
		client.turnMu.Unlock()
		if inFlight != "" {
			t.Fatalf("turn %q in flight after a refused Prompt", inFlight)
		}
	})
}
