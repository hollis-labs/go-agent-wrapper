# Shared materialization conformance matrix

Status: M16 evidence for Torque task `CW-20260906-0019`.

This matrix is library-side evidence only. It uses synthetic consumer shapes and shared Go APIs; no Cairn, Nanite, Torque, Tether, Tachyon or production app source is modified by these checks.

## Runnable synthetic consumers

`go run ./examples/shared-conformance -scenario all -root <tmp>` creates four app-shaped boot roots and prints JSON evidence. `go test ./examples/shared-conformance -count=1` runs the same scenarios and asserts representative files, modes, ownership behavior and runtime requirements.

| Consumer shape | Shared API path | Synthetic behavior proved | Example/test evidence |
| --- | --- | --- | --- |
| Cairn | `agentcontext.AuthoredRecipe` + `artifact.FilesystemSource` + `materialize.MergeDocument` + `agentlaunch.ResolvePreparation` | Authored base/part composition, copied filesystem tree, fresh boot dir, executable mode preservation, managed JSON and TOML install entries | `examples/shared-conformance` scenario `cairn`; `TestSharedConformanceScenariosRun`; `TestSharedConformanceProgramRuns` |
| Nanite | `artifact.ImmutableRef`/`MemoryStore` + `agentlaunch.MaterializeArtifacts` create/refresh | Immutable blob object resolution with digest, native skill-package tree, empty directory preservation, owned refresh/removal of omitted blob | `examples/shared-conformance` scenario `nanite`; materializer ownership tests in `agentkit/materialize` |
| Torque | `artifact.GeneratedSource`-style tree values + copied filesystem tree + `agentlaunch.ResolvePreparation` | Generated task tree, copied task assets, explicit task-scoped loopback `RuntimeEffect`, required local access roots preserved | `examples/shared-conformance` scenario `torque`; prepared-access tests in `agentkit/agentlaunch` and `agentkit/agentsessions` |
| Tether | `artifact.FilesystemSource` + `plant.SharedPlanter` + wrapper legacy provider-setting adapters | Multiple task bundles, copied trees, provider overlays for Claude/Codex, wrapper planter delegation to shared materializer | `examples/shared-conformance` scenario `tether`; `go-agent-wrapper/plant` shared-planter tests |

Every scenario combines direct files with at least one arbitrary tree shape and uses fake in-memory or temp-dir inputs.

## Architecture guarantee to executable evidence

| Guarantee | Evidence |
| --- | --- |
| Package ownership and acyclic graph | `agentkit/docs/shared-materialization-contracts.md`; module imports compile under `go test ./...`; go-providers has no agentkit import. |
| Direct artifacts, resolved composition and authored recipe are independent entry points | `agentkit/agentlaunch/shared_contract_examples_test.go`; `agentkit/agentlaunch/shared_preparation_test.go`; `examples/shared-conformance` Cairn/Torque scenarios. |
| Files, directories, executable modes, binary bytes and empty directories survive resolution and materialization | `agentkit/artifact/resolver_test.go`; `agentkit/materialize/engine_test.go`; `examples/shared-conformance` Cairn/Nanite tests. |
| Immutable content is verified before install | `agentkit/artifact/resolver_test.go` (`TestResolverRejectsImmutableMissingAndChanged`); `examples/shared-conformance` Nanite scenario uses a digest-checked immutable blob. |
| Collision, traversal, limit and symlink safety are rejected before write | `agentkit/artifact/resolver_test.go`; `agentkit/materialize/engine_test.go`; `agentkit/agentlaunch/providerplant/plant_test.go`. |
| Create/reconcile/refresh preserve ownership, report partial failure and protect unowned content | `agentkit/materialize/reconcile_test.go`; `go-agent-wrapper/plant/plant_test.go`; Nanite refresh scenario. |
| Provider projection is pure and version/capability aware | `go-providers/provider/projection_test.go`; `go-providers/provider/preparation_test.go`; `agentkit/agentlaunch/provider_projection_bridge.go` tests through M12 coverage. |
| Prepared execution preserves exact argv/env/cwd and disables double planting | `agentkit/agentlaunch/sessionshim/shim_test.go`; `agentkit/agentsessions/prepared_sandbox_test.go`; `go-agent-wrapper/wrapper/wrapper_integration_test.go`. |
| Empty environment means empty, not inherited | `agentkit/agentsessions/prepared_sandbox_test.go`; `go-agent-wrapper/wrapper/wrapper_integration_test.go`; `go-agent-wrapper/adapters/codexacp/client_test.go` (`TestClientPreparedCommandEnvIsExact`). |
| Local required confinement applies before start or returns unsupported/failure | `go-sandbox/sandbox/resolved_policy_integration_test.go`; `go-runner/runner/sandbox_policy_test.go`; `agentkit/agentsessions/prepared_sandbox_test.go`; wrapper native/ACP sandbox tests. |
| Remote/pre-existing ACP endpoints do not claim local confinement | `go-agent-wrapper/acp/sandbox.go`; `go-agent-wrapper/adapters/copilotacp/client_test.go` (`TestClientTCP_DialOnlyRejectsRequiredSandboxBeforeDial`); `go-agent-wrapper/wrapper/acp_subprocess_test.go` (`TestACPRemoteDialOnlyRejectsRequiredSandboxAndReportsDisabled`). |
| Runtime events distinguish configured/started/disabled/unsupported/failed enforcement | `go-runner/runner/sandbox_policy_test.go`; `agentkit/agentsessions` sandbox outcome tests; `go-agent-wrapper/wrapper` sandbox event tests. |

