package opencode

import (
	"errors"
	"reflect"
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestNameStable(t *testing.T) {
	// "opencode" is the matrix key the wrapper uses to look up the
	// go-providers adapter at session start time. Changing it would
	// break that lookup silently.
	if got := New().Name(); got != "opencode" {
		t.Errorf("Name() = %q, want opencode", got)
	}
}

func TestDescribe(t *testing.T) {
	desc := New().Describe()
	if desc.Provider != "opencode" {
		t.Errorf("Provider = %q, want opencode", desc.Provider)
	}
	if desc.Runtime != "http-sse" {
		t.Errorf("Runtime = %q, want http-sse", desc.Runtime)
	}
	wantChannels := []runtimeevents.SourceChannel{runtimeevents.ChannelOpenCodePlugin}
	if !reflect.DeepEqual(desc.Channels, wantChannels) {
		t.Errorf("Channels = %v, want %v", desc.Channels, wantChannels)
	}
}

func TestResolveDefaultBinary(t *testing.T) {
	spec, err := New().Resolve(adapters.ResolveContext{Cwd: "/tmp/work"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Binary != "opencode" {
		t.Errorf("Binary = %q, want opencode (default — resolved via PATH at exec time)", spec.Binary)
	}
	wantArgs := []string{"serve", "--port", "0", "--hostname", "127.0.0.1"}
	if !reflect.DeepEqual(spec.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", spec.Args, wantArgs)
	}
	if spec.Cwd != "/tmp/work" {
		t.Errorf("Cwd = %q, want /tmp/work", spec.Cwd)
	}
}

func TestResolveWithBinaryOverride(t *testing.T) {
	a := New(WithBinary("/opt/homebrew/bin/opencode"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Binary != "/opt/homebrew/bin/opencode" {
		t.Errorf("Binary = %q, want /opt/homebrew/bin/opencode", spec.Binary)
	}
}

func TestResolveWithExtraArgsAppendsAfterDefaults(t *testing.T) {
	a := New(WithExtraArgs("--port", "8080", "--log-level", "debug"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Extra args appear AFTER the defaults — callers overriding
	// --port effectively get the LAST --port flag winning per
	// typical CLI semantics.
	want := []string{"serve", "--port", "0", "--hostname", "127.0.0.1",
		"--port", "8080", "--log-level", "debug"}
	if !reflect.DeepEqual(spec.Args, want) {
		t.Errorf("Args = %v\n  want %v", spec.Args, want)
	}
}

func TestResolveRejectsPTY(t *testing.T) {
	_, err := New().Resolve(adapters.ResolveContext{PTY: true})
	if !errors.Is(err, ErrPTYUnsupported) {
		t.Fatalf("Resolve(PTY=true): err=%v, want ErrPTYUnsupported", err)
	}
}

func TestResolvePropagatesEnv(t *testing.T) {
	env := []string{"OPENCODE_CLI_PATH=/custom/path", "FOO=bar"}
	spec, err := New().Resolve(adapters.ResolveContext{Env: env})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !reflect.DeepEqual(spec.Env, env) {
		t.Errorf("Env = %v, want %v", spec.Env, env)
	}
}

func TestSatisfiesAdapterInterface(t *testing.T) {
	var _ adapters.Adapter = New()
}

func TestSatisfiesRuntimeAdapterInterface(t *testing.T) {
	var _ adapters.RuntimeAdapter = New()
}

func TestCLIAdapterReturnsFreshInstance(t *testing.T) {
	a := New()
	first := a.CLIAdapter()
	second := a.CLIAdapter()
	if first == nil || second == nil {
		t.Fatal("CLIAdapter returned nil")
	}
	if first == second {
		t.Errorf("CLIAdapter returned the same instance on two calls; mutating one would affect the other")
	}
	if first.Name() != "opencode" {
		t.Errorf("CLIAdapter.Name() = %q, want opencode", first.Name())
	}
}
