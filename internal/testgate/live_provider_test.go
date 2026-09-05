package testgate

import (
	"fmt"
	"strings"
	"testing"
)

type recordingSkipper struct {
	helperCalled bool
	skipMessage  string
}

func (s *recordingSkipper) Helper() { s.helperCalled = true }

func (s *recordingSkipper) Skipf(format string, args ...any) {
	s.skipMessage = fmt.Sprintf(format, args...)
}

func TestRequireLiveProviderDefaultsToSkipped(t *testing.T) {
	t.Setenv(LiveProviderEnvironment, "")
	var got recordingSkipper
	RequireLiveProvider(&got)
	if !got.helperCalled {
		t.Fatal("gate did not mark itself as a test helper")
	}
	if got.skipMessage == "" {
		t.Fatal("unset opt-in did not skip")
	}
	if want := LiveProviderEnvironment + "=1"; !strings.Contains(got.skipMessage, want) {
		t.Fatalf("skip message %q does not tell the operator to set %s", got.skipMessage, want)
	}
}

func TestRequireLiveProviderRejectsTruthyButInexactValues(t *testing.T) {
	for _, value := range []string{"0", "true", "TRUE", "yes"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(LiveProviderEnvironment, value)
			var got recordingSkipper
			RequireLiveProvider(&got)
			if got.skipMessage == "" {
				t.Fatalf("value %q unexpectedly enabled installed-provider tests", value)
			}
		})
	}
}

func TestRequireLiveProviderAcceptsExactOptIn(t *testing.T) {
	t.Setenv(LiveProviderEnvironment, "1")
	var got recordingSkipper
	RequireLiveProvider(&got)
	if got.skipMessage != "" {
		t.Fatalf("exact opt-in unexpectedly skipped: %s", got.skipMessage)
	}
}