## Provider/runtime capability evidence

| Provider/runtime family | Contract source | Concrete verification |
| --- | --- | --- |
| Claude native paths (`print`, `bare`, `pty`, `streaming-stdio`) | `go-providers/provider.ProviderCapabilityMatrix()` tested version `2.1.263` | `go-providers/provider/projection_test.go`; `go-providers/provider/bootdir_*_test.go`; wrapper native prepared/sandbox tests. |
| Codex native paths (`exec`, `app-server`) | Capability matrix tested version `0.153.4`; Codex ACP bridge pinned to `@agentclientprotocol/codex-acp@1.6.2` | `go-providers/provider/projection_test.go`; `go-agent-wrapper/adapters/codexacp/client_test.go`; wrapper ACP prepared exact launch tests. |
| OpenCode native paths (`run`, `serve-http`) | Capability matrix tested version `1.15.6` | `go-providers/provider/projection_test.go`; `go-agent-wrapper/adapters/opencodeacp/client_test.go`; wrapper ACP/local stdio sandbox fixture and TCP capability test. |
| Runner subprocess-per-turn and supervised restart | `go-runner` sandbox policy contract | `go-runner/runner/sandbox_policy_test.go` including restart and real backend denial. |
| Agentkit persistent native sessions | `agentsessions.StartOptions.PreparedExecution` and `SandboxPolicy` | `agentkit/agentsessions/prepared_sandbox_test.go`; broad `agentkit` test suite excluding local-catalog parity drift. |
| Wrapper native and ACP sessions | `wrapper.Config.PreparedExecution`, `PrepareRequest`, `SandboxPolicy`; `acp.LaunchParams` | `go-agent-wrapper/wrapper/wrapper_integration_test.go`; `go-agent-wrapper/wrapper/acp_subprocess_test.go`; ACP adapter tests. Local TCP is enforced where host-to-child loopback is supported and rejects required Linux bwrap private-network policy before start. |

## Required OS-denial evidence commands

After M17 removed committed local `replace` directives, reproduce these commands with a temporary workspace file outside the repositories:

```sh
cat > /Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/m17-go.work <<'WORK'
go 1.26.6

use (
	/Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/go-providers-m06
	/Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/go-sandbox-m08
	/Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/go-runner-m11
	/Users/chrispian/dev/hollis-labs/libs/agentkit
	/Users/chrispian/dev/hollis-labs/libs/go-agent-wrapper
)
WORK
export GOWORK=/Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/m17-go.work
```

