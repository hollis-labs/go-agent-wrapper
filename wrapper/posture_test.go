package wrapper

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/agentkit/agentlaunch"
	permission "github.com/hollis-labs/go-permission"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-providers/providertest"
	"github.com/hollis-labs/go-providers/registry"

	"github.com/hollis-labs/go-agent-wrapper/launch"
)

// An explicit PermissionPosture on a native launch reaches the CLI as the
// registry maps it: flags before the prompt, environment set
// (CW-20260930-0138).
func TestNativePostureReachesTheLaunch(t *testing.T) {
	skipUnlessSh(t)
	posture := permission.ModePlan
	for _, c := range postureLaunchCases {
		t.Run(string(c.id), func(t *testing.T) {
			desc, _ := registry.Lookup(string(c.id))
			want, err := desc.PostureFor(posture, desc.DefaultMode)
			if err != nil {
				t.Fatal(err)
			}
			fake := providertest.New(t, c.id, c.runs...)
			cfg := func(cf *Config) {
				if c.cfg != nil {
					c.cfg(cf)
				}
				cf.PermissionPosture = posture
			}
			runSelected(t, launch.Selection{Runtime: string(c.id), Binary: fake.Path, ExtraArgs: []string{"--caller-flag"}}, cfg, c.drive)
			call := fake.Call(0)
			if len(want.Args) > 0 {
				flags := append(slices.Clone(want.Args), "--caller-flag")
				at := -1
				for i := range call.Args {
					if i+len(flags) <= len(call.Args) && slices.Equal(call.Args[i:i+len(flags)], flags) {
						at = i
						break
					}
				}
				if at < 0 {
					t.Fatalf("argv %q lacks the posture's flags, then the caller's: %q", call.Args, flags)
				}
				prompt := slices.Index(call.Args, "--")
				if prompt < 0 {
					prompt = slices.IndexFunc(call.Args, func(a string) bool { return strings.HasPrefix(a, "-p=") })
				}
				if prompt >= 0 && at+len(flags) > prompt {
					t.Errorf("posture flags after the prompt starts: %q", call.Args)
				}
			}
			for name, value := range want.Env {
				if got, _ := call.Getenv(name); got != value {
					t.Errorf("env %s = %q, want %q", name, got, value)
				}
			}
		})
	}
}

func TestSessionPosture(t *testing.T) {
	prepared := func(p permission.Mode) *agentlaunch.PreparedExecution {
		return &agentlaunch.PreparedExecution{Posture: p}
	}
	for _, c := range []struct {
		name     string
		cfg      permission.Mode
		prepared *agentlaunch.PreparedExecution
		want     permission.Mode
		err      error
	}{
		{"nothing set", "", nil, permission.ModeDefault, nil},
		{"config only", permission.ModeYolo, nil, permission.ModeYolo, nil},
		{"prepared, no posture", "", prepared(""), permission.ModeDefault, nil},
		{"prepared posture answers", "", prepared(permission.ModeAcceptEdits), permission.ModeAcceptEdits, nil},
		{"both, the same", permission.ModePlan, prepared(permission.ModePlan), permission.ModePlan, nil},
		{"both, different", permission.ModeYolo, prepared(permission.ModePlan), "", ErrPostureConflict},
		{"config over a prepared launch with none", permission.ModeYolo, prepared(""), permission.ModeYolo, nil},
	} {
		got, err := sessionPosture(c.cfg, c.prepared)
		if !errors.Is(err, c.err) || got != c.want {
			t.Errorf("%s: sessionPosture = %q, %v; want %q, %v", c.name, got, err, c.want, c.err)
		}
	}
}

func TestWithPostureArgsCopiesTheAdapter(t *testing.T) {
	host := &provider.ClaudeAdapter{ExtraArgs: []string{"--host"}}
	got, err := withPostureArgs(host, []string{"--permission-mode", "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if c := got.(*provider.ClaudeAdapter); c == host || !slices.Equal(c.ExtraArgs, []string{"--permission-mode", "plan", "--host"}) {
		t.Errorf("ExtraArgs = %q (same adapter: %v)", c.ExtraArgs, c == host)
	}
	if !slices.Equal(host.ExtraArgs, []string{"--host"}) {
		t.Errorf("the host's adapter changed: %q", host.ExtraArgs)
	}
	if _, err := withPostureArgs(&fakeCLI{name: "custom"}, []string{"--x"}); err == nil {
		t.Error("flags on a non-go-providers adapter were accepted")
	}
	if a, err := withPostureArgs(&fakeCLI{name: "custom"}, nil); err != nil || a == nil {
		t.Errorf("no flags: %v, %v", a, err)
	}
}

func TestWithPostureEnv(t *testing.T) {
	got := withPostureEnv([]string{"A=1", "OPENCODE_PERMISSION=old", "B=2"}, map[string]string{"OPENCODE_PERMISSION": "new"})
	if want := []string{"A=1", "B=2", "OPENCODE_PERMISSION=new"}; !slices.Equal(got, want) {
		t.Errorf("env = %q, want %q", got, want)
	}
	if got := withPostureEnv([]string{"A=1"}, nil); !slices.Equal(got, []string{"A=1"}) {
		t.Errorf("no posture env changed the environment: %q", got)
	}
}
