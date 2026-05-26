package claude

import (
	"errors"
	"reflect"
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestNameStable(t *testing.T) {
	// "claude" is the matrix key the wrapper uses to look up the
	// go-providers adapter at session start time. Changing it would
	// break that lookup silently.
	if got := New().Name(); got != "claude" {
		t.Errorf("Name() = %q, want claude", got)
	}
}

func TestDescribe(t *testing.T) {
	desc := New().Describe()
	if desc.Provider != "claude" {
		t.Errorf("Provider = %q, want claude", desc.Provider)
	}
	if desc.Runtime != "streaming-stdio" {
		t.Errorf("Runtime = %q, want streaming-stdio", desc.Runtime)
	}
	wantChannels := []runtimeevents.SourceChannel{runtimeevents.ChannelClaudeStreamJSON}
	if !reflect.DeepEqual(desc.Channels, wantChannels) {
		t.Errorf("Channels = %v, want %v", desc.Channels, wantChannels)
	}
}

func TestResolveDefaultBinary(t *testing.T) {
	spec, err := New().Resolve(adapters.ResolveContext{Cwd: "/tmp/work"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Binary != "claude" {
		t.Errorf("Binary = %q, want claude (default — resolved via PATH at exec time)", spec.Binary)
	}
	wantArgs := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	if !reflect.DeepEqual(spec.Args, wantArgs) {
		t.Errorf("Args = %v\n  want %v", spec.Args, wantArgs)
	}
	if spec.Cwd != "/tmp/work" {
		t.Errorf("Cwd = %q, want /tmp/work", spec.Cwd)
	}
}

func TestResolveWithBinaryOverride(t *testing.T) {
	a := New(WithBinary("/opt/homebrew/bin/claude"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Binary != "/opt/homebrew/bin/claude" {
		t.Errorf("Binary = %q, want /opt/homebrew/bin/claude", spec.Binary)
	}
}

func TestResolveWithExtraArgsAppends(t *testing.T) {
	a := New(WithExtraArgs("--model", "claude-opus-4-7", "--max-tokens", "8192"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Extra args must come AFTER the streaming-stdio core flags so
	// they can't override transport configuration.
	want := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--model", "claude-opus-4-7",
		"--max-tokens", "8192",
	}
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
	env := []string{"FOO=bar", "BAZ=qux"}
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
	// to drive the agentkit/agentsessions runtime.
	var _ adapters.RuntimeAdapter = New()
}

func TestCLIAdapterReturnsFreshInstance(t *testing.T) {
	a := New()
	first := a.CLIAdapter()
	second := a.CLIAdapter()
	if first == nil || second == nil {
		t.Fatal("CLIAdapter returned nil")
	}
	// Distinct instances so per-session state (resume IDs, bare-mode
	// paths) on one doesn't leak into another.
	if first == second {
		t.Errorf("CLIAdapter returned the same instance on two calls; mutating one would affect the other")
	}
	if first.Name() != "claude" {
		t.Errorf("CLIAdapter.Name() = %q, want claude", first.Name())
	}
}
