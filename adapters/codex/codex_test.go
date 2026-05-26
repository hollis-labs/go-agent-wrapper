package codex

import (
	"errors"
	"reflect"
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestNameStable(t *testing.T) {
	// "codex" is the matrix key the wrapper uses to look up the
	// go-providers adapter at session start time. Changing it would
	// break that lookup silently.
	if got := New().Name(); got != "codex" {
		t.Errorf("Name() = %q, want codex", got)
	}
}

func TestDescribe(t *testing.T) {
	desc := New().Describe()
	if desc.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", desc.Provider)
	}
	if desc.Runtime != "jsonrpc-stdio" {
		t.Errorf("Runtime = %q, want jsonrpc-stdio", desc.Runtime)
	}
	wantChannels := []runtimeevents.SourceChannel{runtimeevents.ChannelJSONRPC}
	if !reflect.DeepEqual(desc.Channels, wantChannels) {
		t.Errorf("Channels = %v, want %v", desc.Channels, wantChannels)
	}
}

func TestResolveDefaultBinary(t *testing.T) {
	spec, err := New().Resolve(adapters.ResolveContext{Cwd: "/tmp/work"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Binary != "codex" {
		t.Errorf("Binary = %q, want codex (default — resolved via PATH at exec time)", spec.Binary)
	}
	wantArgs := []string{"app-server"}
	if !reflect.DeepEqual(spec.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", spec.Args, wantArgs)
	}
	if spec.Cwd != "/tmp/work" {
		t.Errorf("Cwd = %q, want /tmp/work", spec.Cwd)
	}
}

func TestResolveWithBinaryOverride(t *testing.T) {
	a := New(WithBinary("/opt/homebrew/bin/codex"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Binary != "/opt/homebrew/bin/codex" {
		t.Errorf("Binary = %q, want /opt/homebrew/bin/codex", spec.Binary)
	}
}

func TestResolveWithExtraArgsAppendsAfterMode(t *testing.T) {
	a := New(WithExtraArgs("--listen", "tcp://127.0.0.1:7777"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Extra args must come AFTER the app-server mode flag so they
	// can't change which subcommand codex runs.
	want := []string{"app-server", "--listen", "tcp://127.0.0.1:7777"}
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
	env := []string{"CODEX_CLI_PATH=/custom/path", "FOO=bar"}
	spec, err := New().Resolve(adapters.ResolveContext{Env: env})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !reflect.DeepEqual(spec.Env, env) {
		t.Errorf("Env = %v, want %v", spec.Env, env)
	}
}

func TestSatisfiesAdapterInterface(t *testing.T) {
	// Compile-time assertion the concrete type satisfies the
	// wrapper.Adapter contract.
	var _ adapters.Adapter = New()
}

func TestSatisfiesRuntimeAdapterInterface(t *testing.T) {
	// Compile-time assertion the concrete type satisfies the
	// optional RuntimeAdapter capability — the wrapper needs this
	// to drive the agentkit/agentsessions JSON-RPC runtime.
	var _ adapters.RuntimeAdapter = New()
}

func TestCLIAdapterReturnsFreshInstance(t *testing.T) {
	a := New()
	first := a.CLIAdapter()
	second := a.CLIAdapter()
	if first == nil || second == nil {
		t.Fatal("CLIAdapter returned nil")
	}
	// Distinct instances so per-session state doesn't leak.
	if first == second {
		t.Errorf("CLIAdapter returned the same instance on two calls; mutating one would affect the other")
	}
	if first.Name() != "codex" {
		t.Errorf("CLIAdapter.Name() = %q, want codex", first.Name())
	}
}
