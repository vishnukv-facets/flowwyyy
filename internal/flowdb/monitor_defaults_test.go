package flowdb

import (
	"testing"
)

// TestDefaultSlackAutomationRules pins the seeded Slack rule modes. Drift
// in these defaults silently changes what arrives in users' inboxes after
// a `flow init`, so an explicit table fights that. Add a new row when a
// new Slack kind ships.
func TestDefaultSlackAutomationRules(t *testing.T) {
	db := openTempDB(t)
	if err := EnsureDefaultAutomationRules(db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := map[string]string{
		"slack.dm":               "approval",
		"slack.mention":          "approval",
		"slack.personal_mention": "approval",
		"slack.channel_message":  "log",
	}
	for id, wantMode := range want {
		var mode string
		if err := db.QueryRow(`SELECT mode FROM automation_rules WHERE id = ?`, id).Scan(&mode); err != nil {
			t.Errorf("%s missing or unreadable: %v", id, err)
			continue
		}
		if mode != wantMode {
			t.Errorf("%s mode = %q, want %q", id, mode, wantMode)
		}
	}
}

// TestMonitorSyncStateLifecycle verifies the start → end transition and
// the field semantics that the Inbox UI depends on. Tracks the contract
// for "you're seeing Slack synced 23s ago because RecordMonitorSyncEnd
// wrote that timestamp."
func TestMonitorSyncStateLifecycle(t *testing.T) {
	db := openTempDB(t)
	// Start one source.
	s1, err := RecordMonitorSyncStart(db, "slack")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !s1.IsSyncing {
		t.Errorf("after start, is_syncing = false; want true")
	}
	if s1.LastStatus != "unknown" {
		t.Errorf("after first start, last_status = %q; want 'unknown'", s1.LastStatus)
	}
	// Successful end.
	s2, err := RecordMonitorSyncEnd(db, "slack", "ok", "")
	if err != nil {
		t.Fatalf("end ok: %v", err)
	}
	if s2.IsSyncing {
		t.Errorf("after end, is_syncing = true; want false")
	}
	if s2.LastStatus != "ok" {
		t.Errorf("last_status = %q; want 'ok'", s2.LastStatus)
	}
	if s2.LastError.Valid {
		t.Errorf("last_error = %q; want NULL on ok", s2.LastError.String)
	}
	if !s2.LastSyncAt.Valid {
		t.Errorf("last_sync_at unset after a successful end")
	}
	// Failed end keeps the prior last_sync_at fresh and records the error.
	s3, err := RecordMonitorSyncEnd(db, "slack", "error", "auth.test: invalid_auth")
	if err != nil {
		t.Fatalf("end error: %v", err)
	}
	if s3.LastStatus != "error" {
		t.Errorf("last_status = %q; want 'error'", s3.LastStatus)
	}
	if s3.LastError.String != "auth.test: invalid_auth" {
		t.Errorf("last_error = %q; want error message", s3.LastError.String)
	}
	// Multi-source list.
	if _, err := RecordMonitorSyncEnd(db, "github", "ok", ""); err != nil {
		t.Fatalf("github end: %v", err)
	}
	rows, err := ListMonitorSyncStates(db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d; want 2", len(rows))
	}
	// Stable alphabetical order is the UI's rendering contract.
	if rows[0].Source != "github" || rows[1].Source != "slack" {
		t.Errorf("rows not alphabetical: %+v", rows)
	}
}

// TestInsertMonitorEventIfNewFreezesExistingRows pins the archival-source
// contract for Slack ingest: once a (source, source_id) tuple is recorded,
// a second InsertMonitorEventIfNew call with different field values must
// NOT mutate the existing row. The user's body, kind, etc. stay frozen at
// first-seen values. Contrast with UpsertMonitorEvent which would update.
func TestInsertMonitorEventIfNewFreezesExistingRows(t *testing.T) {
	db := openTempDB(t)
	first := MonitorEventInput{
		Source: "slack", Kind: "dm", SourceID: "Dx:1.001",
		Title: "first title", Body: "first body", Severity: "medium",
	}
	ev1, isNew1, err := InsertMonitorEventIfNew(db, first)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if !isNew1 {
		t.Errorf("first insert isNew = false, want true")
	}
	if ev1.Title != "first title" || ev1.Body.String != "first body" {
		t.Fatalf("first row content wrong: %+v", ev1)
	}

	// Same source_id, completely different content. Insert-new must
	// preserve the existing row, return isNew=false, and report the
	// ORIGINAL content not the input we passed.
	second := MonitorEventInput{
		Source: "slack", Kind: "dm", SourceID: "Dx:1.001",
		Title: "edited title", Body: "edited body", Severity: "high",
	}
	ev2, isNew2, err := InsertMonitorEventIfNew(db, second)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if isNew2 {
		t.Errorf("second insert isNew = true, want false (row already existed)")
	}
	if ev2.Title != "first title" {
		t.Errorf("title mutated: %q, want %q (frozen)", ev2.Title, "first title")
	}
	if ev2.Body.String != "first body" {
		t.Errorf("body mutated: %q, want %q (frozen)", ev2.Body.String, "first body")
	}
	if ev2.Severity != "medium" {
		t.Errorf("severity mutated: %q, want %q (frozen)", ev2.Severity, "medium")
	}
}

// TestEnsureDefaultAutomationRulesIsIdempotent verifies user edits survive
// repeated seed calls. EnsureDefaultAutomationRules runs on every poll
// cycle; if it clobbered overrides, users would see their Slack routing
// reset every few minutes.
func TestEnsureDefaultAutomationRulesIsIdempotent(t *testing.T) {
	db := openTempDB(t)
	if err := EnsureDefaultAutomationRules(db); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	// User opts a high-volume channel into pings.
	if err := SetAutomationRuleMode(db, "slack", "channel_message", "notify"); err != nil {
		t.Fatalf("override: %v", err)
	}
	// Subsequent poll cycle re-runs the seed.
	if err := EnsureDefaultAutomationRules(db); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	var mode string
	if err := db.QueryRow(`SELECT mode FROM automation_rules WHERE id = 'slack.channel_message'`).Scan(&mode); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if mode != "notify" {
		t.Errorf("user override clobbered: mode = %q, want notify", mode)
	}
}
