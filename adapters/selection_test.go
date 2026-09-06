package adapters

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/hollis-labs/go-providers/provider"
)

func TestSelectRuntimeMatrix(t *testing.T) {
	tests := []struct {
		name          string
		selection     Selection
		wantProtocol  Protocol
		wantTransport Transport
		wantArgs      []string
		wantDevFlag   bool
	}{
		{
			name:         "claude default is streaming",
			selection:    Selection{Provider: ProviderClaude, RuntimeKind: RuntimeKindCLI},
			wantProtocol: ProtocolClaudeStreamJSON, wantTransport: TransportStdio,
			wantArgs: []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"},
		},
		{
			name:         "claude developer streaming",
			selection:    Selection{Provider: ProviderClaude, LaunchMode: LaunchStreamingStdio, DeveloperMode: true},
			wantProtocol: ProtocolClaudeStreamJSON, wantTransport: TransportStdio,
			wantDevFlag: true,
		},
		{
			name:      "claude subprocess per turn",
			selection: Selection{Provider: ProviderClaude, LaunchMode: LaunchSubprocessPerTurn},
			wantArgs:  []string{"-p", "prompt", "--output-format", "stream-json", "--verbose"},
		},
		{
			name:         "codex default preserves app server",
			selection:    Selection{Provider: ProviderCodex},
			wantProtocol: ProtocolCodexAppServer, wantTransport: TransportStdio,
			wantArgs: []string{"app-server"},
		},
		{
			name:      "codex subprocess per turn",
			selection: Selection{Provider: ProviderCodex, LaunchMode: LaunchSubprocessPerTurn},
			wantArgs:  []string{"exec", "prompt", "--json", "--skip-git-repo-check"},
		},
		{
			name:         "opencode default preserves serve http",
			selection:    Selection{Provider: ProviderOpenCode},
			wantProtocol: ProtocolOpenCodeNative, wantTransport: TransportHTTPSSE,
			wantArgs: []string{"serve", "--port", "0", "--hostname", "127.0.0.1"},
		},
		{
			name:      "opencode subprocess per turn",
			selection: Selection{Provider: ProviderOpenCode, LaunchMode: LaunchSubprocessPerTurn},
			wantArgs:  []string{"run", "--agent", "", "prompt"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := Select(tc.selection)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			desc := adapter.Describe()
			if desc.Protocol != tc.wantProtocol || desc.Transport != tc.wantTransport {
				t.Fatalf("descriptor protocol/transport = %q/%q, want %q/%q", desc.Protocol, desc.Transport, tc.wantProtocol, tc.wantTransport)
			}
			args := adapter.CLIAdapter().BuildArgs("prompt", "", "")
			if tc.wantArgs != nil && !reflect.DeepEqual(args, tc.wantArgs) {
				t.Fatalf("args = %#v, want %#v", args, tc.wantArgs)
			}
			if got := slices.Contains(args, "--dangerously-skip-permissions"); got != tc.wantDevFlag {
				t.Fatalf("developer flag present = %v, want %v; args=%#v", got, tc.wantDevFlag, args)
			}
			if err := desc.Delivery.Validate(); err != nil {
				t.Fatalf("Delivery.Validate: %v", err)
			}
			if !desc.Delivery.Supports(DeliveryCapabilitySendTurn) {
				t.Fatal("Delivery does not advertise send_turn")
			}
		})
	}
}

