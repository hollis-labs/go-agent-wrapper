package adapters

import "github.com/hollis-labs/go-providers/provider"

// RuntimeAdapter is an optional [Adapter] capability that exposes the
// go-providers CLIAdapter the wrapper hands to
// github.com/hollis-labs/agentkit/agentsessions at session start time.
//
// Adapters whose runtime is provided by the agentkit/agentsessions stack
// implement this interface. Pure exec-shape adapters — declared via
// [Adapter] alone — do not. The wrapper checks for [RuntimeAdapter] via
// type assertion in its Run path; an Adapter that lacks it cannot be
// driven through the agentkit runtime and the wrapper returns a
// configuration error.
//
// Keeping this as an optional interface lets the base [Adapter]
// contract stay neutral about go-providers — apps that declare exec
// shapes for their own runtime mechanism are not forced to depend on
// the agentkit toolchain.
type RuntimeAdapter interface {
	Adapter

	// CLIAdapter returns the go-providers CLIAdapter the wrapper should
	// hand to agentkit/agentsessions.NewFromAdapter. Implementations
	// typically return a fresh adapter per call so the wrapper can mutate
	// per-session state on it without affecting other sessions.
	CLIAdapter() provider.CLIAdapter
}
