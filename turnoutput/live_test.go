package turnoutput_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/go-providers/registry"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/internal/testgate"
	"github.com/hollis-labs/go-agent-wrapper/launch"
	"github.com/hollis-labs/go-agent-wrapper/turnoutput"
	"github.com/hollis-labs/go-agent-wrapper/wrapper"
)

// TestLiveEveryInstalledRuntimeReducesOneFinalTurn launches each installed
// registry runtime in its default mode, asks it for one word, and requires the
// reducer to report exactly one final Output carrying that word. It spends real
// provider calls, so it runs only behind the shared live-provider gate; a
// runtime that is not installed is skipped.
//
// Codex's default mode is the app-server, whose thread protocol is the host's
// to drive rather than the wrapper's, so no turn completes through the wrapper
// and it is skipped here until its events reach the reducer.
func TestLiveEveryInstalledRuntimeReducesOneFinalTurn(t *testing.T) {
	testgate.RequireLiveProvider(t)
	const prompt = "Reply with the single word: pong"
	for _, d := range registry.All() {
		t.Run(string(d.ID)+"/"+string(d.DefaultMode), func(t *testing.T) {
			if d.DefaultMode == runtimes.ModeJSONRPCStdio {
				t.Skipf("%s drives its turns through the host, not the wrapper", d.ID)
			}
			binary, err := d.LookPath()
			if err != nil {
				t.Skipf("%s not installed: %v", d.Binary, err)
			}
			input := prompt
			if d.DefaultMode == runtimes.ModeStreamingStdio {
				input = `{"type":"user","message":{"role":"user","content":"` + prompt + `"}}`
			}

			var out turnoutput.Output
			left := reduceRun(t, launch.Selection{Runtime: string(d.ID), Binary: binary}, 4*time.Minute,
				func(t *testing.T, w *wrapper.Wrapper, manager *acp.Manager, outs <-chan turnoutput.Output) {
					if d.DefaultMode.ACP() {
						waitReady(t, w, manager)
					}
					out = askWithin(t, w, outs, input, 3*time.Minute)
				})
			if len(left) != 0 {
				t.Fatalf("a single turn reported more than one Output: %+v", left)
			}
			if out.Kind != turnoutput.KindFinal || !strings.Contains(strings.ToLower(out.Text), "pong") {
				t.Fatalf("Output = %+v, want a final turn whose text is the word", out)
			}
			if out.Runtime == "" || out.TurnID == "" || out.SessionID == "" {
				t.Fatalf("Output is missing identity: %+v", out)
			}
			t.Logf("%s: text %q, confidence %s, stop %q", d.ID, out.Text, out.Confidence, out.StopReason)
		})
	}
}
