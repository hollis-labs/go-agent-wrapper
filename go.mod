module github.com/hollis-labs/go-agent-wrapper

go 1.26.1

require (
	github.com/hollis-labs/agentkit v0.5.0
	github.com/hollis-labs/go-harness-filters v0.1.0
	github.com/hollis-labs/go-llm-types v0.3.0
	github.com/hollis-labs/go-providers v0.23.0
	github.com/hollis-labs/go-runtime-events v0.1.0
	github.com/hollis-labs/go-sandbox v0.2.1
)

require (
	github.com/creack/pty v1.1.24 // indirect
	github.com/hollis-labs/go-llm-contracts v0.3.0 // indirect
	github.com/hollis-labs/go-runner v0.5.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Local-development replaces. These let local edits to the sibling libs
// flow through without publishing a new tag for each iteration; the
// require lines above resolve from the proxy normally when a given
// replace is absent.
//
// As of v0.4.0 (TASKS/agent-host-acp/20+22's real correctness fixes to
// agentkit's session-waiter/completion-signaling code), the agentkit
// replace is dropped: v0.5.0 is now pushed and tagged on origin, so the
// require line above resolves directly — no local replace needed.
// go-harness-filters and go-runtime-events keep the older "drop before
// tagging" discipline — nothing outside this repo depends on their
// replace staying.
replace (
	github.com/hollis-labs/go-harness-filters => ../go-harness-filters
	github.com/hollis-labs/go-runtime-events => ../go-runtime-events
)
