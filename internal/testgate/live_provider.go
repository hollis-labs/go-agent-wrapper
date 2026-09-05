// Package testgate centralizes opt-in gates for tests that launch installed
// third-party provider processes. It is internal so it cannot become part of
// go-agent-wrapper's public host API.
package testgate

import "os"

// LiveProviderEnvironment is the single opt-in for tests that launch real,
// installed provider CLIs or bridge packages. The exact value "1" enables
// those tests; every other value keeps the deterministic suite isolated.
const LiveProviderEnvironment = "GO_AGENT_WRAPPER_LIVE_PROVIDER_TESTS"

type testSkipper interface {
	Helper()
	Skipf(format string, args ...any)
}

// RequireLiveProvider skips unless the caller explicitly opted in to running
// installed-provider integration tests. Binary and credential checks remain
// the responsibility of the provider-specific helper that calls this gate.
func RequireLiveProvider(t testSkipper) {
	t.Helper()
	if os.Getenv(LiveProviderEnvironment) != "1" {
		t.Skipf("set %s=1 to run installed-provider integration tests", LiveProviderEnvironment)
	}
}
