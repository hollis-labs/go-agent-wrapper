package opencodeacp

import (
	"errors"
	"reflect"
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestAdapterNameDistinctFromNativeOpenCode(t *testing.T) {
	// "opencode-acp" must not collide with adapters/opencode's "opencode"
	// — both are independently selectable per TASKS/agent-host-acp/09's
	// "additive, not a replacement" requirement (Nanite repo).
	if got := New().Name(); got != "opencode-acp" {
		t.Errorf("Name() = %q, want opencode-acp", got)
	}
}

func TestAdapterDescribe(t *testing.T) {
	desc := New().Describe()
	if desc.Provider != "opencode" {
		t.Errorf("Provider = %q, want opencode (same upstream agent identity as the native adapter)", desc.Provider)
	}
	if desc.Protocol != adapters.ProtocolACP {
		t.Errorf("Protocol = %q, want %q", desc.Protocol, adapters.ProtocolACP)
	}
	if desc.Transport != adapters.TransportStdio {
		t.Errorf("Transport = %q, want %q", desc.Transport, adapters.TransportStdio)
	}
	if desc.Interrupt != adapters.InterruptTurn {
		t.Errorf("Interrupt = %q, want %q (verified live — see package doc)", desc.Interrupt, adapters.InterruptTurn)
	}
	wantChannels := []runtimeevents.SourceChannel{runtimeevents.ChannelJSONRPC}
	if !reflect.DeepEqual(desc.Channels, wantChannels) {
		t.Errorf("Channels = %v, want %v", desc.Channels, wantChannels)
	}
}

func TestAdapterResolveDefaultBinary(t *testing.T) {
	spec, err := New().Resolve(adapters.ResolveContext{Cwd: "/tmp/work"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Binary != "opencode" {
		t.Errorf("Binary = %q, want opencode", spec.Binary)
	}
	if !reflect.DeepEqual(spec.Args, []string{"acp"}) {
		t.Errorf("Args = %v, want [acp]", spec.Args)
	}
	if spec.Cwd != "/tmp/work" {
		t.Errorf("Cwd = %q, want /tmp/work", spec.Cwd)
	}
}

func TestAdapterResolveWithBinaryOverride(t *testing.T) {
	a := New(WithBinary("/opt/homebrew/bin/opencode"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Binary != "/opt/homebrew/bin/opencode" {
		t.Errorf("Binary = %q, want override", spec.Binary)
	}
}

func TestAdapterResolveWithExtraArgs(t *testing.T) {
	a := New(WithExtraArgs("--log-level", "DEBUG"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"acp", "--log-level", "DEBUG"}
	if !reflect.DeepEqual(spec.Args, want) {
		t.Errorf("Args = %v, want %v", spec.Args, want)
	}
}

func TestAdapterResolveRejectsPTY(t *testing.T) {
	_, err := New().Resolve(adapters.ResolveContext{PTY: true})
	if !errors.Is(err, ErrPTYUnsupported) {
		t.Fatalf("Resolve(PTY=true): err=%v, want ErrPTYUnsupported", err)
	}
}

func TestAdapterSatisfiesInterfaces(t *testing.T) {
	var _ adapters.Adapter = New()
	var _ adapters.RuntimeAdapter = New()
}

func TestAdapterCLIAdapterReturnsFreshInstance(t *testing.T) {
	a := New()
	first := a.CLIAdapter()
	second := a.CLIAdapter()
	if first == nil || second == nil {
		t.Fatal("CLIAdapter returned nil")
	}
	if first == second {
		t.Error("CLIAdapter returned the same instance on two calls")
	}
	if first.Name() != "opencode-acp" {
		t.Errorf("CLIAdapter.Name() = %q, want opencode-acp", first.Name())
	}
}

func TestCLIAdapterBuildArgsReturnsRealACPSubcommand(t *testing.T) {
	// Mirrors adapters/codex's own app-server BuildArgs contract: the
	// argv agentkit would actually spawn is the real `acp` subcommand,
	// not a placeholder — see package doc for why this bridge does not
	// itself drive Client.Launch from here.
	cli := New().CLIAdapter()
	got := cli.BuildArgs("ignored-prompt", "ignored-system-prompt", "ignored-session-id")
	if !reflect.DeepEqual(got, []string{"acp"}) {
		t.Errorf("BuildArgs = %v, want [acp]", got)
	}
}

func TestCLIAdapterParseLineIsPassThrough(t *testing.T) {
	cli := New().CLIAdapter()
	evs, err := cli.ParseLine([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{}}`))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if evs != nil {
		t.Errorf("ParseLine = %v, want nil (deliberate pass-through — see package doc)", evs)
	}
}
