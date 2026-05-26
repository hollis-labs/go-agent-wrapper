// Package codex is the wrapper adapter for Codex's app-server runtime
// mode — the long-lived JSON-RPC 2.0 over stdin/stdout shape that
// powers OpenAI's VS Code extension.
//
// It implements [adapters.Adapter] by declaring the provider×runtime
// pair (codex × jsonrpc-stdio) and resolving the exec shape
// (binary + args + env + cwd) for one invocation. It also implements
// [adapters.RuntimeAdapter] by returning the go-providers Codex
// app-server adapter, which the wrapper composes with
// [agentkit/agentsessions.NewFromAdapter] + Caps.JsonRpcStdio at
// session start time.
//
// Target CLI invocation:
//
//	codex app-server
//
// The JSON-RPC framing (Content-Length headers, request/response
// correlation, notification dispatch, server-initiated requests) is
// owned by agentkit's [agentsessions] JSON-RPC runtime; the Codex
// provider's ParseLine is intentionally a pass-through in app-server
// mode — do not "fix" that by adding a JSON-RPC parser here.
//
// PTY allocation is rejected — JSON-RPC stdio cannot run under a PTY
// parent.
package codex
