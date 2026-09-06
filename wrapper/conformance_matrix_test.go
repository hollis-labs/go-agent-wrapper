package wrapper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type conformanceMatrix struct {
	TaskID string                 `json:"task_id"`
	Rows   []conformanceMatrixRow `json:"rows"`
}

type conformanceMatrixRow struct {
	ID         string   `json:"id"`
	Acceptance string   `json:"acceptance"`
	Guarantee  string   `json:"guarantee"`
	Package    string   `json:"package"`
	Tests      []string `json:"tests"`
	Commands   []string `json:"commands"`
	Files      []string `json:"files"`
}

func TestSharedMaterializationAcceptanceMatrixHasEvidence(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "conformance", "acceptance_matrix.json"))
	if err != nil {
		t.Fatalf("read acceptance matrix: %v", err)
	}
	var matrix conformanceMatrix
	if err := json.Unmarshal(data, &matrix); err != nil {
		t.Fatalf("parse acceptance matrix: %v", err)
	}
	if matrix.TaskID != "CW-20260906-0019" {
		t.Fatalf("matrix task_id = %q", matrix.TaskID)
	}
	if len(matrix.Rows) < 12 {
		t.Fatalf("matrix rows = %d, want at least 12", len(matrix.Rows))
	}
	seenAcceptance := map[string]bool{}
	seenID := map[string]bool{}
	for _, row := range matrix.Rows {
		if row.ID == "" || row.Acceptance == "" || row.Guarantee == "" || row.Package == "" {
			t.Fatalf("row missing identity fields: %#v", row)
		}
		if seenID[row.ID] {
			t.Fatalf("duplicate row id %q", row.ID)
		}
		seenID[row.ID] = true
		seenAcceptance[row.Acceptance] = true
		if len(row.Tests) == 0 || len(row.Commands) == 0 || len(row.Files) == 0 {
			t.Fatalf("row %q lacks tests, commands or files: %#v", row.ID, row)
		}
		for _, command := range row.Commands {
			if strings.TrimSpace(command) == "" {
				t.Fatalf("row %q has empty command", row.ID)
			}
		}
	}
	for _, required := range []string{"four-consumer-examples", "matrix", "os-denial", "validation"} {
		if !seenAcceptance[required] {
			t.Fatalf("matrix missing acceptance area %q", required)
		}
	}
}
