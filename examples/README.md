# go-agent-wrapper examples

Runnable examples for the `wrapper` package live here, one `main` package per
subdirectory.

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
