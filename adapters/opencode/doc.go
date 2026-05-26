// Package opencode is the wrapper adapter for OpenCode's serve-http
// runtime mode — the long-lived HTTP API + server-sent events shape
// agentkit drives via its [agentsessions] HTTP/SSE runtime.
//
// It implements [adapters.Adapter] by declaring the provider×runtime
// pair (opencode × http-sse) and resolving the exec shape
// (binary + args + env + cwd) for one invocation. It also implements
// [adapters.RuntimeAdapter] by returning the go-providers OpenCode
// serve-http adapter, which the wrapper composes with
// [agentkit/agentsessions.NewFromAdapter] + Caps.ServeHTTP at session
// start time.
//
// Target CLI invocation:
//
//	opencode serve --port 0 --hostname 127.0.0.1
//
// The HTTP session lifecycle (URL discovery, SSE subscription, per-turn
// HTTP POSTs, shutdown) is owned by agentkit's [agentsessions] HTTP/SSE
// runtime; the OpenCode provider's ParseLine is intentionally a
// pass-through in serve-http mode — server diagnostics on stdout are
// not parsed as events.
//
// PTY allocation is rejected — HTTP/SSE cannot run under a PTY parent.
//
// For OpenCode's single-turn "run" mode (each turn spawns a fresh
// subprocess), use the agentkit adapter runtime directly — that path
// doesn't need a wrapper-level adapter declaration.
package opencode
