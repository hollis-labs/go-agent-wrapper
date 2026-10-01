package launch

import (
	"github.com/hollis-labs/agent-contracts-leaf/runtimes"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	"github.com/hollis-labs/go-agent-wrapper/adapters/claudeacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/codexacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/copilotacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/opencodeacp"
	"github.com/hollis-labs/go-agent-wrapper/adapters/piacp"
)

// acpFactories is every ACP (runtime, mode) the wrapper launches. Binary is
// the process the ACP client spawns: the claude-agent-acp bridge run
// directly instead of through npx, the codex-acp bridge, opencode, copilot,
// or the pi-acp bridge.
var acpFactories = map[Key]factory{
	{runtimes.Claude, runtimes.ModeACPStdio}: func(sel Selection) (adapters.Adapter, error) {
		var opts []claudeacp.Option
		if sel.Binary != "" {
			opts = append(opts, claudeacp.WithDirectBinary(sel.Binary))
		}
		if len(sel.ExtraArgs) > 0 {
			opts = append(opts, claudeacp.WithExtraArgs(sel.ExtraArgs...))
		}
		return claudeacp.New(opts...), nil
	},
	{runtimes.Codex, runtimes.ModeACPStdio}: func(sel Selection) (adapters.Adapter, error) {
		var opts []codexacp.Option
		if sel.Binary != "" {
			opts = append(opts, codexacp.WithBinary(sel.Binary))
		}
		if len(sel.ExtraArgs) > 0 {
			opts = append(opts, codexacp.WithExtraArgs(sel.ExtraArgs...))
		}
		return codexacp.New(opts...), nil
	},
	{runtimes.OpenCode, runtimes.ModeACPStdio}: func(sel Selection) (adapters.Adapter, error) {
		var opts []opencodeacp.Option
		if sel.Binary != "" {
			opts = append(opts, opencodeacp.WithBinary(sel.Binary))
		}
		if len(sel.ExtraArgs) > 0 {
			opts = append(opts, opencodeacp.WithExtraArgs(sel.ExtraArgs...))
		}
		return opencodeacp.New(opts...), nil
	},
	{runtimes.Copilot, runtimes.ModeACPStdio}: func(sel Selection) (adapters.Adapter, error) {
		return copilotacp.New(copilotOptions(sel, adapters.TransportStdio)...), nil
	},
	{runtimes.Copilot, runtimes.ModeACPTCP}: func(sel Selection) (adapters.Adapter, error) {
		return copilotacp.New(copilotOptions(sel, adapters.TransportTCP)...), nil
	},
	{runtimes.Pi, runtimes.ModeACPStdio}: func(sel Selection) (adapters.Adapter, error) {
		var opts []piacp.Option
		if sel.Binary != "" {
			opts = append(opts, piacp.WithBinary(sel.Binary))
		}
		if len(sel.ExtraArgs) > 0 {
			opts = append(opts, piacp.WithExtraArgs(sel.ExtraArgs...))
		}
		return piacp.New(opts...), nil
	},
}

func copilotOptions(sel Selection, transport adapters.Transport) []copilotacp.AdapterOption {
	opts := []copilotacp.AdapterOption{copilotacp.WithAdapterTransport(transport)}
	if sel.Binary != "" {
		opts = append(opts, copilotacp.WithAdapterBinary(sel.Binary))
	}
	if len(sel.ExtraArgs) > 0 {
		opts = append(opts, copilotacp.WithAdapterExtraArgs(sel.ExtraArgs...))
	}
	if sel.Port != 0 {
		opts = append(opts, copilotacp.WithAdapterPort(sel.Port))
	}
	return opts
}
