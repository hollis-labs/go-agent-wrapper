package copilotacp

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider"
)

// cliAdapterGlue is the [provider.CLIAdapter] [Adapter.CLIAdapter]
// returns. See the package doc's "Wrapper.Run composition" note for
// exactly what this does and does not do — this is intentionally NOT a
// full reimplementation of [Client]'s handshake; it exists so this
// Adapter is structurally usable through
// [github.com/hollis-labs/go-agent-wrapper/wrapper.Wrapper.Run]'s
// existing jsonrpc-stdio dispatch, with real (not placeholder) argv and
// real (not placeholder) read-side translation.
type cliAdapterGlue struct {
	adapter *Adapter
}

// Name implements [provider.CLIAdapter].
func (g *cliAdapterGlue) Name() string { return "copilot" }

// Detect implements [provider.CLIAdapter]. Mirrors the env-var-first,
// then-PATH convention every other adapter in this repo uses
// (CLAUDE_CLI_PATH/CODEX_CLI_PATH/OPENCODE_CLI_PATH — here,
// COPILOT_CLI_PATH), except when [WithAdapterBinary] pins an explicit
// path.
func (g *cliAdapterGlue) Detect() (string, bool) {
	if g.adapter.binary != "" {
		return g.adapter.binary, true
	}
	if p := os.Getenv("COPILOT_CLI_PATH"); p != "" {
		return p, true
	}
	p, err := exec.LookPath("copilot")
	if err != nil {
		return "", false
	}
	return p, true
}

// BuildArgs implements [provider.CLIAdapter]. Called once at spawn time
// by agentkit's jsonrpc-stdio runtime, before the child process exists
// — see the package doc for why this returns real argv (so the real
// `copilot --acp` process is what gets spawned) but does NOT attempt to
// drive [Client]'s handshake: BuildArgs has no writer to send
// `initialize` with at this point.
func (g *cliAdapterGlue) BuildArgs(_, _, _ string) []string {
	args := []string{"--acp"}
	if g.adapter.transport == adapters.TransportTCP {
		args = append(args, "--port", strconv.Itoa(g.adapter.port))
	}
	return append(args, g.adapter.extraArgs...)
}

// ParseLine implements [provider.CLIAdapter]. Recognizes `session/update`
// notifications on whatever the child writes to stdout and translates
// them into [llmtypes.StreamEvent] via the same parseSessionUpdate/
// acpUpdateToStreamEvent logic [Client] itself uses (translate.go) — real
// translation, not a placeholder. Lines that aren't `session/update`
// notifications (responses, other notifications, malformed JSON) return
// (nil, nil), matching [provider.CodexAdapter]'s own app-server-mode
// precedent of returning no events for frames outside its scope.
func (g *cliAdapterGlue) ParseLine(line []byte) ([]llmtypes.StreamEvent, error) {
	var f wireFrame
	if err := json.Unmarshal(line, &f); err != nil {
		return nil, nil
	}
	if f.Method != "session/update" {
		return nil, nil
	}
	var su sessionUpdateParams
	if err := json.Unmarshal(f.Params, &su); err != nil {
		return nil, nil
	}
	upd, ok := parseSessionUpdate(su.Update)
	if !ok {
		return nil, nil
	}
	ev, ok := acpUpdateToStreamEvent(upd)
	if !ok {
		return nil, nil
	}
	return []llmtypes.StreamEvent{ev}, nil
}

var _ provider.CLIAdapter = (*cliAdapterGlue)(nil)
