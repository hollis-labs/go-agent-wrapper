// Command claude-stream runs one prompt through Claude's native streaming
// transport and writes normalized runtime events as JSONL to stdout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/hollis-labs/go-agent-wrapper/activity"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/wrapper"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	prompt := flag.String("prompt", "Reply with one short greeting.", "first user prompt")
	workdir := flag.String("workdir", ".", "workspace for the Claude process")
	claude := flag.String("claude", "", "optional absolute path to the Claude executable")
	flag.Parse()

	resolvedWorkdir, err := filepath.Abs(*workdir)
	if err != nil {
		return fmt.Errorf("resolve workdir: %w", err)
	}
	workdirInfo, err := os.Stat(resolvedWorkdir)
	if err != nil {
		return fmt.Errorf("inspect workdir: %w", err)
	}
	if !workdirInfo.IsDir() {
		return fmt.Errorf("workdir %q is not a directory", *workdir)
	}

	adapter, err := adapters.Select(adapters.Selection{
		Provider:    adapters.ProviderClaude,
		RuntimeKind: adapters.RuntimeKindCLI,
		LaunchMode:  adapters.LaunchStreamingStdio,
		Binary:      *claude,
	})
	if err != nil {
		return fmt.Errorf("select Claude adapter: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]string{
			"role":    "user",
			"content": *prompt,
		},
	})
	if err != nil {
		return fmt.Errorf("encode first turn: %w", err)
	}

	encoder := json.NewEncoder(os.Stdout)
	bridge := activity.NewBridge(runtimeevents.SinkFunc(
		func(_ context.Context, event runtimeevents.Event) error {
			return encoder.Encode(event)
		},
	))
	w, err := wrapper.New(wrapper.Config{
		App:               "claude-stream-example",
		Workdir:           resolvedWorkdir,
		Adapter:           adapter,
		Activity:          bridge,
		AutoFireFirstTurn: true,
		FirstTurnPayload:  string(payload),
	})
	if err != nil {
		return fmt.Errorf("construct wrapper: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("run Claude: %w", err)
	}
	return nil
}
