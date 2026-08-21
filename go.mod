module github.com/hollis-labs/go-agent-wrapper

go 1.26.1

require (
	github.com/hollis-labs/agentkit v0.3.0
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

// Local-development replaces. These let local edits to the sibling libs
// flow through without publishing a new tag for each iteration; the
// require lines above resolve from the proxy normally when a given
// replace is absent.
//
// As of v0.2.0, the agentkit replace is intentionally still present in a
// tagged release: this repo's own require (v0.3.0) is current and this
// replace resolves to the same content (agentkit's agentsessions package,
// the only agentkit package this repo imports, is unchanged between
// v0.1.0 and v0.3.0), but Nanite's own go.mod needs a matching local
// replace pointing at go-agent-wrapper itself (same pattern as its
// existing go-modelsdev/go-envelopes replaces) until go-agent-wrapper has
// enough tagged release history to be pinned via the module proxy
// instead. Revisit dropping this once that Nanite-side dependency
// stabilizes. go-harness-filters and go-runtime-events keep the older
// "drop before tagging" discipline — nothing outside this repo depends on
// their replace staying.
replace (
	github.com/hollis-labs/agentkit => ../agentkit
	github.com/hollis-labs/go-harness-filters => ../go-harness-filters
	github.com/hollis-labs/go-runtime-events => ../go-runtime-events
)
