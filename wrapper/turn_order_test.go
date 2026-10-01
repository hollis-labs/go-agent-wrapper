package wrapper

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider"

	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/adapters/claude"
	"github.com/hollis-labs/go-agent-wrapper/launch"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// CW-20260930-0137 slice a (root cause of CW-20261001-0019). A native turn
// emits exactly one terminal event, carrying the turn id and the usage the
// turn reported, and session.idle follows it. Before the fix, usage mapped to
// its own turn.completed, which closed the turn early, so the feed read
// turn.completed(turn_id, usage) → session.idle → turn.completed(no turn_id).

// turnStep is one lifecycle event as the order tests compare it.
type turnStep struct {
	Kind   runtimeevents.EventKind
	TurnID string
}

// turnLifecycle keeps the events whose order the tests pin, in emit order.
func turnLifecycle(evs []runtimeevents.Event) []turnStep {
	var out []turnStep
	for _, ev := range evs {
		switch ev.Kind {
		case runtimeevents.KindTurnStarted,
			runtimeevents.KindSessionProcessing,
			runtimeevents.KindAgentDelta,
			runtimeevents.KindTurnCompleted,
			runtimeevents.KindTurnFailed,
			runtimeevents.KindSessionIdle,
			runtimeevents.KindProcessExited:
			out = append(out, turnStep{Kind: ev.Kind, TurnID: ev.TurnID})
		}
	}
	return out
}

// wantOneTurn is the exact lifecycle of one turn that ends in terminal,
// followed by the process exiting.
func wantOneTurn(turnID string, terminal runtimeevents.EventKind) []turnStep {
	return []turnStep{
		{runtimeevents.KindTurnStarted, turnID},
		{runtimeevents.KindSessionProcessing, turnID},
		{runtimeevents.KindAgentDelta, turnID},
		{terminal, turnID},
		{runtimeevents.KindSessionIdle, turnID},
		{runtimeevents.KindProcessExited, ""},
	}
}

func assertOneTurn(t *testing.T, evs []runtimeevents.Event, terminal runtimeevents.EventKind) runtimeevents.Event {
	t.Helper()
	got := turnLifecycle(evs)
	if len(got) == 0 || got[0].Kind != runtimeevents.KindTurnStarted || got[0].TurnID == "" {
		t.Fatalf("lifecycle does not open with a tagged turn.started: %+v", got)
	}
	if want := wantOneTurn(got[0].TurnID, terminal); !reflect.DeepEqual(got, want) {
		t.Fatalf("lifecycle =\n  %+v\nwant\n  %+v", got, want)
	}
	for _, ev := range evs {
		if ev.Kind == terminal {
			return ev
		}
	}
	t.Fatalf("no %s event", terminal)
	return runtimeevents.Event{}
}