macOS evidence is produced locally on the host:

```sh
cd /Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/go-sandbox-m08 && GOWORK=/Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/m17-go.work go test ./sandbox -run 'TestApplyResolvedPolicyParity_FilesystemAllowlist|TestApplyResolvedDarwin_NetworkDenied|TestApplyResolvedDarwin_SubprocessDenyFailsExplicitly|TestApplyResolvedDarwin_LoopbackForwardFailsExplicitly' -count=1 -v
cd /Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/go-runner-m11 && GOWORK=/Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/m17-go.work go test ./runner -run 'TestRunResolvedSandboxRealBackendDeniesAccess|TestSandboxPolicyAppliedBeforeEveryStartAndRestart|TestSandboxPolicySetupFailurePreventsStart|TestSandboxPolicyStartFailureReportsConfiguredNotLaunched|TestRunResolvedSandboxRealBackendDeniesAccessOnRestart' -count=1 -v
cd /Users/chrispian/dev/hollis-labs/libs/agentkit && GOWORK=/Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/m17-go.work go test ./agentsessions -run 'Test.*Prepared.*Sandbox|Test.*Sandbox.*Restart|Test.*Denied|Test.*Unsupported' -count=1 -v
cd /Users/chrispian/dev/hollis-labs/libs/go-agent-wrapper && GOWORK=/Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/m17-go.work go test ./wrapper -run 'TestRunDirectNativeSandboxFailureEmitsTruthfulEventAndDoesNotStart|TestACPWrapperPreparedLocalStdioSandboxDeniesReadWrite|TestACPWrapperPreparedLocalTCPSandboxCapabilityResult|TestACPRemoteDialOnlyRejectsRequiredSandboxAndReportsDisabled' -count=1 -v
```

Linux/bwrap evidence is produced in the Colima Docker runner used for M10:

```sh
docker run --rm --privileged -v /Users:/Users -w /Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/go-sandbox-m08 golang:1.26-bookworm bash -lc 'export PATH=/usr/local/go/bin:$PATH GOWORK=/Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/m17-go.work; apt-get update >/dev/null && apt-get install -y bubblewrap >/dev/null && go test ./sandbox -run "TestApplyResolvedPolicyParity_FilesystemAllowlist|TestApplyResolvedLinux_NetworkDenied|TestApplyResolvedLinux_LoopbackAndForwarding" -count=1 -v'
docker run --rm --privileged -v /Users:/Users -w /Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/go-runner-m11 golang:1.26-bookworm bash -lc 'export PATH=/usr/local/go/bin:$PATH GOWORK=/Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/m17-go.work; apt-get update >/dev/null && apt-get install -y bubblewrap >/dev/null && go test ./runner -run "TestRunResolvedSandboxRealBackendDeniesAccess|TestSandboxPolicyAppliedBeforeEveryStartAndRestart|TestSandboxPolicySetupFailurePreventsStart|TestRunResolvedSandboxRealBackendDeniesAccessOnRestart" -count=1 -v'
docker run --rm --privileged -v /Users:/Users -w /Users/chrispian/dev/hollis-labs/libs/go-agent-wrapper golang:1.26-bookworm bash -lc 'export PATH=/usr/local/go/bin:$PATH GOWORK=/Users/chrispian/dev/agent-os/workspaces/shared-materialization-20260906/m17-go.work; apt-get update >/dev/null && apt-get install -y bubblewrap >/dev/null && go test ./examples/shared-conformance ./wrapper -run "TestSharedConformance|TestSharedMaterializationAcceptanceMatrixHasEvidence|TestACPWrapperPreparedLocalStdioSandboxDeniesReadWrite|TestACPWrapperPreparedLocalTCPSandboxCapabilityResult|TestRunDirectNativeSandboxFailureEmitsTruthfulEventAndDoesNotStart" -count=1 -v'
```

Skipped tests do not count as passing evidence for M16. If a required command cannot run, the M16 gate remains open with the exact blocker recorded.