func TestSelectBinaryAndExtraArgsStayStructured(t *testing.T) {
	binary := "/tmp/path with spaces/codex;echo-not-a-command"
	extra := []string{"--label", "a value with spaces", "$(touch /tmp/never)", "'quoted' && false"}
	adapter, err := Select(Selection{
		Provider: ProviderCodex, LaunchMode: LaunchSubprocessPerTurn,
		Binary: binary, ExtraArgs: extra,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	cli := adapter.CLIAdapter()
	if got, ok := cli.Detect(); !ok || got != binary {
		t.Fatalf("Detect = (%q, %v), want (%q, true)", got, ok, binary)
	}
	args := cli.BuildArgs("prompt with spaces; still one arg", "", "")
	want := []string{
		"exec", "prompt with spaces; still one arg", "--json", "--skip-git-repo-check",
		"--label", "a value with spaces", "$(touch /tmp/never)", "'quoted' && false",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}

	// Returned adapters and their args are independent copies.
	args[0] = "mutated"
	if got := adapter.CLIAdapter().BuildArgs("prompt with spaces; still one arg", "", ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("fresh CLI adapter args = %#v, want %#v", got, want)
	}
}

func TestSelectRejectsUnsupportedAndAmbiguousSelections(t *testing.T) {
	tests := []struct {
		name string
		cfg  Selection
		want error
	}{
		{name: "unknown provider", cfg: Selection{Provider: "other"}, want: ErrUnsupportedSelection},
		{name: "api runtime", cfg: Selection{Provider: ProviderClaude, RuntimeKind: RuntimeKindAPI}, want: ErrUnsupportedSelection},
		{name: "codex streaming", cfg: Selection{Provider: ProviderCodex, LaunchMode: LaunchStreamingStdio}, want: ErrUnsupportedSelection},
		{name: "opencode app server", cfg: Selection{Provider: ProviderOpenCode, LaunchMode: LaunchAppServer}, want: ErrUnsupportedSelection},
		{name: "codex developer mode", cfg: Selection{Provider: ProviderCodex, DeveloperMode: true}, want: ErrInvalidSelection},
		{name: "relative binary", cfg: Selection{Provider: ProviderCodex, Binary: "./codex"}, want: ErrInvalidSelection},
		{name: "nul arg", cfg: Selection{Provider: ProviderCodex, ExtraArgs: []string{"a\x00b"}}, want: ErrInvalidSelection},
		{name: "custom plus developer", cfg: Selection{Provider: ProviderClaude, DeveloperMode: true, CLIAdapter: provider.NewClaudeAdapterDevStreamingStdio()}, want: ErrInvalidSelection},
		{name: "custom name mismatch", cfg: Selection{Provider: ProviderCodex, CLIAdapter: provider.NewOpencodeAdapter()}, want: ErrInvalidSelection},
		{name: "codex custom shape mismatch", cfg: Selection{Provider: ProviderCodex, LaunchMode: LaunchSubprocessPerTurn, CLIAdapter: provider.NewCodexAdapterAppServer()}, want: ErrInvalidSelection},
		{name: "opencode custom shape mismatch", cfg: Selection{Provider: ProviderOpenCode, LaunchMode: LaunchServeHTTP, CLIAdapter: provider.NewOpencodeAdapter()}, want: ErrInvalidSelection},
		{name: "claude custom shape mismatch", cfg: Selection{Provider: ProviderClaude, LaunchMode: LaunchStreamingStdio, CLIAdapter: provider.NewClaudeAdapter()}, want: ErrInvalidSelection},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Select(tc.cfg)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want errors.Is(_, %v)", err, tc.want)
			}
		})
	}
}

func TestSelectPreservesMatchingConfiguredCLIAdapter(t *testing.T) {
	configured := provider.NewCodexAdapter()
	configured.ApprovalPolicy = "on-request"
	adapter, err := Select(Selection{
		Provider: ProviderCodex, LaunchMode: LaunchSubprocessPerTurn,
		CLIAdapter: configured,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	decorated, ok := adapter.CLIAdapter().(*configuredCLIAdapter)
	if !ok {
		t.Fatalf("CLIAdapter type = %T, want *configuredCLIAdapter", adapter.CLIAdapter())
	}
	if decorated.CLIAdapter != configured {
		t.Fatal("Select replaced the host-configured CLI adapter")
	}
}

func TestSelectedAdapterDescribeClonesDeliveryCapabilities(t *testing.T) {
	adapter, err := Select(Selection{Provider: ProviderClaude})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	desc := adapter.Describe()
	desc.Delivery.Supported[0].Evidence = "mutated"
	if got, _ := adapter.Describe().Delivery.Evidence(DeliveryCapabilitySendTurn); got.Evidence == "mutated" {
		t.Fatal("Describe returned delivery slice sharing adapter backing storage")
	}
}
