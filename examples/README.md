# go-agent-wrapper examples

Runnable examples live here, one `main` package per subdirectory.

## Claude native streaming

[`claude-stream`](./claude-stream/) selects the native Claude
streaming-stdio adapter, auto-delivers one correctly framed first turn, writes
normalized runtime events as JSONL, and cancels the session on Ctrl-C.

Prerequisites: Go 1.26.6 and an authenticated `claude` executable on `PATH`.
An explicit executable must be an absolute path.

```sh
go run ./examples/claude-stream \
  -workdir "$PWD" \
  -prompt 'Summarize this repository in three bullets.'

# Or pin the executable explicitly:
go run ./examples/claude-stream -claude /absolute/path/to/claude
```

The example deliberately uses the zero-value child environment, which inherits
the host environment so the installed CLI can find its authentication. A
security-sensitive host should instead configure the allowlisted
`wrapper.ChildEnvironment` contract documented in the repository README.

## Shared materialization conformance

[`shared-conformance`](./shared-conformance/) builds app-shaped boot roots
(Cairn, Nanite, Torque and Tether scenarios) through agentkit's preparation
and materialization APIs and `plant.SharedPlanter`, and prints JSON evidence.
It needs no provider CLI:

```sh
go run ./examples/shared-conformance -scenario all -root "$(mktemp -d)"
```

[`docs/shared-materialization-conformance.md`](../docs/shared-materialization-conformance.md)
maps each scenario to the guarantee it evidences.
