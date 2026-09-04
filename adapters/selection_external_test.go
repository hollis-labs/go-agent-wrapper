package adapters_test

import (
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/adapters"
)

func TestPublicSelectionSurfaceCompilesForHostMigration(t *testing.T) {
	adapter, err := adapters.Select(adapters.Selection{
		Provider:    adapters.ProviderCodex,
		RuntimeKind: adapters.RuntimeKindCLI,
		LaunchMode:  adapters.LaunchSubprocessPerTurn,
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if adapter.Name() != "codex" {
		t.Fatalf("Name = %q, want codex", adapter.Name())
	}
}
