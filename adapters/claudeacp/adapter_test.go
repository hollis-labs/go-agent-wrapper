package claudeacp

import (
	"reflect"
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestAdapterNameDistinctFromNativeClaude(t *testing.T) {
	// "claude-acp" must not collide with adapters/claude's "claude" —
	// both are independently selectable per TASKS/agent-host-acp/13's
	// "additive, not a replacement" requirement (Nanite repo).
	if got := New().Name(); got != "claude-acp" {
		t.Errorf("Name() = %q, want claude-acp", got)
	}
}

func TestAdapterDescribe(t *testing.T) {
	desc := New().Describe()
	if desc.Provider != "claude" {
		t.Errorf("Provider = %q, want claude (same upstream agent identity as the native adapter)", desc.Provider)
	}
	if desc.Protocol != adapters.ProtocolACP {
		t.Errorf("Protocol = %q, want %q", desc.Protocol, adapters.ProtocolACP)
	}
	if desc.Transport != adapters.TransportStdio {
		t.Errorf("Transport = %q, want %q", desc.Transport, adapters.TransportStdio)
	}
	if desc.Interrupt != adapters.InterruptTurn {
		t.Errorf("Interrupt = %q, want %q (verified live and by source — see package doc)", desc.Interrupt, adapters.InterruptTurn)
	}
	if err := desc.Delivery.Validate(); err != nil {
		t.Fatalf("Delivery.Validate: %v", err)
	}
	if !desc.Delivery.Supports(adapters.DeliveryCapabilitySendTurn) {
		t.Fatal("Delivery does not advertise send_turn")
	}
	wantChannels := []runtimeevents.SourceChannel{runtimeevents.ChannelJSONRPC}
	if !reflect.DeepEqual(desc.Channels, wantChannels) {
		t.Errorf("Channels = %v, want %v", desc.Channels, wantChannels)
	}
}

func TestAdapterResolveDefaultsToNpx(t *testing.T) {
	spec, err := New().Resolve(adapters.ResolveContext{Cwd: "/tmp/work"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Binary != "npx" {
		t.Errorf("Binary = %q, want npx", spec.Binary)
	}
	want := []string{"-y", defaultBridgePackage}
	if !reflect.DeepEqual(spec.Args, want) {
		t.Errorf("Args = %v, want %v", spec.Args, want)
	}
	if spec.Cwd != "/tmp/work" {
		t.Errorf("Cwd = %q, want /tmp/work", spec.Cwd)
	}
}

func TestAdapterResolveWithDirectBinaryOverride(t *testing.T) {
	a := New(WithDirectBinary("/opt/bin/claude-agent-acp"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Binary != "/opt/bin/claude-agent-acp" {
		t.Errorf("Binary = %q, want override", spec.Binary)
	}
	if len(spec.Args) != 0 {
		t.Errorf("Args = %v, want empty (no npx/-y/package args when bypassing npx)", spec.Args)
	}
}

func TestAdapterResolveWithExtraArgs(t *testing.T) {
	a := New(WithExtraArgs("--cli"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"-y", defaultBridgePackage, "--cli"}
	if !reflect.DeepEqual(spec.Args, want) {
		t.Errorf("Args = %v, want %v", spec.Args, want)
	}
}

func TestAdapterResolveRejectsPTY(t *testing.T) {
	_, err := New().Resolve(adapters.ResolveContext{PTY: true})
	if err != ErrPTYUnsupported {
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
	if first.Name() != "claude-acp" {
		t.Errorf("CLIAdapter.Name() = %q, want claude-acp", first.Name())
	}
}

func TestCLIAdapterBuildArgsReturnsRealBridgeArgs(t *testing.T) {
	cli := New().CLIAdapter()
	got := cli.BuildArgs("ignored-prompt", "ignored-system-prompt", "ignored-session-id")
	want := []string{"-y", defaultBridgePackage}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildArgs = %v, want %v", got, want)
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
