package flowdb

import (
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func TestSteeringTraceInsertAndList(t *testing.T) {
	db := openTempDB(t)

	traces := []SteeringTrace{
		{
			ID: "t1", CreatedAt: "2026-06-05T08:00:00Z",
			Origin: "live", Source: "slack", Channel: "C1", ChannelType: "public",
			Author: "alice", ThreadKey: "C1:1.0", TextPreview: "hello",
			Disposition: "surfaced", StageReached: "stage3",
			Stage1Relevant: boolPtr(true),
			Stage2Action: "surface", Stage2Confidence: 0.8,
			Stage3Action: "make_task", Stage3Confidence: 0.9,
			FinalAction: "make_task", FinalConfidence: 0.9,
			FeedItemID: "fi1", LatencyMS: 120, Model: "gpt-4",
		},
		{
			ID: "t2", CreatedAt: "2026-06-05T09:00:00Z",
			Origin: "live", Source: "slack", Channel: "C2", ChannelType: "dm",
			Author: "bob", ThreadKey: "C2:2.0", TextPreview: "bye",
			Disposition: "dropped", StageReached: "stage1",
			DropReason:     "irrelevant",
			Stage1Relevant: boolPtr(false),
			LatencyMS: 30,
		},
		{
			ID: "t3", CreatedAt: "2026-06-05T10:00:00Z",
			Origin: "backfill", Source: "github",
			Disposition: "dropped", StageReached: "stage0",
			DropReason: "dedup",
			LatencyMS:  5,
		},
		{
			ID: "t4", CreatedAt: "2026-06-05T07:00:00Z",
			Origin: "live", Source: "slack",
			Disposition: "dropped", StageReached: "cache",
			DropReason: "cache-hit",
			LatencyMS:  2,
		},
		{
			ID: "t5", CreatedAt: "2026-06-05T06:00:00Z",
			Origin: "live", Source: "slack",
			Disposition: "error", StageReached: "stage2",
			Error: "timeout", LatencyMS: 500,
		},
		{
			ID: "t6", CreatedAt: "2026-06-04T12:00:00Z",
			Origin: "live", Source: "slack",
			Disposition: "dropped", StageReached: "stage2",
			DropReason: "low-confidence",
			LatencyMS:  80,
		},
	}

	for _, tr := range traces {
		if err := InsertSteeringTrace(db, tr); err != nil {
			t.Fatalf("InsertSteeringTrace %s: %v", tr.ID, err)
		}
	}

	// ListSteeringTrace{} returns all, newest-first
	all, err := ListSteeringTrace(db, TraceFilter{})
	if err != nil {
		t.Fatalf("ListSteeringTrace all: %v", err)
	}
	if len(all) != len(traces) {
		t.Fatalf("want %d rows, got %d", len(traces), len(all))
	}
	// newest-first: t3 (10:00) > t2 (09:00) > t1 (08:00) > t4 (07:00) > t5 (06:00) > t6 (2026-06-04)
	wantOrder := []string{"t3", "t2", "t1", "t4", "t5", "t6"}
	for i, id := range wantOrder {
		if all[i].ID != id {
			t.Errorf("pos %d: want id %q, got %q", i, id, all[i].ID)
		}
	}

	// ListSteeringTrace{Disposition:"dropped"} returns only drops
	drops, err := ListSteeringTrace(db, TraceFilter{Disposition: "dropped"})
	if err != nil {
		t.Fatalf("ListSteeringTrace dropped: %v", err)
	}
	for _, d := range drops {
		if d.Disposition != "dropped" {
			t.Errorf("expected dropped, got %q (id=%s)", d.Disposition, d.ID)
		}
	}
	if len(drops) != 4 {
		t.Errorf("want 4 drops, got %d", len(drops))
	}

	// ListSteeringTrace{Since: cutoff} excludes older rows
	// cutoff = 2026-06-05T00:00:00Z — should exclude t6 (2026-06-04)
	recent, err := ListSteeringTrace(db, TraceFilter{Since: "2026-06-05T00:00:00Z"})
	if err != nil {
		t.Fatalf("ListSteeringTrace since: %v", err)
	}
	if len(recent) != 5 {
		t.Errorf("want 5 since 2026-06-05, got %d", len(recent))
	}
	for _, r := range recent {
		if r.ID == "t6" {
			t.Error("t6 (2026-06-04) should be excluded by Since filter")
		}
	}

	// ListSteeringTrace{Limit:2} returns 2
	limited, err := ListSteeringTrace(db, TraceFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListSteeringTrace limit: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("want 2, got %d", len(limited))
	}
}

func TestGetSteeringTraceByFeedItem(t *testing.T) {
	db := openTempDB(t)

	traces := []SteeringTrace{
		{ID: "g1", CreatedAt: "2026-06-05T08:00:00Z", Origin: "live", Source: "slack",
			Disposition: "surfaced", StageReached: "stage3", FeedItemID: "fi-A", FinalAction: "reply"},
		// A newer row for the SAME feed item — GetSteeringTraceByFeedItem must
		// return this one (most recent).
		{ID: "g2", CreatedAt: "2026-06-05T09:00:00Z", Origin: "live", Source: "slack",
			Disposition: "surfaced", StageReached: "stage3", FeedItemID: "fi-A", FinalAction: "forward"},
		{ID: "g3", CreatedAt: "2026-06-05T10:00:00Z", Origin: "live", Source: "github",
			Disposition: "surfaced", StageReached: "stage3", FeedItemID: "fi-B", FinalAction: "make_task"},
	}
	for _, tr := range traces {
		if err := InsertSteeringTrace(db, tr); err != nil {
			t.Fatalf("seed %s: %v", tr.ID, err)
		}
	}

	got, err := GetSteeringTraceByFeedItem(db, "fi-A")
	if err != nil {
		t.Fatalf("GetSteeringTraceByFeedItem(fi-A): %v", err)
	}
	if got.ID != "g2" {
		t.Errorf("ID = %q, want g2 (most recent for fi-A)", got.ID)
	}
	if got.FinalAction != "forward" {
		t.Errorf("FinalAction = %q, want forward", got.FinalAction)
	}

	got2, err := GetSteeringTraceByFeedItem(db, "fi-B")
	if err != nil {
		t.Fatalf("GetSteeringTraceByFeedItem(fi-B): %v", err)
	}
	if got2.ID != "g3" || got2.Source != "github" {
		t.Errorf("fi-B trace = %+v, want g3/github", got2)
	}

	// Unknown feed item → error.
	if _, err := GetSteeringTraceByFeedItem(db, "nope"); err == nil {
		t.Error("GetSteeringTraceByFeedItem(unknown) should return an error")
	}
}

func TestListSteeringTraceSourceFilter(t *testing.T) {
	db := openTempDB(t)

	traces := []SteeringTrace{
		{ID: "s1", CreatedAt: "2026-06-05T10:00:00Z", Origin: "live", Source: "slack",
			Disposition: "surfaced", StageReached: "stage3"},
		{ID: "s2", CreatedAt: "2026-06-05T09:00:00Z", Origin: "live", Source: "slack",
			Disposition: "dropped", StageReached: "stage1"},
		{ID: "g1", CreatedAt: "2026-06-05T08:00:00Z", Origin: "live", Source: "github",
			Disposition: "surfaced", StageReached: "stage3"},
	}
	for _, tr := range traces {
		if err := InsertSteeringTrace(db, tr); err != nil {
			t.Fatalf("seed %s: %v", tr.ID, err)
		}
	}

	gh, err := ListSteeringTrace(db, TraceFilter{Source: "github"})
	if err != nil {
		t.Fatalf("ListSteeringTrace github: %v", err)
	}
	if len(gh) != 1 || gh[0].ID != "g1" {
		t.Errorf("source=github → %d rows %v, want only g1", len(gh), traceIDs(gh))
	}

	slack, err := ListSteeringTrace(db, TraceFilter{Source: "slack"})
	if err != nil {
		t.Fatalf("ListSteeringTrace slack: %v", err)
	}
	if len(slack) != 2 {
		t.Errorf("source=slack → %d rows, want 2", len(slack))
	}
	for _, tr := range slack {
		if tr.Source != "slack" {
			t.Errorf("source=slack returned a %q row (id=%s)", tr.Source, tr.ID)
		}
	}

	// Empty source → all rows (no filter).
	all, err := ListSteeringTrace(db, TraceFilter{Source: ""})
	if err != nil {
		t.Fatalf("ListSteeringTrace all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("source='' → %d rows, want all 3", len(all))
	}

	// Source combines with disposition.
	ghSurfaced, err := ListSteeringTrace(db, TraceFilter{Source: "slack", Disposition: "dropped"})
	if err != nil {
		t.Fatalf("ListSteeringTrace combined: %v", err)
	}
	if len(ghSurfaced) != 1 || ghSurfaced[0].ID != "s2" {
		t.Errorf("source=slack+dropped → %v, want only s2", traceIDs(ghSurfaced))
	}
}

func traceIDs(ts []SteeringTrace) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func TestSteeringTraceStage1RelevantRoundTrip(t *testing.T) {
	db := openTempDB(t)

	// nil Stage1Relevant (stage not reached)
	nilTrace := SteeringTrace{
		ID: "nil1", CreatedAt: "2026-06-05T10:00:00Z",
		Origin: "live", Source: "slack",
		Disposition: "dropped", StageReached: "stage0",
		LatencyMS: 0,
	}
	if err := InsertSteeringTrace(db, nilTrace); err != nil {
		t.Fatalf("insert nil trace: %v", err)
	}

	// true Stage1Relevant
	trueTrace := SteeringTrace{
		ID: "true1", CreatedAt: "2026-06-05T09:00:00Z",
		Origin: "live", Source: "slack",
		Disposition: "surfaced", StageReached: "stage3",
		Stage1Relevant: boolPtr(true),
		LatencyMS: 50,
	}
	if err := InsertSteeringTrace(db, trueTrace); err != nil {
		t.Fatalf("insert true trace: %v", err)
	}

	// false Stage1Relevant
	falseTrace := SteeringTrace{
		ID: "false1", CreatedAt: "2026-06-05T08:00:00Z",
		Origin: "live", Source: "slack",
		Disposition: "dropped", StageReached: "stage1",
		Stage1Relevant: boolPtr(false),
		LatencyMS: 10,
	}
	if err := InsertSteeringTrace(db, falseTrace); err != nil {
		t.Fatalf("insert false trace: %v", err)
	}

	all, err := ListSteeringTrace(db, TraceFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := make(map[string]SteeringTrace)
	for _, tr := range all {
		byID[tr.ID] = tr
	}

	if byID["nil1"].Stage1Relevant != nil {
		t.Errorf("nil1: Stage1Relevant should be nil, got %v", byID["nil1"].Stage1Relevant)
	}
	if byID["true1"].Stage1Relevant == nil || *byID["true1"].Stage1Relevant != true {
		t.Errorf("true1: Stage1Relevant should be *true, got %v", byID["true1"].Stage1Relevant)
	}
	if byID["false1"].Stage1Relevant == nil || *byID["false1"].Stage1Relevant != false {
		t.Errorf("false1: Stage1Relevant should be *false, got %v", byID["false1"].Stage1Relevant)
	}
}

func TestSteeringTraceURLRoundTrip(t *testing.T) {
	db := openTempDB(t)

	tr := SteeringTrace{
		ID: "url1", CreatedAt: "2026-06-05T10:00:00Z",
		Origin: "live", Source: "github", Channel: "o/r",
		Disposition: "surfaced", StageReached: "stage3",
		URL: "https://github.com/o/r/pull/5",
	}
	if err := InsertSteeringTrace(db, tr); err != nil {
		t.Fatalf("insert: %v", err)
	}

	all, err := ListSteeringTrace(db, TraceFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 row, got %d", len(all))
	}
	if all[0].URL != "https://github.com/o/r/pull/5" {
		t.Errorf("URL = %q, want %q", all[0].URL, "https://github.com/o/r/pull/5")
	}
}

func TestSteeringTraceTSTeamIDRoundTrip(t *testing.T) {
	db := openTempDB(t)

	tr := SteeringTrace{
		ID: "ts1", CreatedAt: "2026-06-05T10:00:00Z",
		Origin: "live", Source: "slack", Channel: "C1",
		Disposition: "surfaced", StageReached: "stage3",
		TS: "1700000000.000100", TeamID: "T1",
		LatencyMS: 0,
	}
	if err := InsertSteeringTrace(db, tr); err != nil {
		t.Fatalf("insert: %v", err)
	}

	all, err := ListSteeringTrace(db, TraceFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 row, got %d", len(all))
	}
	if all[0].TS != "1700000000.000100" {
		t.Errorf("TS = %q, want %q", all[0].TS, "1700000000.000100")
	}
	if all[0].TeamID != "T1" {
		t.Errorf("TeamID = %q, want %q", all[0].TeamID, "T1")
	}
}

func TestSteeringFunnelSince(t *testing.T) {
	db := openTempDB(t)

	traces := []SteeringTrace{
		{ID: "f1", CreatedAt: "2026-06-05T10:00:00Z", Origin: "live", Source: "s", Disposition: "surfaced", StageReached: "stage3", LatencyMS: 0},
		{ID: "f2", CreatedAt: "2026-06-05T09:00:00Z", Origin: "live", Source: "s", Disposition: "surfaced", StageReached: "stage3", LatencyMS: 0},
		{ID: "f3", CreatedAt: "2026-06-05T08:00:00Z", Origin: "live", Source: "s", Disposition: "dropped", StageReached: "stage0", LatencyMS: 0},
		{ID: "f4", CreatedAt: "2026-06-05T07:00:00Z", Origin: "live", Source: "s", Disposition: "dropped", StageReached: "cache", LatencyMS: 0},
		{ID: "f5", CreatedAt: "2026-06-05T06:00:00Z", Origin: "live", Source: "s", Disposition: "dropped", StageReached: "stage1", LatencyMS: 0},
		{ID: "f6", CreatedAt: "2026-06-05T05:00:00Z", Origin: "live", Source: "s", Disposition: "dropped", StageReached: "stage2", LatencyMS: 0},
		{ID: "f7", CreatedAt: "2026-06-05T04:00:00Z", Origin: "live", Source: "s", Disposition: "error", StageReached: "stage2", LatencyMS: 0},
		// Before the cutoff — excluded when Since is set
		{ID: "f8", CreatedAt: "2026-06-04T12:00:00Z", Origin: "live", Source: "s", Disposition: "surfaced", StageReached: "stage3", LatencyMS: 0},
	}
	for _, tr := range traces {
		if err := InsertSteeringTrace(db, tr); err != nil {
			t.Fatalf("seed %s: %v", tr.ID, err)
		}
	}

	// All rows (since == "")
	full, err := SteeringFunnelSince(db, "")
	if err != nil {
		t.Fatalf("SteeringFunnelSince: %v", err)
	}
	if full.Observed != 8 {
		t.Errorf("Observed: want 8, got %d", full.Observed)
	}
	if full.Surfaced != 3 {
		t.Errorf("Surfaced: want 3, got %d", full.Surfaced)
	}
	if full.Errors != 1 {
		t.Errorf("Errors: want 1, got %d", full.Errors)
	}
	if full.DroppedStage0 != 1 {
		t.Errorf("DroppedStage0: want 1, got %d", full.DroppedStage0)
	}
	if full.DroppedCache != 1 {
		t.Errorf("DroppedCache: want 1, got %d", full.DroppedCache)
	}
	if full.DroppedStage1 != 1 {
		t.Errorf("DroppedStage1: want 1, got %d", full.DroppedStage1)
	}
	if full.DroppedStage2 != 1 {
		t.Errorf("DroppedStage2: want 1, got %d", full.DroppedStage2)
	}

	// With Since cutoff, f8 (2026-06-04) excluded → Observed=7, Surfaced=2
	windowed, err := SteeringFunnelSince(db, "2026-06-05T00:00:00Z")
	if err != nil {
		t.Fatalf("SteeringFunnelSince windowed: %v", err)
	}
	if windowed.Observed != 7 {
		t.Errorf("windowed Observed: want 7, got %d", windowed.Observed)
	}
	if windowed.Surfaced != 2 {
		t.Errorf("windowed Surfaced: want 2, got %d", windowed.Surfaced)
	}
}
