package flowdb

import (
	"database/sql"
	"testing"
)

func TestWorkEventLogAppendsOnceWithProvenance(t *testing.T) {
	db := openTempDB(t)
	now := NowISO()
	insertTask(t, db, "ledger-task", "Ledger task", "backlog", "high", t.TempDir(), nil)
	ctx, err := CreateWorkContext(db, WorkContext{Title: "Ledger context"})
	if err != nil {
		t.Fatalf("CreateWorkContext: %v", err)
	}
	anchor, err := CreateWorkContextSourceAnchor(db, WorkContextSourceAnchor{
		WorkContextID: ctx.ID,
		Source:        "slack",
		AnchorType:    "slack_channel_thread",
		ExternalID:    "C123:1780000000.000100",
	})
	if err != nil {
		t.Fatalf("CreateWorkContextSourceAnchor: %v", err)
	}

	first, inserted, err := AppendWorkEventLog(db, WorkEventLogEntry{
		EventID:        "slack-send:C123:1780000000.000100",
		EventType:      "slack_send",
		Provider:       "claude",
		SessionID:      "session-1",
		TaskSlug:       "ledger-task",
		WorkContextID:  ctx.ID,
		ActorKind:      "agent",
		ActorID:        "claude:session-1",
		SourceAnchorID: anchor.ID,
		Source:         "slack",
		ExternalID:     "1780000000.000100",
		ExternalURL:    "https://example.slack.com/archives/C123/p1780000000000100",
		OccurredAt:     now,
		MetadataJSON:   `{"thread_ts":"1780000000.000100", "channel":"C123"}`,
	})
	if err != nil {
		t.Fatalf("AppendWorkEventLog first: %v", err)
	}
	if !inserted {
		t.Fatal("first append inserted=false, want true")
	}
	if first.MetadataJSON != `{"thread_ts":"1780000000.000100","channel":"C123"}` {
		t.Fatalf("MetadataJSON = %q", first.MetadataJSON)
	}

	duplicate, inserted, err := AppendWorkEventLog(db, WorkEventLogEntry{
		EventID:      first.EventID,
		EventType:    "slack_send",
		TaskSlug:     "ledger-task",
		OccurredAt:   now,
		MetadataJSON: `{"changed":true}`,
	})
	if err != nil {
		t.Fatalf("AppendWorkEventLog duplicate: %v", err)
	}
	if inserted {
		t.Fatal("duplicate append inserted=true, want false")
	}
	if duplicate.MetadataJSON != first.MetadataJSON {
		t.Fatalf("duplicate mutated metadata = %q, want %q", duplicate.MetadataJSON, first.MetadataJSON)
	}

	rows, err := ListWorkEventLog(db, WorkEventLogFilter{TaskSlug: "ledger-task", WorkContextID: ctx.ID})
	if err != nil {
		t.Fatalf("ListWorkEventLog: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.EventID != first.EventID || got.EventType != "slack_send" || got.Provider != "claude" || got.SessionID != "session-1" {
		t.Fatalf("unexpected row: %+v", got)
	}
	if got.SourceAnchorID != anchor.ID || got.ExternalURL == "" || got.ActorKind != "agent" || got.ActorID == "" {
		t.Fatalf("missing provenance: %+v", got)
	}
}

func TestWorkEventLogRejectsInvalidMetadataJSON(t *testing.T) {
	db := openTempDB(t)
	_, _, err := AppendWorkEventLog(db, WorkEventLogEntry{
		EventID:      "bad-json",
		EventType:    "flow_tell",
		OccurredAt:   NowISO(),
		MetadataJSON: `{"broken"`,
	})
	if err == nil {
		t.Fatal("AppendWorkEventLog accepted invalid metadata JSON")
	}
}

func TestWorkEventLogRequiresEventType(t *testing.T) {
	db := openTempDB(t)
	_, _, err := AppendWorkEventLog(db, WorkEventLogEntry{
		EventID:    "missing-type",
		OccurredAt: NowISO(),
	})
	if err == nil {
		t.Fatal("AppendWorkEventLog accepted missing event type")
	}
}

func TestWorkEventLogCanUseGeneratedEventID(t *testing.T) {
	db := openTempDB(t)
	got, inserted, err := AppendWorkEventLog(db, WorkEventLogEntry{
		EventType:  "session_bound",
		Provider:   "codex",
		SessionID:  "codex-session-1",
		OccurredAt: NowISO(),
	})
	if err != nil {
		t.Fatalf("AppendWorkEventLog: %v", err)
	}
	if !inserted || got.EventID == "" {
		t.Fatalf("generated append = %+v inserted=%v", got, inserted)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_event_log WHERE event_id = ?`, got.EventID).Scan(&count); err != nil && err != sql.ErrNoRows {
		t.Fatalf("count generated row: %v", err)
	}
	if count != 1 {
		t.Fatalf("generated row count = %d, want 1", count)
	}
}
