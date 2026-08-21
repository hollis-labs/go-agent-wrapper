package copilotacp

import (
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
	llmtypes "github.com/hollis-labs/go-llm-types"
	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

func TestAdapter_Name(t *testing.T) {
	if got := New().Name(); got != "copilot" {
		t.Errorf("Name() = %q, want copilot", got)
	}
}

func TestAdapter_Describe_DefaultsToStdio(t *testing.T) {
	a := New()
	desc := a.Describe()
	if desc.Provider != "copilot" {
		t.Errorf("Provider = %q, want copilot", desc.Provider)
	}
	if desc.Protocol != adapters.ProtocolACP {
		t.Errorf("Protocol = %q, want %q", desc.Protocol, adapters.ProtocolACP)
	}
	if desc.Transport != adapters.TransportStdio {
		t.Errorf("Transport = %q, want %q", desc.Transport, adapters.TransportStdio)
	}
	if desc.Interrupt != adapters.InterruptTurn {
		t.Errorf("Interrupt = %q, want %q", desc.Interrupt, adapters.InterruptTurn)
	}
	if len(desc.Channels) != 1 || desc.Channels[0] != runtimeevents.ChannelJSONRPC {
		t.Errorf("Channels = %v, want [jsonrpc]", desc.Channels)
	}
}

func TestAdapter_Describe_TCPTransportIsARealChoice(t *testing.T) {
	// Exercises task 02's Transport field as a real, functioning choice,
	// not a single hardcoded value — the same proof
	// TestDescriptorForTCPTransport pins in acp/client_test.go, here for
	// a real (not fake) ACP-backed Adapter.
	a := New(WithAdapterTransport(adapters.TransportTCP))
	desc := a.Describe()
	if desc.Transport != adapters.TransportTCP {
		t.Errorf("Transport = %q, want %q", desc.Transport, adapters.TransportTCP)
	}
	if desc.Protocol != adapters.ProtocolACP {
		t.Errorf("Protocol = %q, want %q (protocol is unchanged by transport choice)", desc.Protocol, adapters.ProtocolACP)
	}
}

func TestAdapter_Resolve_Stdio(t *testing.T) {
	a := New(WithAdapterBinary("/opt/homebrew/bin/copilot"))
	spec, err := a.Resolve(adapters.ResolveContext{Cwd: "/work"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Binary != "/opt/homebrew/bin/copilot" {
		t.Errorf("Binary = %q", spec.Binary)
	}
	if len(spec.Args) != 1 || spec.Args[0] != "--acp" {
		t.Errorf("Args = %v, want [--acp]", spec.Args)
	}
	if spec.Cwd != "/work" {
		t.Errorf("Cwd = %q, want /work", spec.Cwd)
	}
}

func TestAdapter_Resolve_TCP(t *testing.T) {
	a := New(WithAdapterTransport(adapters.TransportTCP), WithAdapterPort(4123))
	spec, err := a.Resolve(adapters.ResolveContext{Cwd: "/work"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"--acp", "--port", "4123"}
	if len(spec.Args) != len(want) {
		t.Fatalf("Args = %v, want %v", spec.Args, want)
	}
	for i := range want {
		if spec.Args[i] != want[i] {
			t.Errorf("Args[%d] = %q, want %q", i, spec.Args[i], want[i])
		}
	}
}

func TestAdapter_Resolve_RejectsPTY(t *testing.T) {
	a := New()
	if _, err := a.Resolve(adapters.ResolveContext{PTY: true}); err != ErrPTYUnsupported {
		t.Errorf("Resolve with PTY = %v, want ErrPTYUnsupported", err)
	}
}

func TestAdapter_ExtraArgsAppendedAfterTransportFlags(t *testing.T) {
	a := New(WithAdapterExtraArgs("--model", "gpt-5.4"))
	spec, err := a.Resolve(adapters.ResolveContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"--acp", "--model", "gpt-5.4"}
	if len(spec.Args) != len(want) {
		t.Fatalf("Args = %v, want %v", spec.Args, want)
	}
	for i := range want {
		if spec.Args[i] != want[i] {
			t.Errorf("Args[%d] = %q, want %q", i, spec.Args[i], want[i])
		}
	}
}

func TestAdapter_ImplementsRuntimeAdapter(t *testing.T) {
	a := New()
	var _ adapters.Adapter = a
	ra, ok := adapters.Adapter(a).(adapters.RuntimeAdapter)
	if !ok {
		t.Fatal("Adapter does not implement adapters.RuntimeAdapter")
	}
	if ra.CLIAdapter() == nil {
		t.Fatal("CLIAdapter() returned nil")
	}
}

func TestCLIAdapterGlue_Name(t *testing.T) {
	a := New()
	glue := a.CLIAdapter()
	if got := glue.Name(); got != "copilot" {
		t.Errorf("Name() = %q, want copilot", got)
	}
}

func TestCLIAdapterGlue_Detect_ExplicitBinaryWins(t *testing.T) {
	a := New(WithAdapterBinary("/custom/path/copilot"))
	glue := a.CLIAdapter()
	path, ok := glue.Detect()
	if !ok || path != "/custom/path/copilot" {
		t.Errorf("Detect() = (%q, %v), want (/custom/path/copilot, true)", path, ok)
	}
}

func TestCLIAdapterGlue_Detect_EnvVarOverride(t *testing.T) {
	t.Setenv("COPILOT_CLI_PATH", "/env/path/copilot")
	a := New()
	glue := a.CLIAdapter()
	path, ok := glue.Detect()
	if !ok || path != "/env/path/copilot" {
		t.Errorf("Detect() = (%q, %v), want (/env/path/copilot, true)", path, ok)
	}
}

func TestCLIAdapterGlue_BuildArgs_Stdio(t *testing.T) {
	a := New()
	glue := a.CLIAdapter()
	args := glue.BuildArgs("prompt-ignored", "system-ignored", "session-ignored")
	if len(args) != 1 || args[0] != "--acp" {
		t.Errorf("BuildArgs = %v, want [--acp]", args)
	}
}

func TestCLIAdapterGlue_BuildArgs_TCP(t *testing.T) {
	a := New(WithAdapterTransport(adapters.TransportTCP), WithAdapterPort(9001))
	glue := a.CLIAdapter()
	args := glue.BuildArgs("", "", "")
	want := []string{"--acp", "--port", "9001"}
	if len(args) != len(want) {
		t.Fatalf("BuildArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

// TestCLIAdapterGlue_ParseLine_RealSessionUpdate feeds a real,
// full `session/update` JSON-RPC notification frame — captured verbatim
// against the live `copilot --acp` binary — through ParseLine end to
// end (including the outer {"jsonrpc":"2.0","method":"session/update",
// "params":{...}} envelope, not just the inner update payload
// translate_test.go already covers).
func TestCLIAdapterGlue_ParseLine_RealSessionUpdate(t *testing.T) {
	line := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"2913b08c-c27c-427c-be98-aa0f1cd79612","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"PONG"}}}}`)

	a := New()
	glue := a.CLIAdapter()
	evs, err := glue.ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("ParseLine returned %d events, want 1: %+v", len(evs), evs)
	}
	if evs[0].Type != llmtypes.EventDelta || evs[0].Content != "PONG" {
		t.Errorf("unexpected StreamEvent: %+v", evs[0])
	}
}

func TestCLIAdapterGlue_ParseLine_NonUpdateLinesReturnNoEvents(t *testing.T) {
	a := New()
	glue := a.CLIAdapter()

	cases := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`),
		[]byte(`not json at all`),
		[]byte(`{"jsonrpc":"2.0","method":"some/other/notification","params":{}}`),
	}
	for _, line := range cases {
		evs, err := glue.ParseLine(line)
		if err != nil {
			t.Errorf("ParseLine(%s) error = %v, want nil", line, err)
		}
		if len(evs) != 0 {
			t.Errorf("ParseLine(%s) = %v, want no events", line, evs)
		}
	}
}

func TestAdapter_ClientReturnsUnderlyingClient(t *testing.T) {
	a := New()
	if a.Client() == nil {
		t.Fatal("Client() returned nil")
	}
}
