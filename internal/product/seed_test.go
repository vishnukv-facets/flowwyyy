package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeedSteererPersonaCreatesPersona(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)

	if err := seedSteererPersona(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "persona.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Write like a real person") || !strings.Contains(string(data), "Never add a signature") {
		t.Fatalf("persona seed missing expected content: %q", string(data))
	}
}
