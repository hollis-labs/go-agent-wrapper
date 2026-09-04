package wrapper

import (
	"errors"
	"reflect"
	"testing"
)

func TestChildEnvironmentModes(t *testing.T) {
	tests := []struct {
		name      string
		cfg       ChildEnvironment
		inherited []string
		want      []string
		explicit  bool
	}{
		{
			name: "zero value inherits with deterministic duplicate resolution",
			cfg:  ChildEnvironment{},
			inherited: []string{
				"SECRET=first", "PATH=/usr/bin", "SECRET=last",
			},
			want: []string{"PATH=/usr/bin", "SECRET=last"},
		},
		{
			name: "inherit allowlist and unset narrow ambient values",
			cfg: ChildEnvironment{
				Allowlist: []string{"PATH", "HOME", "PATH"},
				Unset:     []string{"HOME", "HOME"},
			},
			inherited: []string{"API_TOKEN=secret", "HOME=/users/test", "PATH=/bin"},
			want:      []string{"PATH=/bin"},
			explicit:  true,
		},
		{
			name: "non-nil empty allowlist inherits nothing",
			cfg: ChildEnvironment{
				Mode:      EnvironmentInherit,
				Allowlist: []string{},
			},
			inherited: []string{"API_TOKEN=secret"},
			want:      []string{},
			explicit:  true,
		},
		{
			name: "merge allowlist set duplicates metacharacters and unset wins",
			cfg: ChildEnvironment{
				Mode:      EnvironmentMerge,
				Allowlist: []string{"PATH", "KEEP"},
				Set: []string{
					"KEEP=overridden",
					"SPACED=a value with spaces",
					"META=$(touch /tmp/never); `echo nope` && still-data",
					"DUP=first",
					"DUP=last",
				},
				Unset: []string{"KEEP", "MISSING"},
			},
			inherited: []string{"PATH=/usr/bin", "KEEP=ambient", "TOKEN=secret"},
			want: []string{
				"DUP=last",
				"META=$(touch /tmp/never); `echo nope` && still-data",
				"PATH=/usr/bin",
				"SPACED=a value with spaces",
			},
			explicit: true,
		},
		{
			name: "replace never inherits a secret",
			cfg: ChildEnvironment{
				Mode:  EnvironmentReplace,
				Set:   []string{"PATH=/safe/bin", "PATH=/safer/bin", "EMPTY="},
				Unset: []string{"ABSENT"},
			},
			inherited: []string{"SECRET=must-not-leak", "PATH=/ambient"},
			want:      []string{"EMPTY=", "PATH=/safer/bin"},
			explicit:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, explicit, err := tc.cfg.resolve(tc.inherited)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("env = %#v, want %#v", got, tc.want)
			}
			if explicit != tc.explicit {
				t.Fatalf("explicit = %v, want %v", explicit, tc.explicit)
			}
		})
	}
}

func TestChildEnvironmentRejectsAmbiguousOrInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		cfg  ChildEnvironment
		want error
	}{
		{name: "unknown mode", cfg: ChildEnvironment{Mode: "maybe"}, want: ErrInvalidEnvironment},
		{name: "set in inherit", cfg: ChildEnvironment{Mode: EnvironmentInherit, Set: []string{"A=b"}}, want: ErrEnvironmentSetInInherit},
		{name: "allowlist in replace", cfg: ChildEnvironment{Mode: EnvironmentReplace, Allowlist: []string{}}, want: ErrEnvironmentAllowlistInReplace},
		{name: "set missing equals", cfg: ChildEnvironment{Mode: EnvironmentReplace, Set: []string{"NOPE"}}, want: ErrInvalidEnvironment},
		{name: "empty set name", cfg: ChildEnvironment{Mode: EnvironmentReplace, Set: []string{"=value"}}, want: ErrInvalidEnvironment},
		{name: "equals in unset", cfg: ChildEnvironment{Mode: EnvironmentMerge, Unset: []string{"A=B"}}, want: ErrInvalidEnvironment},
		{name: "nul in allowlist", cfg: ChildEnvironment{Mode: EnvironmentMerge, Allowlist: []string{"A\x00B"}}, want: ErrInvalidEnvironment},
		{name: "nul in value", cfg: ChildEnvironment{Mode: EnvironmentReplace, Set: []string{"A=B\x00C"}}, want: ErrInvalidEnvironment},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := tc.cfg.resolve([]string{"BASE=value"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want errors.Is(_, %v)", err, tc.want)
			}
		})
	}
}

func TestResolvedSpecEnvironmentNilInheritsAndNonNilReplaces(t *testing.T) {
	base := []string{"A=base", "SECRET=ambient"}
	got, explicit, err := resolvedSpecEnvironment(base, nil)
	if err != nil {
		t.Fatalf("nil spec: %v", err)
	}
	if explicit || !reflect.DeepEqual(got, base) {
		t.Fatalf("nil spec = (%#v, %v), want inherited base and false", got, explicit)
	}
	got[0] = "MUTATED=yes"
	if base[0] != "A=base" {
		t.Fatal("nil spec result aliases base")
	}

	got, explicit, err = resolvedSpecEnvironment(base, []string{"A=one", "A=two", "SAFE=x"})
	if err != nil {
		t.Fatalf("explicit spec: %v", err)
	}
	want := []string{"A=two", "SAFE=x"}
	if !explicit || !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit spec = (%#v, %v), want (%#v, true)", got, explicit, want)
	}
}

func TestChildEnvironmentWindowsNamesAreCaseInsensitive(t *testing.T) {
	cfg := ChildEnvironment{
		Mode:      EnvironmentMerge,
		Allowlist: []string{"path", "secret"},
		Set:       []string{"Path=first", "PATH=last"},
		Unset:     []string{"SECRET"},
	}
	got, _, err := cfg.resolveForOS([]string{"Path=ambient", "Secret=must-not-leak"}, "windows")
	if err != nil {
		t.Fatalf("resolveForOS: %v", err)
	}
	if want := []string{"PATH=last"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Windows env = %#v, want %#v", got, want)
	}

	got, _, err = cfg.resolveForOS([]string{"Path=ambient", "Secret=unix-value"}, "linux")
	if err != nil {
		t.Fatalf("Unix resolveForOS: %v", err)
	}
	if want := []string{"PATH=last", "Path=first"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Unix env = %#v, want %#v", got, want)
	}
}
