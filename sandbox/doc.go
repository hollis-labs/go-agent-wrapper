// Package sandbox defines the wrapper's pre-exec sandbox application
// contract. The wrapper composes
// github.com/hollis-labs/go-sandbox (v0.2.1+) profiles into the launch
// sequence; this package owns only the wrapper-facing surface so apps
// don't have to depend on the lower-level lib directly when they only
// need the profile-application boundary.
package sandbox
