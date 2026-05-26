module github.com/hollis-labs/go-agent-wrapper

go 1.26.1

require (
	github.com/hollis-labs/agentkit v0.1.0
	github.com/hollis-labs/go-harness-filters v0.1.0
	github.com/hollis-labs/go-llm-types v0.3.0
	github.com/hollis-labs/go-providers v0.23.0
	github.com/hollis-labs/go-runtime-events v0.1.0
)

require (
	github.com/creack/pty v1.1.24 // indirect
	github.com/hollis-labs/go-llm-contracts v0.3.0 // indirect
	github.com/hollis-labs/go-runner v0.5.0 // indirect
	github.com/hollis-labs/go-sandbox v0.2.1 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Local-development replaces — DO NOT COMMIT to a tagged release.
// These let local edits to the three sibling libs flow through without
// publishing a new tag for each iteration. Drop (or comment out) before
// tagging a release; the require lines above resolve from the proxy
// normally when these are absent.
replace (
	github.com/hollis-labs/agentkit => ../agentkit
	github.com/hollis-labs/go-harness-filters => ../go-harness-filters
	github.com/hollis-labs/go-runtime-events => ../go-runtime-events
)
