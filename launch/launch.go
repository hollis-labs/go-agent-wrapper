package launch

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-providers/registry"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
)

// Selection is what an app asks for: a runtime, a mode, and the few launch
// facts that are not runtime-specific policy.
type Selection struct {
	// Runtime is a registry runtime id or alias ("claude", "claude-code",
	// "agy", "copilot"). Required.
	Runtime string

	// Mode is how the runtime is driven. Empty selects the registry's
	// default mode for the runtime: native where the runtime has one.
	Mode runtimes.Mode

	// DeveloperMode selects Claude's native developer variant
	// (--dangerously-skip-permissions). It is defined only for Claude's
	// native modes and cannot be combined with CLIAdapter.
	DeveloperMode bool

	// CLIAdapter, when non-nil, is the host's already-configured
	// go-providers adapter for a native mode. It is returned through the
	// selected adapter unchanged except for Binary/ExtraArgs decoration.
	// Select checks its name against the runtime and, for the built-in
	// go-providers adapter types, its shape against the mode. Not
	// accepted for an ACP mode.
	CLIAdapter provider.CLIAdapter

	// Binary optionally pins the executable without consulting PATH or the
	// runtime's env override. It must be absolute; it is passed to os/exec
	// directly, never through a shell. For an ACP mode it is the process
	// the ACP client spawns (Claude's and Codex's bridge, copilot, the
	// pi-acp bridge, opencode).
	Binary string

	// ExtraArgs are appended as distinct argv entries. Spaces and shell
	// metacharacters stay data.
	ExtraArgs []string

	// Port is the TCP port Copilot's ACP daemon listens on
	// (copilot --acp --port N). Only valid with runtimes.ModeACPTCP; zero
	// keeps the copilotacp default.
	Port int
}

var (
	// ErrUnsupportedSelection is wrapped when the runtime is unknown, the
	// registry does not list the mode for it, or no launch factory drives
	// that (runtime, mode).
	ErrUnsupportedSelection = errors.New("launch: unsupported selection")

	// ErrInvalidSelection is wrapped when fields are malformed or do not
	// fit the selected mode.
	ErrInvalidSelection = errors.New("launch: invalid selection")
)

// Key names one launch factory.
type Key struct {
	Runtime runtimes.ID
	Mode    runtimes.Mode
}

// String renders "<runtime>/<mode>".
func (k Key) String() string { return string(k.Runtime) + "/" + string(k.Mode) }

// factory builds the adapter for a validated Selection whose Runtime is the
// canonical id and whose Mode is set.
type factory func(Selection) (adapters.Adapter, error)

// factories is the closed set of launchable (runtime, mode) pairs.
var factories = map[Key]factory{}

func init() {
	for k, f := range nativeFactories {
		factories[k] = f
	}
	for k, f := range acpFactories {
		factories[k] = f
	}
}

// Select returns the adapter that launches sel.Runtime in sel.Mode (or the
// registry's default mode). It returns ErrUnsupportedSelection for an unknown
// runtime, a mode the registry does not list for it, or a pair no factory
// drives, and ErrInvalidSelection for malformed or mode-incompatible fields.
func Select(sel Selection) (adapters.Adapter, error) {
	d, ok := registry.Lookup(sel.Runtime)
	if !ok {
		return nil, fmt.Errorf("%w: unknown runtime %q", ErrUnsupportedSelection, sel.Runtime)
	}
	mode := sel.Mode
	if mode == "" {
		mode = d.DefaultMode
	}
	if !d.Supports(mode) {
		return nil, fmt.Errorf("%w: %s does not support mode %q", ErrUnsupportedSelection, d.ID, mode)
	}
	key := Key{Runtime: d.ID, Mode: mode}
	f, ok := factories[key]
	if !ok {
		return nil, fmt.Errorf("%w: no launch factory for %s", ErrUnsupportedSelection, key)
	}
	if err := validate(sel, key); err != nil {
		return nil, err
	}
	sel.Runtime, sel.Mode = string(d.ID), mode
	sel.ExtraArgs = append([]string(nil), sel.ExtraArgs...)
	return f(sel)
}

// Supported returns every (runtime, mode) a factory launches, in registry
// order.
func Supported() []Key {
	var out []Key
	for _, d := range registry.All() {
		for _, ms := range d.Modes {
			if k := (Key{Runtime: d.ID, Mode: ms.Mode}); factories[k] != nil {
				out = append(out, k)
			}
		}
	}
	return out
}

func validate(sel Selection, key Key) error {
	if sel.Binary != "" {
		if strings.ContainsRune(sel.Binary, '\x00') {
			return fmt.Errorf("%w: Binary contains NUL", ErrInvalidSelection)
		}
		if !filepath.IsAbs(sel.Binary) {
			return fmt.Errorf("%w: Binary must be absolute", ErrInvalidSelection)
		}
	}
	for i, arg := range sel.ExtraArgs {
		if strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("%w: ExtraArgs[%d] contains NUL", ErrInvalidSelection, i)
		}
	}
	if sel.DeveloperMode && (key.Runtime != runtimes.Claude || key.Mode.ACP()) {
		return fmt.Errorf("%w: DeveloperMode is only defined for Claude's native modes, not %s", ErrInvalidSelection, key)
	}
	if sel.CLIAdapter != nil {
		if key.Mode.ACP() {
			return fmt.Errorf("%w: CLIAdapter is for native modes, not %s", ErrInvalidSelection, key)
		}
		if sel.DeveloperMode {
			return fmt.Errorf("%w: DeveloperMode cannot be combined with CLIAdapter", ErrInvalidSelection)
		}
		if err := validateCLIAdapter(key, sel.CLIAdapter); err != nil {
			return err
		}
	}
	if sel.Port != 0 && key.Mode != runtimes.ModeACPTCP {
		return fmt.Errorf("%w: Port is only for %s, not %s", ErrInvalidSelection, runtimes.ModeACPTCP, key)
	}
	if sel.Port < 0 || sel.Port > 65535 {
		return fmt.Errorf("%w: Port %d out of range", ErrInvalidSelection, sel.Port)
	}
	return nil
}
