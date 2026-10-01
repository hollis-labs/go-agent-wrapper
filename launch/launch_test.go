package launch

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-providers/registry"

	"github.com/hollis-labs/go-agent-wrapper/acp"
	"github.com/hollis-labs/go-agent-wrapper/adapters"
)

func TestSelectNativeMatrix(t *testing.T) {
	tests := []struct {
		name          string
		selection     Selection
		wantProtocol  adapters.Protocol
		wantTransport adapters.Transport
		wantArgs      []string
		wantDevFlag   bool
	}{
		{
			name:         "claude default is streaming",
			selection:    Selection{Runtime: "claude"},
			wantProtocol: adapters.ProtocolClaudeStreamJSON, wantTransport: adapters.TransportStdio,
			wantArgs: []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"},
		},
		{
			name:         "claude developer streaming",
			selection:    Selection{Runtime: "claude-code", Mode: runtimes.ModeStreamingStdio, DeveloperMode: true},
			wantProtocol: adapters.ProtocolClaudeStreamJSON, wantTransport: adapters.TransportStdio,
			wantDevFlag: true,
		},
		{
			name:      "claude subprocess per turn",
			selection: Selection{Runtime: "claude", Mode: runtimes.ModeSubprocessPerTurn},
			wantArgs:  []string{"-p", "--output-format", "stream-json", "--verbose", "--", "prompt"},
		},
		{
			name:         "codex default is app server (D-74)",
			selection:    Selection{Runtime: "codex"},
			wantProtocol: adapters.ProtocolCodexAppServer, wantTransport: adapters.TransportStdio,
			wantArgs: []string{"app-server"},
		},
		{
			name:      "codex subprocess per turn",
			selection: Selection{Runtime: "codex", Mode: runtimes.ModeSubprocessPerTurn},
			wantArgs:  []string{"exec", "--json", "--skip-git-repo-check", "--", "prompt"},
		},
		{
			name:      "opencode default is subprocess per turn",
			selection: Selection{Runtime: "opencode"},
			wantArgs:  []string{"run", "--format", "json", "--agent", "", "--", "prompt"},
		},
		{
			name:         "opencode http-sse is explicit",
			selection:    Selection{Runtime: "opencode", Mode: runtimes.ModeHTTPSSE},
			wantProtocol: adapters.ProtocolOpenCodeNative, wantTransport: adapters.TransportHTTPSSE,
			wantArgs: []string{"serve", "--port", "0", "--hostname", "127.0.0.1"},
		},
		{
			name:      "antigravity default is subprocess per turn",
			selection: Selection{Runtime: "agy"},
			wantArgs:  []string{"--output-format", "stream-json", "-p=prompt"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := Select(tc.selection)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			native, ok := adapter.(adapters.RuntimeAdapter)
			if !ok {
				t.Fatalf("%T is not a RuntimeAdapter", adapter)
			}
			desc := adapter.Describe()
			if desc.Protocol != tc.wantProtocol || desc.Transport != tc.wantTransport {
				t.Fatalf("descriptor protocol/transport = %q/%q, want %q/%q", desc.Protocol, desc.Transport, tc.wantProtocol, tc.wantTransport)
			}
			args := native.CLIAdapter().BuildArgs("prompt", "", "")
			if tc.wantArgs != nil && !reflect.DeepEqual(args, tc.wantArgs) {
				t.Fatalf("args = %#v, want %#v", args, tc.wantArgs)
			}
			if got := slices.Contains(args, "--dangerously-skip-permissions"); got != tc.wantDevFlag {
				t.Fatalf("developer flag present = %v, want %v; args=%#v", got, tc.wantDevFlag, args)
			}
			if err := desc.Delivery.Validate(); err != nil {
				t.Fatalf("Delivery.Validate: %v", err)
			}
			if !desc.Delivery.Supports(adapters.DeliveryCapabilitySendTurn) {
				t.Fatal("Delivery does not advertise send_turn")
			}
		})
	}
}

