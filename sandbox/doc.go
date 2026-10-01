// Package sandbox defines the wrapper's post-start sandbox application
// contract: an [Applier] that wrapper.Wrapper.Run calls once on a native
// runtime, against the child's PID after the session starts, reporting the
// result as a sandbox.applied runtime event. It suits long-lived runtimes
// with a stable PID; a subprocess-per-turn runtime has no live child between
// turns.
//
// Pre-spawn confinement does not go through this package: it is
// github.com/hollis-labs/go-sandbox's resolved policy or profile, set as
// wrapper.Config.SandboxPolicy or SandboxProfile (native only), plus
// wrapper.Config.ProtectedPaths, which the wrapper forwards to agentkit's
// runtime or to the ACP launch. This package owns only the wrapper-facing
// surface so apps don't have to depend on the lower-level lib directly when
// they only need the profile-application boundary.
package sandbox
