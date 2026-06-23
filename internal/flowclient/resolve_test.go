package flowclient

import (
	"os"
	"path/filepath"
	"testing"
)

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePrefersFlowBin(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "custom-flow")
	writeExecutable(t, bin)
	t.Setenv("FLOW_BIN", bin)

	got, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got != bin {
		t.Fatalf("Resolve() = %q, want FLOW_BIN %q", got, bin)
	}
}

func TestResolveFallsBackToSiblingThenPath(t *testing.T) {
	t.Setenv("FLOW_BIN", "")
	siblingDir := t.TempDir()
	sibling := filepath.Join(siblingDir, "flow")
	writeExecutable(t, sibling)
	oldExecutable := executablePath
	executablePath = func() (string, error) { return filepath.Join(siblingDir, "flowwyyy"), nil }
	t.Cleanup(func() { executablePath = oldExecutable })

	got, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got != sibling {
		t.Fatalf("Resolve() = %q, want sibling %q", got, sibling)
	}

	os.Remove(sibling)
	pathDir := t.TempDir()
	pathFlow := filepath.Join(pathDir, "flow")
	writeExecutable(t, pathFlow)
	t.Setenv("PATH", pathDir)

	got, err = Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got != pathFlow {
		t.Fatalf("Resolve() = %q, want PATH flow %q", got, pathFlow)
	}
}

func TestResolveErrorsWhenFlowMissing(t *testing.T) {
	t.Setenv("FLOW_BIN", "")
	t.Setenv("PATH", t.TempDir())
	oldExecutable := executablePath
	executablePath = func() (string, error) { return filepath.Join(t.TempDir(), "flowwyyy"), nil }
	t.Cleanup(func() { executablePath = oldExecutable })

	if got, err := Resolve(); err == nil || got != "" {
		t.Fatalf("Resolve() = %q, %v; want missing error", got, err)
	}
}