// ACP is a mode: Copilot and Pi default to it, Copilot also over TCP, and
// Claude, Codex and OpenCode take it on request.
func TestSelectACP(t *testing.T) {
	tests := []struct {
		selection     Selection
		wantName      string
		wantTransport adapters.Transport
	}{
		{Selection{Runtime: "copilot"}, "copilot", adapters.TransportStdio},
		{Selection{Runtime: "copilot", Mode: runtimes.ModeACPTCP, Port: 4321}, "copilot", adapters.TransportTCP},
		{Selection{Runtime: "pi"}, "pi-acp", adapters.TransportStdio},
		{Selection{Runtime: "claude", Mode: runtimes.ModeACPStdio}, "claude-acp", adapters.TransportStdio},
		{Selection{Runtime: "codex", Mode: runtimes.ModeACPStdio}, "codex-acp", adapters.TransportStdio},
		{Selection{Runtime: "opencode", Mode: runtimes.ModeACPStdio, Binary: "/opt/opencode"}, "opencode-acp", adapters.TransportStdio},
	}
	for _, tc := range tests {
		t.Run(tc.selection.Runtime+"/"+string(tc.selection.Mode), func(t *testing.T) {
			adapter, err := Select(tc.selection)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if adapter.Name() != tc.wantName {
				t.Errorf("Name = %q, want %q", adapter.Name(), tc.wantName)
			}
			desc := adapter.Describe()
			if desc.Protocol != adapters.ProtocolACP || desc.Transport != tc.wantTransport {
				t.Errorf("descriptor = %s/%s, want acp/%s", desc.Protocol, desc.Transport, tc.wantTransport)
			}
			if _, ok := adapter.(interface{ ACPClient() acp.Client }); !ok {
				t.Errorf("%T exposes no ACP client", adapter)
			}
		})
	}
}

// Every factory is a pair the registry lists, and every pair the registry
// lists has a factory unless it is a mode the wrapper deliberately does not
// drive. Defaults always launch.
func TestFactoriesFollowTheRegistry(t *testing.T) {
	notDriven := map[Key]string{
		{runtimes.Claude, runtimes.ModePTY}: "the Claude TUI is a human path, not one the wrapper drives",
	}
	for k := range factories {
		d, ok := registry.Lookup(string(k.Runtime))
		if !ok || !d.Supports(k.Mode) {
			t.Errorf("factory %s is not a registry pair", k)
		}
	}
	for _, d := range registry.All() {
		if _, ok := factories[Key{d.ID, d.DefaultMode}]; !ok {
			t.Errorf("%s's default mode %s has no factory", d.ID, d.DefaultMode)
		}
		for _, ms := range d.Modes {
			k := Key{d.ID, ms.Mode}
			if _, ok := factories[k]; !ok && notDriven[k] == "" {
				t.Errorf("registry pair %s has no factory and no recorded reason", k)
			}
		}
	}
	if got := len(Supported()); got != len(factories) {
		t.Errorf("Supported() lists %d of %d factories", got, len(factories))
	}
}

