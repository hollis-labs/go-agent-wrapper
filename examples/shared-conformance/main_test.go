package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSharedConformanceScenariosRun(t *testing.T) {
	results, err := runSelected(context.Background(), "all", t.TempDir())
	if err != nil {
		t.Fatalf("runSelected(all): %v", err)
	}
	var names []string
	for _, result := range results {
		names = append(names, result.Scenario)
		if result.BootRoot == "" || len(result.EntryPaths) == 0 || len(result.Checks) == 0 {
			t.Fatalf("incomplete result: %#v", result)
		}
		for _, check := range result.Checks {
			if len(check) < 3 || check[:3] != "ok:" {
				t.Fatalf("failed check in %s: %s", result.Scenario, check)
			}
		}
	}
	want := []string{"cairn", "nanite", "torque", "tether"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("scenarios = %#v, want %#v", names, want)
	}
}

func TestSharedConformanceProgramRuns(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not available")
	}
	root := t.TempDir()
	cmd := exec.Command("go", "run", ".", "-scenario", "all", "-root", root)
	cmd.Dir = "."
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run shared-conformance: %v\n%s", err, output)
	}
	for _, rel := range []string{
		"cairn/boot/managed/config.json",
		"nanite/boot/skills/nanite/SKILL.md",
		"torque/boot/tasks/T-100/task.json",
		"tether/boot/bundles/task-alpha/AGENTS.md",
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("expected generated artifact %s: %v\noutput:\n%s", rel, err, output)
		}
	}
}
