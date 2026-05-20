package monitor

import (
	"os"
	"path/filepath"
	"testing"
)

// pinFlowRoot points FLOW_ROOT at a temp dir so the server.url file lands
// somewhere we can inspect — and doesn't pollute the real ~/.flow/.
func pinFlowRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLOW_ROOT", dir)
	return dir
}

func TestFlowBaseURLEnvWinsEverything(t *testing.T) {
	pinFlowRoot(t)
	t.Setenv("FLOW_BASE_URL", "https://flow.acme.com/")
	// Also write a server.url file — env must take precedence.
	if _, err := WriteServerURLFile("http://localhost:9999"); err != nil {
		t.Fatal(err)
	}
	got := FlowBaseURL()
	if got != "https://flow.acme.com" {
		t.Errorf("FlowBaseURL = %q, want trimmed env value (env wins over server.url)", got)
	}
}

func TestFlowBaseURLFallsBackToServerURLFile(t *testing.T) {
	root := pinFlowRoot(t)
	t.Setenv("FLOW_BASE_URL", "")
	path, err := WriteServerURLFile("http://localhost:9999/")
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: the path must be under FLOW_ROOT.
	if path != filepath.Join(root, "server.url") {
		t.Errorf("server.url written at %q, want %q", path, filepath.Join(root, "server.url"))
	}
	got := FlowBaseURL()
	if got != "http://localhost:9999" {
		t.Errorf("FlowBaseURL = %q, want value from server.url with trailing slash trimmed", got)
	}
}

func TestFlowBaseURLReturnsEmptyWhenNothingConfigured(t *testing.T) {
	pinFlowRoot(t)
	t.Setenv("FLOW_BASE_URL", "")
	got := FlowBaseURL()
	if got != "" {
		t.Errorf("FlowBaseURL = %q, want empty (no env, no server.url)", got)
	}
}

func TestRemoveServerURLFileIsIdempotent(t *testing.T) {
	pinFlowRoot(t)
	// Calling remove with no file present must not error.
	if err := RemoveServerURLFile(); err != nil {
		t.Errorf("remove on missing file errored: %v", err)
	}
	// Now write, remove, and re-remove.
	if _, err := WriteServerURLFile("http://localhost:8000"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveServerURLFile(); err != nil {
		t.Fatal(err)
	}
	if err := RemoveServerURLFile(); err != nil {
		t.Errorf("second remove errored: %v", err)
	}
}

func TestWriteServerURLFileNoOpsOnEmpty(t *testing.T) {
	root := pinFlowRoot(t)
	t.Setenv("FLOW_BASE_URL", "")
	path, err := WriteServerURLFile("   ")
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Errorf("empty URL write returned path %q, want empty", path)
	}
	// File must not have been created.
	if _, err := os.Stat(filepath.Join(root, "server.url")); !os.IsNotExist(err) {
		t.Errorf("server.url should not exist for empty write; stat err = %v", err)
	}
}