func TestSelectBinaryAndExtraArgsStayStructured(t *testing.T) {
	binary := "/tmp/path with spaces/codex;echo-not-a-command"
	extra := []string{"--label", "a value with spaces", "$(touch /tmp/never)", "'quoted' && false"}
	adapter, err := Select(Selection{
		Runtime: "codex", Mode: runtimes.ModeSubprocessPerTurn,
		Binary: binary, ExtraArgs: extra,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	native := adapter.(adapters.RuntimeAdapter)
	cli := native.CLIAdapter()
	if got, ok := cli.Detect(); !ok || got != binary {
		t.Fatalf("Detect = (%q, %v), want (%q, true)", got, ok, binary)
	}
	args := cli.BuildArgs("prompt with spaces; still one arg", "", "")
	// The extras sit at codex exec's extra slot, before --json, and the
	// untrusted prompt is last, after "--" (go-providers v0.34.1).
	want := []string{
		"exec",
		"--label", "a value with spaces", "$(touch /tmp/never)", "'quoted' && false",
		"--json", "--skip-git-repo-check",
		"--", "prompt with spaces; still one arg",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}

	// Returned adapters and their args are independent copies, also of the
	// caller's ExtraArgs slice.
	args[0] = "mutated"
	extra[0] = "mutated"
	if got := native.CLIAdapter().BuildArgs("prompt with spaces; still one arg", "", ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("fresh CLI adapter args = %#v, want %#v", got, want)
	}
}

// The adapter Select hands the wrapper is the go-providers adapter itself,
// with Binary and ExtraArgs set on its own fields, so every optional
// interface the registry declares for the mode survives selection
// (CW-20260930-0137's capability-forwarding half).
func TestNativeAdaptersKeepTheirOptionalInterfaces(t *testing.T) {
	implements := map[runtimes.Capability]func(provider.CLIAdapter) bool{
		runtimes.CapTypedEvents:           func(c provider.CLIAdapter) bool { _, ok := c.(provider.EventParser); return ok },
		runtimes.CapSessionLostClassifier: func(c provider.CLIAdapter) bool { _, ok := c.(provider.SessionLostClassifier); return ok },
		runtimes.CapAuthClassifier:        func(c provider.CLIAdapter) bool { _, ok := c.(provider.AuthFailureClassifier); return ok },
		runtimes.CapPreflight:             func(c provider.CLIAdapter) bool { _, ok := c.(provider.Preflighter); return ok },
		runtimes.CapResumeKeepsID: func(c provider.CLIAdapter) bool {
			v, ok := c.(provider.SessionResumeVerifier)
			return ok && v.ResumeKeepsSessionID()
		},
	}
	for key := range nativeSpecs {
		for _, sel := range []Selection{
			{Runtime: string(key.Runtime), Mode: key.Mode},
			{Runtime: string(key.Runtime), Mode: key.Mode, Binary: "/opt/pinned", ExtraArgs: []string{"--x"}},
		} {
			adapter, err := Select(sel)
			if err != nil {
				t.Fatalf("%s: %v", key, err)
			}
			cli := adapter.(adapters.RuntimeAdapter).CLIAdapter()
			if _, ok := cli.(provider.BootDirProvider); !ok {
				t.Errorf("%s (binary %q): %T lost BootDirProvider", key, sel.Binary, cli)
			}
			d, _ := registry.Lookup(string(key.Runtime))
			for _, c := range d.Capabilities(key.Mode) {
				if check, ok := implements[c]; ok && !check(cli) {
					t.Errorf("%s (binary %q): %T does not implement declared %s", key, sel.Binary, cli, c)
				}
			}
			if sel.Binary != "" {
				if got, _ := cli.Detect(); got != sel.Binary {
					t.Errorf("%s: Detect = %q, want the pinned binary", key, got)
				}
			}
		}
	}
}

func TestSelectRejectsUnsupportedAndAmbiguousSelections(t *testing.T) {
	tests := []struct {
		name string
		cfg  Selection
		want error
	}{
		{name: "unknown runtime", cfg: Selection{Runtime: "other"}, want: ErrUnsupportedSelection},
		{name: "no runtime", cfg: Selection{}, want: ErrUnsupportedSelection},
		{name: "old mode spelling", cfg: Selection{Runtime: "codex", Mode: "app-server"}, want: ErrUnsupportedSelection},
		{name: "codex streaming", cfg: Selection{Runtime: "codex", Mode: runtimes.ModeStreamingStdio}, want: ErrUnsupportedSelection},
		{name: "pi native", cfg: Selection{Runtime: "pi", Mode: runtimes.ModeSubprocessPerTurn}, want: ErrUnsupportedSelection},
		{name: "claude pty is not driven", cfg: Selection{Runtime: "claude", Mode: runtimes.ModePTY}, want: ErrUnsupportedSelection},
		{name: "codex developer mode", cfg: Selection{Runtime: "codex", DeveloperMode: true}, want: ErrInvalidSelection},
		{name: "claude acp developer mode", cfg: Selection{Runtime: "claude", Mode: runtimes.ModeACPStdio, DeveloperMode: true}, want: ErrInvalidSelection},
		{name: "relative binary", cfg: Selection{Runtime: "codex", Binary: "./codex"}, want: ErrInvalidSelection},
		{name: "nul arg", cfg: Selection{Runtime: "codex", ExtraArgs: []string{"a\x00b"}}, want: ErrInvalidSelection},
		{name: "port outside acp-tcp", cfg: Selection{Runtime: "copilot", Port: 4321}, want: ErrInvalidSelection},
		{name: "cli adapter for acp", cfg: Selection{Runtime: "copilot", CLIAdapter: provider.NewClaudeAdapter()}, want: ErrInvalidSelection},
		{name: "custom plus developer", cfg: Selection{Runtime: "claude", DeveloperMode: true, CLIAdapter: provider.NewClaudeAdapterDevStreamingStdio()}, want: ErrInvalidSelection},
		{name: "custom name mismatch", cfg: Selection{Runtime: "codex", CLIAdapter: provider.NewOpencodeAdapter()}, want: ErrInvalidSelection},
		{name: "codex custom shape mismatch", cfg: Selection{Runtime: "codex", Mode: runtimes.ModeSubprocessPerTurn, CLIAdapter: provider.NewCodexAdapterAppServer()}, want: ErrInvalidSelection},
		{name: "opencode custom shape mismatch", cfg: Selection{Runtime: "opencode", Mode: runtimes.ModeHTTPSSE, CLIAdapter: provider.NewOpencodeAdapter()}, want: ErrInvalidSelection},
		{name: "claude custom shape mismatch", cfg: Selection{Runtime: "claude", Mode: runtimes.ModeStreamingStdio, CLIAdapter: provider.NewClaudeAdapter()}, want: ErrInvalidSelection},
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
		Runtime: "codex", Mode: runtimes.ModeSubprocessPerTurn,
		CLIAdapter: configured,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := adapter.(adapters.RuntimeAdapter).CLIAdapter(); got != configured {
		t.Fatalf("CLIAdapter = %T %p, want the host's adapter itself", got, got)
	}

	// With a Binary pin the host adapter is copied, not mutated or wrapped.
	pinned, err := Select(Selection{
		Runtime: "codex", Mode: runtimes.ModeSubprocessPerTurn,
		CLIAdapter: configured, Binary: "/opt/codex", ExtraArgs: []string{"--x"},
	})
	if err != nil {
		t.Fatalf("Select pinned: %v", err)
	}
	cli, ok := pinned.(adapters.RuntimeAdapter).CLIAdapter().(*provider.CodexAdapter)
	if !ok || cli == configured || cli.ApprovalPolicy != "on-request" || cli.Binary != "/opt/codex" || !slices.Equal(cli.ExtraArgs, []string{"--x"}) {
		t.Fatalf("pinned CLIAdapter = %#v", cli)
	}
	if configured.Binary != "" || configured.ExtraArgs != nil {
		t.Fatal("Select mutated the host's adapter")
	}
}

type customCLI struct{ provider.CLIAdapter }

func (customCLI) Name() string { return "codex" }

// A custom adapter cannot take Binary/ExtraArgs without being wrapped, which
// would hide its interfaces; Select refuses instead.
func TestSelectRefusesToWrapACustomAdapter(t *testing.T) {
	custom := customCLI{provider.NewCodexAdapter()}
	if _, err := Select(Selection{Runtime: "codex", Mode: runtimes.ModeSubprocessPerTurn, CLIAdapter: custom}); err != nil {
		t.Fatalf("custom adapter alone: %v", err)
	}
	if _, err := Select(Selection{Runtime: "codex", Mode: runtimes.ModeSubprocessPerTurn, CLIAdapter: custom, Binary: "/opt/codex"}); !errors.Is(err, ErrInvalidSelection) {
		t.Fatalf("custom adapter with Binary = %v, want ErrInvalidSelection", err)
	}
}

func TestSelectedAdapterDescribeClonesDeliveryCapabilities(t *testing.T) {
	adapter, err := Select(Selection{Runtime: "claude"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	desc := adapter.Describe()
	desc.Delivery.Supported[0].Evidence = "mutated"
	if got, _ := adapter.Describe().Delivery.Evidence(adapters.DeliveryCapabilitySendTurn); got.Evidence == "mutated" {
		t.Fatal("Describe returned delivery slice sharing adapter backing storage")
	}
}
