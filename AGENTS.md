# go-agent-wrapper

The shared harness for launching, observing and governing CLI-agent
subprocesses. It composes `agentkit` plus `go-runner`, `go-providers` and
`go-sandbox` into one standardized execution boundary, and translates provider
output into `runtimeevents`. It deliberately prescribes no prompt design, no
workflow logic, no turn semantics and no agent cognition — it runs the child
and reports what happened.

## Start Here

- `README.md` is the current status and the full event vocabulary.
- `ROADMAP.md` records what is deferred and why.
- `wrapper/` owns `Run`: runtime dispatch, session drive, event translation.
- `adapters/` holds native and ACP adapters; `adapters/selection.go` chooses one
  from provider, runtime kind and launch mode.
- `acp/` owns ACP v1 negotiation, create-or-resume, cancellation and close.
- `policy/` defines the advisory observer surface.
- `filters/` adapts `go-harness-filters` rules onto agent text and tool
  envelopes.
- `plant/` and `sandbox/` own pre-spawn materialization and confinement.
- `internal/testgate/` owns the live-provider opt-in gate.

## Commands

```bash
gofmt -l .
go vet ./...
go test -race -count=1 ./...
govulncheck ./...
```

CI runs all four. Live provider tests are skipped unless
`GO_AGENT_WRAPPER_LIVE_PROVIDER_TESTS=1`, so a default run exercises no real
CLI.

This module tracks published `agentkit` and `go-sandbox` releases through
ordinary `go.mod` pins. Never add a local `replace` directive to pick up
unreleased work — `agentkit/docs/shared-materialization-handoff.md` forbids it
and prescribes a temporary `go.work` outside the repositories instead.

## Boundaries

Policy here is advisory and must stay that way. A `PolicyObserver`
recommendation emits a correlated `policy.nudge` / `rewrite` / `block` /
`approval_requested` event; it never changes, delays or prevents child
execution. Wiring an observer into the execution path would turn an
observability surface into an enforcement one, which is a different product.

A failed advertised ACP resume is returned as an error, never downgraded into
silently starting a fresh session — a caller that asked to resume and got a new
session would lose history without being told.

Every live-provider test must go through `internal/testgate.RequireLiveProvider`,
and `TestEveryInstalledProviderTestUsesTheSharedGate` fails the build if one
does not. The gate accepts the exact value `1` and nothing else, so a stray
truthy string cannot switch real CLIs on in CI.

`Config.Environment` materializes an explicit child environment for every spawn
— inherit/merge/replace mode, allowlist, ordered overrides, final unset list.
That explicitness is the point: secret egress and precedence stay inspectable
without wrapping the child in `env -i`.
