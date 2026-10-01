// Package launch is the one call an app makes to pick an agent runtime:
// [Select] takes a runtime id (or registry alias) and a mode and returns the
// [adapters.Adapter] that launches it, native or ACP, ready for
// wrapper.Config.Adapter.
//
// Which runtimes exist, their aliases, the modes each supports and each one's
// default mode come from the go-providers runtime registry, over the
// agent-contracts-leaf runtimes vocabulary (D-73). This package adds the
// launch factories: one per (runtime, mode) the wrapper can drive (D-71).
// Native factories wrap a go-providers CLIAdapter; ACP factories wrap the
// claudeacp, codexacp, copilotacp, opencodeacp and piacp adapters. A mode the
// registry lists but no factory builds (Claude's PTY, a human TUI path) is
// ErrUnsupportedSelection, never a silent substitute.
//
// The factory set is closed, like the registry: there is no registration API.
// [Supported] enumerates it.
package launch
