package inbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendAndReadFlowTell(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "tasks", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := AppendInboxEvent("demo", FlowTellEvent("parent", "hi", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadInboxEntries("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
}
