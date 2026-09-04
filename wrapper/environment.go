package wrapper

import (
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// EnvironmentMode defines how [ChildEnvironment] derives the environment
// passed to a wrapped subprocess.
type EnvironmentMode string

const (
	// EnvironmentInherit snapshots the wrapper process environment. Set must
	// be empty; Allowlist and Unset may narrow the inherited snapshot. This is
	// also the zero-value mode for backward compatibility.
	EnvironmentInherit EnvironmentMode = "inherit"

	// EnvironmentMerge snapshots the wrapper process environment, optionally
	// narrows it with Allowlist, then applies Set and Unset.
	EnvironmentMerge EnvironmentMode = "merge"

	// EnvironmentReplace starts from no inherited entries, applies Set, then
	// applies Unset. Allowlist is invalid because there is no inherited base.
	EnvironmentReplace EnvironmentMode = "replace"
)

// ChildEnvironment is the explicit subprocess-environment contract on
// [Config]. It is materialized to a fresh, deterministic KEY=VALUE slice for
// every [Wrapper.Run]; no shell or command-string construction is involved.
//
// Allowlist applies only to inherited entries. Nil means "allow every
// inherited key"; a non-nil empty slice means "allow none". Set entries are
// applied in order and the last duplicate wins. Unset is applied last, so it
// always wins over both an inherited value and Set. Invalid names, assignments,
// or NUL bytes fail the launch before any child is spawned.
// Names follow the host platform: comparisons are case-insensitive on Windows
// and case-sensitive elsewhere.
//
// The zero value preserves the historical behavior: inherit the wrapper
// process environment unchanged. Security-sensitive hosts should normally use
// EnvironmentReplace with a fully composed allowlisted environment, or
// EnvironmentMerge with an explicit non-nil Allowlist.
type ChildEnvironment struct {
	Mode      EnvironmentMode
	Allowlist []string
	Set       []string
	Unset     []string
}

var (
	// ErrInvalidEnvironment is wrapped when a ChildEnvironment mode, variable
	// name, or KEY=VALUE assignment is invalid.
	ErrInvalidEnvironment = errors.New("wrapper: invalid child environment")

	// ErrEnvironmentSetInInherit is wrapped when EnvironmentInherit is paired
	// with Set. Use EnvironmentMerge when overrides are intended.
	ErrEnvironmentSetInInherit = errors.New("wrapper: child environment inherit mode cannot set values")

	// ErrEnvironmentAllowlistInReplace is wrapped when EnvironmentReplace is
	// paired with Allowlist. Put explicitly permitted values in Set instead.
	ErrEnvironmentAllowlistInReplace = errors.New("wrapper: child environment replace mode cannot use an inherited allowlist")
)

// resolve materializes cfg against inherited. The returned slice is always a
// fresh non-nil value; explicit reports whether the caller selected a mode
// other than the backward-compatible zero/inherit configuration.
func (cfg ChildEnvironment) resolve(inherited []string) (env []string, explicit bool, err error) {
	return cfg.resolveForOS(inherited, runtime.GOOS)
}

func (cfg ChildEnvironment) resolveForOS(inherited []string, goos string) (env []string, explicit bool, err error) {
	mode := cfg.Mode
	if mode == "" {
		mode = EnvironmentInherit
	}
	if mode != EnvironmentInherit && mode != EnvironmentMerge && mode != EnvironmentReplace {
		return nil, false, fmt.Errorf("%w: unknown mode %q", ErrInvalidEnvironment, cfg.Mode)
	}
	if mode == EnvironmentInherit && len(cfg.Set) != 0 {
		return nil, false, ErrEnvironmentSetInInherit
	}
	if mode == EnvironmentReplace && cfg.Allowlist != nil {
		return nil, false, ErrEnvironmentAllowlistInReplace
	}

	allow, err := environmentNameSet(cfg.Allowlist, "allowlist", goos)
	if err != nil {
		return nil, false, err
	}
	unset, err := environmentNameSet(cfg.Unset, "unset", goos)
	if err != nil {
		return nil, false, err
	}

	type entry struct {
		name  string
		value string
	}
	values := make(map[string]entry, len(inherited)+len(cfg.Set))
	if mode != EnvironmentReplace {
		for _, assignment := range inherited {
			key, value, ok := strings.Cut(assignment, "=")
			if !ok || key == "" {
				// os.Environ produces ordinary KEY=VALUE entries on the Unix
				// platforms this process host supports. Ignore malformed ambient
				// entries rather than reflecting them into a sanitized child.
				continue
			}
			canonicalKey := canonicalEnvironmentName(key, goos)
			if cfg.Allowlist != nil {
				if _, ok := allow[canonicalKey]; !ok {
					continue
				}
			}
			values[canonicalKey] = entry{name: key, value: value}
		}
	}

	for i, assignment := range cfg.Set {
		key, value, ok := strings.Cut(assignment, "=")
		if !ok {
			return nil, false, fmt.Errorf("%w: set[%d] is not KEY=VALUE", ErrInvalidEnvironment, i)
		}
		if err := validateEnvironmentName(key); err != nil {
			return nil, false, fmt.Errorf("%w: set[%d]: %v", ErrInvalidEnvironment, i, err)
		}
		if strings.ContainsRune(value, '\x00') {
			return nil, false, fmt.Errorf("%w: set[%d] value contains NUL", ErrInvalidEnvironment, i)
		}
		values[canonicalEnvironmentName(key, goos)] = entry{name: key, value: value}
	}
	for canonicalKey := range unset {
		delete(values, canonicalKey)
	}

	entries := make([]entry, 0, len(values))
	for _, value := range values {
		entries = append(entries, value)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	env = make([]string, 0, len(entries))
	for _, value := range entries {
		env = append(env, value.name+"="+value.value)
	}
	return env, cfg.Mode != "" || cfg.Allowlist != nil || len(cfg.Unset) != 0, nil
}

// resolvedSpecEnvironment applies the Adapter.Resolve result to the wrapper's
// materialized base. A nil Spec.Env inherits the base unchanged; a non-nil
// value replaces it and is normalized with the same last-duplicate-wins,
// deterministic validation as Config.Environment.Set.
func resolvedSpecEnvironment(base, spec []string) ([]string, bool, error) {
	if spec == nil {
		return append([]string{}, base...), false, nil
	}
	env, _, err := (ChildEnvironment{Mode: EnvironmentReplace, Set: spec}).resolve(nil)
	return env, true, err
}

func environmentNameSet(names []string, field, goos string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(names))
	for i, name := range names {
		if err := validateEnvironmentName(name); err != nil {
			return nil, fmt.Errorf("%w: %s[%d]: %v", ErrInvalidEnvironment, field, i, err)
		}
		set[canonicalEnvironmentName(name, goos)] = struct{}{}
	}
	return set, nil
}

func canonicalEnvironmentName(name, goos string) string {
	if goos == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

func validateEnvironmentName(name string) error {
	switch {
	case name == "":
		return errors.New("name is empty")
	case strings.ContainsRune(name, '='):
		return errors.New("name contains '='")
	case strings.ContainsRune(name, '\x00'):
		return errors.New("name contains NUL")
	default:
		return nil
	}
}

func cloneStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string{}, in...)
}

// nonInheritingEmptyEnvironment is used only for long-lived agentkit
// runtimes. agentkit v0.5.0 treats len(StartOptions.Env)==0 as "inherit",
// even when the caller supplied a non-nil empty slice. A benign private marker
// keeps an explicitly empty sanitized environment non-empty at that boundary,
// preventing ambient secrets from reappearing. Subprocess-per-turn uses
// go-runner, which already distinguishes nil from an empty slice and does not
// need the marker.
const nonInheritingEmptyEnvironment = "GO_AGENT_WRAPPER_EMPTY_ENVIRONMENT=1"
