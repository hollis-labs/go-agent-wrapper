package codexacp

import (
	"errors"
	"reflect"
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestAdapterNameDistinctFromNativeCodex(t *testing.T) {
	// "codex-acp" must not collide with adapters/codex's "codex" — both
	// are independently selectable per TASKS/agent-host-acp/14's
	// "additive, not a replacement" requirement (Nanite repo).
	if got := New().Name(); got != "codex-acp" {
		t.Errorf("Name() = %q, want codex-acp", got)
	}
}

func TestAdapterDescribe(t *testing.T) {
	desc := New().Describe()
	if desc.Provider != "codex" {
		t.Errorf("Provider = %q, want codex (same upstream agent identity as the native adapter)", desc.Provider)
	}
	if desc.Protocol != adapters.ProtocolACP {
		t.Errorf("Protocol = %q, want %q", desc.Protocol, adapters.ProtocolACP)
	}
	if desc.Transport != adapters.TransportStdio {
		t.Errorf("Transport = %q, want %q", desc.Transport, adapters.TransportStdio)
	}
	if desc.Interrupt != adapters.InterruptTurn {
		t.Errorf("Interrupt = %q, want %q (verified both by source and live — see package doc)", desc.Interrupt, adapters.InterruptTurn)
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
	if spec.Binary != "npx" {
		t.Errorf("Binary = %q, want npx", spec.Binary)
	}
	wantArgs := []string{"-y", defaultBridgePackage + "@" + defaultBridgeVersion}
	if !reflect.DeepEqual(spec.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", spec.Args, wantArgs)
	}
	if spec.Cwd != "/tmp/work" {
		t.Errorf("Cwd = %q, want /tmp/work", spec.Cwd)
	}
}

func TestAdapterResolveWithBinaryOverride(t *testing.T) {
	a := New(WithBinary("/opt/homebrew/bin/npx"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Binary != "/opt/homebrew/bin/npx" {
		t.Errorf("Binary = %q, want override", spec.Binary)
	}
}

func TestAdapterResolveWithBridgeVersionOverride(t *testing.T) {
	a := New(WithBridgeVersion("1.0.0"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"-y", defaultBridgePackage + "@1.0.0"}
	if !reflect.DeepEqual(spec.Args, want) {
		t.Errorf("Args = %v, want %v", spec.Args, want)
	}
}

func TestAdapterResolveWithEmptyBridgeVersionTracksLatest(t *testing.T) {
	a := New(WithBridgeVersion(""))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"-y", defaultBridgePackage}
	if !reflect.DeepEqual(spec.Args, want) {
		t.Errorf("Args = %v, want %v (bare package name, unpinned)", spec.Args, want)
	}
}

func TestAdapterResolveWithPackageSpecOverride(t *testing.T) {
	a := New(WithBridgePackageSpec("./local-codex-acp.tgz"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"-y", "./local-codex-acp.tgz"}
	if !reflect.DeepEqual(spec.Args, want) {
		t.Errorf("Args = %v, want %v", spec.Args, want)
	}
}

func TestAdapterResolveWithExtraArgs(t *testing.T) {
	a := New(WithExtraArgs("--foo", "bar"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"-y", defaultBridgePackage + "@" + defaultBridgeVersion, "--foo", "bar"}
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
	if first.Name() != "codex-acp" {
		t.Errorf("CLIAdapter.Name() = %q, want codex-acp", first.Name())
	}
}

func TestCLIAdapterBuildArgsReturnsRealBridgeArgs(t *testing.T) {
	// Mirrors adapters/opencodeacp's own contract: the argv agentkit
	// would actually spawn is the real bridge invocation, not a
	// placeholder — see package doc for why this bridge does not itself
	// drive Client.Launch from here.
	cli := New().CLIAdapter()
	got := cli.BuildArgs("ignored-prompt", "ignored-system-prompt", "ignored-session-id")
	want := []string{"-y", defaultBridgePackage + "@" + defaultBridgeVersion}
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
