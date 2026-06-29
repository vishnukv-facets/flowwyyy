package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

func TestCmdTellAppendsActionableInboxJSONL(t *testing.T) {
	root := setupFlowRoot(t)
	t.Setenv("FLOW_UI_URL", "http://127.0.0.1:1")
	wd := t.TempDir()
	if rc := cmdAdd([]string{"task", "Review plan", "--slug", "review-plan", "--work-dir", wd, "--agent", "claude"}); rc != 0 {
		t.Fatalf("cmdAdd rc=%d", rc)
	}

	if rc := cmdTell([]string{"review-plan", "Forwarded file context\n\nSecurity report: clean", "--from", "attention-router", "--no-notify"}); rc != 0 {
		t.Fatalf("cmdTell rc=%d", rc)
	}

	if _, err := os.Stat(filepath.Join(root, "tasks", "review-plan", "inbox.md")); err != nil {
		t.Fatalf("inbox.md missing: %v", err)
	}
	entries, err := monitor.ReadInboxEntries("review-plan")
	if err != nil {
		t.Fatalf("ReadInboxEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	got := entries[0]
	if got.Meta.Source != "flow" || !got.Meta.Actionable {
		t.Fatalf("meta = %+v, want actionable flow", got.Meta)
	}
	if got.Event.Kind != "flow_tell" || got.Event.ChannelType != "flow" {
		t.Fatalf("event kind/channel_type = %q/%q", got.Event.Kind, got.Event.ChannelType)
	}
	if got.Event.UserID != "attention-router" {
		t.Fatalf("sender = %q", got.Event.UserID)
	}
	if !strings.Contains(got.Event.Text, "Forwarded file context") || !strings.Contains(got.Event.Text, "Security report: clean") {
		t.Fatalf("event text missing forwarded context: %q", got.Event.Text)
	}

	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()
	rows, err := flowdb.ListWorkEventLog(db, flowdb.WorkEventLogFilter{EventType: "flow_tell", TaskSlug: "review-plan"})
	if err != nil {
		t.Fatalf("ListWorkEventLog: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("flow_tell rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].Source != "flow" || rows[0].ActorID != "attention-router" || rows[0].ExternalID == "" {
		t.Fatalf("flow_tell provenance = %+v", rows[0])
	}
}