// terminalUsage decodes the usage a terminal event carries.
func terminalUsage(t *testing.T, ev runtimeevents.Event) llmtypes.Usage {
	t.Helper()
	var p struct {
		Usage *llmtypes.Usage `json:"usage"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("decode %s payload %s: %v", ev.Kind, ev.Payload, err)
	}
	if p.Usage == nil {
		t.Fatalf("%s payload carries no usage: %s", ev.Kind, ev.Payload)
	}
	return *p.Usage
}

// stopWhenIdle ends a session once its turn has gone idle. The fake children
// in these tests block on stdin after their last line rather than exiting
// straight after printing it: agentkit's long-lived runtimes call cmd.Wait
// while the reader may still be draining stdout, and Wait closes the pipe, so
// a child that prints and exits at once can lose its last lines before the
// wrapper reads them (CW-20261001-0046).
func stopWhenIdle(t *testing.T) func(w *Wrapper, sink *capturingSink) {
	return func(w *Wrapper, sink *capturingSink) {
		sink.waitFor(t, runtimeevents.KindSessionIdle, 5*time.Second)
		_ = w.Stop(context.Background())
	}
}

func runToExit(t *testing.T, cfg Config, drive func(w *Wrapper, sink *capturingSink)) []runtimeevents.Event {
	t.Helper()
	sink := newCapturingSink()
	cfg.Activity = activity.NewBridge(sink)
	w, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- w.Run(ctx) }()
	if drive != nil {
		drive(w, sink)
	}
	select {
	case <-runErrCh:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
	return sink.snapshot()
}

// Claude, streaming stdio: the result line carries usage and ends the turn.
func TestNativeTurnOrder_ClaudeStreamingStdio(t *testing.T) {
	skipUnlessSh(t)
	dir := t.TempDir()
	script := writeShellFixtureLauncher(t, dir, "fake-claude", []byte(`#!/bin/sh
IFS= read -r line
printf '%s\n' '{"type":"system","subtype":"init","session_id":"claude-order"}'
printf '%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","result":"hello","stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}'
while IFS= read -r line; do :; done
`))
	t.Setenv("CLAUDE_CLI_PATH", script)

	evs := runToExit(t, Config{
		App:               "test-turn-order",
		Adapter:           claude.New(),
		Workdir:           dir,
		AutoFireFirstTurn: true,
		FirstTurnPayload:  `{"type":"user","message":{"role":"user","content":"hi"}}`,
	}, stopWhenIdle(t))

	done := assertOneTurn(t, evs, runtimeevents.KindTurnCompleted)
	want := llmtypes.Usage{InputTokens: 11, OutputTokens: 7, CacheCreationTokens: 2, CacheReadTokens: 3, StopReason: "end_turn"}
	if got := terminalUsage(t, done); got != want {
		t.Errorf("turn.completed usage = %+v, want %+v", got, want)
	}
}

// Claude, streaming stdio, a turn that ends in error: one tagged turn.failed,
// no turn.completed, then idle.
func TestNativeTurnOrder_ClaudeStreamingStdioError(t *testing.T) {
	skipUnlessSh(t)
	dir := t.TempDir()
	script := writeShellFixtureLauncher(t, dir, "fake-claude", []byte(`#!/bin/sh
IFS= read -r line
printf '%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial"}]}}'
printf '%s\n' '{"type":"result","subtype":"error","is_error":true,"result":"overloaded"}'
while IFS= read -r line; do :; done
`))
	t.Setenv("CLAUDE_CLI_PATH", script)

	evs := runToExit(t, Config{
		App:               "test-turn-order",
		Adapter:           claude.New(),
		Workdir:           dir,
		AutoFireFirstTurn: true,
		FirstTurnPayload:  `{"type":"user","message":{"role":"user","content":"hi"}}`,
	}, stopWhenIdle(t))

	failed := assertOneTurn(t, evs, runtimeevents.KindTurnFailed)
	if !strings.Contains(string(failed.Payload), "overloaded") {
		t.Errorf("turn.failed payload = %s, want the provider's error", failed.Payload)
	}
}

// Codex, subprocess per turn (`codex exec --json`, the shape Nanite launches
// Codex with): turn.completed carries usage and ends the turn.
func TestNativeTurnOrder_CodexSubprocessPerTurn(t *testing.T) {
	skipUnlessSh(t)
	dir := t.TempDir()
	script := writeShellFixtureLauncher(t, dir, "fake-codex", []byte(`#!/bin/sh
printf '%s\n' '{"type":"thread.started","thread_id":"codex-order"}'
printf '%s\n' '{"type":"turn.started"}'
printf '%s\n' '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hello"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":20,"cached_input_tokens":5,"output_tokens":9}}'
`))
	t.Setenv("CODEX_CLI_PATH", script)

	adapter, err := launch.Select(launch.Selection{
		Runtime: "codex",
		Mode:    runtimes.ModeSubprocessPerTurn,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	evs := runToExit(t, Config{App: "test-turn-order", Adapter: adapter, Workdir: dir}, func(w *Wrapper, sink *capturingSink) {
		sink.waitFor(t, runtimeevents.KindSessionReady, 5*time.Second)
		if err := w.SendInput(context.Background(), []byte("hi")); err != nil {
			t.Errorf("SendInput: %v", err)
		}
		sink.waitFor(t, runtimeevents.KindSessionIdle, 5*time.Second)
		_ = w.Stop(context.Background())
	})

	done := assertOneTurn(t, evs, runtimeevents.KindTurnCompleted)
	want := llmtypes.Usage{InputTokens: 20, OutputTokens: 9, CacheReadTokens: 5}
	if got := terminalUsage(t, done); got != want {
		t.Errorf("turn.completed usage = %+v, want %+v", got, want)
	}
}

// codexAppServerShapeCLI maps Codex app-server notifications onto stream
// events, so a jsonrpc-stdio session produces a Codex-shaped turn. The real
// go-providers adapter passes app-server lines through without events (its
// ParseLine returns nothing in app-server mode), so this stands in for an
// adapter that surfaces them; the method names are the real ones.
type codexAppServerShapeCLI struct{ script string }

func (c codexAppServerShapeCLI) Name() string                      { return "codex" }
func (c codexAppServerShapeCLI) BuildArgs(_, _, _ string) []string { return nil }
func (c codexAppServerShapeCLI) Detect() (string, bool)            { return c.script, true }

func (c codexAppServerShapeCLI) ParseLine(line []byte) ([]llmtypes.StreamEvent, error) {
	var n struct {
		Method string `json:"method"`
		Params struct {
			Delta      string `json:"delta"`
			TokenUsage struct {
				Last struct {
					InputTokens       int `json:"inputTokens"`
					CachedInputTokens int `json:"cachedInputTokens"`
					OutputTokens      int `json:"outputTokens"`
				} `json:"last"`
			} `json:"tokenUsage"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &n); err != nil {
		return nil, nil
	}
	switch n.Method {
	case "item/agentMessage/delta":
		return []llmtypes.StreamEvent{{Type: llmtypes.EventDelta, Content: n.Params.Delta}}, nil
	case "thread/tokenUsage/updated":
		last := n.Params.TokenUsage.Last
		return []llmtypes.StreamEvent{{Type: llmtypes.EventUsage, Usage: &llmtypes.Usage{
			InputTokens: last.InputTokens, OutputTokens: last.OutputTokens, CacheReadTokens: last.CachedInputTokens,
		}}}, nil
	case "turn/completed":
		return []llmtypes.StreamEvent{{Type: llmtypes.EventDone}}, nil
	}
	return nil, nil
}

type codexAppServerShapeAdapter struct{ cli codexAppServerShapeCLI }

func (a codexAppServerShapeAdapter) Name() string { return "codex" }
func (a codexAppServerShapeAdapter) Describe() adapters.Descriptor {
	return adapters.Descriptor{
		Provider:  "codex",
		Protocol:  adapters.ProtocolCodexAppServer,
		Transport: adapters.TransportStdio,
		Interrupt: adapters.InterruptProcess,
		Channels:  []runtimeevents.SourceChannel{runtimeevents.ChannelJSONRPC},
	}
}
func (a codexAppServerShapeAdapter) Resolve(rc adapters.ResolveContext) (adapters.Spec, error) {
	return adapters.Spec{Binary: a.cli.script, Cwd: rc.Cwd, Env: rc.Env}, nil
}
func (a codexAppServerShapeAdapter) CLIAdapter() provider.CLIAdapter { return a.cli }

// Codex, jsonrpc-stdio (app-server shape): token usage arrives as its own
// notification before turn/completed, and the long-lived child keeps running
// after the turn.
func TestNativeTurnOrder_CodexJSONRPCStdio(t *testing.T) {
	skipUnlessSh(t)
	dir := t.TempDir()
	script := writeShellFixtureLauncher(t, dir, "fake-codex-app-server", []byte(`#!/bin/sh
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"t","turn":{"id":"u"}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"t","turnId":"u","itemId":"i","delta":"hello"}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"t","turnId":"u","tokenUsage":{"last":{"inputTokens":30,"cachedInputTokens":4,"outputTokens":6}}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t","turn":{"id":"u","status":"completed"}}}'
while IFS= read -r line; do :; done
`))

	evs := runToExit(t, Config{
		App:               "test-turn-order",
		Adapter:           codexAppServerShapeAdapter{cli: codexAppServerShapeCLI{script: script}},
		Workdir:           dir,
		AutoFireFirstTurn: true,
		FirstTurnPayload:  `{"jsonrpc":"2.0","id":1,"method":"turn/start","params":{}}`,
	}, stopWhenIdle(t))

	done := assertOneTurn(t, evs, runtimeevents.KindTurnCompleted)
	want := llmtypes.Usage{InputTokens: 30, OutputTokens: 6, CacheReadTokens: 4}
	if got := terminalUsage(t, done); got != want {
		t.Errorf("turn.completed usage = %+v, want %+v", got, want)
	}
}

// A child that exits with a turn open and no terminal event: the turn is
// flushed as one tagged turn.failed carrying the usage it reported, then
// idle, before process.exited. The child reports usage before its delta, so
// once the delta is seen the usage has been taken, and it exits only when
// the test sends a second line.
func TestNativeTurnOrder_ExitMidTurnFlushesTurnFailed(t *testing.T) {
	skipUnlessSh(t)
	dir := t.TempDir()
	script := writeShellFixtureLauncher(t, dir, "fake-codex-app-server", []byte(`#!/bin/sh
IFS= read -r line
printf '%s\n' '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"tokenUsage":{"last":{"inputTokens":8,"cachedInputTokens":0,"outputTokens":1}}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"delta":"partial"}}'
IFS= read -r line
exit 3
`))

	evs := runToExit(t, Config{
		App:               "test-turn-order",
		Adapter:           codexAppServerShapeAdapter{cli: codexAppServerShapeCLI{script: script}},
		Workdir:           dir,
		AutoFireFirstTurn: true,
		FirstTurnPayload:  `{"jsonrpc":"2.0","id":1,"method":"turn/start","params":{}}`,
	}, func(w *Wrapper, sink *capturingSink) {
		sink.waitFor(t, runtimeevents.KindAgentDelta, 5*time.Second)
		if err := w.SendInput(context.Background(), []byte(`{"jsonrpc":"2.0","method":"exit"}`)); err != nil {
			t.Errorf("SendInput: %v", err)
		}
	})

	failed := assertOneTurn(t, evs, runtimeevents.KindTurnFailed)
	var p struct {
		Reason   string `json:"reason"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(failed.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Reason != "process_exited" || p.ExitCode != 3 {
		t.Errorf("turn.failed payload = %s, want reason process_exited and exit_code 3", failed.Payload)
	}
	if got, want := terminalUsage(t, failed), (llmtypes.Usage{InputTokens: 8, OutputTokens: 1}); got != want {
		t.Errorf("turn.failed usage = %+v, want %+v", got, want)
	}
}
